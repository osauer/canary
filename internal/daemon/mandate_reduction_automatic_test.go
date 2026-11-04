package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

const automaticMandateAuthority = `pre_authorised = ["mandate_reduction"]`

// mandateProposal is a mandate_reduction row on one stock line, built with
// the engine's own row constructor and flags.
func (r *automaticTestRig) mandateProposal(row rpc.PositionView, qty, rank, batch int) rpc.TradeProposal {
	r.t.Helper()
	policy, status := r.server.protectionPolicies.Active()
	effect := rpc.OrderPositionEffectReduce
	if float64(qty) == row.Quantity {
		effect = rpc.OrderPositionEffectClose
	}
	p := baseProposal(policy, status, rpc.TradeProposalSourceFingerprints{}, r.now, rpc.TradeProposalBucketMandateReduction, row, rpc.OrderActionSell, qty, effect,
		"mandate risk is over the budget; the largest risk contributor left")
	p.NeverSkipVeto = true
	p.Mandate = &rpc.TradeProposalMandate{Rank: rank, BatchRows: batch, Phase: rpc.MandatePhaseRisk, Units: qty,
		VerdictStatus: mandate.StatusOver, VerdictAt: r.now, VerdictFingerprint: "synthetic", Episode: "mandate-synthetic-1",
		Lot: 1, Session: mandateSessionDate(r.now)}
	return p
}

func automaticTestStockRowB() rpc.PositionView {
	row := automaticTestStockRow()
	row.ConID, row.Symbol = 102, "SYO"
	return row
}

// Not listed under [authority].pre_authorised, the batch waits for the
// owner's hand: no record, no attempt, however long it stands.
func TestMandateReductionWaitsForTheHandWhenNotPreAuthorised(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	revision := rig.install(prop)
	for range 3 {
		rig.cycle()
		rig.advance(20 * time.Minute)
	}
	rig.noRecord(prop.Key, revision)
	if rig.revalidations != 0 {
		t.Fatalf("the scheduler tried a row the owner did not pre-authorise: %d attempts", rig.revalidations)
	}
	served := rig.engine.Snapshot(false).Proposals[0]
	if a := served.Automatic; a == nil || a.PreAuthorised || a.Bucket != preAuthorisedBucketMandateReduction || a.State != "" {
		t.Fatalf("served automatic = %+v", a)
	}
	if !served.AutomaticEligible() || len(mandateHeldBlockers(served)) != 0 {
		t.Fatalf("an unheld row is not open to the owner's hand: %+v", served)
	}
}

// Pre-authorised, every row waits the full veto window even under a latched
// drawdown brake: the latch never skips it for a mandate reduction.
func TestMandateReductionLatchedBrakeKeepsTheVeto(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	rig.latched = true
	prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.Bucket != preAuthorisedBucketMandateReduction || rec.State != rpc.TradeProposalAutomaticPending || rec.LatchSkippedWindow || !rec.SubmitAt.Equal(rig.now.Add(30*time.Minute)) {
		t.Fatalf("record under the latch = %+v", rec)
	}
	rig.notice(rec)
	rig.advance(29 * time.Minute)
	rig.cycle()
	if rig.revalidations != 0 {
		t.Fatalf("attempted inside the window under the latch: %d", rig.revalidations)
	}
	notices := rig.engine.automaticPendingNotices(rig.scope)
	if len(notices) != 1 || notices[0].Latched || alertProtectionAutomaticPresentationCode(notices[0]) != rpc.AlertPresentationProtectionAutoMandateReduction {
		t.Fatalf("notices = %+v", notices)
	}
	rig.advance(2 * time.Minute)
	rig.cycle()
	if rig.revalidations != 1 {
		t.Fatalf("attempts after the window = %d, want 1", rig.revalidations)
	}
}

// A held row (no verdict confirms its sizing) is never attempted when its
// window closes: the record waits on its original window with the hold and
// its notice says so; once a fresh verdict confirms the row, it is attempted
// without a new window.
func TestMandateReductionHeldRowHoldsItsRecord(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	held := prop
	held.Mandate = rpc.CloneProposalMandate(prop.Mandate)
	held.Mandate.Held, held.Mandate.HoldReason = true, "the mandate risk cannot be measured: daily history for SYN is stale"
	revision := rig.install(held)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	rig.notice(rec)
	due := rec.SubmitAt
	rig.advance(31 * time.Minute)
	rig.cycle()
	rig.cycle()
	rec = rig.record(prop.Key, revision)
	if rig.revalidations != 0 || rec.State != rpc.TradeProposalAutomaticPending || rec.HeldAt.IsZero() || rec.HoldReason != automaticHoldMandateVerdict || !rec.SubmitAt.Equal(due) {
		t.Fatalf("held row: attempts %d record %+v", rig.revalidations, rec)
	}
	if n := rig.eventCount(prop.Key, revision, automaticEventHeld); n != 1 {
		t.Fatalf("held events = %d, want 1", n)
	}
	notices := rig.engine.automaticPendingNotices(rig.scope)
	if len(notices) != 1 || notices[0].Hold != automaticNoticeHoldMandate || alertProtectionAutomaticPresentationCode(notices[0]) != rpc.AlertPresentationProtectionAutoHeldMandate {
		t.Fatalf("held notice = %+v", notices)
	}
	served := rig.engine.Snapshot(false).Proposals[0]
	if served.Readiness == nil || served.Readiness.Code != rpc.ReadinessNotExecutable || !strings.Contains(served.Readiness.Message, "cannot be confirmed") {
		t.Fatalf("held readiness = %+v", served.Readiness)
	}
	if again := rig.install(prop); again != revision {
		t.Fatalf("confirming the row changed its revision: %s → %s", revision, again)
	}
	rig.cycle()
	if rig.revalidations != 1 {
		t.Fatalf("confirmed row attempts = %d, want 1", rig.revalidations)
	}
}

// Back within budget the batch leaves the set: a waiting record is
// superseded and never sent, a submitted one stays as it was, and nothing is
// cancelled at the broker.
func TestMandateReductionWithinSupersedesTheUnsentOnly(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	rig.server.orderCancelBroker = func(context.Context, int) error {
		t.Error("a mandate release cancelled an order at the broker")
		return nil
	}
	sent := rig.mandateProposal(automaticTestStockRow(), 40, 1, 2)
	unsent := rig.mandateProposal(automaticTestStockRowB(), 40, 2, 2)
	revision := rig.install(sent, unsent)
	rig.cycle()
	if err := rig.engine.automatic.update(context.Background(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r := records[automaticRecordKey(sent.Key, revision)]
		r.State, r.SubmittedAt, r.ResolvedAt, r.OrderRef = rpc.TradeProposalAutomaticSubmitted, rig.now, rig.now, "canary-synthetic-1"
		return []automaticSubmissionEvent{{At: rig.now, Type: automaticEventSubmitted, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State}}
	}); err != nil {
		t.Fatal(err)
	}
	rig.install()
	rig.cycle()
	if rec := rig.record(unsent.Key, revision); rec.State != rpc.TradeProposalAutomaticSuperseded || !strings.Contains(rec.Reason, "disappeared") {
		t.Fatalf("unsent record = %+v", rec)
	}
	if rec := rig.record(sent.Key, revision); rec.State != rpc.TradeProposalAutomaticSubmitted || rec.OrderRef != "canary-synthetic-1" {
		t.Fatalf("sent record = %+v", rec)
	}
	rig.advance(time.Hour)
	rig.cycle()
	if rig.revalidations != 0 {
		t.Fatalf("a withdrawn row was attempted: %d", rig.revalidations)
	}
}

// The batch goes out in rank order within one cycle, whatever the keys'
// order, and one row's refusal never holds the next.
func TestMandateReductionDispatchesInRankOrder(t *testing.T) {
	t.Parallel()
	for _, firstRanked := range []int{101, 102} {
		rig := newAutomaticTestRig(t, automaticMandateAuthority)
		var order []string
		revalidate := rig.engine.revalidateForTest
		rig.engine.revalidateForTest = func(ctx context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			order = append(order, key)
			return revalidate(ctx, key, revision)
		}
		rankOf := func(conID int) int {
			if conID == firstRanked {
				return 1
			}
			return 2
		}
		a := rig.mandateProposal(automaticTestStockRow(), 40, rankOf(101), 2)
		b := rig.mandateProposal(automaticTestStockRowB(), 40, rankOf(102), 2)
		revision := rig.install(a, b)
		rig.cycle()
		rig.notice(rig.record(a.Key, revision))
		rig.notice(rig.record(b.Key, revision))
		rig.advance(31 * time.Minute)
		rig.cycle()
		want := []string{a.Key, b.Key}
		if firstRanked == 102 {
			want = []string{b.Key, a.Key}
		}
		// The default build refuses both at the write gate; the second is
		// still attempted after the first's refusal.
		if !slices.Equal(order, want) {
			t.Fatalf("rank 1 = %d: dispatch order %v, want %v", firstRanked, order, want)
		}
		for _, key := range want {
			if rec := rig.record(key, revision); !rec.terminal() {
				t.Fatalf("record %s not resolved: %+v", key, rec)
			}
		}
	}
}

// One proposal per contract and side: where the budget governor also wants
// to sell the line, one row stands, sized by the larger need, never the sum.
// A held mandate row stands for nothing, so the governor's row stays live.
func TestMandateReductionOneRowPerContractWithTheGovernor(t *testing.T) {
	t.Parallel()
	leg := budgetOptionLeg("AAA", 601, "C", 6, 2000, -1000)
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15)
	row := func(bucket string, qty int) rpc.TradeProposal {
		p := baseProposal(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, optionExitTestTime(), bucket, leg, rpc.OrderActionSell, qty, rpc.OrderPositionEffectReduce, bucket+" reason")
		p.NeverSkipVeto = true
		if bucket == rpc.TradeProposalBucketMandateReduction {
			p.Mandate = &rpc.TradeProposalMandate{Rank: 1, BatchRows: 1, Phase: rpc.MandatePhaseRisk, Units: qty}
		}
		return p
	}
	for _, tc := range []struct{ mandateQty, budgetQty int }{{5, 3}, {2, 4}} {
		props := []rpc.TradeProposal{row(rpc.TradeProposalBucketBudgetReduction, tc.budgetQty), row(rpc.TradeProposalBucketMandateReduction, tc.mandateQty)}
		mergeSameContractProposals(props, nil)
		var live []rpc.TradeProposal
		for _, p := range props {
			if p.State == rpc.TradeProposalStateGenerated && len(p.Blockers) == 0 {
				live = append(live, p)
			}
		}
		want := max(tc.mandateQty, tc.budgetQty)
		if len(live) != 1 || live[0].Quantity != want || len(live[0].Covers) != 1 {
			t.Fatalf("mandate %d / budget %d: live %+v", tc.mandateQty, tc.budgetQty, live)
		}
		for _, p := range props {
			if p.Key != live[0].Key && (p.CoveredBy != live[0].Key || p.Blockers[0].Code != coveredByProposalCode) {
				t.Fatalf("covered row %+v", p)
			}
		}
	}
	held := row(rpc.TradeProposalBucketMandateReduction, 5)
	held.Mandate.Held = true
	props := []rpc.TradeProposal{row(rpc.TradeProposalBucketBudgetReduction, 3), held}
	mergeSameContractProposals(props, nil)
	for _, p := range props {
		if len(p.Blockers) != 0 || p.CoveredBy != "" || len(p.Covers) != 0 {
			t.Fatalf("a held mandate row took part in the merge: %+v", p)
		}
	}
	if sameContractRule(held) != "the mandate reduction" || !proposalIsReduction(held) {
		t.Fatalf("rule %q reduction %v", sameContractRule(held), proposalIsReduction(held))
	}
}

// The vocabulary: mandate_reduction is pre-authorisable, absent from the
// embedded default, and the scheduler names it for a mandate row only.
func TestMandateReductionIsAClosedPreAuthorisableBucket(t *testing.T) {
	t.Parallel()
	if !validPreAuthorisedBucket(preAuthorisedBucketMandateReduction) || validPreAuthorisedBucket("mandate_reductions") {
		t.Fatal("vocabulary")
	}
	if defaultProtectionPolicy().preAuthorised(preAuthorisedBucketMandateReduction) {
		t.Fatal("the embedded default pre-authorises the mandate reduction")
	}
	m := writePreAuthPolicy(t, preAuthPolicyTOML(automaticMandateAuthority, 1))
	read, err := m.loadPolicy()
	if err != nil || !read.policy.preAuthorised(preAuthorisedBucketMandateReduction) || read.policy.preAuthorised(preAuthorisedBucketBudgetReduction) {
		t.Fatalf("parsed %+v err %v", read.policy.Authority, err)
	}
	if got := automaticBucketFor(rpc.TradeProposal{Bucket: rpc.TradeProposalBucketMandateReduction}); got != preAuthorisedBucketMandateReduction {
		t.Fatalf("automatic bucket %q", got)
	}
	if _, err := writePreAuthPolicy(t, preAuthPolicyTOML(`pre_authorised = ["mandate"]`, 1)).loadPolicy(); err == nil || !strings.Contains(err.Error(), "mandate_reduction") {
		t.Fatalf("an unknown bucket was accepted: %v", err)
	}
}

// A mandate row that comes back within its breach keeps the veto clock its
// notice started, as a re-sized one does: after it dropped out of the set
// (a broker outage, a blocked refresh) and after a send refused because the
// row moved on (stale_revision), a fresh 30 minutes restarted the window at
// every such blip and the batch never went (2026-11-03 19:27-20:00 CET).
func TestMandateReductionRowBackWithinItsBreachKeepsTheClock(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	a := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev := rig.install(a)
	rig.cycle()
	rig.notice(rig.record(a.Key, rev))
	first := rig.record(a.Key, rev)

	rig.advance(10 * time.Minute)
	rig.install()
	rig.cycle()
	if gone := rig.record(a.Key, rev); gone.State != rpc.TradeProposalAutomaticSuperseded {
		t.Fatalf("the record of the row that dropped out: %s %q", gone.State, gone.Reason)
	}
	// The owner could not veto while the row was out of the set: the clock
	// pauses for the gap, so the usable veto time stays the whole window.
	rig.advance(5 * time.Minute)
	back := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev = rig.install(back)
	rig.cycle()
	rec := rig.record(back.Key, rev)
	if rec.State != rpc.TradeProposalAutomaticPending || !rec.SubmitAt.Equal(first.SubmitAt.Add(5*time.Minute)) || !rec.NoticedAt.Equal(first.NoticedAt) || !rec.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("the row back within its breach: %+v, want the clock of %+v paused 5 minutes", rec, first)
	}

	// Due, the send is refused because the row moved on; its window had run
	// with the notice open, so the row served again keeps the clock as it
	// stands and is attempted at once.
	rig.advance(20 * time.Minute)
	revalidate := rig.engine.revalidateForTest
	stale := true
	rig.engine.revalidateForTest = func(ctx context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		if stale {
			stale = false
			rig.count(&rig.revalidations)
			return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "stale"}}, nil
		}
		return revalidate(ctx, key, revision)
	}
	rig.cycle()
	if refused := rig.record(back.Key, rev); refused.State != rpc.TradeProposalAutomaticSuperseded {
		t.Fatalf("the record refused stale_revision: %s %q", refused.State, refused.Reason)
	}
	rig.cycle()
	if again := rig.record(back.Key, rev); !again.SubmitAt.Equal(rec.SubmitAt) || !again.NoticedAt.Equal(first.NoticedAt) {
		t.Fatalf("the row served again after stale_revision: %+v, want the clock of %+v", again, rec)
	}
}

// A row out of the set past its deadline is not sent the moment it returns:
// the owner, who could not veto it while it was out, keeps the rest of the
// window.
func TestMandateReductionRowBackAfterItsDeadlineWaitsTheRestOfTheWindow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	a := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev := rig.install(a)
	rig.cycle()
	rig.notice(rig.record(a.Key, rev))
	first := rig.record(a.Key, rev)
	rig.advance(10 * time.Minute)
	rig.install()
	rig.cycle()
	rig.advance(35 * time.Minute) // past the old deadline
	rev = rig.install(rig.mandateProposal(automaticTestStockRow(), 20, 1, 1))
	rig.cycle()
	rec := rig.record(a.Key, rev)
	if rig.revalidations != 0 || rec.State != rpc.TradeProposalAutomaticPending {
		t.Fatalf("a row back past its deadline was attempted at once: %d attempts, %+v", rig.revalidations, rec)
	}
	usable := first.SubmitAt.Sub(first.NoticedAt)
	if before, after := 10*time.Minute, rec.SubmitAt.Sub(rig.now); before+after != usable {
		t.Fatalf("usable veto time %s + %s, want the window %s", before, after, usable)
	}
}

// Proposals switched off by config is the owner's act: the rows that leave
// pass no clock on.
func TestMandateReductionProposalsDisabledPassesNoClockOn(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	a := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev := rig.install(a)
	rig.cycle()
	rig.installWith(func(snap *rpc.TradeProposalSnapshot) {
		snap.Blockers = []rpc.TradingBlocker{{Code: "proposals_disabled", Message: "manual protection proposals are disabled by config"}}
	})
	rig.cycle()
	if rec := rig.record(a.Key, rev); rec.State != rpc.TradeProposalAutomaticSuperseded || rec.CarryClock {
		t.Fatalf("the record of a row gone with proposals disabled: %s carry=%t", rec.State, rec.CarryClock)
	}
}

// A row the owner ignored leaves the set by the owner's act: its record
// passes no clock on.
func TestMandateReductionIgnoredRowPassesNoClockOn(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	a := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev := rig.install(a)
	rig.cycle()
	rig.engine.mu.Lock()
	rig.engine.ignored[scopedIgnoreKey(rig.scope, a.Key)] = struct{}{}
	rig.engine.mu.Unlock()
	rig.install()
	rig.cycle()
	if rec := rig.record(a.Key, rev); rec.State != rpc.TradeProposalAutomaticSuperseded || rec.CarryClock {
		t.Fatalf("the ignored row's record: %s carry=%t", rec.State, rec.CarryClock)
	}
}

// A record an older binary superseded as re-sized carries no CarryClock
// mark; the row served after a gap still gets the paused clock.
func TestMandateReductionLegacyResizedRecordPausesTheClock(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	a := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev := rig.install(a)
	rig.cycle()
	rig.notice(rig.record(a.Key, rev))
	first := rig.record(a.Key, rev)
	rig.advance(10 * time.Minute)
	resized := rig.mandateProposal(automaticTestStockRow(), 19, 1, 1)
	resized.State = rpc.TradeProposalStateBlocked
	resized.Blockers = []rpc.TradingBlocker{{Code: "quote_stale", Message: "synthetic temporary quote gap"}}
	rig.install(resized)
	rig.cycle()
	if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		var events []automaticSubmissionEvent
		for _, rec := range records {
			rec.CarryClock = false // as an older binary wrote it
			events = append(events, automaticSubmissionEvent{At: rig.now, Type: automaticEventSuperseded, Key: rec.Key, Revision: rec.Revision, Bucket: rec.Bucket, State: rec.State, AccountID: rec.AccountID, AccountMode: rec.AccountMode, Reason: rec.Reason})
		}
		return events
	}); err != nil {
		t.Fatal(err)
	}
	if old := rig.record(a.Key, rev); old.State != rpc.TradeProposalAutomaticSuperseded || old.Reason != automaticEpisodeResizedReason || old.CarryClock {
		t.Fatalf("the standing record: %s %q", old.State, old.Reason)
	}
	rig.advance(5 * time.Minute)
	back := rig.mandateProposal(automaticTestStockRow(), 19, 1, 1)
	rev2 := rig.install(back)
	rig.cycle()
	if rec := rig.record(back.Key, rev2); !rec.SubmitAt.Equal(first.SubmitAt.Add(5 * time.Minute)) {
		t.Fatalf("the row back after a legacy re-size: due %s, want %s", rec.SubmitAt, first.SubmitAt.Add(5*time.Minute))
	}
}

// The decided send order (nearest expiry first, owner 2026-11-03 11:21 CET)
// holds across veto clocks: a row whose own clock has run waits for a
// better-ranked row still inside its window, so a newer row cannot beat an
// older, nearer one (2026-11-04: a December line went an hour ahead of November
// lines). A better row that is refused or far off never jams the batch.
func TestMandateReductionLaterRankWaitsForAnEarlierRankOnItsClock(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setFirst func(rec *automaticSubmissionRecord, now time.Time)
		waits    bool
	}{
		"inside its window":      {func(rec *automaticSubmissionRecord, now time.Time) { rec.SubmitAt = now.Add(40 * time.Minute) }, true},
		"beyond one window":      {func(rec *automaticSubmissionRecord, now time.Time) { rec.SubmitAt = now.Add(90 * time.Minute) }, false},
		"refused, retrying soon": {func(rec *automaticSubmissionRecord, now time.Time) { rec.ResubmitAt = now.Add(40 * time.Minute) }, false},
		"held by a hand order":   {func(rec *automaticSubmissionRecord, now time.Time) { rec.HeldAt = now }, false},
	} {
		rig := newAutomaticTestRig(t, automaticMandateAuthority)
		var order []string
		revalidate := rig.engine.revalidateForTest
		rig.engine.revalidateForTest = func(ctx context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			order = append(order, key)
			return revalidate(ctx, key, revision)
		}
		first := rig.mandateProposal(automaticTestStockRow(), 40, 1, 2)
		second := rig.mandateProposal(automaticTestStockRowB(), 40, 2, 2)
		revision := rig.install(first, second)
		rig.cycle()
		rig.notice(rig.record(first.Key, revision))
		rig.notice(rig.record(second.Key, revision))
		start := rig.now
		if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
			rec := records[automaticRecordKey(first.Key, revision)]
			tc.setFirst(rec, start)
			return []automaticSubmissionEvent{{At: start, Type: automaticEventNoticed, Key: rec.Key, Revision: rec.Revision}}
		}); err != nil {
			t.Fatal(err)
		}
		rig.advance(31 * time.Minute)
		rig.cycle()
		if tc.waits {
			if len(order) != 0 {
				t.Fatalf("%s: the later rank went ahead: %v", name, order)
			}
			if rec := rig.record(second.Key, revision); rec.State != rpc.TradeProposalAutomaticPending {
				t.Fatalf("%s: the waiting row's record changed: %+v", name, rec)
			}
			rig.advance(10 * time.Minute)
			rig.cycle()
			if want := []string{first.Key, second.Key}; !slices.Equal(order, want) {
				t.Fatalf("%s: dispatch order %v, want %v", name, order, want)
			}
			continue
		}
		if !slices.Contains(order, second.Key) {
			t.Fatalf("%s: the later rank was jammed: %v", name, order)
		}
	}
}
