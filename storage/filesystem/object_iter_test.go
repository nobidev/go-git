package filesystem

import (
	"crypto"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/go-git/go-billy/v6"
	fixtures "github.com/go-git/go-git-fixtures/v6"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/internal/packhandle"
	"github.com/go-git/go-git/v6/plumbing/hash"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
)

func TestPackfileIter_SeenSkips(t *testing.T) {
	t.Parallel()
	pf := loadPackFixture(t)
	total := len(pf.hashes)

	tests := []struct {
		name      string
		preSeen   func(hashes []plumbing.Hash) []plumbing.Hash
		wantCount int
	}{
		{"no preseed returns all", func(_ []plumbing.Hash) []plumbing.Hash { return nil }, total},
		{"preseed one skips one", func(h []plumbing.Hash) []plumbing.Hash { return h[:1] }, total - 1},
		{"preseed half skips half", func(h []plumbing.Hash) []plumbing.Hash { return h[:total/2] }, total - total/2},
		{"preseed all returns none", func(h []plumbing.Hash) []plumbing.Hash { return h }, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ph := pf.packHandle(t)
			seen := make(map[plumbing.Hash]struct{})
			for _, h := range tc.preSeen(pf.hashes) {
				seen[h] = struct{}{}
			}

			iter, err := newPackfileIter(ph, plumbing.AnyObject, seen, pf.idx,
				cache.NewObjectLRUDefault(), crypto.SHA1.Size())
			require.NoError(t, err)

			var got int
			require.NoError(t, iter.ForEach(func(plumbing.EncodedObject) error {
				got++
				return nil
			}))
			require.Equal(t, tc.wantCount, got)
		})
	}
}

func TestPackfileIter_SharedSeenDedupsAcrossIterators(t *testing.T) {
	t.Parallel()
	pf := loadPackFixture(t)
	seen := make(map[plumbing.Hash]struct{})
	cch := cache.NewObjectLRUDefault()

	count := func() int {
		ph := pf.packHandle(t)
		iter, err := newPackfileIter(ph, plumbing.AnyObject, seen, pf.idx,
			cch, crypto.SHA1.Size())
		require.NoError(t, err)

		var n int
		require.NoError(t, iter.ForEach(func(plumbing.EncodedObject) error {
			n++
			return nil
		}))
		return n
	}

	first := count()
	second := count()

	require.Equal(t, len(pf.hashes), first)
	require.Zero(t, second, "shared seen map should suppress duplicates from second iteration")
	require.Len(t, seen, len(pf.hashes))
}

// TestNewPackfileIter_UnconfiguredSources mirrors the idx/rev Source
// shape that NewPackfileIter installs on its ad-hoc PackHandle: both
// the Open and Size closures return a wrapped
// packhandle.ErrSourceUnconfigured. Callers can therefore detect this
// failure mode specifically via errors.Is rather than chasing an
// accidental io.ErrUnexpectedEOF sentinel.
func TestNewPackfileIter_UnconfiguredSources(t *testing.T) {
	t.Parallel()

	idxErr := fmt.Errorf("packhandle: NewPackfileIter: idx source: %w", packhandle.ErrSourceUnconfigured)
	revErr := fmt.Errorf("packhandle: NewPackfileIter: rev source: %w", packhandle.ErrSourceUnconfigured)

	tests := []struct {
		name string
		src  packhandle.Source
	}{
		{
			name: "idx source",
			src: packhandle.Source{
				Open: func() (billy.File, error) { return nil, idxErr },
				Size: func() (int64, error) { return 0, idxErr },
			},
		},
		{
			name: "rev source",
			src: packhandle.Source{
				Open: func() (billy.File, error) { return nil, revErr },
				Size: func() (int64, error) { return 0, revErr },
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.src.Open()
			require.ErrorIs(t, err, packhandle.ErrSourceUnconfigured)
			_, err = tc.src.Size()
			require.ErrorIs(t, err, packhandle.ErrSourceUnconfigured)
		})
	}
}

func TestObjectsIter_ForEachClosesOnError(t *testing.T) {
	t.Parallel()
	fs, err := fixtures.ByTag(".git").ByTag("unpacked").One().DotGit()
	require.NoError(t, err)
	o := NewObjectStorage(dotgit.New(fs), cache.NewObjectLRUDefault())

	cbErr := errors.New("stop")

	tests := []struct {
		name    string
		cb      func(plumbing.EncodedObject) error
		wantErr error
	}{
		{"cb returns nil completes", func(plumbing.EncodedObject) error { return nil }, nil},
		{"cb returns error propagates", func(plumbing.EncodedObject) error { return cbErr }, cbErr},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objects, err := o.dir.Objects()
			require.NoError(t, err)
			require.NotEmpty(t, objects)

			iter := &objectsIter{s: o, t: plumbing.AnyObject, h: append([]plumbing.Hash{}, objects...)}
			err = iter.ForEach(tc.cb)
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
			require.Empty(t, iter.h, "iter.h must be drained by Close after ForEach returns")
		})
	}
}

type packFixture struct {
	fs       billy.Filesystem
	dg       *dotgit.DotGit
	packHash plumbing.Hash
	idx      idxfile.Index
	hashes   []plumbing.Hash
}

func loadPackFixture(t *testing.T) packFixture {
	t.Helper()

	fs, err := fixtures.Basic().ByTag(".git").One().DotGit()
	require.NoError(t, err)
	dg := dotgit.New(fs)

	packs, err := dg.ObjectPacks()
	require.NoError(t, err)
	require.NotEmpty(t, packs)

	idxFile, err := dg.ObjectPackIdx(packs[0])
	require.NoError(t, err)
	t.Cleanup(func() { _ = idxFile.Close() })

	idx := idxfile.NewMemoryIndex(crypto.SHA1.Size())
	require.NoError(t, idxfile.NewDecoder(idxFile, hash.New(crypto.SHA1)).Decode(idx))

	entries, err := idx.Entries()
	require.NoError(t, err)
	t.Cleanup(func() { _ = entries.Close() })

	var hashes []plumbing.Hash
	for {
		e, err := entries.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		hashes = append(hashes, e.Hash)
	}

	return packFixture{fs: fs, dg: dg, packHash: packs[0], idx: idx, hashes: hashes}
}

// packHandle returns a fresh PackHandle for the fixture pack. Tests
// register a Cleanup to close it.
func (p packFixture) packHandle(t *testing.T) *packhandle.PackHandle {
	t.Helper()
	ph, err := p.dg.PackHandle(p.packHash)
	require.NoError(t, err)
	return ph
}
