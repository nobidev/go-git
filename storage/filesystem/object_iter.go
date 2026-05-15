package filesystem

import (
	"crypto"
	"fmt"
	"io"

	billy "github.com/go-git/go-billy/v6"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/internal/packhandle"
	"github.com/go-git/go-git/v6/plumbing/hash"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

type lazyPackfilesIter struct {
	hashes []plumbing.Hash
	open   func(h plumbing.Hash) (storer.EncodedObjectIter, error)
	cur    storer.EncodedObjectIter
}

func (it *lazyPackfilesIter) Next() (plumbing.EncodedObject, error) {
	for {
		if it.cur == nil {
			if len(it.hashes) == 0 {
				return nil, io.EOF
			}
			h := it.hashes[0]
			it.hashes = it.hashes[1:]

			sub, err := it.open(h)
			if err == io.EOF {
				continue
			} else if err != nil {
				return nil, err
			}
			it.cur = sub
		}
		ob, err := it.cur.Next()
		if err == io.EOF {
			it.cur.Close()
			it.cur = nil
			continue
		} else if err != nil {
			it.cur.Close()
			it.cur = nil
			return nil, err
		}
		return ob, nil
	}
}

func (it *lazyPackfilesIter) ForEach(cb func(plumbing.EncodedObject) error) error {
	return storer.ForEachIterator(it, cb)
}

func (it *lazyPackfilesIter) Close() {
	if it.cur != nil {
		it.cur.Close()
		it.cur = nil
	}
	it.hashes = nil
}

type packfileIter struct {
	pack billy.File
	iter storer.EncodedObjectIter
	seen map[plumbing.Hash]struct{}

	// tells whether the pack file should be left open after iteration or not
	keepPack bool
}

// NewPackfileIter returns a new EncodedObjectIter for the provided packfile
// and object type. Packfile and index file will be closed after they're
// used. If keepPack is true the packfile won't be closed after the iteration
// finished.
func NewPackfileIter(
	fs billy.Filesystem,
	f billy.File,
	idxFile billy.File,
	t plumbing.ObjectType,
	keepPack bool,
	_ int64, // largeObjectThreshold - currently unused
	objectIDSize int,
) (storer.EncodedObjectIter, error) {
	var hasher hash.Hash
	if objectIDSize == crypto.SHA256.Size() {
		hasher = hash.New(crypto.SHA256)
	} else {
		hasher = hash.New(crypto.SHA1)
	}

	idx := idxfile.NewMemoryIndex(objectIDSize)
	if err := idxfile.NewDecoder(idxFile, hasher).Decode(idx); err != nil {
		return nil, err
	}

	if err := idxFile.Close(); err != nil {
		return nil, err
	}

	seen := make(map[plumbing.Hash]struct{})
	return newPackfileIter(fs, f, t, seen, idx, nil, keepPack, objectIDSize)
}

func newPackfileIter(
	fs billy.Filesystem,
	f billy.File,
	t plumbing.ObjectType,
	seen map[plumbing.Hash]struct{},
	index idxfile.Index,
	cache cache.Object,
	keepPack bool,
	objectIDSize int,
) (storer.EncodedObjectIter, error) {
	// Transitional: build a PackHandle over the pack file via
	// PathSource so FSObjects cached during iteration can re-open
	// the pack file independently of the caller's lifecycle. The
	// caller-owned `f` continues to be closed by packfileIter.Close.
	// T10 replaces this with the dotgit PackHandle cache.
	ph := transitionalIterPackHandle(fs, f.Name())

	p := packfile.NewPackfile(ph,
		packfile.WithCache(cache),
		packfile.WithIdx(index),
		packfile.WithObjectIDSize(objectIDSize),
	)

	iter, err := p.GetByType(t)
	if err != nil {
		if !keepPack {
			_ = f.Close()
		}
		return nil, err
	}

	return &packfileIter{
		pack:     f,
		iter:     iter,
		seen:     seen,
		keepPack: keepPack,
	}, nil
}

// transitionalIterPackHandle builds a PackHandle over the pack file
// at fs/packPath using PathSource. Idx and Rev Sources are stubs —
// callers must provide the index via WithIdx and must not exercise
// the .rev path. Removed in T10.
func transitionalIterPackHandle(fs billy.Filesystem, packPath string) *packhandle.PackHandle {
	errSrc := func(name string) packhandle.Source {
		err := func() error {
			return fmt.Errorf("packhandle: transitional iter handle has no %s source", name)
		}
		return packhandle.Source{
			Open: func() (billy.File, error) { return nil, err() },
			Size: func() (int64, error) { return 0, err() },
		}
	}
	return packhandle.New(packhandle.Sources{
		Pack: packhandle.PathSource(fs, packPath),
		Idx:  errSrc("idx"),
		Rev:  errSrc("rev"),
	}, plumbing.ZeroHash)
}

func (iter *packfileIter) Next() (plumbing.EncodedObject, error) {
	for {
		obj, err := iter.iter.Next()
		if err != nil {
			return nil, err
		}

		if _, ok := iter.seen[obj.Hash()]; ok {
			continue
		}

		iter.seen[obj.Hash()] = struct{}{}
		return obj, nil
	}
}

func (iter *packfileIter) ForEach(cb func(plumbing.EncodedObject) error) error {
	defer iter.Close()
	for {
		o, err := iter.Next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		if err := cb(o); err != nil {
			return err
		}
	}
}

func (iter *packfileIter) Close() {
	iter.iter.Close()
	if !iter.keepPack {
		_ = iter.pack.Close()
	}
}

type objectsIter struct {
	s *ObjectStorage
	t plumbing.ObjectType
	h []plumbing.Hash
}

func (iter *objectsIter) Next() (plumbing.EncodedObject, error) {
	if len(iter.h) == 0 {
		return nil, io.EOF
	}

	obj, err := iter.s.getFromUnpacked(iter.h[0])
	iter.h = iter.h[1:]

	if err != nil {
		return nil, err
	}

	if iter.t != plumbing.AnyObject && iter.t != obj.Type() {
		return iter.Next()
	}

	return obj, err
}

func (iter *objectsIter) ForEach(cb func(plumbing.EncodedObject) error) error {
	defer iter.Close()
	for {
		o, err := iter.Next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		if err := cb(o); err != nil {
			return err
		}
	}
}

func (iter *objectsIter) Close() {
	iter.h = []plumbing.Hash{}
}
