package observer

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const (
	// ringSize is how many events the service remembers. A client that
	// reconnects with an older cursor is told to reload instead of being
	// served a gap.
	ringSize = 1600
	// journalMaxBytes is the size at which the journal is rotated.
	journalMaxBytes = 24 * 1024 * 1024
)

// EventHub orders the activity of the fleet, gives every event a monotonic
// cursor and keeps a journal of what it saw, so a restarted service replays
// what it already knows and a client reconnects where it left off.
type EventHub struct {
	mu           sync.Mutex
	events       []ActivityEvent
	seen         map[string]struct{}
	nextCursor   int64
	listeners    map[int64]chan ActivityEvent
	nextListener int64

	journalPath  string
	journalBytes int64
}

// NewEventHub returns a hub that journals into the state directory, or one
// without a journal when the state directory is empty.
func NewEventHub(stateDir string) *EventHub {
	hub := &EventHub{
		seen:      map[string]struct{}{},
		listeners: map[int64]chan ActivityEvent{},
	}
	if stateDir != "" {
		hub.journalPath = filepath.Join(stateDir, "activity.jsonl")
		if err := os.MkdirAll(stateDir, 0o755); err == nil {
			hub.loadJournal()
		}
	}
	return hub
}

// loadJournal reads the journal of a previous run. A torn line after a crash
// is ignored: it costs history, not live data.
func (h *EventHub) loadJournal() {
	file, err := os.Open(h.journalPath)
	if err != nil {
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err == nil {
		h.journalBytes = info.Size()
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event ActivityEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if event.ID == "" || event.At == "" || event.Kind == "" {
			continue
		}
		h.push(event, false)
	}
}

// appendJournal writes one event to the journal. The live stream must not
// fail because the journal cannot be written.
func (h *EventHub) appendJournal(event ActivityEvent) {
	if h.journalPath == "" {
		return
	}
	file, err := os.OpenFile(h.journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if _, err := file.Write(line); err != nil {
		return
	}
	h.journalBytes += int64(len(line))
	if h.journalBytes > journalMaxBytes {
		_ = os.Rename(h.journalPath, h.journalPath+".1")
		h.journalBytes = 0
	}
}

// push stores an event in the ring and gives it its cursor.
func (h *EventHub) push(event ActivityEvent, journal bool) ActivityEvent {
	if _, ok := h.seen[event.ID]; ok {
		return event
	}
	stored := event
	stored.Cursor = h.nextCursor
	h.nextCursor++
	h.events = append(h.events, stored)
	h.seen[stored.ID] = struct{}{}
	if len(h.events) > ringSize {
		removed := h.events[:len(h.events)-ringSize]
		h.events = h.events[len(h.events)-ringSize:]
		for _, item := range removed {
			delete(h.seen, item.ID)
		}
	}
	if journal {
		h.appendJournal(stored)
	}
	return stored
}

// Publish records an event and hands it to the subscribers.
func (h *EventHub) Publish(event ActivityEvent) ActivityEvent {
	h.mu.Lock()
	for _, existing := range h.events {
		if existing.ID == event.ID {
			h.mu.Unlock()
			return existing
		}
	}
	stored := h.push(event, true)
	listeners := make([]chan ActivityEvent, 0, len(h.listeners))
	for _, listener := range h.listeners {
		listeners = append(listeners, listener)
	}
	h.mu.Unlock()

	for _, listener := range listeners {
		select {
		case listener <- stored:
		default:
			// A subscriber that cannot keep up misses the event rather than
			// stalling the fleet.
		}
	}
	return stored
}

// PublishMany records a batch of events and returns them in cursor order.
func (h *EventHub) PublishMany(events []ActivityEvent) []ActivityEvent {
	stored := make([]ActivityEvent, 0, len(events))
	for _, event := range events {
		stored = append(stored, h.Publish(event))
	}
	for i := 1; i < len(stored); i++ {
		for j := i; j > 0 && stored[j].Cursor < stored[j-1].Cursor; j-- {
			stored[j], stored[j-1] = stored[j-1], stored[j]
		}
	}
	return stored
}

// Cursor is the cursor of the newest event.
func (h *EventHub) Cursor() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nextCursor - 1
}

// All returns every event the hub remembers.
func (h *EventHub) All() []ActivityEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ActivityEvent, len(h.events))
	copy(out, h.events)
	return out
}

// Recent returns the newest events, oldest first.
func (h *EventHub) Recent(limit int) []ActivityEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	if limit <= 0 || limit > len(h.events) {
		limit = len(h.events)
	}
	out := make([]ActivityEvent, limit)
	copy(out, h.events[len(h.events)-limit:])
	return out
}

// Since returns the events after a cursor. It returns nil when the cursor is
// older than the ring, which tells the client to reload instead of being
// served a gap.
func (h *EventHub) Since(cursor int64) []ActivityEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cursor < 0 {
		return nil
	}
	if cursor == 0 {
		limit := 240
		if len(h.events) < limit {
			limit = len(h.events)
		}
		out := make([]ActivityEvent, limit)
		copy(out, h.events[len(h.events)-limit:])
		return out
	}
	oldest := int64(0)
	if len(h.events) > 0 {
		oldest = h.events[0].Cursor
	}
	if cursor+1 < oldest {
		return nil
	}
	out := make([]ActivityEvent, 0, len(h.events))
	for _, event := range h.events {
		if event.Cursor > cursor {
			out = append(out, event)
		}
	}
	return out
}

// Subscribe returns a channel of the events published from now on, and the
// function that ends the subscription.
func (h *EventHub) Subscribe() (<-chan ActivityEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextListener
	h.nextListener++
	channel := make(chan ActivityEvent, 256)
	h.listeners[id] = channel
	return channel, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if existing, ok := h.listeners[id]; ok {
			delete(h.listeners, id)
			close(existing)
		}
	}
}
