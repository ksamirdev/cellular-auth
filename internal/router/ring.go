package router

import (
	"cmp"
	"hash/fnv"
	"slices"
)

// Owners ranks cells for a routing key by rendezvous hashing. The first cell
// is the key's home, the second its backup. Adding or removing a cell only
// moves the keys that ranked it first or second, and every router computes
// the same answer without coordination.
func Owners(cells []string, key string) []string {
	type scored struct {
		id    string
		score uint64
	}
	ranked := make([]scored, len(cells))
	for i, c := range cells {
		ranked[i] = scored{c, score(c, key)}
	}
	slices.SortFunc(ranked, func(a, b scored) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.id
	}
	return out
}

func score(cell, key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(cell))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return mix(h.Sum64())
}

// mix is the splitmix64 finalizer. FNV alone clusters on keys that differ
// only in their last bytes, like sequential account ids.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
