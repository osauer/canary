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
	renderRulesPolicy(&Env{Stdout: &out, Stderr: &bytes.Buffer{}}, &rpc.RulebookPolicyStatus{Status: rpc.RulebookPolicyStatusActive, Source: "file"}, p)
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
}
