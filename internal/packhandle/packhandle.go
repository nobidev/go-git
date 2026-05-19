package packhandle

import (
	"errors"
	"io"
	"sync"

	billy "github.com/go-git/go-billy/v6"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
)

// ErrSourceUnconfigured indicates that a Source has been
// deliberately left without an Open/Size implementation. Returned
// by PackHandle methods when a code path reaches a Source whose
// contract the caller did not intend to honour (typically idx/rev
// on a PackHandle built only for .pack access).
var ErrSourceUnconfigured = errors.New("packhandle: source is unconfigured")

// PackReader is the union of methods Packfile needs against the
// .pack file. Read and Seek share a single cursor protected by an
// internal mutex; do not call Read concurrently with itself or with
// Seek on the same PackReader. ReadAt is intentionally hidden — the
// underlying file's ReadAt is concurrent-safe, but callers needing
// parallel random access acquire additional PackReaders via
// PackHandle.OpenPackReader rather than sharing one cursor.
type PackReader interface {
	io.Reader
	io.Seeker
	io.Closer
}

// Source provides on-demand access to one file in a pack triple.
// PackHandle.New takes a Sources value (one Source each for pack,
// idx, rev). Pack is mandatory; Idx and Rev may be zero values, in
// which case PackHandle.Index returns ErrSourceUnconfigured.
//
// Open returns a fresh concurrent-safe billy.File. The returned
// handle's Close is the disposal point; sharedFile invokes Close
// after the grace period or on terminal Close.
//
// Size returns the file's size in bytes. May be called lazily.
type Source struct {
	Open func() (billy.File, error)
	Size func() (int64, error)
}

// Sources bundles the three Sources required to construct a
// PackHandle. Pack is required; Idx and Rev are optional — a zero
// Source means "unconfigured" and causes Index() to return
// ErrSourceUnconfigured.
type Sources struct {
	Pack, Idx, Rev Source
}

// PathSource returns a Source backed by a billy.Basic + path.
func PathSource(fs billy.Basic, path string) Source {
	return Source{
		Open: func() (billy.File, error) {
			return fs.Open(path)
		},
		Size: func() (int64, error) {
			info, err := fs.Stat(path)
			if err != nil {
				return 0, err
			}
			return info.Size(), nil
		},
	}
}

// PackMeta is the pack-level metadata PackHandle caches on first
// request. Pack files are immutable post-creation, so this can be
// computed once and reused.
type PackMeta struct {
	ID      plumbing.Hash // footer hash
	Version uint32        // header version
	Count   uint32        // header object count
}

// PackHandle owns the file-descriptor lifecycle for the .pack file
// of one pack triple, and lazily constructs an idxfile.LazyIndex
// from the configured idx/rev Sources. All methods are safe for
// concurrent use.
type PackHandle struct {
	sources  Sources
	packHash plumbing.Hash // pinned at construction
	hashSize int           // derived from len(packHash); passed to LazyIndex/Meta
	pack     *sharedFile

	meta func() (PackMeta, error) // wired in subsequent commit

	// Index caching uses sync.Once + result fields (NOT sync.OnceValues)
	// so Close() can check indexVal directly without triggering lazy
	// init.
	indexOnce sync.Once
	indexVal  idxfile.Index
	indexErr  error

	closeOnce sync.Once
}

// New constructs a PackHandle. Pack is mandatory; Idx and Rev may
// be zero values (Index() then returns ErrSourceUnconfigured).
// Panics if sources.Pack.Open or sources.Pack.Size is nil. No I/O
// occurs at construction.
//
// packHash is the expected footer hash of the .pack file; pinning
// it at construction lets Meta() verify and lets Index() pass it
// to idxfile.NewLazyIndex.
func New(sources Sources, packHash plumbing.Hash) *PackHandle {
	if sources.Pack.Open == nil {
		panic("packhandle: New: Pack.Open is nil")
	}
	if sources.Pack.Size == nil {
		panic("packhandle: New: Pack.Size is nil")
	}
	h := &PackHandle{
		sources:  sources,
		packHash: packHash,
		hashSize: packHash.Size(),
		pack:     newSharedFile(sources.Pack.Open),
	}
	// meta is wired by the next commit (cd702a2c equivalent).
	h.meta = func() (PackMeta, error) {
		return PackMeta{}, errors.New("packhandle: Meta wired in subsequent commit")
	}
	return h
}

// OpenPackReader returns a refcount-holding reader over the .pack
// file. See PackReader for concurrency contract.
func (h *PackHandle) OpenPackReader() (PackReader, error) {
	ra, err := h.pack.acquire()
	if err != nil {
		return nil, err
	}
	return newCursorReader(ra, h.pack.release, h.sources.Pack.Size), nil
}

// Meta returns the cached pack metadata, computing it on first
// call. Cached for the PackHandle's lifetime.
func (h *PackHandle) Meta() (PackMeta, error) {
	return h.meta()
}

// Index returns the cached idxfile.Index for this pack, building
// it on first call from sources.Idx / sources.Rev. Returns
// ErrSourceUnconfigured if either source is unconfigured. Wired in
// a subsequent commit.
func (h *PackHandle) Index() (idxfile.Index, error) {
	return nil, errors.New("packhandle: Index wired in subsequent commit")
}

// Close marks the pack sharedFile permanently closed and releases
// any cached LazyIndex. Idempotent. Does NOT trigger lazy Index
// init — only closes the LazyIndex if Index() was called and
// produced a non-nil value.
func (h *PackHandle) Close() error {
	var err error
	h.closeOnce.Do(func() {
		// indexVal is non-nil iff Index() was called and the build
		// succeeded. If Index() was never called (or errored),
		// indexVal is nil and we skip without doing a lazy build.
		if h.indexVal != nil {
			if closer, ok := h.indexVal.(io.Closer); ok {
				err = errors.Join(err, closer.Close())
			}
		}
		err = errors.Join(err, h.pack.Close())
	})
	return err
}
