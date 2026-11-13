// Package price computes what an order costs. It is the unit under test: pure
// Go, no clock, no file, no socket.
package price

import (
	"errors"
	"fmt"
)

// Line is one item of an order.
type Line struct {
	SKU   string
	Qty   int
	Cents int
}

// ErrNoSKU is returned for a line that names no item.
var ErrNoSKU = errors.New("price: a line needs a SKU")

// Total returns the price of the order in cents.
func Total(lines []Line) (int, error) {
	total := 0
	for _, line := range lines {
		if line.SKU == "" {
			return 0, ErrNoSKU
		}
		if line.Qty <= 0 {
			return 0, fmt.Errorf("price: line %q has quantity %d", line.SKU, line.Qty)
		}
		total += line.Qty * line.Cents
	}
	return total, nil
}
