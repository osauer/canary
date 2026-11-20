package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
)

const policyOwnerMark = "\n# the owner's own line\n"

// policyDirLayout is a machine where the daemon's config root is not
// ~/.config: the old directory under HOME and the new one under the root.
type policyDirLayout struct {
	s            *Server
	log          *bytes.Buffer
	set          PolicyFileSet
	oldDir       string
	ownerFiles   map[string][]byte // by file name, as left in the old directory
	constitution string            // the owner's risk policy fingerprint
}

// newPolicyDirLayout writes the four files a v3.34.1 daemon would have left
// in the old directory, each carrying a line of the owner's, and the risk
// policy with settings of its own so its fingerprint is not the default's.
func newPolicyDirLayout(t *testing.T) *policyDirLayout {
	t.Helper()
	dir := shortTempDir(t)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	l := &policyDirLayout{log: &bytes.Buffer{}, set: PolicyFileSetFor(nil), ownerFiles: map[string][]byte{}}
	l.s = &Server{coreStore: store, version: "v9.9.9", ensurePolicyFiles: true, logger: NewLogger(l.log, "warn")}
	l.oldDir = filepath.Join(dir, "home", ".config", "ibkr", "policies")
	if got := filepath.Dir(l.set.Constitution); got != filepath.Join(dir, "config", "ibkr", "policies") {
		t.Fatalf("policy directory=%s, want it under the config root", got)
	}
	templates := map[string][]byte{
		"rulebook-policy.toml":    RulebookPolicyTemplate("v3.34.1"),
		"protection-policy.toml":  ProtectionPolicyTemplate("v3.34.1"),
		"opportunity-policy.toml": OpportunityPolicyTemplate("v3.34.1"),
		"risk-policy.toml":        constitutionPolicyTemplateFrom("v3.34.1", l.set.OrderGateSource, l.set.OrderGateSourceRead),
	}
	for name, data := range templates {
		if name == "risk-policy.toml" {
			if !bytes.Contains(data, []byte("policy_version = 1\n")) {
				t.Fatal("the risk policy template no longer starts at policy_version 1; give this fixture another owner setting")
			}
			data = bytes.Replace(data, []byte("policy_version = 1\n"), []byte("policy_version = 7\n"), 1)
		}
		data = append(data, policyOwnerMark...)
		l.ownerFiles[name] = data
		if err := writePrivateFileExclusive(filepath.Join(l.oldDir, name), data); err != nil {
			t.Fatal(err)
		}
	}
	c, err := parseConstitutionFile(l.ownerFiles["risk-policy.toml"])
	if err != nil {
		t.Fatal(err)
	}
	l.constitution = c.FingerprintKey()
	return l
}

// ranOn journals that this daemon activated a risk policy with fingerprint,
// as every daemon does at start.
func (l *policyDirLayout) ranOn(t *testing.T, fingerprint string) {
	t.Helper()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{"kind": "policy_status", "policy_fingerprint": fingerprint})
	key, err := coreStoreEventKey(t.Context(), l.s.coreStore, coreEventRiskPolicy, at, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.s.coreStore.AppendEvents(t.Context(), []corestore.EventInput{{
		ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRiskPolicy, Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
		OccurredAt: at, PayloadJSON: raw, Projection: corestore.EventProjection{RiskPolicyEvent: &corestore.RiskPolicyEventProjection{Kind: "policy_status", PolicyFingerprint: fingerprint}},
	}}); err != nil {
		t.Fatal(err)
	}
}

// start is the policy part of a daemon start, as the daemon runs it. It
// returns what that start logged.
func (l *policyDirLayout) start() string {
	l.log.Reset()
	l.s.ensurePolicyFilesOnStart()
	return l.log.String()
}

func (l *policyDirLayout) paths() map[string]string {
	return map[string]string{
		"rulebook-policy.toml": l.set.Rulebook, "protection-policy.toml": l.set.Protection,
		"opportunity-policy.toml": l.set.Opportunity, "risk-policy.toml": l.set.Constitution,
	}
}

func (l *policyDirLayout) oldDirUntouched(t *testing.T) {
	t.Helper()
	for name, want := range l.ownerFiles {
		if got, err := os.ReadFile(filepath.Join(l.oldDir, name)); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("old directory's %s was changed or removed: %v", name, err)
		}
	}
	entries, _ := os.ReadDir(l.oldDir)
	if len(entries) != len(l.ownerFiles) {
		t.Fatalf("old directory holds %d files, want the owner's %d and nothing written beside them", len(entries), len(l.ownerFiles))
	}
}

// A daemon whose config root is not ~/.config, updated from v3.34.1, finds
// its four policy files in the old directory and nothing in the new one.
// v3.35.0 through v3.40.0 wrote Canary's defaults there and ran on them, so
// the owner's limits stopped applying without a word. The daemon's own
// record shows it ran on the old risk policy, so start adopts all four, and
// the old directory is only read.
func TestPolicyFilesFromTheOldDirectoryAreAdoptedAtStart(t *testing.T) {
	l := newPolicyDirLayout(t)
	l.ranOn(t, l.constitution)
	l.start()
	for name, path := range l.paths() {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Contains(got, []byte(strings.TrimSpace(policyOwnerMark))) {
			t.Fatalf("%s in use is not the owner's file: Canary's defaults are in force in place of the owner's limits", name)
		}
	}
	c, err := parseConstitutionFile(mustReadFile(t, l.set.Constitution))
	if err != nil || c.PolicyVersion < 7 {
		t.Fatalf("risk policy in use: version=%v err=%v, want the owner's settings", c, err)
	}
	l.oldDirUntouched(t)
	// A second start changes nothing more and has nothing to say.
	before := mustReadFile(t, l.set.Constitution)
	if logged := l.start(); !bytes.Equal(before, mustReadFile(t, l.set.Constitution)) || strings.Contains(logged, "v3.35.0") {
		t.Fatalf("a second start rewrote the adopted risk policy or spoke of the old directory again:\n%s", logged)
	}
}

// Each file is settled once. After that the owner changes a limit in the
// adopted file, which now differs from the stale one in the old directory.
// That is no conflict: the next start leaves the file alone, says nothing,
// and `policy show --explain` has no note. A file the owner then deletes is
// not adopted a second time.
func TestAnAdoptedPolicyFileIsSettledOnce(t *testing.T) {
	l := newPolicyDirLayout(t)
	l.ranOn(t, l.constitution)
	l.start()
	edited := bytes.Replace(mustReadFile(t, l.set.Constitution), []byte("policy_version = 7\n"), []byte("policy_version = 8\n"), 1)
	if err := writePrivateFile(l.set.Constitution, edited); err != nil {
		t.Fatal(err)
	}
	logged := l.start()
	if strings.Contains(logged, "v3.35.0") || len(l.s.policyDirNotes(t.Context())) != 0 {
		t.Fatalf("the owner's later edit of an adopted file is reported as a conflict with the stale old file:\n%s", logged)
	}
	c, err := parseConstitutionFile(mustReadFile(t, l.set.Constitution))
	if err != nil || c.PolicyVersion < 8 {
		t.Fatalf("the owner's later edit was lost: %v %v", c, err)
	}
	if err := os.Remove(l.set.Rulebook); err != nil {
		t.Fatal(err)
	}
	l.start()
	if bytes.Contains(mustReadFile(t, l.set.Rulebook), []byte(strings.TrimSpace(policyOwnerMark))) {
		t.Fatal("a policy file the owner deleted came back from the old directory")
	}
	l.oldDirUntouched(t)
}

// The directory moved so that two daemons can keep separate policies. A
// daemon given its own config root has no record of ever running on the
// files in the old directory, which are another daemon's: it gets Canary's
// defaults, never that daemon's limits or its permission to send orders.
func TestASeparateDaemonDoesNotInheritAnotherDaemonsPolicyFiles(t *testing.T) {
	l := newPolicyDirLayout(t)
	// What every fresh daemon has journalled: Canary's default policy.
	def, err := parseConstitutionFile(constitutionPolicyTemplateFrom(l.s.version, l.set.OrderGateSource, l.set.OrderGateSourceRead))
	if err != nil {
		t.Fatal(err)
	}
	l.ranOn(t, def.FingerprintKey())
	if logged := l.start(); strings.Contains(logged, "v3.35.0") {
		t.Fatalf("a separate daemon spoke of the old directory:\n%s", logged)
	}
	for name, path := range l.paths() {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(got, []byte(strings.TrimSpace(policyOwnerMark))) {
			t.Fatalf("%s: a separate daemon was given another daemon's policy file", name)
		}
	}
	l.oldDirUntouched(t)
}

// The installer runs `canary policy ensure` with the new binary before the
// daemon restarts, and that step cannot read the daemon's record. Writing
// defaults then would put them where the daemon looks, in place of the files
// in the old directory. It writes nothing for those and says why; the daemon
// adopts them at its start.
func TestEnsureWithoutTheDaemonsRecordLeavesTheOldDirectorysFilesToTheDaemon(t *testing.T) {
	l := newPolicyDirLayout(t)
	l.ranOn(t, l.constitution)
	for _, dry := range []bool{true, false} {
		for _, a := range EnsurePolicyFiles(l.set, EnsureOptions{Release: l.s.version, DryRun: dry}) {
			if a.Action != PolicyFileLeftToDaemon || len(a.Notes) == 0 {
				t.Fatalf("dry=%v %s: action=%s notes=%v, want it left to the daemon with the reason", dry, a.Policy, a.Action, a.Notes)
			}
			if _, err := os.Stat(a.Path); !os.IsNotExist(err) {
				t.Fatalf("dry=%v %s: a file was written at %s", dry, a.Policy, a.Path)
			}
		}
	}
	l.start()
	if !bytes.Contains(mustReadFile(t, l.set.Constitution), []byte(strings.TrimSpace(policyOwnerMark))) {
		t.Fatal("after the installer's ensure step, daemon start did not adopt the owner's risk policy")
	}
	// With no file in the old directory the same step writes defaults as before.
	if err := os.RemoveAll(l.oldDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(l.set.Constitution)); err != nil {
		t.Fatal(err)
	}
	for _, a := range EnsurePolicyFiles(l.set, EnsureOptions{Release: l.s.version}) {
		if a.Action != PolicyFileCreated {
			t.Fatalf("%s: action=%s, want created", a.Policy, a.Action)
		}
	}
}

// A daemon already updated to v3.35.0 through v3.40.0 has Canary's defaults
// in the new directory. An untouched default is replaced by the owner's old
// file, and kept as a backup. A file the owner has since edited there is
// never replaced: both stay and the row says which is in use.
func TestAnUntouchedDefaultIsReplacedAndAnEditedFileIsKept(t *testing.T) {
	l := newPolicyDirLayout(t)
	l.ranOn(t, l.constitution)
	// What an affected release wrote: its own defaults, naming its release.
	older := map[string][]byte{
		l.set.Rulebook:     RulebookPolicyTemplate("v3.36.0"),
		l.set.Protection:   ProtectionPolicyTemplate("v3.36.0"),
		l.set.Opportunity:  OpportunityPolicyTemplate("v3.36.0"),
		l.set.Constitution: constitutionPolicyTemplateFrom("v3.36.0", l.set.OrderGateSource, l.set.OrderGateSourceRead),
	}
	// A setting changed there since the update. (A comment alone is not an
	// edit of the owner's settings.)
	edited := bytes.Replace(older[l.set.Rulebook], []byte("policy_version = 5\n"), []byte("policy_version = 6\n"), 1)
	if bytes.Equal(edited, older[l.set.Rulebook]) {
		t.Fatal("the Rulebook template no longer carries policy_version 5; change another setting in this fixture")
	}
	for path, data := range older {
		if path == l.set.Rulebook {
			data = edited
		}
		if err := writePrivateFileExclusive(path, data); err != nil {
			t.Fatal(err)
		}
	}
	logged := l.start()
	for _, path := range []string{l.set.Protection, l.set.Opportunity, l.set.Constitution} {
		if !bytes.Contains(mustReadFile(t, path), []byte(strings.TrimSpace(policyOwnerMark))) {
			t.Fatalf("%s: the owner's file is not in use", filepath.Base(path))
		}
		backups, _ := filepath.Glob(path + ".bak-*")
		kept := false
		for _, backup := range backups {
			kept = kept || bytes.Equal(mustReadFile(t, backup), older[path])
		}
		if !kept {
			t.Fatalf("%s: the default that was replaced is not kept as a backup (%d backups)", filepath.Base(path), len(backups))
		}
	}
	// The Rulebook policy was edited in both places: kept, said once at this
	// start, and noted for `policy show --explain` from then on.
	if !bytes.HasPrefix(mustReadFile(t, l.set.Rulebook), edited[:len(edited)-1]) || bytes.Contains(mustReadFile(t, l.set.Rulebook), []byte(strings.TrimSpace(policyOwnerMark))) {
		t.Fatal("a policy file the owner edited was replaced")
	}
	if !strings.Contains(logged, "policy file rulebook: unchanged") || !strings.Contains(logged, l.oldDir) {
		t.Fatalf("the start did not say that two Rulebook policies exist:\n%s", logged)
	}
	notes := l.s.policyDirNotes(t.Context())
	if len(notes) != 1 || !strings.Contains(notes[l.set.Rulebook], l.oldDir) {
		t.Fatalf("notes=%v, want one, on the Rulebook policy, naming the old file", notes)
	}
	if again := l.start(); strings.Contains(again, "v3.35.0") || len(l.s.policyDirNotes(t.Context())) != 1 {
		t.Fatalf("the next start said it again, or the note went away:\n%s", again)
	}
	l.oldDirUntouched(t)
}

// A policy file named explicitly in config.toml, or a config root that is
// ~/.config itself, has no twin in the old directory.
func TestLegacyPolicyPathIsOnlyForDefaultsUnderAnotherRoot(t *testing.T) {
	l := newPolicyDirLayout(t)
	if got := legacyPolicyPath(l.set.Constitution); got != filepath.Join(l.oldDir, "risk-policy.toml") {
		t.Fatalf("default under another root: %q", got)
	}
	if got := legacyPolicyPath(filepath.Join(shortTempDir(t), "risk-policy.toml")); got != "" {
		t.Fatalf("an explicitly placed file has a twin: %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := legacyPolicyPath(PolicyFileSetFor(nil).Constitution); got != "" {
		t.Fatalf("with no config root set the file is its own twin: %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(os.Getenv("HOME"), ".config"))
	if got := legacyPolicyPath(PolicyFileSetFor(nil).Constitution); got != "" {
		t.Fatalf("with the config root at ~/.config the file is its own twin: %q", got)
	}
}

// The adopted risk policy gets exactly the record its original had: a batch
// the old file was given is not written again (a table the owner deleted
// stays deleted), and a batch it never had is still owed.
func TestAdoptedRiskPolicyCarriesItsMandateDefaultsRecord(t *testing.T) {
	l := newPolicyDirLayout(t)
	oldPath, newPath := filepath.Join(l.oldDir, "risk-policy.toml"), l.set.Constitution
	given := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	records := map[string]time.Time{
		mandateDefaultsRecordKey(oldPath, ""):           given,
		mandateDefaultsRecordKey(oldPath, "2026-11-03"): given,
		// Recorded for the new path while Canary's defaults sat there.
		mandateDefaultsRecordKey(newPath, "2026-11-17"): given.AddDate(0, 0, 14),
	}
	raw, _ := json.Marshal(records)
	if _, err := l.s.coreStore.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindMandateDefaults, JSON: raw}); err != nil {
		t.Fatal(err)
	}
	l.s.carryMandateDefaults(t.Context(), oldPath, newPath)
	done, all := l.s.mandateDefaultsDone(newPath)
	if all || len(done) != 2 || done[0] != "" || done[1] != "2026-11-03" {
		t.Fatalf("done=%q all=%v, want the first two batches done and the third still owed", done, all)
	}
	if old, _ := l.s.mandateDefaultsDone(oldPath); len(old) != 2 {
		t.Fatalf("the old path's record changed: %q", old)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Two default files differ only in the release their comments name. A '#'
// inside a quoted value is not a comment.
func TestPolicyActiveLinesIgnoreCommentsOnly(t *testing.T) {
	a := []byte("# written by Canary v1\nwindow = \"10m\"  # written by Canary v1\n\nnote = \"a # b\" # c\npath = 'x#y'\n")
	b := []byte("window = \"10m\"\nnote = \"a # b\"\npath = 'x#y'\n# written by Canary v2\n")
	if policyActiveLines(a) != policyActiveLines(b) || !strings.Contains(policyActiveLines(a), `"a # b"`) || !strings.Contains(policyActiveLines(a), "'x#y'") {
		t.Fatalf("active lines differ or lost a quoted '#':\n%q\n%q", policyActiveLines(a), policyActiveLines(b))
	}
	if policyActiveLines(a) == policyActiveLines([]byte("window = \"15m\"\nnote = \"a # b\"\npath = 'x#y'\n")) {
		t.Fatal("a changed setting was not seen")
	}
}
