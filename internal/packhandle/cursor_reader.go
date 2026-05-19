package packhandle

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// cursorReader is the Read-synth adapter that wraps an io.ReaderAt
// plus a cursor into a Read+Seek+ReadAt+Closer concrete type. It is
// the underlying concrete value behind the PackReader interface
// returned by PackHandle.OpenPackReader.
//
// Concurrency:
//   - ReadAt takes the cursor mutex only briefly to check the closed
//     flag; it does NOT hold mu across the underlying I/O. Concurrent
//     ReadAts on the same cursorReader therefore proceed in parallel
//     via the underlying handle.
//   - Read and Seek share a cursor protected by mu. Read is NOT
//     concurrent-safe with itself or with Seek on the SAME
//     cursorReader. The mutex prevents data races, but interleaving
//     Read calls from multiple goroutines yields semantically
//     meaningless results — callers should each acquire their own
//     cursorReader via PackHandle.OpenPackReader.
//   - Close is idempotent: first call returns nil, subsequent calls
//     return os.ErrClosed. Close does NOT nil the underlying ra
//     (that would open a nil-deref window against concurrent
//     in-flight ReadAt); instead it sets a closed flag and releases
//     the sharedFile refcount exactly once via releaseOnce.
//
// Close-vs-ReadAt: Close may race with ReadAt on a DIFFERENT
// cursorReader backed by the same sharedFile — the sibling's
// refcount keeps the underlying file open until its own Close.
// Close on the SAME cursorReader as an in-flight ReadAt is NOT
// safe under terminal sharedFile.Close conditions: if this Close
// drops the last refcount on a permanently-closed sharedFile, the
// sharedFile synchronously closes the underlying file. With an
// mmap-backed billy.File, that unmaps the region the in-flight
// ReadAt is still reading from, which is a SIGBUS-class hazard.
// The caller must serialise Close with its own outstanding ReadAts
// on the same cursorReader.
//
// In summary, the safe patterns are:
//   - Concurrent ReadAt across different cursorReaders sharing one
//     sharedFile.
//   - Concurrent ReadAt calls on the same cursorReader.
//   - Close on one cursorReader concurrent with ReadAt on a sibling
//     cursorReader (same sharedFile).
//
// And the unsafe pattern is:
//   - Close on a cursorReader concurrent with ReadAt (or Read) on
//     that same cursorReader.
//
// sizeFn is called lazily by Seek(SeekEnd). Most consumers
// (Scanner.SeekFromStart, FSObject.Seek(offset, SeekStart)) never
// invoke it, so the cost stays out of the hot path.
type cursorReader struct {
	ra      io.ReaderAt
	release func() // calls sharedFile.release; guarded by releaseOnce
	sizeFn  func() (int64, error)

	releaseOnce sync.Once

	mu     sync.Mutex
	cursor int64
	closed bool
}

func newCursorReader(ra io.ReaderAt, release func(), sizeFn func() (int64, error)) *cursorReader {
	return &cursorReader{ra: ra, release: release, sizeFn: sizeFn}
}

func (r *cursorReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, os.ErrClosed
	}
	off := r.cursor
	r.mu.Unlock()

	n, err = r.ra.ReadAt(p, off)

	r.mu.Lock()
	r.cursor += int64(n)
	r.mu.Unlock()

	return n, err
}

func (r *cursorReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return 0, os.ErrClosed
	}
	return r.ra.ReadAt(p, off)
}

func (r *cursorReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.cursor + offset
	case io.SeekEnd:
		size, err := r.sizeFn()
		if err != nil {
			return 0, fmt.Errorf("packhandle: cursor seek-end: %w", err)
		}
		abs = size + offset
	default:
		return 0, fmt.Errorf("packhandle: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("packhandle: negative seek position %d", abs)
	}
	r.cursor = abs
	return abs, nil
}

func (r *cursorReader) Close() error {
	var firstClose bool
	r.releaseOnce.Do(func() {
		firstClose = true
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		if r.release != nil {
			r.release()
		}
	})
	if firstClose {
		return nil
	}
	return os.ErrClosed
}

// Compile-time interface satisfaction.
var (
	_ PackReader = (*cursorReader)(nil)
)
