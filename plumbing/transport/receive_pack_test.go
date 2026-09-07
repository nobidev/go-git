package transport

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/go-git/go-git/v6/utils/ioutil"
)

const receivePackTestHash = "0123456789012345678901234567890123456789"

// receivePackRequest builds a wire-format receive-pack body for the given
// commands, with ReportStatus advertised plus any extra caps. A packfile
// follows unless every command is a Delete, because receive-pack expects one
// there; the pack is empty so that these tests stay on ref handling rather than
// pack decoding.
func receivePackRequest(t *testing.T, cmds []*packp.Command, extra ...capability.Capability) io.ReadCloser {
	t.Helper()

	caps := capability.List{}
	caps.Add(capability.ReportStatus)
	for _, c := range extra {
		caps.Add(c)
	}

	req := &packp.UpdateRequests{
		Capabilities: caps,
		Commands:     cmds,
	}

	var buf bytes.Buffer
	require.NoError(t, req.Encode(&buf))

	for _, cmd := range cmds {
		if cmd.Action() != packp.Delete {
			// A zero-object packfile: the "PACK" signature, version 2 and an
			// object count of 0, followed by the SHA-1 of those twelve bytes.
			header := []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x00")
			sum := sha1.Sum(header)
			buf.Write(header)
			buf.Write(sum[:])
			break
		}
	}

	return io.NopCloser(&buf)
}

func deleteCmd(ref plumbing.ReferenceName, hash plumbing.Hash) *packp.Command {
	return &packp.Command{Name: ref, Old: hash, New: plumbing.ZeroHash}
}

func seedRef(t *testing.T, ref plumbing.ReferenceName, hash plumbing.Hash) storage.Storer {
	t.Helper()
	st := memory.NewStorage()
	require.NoError(t, st.SetReference(plumbing.NewHashReference(ref, hash)))
	return st
}

func TestReceivePackNilHooksDeleteRef(t *testing.T) {
	t.Parallel()

	ref := plumbing.ReferenceName("refs/heads/main")
	hash := plumbing.NewHash(receivePackTestHash)
	st := seedRef(t, ref, hash)

	var out bytes.Buffer
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{deleteCmd(ref, hash)}),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{StatelessRPC: true},
	)
	require.NoError(t, err)

	assert.Contains(t, out.String(), "unpack ok")
	assert.Contains(t, out.String(), "ok refs/heads/main")

	_, err = st.Reference(ref)
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

func TestReceivePackPreReceiveAllowsUpdate(t *testing.T) {
	t.Parallel()

	ref := plumbing.ReferenceName("refs/heads/main")
	hash := plumbing.NewHash(receivePackTestHash)
	st := seedRef(t, ref, hash)

	var (
		out  bytes.Buffer
		info *PreReceiveInfo
	)
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{deleteCmd(ref, hash)}),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{
			StatelessRPC: true,
			Hooks: ReceivePackHooks{
				PreReceive: func(_ context.Context, i *PreReceiveInfo) error {
					info = i
					return nil
				},
			},
		},
	)
	require.NoError(t, err)

	require.NotNil(t, info)
	assert.Same(t, st, info.Storer)
	assert.NotNil(t, info.Progress)
	assert.Empty(t, info.PushOptions)
	require.Len(t, info.Commands, 1)
	assert.Equal(t, ref, info.Commands[0].Name)
	assert.Equal(t, packp.Delete, info.Commands[0].Action())
	assert.Contains(t, out.String(), "ok refs/heads/main")
}

func TestReceivePackPreReceiveRejectsRef(t *testing.T) {
	t.Parallel()

	ref := plumbing.ReferenceName("refs/heads/main")
	hash := plumbing.NewHash(receivePackTestHash)
	st := seedRef(t, ref, hash)

	postReceiveCalled := false
	var out bytes.Buffer
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{deleteCmd(ref, hash)}),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{
			StatelessRPC: true,
			Hooks: ReceivePackHooks{
				PreReceive: func(context.Context, *PreReceiveInfo) error {
					return errors.New("policy blocks main")
				},
				PostReceive: func(context.Context, *PostReceiveInfo) error {
					postReceiveCalled = true
					return nil
				},
			},
		},
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "policy blocks main")

	assert.Contains(t, out.String(), "unpack ok")
	assert.Contains(t, out.String(), "ng refs/heads/main policy blocks main")
	assert.False(t, postReceiveCalled, "PostReceive must not run when PreReceive rejects")

	got, err := st.Reference(ref)
	require.NoError(t, err)
	assert.Equal(t, hash, got.Hash(), "ref must not move when PreReceive rejects")
}

func TestReceivePackPostReceiveRunsAfterUpdate(t *testing.T) {
	t.Parallel()

	ref := plumbing.ReferenceName("refs/heads/main")
	hash := plumbing.NewHash(receivePackTestHash)
	st := seedRef(t, ref, hash)

	var (
		out        bytes.Buffer
		info       *PostReceiveInfo
		refMissing bool
	)
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{deleteCmd(ref, hash)}),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{
			StatelessRPC: true,
			Hooks: ReceivePackHooks{
				PostReceive: func(_ context.Context, i *PostReceiveInfo) error {
					info = i
					_, refErr := i.Storer.Reference(ref)
					refMissing = errors.Is(refErr, plumbing.ErrReferenceNotFound)
					return nil
				},
			},
		},
	)
	require.NoError(t, err)

	require.NotNil(t, info)
	require.Len(t, info.Commands, 1)
	assert.Equal(t, ref, info.Commands[0].Name)
	assert.NotNil(t, info.Progress)
	assert.True(t, refMissing, "ref must be gone by the time PostReceive runs")
}

func TestReceivePackPostReceivePartialSuccess(t *testing.T) {
	t.Parallel()

	good := plumbing.ReferenceName("refs/heads/good")
	bad := plumbing.ReferenceName("refs/heads/bad")
	hash := plumbing.NewHash(receivePackTestHash)
	// Only seed `good`; deleting `bad` will fail with ErrUpdateReference.
	st := seedRef(t, good, hash)

	var (
		out  bytes.Buffer
		info *PostReceiveInfo
	)
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{
			deleteCmd(good, hash),
			deleteCmd(bad, hash),
		}),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{
			StatelessRPC: true,
			Hooks: ReceivePackHooks{
				PostReceive: func(_ context.Context, i *PostReceiveInfo) error {
					info = i
					return nil
				},
			},
		},
	)
	require.ErrorIs(t, err, ErrUpdateReference)

	require.NotNil(t, info)
	require.Len(t, info.Commands, 1, "PostReceive must only see refs that applied")
	assert.Equal(t, good, info.Commands[0].Name)

	// A refused ref is reported per-command; the unpack status describes the
	// packfile only and must stay "ok".
	assert.Contains(t, out.String(), "unpack ok")
	assert.Contains(t, out.String(), "ok refs/heads/good")
	assert.Contains(t, out.String(), "ng refs/heads/bad")
}

func TestReceivePackPreReceiveWritesProgressOnSideband(t *testing.T) {
	t.Parallel()

	ref := plumbing.ReferenceName("refs/heads/main")
	hash := plumbing.NewHash(receivePackTestHash)
	st := seedRef(t, ref, hash)

	var out bytes.Buffer
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{deleteCmd(ref, hash)}, capability.Sideband64k),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{
			StatelessRPC: true,
			Hooks: ReceivePackHooks{
				PreReceive: func(_ context.Context, info *PreReceiveInfo) error {
					_, _ = io.WriteString(info.Progress, "policy check passed\n")
					return nil
				},
			},
		},
	)
	require.NoError(t, err)

	demuxed := readSideband(t, &out)
	assert.Contains(t, demuxed.progress.String(), "policy check passed")
	assert.Contains(t, demuxed.data.String(), "ok refs/heads/main")
}

type sidebandPayload struct {
	data     bytes.Buffer
	progress bytes.Buffer
}

func readSideband(t *testing.T, r io.Reader) sidebandPayload {
	t.Helper()
	var p sidebandPayload
	demux := sideband.NewDemuxer(sideband.Sideband64k, r)
	demux.Progress = &p.progress
	_, err := io.Copy(&p.data, demux)
	require.NoError(t, err)
	return p
}

// funnyNames are refnames receive-pack must refuse whatever storer sits behind
// it: upstream's builtin/receive-pack.c reports "funny refname" for a command
// whose name is not under refs/ or fails its format check. Every test here
// drives ReceivePack against memory.NewStorage(), so a refusal can only come
// from the transport gate — the dotgit layer's own checks are not in the way.
var funnyNames = []plumbing.ReferenceName{
	// Root refs and top-level metadata: not under refs/ at all.
	"HEAD",
	"CONFIG",
	"config",
	"INDEX",
	"SHALLOW",
	"ORIG_HEAD",
	// Escapes from the refs/ sub-tree, spelled literally...
	"refs/../CONFIG",
	"refs/heads/../../config",
	// ...and disguised with the code points HFS+ drops during path
	// normalisation. Each component below is a ".." to the filesystem while
	// holding no literal "..", which is exactly what carries it past IsSafe
	// (a literal comparison) and Validate (rule 3, "contains ..").
	// ZERO WIDTH NON-JOINER around the dots:
	"refs/\u200c.\u200c./CONFIG",
	"refs/heads/\u200c.\u200c./\u200c.\u200c./config",
	// ZERO WIDTH NO-BREAK SPACE (the UTF-8 BOM):
	"refs/\ufeff.\ufeff.\ufeff/CONFIG",
	// LEFT-TO-RIGHT MARK:
	"refs/\u200e.\u200e./config",
	// A component the filesystem reads as a single "." aliases the directory
	// it sits in, so each of these names resolves to a ref one level up while
	// holding no literal "." component. The leading-ignorable spellings pass
	// every other gate: IsSafe compares literally, and Validate's rule 1 sees
	// a component starting with a code point rather than a dot.
	"refs/\u200c./heads/main",
	"refs/heads/\u200c./main",
	"refs/heads/\u200c.\u200c/main",
	"refs/\ufeff./heads/main",
	"refs/heads/.:$DATA/main",
	// The trailing-space spellings NTFS also folds ("refs/heads/. /main") are
	// absent because they cannot arrive here: a pkt-line command is
	// space-separated, so the name is truncated at the space during decoding
	// and never reaches the gate whole. pathutil and the dotgit storer cover
	// them, where a name arrives as a Go string rather than off the wire.
	// Malformed by check_refname_format.
	"refs/",
	"refs/heads/foo..bar",
	"refs/heads/foo.lock",
	"refs/heads/foo\\bar",
	// Knowingly stricter than upstream: real git accepts both of these on
	// push, but ReferenceName.Validate refuses a third component starting with
	// "-" and a component spelled "@", and this gate reuses Validate. Pinned so
	// the divergence stays a decision on record rather than a surprise.
	"refs/heads/-foo",
	"refs/heads/@",
}

func TestReceivePackRefusesFunnyRefnameCreate(t *testing.T) {
	t.Parallel()

	for _, name := range funnyNames {
		t.Run(name.String(), func(t *testing.T) {
			t.Parallel()

			st := memory.NewStorage()
			hash := plumbing.NewHash(receivePackTestHash)

			var out bytes.Buffer
			err := ReceivePack(
				context.Background(),
				st,
				receivePackRequest(t, []*packp.Command{
					{Name: name, Old: plumbing.ZeroHash, New: hash},
				}),
				ioutil.WriteNopCloser(&out),
				&ReceivePackRequest{StatelessRPC: true},
			)
			require.ErrorIs(t, err, ErrFunnyRefname)

			assert.Contains(t, out.String(), "unpack ok")
			assert.Contains(t, out.String(), "ng "+name.String()+" funny refname")

			_, refErr := st.Reference(name)
			assert.ErrorIs(t, refErr, plumbing.ErrReferenceNotFound,
				"%q must not be created", name)
		})
	}
}

func TestReceivePackRefusesFunnyRefnameUpdate(t *testing.T) {
	t.Parallel()

	for _, name := range funnyNames {
		t.Run(name.String(), func(t *testing.T) {
			t.Parallel()

			old := plumbing.NewHash("1111111111111111111111111111111111111111")
			st := seedRef(t, name, old)

			var out bytes.Buffer
			err := ReceivePack(
				context.Background(),
				st,
				receivePackRequest(t, []*packp.Command{
					{Name: name, Old: old, New: plumbing.NewHash(receivePackTestHash)},
				}),
				ioutil.WriteNopCloser(&out),
				&ReceivePackRequest{StatelessRPC: true},
			)
			require.ErrorIs(t, err, ErrFunnyRefname)

			assert.Contains(t, out.String(), "ng "+name.String()+" funny refname")

			got, refErr := st.Reference(name)
			require.NoError(t, refErr)
			assert.Equal(t, old, got.Hash(), "%q must not move", name)
		})
	}
}

func TestReceivePackRefusesFunnyRefnameDelete(t *testing.T) {
	t.Parallel()

	for _, name := range funnyNames {
		t.Run(name.String(), func(t *testing.T) {
			t.Parallel()

			hash := plumbing.NewHash(receivePackTestHash)
			st := seedRef(t, name, hash)

			var out bytes.Buffer
			err := ReceivePack(
				context.Background(),
				st,
				receivePackRequest(t, []*packp.Command{deleteCmd(name, hash)}),
				ioutil.WriteNopCloser(&out),
				&ReceivePackRequest{StatelessRPC: true},
			)
			require.ErrorIs(t, err, ErrFunnyRefname)

			assert.Contains(t, out.String(), "ng "+name.String()+" funny refname")

			_, refErr := st.Reference(name)
			assert.NoError(t, refErr, "%q must not be deleted", name)
		})
	}
}

func TestReceivePackFunnyRefnameDoesNotBlockGoodRefs(t *testing.T) {
	t.Parallel()

	good := plumbing.ReferenceName("refs/heads/ok")
	hash := plumbing.NewHash(receivePackTestHash)
	st := memory.NewStorage()

	var out bytes.Buffer
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{
			{Name: "CONFIG", Old: plumbing.ZeroHash, New: hash},
			{Name: good, Old: plumbing.ZeroHash, New: hash},
		}),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{StatelessRPC: true},
	)
	require.ErrorIs(t, err, ErrFunnyRefname)

	assert.Contains(t, out.String(), "ng CONFIG funny refname")
	assert.Contains(t, out.String(), "ok refs/heads/ok")

	ref, refErr := st.Reference(good)
	require.NoError(t, refErr)
	assert.Equal(t, hash, ref.Hash())
}

func TestReceivePackFunnyRefnameReportsOnSideband(t *testing.T) {
	t.Parallel()

	st := memory.NewStorage()

	var out bytes.Buffer
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{
			{Name: "CONFIG", Old: plumbing.ZeroHash, New: plumbing.NewHash(receivePackTestHash)},
		}, capability.Sideband64k),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{StatelessRPC: true},
	)
	require.ErrorIs(t, err, ErrFunnyRefname)

	demuxed := readSideband(t, &out)
	assert.Contains(t, demuxed.data.String(), "unpack ok")
	assert.Contains(t, demuxed.data.String(), "ng CONFIG funny refname")
}

// TestReceivePackFunnyRefnameWireFormat pins the bytes rather than the error,
// because the report-status wire format is what a real git client parses: the
// unpack line stays "ok" (the packfile was fine, one command was not), the
// refusal is a single "ng" line carrying ErrFunnyRefname's message verbatim,
// and the report ends with a flush.
func TestReceivePackFunnyRefnameWireFormat(t *testing.T) {
	t.Parallel()

	st := memory.NewStorage()

	var out bytes.Buffer
	err := ReceivePack(
		context.Background(),
		st,
		receivePackRequest(t, []*packp.Command{
			{Name: "CONFIG", Old: plumbing.ZeroHash, New: plumbing.NewHash(receivePackTestHash)},
		}),
		ioutil.WriteNopCloser(&out),
		&ReceivePackRequest{StatelessRPC: true},
	)
	require.ErrorIs(t, err, ErrFunnyRefname)

	assert.Equal(t, "000eunpack ok\n001cng CONFIG funny refname\n0000", out.String())
}

func TestReceivePackAcceptsWellFormedRefs(t *testing.T) {
	t.Parallel()

	hash := plumbing.NewHash(receivePackTestHash)
	other := plumbing.NewHash("1111111111111111111111111111111111111111")

	for _, name := range []plumbing.ReferenceName{
		"refs/heads/main",
		"refs/heads/feature/nested/name",
		"refs/heads/release-1.2",
		"refs/tags/v1.0.0",
		"refs/stash",
		"refs/remotes/origin/HEAD",
		// Namespaces outside refs/heads and refs/tags that tools push to, all
		// accepted by upstream git's receive-pack.
		"refs/notes/commits",
		"refs/replace/deadbeef",
		"refs/meta/config",
		"refs/for/main",
		"refs/pull/1/head",
		"refs/keep-around/abc123",
	} {
		t.Run(name.String(), func(t *testing.T) {
			t.Parallel()

			st := memory.NewStorage()

			var out bytes.Buffer
			require.NoError(t, ReceivePack(
				context.Background(), st,
				receivePackRequest(t, []*packp.Command{
					{Name: name, Old: plumbing.ZeroHash, New: other},
				}),
				ioutil.WriteNopCloser(&out),
				&ReceivePackRequest{StatelessRPC: true},
			))
			assert.Contains(t, out.String(), "ok "+name.String())

			out.Reset()
			require.NoError(t, ReceivePack(
				context.Background(), st,
				receivePackRequest(t, []*packp.Command{
					{Name: name, Old: other, New: hash},
				}),
				ioutil.WriteNopCloser(&out),
				&ReceivePackRequest{StatelessRPC: true},
			))
			assert.Contains(t, out.String(), "ok "+name.String())
			ref, refErr := st.Reference(name)
			require.NoError(t, refErr)
			assert.Equal(t, hash, ref.Hash())

			out.Reset()
			require.NoError(t, ReceivePack(
				context.Background(), st,
				receivePackRequest(t, []*packp.Command{deleteCmd(name, hash)}),
				ioutil.WriteNopCloser(&out),
				&ReceivePackRequest{StatelessRPC: true},
			))
			assert.Contains(t, out.String(), "ok "+name.String())
			_, refErr = st.Reference(name)
			assert.ErrorIs(t, refErr, plumbing.ErrReferenceNotFound)
		})
	}
}
