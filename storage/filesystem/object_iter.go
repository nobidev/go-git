package filesystem

import (
	"crypto"
	"io"

	billy "github.com/go-git/go-billy/v6"

	"github.com/go-git/go-git/v6/internal/packhandle"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
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
	iter storer.EncodedObjectIter
	seen map[plumbing.Hash]struct{}
}

// NewPackfileIter returns a new EncodedObjectIter for the provided
// .pack/.idx pair, with FD lifecycle managed by an ad-hoc PackHandle.
// idxFile is consumed (Decoded into a MemoryIndex and Closed) before
// iteration begins; the pack file is wrapped behind a PathSource so
// FDs auto-release after the iterator's PackReader is released.
func NewPackfileIter(
	fs billy.Filesystem,
	f billy.File,
	idxFile billy.File,
	t plumbing.ObjectType,
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

	packPath := f.Name()
	// f's role is to anchor the path; PackHandle reopens via
	// PathSource. Close the caller's handle now so the PackHandle
	// fully owns FD lifecycle.
	_ = f.Close()

	// Pack-only PackHandle: Sources.Idx and Sources.Rev are zero
	// values, so PackHandle.Index() short-circuits to
	// ErrSourceUnconfigured. The MemoryIndex above is fed in via
	// packfile.WithIdx, so the LazyIndex path is never reached.
	ph := packhandle.New(packhandle.Sources{
		Pack: packhandle.PathSource(fs, packPath),
	}, plumbing.ZeroHash)

	seen := make(map[plumbing.Hash]struct{})
	return newPackfileIter(ph, t, seen, idx, nil, objectIDSize)
}

func newPackfileIter(
	handle *packhandle.PackHandle,
	t plumbing.ObjectType,
	seen map[plumbing.Hash]struct{},
	index idxfile.Index,
	cache cache.Object,
	objectIDSize int,
) (storer.EncodedObjectIter, error) {
	p := packfile.NewPackfile(handle,
		packfile.WithCache(cache),
		packfile.WithIdx(index),
		packfile.WithObjectIDSize(objectIDSize),
	)

	iter, err := p.GetByType(t)
	if err != nil {
		return nil, err
	}

	return &packfileIter{iter: iter, seen: seen}, nil
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
