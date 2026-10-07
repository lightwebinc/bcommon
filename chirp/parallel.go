package chirp

import (
	"crypto/sha256"
	"errors"
	"hash"
	"runtime"
	"sync"
	"sync/atomic"
)

// MaxWorkers bounds the blob hashers of a ParallelChunker and of
// BuildParallel.
const MaxWorkers = 64

// DefaultWorkers is the number of blob hashers used when none is given: the
// cores the process may use, at most 8.
func DefaultWorkers() int { return min(max(runtime.GOMAXPROCS(0), 1), 8) }

func clampWorkers(n int) int {
	if n <= 0 {
		return DefaultWorkers()
	}
	return min(n, MaxWorkers)
}

// ErrFinished is a Write, Finish or Abort on a ParallelChunker after
// Finish or Abort.
var ErrFinished = errors.New("chirp: chunker already finished")

// errAborted is what Write and Finish return after Abort.
var errAborted = errors.New("chirp: chunker aborted")

// ParallelChunker is Chunker with the hashing spread over several cores:
// each complete blob is hashed (the SHA-256 that names it) and handed to
// emit by one of workers goroutines, while one more goroutine hashes the
// content in order. Write only copies into the current blob, so the
// caller's loop waits for a hash only when every buffer is in use. Memory
// holds at most workers + 2 blobs.
//
// Finish returns the closure Chunker returns for the same content, byte for
// byte.
//
// emit is called concurrently, from the worker goroutines, once per blob,
// in no particular order: i is the blob's index in content order and ref
// its reference. The slice is reused after emit returns. emit is where a
// caller does its own per-blob work on the same core, such as
// commit.SegmentRoot of the blob for a byte-leaf tree (see
// commit.SegmentRoots). The first error from emit stops the build: later
// blobs are not hashed or emitted, and Write and Finish return it.
//
// A ParallelChunker is used from one goroutine; emit runs on others. Call
// Finish or Abort once, or its goroutines are never released.
type ParallelChunker struct {
	emit func(i int, ref Child, blob []byte) error

	free    chan []byte
	work    chan pjob
	content chan pjob
	wg      sync.WaitGroup
	cur     []byte
	blobs   int
	done    bool

	mu   sync.Mutex
	refs []Child
	err  error

	contentHash [32]byte
}

type pjob struct {
	i    int
	blob []byte
	refs *atomic.Int32
}

// NewParallelChunker returns a ParallelChunker with workers blob hashers
// (DefaultWorkers when 0 or less, at most MaxWorkers) that hands each blob
// to emit.
func NewParallelChunker(workers int, emit func(i int, ref Child, blob []byte) error) *ParallelChunker {
	workers = clampWorkers(workers)
	c := &ParallelChunker{
		emit:    emit,
		free:    make(chan []byte, workers+2),
		work:    make(chan pjob, workers+2),
		content: make(chan pjob, workers+2),
	}
	for range workers + 2 {
		c.free <- make([]byte, 0, ChunkSize)
	}
	c.wg.Add(workers + 1)
	for range workers {
		go c.worker()
	}
	go c.hashContent()
	return c
}

func (c *ParallelChunker) failed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *ParallelChunker) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

// release returns a buffer to the free list once both its hasher and the
// content hasher are done with it.
func (c *ParallelChunker) release(j pjob) {
	if j.refs.Add(-1) == 0 {
		c.free <- j.blob[:0]
	}
}

func (c *ParallelChunker) worker() {
	defer c.wg.Done()
	for j := range c.work {
		if c.failed() == nil {
			ref := Child{Kind: ChildBlob, Length: uint64(len(j.blob)), Hash: sha256.Sum256(j.blob)}
			c.mu.Lock()
			c.refs[j.i] = ref
			c.mu.Unlock()
			if err := c.emit(j.i, ref, j.blob); err != nil {
				c.fail(err)
			}
		}
		c.release(j)
	}
}

func (c *ParallelChunker) hashContent() {
	defer c.wg.Done()
	var h hash.Hash = sha256.New()
	for j := range c.content {
		h.Write(j.blob)
		c.release(j)
	}
	c.contentHash = [32]byte(h.Sum(nil))
}

func (c *ParallelChunker) dispatch() {
	b := c.cur
	c.cur = nil
	i := c.blobs
	c.blobs++
	c.mu.Lock()
	c.refs = append(c.refs, Child{})
	c.mu.Unlock()
	refs := new(atomic.Int32)
	refs.Store(2)
	c.content <- pjob{i: i, blob: b, refs: refs}
	c.work <- pjob{i: i, blob: b, refs: refs}
}

// Write takes content in order. After an error from emit, it returns that
// error, having taken the bytes counted by n.
func (c *ParallelChunker) Write(p []byte) (int, error) {
	if c.done {
		return 0, ErrFinished
	}
	if err := c.failed(); err != nil {
		return 0, err
	}
	n := len(p)
	for len(p) > 0 {
		if c.cur == nil {
			c.cur = <-c.free
		}
		k := min(ChunkSize-len(c.cur), len(p))
		c.cur = append(c.cur, p[:k]...)
		p = p[k:]
		if len(c.cur) == ChunkSize {
			c.dispatch()
			if err := c.failed(); err != nil {
				return n - len(p), err
			}
		}
	}
	return n, nil
}

func (c *ParallelChunker) stop() {
	c.done = true
	close(c.work)
	close(c.content)
	c.wg.Wait()
}

// Finish hands over the last blob, waits for every blob, and returns the
// closure's root and branches; Built.Blobs is nil, since every blob went to
// emit.
func (c *ParallelChunker) Finish(ext []Extension) (Built, error) {
	if c.done {
		return Built{}, ErrFinished
	}
	if len(c.cur) > 0 && c.failed() == nil {
		c.dispatch()
	}
	c.stop()
	if c.err != nil {
		return Built{}, c.err
	}
	return BuildTree(c.refs, c.contentHash, ext)
}

// Abort stops the chunker without a result and waits for its goroutines;
// emit is not called again once Abort returns. It is safe after Finish.
func (c *ParallelChunker) Abort() {
	if c.done {
		return
	}
	c.fail(errAborted)
	c.stop()
}

// BuildParallel is Build with the blobs and the content hashed on up to
// workers + 1 cores (workers as NewParallelChunker takes it). It returns
// what Build returns, byte for byte, and holds nothing beyond it: the blobs
// are slices of content.
func BuildParallel(content []byte, ext []Extension, workers int) (Built, error) {
	workers = clampWorkers(workers)
	n := (len(content) + ChunkSize - 1) / ChunkSize
	blobs := make([][]byte, n)
	refs := make([]Child, n)
	for i := range blobs {
		off := i * ChunkSize
		blobs[i] = content[off:min(off+ChunkSize, len(content))]
	}
	var contentHash [32]byte
	var wg sync.WaitGroup
	wg.Go(func() { contentHash = sha256.Sum256(content) })
	var next atomic.Int64
	for range min(workers, n) {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				refs[i] = Child{Kind: ChildBlob, Length: uint64(len(blobs[i])), Hash: sha256.Sum256(blobs[i])}
			}
		})
	}
	wg.Wait()
	out, err := BuildTree(refs, contentHash, ext)
	if err != nil {
		return Built{}, err
	}
	if n > 0 {
		out.Blobs = blobs
	}
	return out, nil
}
