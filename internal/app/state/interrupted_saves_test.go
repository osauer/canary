package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveInterruptedSavesLeavesFreshTempsAndOtherFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write(".state-1708729306.tmp", 3*time.Hour)
	write(".state-1871744317.tmp", 25*time.Hour)
	write(".state-3745410802.tmp", 10*time.Minute)
	write("state.json", 48*time.Hour)
	write("app.lock", 48*time.Hour)
	write("other.tmp", 48*time.Hour)

	removed, err := RemoveInterruptedSaves(dir, now)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed %d, want 2", removed)
	}
	for _, name := range []string{".state-3745410802.tmp", "state.json", "app.lock", "other.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should still exist: %v", name, err)
		}
	}
	for _, name := range []string{".state-1708729306.tmp", ".state-1871744317.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s should be gone", name)
		}
	}
	if removed, err := RemoveInterruptedSaves(filepath.Join(dir, "missing"), now); err != nil || removed != 0 {
		t.Fatalf("missing dir: removed %d err %v", removed, err)
	}
}
