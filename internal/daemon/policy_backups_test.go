package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Owner decision 2026-10-29: the newest five Canary-stamped backups and every
// one younger than thirty days stay; older ones beyond the five go; files
// without a Canary stamp are not Canary's to remove.
func TestPrunePolicyBackupsKeepsNewestFiveAndEverythingYoung(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "protection-policy.toml")
	now := time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	stamp := func(age time.Duration) string { return now.Add(-age).Format("20060102T150405Z") }
	day := 24 * time.Hour

	stamped := map[string]time.Duration{
		"protection-policy.toml.bak-v3.29.0-2-g83735137-" + stamp(1*day):   1 * day,
		"protection-policy.toml.bak-reset-" + stamp(2*day):                 2 * day,
		"protection-policy.toml.bak-desk-" + stamp(3*day) + "-2":           3 * day,
		"protection-policy.toml.bak-before-6885795a-" + stamp(31*day):      31 * day,
		"protection-policy.toml.bak-v3.18.1-10-gca83d1b1-" + stamp(40*day): 40 * day,
		"protection-policy.toml.bak-v3.17.0-27-g4966751f-" + stamp(50*day): 50 * day,
		"protection-policy.toml.bak-v3.17.0-18-g7727a5f1-" + stamp(60*day): 60 * day,
		"protection-policy.toml.bak-v3.12.1-74-g9922b63e-" + stamp(70*day): 70 * day,
	}
	handMade := []string{
		"protection-policy.toml.bak-20261005T2151-before-no-buy-while-borrowed",
		"protection-policy.toml.before-eur-allowlist-20261001",
		"protection-policy.toml.v2-2026-07-18.bak",
		"protection-policy.toml.bak-before-auto-release",
		// Another policy file's backup, old, must not be counted against this one.
		"risk-policy.toml.bak-v3.17.0-18-g7727a5f1-" + stamp(90*day),
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("protection-policy.toml")
	for name := range stamped {
		write(name)
	}
	for _, name := range handMade {
		write(name)
	}
	if err := os.Mkdir(filepath.Join(dir, "protection-policy.toml.bak-dir-"+stamp(80*day)), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, bytes, err := prunePolicyBackups(policy, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 3 || bytes != 3*6 {
		t.Fatalf("removed %d (%d B), want 3 (18 B)", removed, bytes)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	for name, age := range stamped {
		kept := slices.Contains(names, name)
		if want := age < 50*day; kept != want {
			t.Errorf("%s: kept=%v want %v", name, kept, want)
		}
	}
	for _, name := range handMade {
		if !slices.Contains(names, name) {
			t.Errorf("%s: a file without a Canary stamp was removed", name)
		}
	}
	if !slices.Contains(names, "protection-policy.toml") {
		t.Fatal("the policy file itself was removed")
	}
	// A second pass finds nothing more to do.
	if removed, _, err := prunePolicyBackups(policy, now); err != nil || removed != 0 {
		t.Fatalf("second prune: removed %d err %v", removed, err)
	}
}

func TestPrunePolicyBackupsLeavesFewOldBackupsAndMissingDirs(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "risk-policy.toml")
	now := time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	for i := range 4 {
		name := "risk-policy.toml.bak-v1-" + now.Add(-time.Duration(100+i)*24*time.Hour).Format("20060102T150405Z")
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if removed, _, err := prunePolicyBackups(policy, now); err != nil || removed != 0 {
		t.Fatalf("four old backups are under the keep count: removed %d err %v", removed, err)
	}
	if removed, _, err := prunePolicyBackups(filepath.Join(dir, "missing", "x.toml"), now); err != nil || removed != 0 {
		t.Fatalf("missing dir: removed %d err %v", removed, err)
	}
}
