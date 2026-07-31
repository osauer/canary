package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestAuthorityWatermarkRoundTripAndRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.db.head")
	if got, err := loadAuthorityWatermark(path); err != nil || got != nil {
		t.Fatalf("missing watermark = %+v, %v", got, err)
	}
	want := corestore.AuthorityHead{AuthorityEpoch: "epoch", HeadGeneration: 9, LastEventSeq: 7, SignerGeneration: 2}
	if err := writeAuthorityWatermark(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadAuthorityWatermark(path)
	if err != nil || got == nil || *got != want {
		t.Fatalf("watermark = %+v, %v; want %+v", got, err, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("watermark mode = %v, %v", info, err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Unix(123456789, 0)
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	if err := writeAuthorityWatermark(path, want); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || !info.ModTime().Equal(fixedTime) {
		t.Fatalf("identical watermark write changed bytes or timestamp: bytes_equal=%v mtime=%v", bytes.Equal(after, before), info.ModTime())
	}

	if err := os.WriteFile(path, []byte(`{"version":1,"head":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAuthorityWatermark(path); err == nil {
		t.Fatal("invalid watermark was accepted")
	}
}

func TestAuthorityWatermarkRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "daemon.db.head")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAuthorityWatermark(path); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink error = %v", err)
	}
}
