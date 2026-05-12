package packfile

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestParserRejectsDeepDeltaChain(t *testing.T) {
	t.Parallel()

	objects := make([]testPackObject, 0, maxDeltaChainDepth+2)
	content := []byte{0, 0}
	parentHash := testObjectHash(plumbing.BlobObject, content)
	objects = append(objects, testPackObject{
		typ:     plumbing.BlobObject,
		content: content,
	})

	for i := range maxDeltaChainDepth + 1 {
		content = []byte{byte(i + 1), byte((i + 1) >> 8)}
		delta := buildDelta(2, 2, insertOp(content))
		objects = append(objects, testPackObject{
			typ:       plumbing.REFDeltaObject,
			content:   delta,
			reference: parentHash,
		})
		parentHash = testObjectHash(plumbing.BlobObject, content)
	}

	pack, _ := buildTestPack(t, objects...)
	parser := NewParser(bytes.NewReader(pack))

	_, err := parser.Parse()
	require.ErrorIs(t, err, ErrMalformedPackfile)
	require.ErrorContains(t, err, "delta chain depth")
}

func TestParserRejectsInflatedObjectLargerThanDeclared(t *testing.T) {
	t.Parallel()

	pack, _ := buildTestPack(t, testPackObject{
		typ:          plumbing.BlobObject,
		declaredSize: 1,
		content:      bytes.Repeat([]byte("a"), 1024),
	})
	parser := NewParser(bytes.NewReader(pack))

	_, err := parser.Parse()
	require.ErrorIs(t, err, ErrMalformedPackfile)
	require.ErrorContains(t, err, "inflated object exceeds declared size")
}
