package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
)

// legacyPolicyDir is where the four policy files lived through v3.34.1,
// whatever XDG_CONFIG_HOME said. v3.35.0 moved their default under the XDG
// config root when that variable is set (config.PolicyDir), so that two
// daemons can keep separate policies, and read the old directory no more: a
// daemon with a non-default XDG_CONFIG_HOME that was updated across v3.35.0
// found no files, wrote Canary's defaults and ran on them, with the owner's
// limits left unread in the old directory.
const legacyPolicyDir = "~/.config/ibkr/policies"

// Ensure actions of the adoption step.
const (
	// PolicyFileAdopted: the file from the old directory was copied to
	// where this daemon reads its policies.
	PolicyFileAdopted = "adopted"
	// PolicyFileWouldAdopt is its dry run.
	PolicyFileWouldAdopt = "would_adopt"
	// PolicyFileLeftToDaemon: the ensure step ran without the daemon's
	// record (the installer's `canary policy ensure`) and found a file in
	// the old directory, so it wrote no default: the daemon decides at start.
	PolicyFileLeftToDaemon = "left_to_daemon"
)

// legacyPolicyPath returns the old-directory twin of a policy file that sits
// at its default place under a different XDG config root, or "" when there
// is none: the directories are the same, the path was set explicitly
// elsewhere (policy_file), or no regular file is there.
func legacyPolicyPath(path string) string {
	if path == "" {
		return ""
	}
	oldDir := filepath.Clean(expandUserPath(legacyPolicyDir))
	newDir := filepath.Clean(expandUserPath(config.PolicyDir()))
	if oldDir == newDir || filepath.Dir(filepath.Clean(path)) != newDir {
		return ""
	}
	if a, err := filepath.EvalSymlinks(oldDir); err == nil {
		if b, err := filepath.EvalSymlinks(newDir); err == nil && a == b {
			return ""
		}
	}
	legacy := filepath.Join(oldDir, filepath.Base(path))
	if info, err := os.Stat(legacy); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return legacy
}

// policyActiveLines is a policy file without its comments and blank lines.
// Canary's defaults differ between releases only in comments, which name the
// release that wrote them, so two files with the same active lines hold the
// same settings whichever release wrote them.
func policyActiveLines(data []byte) string {
	var b strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(stripTOMLComment(line))
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// stripTOMLComment cuts a line at the first '#' outside a quoted string.
func stripTOMLComment(line string) string {
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case quote == '"' && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return line[:i]
		}
	}
	return line
}

// legacyPolicyDirIsOurs says whether this daemon's own record shows it ran on
// the policy files now sitting in the old directory. Only then are they
// adopted. A second daemon given its own config root, which is what the
// directory move was for, has no such record and must not inherit another
// daemon's limits or its permission to send orders.
//
// The record is the journal of risk-policy status events: every risk policy
// and Rulebook policy the daemon activated, by fingerprint. A fingerprint
// equal to that of Canary's own default proves nothing, because every fresh
// daemon has activated the default; an edited file's fingerprint is its own.
// One of the two files is enough: until v3.35.0 all four defaults lived in
// the one directory.
func (s *Server) legacyPolicyDirIsOurs(ctx context.Context, set PolicyFileSet) bool {
	if s == nil || s.coreStore == nil {
		return false
	}
	seen := func(kind, legacy string, fingerprint func([]byte) (string, bool), template []byte) bool {
		if legacy == "" {
			return false
		}
		data, err := os.ReadFile(legacy)
		if err != nil {
			return false
		}
		fp, ok := fingerprint(data)
		if !ok {
			return false
		}
		if def, ok := fingerprint(template); ok && def == fp {
			return false
		}
		found, err := s.coreStore.RiskPolicyFingerprintSeen(ctx, kind, fp)
		return err == nil && found
	}
	constitution := func(data []byte) (string, bool) {
		c, err := parseConstitutionFile(data)
		if err != nil || c == nil {
			return "", false
		}
		return c.FingerprintKey(), true
	}
	rulebook := func(data []byte) (string, bool) {
		read, err := parseRulebookPolicy(data)
		if err != nil {
			return "", false
		}
		return read.policy.FingerprintKey(), true
	}
	return seen("policy_status", legacyPolicyPath(set.Constitution), constitution, constitutionPolicyTemplateFrom(s.version, set.OrderGateSource, set.OrderGateSourceRead)) ||
		seen("rulebook_policy_status", legacyPolicyPath(set.Rulebook), rulebook, RulebookPolicyTemplate(s.version))
}

// Outcomes of settling one policy file against its twin in the old directory.
const (
	policyDirAdopted         = "adopted"          // nothing was at the new place
	policyDirReplacedDefault = "replaced_default" // Canary's untouched defaults were
	policyDirSame            = "same"             // the two files were identical
	policyDirKeptBoth        = "kept_both"        // both carried settings of the owner's
)

const stateKindPolicyDirAdoption = "policy_dir_adoption"

// policyDirSettlement records that a policy file was settled against its
// twin in the old directory, once. From then on the old file is never read
// again for it: without the record the first later change to an adopted
// file, by the owner or by a start-up migration, would make the two differ
// and every later start would report a conflict with a file that is merely
// stale. It also keeps a file the owner deleted on purpose from coming back.
type policyDirSettlement struct {
	Legacy       string    `json:"legacy"`
	LegacySHA256 string    `json:"legacy_sha256"`
	Outcome      string    `json:"outcome"`
	At           time.Time `json:"at"`
}

func (s *Server) loadPolicyDirSettlements(ctx context.Context) (map[string]policyDirSettlement, int64, error) {
	out := map[string]policyDirSettlement{}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, stateKindPolicyDirAdoption)
	if err != nil || !ok {
		return out, 0, err
	}
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return nil, 0, fmt.Errorf("decode policy directory record: %w", err)
	}
	return out, doc.Revision, nil
}

const policyDirKeptBothNote = "this file is in use; a different one from before v3.35.0 is at %s and is not read. Both carry settings of yours, so Canary replaces neither: copy what you want to keep into this file"

// adoptLegacyPolicyFiles settles each policy file that has a twin in the old
// directory and is not in settled yet, and returns what it did with the
// outcome to record. The caller has established that the old files are this
// daemon's (legacyPolicyDirIsOurs). The old file is copied to where this
// daemon reads its policies when nothing is there, or when what is there
// still holds Canary's defaults untouched, which is what the affected
// releases wrote; that default is kept as a backup. A file the owner has
// edited at the new place is never replaced: both stay, and the note says
// which one is in use. The old directory is only read.
func adoptLegacyPolicyFiles(set PolicyFileSet, release string, now time.Time, settled map[string]policyDirSettlement) ([]PolicyFileAction, map[string]policyDirSettlement) {
	if now.IsZero() {
		now = time.Now()
	}
	var out []PolicyFileAction
	outcomes := map[string]policyDirSettlement{}
	for _, k := range []struct {
		name, path string
		template   func(string) []byte
	}{
		{PolicyFileRulebook, set.Rulebook, RulebookPolicyTemplate},
		{PolicyFileProtection, set.Protection, ProtectionPolicyTemplate},
		{PolicyFileOpportunity, set.Opportunity, OpportunityPolicyTemplate},
		{PolicyFileConstitution, set.Constitution, func(release string) []byte {
			return constitutionPolicyTemplateFrom(release, set.OrderGateSource, set.OrderGateSourceRead)
		}},
	} {
		if _, done := settled[k.path]; done {
			continue
		}
		legacy := legacyPolicyPath(k.path)
		if legacy == "" {
			continue
		}
		old, err := os.ReadFile(legacy)
		if err != nil {
			continue
		}
		act := PolicyFileAction{Policy: k.name, Path: k.path, Action: PolicyFileAdopted}
		outcome := policyDirAdopted
		current, err := os.ReadFile(k.path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			act.Notes = append(act.Notes, "copied from "+legacy+", where this daemon's policy files lived before v3.35.0")
			if err := writePrivateFileExclusive(k.path, old); err != nil {
				act.Action, act.Error = PolicyFileFailed, "adopt "+legacy+": "+err.Error()
			}
		case err != nil:
			continue
		case bytes.Equal(current, old):
			act.Action, outcome = PolicyFileUnchanged, policyDirSame
		case policyActiveLines(current) == policyActiveLines(k.template(release)):
			outcome = policyDirReplacedDefault
			act.Notes = append(act.Notes, "copied from "+legacy+", where this daemon's policy files lived before v3.35.0; the file it replaced held Canary's defaults untouched")
			backup := fmt.Sprintf("%s.bak-%s-%s", k.path, sanitizeReleaseLabel(release), now.UTC().Format("20060102T150405Z"))
			if err := writePrivateFileExclusive(backup, current); err != nil {
				act.Action, act.Error = PolicyFileFailed, "backup: "+err.Error()
			} else if err := writePrivateFile(k.path, old); err != nil {
				act.Action, act.Error = PolicyFileFailed, "adopt "+legacy+": "+err.Error()
			} else {
				act.Backup = backup
			}
		default:
			act.Action, outcome = PolicyFileUnchanged, policyDirKeptBoth
			act.Notes = append(act.Notes, fmt.Sprintf(policyDirKeptBothNote, legacy))
		}
		out = append(out, act)
		if act.Action != PolicyFileFailed {
			outcomes[k.path] = policyDirSettlement{Legacy: legacy, LegacySHA256: policyFileDigest(old), Outcome: outcome, At: now.UTC()}
		}
	}
	return out, outcomes
}

// carryMandateDefaults gives the adopted risk policy the record its original
// had. The record says which default batches a file at a path has had, and
// the start-up migration writes the others. Without it the adopted copy
// would count as never seen: a table the owner deleted on purpose would be
// written again. The new path gets exactly the old path's record, no more,
// so a batch the old file never had is still written.
func (s *Server) carryMandateDefaults(ctx context.Context, oldPath, newPath string) {
	if s == nil || s.coreStore == nil || oldPath == "" || newPath == "" || oldPath == newPath {
		return
	}
	records, revision, err := loadMandateDefaults(ctx, s.coreStore)
	if err != nil {
		s.warnf("carry mandate defaults record: %v", err)
		return
	}
	changed := false
	for _, b := range mandateDefaultBatches {
		from, to := mandateDefaultsRecordKey(oldPath, b.key), mandateDefaultsRecordKey(newPath, b.key)
		switch at, ok := records[from]; {
		case ok && !at.IsZero():
			if !records[to].Equal(at) {
				records[to], changed = at, true
			}
		default:
			if _, had := records[to]; had {
				delete(records, to)
				changed = true
			}
		}
	}
	if !changed {
		return
	}
	raw, err := json.Marshal(records)
	if err == nil {
		_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindMandateDefaults, ExpectedRevision: revision, JSON: raw})
	}
	if err != nil {
		s.warnf("carry mandate defaults record: %v", err)
	}
}

// adoptLegacyPolicyFilesOnStart runs before the ensure step, so that the
// adopted files are what it migrates. Each file is settled once and the
// outcome recorded; a failure to read or write the record settles nothing,
// so no file is changed without a record that it was.
func (s *Server) adoptLegacyPolicyFilesOnStart(set PolicyFileSet) {
	if s == nil || s.coreStore == nil {
		return
	}
	ctx := context.Background()
	settled, revision, err := s.loadPolicyDirSettlements(ctx)
	if err != nil {
		s.warnf("policy directory record unreadable, old policy files left alone: %v", err)
		return
	}
	pending := false
	for _, path := range []string{set.Rulebook, set.Protection, set.Opportunity, set.Constitution} {
		if _, done := settled[path]; !done && legacyPolicyPath(path) != "" {
			pending = true
		}
	}
	if !pending || !s.legacyPolicyDirIsOurs(ctx, set) {
		return
	}
	// The files are settled, then the record is written. If that write
	// fails the next start looks again and finds each adopted file equal to
	// its twin, which it records as the same.
	actions, outcomes := adoptLegacyPolicyFiles(set, s.version, time.Now(), settled)
	maps.Copy(settled, outcomes)
	raw, err := json.Marshal(settled)
	if err == nil {
		_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindPolicyDirAdoption, ExpectedRevision: revision, JSON: raw})
	}
	if err != nil {
		s.warnf("policy directory record not written; the old policy files will be looked at again at the next start: %v", err)
	}
	for _, a := range actions {
		if a.Action == PolicyFileUnchanged && len(a.Notes) == 0 {
			continue // identical files: settled, nothing to say
		}
		line := fmt.Sprintf("policy file %s: %s %s", a.Policy, a.Action, a.Path)
		if a.Backup != "" {
			line += " (backup " + a.Backup + ")"
		}
		if a.Error != "" {
			line += "; error: " + a.Error
		}
		if len(a.Notes) > 0 {
			line += "; " + strings.Join(a.Notes, "; ")
		}
		// Said once, at the start that settled it: limits came back into
		// force, or two files disagree.
		s.warnf("%s", line)
		if a.Policy == PolicyFileConstitution && a.Action == PolicyFileAdopted {
			s.carryMandateDefaults(ctx, legacyPolicyPath(set.Constitution), set.Constitution)
		}
	}
}

// policyDirNotes are the standing notes for `canary policy show --explain`:
// a file kept beside a different one in the old directory says so for as
// long as that old file is there.
func (s *Server) policyDirNotes(ctx context.Context) map[string]string {
	out := map[string]string{}
	if s == nil || s.coreStore == nil {
		return out
	}
	settled, _, err := s.loadPolicyDirSettlements(ctx)
	if err != nil {
		return out
	}
	for path, settlement := range settled {
		if settlement.Outcome != policyDirKeptBoth {
			continue
		}
		if _, err := os.Stat(settlement.Legacy); err == nil {
			out[path] = fmt.Sprintf(policyDirKeptBothNote, settlement.Legacy)
		}
	}
	return out
}
