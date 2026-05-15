package packfile

import (
	"crypto"
	"fmt"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/internal/packhandle"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/utils/ioutil"
	gogitsync "github.com/go-git/go-git/v6/utils/sync"
)

var (
	// ErrInvalidObject is returned by Decode when an invalid object is
	// found in the packfile.
	ErrInvalidObject = NewError("invalid git object")
	// ErrZLib is returned by Decode when there was an error unzipping
	// the packfile contents.
	ErrZLib = NewError("zlib reading error")
)

// Packfile allows retrieving information from inside a packfile.
//
// Packfile is metadata-only: it holds no file descriptors. Each
// operation acquires a PackReader from the underlying PackHandle,
// builds a Scanner over it, executes, and releases the reader on
// return.
type Packfile struct {
	idxfile.Index
	handle *packhandle.PackHandle

	cache cache.Object

	id           plumbing.Hash
	m            sync.Mutex
	objectIDSize int

	once    sync.Once
	onceErr error
}

// NewPackfile returns a Packfile representation backed by the given
// PackHandle. The Packfile does not own the PackHandle; closing the
// Packfile does NOT close the PackHandle.
//
// The Packfile is metadata-only: it holds no file descriptors.
// Per-operation reader acquisition happens inside Get/GetByOffset/
// GetByType.
func NewPackfile(
	handle *packhandle.PackHandle,
	opts ...PackfileOption,
) *Packfile {
	p := &Packfile{
		handle:       handle,
		objectIDSize: crypto.SHA1.Size(),
	}
	for _, opt := range opts {
		opt(p)
	}

	return p
}

// Get retrieves the encoded object in the packfile with the given hash.
func (p *Packfile) Get(h plumbing.Hash) (plumbing.EncodedObject, error) {
	if err := p.init(); err != nil {
		return nil, err
	}
	p.m.Lock()
	defer p.m.Unlock()

	// Cache-hit fast path: skip Scanner construction entirely.
	if obj, ok := p.cache.Get(h); ok {
		return obj, nil
	}

	var result plumbing.EncodedObject
	err := p.withScanner(func(s *Scanner) error {
		obj, err := p.getWith(s, h)
		if err != nil {
			return err
		}
		result = obj
		return nil
	})
	return result, err
}

// GetByOffset retrieves the encoded object from the packfile at the given
// offset.
func (p *Packfile) GetByOffset(offset int64) (plumbing.EncodedObject, error) {
	if err := p.init(); err != nil {
		return nil, err
	}
	p.m.Lock()
	defer p.m.Unlock()

	// Cache-hit fast path: skip Scanner construction entirely.
	if h, err := p.FindHash(offset); err == nil {
		if obj, ok := p.cache.Get(h); ok {
			return obj, nil
		}
	}

	var result plumbing.EncodedObject
	err := p.withScanner(func(s *Scanner) error {
		obj, err := p.getByOffsetWith(s, offset)
		if err != nil {
			return err
		}
		result = obj
		return nil
	})
	return result, err
}

// GetSizeByOffset retrieves the size of the encoded object from the
// packfile with the given offset.
func (p *Packfile) GetSizeByOffset(offset int64) (int64, error) {
	if err := p.init(); err != nil {
		return 0, err
	}

	d, err := p.GetByOffset(offset)
	if err != nil {
		return 0, err
	}

	return d.Size(), nil
}

// GetAll returns an iterator with all encoded objects in the packfile.
// The iterator returned is not thread-safe, it should be used in the same
// thread as the Packfile instance.
func (p *Packfile) GetAll() (storer.EncodedObjectIter, error) {
	return p.GetByType(plumbing.AnyObject)
}

// GetByType returns all the objects of the given type.
func (p *Packfile) GetByType(typ plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	if err := p.init(); err != nil {
		return nil, err
	}

	switch typ {
	case plumbing.AnyObject,
		plumbing.BlobObject,
		plumbing.TreeObject,
		plumbing.CommitObject,
		plumbing.TagObject:
		// withScanner won't work because the iterator outlives this
		// function. Acquire reader + scanner inline; iterator's Close
		// releases them.
		pr, err := p.handle.OpenPackReader()
		if err != nil {
			return nil, err
		}
		rbuf := gogitsync.GetBufioReader(nil)
		sopts := []ScannerOption{WithBufioReader(rbuf)}
		if p.objectIDSize == format.SHA256Size {
			sopts = append(sopts, WithSHA256())
		}
		scanner := NewScanner(pr, sopts...)
		if !scanner.Scan() {
			gogitsync.PutBufioReader(rbuf)
			_ = pr.Close()
			return nil, scanner.Error()
		}

		entries, err := p.EntriesByOffset()
		if err != nil {
			gogitsync.PutBufioReader(rbuf)
			_ = pr.Close()
			return nil, err
		}

		return &objectIter{
			p:       p,
			iter:    entries,
			typ:     typ,
			scanner: scanner,
			reader:  pr,
			rbuf:    rbuf,
		}, nil
	default:
		return nil, plumbing.ErrInvalidType
	}
}

// DeltaObject returns the delta-flavored EncodedObject for the given
// hash + offset. If the header at offset is not a delta type, the
// fully-decoded object at that offset is returned via getByOffsetWith.
// This subsumes the logic previously exposed via Scanner() to
// ObjectStorage.decodeDeltaObjectAt.
func (p *Packfile) DeltaObject(hash plumbing.Hash, offset int64) (plumbing.EncodedObject, error) {
	if err := p.init(); err != nil {
		return nil, err
	}
	p.m.Lock()
	defer p.m.Unlock()

	var result plumbing.EncodedObject
	err := p.withScanner(func(s *Scanner) error {
		if err := s.SeekFromStart(offset); err != nil {
			return err
		}
		if !s.Scan() {
			return fmt.Errorf("failed to decode delta object")
		}
		header := s.Data().Value().(ObjectHeader)

		var base plumbing.Hash
		switch header.Type {
		case plumbing.REFDeltaObject:
			base = header.Reference
		case plumbing.OFSDeltaObject:
			var err error
			base, err = p.FindHash(header.OffsetReference)
			if err != nil {
				return err
			}
		default:
			obj, err := p.getByOffsetWith(s, offset)
			if err != nil {
				return err
			}
			result = obj
			return nil
		}

		obj := &plumbing.MemoryObject{}
		obj.SetType(header.Type)
		w, err := obj.Writer()
		if err != nil {
			return err
		}
		if err := s.WriteObject(&header, w); err != nil {
			return err
		}
		result = newDeltaObject(obj, hash, base, header.Size)
		return nil
	})
	return result, err
}

// ID returns the ID of the packfile, which is the checksum at the end of it.
func (p *Packfile) ID() (plumbing.Hash, error) {
	if err := p.init(); err != nil {
		return plumbing.ZeroHash, err
	}

	return p.id, nil
}

// withScanner acquires a fresh PackReader from the handle, builds a
// Scanner over it, calls fn, and releases the reader on return. The
// caller owns Packfile's external mutex (p.m).
func (p *Packfile) withScanner(fn func(*Scanner) error) error {
	pr, err := p.handle.OpenPackReader()
	if err != nil {
		return err
	}
	defer pr.Close()

	rbuf := gogitsync.GetBufioReader(nil)
	defer gogitsync.PutBufioReader(rbuf)

	sopts := []ScannerOption{WithBufioReader(rbuf)}
	if p.objectIDSize == format.SHA256Size {
		sopts = append(sopts, WithSHA256())
	}

	scanner := NewScanner(pr, sopts...)
	if !scanner.Scan() {
		return scanner.Error()
	}
	return fn(scanner)
}

// getWith decodes the object identified by h using the supplied
// Scanner. Caller holds p.m.
func (p *Packfile) getWith(s *Scanner, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if obj, ok := p.cache.Get(h); ok {
		return obj, nil
	}

	offset, err := p.FindOffset(h)
	if err != nil {
		return nil, err
	}

	oh, err := p.headerFromOffsetWith(s, offset)
	if err != nil {
		return nil, err
	}

	return p.objectFromHeaderWith(s, oh)
}

// getByOffsetWith decodes the object at the given offset using the
// supplied Scanner. Caller holds p.m.
func (p *Packfile) getByOffsetWith(s *Scanner, offset int64) (plumbing.EncodedObject, error) {
	h, err := p.FindHash(offset)
	if err != nil {
		return nil, err
	}

	if obj, ok := p.cache.Get(h); ok {
		return obj, nil
	}

	oh, err := p.headerFromOffsetWith(s, offset)
	if err != nil {
		return nil, err
	}

	return p.objectFromHeaderWith(s, oh)
}

func (p *Packfile) init() error {
	p.once.Do(func() {
		if p.handle == nil {
			p.onceErr = fmt.Errorf("pack handle is not set")
			return
		}

		if p.Index == nil {
			p.onceErr = fmt.Errorf("index is not set")
			return
		}

		meta, err := p.handle.Meta()
		if err != nil {
			p.onceErr = err
			return
		}
		p.id = meta.ID

		if p.cache == nil {
			p.cache = cache.NewObjectLRUDefault()
		}
	})

	return p.onceErr
}

func (p *Packfile) headerFromOffsetWith(s *Scanner, offset int64) (*ObjectHeader, error) {
	err := s.SeekFromStart(offset)
	if err != nil {
		return nil, err
	}

	if !s.Scan() {
		return nil, plumbing.ErrObjectNotFound
	}

	oh := s.Data().Value().(ObjectHeader)
	return &oh, nil
}

// Close is a no-op preserved for API compatibility. The Packfile
// holds no FDs; PackHandle owns FD lifecycle.
func (p *Packfile) Close() error {
	return nil
}

// objectAtScannerWith decodes one object at the given offset using
// the iterator-held scanner. Caller holds p.m.
func (p *Packfile) objectAtScannerWith(s *Scanner, offset int64) (plumbing.EncodedObject, error) {
	oh, err := p.headerFromOffsetWith(s, offset)
	if err != nil {
		return nil, err
	}
	return p.objectFromHeaderWith(s, oh)
}

func (p *Packfile) objectFromHeaderWith(s *Scanner, oh *ObjectHeader) (plumbing.EncodedObject, error) {
	if oh == nil {
		return nil, plumbing.ErrObjectNotFound
	}

	// FSObject path: non-delta object, PackHandle available.
	if !oh.Type.IsDelta() && p.handle != nil {
		fs := NewFSObject(
			oh.ID(),
			oh.Type,
			oh.ContentOffset,
			oh.Size,
			p.Index,
			p.handle,
			p.cache,
		)

		p.cache.Put(fs)
		return fs, nil
	}

	return p.getMemoryObjectWith(s, oh)
}

func (p *Packfile) getMemoryObjectWith(s *Scanner, oh *ObjectHeader) (plumbing.EncodedObject, error) {
	of := format.SHA1
	if p.objectIDSize == format.SHA256.Size() {
		of = format.SHA256
	}
	h := plumbing.FromObjectFormat(of)
	obj := plumbing.NewMemoryObject(h)

	obj.SetSize(oh.Size)
	obj.SetType(oh.Type)

	w, err := obj.Writer()
	if err != nil {
		return nil, err
	}
	defer ioutil.CheckClose(w, &err)

	switch oh.Type {
	case plumbing.CommitObject, plumbing.TreeObject, plumbing.BlobObject, plumbing.TagObject:
		err = s.inflateContent(oh.ContentOffset, w, oh.Size)

	case plumbing.REFDeltaObject, plumbing.OFSDeltaObject:
		var parent plumbing.EncodedObject

		switch oh.Type {
		case plumbing.REFDeltaObject:
			var ok bool
			parent, ok = p.cache.Get(oh.Reference)
			if !ok {
				parent, err = p.getWith(s, oh.Reference)
			}
		case plumbing.OFSDeltaObject:
			parent, err = p.getByOffsetWith(s, oh.OffsetReference)
		}

		if err != nil {
			return nil, fmt.Errorf("cannot find base object: %w", err)
		}

		// The scanner pre-populates oh.content for delta objects when
		// running outside low-memory mode; only inflate when we don't
		// already hold the bytes, otherwise this would append a
		// duplicate copy of the delta payload.
		if oh.content == nil {
			oh.content = gogitsync.GetBytesBuffer()
			err = s.inflateContent(oh.ContentOffset, oh.content, oh.Size)
			if err != nil {
				return nil, fmt.Errorf("cannot inflate content: %w", err)
			}
		}

		obj.SetType(parent.Type())
		err = ApplyDelta(obj, parent, oh.content)

	default:
		err = ErrInvalidObject.AddDetails("type %q", oh.Type)
	}

	if err != nil {
		return nil, err
	}

	p.cache.Put(obj)

	return obj, nil
}
