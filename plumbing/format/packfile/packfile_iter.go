package packfile

import (
	"bufio"
	"errors"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/internal/packhandle"
	gogitsync "github.com/go-git/go-git/v6/utils/sync"
)

// objectIter iterates objects in a packfile in offset order. It owns
// the PackReader + Scanner acquired in Packfile.GetByType, releasing
// both on Close.
type objectIter struct {
	p       *Packfile
	iter    idxfile.EntryIter
	typ     plumbing.ObjectType
	scanner *Scanner
	reader  packhandle.PackReader
	rbuf    *bufio.Reader
}

func (iter *objectIter) Next() (plumbing.EncodedObject, error) {
	for {
		entry, err := iter.iter.Next()
		if err != nil {
			return nil, err
		}

		iter.p.m.Lock()
		obj, err := iter.p.objectAtScannerWith(iter.scanner, int64(entry.Offset))
		iter.p.m.Unlock()
		if err != nil {
			return nil, err
		}

		if iter.typ == plumbing.AnyObject || obj.Type() == iter.typ {
			return obj, nil
		}
		// Object doesn't match type filter; continue.
	}
}

func (iter *objectIter) ForEach(cb func(plumbing.EncodedObject) error) error {
	defer iter.Close()
	for {
		obj, err := iter.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := cb(obj); err != nil {
			return err
		}
	}
}

func (iter *objectIter) Close() {
	iter.iter.Close()
	iter.scanner = nil
	if iter.reader != nil {
		_ = iter.reader.Close()
		iter.reader = nil
	}
	if iter.rbuf != nil {
		gogitsync.PutBufioReader(iter.rbuf)
		iter.rbuf = nil
	}
}
