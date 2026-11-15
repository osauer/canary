package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The shadow record (internal-docs/design/strategy-register.md, section 3),
// witnessed on synthetic books.

// coreShadowServer is the core test server with the core in shadow, as the
// owner writes it before the core goes live.
func coreShadowServer(t *testing.T, pos *rpc.PositionsResult) *Server {
	t.Helper()
	c := testCoreConstitution()
	c.Mandate.Core.Stage = risk.StageShadow
	srv := coreTestServer(t, c, pos, 0.25)
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict == nil {
		t.Fatal("no mandate verdict")
	}
	return srv
}

func coreShadowRecords(t *testing.T, srv *Server) []rpc.ShadowRecord {
	t.Helper()
	records, err := srv.shadowRecords(t.Context(), risk.CoreStrategyID)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// The month's decision, the intent for the next session on the paper
// position and its fill at that session's close are written once each,
// however often the refresh runs; the paper position starts at zero and
// moves only by the fill; the account's holding and the kept verdict are
// untouched, and the core's own verdict still sends nothing.
func TestCoreShadowRecordsDecisionIntentAndFillOnce(t *testing.T) {
	srv := coreShadowServer(t, mandateTestPositions(100))
	keptRes, keptIn, keptAt, keptFP := srv.mandateVerdicts.current()
	for range 3 {
		srv.refreshCoreTarget(t.Context())
	}
	records := coreShadowRecords(t, srv)
	if len(records) != 3 || records[0].Kind != rpc.ShadowKindDecision || records[1].Kind != rpc.ShadowKindIntent || records[2].Kind != rpc.ShadowKindFill {
		t.Fatalf("records %+v, want decision, intent and fill once each", records)
	}
	d, in, fill := records[0], records[1].Intent, records[2].Fill
	if d.Session != "2026-09-30" || d.Decision == nil || !d.Decision.TrendOn || d.Decision.TargetShares <= 0 || d.Rule != core.RuleID || d.SpecHash != core.SpecHash || !d.SpecCited || d.Stage != "shadow" {
		t.Fatalf("decision %+v %+v", d, d.Decision)
	}
	if in == nil || in.ForSession != "2026-10-01" || in.Kind != core.ActionBuy || in.Quantity <= 0 || in.HoldingBefore != 0 || !strings.Contains(in.Why, "first buy") {
		t.Fatalf("intent %+v", in)
	}
	series := srv.mandateHistorySeries(t.Context(), coreSeriesKey(coreTestConID))
	close := series.Closes[slices.Index(series.Dates, "2026-10-01")]
	if fill == nil || records[2].Session != "2026-10-01" || fill.Quantity != in.Quantity || fill.Price != close || fill.HoldingAfter != in.Quantity || fill.Unobserved != "" {
		t.Fatalf("fill %+v, want %d at the close %g", fill, in.Quantity, close)
	}
	if res, inp, at, fp := srv.mandateVerdicts.current(); res != keptRes || inp != keptIn || at != keptAt || fp != keptFP {
		t.Fatal("the shadow's hypothetical book reached the kept verdict")
	}
	res := srv.evaluateCoreTarget(t.Context(), srv.nowUTC())
	if res.Shadow == nil || res.Shadow.Holding != in.Quantity || res.Shadow.Records != 3 || res.Shadow.Recent[0].Kind != rpc.ShadowKindFill || res.Shadow.Stage != "shadow" {
		t.Fatalf("shadow view %+v", res.Shadow)
	}
	if res.Holding.Shares != 0 || res.Action.Kind != core.ActionWait || !strings.Contains(res.Action.Why, "runs in shadow") {
		t.Fatalf("the core's own verdict: holding %+v action %+v", res.Holding, res.Action)
	}
}

// The account's units of the core's fund are replaced by the paper position
// in the hypothetical book, so a real holding neither counts twice nor makes
// the shadow adopt it; a session past the series without its bar records the
// fill unobserved and leaves the paper position where it was.
func TestCoreShadowReplacesTheRealHoldingAndNeverGuessesAClose(t *testing.T) {
	srv := coreShadowServer(t, withIBCF(mandateTestPositions(100), 500, 96))
	series := srv.mandateHistorySeries(t.Context(), coreSeriesKey(coreTestConID))
	i := slices.Index(series.Dates, "2026-10-01")
	cut := *series
	cut.Dates = slices.Delete(slices.Clone(series.Dates), i, i+1)
	cut.Closes = slices.Delete(slices.Clone(series.Closes), i, i+1)
	if err := srv.saveMandateSeries(t.Context(), &cut); err != nil {
		t.Fatal(err)
	}
	srv.refreshCoreTarget(t.Context())
	records := coreShadowRecords(t, srv)
	if len(records) != 3 || records[1].Intent == nil || records[1].Intent.Kind != core.ActionBuy || records[1].Intent.HoldingBefore != 0 {
		t.Fatalf("records %+v, want a first buy from a zero paper position", records)
	}
	if f := records[2].Fill; f == nil || f.Quantity != 0 || f.HoldingAfter != 0 || !strings.Contains(f.Unobserved, "no close of 2026-10-01") {
		t.Fatalf("fill %+v, want it unobserved", f)
	}
	if got := shadowHolding(records); got != 0 {
		t.Fatalf("paper position %d after an unobserved fill", got)
	}
}

// Only a strategy in shadow writes a record: a live core (its rule sends)
// and a suspended one write none, and neither does a core whose decision
// holds; a hold is recorded once for the month.
func TestCoreShadowRecordsOnlyInShadow(t *testing.T) {
	live := coreTestServer(t, testCoreConstitution(), mandateTestPositions(100), 0.25)
	live.evaluateMandate(t.Context(), false)
	live.refreshCoreTarget(t.Context())
	if got := coreShadowRecords(t, live); len(got) != 0 {
		t.Fatalf("a live core wrote shadow records: %+v", got)
	}
	c := testCoreConstitution()
	c.Mandate.Core.Stage = risk.StageSuspended
	suspended := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	suspended.evaluateMandate(t.Context(), false)
	suspended.refreshCoreTarget(t.Context())
	if got := coreShadowRecords(t, suspended); len(got) != 0 {
		t.Fatalf("a suspended core wrote shadow records: %+v", got)
	}
	// The core's first form, without a stage, runs in shadow too.
	first := testCoreConstitution()
	first.Mandate.Core.Stage, first.Mandate.Core.SpecHash = "", ""
	srv := coreTestServer(t, first, mandateTestPositions(100), 0.25)
	srv.evaluateMandate(t.Context(), false)
	srv.refreshCoreTarget(t.Context())
	if got := coreShadowRecords(t, srv); len(got) != 3 || got[0].SpecCited {
		t.Fatalf("the first form's records %+v, want three with the compiled spec, not cited", got)
	}
}

// No order path reads the shadow record: its scope, event type and reader
// appear only in the recorder and the core's verdict.
func TestShadowRecordNeverReachesAnOrderPath(t *testing.T) {
	allowed := map[string]bool{"shadow_record.go": true, "strategy_target.go": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"shadowRecordType", "shadowScope(", "shadowRecords(", "shadowView(", "recordStrategyShadow(", "strategyKill(", "shadow.record.v1", `"shadow/`} {
			if strings.Contains(string(raw), name) {
				t.Errorf("%s names %s: only the recorder and the strategy verdicts read the shadow record", f, name)
			}
		}
	}
}
