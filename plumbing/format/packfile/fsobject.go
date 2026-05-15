package packfile

import (
	"bufio"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/internal/packhandle"
	"github.com/go-git/go-git/v6/utils/ioutil"
	"github.com/go-git/go-git/v6/utils/sync"
)

// FSObject is an object from the packfile, backed by a PackHandle.
type FSObject struct {
	hash   plumbing.Hash
	offset int64
	size   int64
	typ    plumbing.ObjectType
	index  idxfile.Index
	handle *packhandle.PackHandle
	cache  cache.Object
}

// NewFSObject creates a new filesystem object backed by a PackHandle.
// The handle owns the FD lifecycle: Reader() acquires a fresh
// PackReader per call and releases it on the returned ReadCloser's
// Close.
func NewFSObject(
	hash plumbing.Hash,
	finalType plumbing.ObjectType,
	offset int64,
	contentSize int64,
	index idxfile.Index,
	handle *packhandle.PackHandle,
	cache cache.Object,
) *FSObject {
	return &FSObject{
		hash:   hash,
		offset: offset,
		size:   contentSize,
		typ:    finalType,
		index:  index,
		handle: handle,
		cache:  cache,
	}
}

// Reader implements the plumbing.EncodedObject interface.
func (o *FSObject) Reader() (io.ReadCloser, error) {
	if obj, ok := o.cache.Get(o.hash); ok && obj != o {
		return obj.Reader()
	}

	pr, err := o.handle.OpenPackReader()
	if err != nil {
		return nil, err
	}

	if _, err := pr.Seek(o.offset, io.SeekStart); err != nil {
		_ = pr.Close()
		return nil, err
	}

	br := sync.GetBufioReader(pr)
	zr, err := sync.GetZlibReader(br)
	if err != nil {
		sync.PutBufioReader(br)
		_ = pr.Close()
		return nil, err
	}

	return NewBoundedReadCloser(&zlibReadCloser{
		r:    zr,
		f:    pr, // packhandle.PackReader is io.Closer; close releases the refcount
		rbuf: br,
	}, o.size), nil
}

type zlibReadCloser struct {
	r      *sync.ZLibReader
	f      io.Closer
	rbuf   *bufio.Reader
	closed bool
}

// Read reads up to len(p) bytes into p from the data.
func (r *zlibReadCloser) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *zlibReadCloser) Close() (err error) {
	if r.closed {
		return nil
	}
	r.closed = true

	if r.f != nil {
		defer ioutil.CheckClose(r.f, &err)
	}

	defer sync.PutBufioReader(r.rbuf)

	defer sync.PutZlibReader(r.r)
	return r.r.Close()
}

// SetSize implements the plumbing.EncodedObject interface. This method
// is a noop.
func (o *FSObject) SetSize(int64) {}

// SetType implements the plumbing.EncodedObject interface. This method is
// a noop.
func (o *FSObject) SetType(plumbing.ObjectType) {}

// Hash implements the plumbing.EncodedObject interface.
func (o *FSObject) Hash() plumbing.Hash { return o.hash }

// Size implements the plumbing.EncodedObject interface.
func (o *FSObject) Size() int64 { return o.size }

// Type implements the plumbing.EncodedObject interface.
func (o *FSObject) Type() plumbing.ObjectType {
	return o.typ
}

// Writer implements the plumbing.EncodedObject interface. This method always
// returns a nil writer.
func (o *FSObject) Writer() (io.WriteCloser, error) {
	return nil, nil
}
