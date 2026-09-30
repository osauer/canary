package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Every limit an owner can set must show on the screen they read: a key that
// moves a verdict but never appears here reads as fixed.
func TestRulesPolicyScreenShowsEveryRuleAndTheOverhedgeMultiple(t *testing.T) {
	p := risk.DefaultRulebookPolicy()
	p.OverhedgeMultiple = 1.5
	var out bytes.Buffer
	renderRulesPolicy(&Env{Stdout: &out, Stderr: &bytes.Buffer{}}, &rpc.RulebookPolicyStatus{Status: rpc.RulebookPolicyStatusActive, Source: "file"}, p, nil)
	screen := out.String()
	for _, id := range risk.RuleIDs() {
		line := ""
		for l := range strings.SplitSeq(screen, "\n") {
			if strings.Contains(l, " "+id+" ") {
				line = l
			}
		}
		if fields := strings.Fields(line); len(fields) < 4 {
			t.Fatalf("rule %s has no limits line:\n%s", id, screen)
		}
	}
	if !strings.Contains(screen, "act above 1.5× the band's top") {
		t.Fatalf("the over-hedge multiple is not on the screen:\n%s", screen)
	}
	// Rules 3 and 15 show their regime bands (amendment 17); the retired
	// cash reserve example is gone.
	for _, want := range []string{
		"option premium at risk of NLV, protection excluded, watch/act at: calm 25%/35%, early warning 20%/30%, confirmed 15%/25%; sell-only from watch",
		"net exposure of NLV, whole book with hedges, watch/act at: calm 100%/150%, early warning 100%/130%, confirmed 75%/100%; sell-only from watch",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("screen lacks %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "cash_reserve_min_pct") {
		t.Fatalf("the screen still names the retired cash reserve:\n%s", screen)
	}
}

// A terminal-evidence import that did not apply shows on the rules policy
// screen with its error and what stays in force; a clean one shows the
// revision the rules read.
func TestRulesPolicyScreenShowsTheTerminalEvidenceImport(t *testing.T) {
	var out bytes.Buffer
	failed := &rpc.TerminalEvidenceStatus{Status: rpc.TerminalEvidenceStatusImportError, ImportConfigured: true, ImportPath: "/tmp/terminal-evidence.json",
		AuthorityRevision: 3, Contracts: 1, ImportError: "configured earnings terminal evidence is invalid: unexpected EOF",
		Message: "the startup import was not applied; committed revision 3 (1 contract(s)) stays in force"}
	renderRulesPolicy(&Env{Stdout: &out, Stderr: &bytes.Buffer{}}, &rpc.RulebookPolicyStatus{Status: rpc.RulebookPolicyStatusActive, Source: "file"}, risk.DefaultRulebookPolicy(), failed)
	for _, want := range []string{"Terminal evidence: revision 3, 1 contract(s); import /tmp/terminal-evidence.json",
		"import failed:  configured earnings terminal evidence is invalid: unexpected EOF", "committed revision 3 (1 contract(s)) stays in force"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("screen lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	ok := &rpc.TerminalEvidenceStatus{Status: rpc.TerminalEvidenceStatusOK, AuthorityRevision: 1}
	renderRulesPolicy(&Env{Stdout: &out, Stderr: &bytes.Buffer{}}, &rpc.RulebookPolicyStatus{Status: rpc.RulebookPolicyStatusActive, Source: "file"}, risk.DefaultRulebookPolicy(), ok)
	if !strings.Contains(out.String(), "Terminal evidence: revision 1, 0 contract(s)\n") || strings.Contains(out.String(), "import failed") {
		t.Fatalf("clean terminal evidence screen:\n%s", out.String())
	}
}
