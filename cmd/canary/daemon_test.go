package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDaemonLogSecuresExistingPath(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ibkr-daemon.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := openDaemonLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}

	assertMode(t, dir, 0o700)
	assertMode(t, path, 0o600)
}

func TestRotateDaemonLogIfLarge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ibkr-daemon.log")

	// Below the cap: left in place, no backup created.
	if err := os.WriteFile(path, []byte("small\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateDaemonLogIfLarge(path)
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("under-cap log must not rotate; stat(.1) err = %v, want not-exist", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "small\n" {
		t.Fatalf("under-cap log content = %q, %v; want preserved", b, err)
	}

	// At the cap (>=): rolled aside to .1, original path freed for a fresh
	// open. Truncate grows the file sparsely so the test stays instant.
	if err := os.Truncate(path, maxDaemonLogBytes); err != nil {
		t.Fatal(err)
	}
	rotateDaemonLogIfLarge(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("over-cap log must be renamed away; stat(path) err = %v, want not-exist", err)
	}
	info, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("over-cap log must become .1: %v", err)
	}
	if info.Size() != maxDaemonLogBytes {
		t.Fatalf(".1 size = %d, want %d", info.Size(), int64(maxDaemonLogBytes))
	}
}

func TestOpenDaemonLogRotatesOverCap(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ibkr-daemon.log")
	if err := os.WriteFile(path, []byte("OLD-HEADER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxDaemonLogBytes); err != nil {
		t.Fatal(err)
	}

	w, err := openDaemonLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "fresh\n"); err != nil {
		t.Fatal(err)
	}
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}

	// The fresh log holds only the new write; the over-cap content moved to .1.
	if b, err := os.ReadFile(path); err != nil || string(b) != "fresh\n" {
		t.Fatalf("fresh log = %q, %v; want just the post-rotation write", b, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated backup .1 missing: %v", err)
	}
	assertMode(t, path+".1", 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
