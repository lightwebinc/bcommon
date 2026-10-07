package chirp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
)

var benchSizes = []struct {
	name string
	size int
}{{"100MiB", 100 << 20}, {"1GiB", 1 << 30}}

// writeBench feeds size bytes of distinct blobs to w in 1 MiB writes.
func writeBench(w interface{ Write([]byte) (int, error) }, size int) {
	blob := make([]byte, ChunkSize)
	for i := 0; i*ChunkSize < size; i++ {
		genBlob(blob, uint64(i))
		b := blob[:min(ChunkSize, size-i*ChunkSize)]
		for off := 0; off < len(b); off += 1 << 20 {
			w.Write(b[off:min(off+1<<20, len(b))])
		}
	}
}

// Staging: content written in pieces, each blob hashed and handed on.
func BenchmarkChunker(b *testing.B) {
	for _, s := range benchSizes {
		b.Run("sequential/"+s.name, func(b *testing.B) {
			b.SetBytes(int64(s.size))
			for b.Loop() {
				c := NewChunker(func([]byte) error { return nil })
				writeBench(c, s.size)
				if _, err := c.Finish(nil); err != nil {
					b.Fatal(err)
				}
			}
		})
		for _, w := range []int{2, 4, 8} {
			b.Run(fmt.Sprintf("parallel-%d/%s", w, s.name), func(b *testing.B) {
				b.SetBytes(int64(s.size))
				for b.Loop() {
					c := NewParallelChunker(w, func(int, Child, []byte) error { return nil })
					writeBench(c, s.size)
					if _, err := c.Finish(nil); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Content in memory.
func BenchmarkBuild(b *testing.B) {
	for _, s := range benchSizes {
		content := make([]byte, s.size)
		for i := 0; i*ChunkSize < s.size; i++ {
			genBlob(content[i*ChunkSize:min((i+1)*ChunkSize, s.size)], uint64(i))
		}
		b.Run("sequential/"+s.name, func(b *testing.B) {
			b.SetBytes(int64(s.size))
			for b.Loop() {
				Build(content, nil)
			}
		})
		b.Run("parallel-8/"+s.name, func(b *testing.B) {
			b.SetBytes(int64(s.size))
			for b.Loop() {
				BuildParallel(content, nil, 8)
			}
		})
	}
}

// The closure check: Verify at the end against Incremental fed by 8
// goroutines as the pieces arrive, the time from first piece to verdict.
func BenchmarkVerify(b *testing.B) {
	for _, s := range benchSizes {
		content := make([]byte, s.size)
		for i := 0; i*ChunkSize < s.size; i++ {
			genBlob(content[i*ChunkSize:min((i+1)*ChunkSize, s.size)], uint64(i))
		}
		built, _ := Build(content, nil)
		objects := map[[32]byte][]byte{built.RootHash: built.Root}
		for _, br := range built.Branches {
			objects[sha256.Sum256(br)] = br
		}
		var order [][]byte
		order = append(order, built.Root)
		for _, bl := range built.Blobs {
			objects[sha256.Sum256(bl)] = bl
			order = append(order, bl)
		}
		for _, br := range built.Branches {
			order = append(order, br)
		}
		// The hashes a provider computed as the pieces arrived, for
		// AddHashed.
		sums := make([][32]byte, len(order))
		for i, p := range order {
			sums[i] = sha256.Sum256(p)
		}
		fetch := func(h [32]byte) ([]byte, error) {
			if o, ok := objects[h]; ok {
				return o, nil
			}
			return nil, errors.New("none")
		}
		b.Run("sequential/"+s.name, func(b *testing.B) {
			b.SetBytes(int64(s.size))
			for b.Loop() {
				if _, err := Verify(built.RootHash, fetch, Limits{}); err != nil {
					b.Fatal(err)
				}
			}
		})
		for _, hashed := range []bool{false, true} {
			name := "incremental-8"
			if hashed {
				name = "incremental-8-hashed"
			}
			b.Run(name+"/"+s.name, func(b *testing.B) {
				b.SetBytes(int64(s.size))
				for b.Loop() {
					inc := NewIncremental(built.RootHash, fetch, Limits{}, 0)
					next := make(chan int, 8)
					var wg sync.WaitGroup
					for range 8 {
						wg.Go(func() {
							for i := range next {
								if hashed {
									inc.AddHashed(sums[i], order[i])
								} else {
									inc.Add(order[i])
								}
							}
						})
					}
					for i := range order {
						next <- i
					}
					close(next)
					wg.Wait()
					if c, err := inc.Check(); err != nil || c == nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
