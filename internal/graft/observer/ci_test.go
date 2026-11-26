package observer

import (
	"strings"
	"testing"
)

// The CI telemetry is read from the database of Woodpecker, where the
// timestamps are seconds and the numbers may arrive as any kind of number.

func TestNumberOfReadsEveryKindOfNumber(t *testing.T) {
	cases := map[any]int64{
		int64(7):   7,
		int32(7):   7,
		int(7):     7,
		float64(7): 7,
		"seven":    0,
		nil:        0,
	}
	for value, want := range cases {
		if got := numberOf(value); got != want {
			t.Fatalf("numberOf(%v) = %d, want %d", value, got, want)
		}
	}
}

func TestTextOfFallsBackWhenTheColumnIsEmpty(t *testing.T) {
	if got := textOf("woodpecker", "fallback"); got != "woodpecker" {
		t.Fatalf("textOf = %q, want woodpecker", got)
	}
	if got := textOf("", "fallback"); got != "fallback" {
		t.Fatalf("textOf = %q, want the fallback", got)
	}
	if got := textOf(nil, "fallback"); got != "fallback" {
		t.Fatalf("textOf = %q, want the fallback", got)
	}
}

func TestInstantOfReadsTheSecondsOfWoodpecker(t *testing.T) {
	stamp := instantOf(int64(1764000000))
	if stamp == nil || !strings.HasPrefix(*stamp, "2025-11") {
		t.Fatalf("instant = %v, want a stamp of november 2025", stamp)
	}
	if got := instantOf(int64(0)); got != nil {
		t.Fatalf("instant = %v, want nil for the epoch of nothing", got)
	}
	if got := instantOf("2025-11-24T00:00:00Z"); got != nil {
		t.Fatalf("instant = %v, want nil: the column is a number", got)
	}
}

func TestSpanOfIsTheDurationBetweenTwoStamps(t *testing.T) {
	duration := spanOf(int64(100), int64(160))
	if duration == nil || *duration != 60000 {
		t.Fatalf("span = %v, want 60000ms", duration)
	}
	if got := spanOf(int64(160), int64(100)); got != nil {
		t.Fatalf("span = %v, want nil when the finish is before the start", got)
	}
	if got := spanOf(nil, int64(100)); got != nil {
		t.Fatalf("span = %v, want nil when a stamp is missing", got)
	}
}

func TestFirstCIErrorKeepsTheMessageOfThePipeline(t *testing.T) {
	if got := firstCIError("  the step failed  "); got != "the step failed" {
		t.Fatalf("error = %q, want the trimmed message", got)
	}
	if got := firstCIError(""); got != "" {
		t.Fatalf("error = %q, want empty", got)
	}
	if got := firstCIError(`{"message":"the step failed"}`); !strings.Contains(got, "the step failed") {
		t.Fatalf("error = %q, want the whole document", got)
	}
}

func TestAnUnconfiguredCIDatabaseIsAnEmptyReadingNotAFailure(t *testing.T) {
	intel := NewCIIntel(Config{})
	response := intel.Get(t.Context())
	if response.OK {
		t.Fatal("the reading is ok, want it to say that there is nothing to read")
	}
	if response.Source.Error == nil || !strings.Contains(*response.Source.Error, "CI database") {
		t.Fatalf("error = %v, want the reason", response.Source.Error)
	}
	if response.Summary.WindowHours != ciWindowHours {
		t.Fatalf("window = %d, want %d", response.Summary.WindowHours, ciWindowHours)
	}
	if response.Pipelines == nil || response.Repos == nil || response.Agents == nil {
		t.Fatal("the lists are nil, want empty lists: the client walks them without a check")
	}
}
