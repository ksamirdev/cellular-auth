package router

import (
	"fmt"
	"slices"
	"testing"
)

func TestOwnersIsBalanced(t *testing.T) {
	cells := []string{"cell-1", "cell-2", "cell-3", "cell-4"}
	const keys = 100_000
	homes := map[string]int{}
	for i := range keys {
		o := Owners(cells, fmt.Sprintf("acct-%d", i))
		if o[0] == o[1] {
			t.Fatalf("home and backup are both %s", o[0])
		}
		homes[o[0]]++
	}
	for _, c := range cells {
		share := float64(homes[c]) / keys
		if share < 0.24 || share > 0.26 {
			t.Errorf("%s homes %.3f of keys, want about 0.25", c, share)
		}
	}
}

func TestOwnersMovesOnlyKeysOfAddedCell(t *testing.T) {
	before := []string{"cell-1", "cell-2", "cell-3", "cell-4"}
	after := slices.Concat(before, []string{"cell-5"})
	const keys = 50_000
	moved := 0
	for i := range keys {
		k := fmt.Sprintf("acct-%d", i)
		old, cur := Owners(before, k)[0], Owners(after, k)[0]
		if old != cur {
			if cur != "cell-5" {
				t.Fatalf("%s moved from %s to %s, not to the new cell", k, old, cur)
			}
			moved++
		}
	}
	if share := float64(moved) / keys; share < 0.18 || share > 0.22 {
		t.Errorf("%.3f of keys moved, want about 0.20", share)
	}
}

func TestOwnersIgnoresInputOrder(t *testing.T) {
	a := Owners([]string{"cell-1", "cell-2", "cell-3"}, "acct-42")
	b := Owners([]string{"cell-3", "cell-1", "cell-2"}, "acct-42")
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("order-dependent result: %v vs %v", a, b)
	}
}
