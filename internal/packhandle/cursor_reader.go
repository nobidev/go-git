package packhandle

import (
	"io"
	"sync"
)

// cursorReader is the Read-synth adapter that wraps a billy.File
// (random-access) plus a cursor into a Read+Seek+ReadAt+Closer
// concrete type. It is the underlying concrete value behind the
// PackReader interface returned by PackHandle.OpenPackReader.
//
// Concurrency:
//   - ReadAt does not touch the cursor mutex; concurrent ReadAts
//     proceed in parallel via the underlying handle.
//   - Read and Seek share a cursor protected by mu. Read is NOT
//     concurrent-safe with itself or with Seek on the SAME
//     cursorReader. The mutex prevents data races, but interleaving
//     Read calls from multiple goroutines yields semantically
//     meaningless results — callers should each acquire their own
//     cursorReader via PackHandle.OpenPackReader.
//   - Close is idempotent. First call returns nil; second returns
//     os.ErrClosed. Close does NOT nil the underlying ra — that
//     would open a nil-deref window against concurrent in-flight
//     ReadAt. Instead Close sets a closed flag and releases the
//     sharedFile refcount exactly once via releaseOnce.
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
	// stub — implemented in subsequent commit.
	return 0, nil
}

func (r *cursorReader) ReadAt(p []byte, off int64) (n int, err error) {
	// stub — implemented in subsequent commit.
	return 0, nil
}

func (r *cursorReader) Seek(offset int64, whence int) (int64, error) {
	// stub — implemented in subsequent commit.
	return 0, nil
}

func (r *cursorReader) Close() error {
	// stub — implemented in subsequent commit.
	return nil
}

// Compile-time interface satisfaction.
var (
	_ PackReader = (*cursorReader)(nil)
)
