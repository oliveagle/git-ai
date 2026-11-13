package price

import (
	"errors"
	"testing"
)

// TestTotal is the unit suite: it answers "is the arithmetic right" without a
// server, a database or a network, so it runs in a second in the workspace of
// the agent and again in the CI.
func TestTotal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lines   []Line
		want    int
		wantErr bool
	}{
		{name: "one line", lines: []Line{{SKU: "tea", Qty: 3, Cents: 250}}, want: 750},
		{name: "many lines", lines: []Line{{SKU: "tea", Qty: 2, Cents: 250}, {SKU: "cup", Qty: 1, Cents: 900}}, want: 1400},
		{name: "empty order", lines: nil, want: 0},
		{name: "no quantity", lines: []Line{{SKU: "tea", Qty: 0, Cents: 250}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Total(tc.lines)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Total(%v) = %d, want an error", tc.lines, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Total(%v) = %v, want no error", tc.lines, err)
			}
			if got != tc.want {
				t.Fatalf("Total(%v) = %d, want %d", tc.lines, got, tc.want)
			}
		})
	}
}

// TestTotalRejectsALineWithoutASKU names the error a caller matches on: a table
// entry cannot say "this exact error" without hiding the other cases.
func TestTotalRejectsALineWithoutASKU(t *testing.T) {
	if _, err := Total([]Line{{Qty: 1, Cents: 100}}); !errors.Is(err, ErrNoSKU) {
		t.Fatalf("Total(line without SKU) = %v, want %v", err, ErrNoSKU)
	}
}
