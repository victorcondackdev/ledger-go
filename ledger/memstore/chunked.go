package memstore

// chunkSize bounds the copy cost of appends: history grows by whole
// chunks instead of re-allocating (and re-copying) one ever larger slice.
const chunkSize = 1024

// chunked is an append-only list stored in fixed-size chunks. Elements
// never move once appended.
type chunked[T any] struct {
	chunks [][]T
	n      int
}

func (c *chunked[T]) len() int { return c.n }

func (c *chunked[T]) append(v T) {
	if c.n%chunkSize == 0 {
		c.chunks = append(c.chunks, make([]T, 0, chunkSize))
	}
	last := len(c.chunks) - 1
	c.chunks[last] = append(c.chunks[last], v)
	c.n++
}

// at returns a pointer to element i (0-based); it must not be mutated.
func (c *chunked[T]) at(i int) *T { return &c.chunks[i/chunkSize][i%chunkSize] }

// copyRange returns a copy of elements [from, to).
func (c *chunked[T]) copyRange(from, to int) []T {
	out := make([]T, 0, to-from)
	for i := from; i < to; {
		ch := c.chunks[i/chunkSize]
		j := i % chunkSize
		k := len(ch)
		if rest := to - i + j; rest < k {
			k = rest
		}
		out = append(out, ch[j:k]...)
		i += k - j
	}
	return out
}
