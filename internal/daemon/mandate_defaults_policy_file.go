package daemon

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
)

// The owner's decision of 2026-10-29 07:06 CET: an install with an active
// [mandate] gains the volatility fallback and the index hedge's contract cap
// at daemon start, so an option without its own volatility no longer blinds
// the verdict and the Reduce hedge can be placed when the trader asks for
// it. The values are the proposals of 2026-10-28 (bands from one week of
// recorded chains; 25 contracts, the owner's number of 20:07 CET). Canary
// writes each table once: the closing note records the write, and a table
// the owner deletes after it stays deleted.
const (
	mandateDefaultsSource = "owner decision 2026-10-29 07:06 CET"
	mandateDefaultsNote   = "mandate defaults of 2026-10-29 written once"
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
// owner wrote is never touched.
func migrateConstitutionMandateDefaults(doc *tomlDoc, c risk.Constitution, data string, raise bool) []string {
	if c.Mandate == nil || doc.headerLine("mandate") < 0 || strings.Contains(data, mandateDefaultsNote) {
		return nil
	}
	present := map[string]bool{"mandate.iv_fallback": c.Mandate.IVFallback != nil, "mandate.index_hedge": c.Mandate.IndexHedge != nil}
	var changes []string
	for _, t := range mandateDefaultTables {
		if present[t.table] || doc.headerLine(t.table) >= 0 {
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
