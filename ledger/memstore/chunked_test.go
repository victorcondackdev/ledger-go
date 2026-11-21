package memstore

import "testing"

func TestChunked(t *testing.T) {
	var c chunked[int]
	const n = 3*chunkSize + 17
	for i := 0; i < n; i++ {
		c.append(i)
	}
	if c.len() != n || *c.at(chunkSize) != chunkSize || *c.at(n - 1) != n-1 {
		t.Fatal("append/at")
	}
	for _, r := range [][2]int{{0, 0}, {0, 5}, {chunkSize - 2, chunkSize + 3}, {5, n}, {2 * chunkSize, 3 * chunkSize}, {n - 1, n}} {
		got := c.copyRange(r[0], r[1])
		if len(got) != r[1]-r[0] {
			t.Fatalf("copyRange%v: len %d", r, len(got))
		}
		for i, v := range got {
			if v != r[0]+i {
				t.Fatalf("copyRange%v[%d] = %d", r, i, v)
			}
		}
	}
}
