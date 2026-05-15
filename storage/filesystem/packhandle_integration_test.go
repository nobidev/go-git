//go:build !wasm

package filesystem

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	fixtures "github.com/go-git/go-git-fixtures/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
)

// openFDCount returns the number of open file descriptors for the
// current process on linux/darwin; skips elsewhere. Uses
// Readdirnames to avoid the per-entry stat that fails for the
// listing FD on darwin's /dev/fd.
func openFDCount(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("openFDCount: linux/darwin only")
	}
	var dir string
	switch runtime.GOOS {
	case "linux":
		dir = "/proc/self/fd"
	case "darwin":
		dir = "/dev/fd"
	}
	f, err := os.Open(dir)
	require.NoError(t, err)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	require.NoError(t, err)
	return len(names)
}

func TestIntegration_ConcurrentReadsSamePack(t *testing.T) {
	t.Parallel()
	fixture := fixtures.Basic().One()
	dir, err := fixture.DotGit()
	require.NoError(t, err)
	storage := NewStorage(dir, cache.NewObjectLRUDefault())
	t.Cleanup(func() { _ = storage.Close() })

	iter, err := storage.IterEncodedObjects(plumbing.AnyObject)
	require.NoError(t, err)
	var hashes []plumbing.Hash
	for i := 0; i < 8; i++ {
		obj, err := iter.Next()
		if err != nil {
			break
		}
		hashes = append(hashes, obj.Hash())
	}
	iter.Close()
	require.NotEmpty(t, hashes)

	var wg sync.WaitGroup
	for _, h := range hashes {
		wg.Go(func() {
			obj, err := storage.EncodedObject(plumbing.AnyObject, h)
			assert.NoError(t, err)
			assert.NotNil(t, obj)
		})
	}
	wg.Wait()
}

func TestIntegration_NoFDLeakAcrossOps(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("FD counting: linux/darwin only")
	}
	fixture := fixtures.Basic().One()
	dir, err := fixture.DotGit()
	require.NoError(t, err)
	storage := NewStorage(dir, cache.NewObjectLRUDefault())
	t.Cleanup(func() { _ = storage.Close() })

	baseline := openFDCount(t)

	iter, err := storage.IterEncodedObjects(plumbing.AnyObject)
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		_, err := iter.Next()
		if err != nil {
			break
		}
	}
	iter.Close()

	// Wait past the grace period. Real time — testing/synctest
	// doesn't help across process boundaries.
	time.Sleep(1500 * time.Millisecond)

	after := openFDCount(t)
	assert.LessOrEqual(t, after, baseline+2, "FD leak after scoped ops")
}

func TestIntegration_RepositoryCloseReleasesAll(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("FD counting: linux/darwin only")
	}
	fixture := fixtures.Basic().One()
	dir, err := fixture.DotGit()
	require.NoError(t, err)
	storage := NewStorage(dir, cache.NewObjectLRUDefault())

	baseline := openFDCount(t)

	iter, _ := storage.IterEncodedObjects(plumbing.AnyObject)
	for i := 0; i < 10; i++ {
		_, _ = iter.Next()
	}
	iter.Close()

	require.NoError(t, storage.Close())

	// Terminal Close bypasses grace timer — should be immediate.
	after := openFDCount(t)
	assert.LessOrEqual(t, after, baseline+2)
}

// TestIntegration_ReindexInvalidatesPackHandles verifies that
// Reindex closes the existing PackHandles synchronously (which drops
// mmap references), allowing the next access to rebuild fresh
// handles. The test reads, calls Reindex, then reads the same hash
// again to verify functional continuity.
func TestIntegration_ReindexInvalidatesPackHandles(t *testing.T) {
	t.Parallel()
	fixture := fixtures.Basic().One()
	scratchDir := t.TempDir()
	originalFS, err := fixture.DotGit()
	require.NoError(t, err)
	scratchFS := osfs.New(scratchDir)

	copyDotGit(t, originalFS, scratchFS)

	storage := NewStorage(scratchFS, cache.NewObjectLRUDefault())
	t.Cleanup(func() { _ = storage.Close() })

	// Force initial PackHandle construction.
	iter, err := storage.IterEncodedObjects(plumbing.AnyObject)
	require.NoError(t, err)
	obj1, err := iter.Next()
	require.NoError(t, err)
	require.NotNil(t, obj1)
	iter.Close()

	// Reindex — closes PackHandles synchronously.
	require.NoError(t, storage.Reindex())

	// Subsequent reads work; PackHandles are rebuilt against the
	// same files. In a real external-repack scenario the files
	// would be different, but verifying functional continuity is
	// enough.
	obj2, err := storage.EncodedObject(plumbing.AnyObject, obj1.Hash())
	require.NoError(t, err)
	require.NotNil(t, obj2)
	assert.Equal(t, obj1.Hash(), obj2.Hash())
}

func TestIntegration_KeepDescriptorsIsNoOp(t *testing.T) {
	t.Parallel()
	for _, keep := range []bool{false, true} {
		t.Run(boolName(keep), func(t *testing.T) {
			t.Parallel()
			fixture := fixtures.Basic().One()
			dir, err := fixture.DotGit()
			require.NoError(t, err)
			storage := NewStorageWithOptions(dir, cache.NewObjectLRUDefault(),
				Options{KeepDescriptors: keep})
			t.Cleanup(func() { _ = storage.Close() })

			iter, err := storage.IterEncodedObjects(plumbing.AnyObject)
			require.NoError(t, err)
			obj, err := iter.Next()
			require.NoError(t, err)
			require.NotNil(t, obj)
			iter.Close()
		})
	}
}

func boolName(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// copyDotGit copies the essential .git contents from src to dst.
// Best-effort; sufficient for read-only tests.
func copyDotGit(t *testing.T, src, dst billy.Filesystem) {
	t.Helper()
	walk := []string{"HEAD", "config", "packed-refs"}
	for _, p := range walk {
		copyOne(t, src, dst, p)
	}
	copyDir(t, src, dst, "refs")
	copyDir(t, src, dst, "objects")
}

func copyOne(t *testing.T, src, dst billy.Filesystem, path string) {
	t.Helper()
	rf, err := src.Open(path)
	if err != nil {
		return
	}
	defer rf.Close()
	data, err := io.ReadAll(rf)
	if err != nil {
		return
	}
	dstParent := filepath.Dir(path)
	_ = dst.MkdirAll(dstParent, 0o755)
	wf, err := dst.Create(path)
	require.NoError(t, err)
	defer wf.Close()
	_, err = wf.Write(data)
	require.NoError(t, err)
}

func copyDir(t *testing.T, src, dst billy.Filesystem, dir string) {
	t.Helper()
	entries, err := src.ReadDir(dir)
	if err != nil {
		return
	}
	_ = dst.MkdirAll(dir, 0o755)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			copyDir(t, src, dst, p)
		} else {
			copyOne(t, src, dst, p)
		}
	}
}
