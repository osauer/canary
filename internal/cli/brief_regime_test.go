package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestBriefGammaPreservesInterpretationAndProvenance(t *testing.T) {
	var out bytes.Buffer
	res := rpc.BriefResult{Ready: rpc.BriefReadySection{Gamma: rpc.BriefGammaRow{
		BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "context only"},
		Underlying:    "SPX", Regime: "short_gamma", Spot: new(100.0),
		Insight: &rpc.GammaInsight{Interpretation: "Amplification in either direction.", SkewInterpretation: "Richer downside pricing; no forecast.", Provenance: "frozen feed; observed 2026-09-04 20:00 UTC; context_only"},
	}}}
	renderBrief(&Env{Stdout: &out, Stderr: &bytes.Buffer{}}, res)
	for _, want := range []string{"SPX", "short gamma", "Amplification in either direction.", "Richer downside pricing; no forecast.", "frozen feed", "2026-09-04 20:00 UTC", "context_only"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in brief", want)
		}
	}
}

// Overview rows are list items: a row that wraps hangs its continuation
// under its own text, so it never reads as the next row.
func TestBriefOverviewRowsHangTheirContinuation(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	var out bytes.Buffer
	res := rpc.BriefResult{Narrative: &rpc.BriefNarrative{Overview: &rpc.BriefOverview{
		Assessment: []rpc.BriefRun{{Text: "Assessment available."}},
		Context:    []rpc.BriefParagraph{{Runs: []rpc.BriefRun{{Text: "Breadth · 46.3% above 50-day average · 61.8% above 200-day average · observed 29 Sep 22:00 CEST"}}}},
	}}}
	renderBrief(&Env{Stdout: &out, Stderr: &bytes.Buffer{}}, res)
	want := "  Breadth · 46.3% above 50-day average · 61.8% above 200-day average ·\n    observed 29 Sep 22:00 CEST\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("overview row does not hang its continuation:\n%s", out.String())
	}
}
