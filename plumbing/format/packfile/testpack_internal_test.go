package packfile

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	gogitbinary "github.com/go-git/go-git/v6/utils/binary"
)

type testPackObject struct {
	typ                 plumbing.ObjectType
	declaredSize        int64
	content             []byte
	reference           plumbing.Hash
	offsetDeltaDistance int64
}

func buildTestPack(t *testing.T, objects ...testPackObject) ([]byte, []int64) {
	t.Helper()

	var body bytes.Buffer
	body.WriteString("PACK")
	require.NoError(t, binary.Write(&body, binary.BigEndian, uint32(2)))
	require.NoError(t, binary.Write(&body, binary.BigEndian, uint32(len(objects))))

	offsets := make([]int64, 0, len(objects))
	for _, obj := range objects {
		offsets = append(offsets, int64(body.Len()))
		declaredSize := obj.declaredSize
		if declaredSize == 0 && len(obj.content) > 0 {
			declaredSize = int64(len(obj.content))
		}

		writeTestObjectHeader(&body, obj.typ, declaredSize)
		switch obj.typ {
		case plumbing.REFDeltaObject:
			body.Write(obj.reference.Bytes())
		case plumbing.OFSDeltaObject:
			require.NoError(t, gogitbinary.WriteVariableWidthInt(&body, obj.offsetDeltaDistance))
		}
		body.Write(zlibCompress(t, obj.content))
	}

	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes(), offsets
}

func writeTestObjectHeader(w io.ByteWriter, typ plumbing.ObjectType, size int64) {
	remaining := uint64(size)
	first := byte(typ)<<4 | byte(remaining&0x0f)
	remaining >>= 4
	if remaining > 0 {
		first |= 0x80
	}
	_ = w.WriteByte(first)

	for remaining > 0 {
		next := byte(remaining & 0x7f)
		remaining >>= 7
		if remaining > 0 {
			next |= 0x80
		}
		_ = w.WriteByte(next)
	}
}

func zlibCompress(t *testing.T, content []byte) []byte {
	t.Helper()

	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, err := zw.Write(content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return compressed.Bytes()
}

func testObjectHash(typ plumbing.ObjectType, content []byte) plumbing.Hash {
	h := plumbing.NewHasher(format.SHA1, typ, int64(len(content)))
	_, _ = h.Write(content)
	return h.Sum()
}

func writeTestPackFile(t *testing.T, pack []byte) *os.File {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "malformed-*.pack")
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	_, err = file.Write(pack)
	require.NoError(t, err)
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(t, err)
	return file
}
