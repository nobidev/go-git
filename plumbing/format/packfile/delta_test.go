package packfile

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/go-git/go-git/v6/plumbing"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
)

type DeltaSuite struct {
	suite.Suite
	testCases []deltaTest
}

func TestDeltaSuite(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(DeltaSuite))
}

type deltaTest struct {
	description string
	base        []piece
	target      []piece
}

func (s *DeltaSuite) SetupSuite() {
	s.testCases = []deltaTest{{
		description: "distinct file",
		base:        []piece{{"0", 300}},
		target:      []piece{{"2", 200}},
	}, {
		description: "same file",
		base:        []piece{{"1", 3000}},
		target:      []piece{{"1", 3000}},
	}, {
		description: "small file",
		base:        []piece{{"1", 3}},
		target:      []piece{{"1", 3}, {"0", 1}},
	}, {
		description: "big file",
		base:        []piece{{"1", 300000}},
		target:      []piece{{"1", 30000}, {"0", 1000000}},
	}, {
		description: "add elements before",
		base:        []piece{{"0", 200}},
		target:      []piece{{"1", 300}, {"0", 200}},
	}, {
		description: "add 10 times more elements at the end",
		base:        []piece{{"1", 300}, {"0", 200}},
		target:      []piece{{"0", 2000}},
	}, {
		description: "add elements between",
		base:        []piece{{"0", 400}},
		target:      []piece{{"0", 200}, {"1", 200}, {"0", 200}},
	}, {
		description: "add elements after",
		base:        []piece{{"0", 200}},
		target:      []piece{{"0", 200}, {"1", 200}},
	}, {
		description: "modify elements at the end",
		base:        []piece{{"1", 300}, {"0", 200}},
		target:      []piece{{"0", 100}},
	}, {
		description: "complex modification",
		base: []piece{
			{"0", 3},
			{"1", 40},
			{"2", 30},
			{"3", 2},
			{"4", 400},
			{"5", 23},
		},
		target: []piece{
			{"1", 30},
			{"2", 20},
			{"7", 40},
			{"4", 400},
			{"5", 10},
		},
	}, {
		description: "A copy operation bigger than 64kb",
		base:        []piece{{bigRandStr, 1}, {"1", 200}},
		target:      []piece{{bigRandStr, 1}},
	}}
}

var bigRandStr = randStringBytes(100 * 1024)

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func randBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return b
}

func randStringBytes(n int) string {
	return string(randBytes(n))
}

func (s *DeltaSuite) TestAddDelta() {
	for _, t := range s.testCases {
		baseBuf := genBytes(t.base)
		targetBuf := genBytes(t.target)
		delta := DiffDelta(baseBuf, targetBuf)
		result, err := PatchDelta(baseBuf, delta)

		s.T().Log("Executing test case:", t.description)
		s.NoError(err)
		s.Equal(targetBuf, result)
	}
}

func (s *DeltaSuite) TestAddDeltaReader() {
	for _, t := range s.testCases {
		baseBuf := genBytes(t.base)
		baseObj := &plumbing.MemoryObject{}
		baseObj.Write(baseBuf)

		targetBuf := genBytes(t.target)

		delta := DiffDelta(baseBuf, targetBuf)
		deltaRC := io.NopCloser(bytes.NewReader(delta))

		s.T().Log("Executing test case:", t.description)

		resultRC, err := ReaderFromDelta(baseObj, deltaRC)
		s.NoError(err)

		result, err := io.ReadAll(resultRC)
		s.NoError(err)

		err = resultRC.Close()
		s.NoError(err)

		s.Equal(targetBuf, result)
	}
}

func (s *DeltaSuite) TestIncompleteDelta() {
	for _, t := range s.testCases {
		s.T().Log("Incomplete delta on:", t.description)
		baseBuf := genBytes(t.base)
		targetBuf := genBytes(t.target)
		delta := DiffDelta(baseBuf, targetBuf)
		delta = delta[:len(delta)-2]
		result, err := PatchDelta(baseBuf, delta)
		s.NotNil(err)
		s.Nil(result)
	}

	// check nil input too
	result, err := PatchDelta(nil, nil)
	s.NotNil(err)
	s.Nil(result)
}

func (s *DeltaSuite) TestMaxCopySizeDelta() {
	baseBuf := randBytes(maxCopySize)
	targetBuf := baseBuf[0:]
	targetBuf = append(targetBuf, byte(1))

	delta := DiffDelta(baseBuf, targetBuf)
	result, err := PatchDelta(baseBuf, delta)
	s.NoError(err)
	s.Equal(targetBuf, result)
}

func (s *DeltaSuite) TestMaxCopySizeDeltaReader() {
	baseBuf := randBytes(maxCopySize)
	baseObj := &plumbing.MemoryObject{}
	baseObj.Write(baseBuf)

	targetBuf := baseBuf[0:]
	targetBuf = append(targetBuf, byte(1))

	delta := DiffDelta(baseBuf, targetBuf)
	deltaRC := io.NopCloser(bytes.NewReader(delta))

	resultRC, err := ReaderFromDelta(baseObj, deltaRC)
	s.NoError(err)

	result, err := io.ReadAll(resultRC)
	s.NoError(err)

	err = resultRC.Close()
	s.NoError(err)
	s.Equal(targetBuf, result)
}

func (s *DeltaSuite) TestPatchDeltaWriterOversizedTargetHeader() {
	// patchDeltaWriter must bound the preemptive Buffer.Grow at
	// maxPatchPreemptionSize regardless of the targetSz advertised in
	// the delta header, matching the behaviour of patchDelta. The
	// header here encodes targetSz = math.MaxInt64; with the cap in
	// place the function reaches the command loop, runs out of input,
	// and returns an error.
	var hdr bytes.Buffer
	hdr.WriteByte(0x00) // srcSz = 0

	n := uint64(math.MaxInt64)
	for n >= 0x80 {
		hdr.WriteByte(byte(n) | 0x80)
		n >>= 7
	}
	hdr.WriteByte(byte(n))

	var dst bytes.Buffer
	_, _, err := patchDeltaWriter(
		&dst, bytes.NewReader(nil), &hdr,
		plumbing.BlobObject, nil, format.SHA1,
	)
	s.Error(err)
}

func FuzzPatchDelta(f *testing.F) {
	f.Add([]byte("some value"), []byte("\n\f\fsomenewvalue"))
	f.Add([]byte("some value"), []byte("\n\x0e\x0evalue"))
	f.Add([]byte("some value"), []byte("\n\x0e\x0eva"))
	f.Add([]byte("some value"), []byte("\n\x80\x80\x80\x80\x80\x802\x7fvalue"))
	// Two copy-from-delta ops whose declared sizes each fit within
	// targetSz but whose sum exceeds it. Header: srcSz=10, targetSz=10.
	f.Add([]byte("AAAAAAAAAA"), []byte("\n\n\aBBBBBBB\aCCCCCCC"))
	// Two copy-from-src ops with the same shape: cmd=0x90 takes one
	// size byte and reads from offset 0 of src.
	f.Add([]byte("AAAAAAAAAA"), []byte("\n\n\x90\a\x90\a"))

	f.Fuzz(func(_ *testing.T, input1, input2 []byte) {
		PatchDelta(input1, input2)
	})
}

func FuzzDeltaRoundTrip(f *testing.F) {
	f.Add([]byte("some value"), []byte("somenewvalue"))
	f.Add([]byte("aaaaaaaaaaaaaaaa"), []byte("aaaaaaaabaaaaaaa"))
	f.Add([]byte("base"), []byte{})

	f.Fuzz(func(t *testing.T, baseData, targetData []byte) {
		if len(baseData) == 0 || len(baseData) > 4096 || len(targetData) > 4096 {
			return
		}

		base := &plumbing.MemoryObject{}
		base.SetType(plumbing.BlobObject)
		w, err := base.Writer()
		if err != nil {
			t.Fatalf("base writer: %v", err)
		}
		if _, err := w.Write(baseData); err != nil {
			t.Fatalf("write base: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close base: %v", err)
		}

		target := &plumbing.MemoryObject{}
		target.SetType(plumbing.BlobObject)
		w, err = target.Writer()
		if err != nil {
			t.Fatalf("target writer: %v", err)
		}
		if _, err := w.Write(targetData); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close target: %v", err)
		}

		delta, err := GetDelta(base, target)
		if err != nil {
			t.Fatalf("get delta: %v", err)
		}
		r, err := delta.Reader()
		if err != nil {
			t.Fatalf("delta reader: %v", err)
		}
		deltaData, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatalf("read delta: %v", err)
		}

		got, err := PatchDelta(baseData, deltaData)
		if err != nil {
			t.Fatalf("patch delta: %v", err)
		}
		if !bytes.Equal(got, targetData) {
			t.Fatalf("patched delta content mismatch")
		}

		rc, err := ReaderFromDelta(base, bytes.NewReader(deltaData))
		if err != nil {
			t.Fatalf("reader from delta: %v", err)
		}
		streamed, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read streamed delta: %v", err)
		}
		if !bytes.Equal(streamed, targetData) {
			t.Fatalf("streamed delta content mismatch")
		}
	})
}
