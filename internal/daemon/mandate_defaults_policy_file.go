package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
)

// The owner's decision of 2026-10-29 07:06 CET: an install with an active
// [mandate] gains the volatility fallback and the index hedge's contract cap
// at daemon start, so an option without its own volatility no longer blinds
// the verdict and the Reduce hedge can be placed when the trader asks for
// it. The values are the proposals of 2026-10-28 (bands from one week of
// recorded chains; 25 contracts, the owner's number of 20:07 CET). Canary
// writes each table once, and a table the owner deletes after it stays
// deleted: the hedge table's presence is the permission to place a hedge.
// The once is daemon state (stateKindMandateDefaults), recorded at the first
// start that sees the file with an active [mandate], whether it wrote a table
// or found both written by the owner. The closing note a write leaves in the
// file is kept for the reader and still honoured, but it cannot be the
// record alone: a file whose tables the owner wrote never gets one, and a
// comment is the owner's to delete.
const (
	mandateDefaultsSource = "owner decision 2026-10-29 07:06 CET"
	mandateDefaultsNote   = "mandate defaults of 2026-10-29 written once"

	stateKindMandateDefaults = "mandate_defaults"
)

var mandateDefaultTables = []struct {
	table string
	keys  [][2]string
}{
	{"mandate.iv_fallback", [][2]string{{"short_band_pct", "65.0"}, {"month_band_pct", "15.0"}, {"long_band_pct", "8.0"}, {"measured_pct", "5.0"}, {"range_pct", "20.0"}}},
	{"mandate.index_hedge", [][2]string{{"max_contracts", "25"}}},
}

// migrateConstitutionMandateDefaults writes each default table the file
// lacks, uncommenting a commented-out proposal in place when nothing active
// sits under it, and raises policy_version when raise is set. A table the
// owner wrote is never touched, nor one the owner commented out with other
// values than the proposal's. write is false unless the daemon's record says
// this file has not had its defaults.
func migrateConstitutionMandateDefaults(doc *tomlDoc, c risk.Constitution, data string, raise, write bool) []string {
	if !write || c.Mandate == nil || doc.headerLine("mandate") < 0 || strings.Contains(data, mandateDefaultsNote) {
		return nil
	}
	present := map[string]bool{"mandate.iv_fallback": c.Mandate.IVFallback != nil, "mandate.index_hedge": c.Mandate.IndexHedge != nil}
	var changes []string
	for _, t := range mandateDefaultTables {
		if present[t.table] || doc.headerLine(t.table) >= 0 || doc.commentedTableDiffers(t.table, t.keys) {
			continue
		}
		if !doc.uncommentTableHeader(t.table) {
			doc.addTable(t.table)
		}
		var lines []string
		for _, kv := range t.keys {
			doc.dropCommentedKey(t.table, kv[0])
			lines = append(lines, kv[0]+" = "+kv[1]+"  # "+mandateDefaultsSource)
			changes = append(changes, fmt.Sprintf("write [%s].%s = %s (%s)", t.table, kv[0], kv[1], mandateDefaultsSource))
		}
		doc.insert(t.table, lines)
	}
	if len(changes) > 0 && raise {
		next := c.PolicyVersion + 1
		doc.set("", "policy_version", strconv.Itoa(next), nil)
		changes = append(changes, fmt.Sprintf("raise policy_version %d -> %d so the daemon adopts the mandate defaults", c.PolicyVersion, next))
	}
	return changes
}

// uncommentTableHeader turns a "# [table]" line into the header when no
// active key sits between it and the next header, so uncommenting adopts no
// key of another table. It reports whether it did.
func (d *tomlDoc) uncommentTableHeader(table string) bool {
	for i, line := range d.lines {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#")) != "["+table+"]" || !strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, next := range d.lines[i+1:] {
			if tomlHeaderRe.MatchString(next) {
				break
			}
			if tomlKeyRe.MatchString(next) {
				return false
			}
		}
		d.lines[i] = "[" + table + "]"
		return true
	}
	return false
}

// commentedTableDiffers reports whether a "# [table]" block carries a
// commented key the proposal lacks or a value other than the proposal's:
// the owner's own table, switched off, which Canary leaves as it is. The
// block ends at the next header, written or commented.
func (d *tomlDoc) commentedTableDiffers(table string, proposal [][2]string) bool {
	commented := func(line string) (string, bool) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "#")
		return strings.TrimSpace(rest), ok
	}
	for i, line := range d.lines {
		if text, ok := commented(line); !ok || text != "["+table+"]" {
			continue
		}
		for _, next := range d.lines[i+1:] {
			text, ok := commented(next)
			if tomlHeaderRe.MatchString(next) || (ok && tomlHeaderRe.MatchString(text)) {
				break
			}
			m := tomlKeyRe.FindStringSubmatch(text)
			if !ok || m == nil {
				continue
			}
			value, _, _ := strings.Cut(strings.TrimSpace(text[len(m[0]):]), "#")
			j := slices.IndexFunc(proposal, func(kv [2]string) bool { return kv[0] == m[1] })
			if j < 0 || !sameTOMLNumber(strings.TrimSpace(value), proposal[j][1]) {
				return true
			}
		}
		return false
	}
	return false
}

func sameTOMLNumber(a, b string) bool {
	x, errA := strconv.ParseFloat(a, 64)
	y, errB := strconv.ParseFloat(b, 64)
	return errA == nil && errB == nil && x == y
}

// dropCommentedKey removes a "# key = value" line in table's block, the
// commented proposal the written key replaces.
func (d *tomlDoc) dropCommentedKey(table, key string) {
	header := d.headerLine(table)
	if header < 0 {
		return
	}
	for i := header + 1; i < len(d.lines) && !tomlHeaderRe.MatchString(d.lines[i]); i++ {
		text := strings.TrimSpace(d.lines[i])
		if rest, ok := strings.CutPrefix(text, "#"); ok {
			if m := tomlKeyRe.FindStringSubmatch(strings.TrimSpace(rest)); m != nil && m[1] == key {
				d.lines = append(d.lines[:i], d.lines[i+1:]...)
				return
			}
		}
	}
}

// mandateDefaultsPreserved reports whether a default table the file wrote
// before keeps every value after.
func mandateDefaultsPreserved(before, after *risk.ConstitutionMandate) bool {
	if before == nil {
		return true
	}
	if after == nil {
		return false
	}
	return (before.IVFallback == nil || reflect.DeepEqual(before.IVFallback, after.IVFallback)) &&
		(before.IndexHedge == nil || reflect.DeepEqual(before.IndexHedge, after.IndexHedge))
}

// mandateDefaultsDone reads the daemon's record that the constitution at
// path already had its mandate defaults. An unreadable record counts as
// done: writing the hedge table hands out a permission, so a doubt keeps it
// unwritten. A server without the SQLite authority (a test server) has no
// record and relies on the file's note.
func (s *Server) mandateDefaultsDone(path string) bool {
	if s.coreStore == nil || path == "" {
		return false
	}
	records, _, err := loadMandateDefaults(context.Background(), s.coreStore)
	if err != nil {
		s.warnf("mandate defaults record unreadable, nothing written: %v", err)
		return true
	}
	return !records[path].IsZero()
}

// recordMandateDefaults records the constitution at path once daemon start
// has seen it with an active [mandate] and its ensure step did not fail:
// from then on the defaults were either written or found written.
func (s *Server) recordMandateDefaults(path string, actions []PolicyFileAction) {
	if s.coreStore == nil || path == "" {
		return
	}
	for _, a := range actions {
		if a.Policy == PolicyFileConstitution && (a.Action == PolicyFileFailed || a.Action == PolicyFileUnreadable) {
			return
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var c risk.Constitution
	if _, err := toml.Decode(string(data), &c); err != nil || c.Mandate == nil || parseTOMLDoc(data).headerLine("mandate") < 0 {
		return
	}
	ctx := context.Background()
	records, revision, err := loadMandateDefaults(ctx, s.coreStore)
	if err != nil {
		s.warnf("record mandate defaults: %v", err)
		return
	}
	records[path] = time.Now().UTC()
	raw, err := json.Marshal(records)
	if err == nil {
		_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindMandateDefaults, ExpectedRevision: revision, JSON: raw})
	}
	if err != nil {
		s.warnf("record mandate defaults: %v", err)
	}
}

func loadMandateDefaults(ctx context.Context, core *corestore.Store) (map[string]time.Time, int64, error) {
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindMandateDefaults)
	if err != nil || !ok {
		return map[string]time.Time{}, 0, err
	}
	records := map[string]time.Time{}
	if err := json.Unmarshal(doc.JSON, &records); err != nil {
		return nil, 0, fmt.Errorf("decode mandate defaults record: %w", err)
	}
	return records, doc.Revision, nil
}
