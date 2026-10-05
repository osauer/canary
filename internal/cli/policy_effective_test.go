package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// syntheticPolicyResult is a policy.snapshot answer whose effective view is
// built from synthetic files by the daemon's own builder.
func syntheticPolicyResult(t *testing.T) rpc.RiskPolicyResult {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	protection := write("protection-policy.toml", `kind = "canary.protection_policy"
schema_version = 1
policy_id = "protection-test"
policy_version = 2

[authority]
close_reduce_only = true
auto_submit = false

[buckets.cash_sweep]
enabled = true
max_order_notional = 12000.0

[buckets.cash_sweep.currency.EUR]
instruments = ["de_bubill"]
isins = ["DE0000000017", "DE0000000025", "DE0000000033", "DE0000000041", "DE0000000058"]
`)
	cfg := write("config.toml", "[trading]\nmode = \"paper\"\nmax_notional = 8000.0\n")
	files := []rpc.PolicyFileStatus{
		{Policy: "protection", Path: protection, Status: "active", PolicyID: "protection-test", PolicyVersion: "2"},
		{Policy: "rulebook", Path: filepath.Join(dir, "rulebook-policy.toml"), Status: "default", Review: rpc.PolicyReviewUnreviewed},
		{Policy: "opportunity", Path: filepath.Join(dir, "opportunity-policy.toml"), Status: "default"},
	}
	settings := &rpc.PlatformSettings{}
	settings.Trading.Mode = rpc.SettingsString{Value: "paper", Source: "config", Access: "read"}
	settings.Trading.Limits.MaxNotional = rpc.SettingsFloat{Value: 5000, Source: rpc.PolicySourceRuntime, Access: "write"}
	limits := risk.ConstitutionLimits(nil)
	return rpc.RiskPolicyResult{Status: "absent", Limits: limits, Files: files,
		Effective: daemon.PolicyEffectiveFromFiles(cfg, files, limits, settings)}
}

func runPolicyShowFor(t *testing.T, res rpc.RiskPolicyResult, args ...string) string {
	t.Helper()
	t.Setenv("COLUMNS", "80")
	var out, stderr bytes.Buffer
	env := &Env{Conn: &riskReadConn{result: res}, Stdout: &out, Stderr: &stderr}
	if code := Run(t.Context(), env, "policy", append([]string{"show"}, args...)); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	return out.String()
}

// --explain prints every row of the view, values aligned in one column per
// section, nothing wider than the terminal, and each key's meaning.
func TestPolicyShowExplainPrintsEveryKeyAligned(t *testing.T) {
	res := syntheticPolicyResult(t)
	screen := runPolicyShowFor(t, res, "--explain")
	all := strings.Split(screen, "\n")
	// The view starts at its first section; the status header above it is
	// the compact print's.
	first := res.Effective.Sections[0].Title
	start := slices.IndexFunc(all, func(l string) bool { return strings.HasPrefix(l, first+"  ") || l == first })
	if start < 0 {
		t.Fatalf("no effective view in:\n%s", screen)
	}
	view := all[start:]
	tempDir := filepath.Dir(res.Files[0].Path)
	for _, l := range view {
		// A file path cannot wrap; everything else must fit.
		if n := utf8.RuneCountInString(l); n > 80 && !strings.Contains(l, tempDir) {
			t.Errorf("line of %d columns breaks the terminal width: %q", n, l)
		}
	}
	for i, sec := range res.Effective.Sections {
		begin := slices.IndexFunc(view, func(l string) bool { return strings.HasPrefix(l, sec.Title+"  ") || l == sec.Title })
		if begin < 0 {
			t.Errorf("section %s missing", sec.Title)
			continue
		}
		end := len(view)
		if i+1 < len(res.Effective.Sections) {
			next := res.Effective.Sections[i+1].Title
			end = begin + slices.IndexFunc(view[begin:], func(l string) bool { return strings.HasPrefix(l, next+"  ") || l == next })
		}
		lines := view[begin:end]
		valueColumn := -1
		for _, g := range sec.Groups {
			for _, r := range g.Rows {
				key := policyRowKey(g, r)
				line := ""
				for _, l := range lines {
					if strings.HasPrefix(l, policyKeyIndent+key+" ") {
						line = l
						break
					}
				}
				if line == "" {
					t.Errorf("%s: key %s (%s) not printed", sec.ID, key, r.Key)
					continue
				}
				if r.Meaning != "" && !strings.Contains(strings.Join(strings.Fields(screen), " "), strings.Join(strings.Fields(r.Meaning), " ")) {
					t.Errorf("%s: meaning of %s not printed", sec.ID, r.Key)
				}
				if len(g.Columns) > 0 || utf8.RuneCountInString(key) >= policyKeyMaxWidth {
					continue
				}
				rest := strings.TrimLeft(line[len(policyKeyIndent+key):], " ")
				column := utf8.RuneCountInString(line) - utf8.RuneCountInString(rest)
				if valueColumn == -1 {
					valueColumn = column
				} else if column != valueColumn {
					t.Errorf("%s: value of %s starts at column %d, want %d:\n%s", sec.ID, key, column, valueColumn, line)
				}
			}
		}
	}
	for _, want := range []string{
		"Risk constitution",
		"    pre_authorised                       none         needs your number",
		"    max_order_notional                   12,000       file",
		"    settlement_days                      Canary's maintained route  machine",
		"    max_notional               5000   runtime override (config.toml 8000)",
		"                                  calm  early warning  confirmed",
		"no table in the file: Canary's compiled declaration for this currency",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("missing %q in:\n%s", want, screen)
		}
	}
}

// The default print stays compact and points to the full print.
func TestPolicyShowDefaultPointsToExplain(t *testing.T) {
	screen := runPolicyShowFor(t, syntheticPolicyResult(t))
	if strings.Contains(screen, "Protection policy") {
		t.Fatalf("compact print carries the full view:\n%s", screen)
	}
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if last := lines[len(lines)-1]; last != "Everything in force, with meanings: canary policy show --explain" {
		t.Fatalf("last line = %q", last)
	}
}

// A section name prints only that part, in text and JSON.
func TestPolicyShowSectionFilters(t *testing.T) {
	res := syntheticPolicyResult(t)
	screen := runPolicyShowFor(t, res, "cash_sweep")
	if !strings.Contains(screen, "[buckets.cash_sweep.currency.EUR]") || strings.Contains(screen, "Rulebook") || strings.Contains(screen, "Capital:") {
		t.Fatalf("cash_sweep filter:\n%s", screen)
	}
	var view rpc.PolicyEffectiveView
	if err := json.Unmarshal([]byte(runPolicyShowFor(t, res, "trading", "--json")), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Sections) == 0 || view.Sections[0].ID != rpc.PolicySectionTrading {
		t.Fatalf("trading filter JSON = %+v", view)
	}
	var stderr bytes.Buffer
	env := &Env{Conn: &riskReadConn{result: res}, Stdout: &bytes.Buffer{}, Stderr: &stderr}
	if Run(t.Context(), env, "policy", []string{"show", "nope"}) == 0 || !strings.Contains(stderr.String(), "cash_sweep") {
		t.Fatalf("unknown section accepted: %s", stderr.String())
	}
}
