package packhandle_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	billy "github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-billy/v6/util"
	fixtures "github.com/go-git/go-git-fixtures/v6"

	"github.com/go-git/go-git/v6/internal/packhandle"
	"github.com/go-git/go-git/v6/plumbing"
)

// Source policy:
//
//   - B1 (BenchmarkPackHandleAcquireGrace) and B3 (BenchmarkPackHandleMeta)
//     use the fixtures embed.FS via packhandle.PathSource. embedfs does not
//     opt into mmap (the embed-fs File backing is read directly into a
//     bytes.Reader); fine for these benchmarks because they measure the
//     cursorReader/sharedFile/Meta paths, not concurrent ReadAt scaling.
//
//   - B2 (BenchmarkPackHandleParallelReadAt) deliberately copies the pack
//     triple onto an osfs.New(b.TempDir(), osfs.WithMmap()) so the .pack
//     handle is mmap-backed. The benchmark validates the spec's claim that
//     concurrent ReadAt against one sharedFile scales near-linearly on
//     mmap-backed filesystems, so we need a real mmap implementation in
//     the loop on platforms that support it.

// fixturePackStem returns the "data/pack-<hash>" stem of the basic fixture
// inside the fixtures embed.FS.
func fixturePackStem(b *testing.B) string {
	b.Helper()
	return fmt.Sprintf("data/pack-%s", fixtures.Basic().One().PackfileHash)
}

// fixturePackHash returns the parsed plumbing.Hash for the basic fixture's
// pack file.
func fixturePackHash(b *testing.B) plumbing.Hash {
	b.Helper()
	h, ok := plumbing.FromHex(fixtures.Basic().One().PackfileHash)
	if !ok {
		b.Fatalf("fixture packfile hash unparseable: %q", fixtures.Basic().One().PackfileHash)
	}
	return h
}

// newEmbedFixturePackHandle constructs a PackHandle whose Sources read the
// basic fixture's pack triple directly from the fixtures embed.FS.
func newEmbedFixturePackHandle(b *testing.B) *packhandle.PackHandle {
	b.Helper()
	stem := fixturePackStem(b)
	return packhandle.New(packhandle.Sources{
		Pack: packhandle.PathSource(fixtures.Filesystem, stem+".pack"),
		Idx:  packhandle.PathSource(fixtures.Filesystem, stem+".idx"),
		Rev:  packhandle.PathSource(fixtures.Filesystem, stem+".rev"),
	}, fixturePackHash(b))
}

// copyFixturePackToTempDir copies the basic fixture's .pack/.idx/.rev into
// b.TempDir() and returns the directory plus the on-disk pack-file base name.
// The destination is an osfs+WithMmap so the .pack handle is mmap-backed on
// platforms that support it.
func copyFixturePackToTempDir(b *testing.B) (dir string, pack, idx, rev string) {
	b.Helper()
	dir = b.TempDir()
	stem := fixturePackStem(b)
	// Use a plain osfs to write the fixture; the mmap-backed osfs
	// used by the bench loop is constructed separately below.
	dstFS := osfs.New(dir)

	for _, ext := range []string{".pack", ".idx", ".rev"} {
		src, err := fixtures.Filesystem.Open(stem + ext)
		if err != nil {
			b.Fatalf("open fixture %s%s: %v", stem, ext, err)
		}
		data, err := io.ReadAll(src)
		_ = src.Close()
		if err != nil {
			b.Fatalf("read fixture %s%s: %v", stem, ext, err)
		}
		name := filepath.Base(stem) + ext
		if err := util.WriteFile(dstFS, name, data, 0o644); err != nil {
			b.Fatalf("write %s: %v", name, err)
		}
	}
	base := filepath.Base(stem)
	return dir, base + ".pack", base + ".idx", base + ".rev"
}

// packSizeFromFixture returns the on-disk size of the basic fixture's pack
// file, sourced from the fixtures embed.FS.
func packSizeFromFixture(b *testing.B) int64 {
	b.Helper()
	stem := fixturePackStem(b)
	info, err := fixtures.Filesystem.Stat(stem + ".pack")
	if err != nil {
		b.Fatalf("stat fixture pack: %v", err)
	}
	return info.Size()
}

// BenchmarkPackHandleAcquireGrace measures the steady-state cost of the
// OpenPackReader -> ReadAt -> Close cycle against a warm PackHandle. The
// 1-second grace period should keep the underlying sharedFile open across
// iterations, so the loop pays only cursorReader allocation, the
// sharedFile acquire/release mutex round-trip, and a single 64-byte ReadAt
// — never a fresh open(2).
func BenchmarkPackHandleAcquireGrace(b *testing.B) {
	ph := newEmbedFixturePackHandle(b)
	b.Cleanup(func() { _ = ph.Close() })

	packSize := packSizeFromFixture(b)
	validRange := packSize - 64
	if validRange <= 0 {
		b.Fatalf("pack too small: %d bytes", packSize)
	}

	// Warm the sharedFile so the first timed iteration also benefits
	// from the grace-period reuse.
	warm, err := ph.OpenPackReader()
	if err != nil {
		b.Fatal(err)
	}
	_ = warm.Close()

	buf := make([]byte, 64)

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		r, err := ph.OpenPackReader()
		if err != nil {
			b.Fatal(err)
		}
		off := int64(i) % validRange
		if _, err := r.(io.ReaderAt).ReadAt(buf, off); err != nil && err != io.EOF {
			b.Fatal(err)
		}
		_ = r.Close()
	}
}

// BenchmarkPackHandleParallelReadAt measures concurrent ReadAt against one
// PackHandle (sibling cursorReaders, one per goroutine) across two
// billy.File backings:
//
//   - packhandle_readat_fd: plain osfs.New(dir), the default go-billy
//     configuration. .pack reads go through (*os.File).ReadAt, which is
//     concurrent-safe via pread but serialises in the file's offset
//     management on some platforms.
//   - packhandle_readat_mmap: osfs.New(dir, osfs.WithMmap()), the opt-in
//     mmap backing. On darwin/linux .pack reads land in a memory-mapped
//     region; on other platforms WithMmap is a no-op and the bench
//     collapses onto the fd path.
//
// baseline_direct_pread is a direct (*os.File).ReadAt against the same
// file as a ceiling reference. Run with `-cpu=1,2,4,8` to see the
// scaling curve for each backing.
func BenchmarkPackHandleParallelReadAt(b *testing.B) {
	dir, packName, _, _ := copyFixturePackToTempDir(b)
	packPath := filepath.Join(dir, packName)

	info, err := os.Stat(packPath)
	if err != nil {
		b.Fatalf("stat pack: %v", err)
	}
	packSize := info.Size()
	validRange := packSize - 64
	if validRange <= 0 {
		b.Fatalf("pack too small: %d bytes", packSize)
	}

	runParallelReadAt := func(b *testing.B, fsys billy.Basic) {
		ph := packhandle.New(packhandle.Sources{
			Pack: packhandle.PathSource(fsys, packName),
		}, plumbing.ZeroHash)
		b.Cleanup(func() { _ = ph.Close() })

		// Pre-warm the sharedFile so the timed region begins with an
		// already-open file.
		warm, err := ph.OpenPackReader()
		if err != nil {
			b.Fatal(err)
		}
		_ = warm.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			r, err := ph.OpenPackReader()
			if err != nil {
				b.Fatal(err)
			}
			defer r.Close()
			ra := r.(io.ReaderAt)
			buf := make([]byte, 64)
			var i int64
			for pb.Next() {
				off := i % validRange
				if _, err := ra.ReadAt(buf, off); err != nil && err != io.EOF {
					b.Fatal(err)
				}
				i++
			}
		})
	}

	b.Run("packhandle_readat_fd", func(b *testing.B) {
		runParallelReadAt(b, osfs.New(dir))
	})

	b.Run("packhandle_readat_mmap", func(b *testing.B) {
		runParallelReadAt(b, osfs.New(dir, osfs.WithMmap()))
	})

	b.Run("baseline_direct_pread", func(b *testing.B) {
		f, err := os.OpenFile(packPath, os.O_RDONLY, 0)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = f.Close() })

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			buf := make([]byte, 64)
			var i int64
			for pb.Next() {
				off := i % validRange
				if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
					b.Fatal(err)
				}
				i++
			}
		})
	})
}

// BenchmarkPackHandleMeta measures PackHandle.Meta(). The "first"
// sub-benchmark constructs a fresh PackHandle per iteration and pays the
// cold cost: stat + acquire + two ReadAts + release. The "cached"
// sub-benchmark shares one PackHandle whose sync.OnceValues has already
// fired, so each iteration is just a struct copy out of the cached tuple.
func BenchmarkPackHandleMeta(b *testing.B) {
	stem := fixturePackStem(b)
	packPath := stem + ".pack"
	idxPath := stem + ".idx"
	revPath := stem + ".rev"
	packHash := fixturePackHash(b)

	b.Run("first", func(b *testing.B) {
		// Sanity-check the fixture once outside the loop to avoid
		// burning iterations on a missing-file failure.
		if _, err := fixtures.Filesystem.Stat(packPath); err != nil {
			b.Fatalf("stat fixture pack: %v", err)
		}

		// Use an atomic to keep the result observable to the compiler
		// and prevent over-eager dead-store elimination.
		var sink atomic.Uint32

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			ph := packhandle.New(packhandle.Sources{
				Pack: packhandle.PathSource(fixtures.Filesystem, packPath),
				Idx:  packhandle.PathSource(fixtures.Filesystem, idxPath),
				Rev:  packhandle.PathSource(fixtures.Filesystem, revPath),
			}, packHash)
			m, err := ph.Meta()
			if err != nil {
				b.Fatal(err)
			}
			sink.Store(m.Count)
			_ = ph.Close()
		}
	})

	b.Run("cached", func(b *testing.B) {
		ph := newEmbedFixturePackHandle(b)
		b.Cleanup(func() { _ = ph.Close() })

		// Warm the sync.OnceValues.
		if _, err := ph.Meta(); err != nil {
			b.Fatal(err)
		}

		var sink atomic.Uint32

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			m, err := ph.Meta()
			if err != nil {
				b.Fatal(err)
			}
			sink.Store(m.Count)
		}
	})
}
