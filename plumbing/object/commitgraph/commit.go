package commitgraph

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/utils/ioutil"
	gogitsync "github.com/go-git/go-git/v6/utils/sync"
)

const (
	headerTree      = "tree"
	headerParent    = "parent"
	headerAuthor    = "author"
	headerCommitter = "committer"
)

// traversalCommit holds the base traversal fields parsed from an encoded commit object.
// It is a lightweight stand-in for object.Commit, carrying only what commit
// graph walking needs: tree, parents, and the committer/author timestamps.
// It is intended to be treated as a read-only value after decode.
type traversalCommit struct {
	id           plumbing.Hash
	treeHash     plumbing.Hash
	parentHashes []plumbing.Hash
	when         time.Time
	authorWhen   time.Time
}

// ID returns the hash of the commit object.
func (c *traversalCommit) ID() plumbing.Hash { return c.id }

// Tree returns the hash of the tree referenced by the commit.
func (c *traversalCommit) Tree() plumbing.Hash { return c.treeHash }

// Parents returns the hashes of the commit's parents.
func (c *traversalCommit) Parents() []plumbing.Hash { return c.parentHashes }

// When returns the committer timestamp.
func (c *traversalCommit) When() time.Time { return c.when }

// AuthorWhen returns the author timestamp.
func (c *traversalCommit) AuthorWhen() time.Time { return c.authorWhen }

// decodeTraversalCommit parses the base traversal fields from an encoded commit object.
// It reads only up to and including the committer header, which in canonical
// git commit order precedes any gpgsig/mergetag/extra headers and the message
// body, so those are never parsed.
func decodeTraversalCommit(obj plumbing.EncodedObject, id plumbing.Hash, expected formatcfg.ObjectFormat) (c *traversalCommit, err error) {
	if obj.Type() != plumbing.CommitObject {
		return nil, object.ErrUnsupportedObject
	}

	reader, err := obj.Reader()
	if err != nil {
		return nil, fmt.Errorf("open commit reader: %w", err)
	}
	defer ioutil.CheckClose(reader, &err)

	if expected != formatcfg.SHA1 && expected != formatcfg.SHA256 {
		return nil, formatcfg.ErrInvalidObjectFormat
	}
	if id.Size() != expected.Size() {
		return nil, errors.New("invalid commit hash width")
	}
	c = &traversalCommit{id: id, when: time.Unix(0, 0).UTC(), authorWhen: time.Unix(0, 0).UTC()}
	if err = c.decode(reader); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *traversalCommit) decode(reader io.Reader) error {
	br := gogitsync.GetBufioReader(reader)
	defer func() {
		// Encoded readers may alias large commit bodies; the pool must not retain them.
		br.Reset(nil)
		gogitsync.PutBufioReader(br)
	}()

	s := &commitScanner{r: br, commit: c}
	for state := scanCommitTree; state != nil; {
		state = state(s)
	}
	if s.err != nil {
		return s.err
	}
	if !s.sawTree {
		return errors.New("malformed commit: missing tree header")
	}
	return nil
}

type commitScanner struct {
	r            *bufio.Reader
	commit       *traversalCommit
	pending      []byte
	pendingAtEnd bool
	err          error

	sawTree bool
}

type commitState func(*commitScanner) commitState

// readLine returns the next line from the input. atEnd is true when EOF was
// reached (the returned line may still contain trailing unterminated bytes).
// Real I/O errors are recorded on the scanner and surfaced via decode's return.
func (s *commitScanner) readLine() (line []byte, atEnd bool) {
	if s.pending != nil {
		line = s.pending
		s.pending = nil
		return line, s.pendingAtEnd
	}
	line, err := s.r.ReadSlice('\n')
	switch {
	case errors.Is(err, io.EOF):
		return line, true
	case errors.Is(err, bufio.ErrBufferFull):
		// Line longer than the bufio buffer: fall back to a copy so the
		// full line is still returned. Rare for commit headers.
		buf := append([]byte(nil), line...)
		rest, rerr := s.r.ReadBytes('\n')
		buf = append(buf, rest...)
		switch {
		case errors.Is(rerr, io.EOF):
			return buf, true
		case rerr != nil:
			s.err = fmt.Errorf("read commit line: %w", rerr)
			return nil, true
		}
		return buf, false
	case err != nil:
		s.err = fmt.Errorf("read commit line: %w", err)
		return nil, true
	}
	return line, false
}

func (s *commitScanner) pushBack(line []byte, atEnd bool) {
	s.pending = line
	s.pendingAtEnd = atEnd
}

func (s *commitScanner) fail(err error) commitState {
	s.err = err
	return nil
}

func scanCommitTree(s *commitScanner) commitState {
	line, atEnd := s.readLine()
	if s.err != nil {
		return nil
	}
	if len(line) == 0 || isBlankLine(line) {
		return s.fail(errors.New("malformed commit: missing tree header"))
	}
	key, data := splitHeader(line)
	if string(key) != headerTree {
		return s.fail(errors.New("malformed commit: tree header must be first"))
	}
	h, ok := hashFromHex(data, s.commit.id.Size())
	if !ok {
		return s.fail(errors.New("invalid tree hash"))
	}
	s.commit.treeHash = h
	s.sawTree = true
	if atEnd {
		return nil
	}
	return scanCommitParents
}

func scanCommitParents(s *commitScanner) commitState {
	line, atEnd := s.readLine()
	if s.err != nil {
		return nil
	}
	if len(line) == 0 || isBlankLine(line) {
		return nil
	}
	key, data := splitHeader(line)
	if string(key) == headerParent {
		h, ok := hashFromHex(data, s.commit.id.Size())
		if !ok {
			return s.fail(errors.New("invalid parent hash"))
		}
		s.commit.parentHashes = append(s.commit.parentHashes, h)
		if atEnd {
			return nil
		}
		return scanCommitParents
	}
	s.pushBack(line, atEnd)
	return scanCommitAuthor
}

func scanCommitAuthor(s *commitScanner) commitState {
	line, atEnd := s.readLine()
	if s.err != nil {
		return nil
	}
	if len(line) == 0 || isBlankLine(line) {
		return nil
	}
	key, data := splitHeader(line)
	if string(key) == headerAuthor {
		w, ok := parseWhen(data)
		if !ok {
			w = time.Unix(0, 0).UTC()
		}
		s.commit.authorWhen = w
		if atEnd {
			return nil
		}
		return scanCommitCommitter
	}
	s.pushBack(line, atEnd)
	return scanCommitCommitter
}

func scanCommitCommitter(s *commitScanner) commitState {
	line, _ := s.readLine()
	if s.err != nil {
		return nil
	}
	if len(line) == 0 || isBlankLine(line) {
		return nil
	}
	key, data := splitHeader(line)
	if string(key) == headerCommitter {
		w, ok := parseWhen(data)
		if !ok {
			w = time.Unix(0, 0).UTC()
		}
		s.commit.when = w
	}
	return nil
}

func isBlankLine(line []byte) bool {
	return len(line) == 1 && line[0] == '\n'
}

func splitHeader(line []byte) (key, value []byte) {
	trimmed := bytes.TrimRight(line, "\n")
	k, v, ok := bytes.Cut(trimmed, []byte{' '})
	if !ok {
		return trimmed, nil
	}
	return k, v
}

// parseWhen extracts the timestamp and timezone from a git signature line.
// Like object.Signature.Decode, it reads no time from a line without an
// email enclosed in angle brackets.
func parseWhen(in []byte) (time.Time, bool) {
	openBracket := bytes.LastIndexByte(in, '<')
	closeBracket := bytes.LastIndexByte(in, '>')
	if openBracket < 0 || closeBracket < openBracket || closeBracket+2 >= len(in) {
		return time.Time{}, false
	}
	tail := in[closeBracket+2:]
	space := bytes.IndexByte(tail, ' ')
	if space < 0 {
		space = len(tail)
	}
	ts, ok := parseIntBytes(tail[:space])
	if !ok {
		return time.Time{}, false
	}
	when := time.Unix(ts, 0).UTC()

	tzStart := space + 1
	if tzStart+5 > len(tail) {
		return when, true
	}
	offset, ok := parseTimezoneOffset(tail[tzStart : tzStart+5])
	if !ok {
		return when, true
	}
	return when.In(time.FixedZone("", offset)), true
}

func parseIntBytes(in []byte) (int64, bool) {
	n, err := strconv.ParseInt(string(in), 10, 64)
	return n, err == nil
}

func parseTimezoneOffset(in []byte) (int, bool) {
	if len(in) != 5 {
		return 0, false
	}
	sign := 1
	switch in[0] {
	case '-':
		sign = -1
	case '+':
	default:
		return 0, false
	}
	hours, ok := parseTwoDigits(in[1], in[2])
	if !ok {
		return 0, false
	}
	mins, ok := parseTwoDigits(in[3], in[4])
	if !ok {
		return 0, false
	}
	return sign * ((hours * 60 * 60) + (mins * 60)), true
}

func parseTwoDigits(a, b byte) (int, bool) {
	if a < '0' || a > '9' || b < '0' || b > '9' {
		return 0, false
	}
	return int(a-'0')*10 + int(b-'0'), true
}

func hashFromHex(in []byte, size int) (plumbing.Hash, bool) {
	if len(in) != size*2 {
		return plumbing.ZeroHash, false
	}
	var raw [32]byte
	var n int
	switch len(in) {
	case 40:
		n = 20
	case 64:
		n = 32
	default:
		return plumbing.ZeroHash, false
	}
	if _, err := hex.Decode(raw[:n], in); err != nil {
		return plumbing.ZeroHash, false
	}
	return plumbing.FromBytes(raw[:n])
}
