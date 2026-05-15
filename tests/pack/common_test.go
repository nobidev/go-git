package pack_test

import (
	"crypto"
	"io"
	"testing"

	billy "github.com/go-git/go-billy/v6"
	"github.com/stretchr/testify/require"

	_ "unsafe"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/internal/packhandle"
	"github.com/go-git/go-git/v6/plumbing/hash"
)

// fixtureTriple is satisfied by both fixtures.Fixture and
// fixtures.OSFixture: each exposes the three pack-triple file
// accessors.
type fixtureTriple interface {
	Packfile() (billy.File, error)
	Idx() (billy.File, error)
	Rev() (billy.File, error)
}

// openFixtureTriple opens .pack/.idx/.rev for the given fixture and
// registers cleanup that closes all three.
func openFixtureTriple(t *testing.T, f fixtureTriple) (pack, idx, rev billy.File) {
	t.Helper()
	pack, err := f.Packfile()
	require.NoError(t, err)
	idx, err = f.Idx()
	require.NoError(t, err)
	rev, err = f.Rev()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = pack.Close()
		_ = idx.Close()
		_ = rev.Close()
	})
	return pack, idx, rev
}

// The existing implementation uses int64, instead of uint64, which is
// the appropriate type to represent offsets. To limit the amount of changes
// this generic interface will be used to enable both types being represented.
// In the future, the use of int64 will need to be replaced by uint64.
type int64OrUint64 interface {
	~int64 | ~uint64
}

type packHandler[T int64OrUint64] interface {
	io.Closer

	FindOffset(h plumbing.Hash) (T, error)
	FindHash(offset T) (plumbing.Hash, error)
	Get(h plumbing.Hash) (plumbing.EncodedObject, error)
	GetByOffset(offset T) (plumbing.EncodedObject, error)
}

// newPackfileOpts builds a Packfile backed by an ad-hoc PackHandle
// over the three fixture billy.Files. The rev file is required —
// PackHandle composes all three triple members.
func newPackfileOpts(pack, idx, rev billy.File, opts ...packfile.PackfileOption) packHandler[int64] {
	i := idxfile.NewMemoryIndex(crypto.SHA1.Size())

	_, err := idx.Seek(0, io.SeekStart)
	if err != nil {
		panic(err)
	}

	err = idxfile.NewDecoder(idx, hash.New(crypto.SHA1)).Decode(i)
	if err != nil {
		panic(err)
	}

	ph := packhandle.New(packhandle.Sources{
		Pack: fileSource(pack),
		Idx:  fileSource(idx),
		Rev:  fileSource(rev),
	}, plumbing.ZeroHash)

	opts = append(opts, packfile.WithIdx(i))
	return packfile.NewPackfile(ph, opts...)
}

// fileSource wraps a billy.File in a single-shot Source that
// surfaces it on the first Open and refuses subsequent Opens. The
// test harness pre-opens the three files once and the SharedFile
// keeps the FD warm for the test's lifetime via its grace period;
// if the FD were ever released and re-acquired past grace, the test
// would fail loudly. Tests close the underlying billy.File via
// their own Cleanup.
func fileSource(f billy.File) packhandle.Source {
	opened := false
	return packhandle.Source{
		Open: func() (billy.File, error) {
			if opened {
				return nil, io.ErrUnexpectedEOF
			}
			opened = true
			return f, nil
		},
		Size: func() (int64, error) {
			info, err := f.Stat()
			if err != nil {
				return 0, err
			}
			return info.Size(), nil
		},
	}
}
