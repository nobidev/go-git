package packhandle

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	billy "github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-billy/v6/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
)

// buildEmptyPack returns (packBytes, packHash) for a minimal valid
// pack with zero objects. Pack version 2, empty object section,
// SHA1 footer hash over the 12-byte header.
func buildEmptyPack(t *testing.T) ([]byte, plumbing.Hash) {
	t.Helper()
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], 0)
	sum := sha1.Sum(header)
	pack := append([]byte{}, header...)
	pack = append(pack, sum[:]...)
	var h plumbing.Hash
	h.ResetBySize(20)
	_, err := h.Write(sum[:])
	require.NoError(t, err)
	return pack, h
}

// instrumentedSource wraps a PathSource with atomic counters for
// opens and size queries, used by timer-independence tests.
func instrumentedSource(fs billy.Basic, path string) (Source, *atomic.Int32, *atomic.Int32) {
	var opens, sizes atomic.Int32
	base := PathSource(fs, path)
	return Source{
		Open: func() (billy.File, error) {
			opens.Add(1)
			return base.Open()
		},
		Size: func() (int64, error) {
			sizes.Add(1)
			return base.Size()
		},
	}, &opens, &sizes
}

func writeMemFile(t *testing.T, fs billy.Basic, path string, data []byte) {
	t.Helper()
	require.NoError(t, util.WriteFile(fs, path, data, 0o644))
}

// newMemPackHandle builds a PackHandle with all three sources
// configured against an in-memory FS. The packHash is ZeroHash —
// tests that need Meta hash verification construct a custom
// PackHandle with the real footer hash.
func newMemPackHandle(t *testing.T, packBytes, idxBytes, revBytes []byte) (*PackHandle, billy.Basic) {
	t.Helper()
	fs := memfs.New()
	writeMemFile(t, fs, "pack-deadbeef.pack", packBytes)
	writeMemFile(t, fs, "pack-deadbeef.idx", idxBytes)
	writeMemFile(t, fs, "pack-deadbeef.rev", revBytes)
	ph := New(Sources{
		Pack: PathSource(fs, "pack-deadbeef.pack"),
		Idx:  PathSource(fs, "pack-deadbeef.idx"),
		Rev:  PathSource(fs, "pack-deadbeef.rev"),
	}, plumbing.ZeroHash)
	return ph, fs
}

func TestPackHandle_LazyOpen(t *testing.T) {
	t.Parallel()
	ph, _ := newMemPackHandle(t, []byte("pack-data"), []byte("idx-data"), []byte("rev-data"))
	defer ph.Close()

	pr, err := ph.OpenPackReader()
	require.NoError(t, err)
	defer pr.Close()

	buf := make([]byte, 9)
	n, err := pr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 9, n)
	assert.Equal(t, "pack-data", string(buf))
}

func TestPackHandle_RefcountedShare(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("pack-data"))
	writeMemFile(t, fs, "p.idx", []byte("idx-data"))
	writeMemFile(t, fs, "p.rev", []byte("rev-data"))

	packSrc, opens, _ := instrumentedSource(fs, "p.pack")
	ph := New(Sources{
		Pack: packSrc,
		Idx:  PathSource(fs, "p.idx"),
		Rev:  PathSource(fs, "p.rev"),
	}, plumbing.ZeroHash)
	defer ph.Close()

	r1, err := ph.OpenPackReader()
	require.NoError(t, err)
	r2, err := ph.OpenPackReader()
	require.NoError(t, err)

	// Both share one underlying open.
	assert.Equal(t, int32(1), opens.Load())

	// Independent cursors.
	b1 := make([]byte, 4)
	b2 := make([]byte, 4)
	_, err = r1.Read(b1)
	require.NoError(t, err)
	_, err = r2.Read(b2)
	require.NoError(t, err)
	assert.Equal(t, "pack", string(b1))
	assert.Equal(t, "pack", string(b2))

	assert.NoError(t, r1.Close())
	assert.NoError(t, r2.Close())
}

func TestPackHandle_GraceWarmCloseReopen(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fs := memfs.New()
		writeMemFile(t, fs, "p.pack", []byte("pack-data"))

		packSrc, packOpens, _ := instrumentedSource(fs, "p.pack")
		ph := New(Sources{Pack: packSrc}, plumbing.ZeroHash)
		defer ph.Close()

		// Open + close pack: opener count = 1, grace timer starts.
		pr, err := ph.OpenPackReader()
		require.NoError(t, err)
		require.NoError(t, pr.Close())
		assert.Equal(t, int32(1), packOpens.Load())

		// Within grace period (800ms): reopen reuses the FD.
		time.Sleep(800 * time.Millisecond)
		synctest.Wait()
		pr2, err := ph.OpenPackReader()
		require.NoError(t, err)
		assert.Equal(t, int32(1), packOpens.Load(), "pack should be warm")
		require.NoError(t, pr2.Close())

		// After grace period (>1s since last release), the FD closes.
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()
		pr3, err := ph.OpenPackReader()
		require.NoError(t, err)
		defer pr3.Close()
		assert.Equal(t, int32(2), packOpens.Load(), "pack should have re-opened")
	})
}

func TestPackHandle_IteratorHeldReader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fs := memfs.New()
		writeMemFile(t, fs, "p.pack", []byte("pack-data"))

		packSrc, packOpens, _ := instrumentedSource(fs, "p.pack")
		ph := New(Sources{Pack: packSrc}, plumbing.ZeroHash)
		defer ph.Close()

		pr, err := ph.OpenPackReader()
		require.NoError(t, err)

		// Past the grace period.
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()

		// Reader still works.
		buf := make([]byte, 4)
		n, err := pr.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, 4, n)
		assert.Equal(t, "pack", string(buf))

		// Opener was called exactly once (refcount kept FD alive).
		assert.Equal(t, int32(1), packOpens.Load())

		require.NoError(t, pr.Close())
	})
}

func TestPackHandle_CloseTerminal(t *testing.T) {
	t.Parallel()
	ph, _ := newMemPackHandle(t, []byte("pack-data"), []byte("idx-data"), []byte("rev-data"))
	require.NoError(t, ph.Close())

	_, err := ph.OpenPackReader()
	assert.ErrorIs(t, err, errSharedFileClosed)
}

func TestPackHandle_CloseIdempotent(t *testing.T) {
	t.Parallel()
	ph, _ := newMemPackHandle(t, []byte("pack-data"), []byte("idx-data"), []byte("rev-data"))
	require.NoError(t, ph.Close())
	require.NoError(t, ph.Close())
	require.NoError(t, ph.Close())
}

// TestPackHandle_NewAllowsZeroIdxRev verifies the loosened New
// contract: zero-value Sources.Idx and Sources.Rev are legal (used
// by the open-pack-only iter path in storage/filesystem).
func TestPackHandle_NewAllowsZeroIdxRev(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("pack-data"))

	// Idx and Rev fields are zero-value Sources — explicitly omitted.
	require.NotPanics(t, func() {
		ph := New(Sources{Pack: PathSource(fs, "p.pack")}, plumbing.ZeroHash)
		_ = ph.Close()
	})
}

func TestPackHandle_NewPanicsOnNilPack(t *testing.T) {
	t.Parallel()
	good := PathSource(memfs.New(), "missing")
	cases := []struct {
		name    string
		mutate  func(s *Sources)
		message string
	}{
		{
			name:    "Pack.Open nil",
			mutate:  func(s *Sources) { s.Pack.Open = nil },
			message: "Pack.Open",
		},
		{
			name:    "Pack.Size nil",
			mutate:  func(s *Sources) { s.Pack.Size = nil },
			message: "Pack.Size",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srcs := Sources{Pack: good}
			tc.mutate(&srcs)
			require.PanicsWithValue(t,
				"packhandle: New: "+tc.message+" is nil",
				func() { _ = New(srcs, plumbing.ZeroHash) },
			)
		})
	}

	// Zero-value Sources panics on Pack.Open (the first required check).
	require.Panics(t, func() { _ = New(Sources{}, plumbing.ZeroHash) })
}

func TestPackHandle_OpenError(t *testing.T) {
	t.Parallel()
	fs := memfs.New() // empty — file doesn't exist
	ph := New(Sources{Pack: PathSource(fs, "missing.pack")}, plumbing.ZeroHash)
	defer ph.Close()

	_, err := ph.OpenPackReader()
	assert.Error(t, err)
	assert.False(t, errors.Is(err, errSharedFileClosed))
}

func TestPackHandle_InMemorySourceAcceptsCustomOpen(t *testing.T) {
	t.Parallel()
	// Exercise the Source flexibility: a Source backed by an
	// in-memory bytes buffer (simulating dotgit's rev fallback). The
	// fixture must satisfy billy.File now.
	packBytes := []byte("synthetic-pack-content")
	customPack := Source{
		Open: func() (billy.File, error) {
			return &nopBillyFile{Reader: bytes.NewReader(packBytes)}, nil
		},
		Size: func() (int64, error) {
			return int64(len(packBytes)), nil
		},
	}

	ph := New(Sources{Pack: customPack}, plumbing.ZeroHash)
	defer ph.Close()

	pr, err := ph.OpenPackReader()
	require.NoError(t, err)
	defer pr.Close()

	buf := make([]byte, len(packBytes))
	n, err := pr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, len(packBytes), n)
	assert.Equal(t, packBytes, buf)
}

func TestPackHandle_SizeErrorNotCached(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("pack-data"))

	base := PathSource(fs, "p.pack")

	// First Size call returns a transient error; subsequent calls
	// delegate to the underlying PathSource and succeed.
	var calls atomic.Int32
	flakySize := Source{
		Open: base.Open,
		Size: func() (int64, error) {
			if calls.Add(1) == 1 {
				return 0, errors.New("transient stat failure")
			}
			return base.Size()
		},
	}

	ph := New(Sources{Pack: flakySize}, plumbing.ZeroHash)
	defer ph.Close()

	pr, err := ph.OpenPackReader()
	require.NoError(t, err)
	defer pr.Close()

	// First Seek(SeekEnd) propagates the transient error.
	_, err = pr.Seek(0, io.SeekEnd)
	require.Error(t, err)

	// Subsequent Seek(SeekEnd) must re-probe and succeed: the
	// transient error must NOT have been cached.
	end, err := pr.Seek(0, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(len("pack-data")), end)

	// Sanity: Size was invoked twice (once flaky, once successful).
	assert.GreaterOrEqual(t, calls.Load(), int32(2))
}

func TestPackHandle_ClosePartialInit_OpenPackReaderOnly(t *testing.T) {
	t.Parallel()
	ph, _ := newMemPackHandle(t, []byte("pack-data"), []byte("idx"), []byte("rev"))

	// Touch only OpenPackReader; Index() is never called.
	pr, err := ph.OpenPackReader()
	require.NoError(t, err)
	require.NoError(t, pr.Close())

	require.NoError(t, ph.Close(), "Close after only-pack-reader use should not panic")
	require.NoError(t, ph.Close(), "second Close should be idempotent")
}

func TestPackHandle_Meta_HappyPath(t *testing.T) {
	t.Parallel()
	packBytes, packHash := buildEmptyPack(t)
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", packBytes)

	ph := New(Sources{Pack: PathSource(fs, "p.pack")}, packHash)
	defer ph.Close()

	meta, err := ph.Meta()
	require.NoError(t, err)
	assert.Equal(t, uint32(2), meta.Version)
	assert.Equal(t, uint32(0), meta.Count)
	assert.True(t, meta.ID.Equal(packHash))
}

func TestPackHandle_Meta_Cached(t *testing.T) {
	t.Parallel()
	packBytes, packHash := buildEmptyPack(t)
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", packBytes)

	packSrc, _, sizes := instrumentedSource(fs, "p.pack")
	ph := New(Sources{Pack: packSrc}, packHash)
	defer ph.Close()

	for range 5 {
		_, err := ph.Meta()
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), sizes.Load(), "Size invoked exactly once across cached calls")
}

func TestPackHandle_Meta_HashMismatch(t *testing.T) {
	t.Parallel()
	packBytes, _ := buildEmptyPack(t)
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", packBytes)

	// Pin a deliberately wrong hash.
	var wrong plumbing.Hash
	wrong.ResetBySize(20)
	bogus := bytes.Repeat([]byte{0xff}, 20)
	_, err := wrong.Write(bogus)
	require.NoError(t, err)

	ph := New(Sources{Pack: PathSource(fs, "p.pack")}, wrong)
	defer ph.Close()

	_, err = ph.Meta()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match expected")
}

func TestPackHandle_Meta_TooShort(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("PACK")) // shorter than 12+hashSize
	ph := New(Sources{Pack: PathSource(fs, "p.pack")}, plumbing.ZeroHash)
	defer ph.Close()

	_, err := ph.Meta()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestPackHandle_Meta_BadSignature(t *testing.T) {
	t.Parallel()
	bad := make([]byte, 12+20)
	copy(bad, "NOPE")
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", bad)
	ph := New(Sources{Pack: PathSource(fs, "p.pack")}, plumbing.ZeroHash)
	defer ph.Close()

	_, err := ph.Meta()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad pack signature")
}

func TestPackHandle_Meta_ErrorCached(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("pack-too-short"))

	base := PathSource(fs, "p.pack")
	var calls atomic.Int32
	src := Source{
		Open: base.Open,
		Size: func() (int64, error) {
			calls.Add(1)
			return 0, errors.New("transient stat failure")
		},
	}
	ph := New(Sources{Pack: src}, plumbing.ZeroHash)
	defer ph.Close()

	for range 3 {
		_, err := ph.Meta()
		require.Error(t, err)
	}
	assert.Equal(t, int32(1), calls.Load(), "Meta error must be cached (sync.OnceValues)")
}

func TestPackHandle_Index_UnconfiguredIdx(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("pack"))
	// Only Pack configured; Idx and Rev are zero Sources.
	ph := New(Sources{Pack: PathSource(fs, "p.pack")}, plumbing.ZeroHash)
	defer ph.Close()

	idx, err := ph.Index()
	require.Nil(t, idx)
	assert.ErrorIs(t, err, ErrSourceUnconfigured)
	assert.Contains(t, err.Error(), "idx source")
}

func TestPackHandle_Index_UnconfiguredRev(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("pack"))
	writeMemFile(t, fs, "p.idx", []byte("idx"))
	// Idx configured; Rev is a zero Source.
	ph := New(Sources{
		Pack: PathSource(fs, "p.pack"),
		Idx:  PathSource(fs, "p.idx"),
	}, plumbing.ZeroHash)
	defer ph.Close()

	idx, err := ph.Index()
	require.Nil(t, idx)
	assert.ErrorIs(t, err, ErrSourceUnconfigured)
	assert.Contains(t, err.Error(), "rev source")
}

func TestPackHandle_Index_ErrorCached(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("pack"))

	var idxCalls atomic.Int32
	wantErr := errors.New("synthetic idx open failure")
	srcs := Sources{
		Pack: PathSource(fs, "p.pack"),
		Idx: Source{
			Open: func() (billy.File, error) {
				idxCalls.Add(1)
				return nil, wantErr
			},
			Size: func() (int64, error) { return 0, wantErr },
		},
		Rev: Source{
			Open: func() (billy.File, error) { return nil, wantErr },
			Size: func() (int64, error) { return 0, wantErr },
		},
	}
	ph := New(srcs, plumbing.ZeroHash)
	defer ph.Close()

	_, err1 := ph.Index()
	_, err2 := ph.Index()
	require.Error(t, err1)
	require.Error(t, err2)
	// Idx opener invoked exactly once across calls (sync.Once gate).
	assert.Equal(t, int32(1), idxCalls.Load(), "Index error must be cached")
}

func TestPackHandle_Index_Concurrent(t *testing.T) {
	t.Parallel()
	fs := memfs.New()
	writeMemFile(t, fs, "p.pack", []byte("pack"))

	var idxCalls atomic.Int32
	wantErr := errors.New("synthetic")
	srcs := Sources{
		Pack: PathSource(fs, "p.pack"),
		Idx: Source{
			Open: func() (billy.File, error) {
				idxCalls.Add(1)
				return nil, wantErr
			},
			Size: func() (int64, error) { return 0, wantErr },
		},
		Rev: Source{
			Open: func() (billy.File, error) { return nil, wantErr },
			Size: func() (int64, error) { return 0, wantErr },
		},
	}
	ph := New(srcs, plumbing.ZeroHash)
	defer ph.Close()

	const N = 32
	results := make([]idxfileIndexResult, N)
	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			idx, err := ph.Index()
			results[i] = idxfileIndexResult{idx: idx, err: err}
		})
	}
	wg.Wait()

	// All N goroutines observe the same (nil, error) tuple.
	for i := range N {
		assert.Nil(t, results[i].idx)
		require.Error(t, results[i].err)
		assert.Equal(t, results[0].err.Error(), results[i].err.Error())
	}
	// And the opener was called exactly once across all of them.
	assert.Equal(t, int32(1), idxCalls.Load(), "race-protected: exactly one build")
}

type idxfileIndexResult struct {
	idx any
	err error
}

func TestPackHandle_ClosePartialInit_NeitherCalled(t *testing.T) {
	t.Parallel()
	ph, _ := newMemPackHandle(t, []byte("pack-data"), []byte("idx"), []byte("rev"))

	// Close without ever calling OpenPackReader or Index.
	require.NoError(t, ph.Close())
	require.NoError(t, ph.Close())
}

// nopBillyFile is a billy.File backed by a *bytes.Reader. Write-side
// methods return os.ErrPermission to match the read-only contract.
type nopBillyFile struct {
	*bytes.Reader
}

func (n *nopBillyFile) Close() error                       { return nil }
func (n *nopBillyFile) Name() string                       { return "memory" }
func (n *nopBillyFile) Write([]byte) (int, error)          { return 0, errors.New("read-only") }
func (n *nopBillyFile) WriteAt([]byte, int64) (int, error) { return 0, errors.New("read-only") }
func (n *nopBillyFile) Truncate(int64) error               { return errors.New("read-only") }
func (n *nopBillyFile) Stat() (fs.FileInfo, error)         { return nil, errors.New("not implemented") }
