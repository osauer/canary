package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestBriefOverviewPreservesAttentionCoverageAndObservationDates(t *testing.T) {
	res := rpc.BriefResult{AsOf: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	res.Ready.Capital = rpc.BriefCapitalRow{BriefRowState: briefAttention("capital warning"), Tier: "warn", Enforcement: "shadow", PeakAsOf: res.AsOf}
	res.Review.Rules = rpc.BriefRulesRow{BriefRowState: briefAttention("rule breach"), Act: 1, Unknown: 1}
	res.Ready.MarketEvents = []rpc.BriefMarketEventRow{{BriefRowState: briefAttention("provider date pending"), Kind: "earnings", Count: 1}}
	res.Ready.Breadth = rpc.BriefBreadthRow{BriefRowState: briefOK("measured"), PctAbove50DMA: new(0.0), AsOf: res.AsOf.Add(-24 * time.Hour)}
	before := briefContentFingerprint(&res)
	res.Narrative = composeBriefNarrative(&res)
	if briefContentFingerprint(&res) != before {
		t.Fatal("presentation changed evidence identity")
	}
	o := res.Narrative.Overview
	encode := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	attention, coverage, context := encode(o.Attention), encode(o.Coverage), encode(o.Context)
	for _, want := range []string{"Capital", "warn tier", "shadow", "observation time unavailable", "Policy adherence"} {
		if !strings.Contains(attention, want) {
			t.Fatalf("missing %q: %s", want, attention)
		}
	}
	for _, want := range []string{"policy adherence", "last session close", "Edge", "unavailable"} {
		if !strings.Contains(coverage, want) {
			t.Fatalf("missing %q: %s", want, coverage)
		}
	}
	if strings.Contains(attention, "earnings") || !strings.Contains(context, "earnings") || !strings.Contains(context, "0.0%") || !strings.Contains(context, "5 Sep") {
		t.Fatalf("lost non-actionable event, measured zero or observation date: %s", context)
	}
	if strings.Contains(encode(res.Narrative.Coda), "Everything else holds") {
		t.Fatal("reassurance despite missing evidence")
	}
}

func TestBriefOverviewDisabledRulesAreNotMissingInputs(t *testing.T) {
	res := &rpc.BriefResult{Review: rpc.BriefReviewSection{Rules: rpc.BriefRulesRow{NotEvaluated: 2}}}
	res.Review.LastSession.BriefRowState = briefOK("captured")
	res.Review.Edge.BriefRowState = briefOK("available")
	topics := []briefTopic{{label: "policy adherence", state: briefOK("two rules off")}}
	o := composeBriefOverview(res, topics)
	if len(o.Coverage) != 0 {
		t.Fatal("disabled rules reported as unavailable", o.Coverage)
	}
	res.Review.Rules.Unknown = 1
	if len(composeBriefOverview(res, topics).Coverage) != 1 {
		t.Fatal("unknown rule was hidden")
	}
}
