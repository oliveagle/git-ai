package observer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The names of the galaxies cost money, so the tests below hold the budget in
// place: a stable fleet is never asked about twice, a day is capped, and a
// failure is not retried for hours.

func TestParseGalaxyLabelReadsTheTwoLines(t *testing.T) {
	name, summary := parseGalaxyLabel("git ai tooling\nA family of agents that land changes every hour.")
	if name != "git ai tooling" {
		t.Fatalf("name = %q, want the first line", name)
	}
	if !strings.Contains(summary, "agents") {
		t.Fatalf("summary = %q, want the second line", summary)
	}
}

func TestParseGalaxyLabelSkipsTheNarration(t *testing.T) {
	name, _ := parseGalaxyLabel("Here is the name you asked for:\ngit ai tooling\nA family of agents.")
	if name != "git ai tooling" {
		t.Fatalf("name = %q, want the label and not the narration", name)
	}
}

func TestParseGalaxyLabelStripsThePrefix(t *testing.T) {
	name, _ := parseGalaxyLabel("Name: git ai tooling\nSummary: A family of agents.")
	if name != "git ai tooling" {
		t.Fatalf("name = %q, want the label without the prefix", name)
	}
}

func TestIsPlausibleLabelRejectsASentence(t *testing.T) {
	if isPlausibleLabel("The group contains mostly rust services and tools.") {
		t.Fatal("a sentence is not a label")
	}
	if isPlausibleLabel("git ai tooling for everyone to use here") {
		t.Fatal("seven words are not a label")
	}
	if isPlausibleLabel("git") {
		t.Fatal("one word is not a label")
	}
	if isPlausibleLabel("we should name this group carefully") {
		t.Fatal("a narration is not a label")
	}
	if !isPlausibleLabel("git ai tooling") {
		t.Fatal("a label is not a label")
	}
}

// fakeModel answers like a naming model and counts the calls.
func fakeModel(t *testing.T, calls *int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output_text": "git ai tooling\nA family of agents.",
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestTheNamerNamesAFamilyOnceAndNeverAgain(t *testing.T) {
	var calls int64
	server := fakeModel(t, &calls)
	dir := t.TempDir()

	namer := NewGalaxyNamer(Config{
		StateDir:         dir,
		LabelTargets:     []LabelTarget{{URL: server.URL, Model: "test-model"}},
		LabelDailyCalls:  32,
		LabelMinInterval: time.Millisecond,
		LabelTimeout:     5 * time.Second,
		LabelRetryAfter:  time.Hour,
	})
	namer.Start()
	defer namer.Stop()

	family := GalaxyFamily{Signature: "1,2,3", Members: []string{"a.git", "b.git"}, RuleName: "git ai"}
	namer.Request([]GalaxyFamily{family})

	waitFor(t, 3*time.Second, func() bool { return len(namer.Names()) == 1 })
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	name := namer.Names()[0]
	if name.Name != "git ai tooling" {
		t.Fatalf("name = %q, want the answer of the model", name.Name)
	}
	if name.Model != "test-model" {
		t.Fatalf("model = %q, want the model that answered", name.Model)
	}

	// The same membership again costs nothing.
	namer.Request([]GalaxyFamily{family})
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("calls = %d, want 1: an unchanged galaxy is never asked about again", calls)
	}
}

func TestTheNamerKeepsANameAcrossARestart(t *testing.T) {
	var calls int64
	server := fakeModel(t, &calls)
	dir := t.TempDir()

	config := Config{
		StateDir:         dir,
		LabelTargets:     []LabelTarget{{URL: server.URL, Model: "test-model"}},
		LabelDailyCalls:  32,
		LabelMinInterval: time.Millisecond,
		LabelTimeout:     5 * time.Second,
		LabelRetryAfter:  time.Hour,
	}
	first := NewGalaxyNamer(config)
	first.Start()
	first.Request([]GalaxyFamily{{Signature: "1,2", Members: []string{"a.git"}}})
	waitFor(t, 3*time.Second, func() bool { return len(first.Names()) == 1 })
	first.Stop()

	second := NewGalaxyNamer(config)
	defer second.Stop()
	second.Request([]GalaxyFamily{{Signature: "1,2", Members: []string{"a.git"}}})
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("calls = %d, want 1: the name is cached on disk", calls)
	}
	if len(second.Names()) != 1 {
		t.Fatalf("names = %d, want the cached one", len(second.Names()))
	}
}

func TestTheDailyCeilingStopsTheCalls(t *testing.T) {
	var calls int64
	server := fakeModel(t, &calls)

	namer := NewGalaxyNamer(Config{
		StateDir:         t.TempDir(),
		LabelTargets:     []LabelTarget{{URL: server.URL, Model: "test-model"}},
		LabelDailyCalls:  1,
		LabelMinInterval: time.Millisecond,
		LabelTimeout:     5 * time.Second,
		LabelRetryAfter:  time.Hour,
	})
	namer.Start()
	defer namer.Stop()

	namer.Request([]GalaxyFamily{
		{Signature: "1", Members: []string{"a.git"}},
		{Signature: "2", Members: []string{"b.git"}},
	})
	waitFor(t, 3*time.Second, func() bool { return len(namer.Names()) == 1 })
	time.Sleep(300 * time.Millisecond)
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("calls = %d, want 1: the ceiling of the day is one call", calls)
	}
	if status := namer.Status(); status.CallsToday != 1 || status.DailyLimit != 1 {
		t.Fatalf("status = %+v, want one call of a ceiling of one", status)
	}
}

func TestAFailedFamilyIsNotRetriedForHours(t *testing.T) {
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("the channel is dry"))
	}))
	t.Cleanup(server.Close)

	namer := NewGalaxyNamer(Config{
		StateDir:         t.TempDir(),
		LabelTargets:     []LabelTarget{{URL: server.URL, Model: "test-model"}},
		LabelDailyCalls:  32,
		LabelMinInterval: time.Millisecond,
		LabelTimeout:     5 * time.Second,
		LabelRetryAfter:  time.Hour,
	})
	namer.Start()
	defer namer.Stop()

	namer.Request([]GalaxyFamily{{Signature: "1", Members: []string{"a.git"}}})
	waitFor(t, 3*time.Second, func() bool { return namer.Status().LastError != nil })

	first := atomic.LoadInt64(&calls)
	namer.Request([]GalaxyFamily{{Signature: "1", Members: []string{"a.git"}}})
	time.Sleep(300 * time.Millisecond)
	if atomic.LoadInt64(&calls) != first {
		t.Fatalf("calls = %d, want %d: a failed family waits for hours", atomic.LoadInt64(&calls), first)
	}
}

func TestTheNamerWithoutAModelNeverCalls(t *testing.T) {
	namer := NewGalaxyNamer(Config{StateDir: t.TempDir()})
	namer.Start()
	defer namer.Stop()

	namer.Request([]GalaxyFamily{{Signature: "1", Members: []string{"a.git"}}})
	time.Sleep(200 * time.Millisecond)
	if len(namer.Names()) != 0 {
		t.Fatalf("names = %d, want none: without a model the rules stand", len(namer.Names()))
	}
	if namer.Configured() {
		t.Fatal("the namer is configured, want it to be without a model")
	}
}

func TestTheCacheIsWrittenBesideTheJournal(t *testing.T) {
	dir := t.TempDir()
	namer := NewGalaxyNamer(Config{
		StateDir:     dir,
		LabelTargets: []LabelTarget{{URL: "http://127.0.0.1:1", Model: "test-model"}},
	})
	namer.mu.Lock()
	namer.cache.Names["1,2"] = GalaxyName{Signature: "1,2", Name: "git ai tooling"}
	namer.mu.Unlock()
	namer.persist()

	raw, err := os.ReadFile(filepath.Join(dir, "galaxy-names.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "git ai tooling") {
		t.Fatalf("the cache does not hold the name: %s", raw)
	}
}

// waitFor waits until a condition holds.
func waitFor(t *testing.T, limit time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the condition did not hold in time")
}

var _ = context.Background
