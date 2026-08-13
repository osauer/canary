package logrotate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRotatesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 128), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("previous generation missing: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("fresh file size=%v err=%v, want empty", info, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// A long-lived process must rotate at runtime, not only at the next restart.
func TestWriteRotatesAtCapKeepingOneGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	w, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	line := bytes.Repeat([]byte("b"), 60)
	for range 5 {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	// 5 x 60 bytes with a 100-byte cap: rotations at writes 3 and 5; the
	// live file holds the final record, .1 holds the generation before it.
	info, err := os.Stat(path)
	if err != nil || info.Size() != 60 {
		t.Fatalf("live file size=%v err=%v, want one 60-byte record", info, err)
	}
	prev, err := os.Stat(path + ".1")
	if err != nil || prev.Size() != 120 {
		t.Fatalf("previous generation size=%v err=%v, want 120", prev, err)
	}
}
