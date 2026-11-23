package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeEvidenceFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// Owner decision 2026-11-22: the newest two quarantines and any younger than
// seven days stay; hand-made folders are never touched.
func TestPruneStoreEvidenceKeepsNewestTwoDamagedAndYoung(t *testing.T) {
	state := t.TempDir()
	db := filepath.Join(state, "daemon.db")
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	stamp := func(age time.Duration) string { return now.Add(-age).Format("20060102T150405Z") }
	day := 24 * time.Hour
	damaged := filepath.Join(state, "damaged")
	for _, name := range []string{
		"outside-opener-" + stamp(20*day), // newest two by stamp: kept
		"lost-write-" + stamp(25*day),
		"lost-write-" + stamp(30*day),                 // old, beyond two: removed
		"damaged-" + stamp(40*day) + "-12345",         // salvage copy, old: removed
		"my-notes",                                    // hand-made: kept
		"before-restore-" + stamp(90*day) + "-manual", // not Canary's shape: kept
	} {
		writeEvidenceFile(t, filepath.Join(damaged, name, "daemon.db"))
	}

	removed, _, err := pruneStoreEvidence(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d, want 2", removed)
	}
	want := []string{"before-restore-" + stamp(90*day) + "-manual", "lost-write-" + stamp(25*day), "my-notes", "outside-opener-" + stamp(20*day)}
	slices.Sort(want)
	if got := dirNames(t, damaged); !slices.Equal(got, want) {
		t.Fatalf("damaged/ = %v, want %v", got, want)
	}

	// A young third quarantine is kept even beyond the newest two.
	writeEvidenceFile(t, filepath.Join(damaged, "outside-opener-"+stamp(1*day), "daemon.db"))
	writeEvidenceFile(t, filepath.Join(damaged, "outside-opener-"+stamp(2*day), "daemon.db"))
	if _, _, err := pruneStoreEvidence(db, now); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"outside-opener-" + stamp(1*day), "outside-opener-" + stamp(2*day)} {
		if _, err := os.Stat(filepath.Join(damaged, keep)); err != nil {
			t.Fatalf("young quarantine %s removed: %v", keep, err)
		}
	}
}

func TestPruneStoreEvidenceKeepsNewestUpgradeSet(t *testing.T) {
	state := t.TempDir()
	db := filepath.Join(state, "daemon.db")
	backups := filepath.Join(state, "backups")
	for _, name := range []string{
		"daemon.db-schema-v8-to-v9-976d92f4.maintenance.json",
		"daemon.db-schema-v8-to-v9-976d92f4.target.db",
		"daemon.db-schema-v9-to-v10-6675c01c.db",
		"daemon.db-schema-v10-to-v11-aa11bb22.db",
		"daemon.db-schema-v10-to-v11-aa11bb22.maintenance.json",
		"hand-copy.db",
	} {
		writeEvidenceFile(t, filepath.Join(backups, name))
	}
	now := time.Now()

	// A pending upgrade keeps every backup.
	writeEvidenceFile(t, coreSchemaUpgradeManifestPath(db))
	if removed, _, err := pruneStoreEvidence(db, now); err != nil || removed != 0 {
		t.Fatalf("pending upgrade: removed %d, err %v; want 0, nil", removed, err)
	}
	if err := os.Remove(coreSchemaUpgradeManifestPath(db)); err != nil {
		t.Fatal(err)
	}

	removed, _, err := pruneStoreEvidence(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d sets, want 2", removed)
	}
	want := []string{"daemon.db-schema-v10-to-v11-aa11bb22.db", "daemon.db-schema-v10-to-v11-aa11bb22.maintenance.json", "hand-copy.db"}
	if got := dirNames(t, backups); !slices.Equal(got, want) {
		t.Fatalf("backups/ = %v, want %v", got, want)
	}
}

func TestPruneStoreEvidenceWithoutFolders(t *testing.T) {
	if removed, _, err := pruneStoreEvidence(filepath.Join(t.TempDir(), "daemon.db"), time.Now()); err != nil || removed != 0 {
		t.Fatalf("removed %d, err %v; want 0, nil", removed, err)
	}
}
