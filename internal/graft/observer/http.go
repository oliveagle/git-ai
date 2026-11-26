package observer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Handler returns the HTTP API of the service. A client speaks JSON to it and
// never reaches a database, a worker host or a model.
func (o *Observer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/snapshot", o.handleSnapshot)
	mux.HandleFunc("/api/health", o.handleHealth)
	mux.HandleFunc("/api/events", o.handleEvents)
	mux.HandleFunc("/api/refresh", o.handleRefresh)
	mux.HandleFunc("/api/galaxy-names", o.handleGalaxyNames)
	if o.cfg.StaticDir != "" {
		mux.Handle("/", staticHandler(o.cfg.StaticDir))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"service": "graft-observer"})
		})
	}
	return withCORS(mux)
}

// writeJSON writes a document as JSON.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(payload); err != nil {
		// The status line is already written; there is nothing left to say.
		return
	}
}

// withCORS lets a frontend on another origin read the API. The service holds
// no secret a browser may not have: the write actions carry their own token.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleSnapshot answers with the picture of the fleet.
func (o *Observer) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "only GET is allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, o.Snapshot())
}

// handleHealth answers with the state of the service.
func (o *Observer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "only GET is allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, o.Health(r.Context()))
}

// handleGalaxyNames answers with the galaxy labels the namer holds.
func (o *Observer) handleGalaxyNames(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "only GET is allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": false,
		"names":      []GalaxyName{},
		"callsToday": 0,
		"dailyLimit": o.cfg.LabelDailyCalls,
		"lastCallAt": nil,
		"lastError":  nil,
	})
}

// handleRefresh reads what changed right now.
func (o *Observer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST is allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := o.Poll(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cursor": o.Hub().Cursor()})
}

// handleEvents streams the activity of the fleet. A client that reconnects
// with a cursor gets what it missed; a client whose cursor fell out of the
// ring is told to reload.
func (o *Observer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "only GET is allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "the connection cannot stream", http.StatusInternalServerError)
		return
	}
	cursor := int64(0)
	if raw := r.URL.Query().Get("since"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			cursor = parsed
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	events, unsubscribe := o.Hub().Subscribe()
	defer unsubscribe()

	replay := o.Hub().Since(cursor)
	if replay == nil {
		fmt.Fprint(w, "event: reset\ndata: {}\n\n")
		flusher.Flush()
	} else {
		for _, event := range replay {
			writeEvent(w, event)
		}
		flusher.Flush()
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			writeEvent(w, event)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// writeEvent writes one event of the stream.
func writeEvent(w http.ResponseWriter, event ActivityEvent) {
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: activity\ndata: %s\n\n", event.Cursor, payload)
}

// staticHandler serves the built frontend, when the service hosts it.
func staticHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dir, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		// The frontend is a single page: every path is served by it.
		index := filepath.Join(dir, "index.html")
		http.ServeFile(w, r, index)
	})
}
