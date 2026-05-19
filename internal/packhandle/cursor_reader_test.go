package packhandle

import (
	"bytes"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trackingReader is an io.ReaderAt backed by a bytes.Reader that
// tracks Close + counts ReadAt calls.
type trackingReader struct {
	*bytes.Reader
	readAtCalls atomic.Int32
	closed      atomic.Bool
}

func (t *trackingReader) ReadAt(p []byte, off int64) (int, error) {
	t.readAtCalls.Add(1)
	return t.Reader.ReadAt(p, off)
}

func (t *trackingReader) Close() error { t.closed.Store(true); return nil }

func newCursorReaderForTest(data []byte) (*cursorReader, *trackingReader, *atomic.Int32) {
	tr := &trackingReader{Reader: bytes.NewReader(data)}
	var releases atomic.Int32
	sizeFn := func() (int64, error) { return int64(len(data)), nil }
	cr := newCursorReader(tr, func() { releases.Add(1) }, sizeFn)
	return cr, tr, &releases
}

func TestCursorReader_SequentialRead(t *testing.T) {
	t.Parallel()
	cr, _, releases := newCursorReaderForTest([]byte("hello world"))
	t.Cleanup(func() { _ = cr.Close() })

	buf := make([]byte, 5)
	n, err := cr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, "hello", string(buf))

	n, err = cr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, " worl", string(buf))

	// Read at the tail: bytes.Reader.ReadAt returns (1, io.EOF) when
	// fewer bytes than requested are available at the offset. We
	// propagate that shape directly (no EOF suppression).
	n, err = cr.Read(buf)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 1, n)
	assert.Equal(t, "d", string(buf[:n]))

	require.NoError(t, cr.Close())
	assert.Equal(t, int32(1), releases.Load())
}

func TestCursorReader_Seek(t *testing.T) {
	t.Parallel()
	cr, _, _ := newCursorReaderForTest([]byte("0123456789"))
	t.Cleanup(func() { _ = cr.Close() })

	pos, err := cr.Seek(3, io.SeekStart)
	require.NoError(t, err)
	assert.Equal(t, int64(3), pos)

	buf := make([]byte, 2)
	_, err = cr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "34", string(buf))

	pos, err = cr.Seek(2, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(7), pos)

	pos, err = cr.Seek(-3, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(7), pos)

	_, err = cr.Seek(-1, io.SeekStart)
	assert.Error(t, err)
}

func TestCursorReader_ReadAt(t *testing.T) {
	t.Parallel()
	cr, _, _ := newCursorReaderForTest([]byte("0123456789"))
	t.Cleanup(func() { _ = cr.Close() })

	// Pre-advance cursor; ReadAt should NOT use it.
	_, err := cr.Seek(5, io.SeekStart)
	require.NoError(t, err)

	buf := make([]byte, 3)
	n, err := cr.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, "012", string(buf))

	pos, err := cr.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(5), pos)
}

func TestCursorReader_ConcurrentReadAt(t *testing.T) {
	t.Parallel()
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i)
	}
	cr, _, _ := newCursorReaderForTest(data)
	t.Cleanup(func() { _ = cr.Close() })

	const workers, iters = 8, 200
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			buf := make([]byte, 64)
			for i := range iters {
				off := int64((i * 271) % (len(data) - len(buf)))
				n, err := cr.ReadAt(buf, off)
				assert.NoError(t, err)
				assert.Equal(t, len(buf), n)
				for j, b := range buf {
					assert.Equal(t, byte(int(off)+j), b, "off=%d j=%d", off, j)
				}
			}
		})
	}
	wg.Wait()
}

func TestCursorReader_ConcurrentReadNoRace(t *testing.T) {
	t.Parallel()
	// Concurrent Read on a single cursorReader is documented as
	// semantically meaningless (interleaved bytes); the test only
	// asserts absence of data races under -race.
	cr, _, _ := newCursorReaderForTest(make([]byte, 64*1024))
	t.Cleanup(func() { _ = cr.Close() })

	const workers = 8
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			buf := make([]byte, 64)
			for range 200 {
				_, _ = cr.Read(buf)
			}
		})
	}
	wg.Wait()
}

func TestCursorReader_MixedReadAndReadAt(t *testing.T) {
	t.Parallel()
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i)
	}
	cr, _, _ := newCursorReaderForTest(data)
	t.Cleanup(func() { _ = cr.Close() })

	var wg sync.WaitGroup
	wg.Go(func() {
		buf := make([]byte, 32)
		for range 100 {
			n, _ := cr.Read(buf)
			if n == 0 {
				break
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			buf := make([]byte, 32)
			for i := range 200 {
				off := int64((i * 31) % (len(data) - len(buf)))
				n, err := cr.ReadAt(buf, off)
				assert.NoError(t, err)
				assert.Equal(t, len(buf), n)
				for j, b := range buf {
					assert.Equal(t, byte(int(off)+j), b)
				}
			}
		})
	}
	wg.Wait()
}

func TestCursorReader_CloseIdempotent(t *testing.T) {
	t.Parallel()
	cr, _, releases := newCursorReaderForTest([]byte("hello"))

	assert.NoError(t, cr.Close())
	assert.ErrorIs(t, cr.Close(), os.ErrClosed)
	assert.Equal(t, int32(1), releases.Load(), "release should be called exactly once")
}

func TestCursorReader_ReadAfterClose(t *testing.T) {
	t.Parallel()
	cr, _, _ := newCursorReaderForTest([]byte("hello"))
	require.NoError(t, cr.Close())

	buf := make([]byte, 5)
	_, err := cr.Read(buf)
	assert.ErrorIs(t, err, os.ErrClosed)

	_, err = cr.ReadAt(buf, 0)
	assert.ErrorIs(t, err, os.ErrClosed)
}
