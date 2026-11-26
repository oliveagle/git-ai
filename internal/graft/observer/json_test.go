package observer

import (
	"encoding/json"
	"strings"
	"testing"
)

// The API answers a list for a list. A browser walks `runs.length` on the
// answer, and Go answers `null` for a slice that was never filled: the page
// goes blank instead of showing that there is nothing yet. The rule is
// enforced at the edge, so it holds for every endpoint and for every field
// that comes later.
//
// A value that may be absent — a timestamp, a measurement, an error — stays
// null: the client asks whether it is there.

func TestEmptyListsFillsEveryAbsentList(t *testing.T) {
	type inner struct {
		Runs   []MirrorRun         `json:"runs"`
		Labels []string            `json:"labels"`
		Nested map[string][]string `json:"nested"`
	}
	payload := struct {
		Targets []MirrorTargetIntel `json:"targets"`
		Events  []ActivityEvent     `json:"events"`
		Warning []string            `json:"warning"`
		Inner   inner               `json:"inner"`
	}{
		Targets: []MirrorTargetIntel{{Name: "one", Runs: nil, Branches: nil}},
		Inner:   inner{Nested: map[string][]string{"a": nil}},
	}

	encoded, err := marshalJSON(payload)
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	for _, want := range []string{
		`"runs":[]`,
		`"labels":[]`,
		`"nested":{"a":[]}`,
		`"events":[]`,
		`"warning":[]`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("the answer does not hold %s: %s", want, encoded)
		}
	}
}

func TestEmptyListsFillsEveryAbsentMap(t *testing.T) {
	payload := MirrorTargetIntel{Name: "one", Published: nil}
	encoded, err := marshalJSON(payload)
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	if !strings.Contains(string(encoded), `"published":{}`) {
		t.Fatalf("the answer holds no map: %s", encoded)
	}
}

func TestEmptyListsKeepsTheListsThatAreThere(t *testing.T) {
	payload := MirrorTargetIntel{
		Name:     "one",
		Branches: []string{"main"},
		Runs:     []MirrorRun{{ID: "r1", Status: "ok"}},
	}
	encoded, err := marshalJSON(payload)
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	if !strings.Contains(string(encoded), `"runs":[{"id":"r1"`) {
		t.Fatalf("the runs are lost: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"branches":["main"]`) {
		t.Fatalf("the branches are lost: %s", encoded)
	}
}

// A value that may be absent stays absent: the client asks whether it is
// there, and the answer says no.
func TestEmptyListsKeepsTheAbsentValues(t *testing.T) {
	encoded, err := marshalJSON(MirrorTargetIntel{Name: "one"})
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	for _, field := range []string{"nextRunAt", "cadenceMs", "lag", "successRate", "error"} {
		value, ok := decoded[field]
		if !ok {
			t.Fatalf("the answer does not hold %s: %s", field, encoded)
		}
		if value != nil {
			t.Fatalf("the answer holds %s = %v, want it absent", field, value)
		}
	}
}

// A mirror target without a run history is the case that broke the page: the
// worker never published anything for it, so the slice was absent, and the
// answer said `null` where the contract says "a list of runs".
func TestAMirrorTargetWithoutRunsAnswersAnEmptyList(t *testing.T) {
	target := MirrorTargetIntel{
		Name:       "github-dot_agents",
		Repository: "dot_agents.git",
		Host:       "github.com",
		Branches:   []string{"*"},
	}
	encoded, err := marshalJSON(target)
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	var decoded struct {
		Runs []MirrorRun `json:"runs"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if decoded.Runs == nil {
		t.Fatalf("the answer holds no list of runs: %s", encoded)
	}
	if len(decoded.Runs) != 0 {
		t.Fatalf("the answer holds %d runs, want none", len(decoded.Runs))
	}
}
