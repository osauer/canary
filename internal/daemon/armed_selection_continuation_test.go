package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func armedSelectionBook(shares float64) *rpc.PositionsResult {
	p := mandateReductionBook(mandateReductionStock("SYNA", 9001, shares, 100))
	value := shares * 100
	pct := value / 100000 * 100
	p.Stocks[0].MarketValue = value
	p.Stocks[0].MarketValueBase = new(value)
	p.Portfolio.DollarDeltaBase = new(value)
	p.Portfolio.NetLiquidationBase = new(100000.0)
	p.Portfolio.ExposureBase = []rpc.UnderlyingExposure{{Underlying: "SYNA", MarketValueBase: value, MarketValuePctNLV: new(pct), DollarDeltaBase: new(value)}}
	return p
}

func armedSelectionSources(p *rpc.PositionsResult) rpc.TradeProposalSourceFingerprints {
	a := rpc.BuildAccountFingerprint(&rpc.AccountResult{BaseCurrency: "EUR", AccountType: "CASH", NetLiquidation: 100000, GrossPositionValue: p.Stocks[0].MarketValue})
	b := rpc.BuildPositionsFingerprint(p, 100000)
	return rpc.TradeProposalSourceFingerprints{Account: &a, Positions: &b}
}

// Use the production row revision, not the older rig's list revision, so a
// settled fill inside the same exposure buckets exercises the real identity.
func armedSelectionInstall(t *testing.T, rig *automaticTestRig, p rpc.TradeProposal, src rpc.TradeProposalSourceFingerprints) rpc.TradeProposal {
	t.Helper()
	policy, status := rig.server.protectionPolicies.Active()
	previous := rig.engine.snapshotForRead(false, false)
	p.Revision = rig.engine.rowRevision(status.EffectiveFingerprint, src, rig.scope, p, previous)
	snap := rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: rig.now,
		Revision: p.Revision, EffectiveRevision: p.Revision, AccountID: rig.scope.Account, AccountMode: rig.scope.Mode,
		PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, EffectivePolicyFingerprint: status.EffectiveFingerprint,
		PolicyStatus: status, Proposals: []rpc.TradeProposal{p}}
	if err := rig.engine.installSnapshot(snap, false); err != nil {
		t.Fatal(err)
	}
	return p
}

// The generator, semantic fingerprints, durable records and automatic
// reconciler run together over a synthetic book, with no broker capability.
func TestArmedSelectionCappedFillQueuesRemainderOnce(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	pos := armedSelectionBook(300)
	in := mandateReductionFixture(t, pos, 1.2, 20000, 2)
	book := *in.Input
	book.Capital.Room = 0
	in = mandateReductionVerdict(book, 2)
	in.MaxOrderNotional = 10000
	rows, st := mandateReductionRun(t, in, pos)
	if len(rows) != 1 || rows[0].Quantity != 100 || rows[0].Mandate.Units != 300 {
		t.Fatalf("initial fixture: %+v %+v", rows, st)
	}
	first := armedSelectionInstall(t, rig, rows[0], armedSelectionSources(pos))
	rig.engine.reconcileAutomatic(t.Context())
	rec := rig.record(first.Key, first.Revision)
	if rec.State != rpc.TradeProposalAutomaticPending {
		t.Fatalf("first order not pending: %+v", rec)
	}
	rig.notice(rec)
	rig.advance(31 * time.Minute)
	if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r := records[automaticRecordKey(first.Key, first.Revision)]
		r.State, r.SubmittedAt, r.ResolvedAt = rpc.TradeProposalAutomaticSubmitted, rig.now, rig.now
		r.OrderRef = "synthetic-completed-first-installment"
		return []automaticSubmissionEvent{{At: rig.now, Type: automaticEventSubmitted, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode}}
	}); err != nil {
		t.Fatal(err)
	}
	// Repeated reads and a restart must not turn the same holding into a
	// second order. A refreshed plan's size alone is not evidence of a fill.
	rig.restart()
	refreshed := first
	refreshed.Mandate = rpc.CloneProposalMandate(first.Mandate)
	refreshed.Mandate.Units--
	refreshed.Mandate.VerdictAt = rig.now
	refreshed = armedSelectionInstall(t, rig, refreshed, armedSelectionSources(pos))
	for range 2 {
		rig.engine.reconcileAutomatic(t.Context())
	}
	if got := rig.record(refreshed.Key, refreshed.Revision); got.State != rpc.TradeProposalAutomaticSubmitted || len(rig.engine.automatic.list()) != 1 {
		t.Fatalf("unchanged holding retried the submitted order: %+v", got)
	}
	filled := armedSelectionBook(200)
	in.Previous = rows
	if premature, status := mandateReductionRun(t, in, filled); len(premature) != 0 || status.State != rpc.MandateReductionBookChanged {
		t.Fatalf("a moved holding was sized before a fresh verdict: %+v %+v", premature, status)
	}
	next := mandateReductionVerdict(mandateReductionRelegs(t, in, filled), 3)
	next.MaxOrderNotional, next.Previous, next.Episode = 10000, rows, st.Episode
	again, after := mandateReductionRun(t, next, filled)
	if len(again) != 1 || again[0].Quantity != 100 || again[0].Mandate.Units != 200 || after.Episode != st.Episode {
		t.Fatalf("remaining fixture: %+v %+v", again, after)
	}
	rig.advance(time.Minute)
	second := armedSelectionInstall(t, rig, again[0], armedSelectionSources(filled))
	for range 2 {
		rig.engine.reconcileAutomatic(t.Context())
	}
	got := rig.record(second.Key, second.Revision)
	if got.State != rpc.TradeProposalAutomaticPending || !got.CreatedAt.Equal(rig.now) || got.SubmitAt.Before(rig.now.Add(30*time.Minute)) || len(rig.engine.automatic.list()) != 2 {
		t.Fatalf("remaining cut needs exactly one new order with its full veto window: %+v; records=%d", got, len(rig.engine.automatic.list()))
	}
	if prior := rig.record(first.Key, first.Revision); prior.State != rpc.TradeProposalAutomaticSubmitted || prior.OrderRef != "synthetic-completed-first-installment" {
		t.Fatalf("first order's durable outcome was replaced: %+v", prior)
	}
}

func TestArmedSelectionChangedHoldingKeepsBreachVeto(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	pos := armedSelectionBook(300)
	first := rig.mandateProposal(pos.Stocks[0], 100, 1, 1)
	first = armedSelectionInstall(t, rig, first, armedSelectionSources(pos))
	rig.engine.reconcileAutomatic(t.Context())
	res, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: first.Key, Origin: rpc.OrderOriginPairedDevice, Reason: "hold this line"})
	if err != nil || !res.Accepted {
		t.Fatalf("veto: %+v, %v", res, err)
	}
	rig.restart()
	filled := armedSelectionBook(200)
	next := rig.mandateProposal(filled.Stocks[0], 100, 1, 1)
	next = armedSelectionInstall(t, rig, next, armedSelectionSources(filled))
	if next.Key != first.Key || next.Revision == first.Revision {
		t.Fatalf("changed holding did not exercise a new revision of the vetoed line: first=%s@%s next=%s@%s", first.Key, first.Revision, next.Key, next.Revision)
	}
	rig.engine.reconcileAutomatic(t.Context())
	for _, record := range rig.engine.automatic.list() {
		if record.State != rpc.TradeProposalAutomaticVetoed {
			t.Fatalf("holding change bypassed the breach veto: %+v", record)
		}
	}
	if got := rig.record(first.Key, first.Revision); got.State != rpc.TradeProposalAutomaticVetoed {
		t.Fatalf("durable veto lost: %+v", got)
	}
}
