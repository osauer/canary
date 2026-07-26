package xdgcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWriteAtomicRoundTrip pins the happy path: bytes in, file readable
// at the destination with the exact contents. The atomic rename means
// the destination is either fully written or absent — never partial —
// which is the property the breadth/gamma/update caches all rely on.
func TestWriteAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "data.json")
	payload := []byte(`{"hello":"world"}`)

	if err := WriteAtomic(path, payload); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("round-trip:\n  want %q\n  got  %q", payload, got)
	}

	// Parent directory was auto-created.
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Errorf("parent dir not created: %v", err)
	}
}

// TestWriteAtomicOverwrites confirms the second write replaces the
// first. Without rename atomicity the consumer could read a half-old,
// half-new file mid-write.
func TestWriteAtomicOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	if err := WriteAtomic(path, []byte("v1")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteAtomic(path, []byte("v2")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("want v2, got %q", got)
	}
}

// TestWriteAtomicLeavesNoTempOnSuccess pins the cleanup invariant:
// after a successful write the only file in the dir is the destination,
// not a stranded `data.json.tmp.*` from os.CreateTemp.
func TestWriteAtomicLeavesNoTempOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	if err := WriteAtomic(path, []byte("payload")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
	if len(ents) != 1 {
		t.Errorf("expected 1 file in dir, got %d", len(ents))
	}
}

// TestOpenLockContention exercises the load-bearing property: a second
// acquisition while the first is held returns ErrLocked, not blocking
// or succeeding. The `canary update` flow relies on this to surface
// "another install in flight" rather than racing on the .bak rotation.
func TestOpenLockContention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	first, err := OpenLock(path)
	if err != nil {
		t.Fatalf("first OpenLock: %v", err)
	}
	defer first.Release()

	second, err := OpenLock(path)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second OpenLock: want ErrLocked, got %v", err)
	}
	if second != nil {
		t.Errorf("second OpenLock returned non-nil lock alongside error")
	}

	// Release the first; a third acquisition now succeeds.
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	third, err := OpenLock(path)
	if err != nil {
		t.Fatalf("third OpenLock after release: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("third Release: %v", err)
	}
}

// TestOpenLockReleaseIdempotent confirms Release can be called twice
// without panicking — the deferred-Release pattern relies on this when
// the caller has already cleaned up explicitly.
func TestOpenLockReleaseIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")
	l, err := OpenLock(path)
	if err != nil {
		t.Fatalf("OpenLock: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release: want nil, got %v", err)
	}
}

// TestOpenLockGoroutineContention exercises the concurrent case the
// daemon and `canary update` actually hit: two goroutines racing for the
// same lock. Exactly one acquires; the other sees ErrLocked.
func TestOpenLockGoroutineContention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	// Take the lock from the main goroutine first so the assertion is
	// deterministic — without that, the test is racy on which goroutine
	// wins. The goroutine just observes the contention.
	holder, err := OpenLock(path)
	if err != nil {
		t.Fatalf("holder OpenLock: %v", err)
	}
	defer holder.Release()

	var wg sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			l, err := OpenLock(path)
			if l != nil {
				_ = l.Release()
			}
			results <- err
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, ErrLocked) {
			t.Errorf("contender: want ErrLocked, got %v", err)
		}
	}
}

// TestCacheDirXDGPreferred pins the precedence: when both
// $XDG_CACHE_HOME and $HOME are set, XDG wins. The XDG-aware path is
// where containers, headless services, and users with a tweaked layout
// expect their cache to land.
func TestCacheDirXDGPreferred(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/custom/cache")
	t.Setenv("HOME", "/never/used")
	got, err := CacheDir("spx-members")
	if err != nil {
		t.Fatalf("CacheDir: %v", err)
	}
	want := filepath.Join("/custom/cache", "ibkr", "spx-members")
	if got != want {
		t.Errorf("CacheDir:\n  want %s\n  got  %s", want, got)
	}
}

// TestCacheDirHomeFallback exercises the XDG-spec documented default:
// when $XDG_CACHE_HOME is unset, callers land under $HOME/.cache.
func TestCacheDirHomeFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/home/test")
	got, err := CacheDir("update")
	if err != nil {
		t.Fatalf("CacheDir: %v", err)
	}
	want := filepath.Join("/home/test", ".cache", "ibkr", "update")
	if got != want {
		t.Errorf("CacheDir:\n  want %s\n  got  %s", want, got)
	}
}
