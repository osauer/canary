package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// coSignRedSnapshot is a regime read in which only the HYG proxy is red: HYG
// below its 50-day average with SPY at its high, every other row calm.
func coSignRedSnapshot() *rpc.RegimeSnapshotResult {
	r := &rpc.RegimeSnapshotResult{AsOf: time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)}
	r.VIXTermStructure = rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK, Ratio: new(0.85)}
	r.VolOfVol = rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, Last: new(85.0)}
	r.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{
		Status: rpc.RegimeStatusOK, HYGPrice: new(78.0), HYG50DMA: new(80.0), SPYPrice: new(100.0), SPY52WHigh: new(100.0),
	}
	r.CreditSpreads = rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK, HYOAS: new(3.0)}
	r.FundingStress = rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK, SpreadBps: new(10.0)}
	r.USDJPY = rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, WeeklyChange: new(0.0)}
	r.GammaZero = rpc.RegimeGammaZero{Status: rpc.RegimeStatusUnavailable}
	r.Breadth = rpc.RegimeBreadth{Status: rpc.RegimeStatusOK, Envelope: rpc.BreadthSPXResult{State: rpc.BreadthStateReady, PctAbove50DMA: 60}}
	return r
}

// The regime summary follows the vote: a co-sign red with no stress witness
// beside it is named as context, not as stressed and not as a dominant risk.
// Beside a standing witness it counts and is named with it.
func TestRegimeSummaryCallsACoSignRedContext(t *testing.T) {
	r := coSignRedSnapshot()
	annotateRegimeMetadata(r, nil)
	summary := buildRegimeSummary(r)
	if strings.Contains(summary.PunchLine, "stressed") ||
		!strings.HasSuffix(summary.PunchLine, "Context, not a stress reading on its own: ETF credit proxy.") ||
		len(summary.DominantRisks) != 0 {
		t.Fatalf("punch line %q, dominant risks %v; want the proxy named as context only", summary.PunchLine, summary.DominantRisks)
	}

	r = coSignRedSnapshot()
	r.VIXTermStructure.Ratio = new(1.06)
	annotateRegimeMetadata(r, nil)
	r.VIXTermStructure.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	summary = buildRegimeSummary(r)
	if !strings.HasPrefix(summary.PunchLine, "volatility term structure and ETF credit proxy are stressed") ||
		strings.Contains(summary.PunchLine, "Context") ||
		!slices.Equal(summary.DominantRisks, []string{"volatility term structure", "ETF credit proxy"}) {
		t.Fatalf("punch line %q, dominant risks %v; want both named as stressed beside the standing witness", summary.PunchLine, summary.DominantRisks)
	}
}

// A snapshot the previous binary retained carries the previous lifecycle
// fingerprint version. It still loads at startup, because the load checks the
// regime fingerprint over the stored fields, and the alert producer refuses it
// as evidence until the first publication under this binary replaces it.
func TestRetainedSnapshotFromThePreviousLifecycleVersion(t *testing.T) {
	at := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	retained := func(version string) rpc.RegimeSnapshotResult {
		snapshot := regimeSnapshotCacheFixture(at, "retained")
		snapshot.Lifecycle = rpc.LifecycleState{Stage: rpc.LifecycleEarlyWarning, Scope: "market", Severity: "watch", Readiness: "ready", Unconfirmed: []string{"credit"}}
		snapshot.Lifecycle.Fingerprint = rpc.BuildLifecycleFingerprint(snapshot.Lifecycle)
		snapshot.Lifecycle.Fingerprint.Version = version
		snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
		return *snapshot
	}
	old := retained("lifecycle-fp-v2")
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRegimeSnapshotDocument(raw); err != nil {
		t.Fatalf("a snapshot stamped lifecycle-fp-v2 no longer loads: %v", err)
	}
	if valid, _, _, reason, _, _ := alertShadowRegimeEvidence(old, at.Add(time.Minute)); valid || reason != alertShadowReasonEvidenceFingerprintInvalid {
		t.Fatalf("lifecycle-fp-v2 snapshot as alert evidence: valid=%v reason=%q, want refused as %q", valid, reason, alertShadowReasonEvidenceFingerprintInvalid)
	}
	current := retained(rpc.BuildLifecycleFingerprint(rpc.LifecycleState{}).Version)
	if _, _, _, reason, _, _ := alertShadowRegimeEvidence(current, at.Add(time.Minute)); reason == alertShadowReasonEvidenceFingerprintInvalid {
		t.Fatal("a snapshot stamped with the current lifecycle version is refused for its fingerprint")
	}
}

// A widening of cash credit spreads opens a warning from a yellow row. The
// summary names it as widening, not as one more mixed reading.
func TestRegimeSummaryNamesTheCashWidening(t *testing.T) {
	r := coSignRedSnapshot()
	r.CreditSpreads.HYOAS, r.CreditSpreads.HY20DChange = new(3.6), new(0.62)
	annotateRegimeMetadata(r, nil)
	r.CreditSpreads.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}
	if openers := rpc.RegimeWarningOpeners(r); !slices.Contains(openers, rpc.RegimeWarningCashWidening) {
		t.Fatalf("the fixture's cash row does not open a warning: %v", openers)
	}
	summary := buildRegimeSummary(r)
	if !strings.HasPrefix(summary.PunchLine, "cash credit spreads is widening; ") || strings.Contains(summary.PunchLine, "mixed") {
		t.Fatalf("punch line %q; want the cash row named as widening", summary.PunchLine)
	}
}
