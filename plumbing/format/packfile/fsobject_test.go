package packfile_test

import (
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	billy "github.com/go-git/go-billy/v6"
	fixtures "github.com/go-git/go-git-fixtures/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/internal/fixtureutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/internal/packhandle"
)

// pickNonDeltaHash returns the hash of the first non-delta scanner
// entry in the fixture. Skips the test if no non-delta entry is
// available.
func pickNonDeltaHash(t testing.TB, f *fixtures.Fixture) plumbing.Hash {
	t.Helper()
	for _, e := range fixtureutil.ScannerEntries(f) {
		if e.Type.IsDelta() {
			continue
		}
		return e.Hash
	}
	t.Skip("fixture has no non-delta entries")
	return plumbing.ZeroHash
}

// instrumentedPackHandleFromFixture mirrors packHandleFromFixture but
// counts opens against the pack file. Used to verify FD refcount
// semantics across multiple Reader() calls.
func instrumentedPackHandleFromFixture(t testing.TB, f *fixtures.Fixture) (*packhandle.PackHandle, *atomic.Int32) {
	t.Helper()
	stem := fmt.Sprintf("data/pack-%s", f.PackfileHash)
	var opens atomic.Int32
	base := packhandle.PathSource(fixtures.Filesystem, stem+".pack")
	packSrc := packhandle.Source{
		Open: func() (billy.File, error) {
			opens.Add(1)
			return base.Open()
		},
		Size: base.Size,
	}
	packHash, ok := plumbing.FromHex(f.PackfileHash)
	require.True(t, ok, "fixture packfile hash unparseable: %q", f.PackfileHash)
	ph := packhandle.New(packhandle.Sources{
		Pack: packSrc,
		Idx:  packhandle.PathSource(fixtures.Filesystem, stem+".idx"),
		Rev:  packhandle.PathSource(fixtures.Filesystem, stem+".rev"),
	}, packHash)
	t.Cleanup(func() { _ = ph.Close() })
	return ph, &opens
}

// fsObjectFromPackfile resolves the given hash via Packfile.Get and
// returns the resulting object — which for non-delta objects is an
// FSObject backed by the PackHandle.
func fsObjectFromPackfile(t testing.TB, ph *packhandle.PackHandle, f *fixtures.Fixture, h plumbing.Hash) plumbing.EncodedObject {
	t.Helper()
	idx := getIndexFromFixture(t, f)
	p := packfile.NewPackfile(ph, packfile.WithIdx(idx))
	obj, err := p.Get(h)
	require.NoError(t, err)
	return obj
}

func TestFSObject_Reader(t *testing.T) {
	t.Parallel()
	f := fixtures.Basic().One()
	h := pickNonDeltaHash(t, f)

	ph := packHandleFromFixture(t, f)
	obj := fsObjectFromPackfile(t, ph, f, h)

	r, err := obj.Reader()
	require.NoError(t, err)
	defer r.Close()

	buf, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NotEmpty(t, buf)
	assert.Equal(t, obj.Size(), int64(len(buf)))
}

func TestFSObject_ReaderCloseReleasesRefcount(t *testing.T) {
	t.Parallel()
	f := fixtures.Basic().One()
	h := pickNonDeltaHash(t, f)

	ph, opens := instrumentedPackHandleFromFixture(t, f)
	obj := fsObjectFromPackfile(t, ph, f, h)

	// The first Get opened the pack to read header + footer via
	// PackHandle.Meta, plus a Scanner open. Record that baseline.
	preOpens := opens.Load()

	r1, err := obj.Reader()
	require.NoError(t, err)
	require.NoError(t, r1.Close())
	first := opens.Load()
	assert.GreaterOrEqual(t, first, preOpens, "first Reader() may or may not need a fresh open within grace")

	// Within the SharedFile grace period, a second Reader() reuses
	// the same underlying FD.
	r2, err := obj.Reader()
	require.NoError(t, err)
	require.NoError(t, r2.Close())
	assert.Equal(t, first, opens.Load(), "second open should reuse the FD within grace")
}

func TestFSObject_MultipleSequentialReaders(t *testing.T) {
	t.Parallel()
	f := fixtures.Basic().One()
	h := pickNonDeltaHash(t, f)

	ph := packHandleFromFixture(t, f)
	obj := fsObjectFromPackfile(t, ph, f, h)

	var firstBytes []byte
	for i := 0; i < 3; i++ {
		r, err := obj.Reader()
		require.NoError(t, err)
		buf, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		if firstBytes == nil {
			firstBytes = buf
		} else {
			assert.Equal(t, firstBytes, buf, "iteration %d returned different bytes", i)
		}
	}
}

func TestFSObject_WorksAfterGraceTimer(t *testing.T) {
	t.Parallel()
	f := fixtures.Basic().One()
	h := pickNonDeltaHash(t, f)

	synctest.Test(t, func(t *testing.T) {
		ph, opens := instrumentedPackHandleFromFixture(t, f)
		obj := fsObjectFromPackfile(t, ph, f, h)

		r1, err := obj.Reader()
		require.NoError(t, err)
		_, err = io.ReadAll(r1)
		require.NoError(t, err)
		require.NoError(t, r1.Close())
		preGrace := opens.Load()

		// Past the grace period; FD is closed.
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()

		// Reader still works — opener is re-invoked.
		r2, err := obj.Reader()
		require.NoError(t, err)
		buf, err := io.ReadAll(r2)
		require.NoError(t, err)
		require.NotEmpty(t, buf)
		require.NoError(t, r2.Close())

		assert.Greater(t, opens.Load(), preGrace, "opener should have re-fired after grace timer")
	})
}
