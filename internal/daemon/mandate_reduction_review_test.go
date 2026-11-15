package daemon

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Regression tests for the safety review of the forced reduction
// (2026-10-14): coverage units, conditional working orders, the maximum
// loss, the high-water mark against a glitch, the merge under the latch and
// the veto clock within a breach episode.

func mandatePlanFor(st *rpc.TradeProposalMandateStatus, conID int) (rpc.TradeProposalMandatePlanOrder, bool) {
	for _, o := range st.Plan {
		if o.Contract.ConID == conID {
			return o, true
		}
	}
	return rpc.TradeProposalMandatePlanOrder{}, false
}

// assertMandateStepsLowerRisk applies the rows in dispatch order, each at
// this cycle's quantity, and requires the book's risk never to rise after
// any step: every order the daemon may send alone leaves the book safer.
func assertMandateStepsLowerRisk(t *testing.T, in mandateReductionInput, rows []rpc.TradeProposal) {
	t.Helper()
	ordered := slices.Clone(rows)
	slices.SortStableFunc(ordered, func(a, b rpc.TradeProposal) int { return a.Mandate.Rank - b.Mandate.Rank })
	before := mandateRiskAfter(t, in, nil)
	cuts := map[int]float64{}
	for _, row := range ordered {
		cuts[row.Contract.ConID] += float64(row.Quantity)
		after := mandateRiskAfter(t, in, cuts)
		if after > before+1e-6 {
			t.Fatalf("rank %d (%s %d of %d) raises the book's risk %.0f → %.0f", row.Mandate.Rank, row.Action, row.Quantity, row.Contract.ConID, before, after)
		}
		before = after
	}
}

// A covered position needs the owner's hand: the batch never buys a short
// call back and never sells the shares that cover it (an unwind takes a
// combination order Canary does not route, and a buyback alone raises the
// risk). It cuts other contributors instead and, where the target cannot be
// met without the covered shares, says so plainly and names them.
func TestMandateReductionLeavesACoveredPositionToTheOwner(t *testing.T) {
	t.Parallel()
	for _, ratio := range []float64{1.3, 2, 4} {
		call := mandateReductionCall("SYNA", 9010, -30, 90, 100, 12, "20261120")
		pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), call, mandateReductionStock("SYNB", 9002, 500, 80))
		in := mandateReductionFixture(t, pos, ratio, 1e7, 2)
		rows, st := mandateReductionRun(t, in, pos)
		assertMandateRowsReduceOnly(t, rows, pos)
		assertMandateStepsLowerRisk(t, in, rows)
		for _, conID := range []int{9001, 9010} {
			if row, ok := mandateRowFor(rows, conID); ok {
				t.Fatalf("ratio %g: the covered position was cut: %+v", ratio, row)
			}
			if plan, ok := mandatePlanFor(st, conID); ok {
				t.Fatalf("ratio %g: the covered position is in the plan: %+v", ratio, plan)
			}
		}
		if !slices.ContainsFunc(st.LeftOut, func(s string) bool { return strings.Contains(s, "a short option") }) ||
			!slices.ContainsFunc(st.LeftOut, func(s string) bool { return strings.HasPrefix(s, "SYNA (its shares cover short calls") }) {
			t.Fatalf("ratio %g: left out %v", ratio, st.LeftOut)
		}
		if !st.TargetReached && !strings.Contains(st.Reason, "a covered position needs your hand: SYNA: 3000 shares cover short calls") {
			t.Fatalf("ratio %g: the status does not say the covered position needs the owner: %q", ratio, st.Reason)
		}
		if ratio >= 2 && st.TargetReached {
			t.Fatalf("ratio %g: the target is met without the covered shares? %+v", ratio, st)
		}
	}
	// Shares above the cover are free: 3,500 shares under 10 short calls
	// leave 2,500 the batch may sell.
	call := mandateReductionCall("SYNA", 9010, -10, 90, 100, 12, "20261120")
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3500, 100), call)
	in := mandateReductionFixture(t, pos, 1.3, 1e7, 2)
	rows, st := mandateReductionRun(t, in, pos)
	sale, ok := mandateRowFor(rows, 9001)
	if !ok || sale.Mandate.Units > 2500 {
		t.Fatalf("free shares: %+v plan %+v", sale, st.Plan)
	}
	if !st.TargetReached && !strings.Contains(st.Reason, "a covered position needs your hand") {
		t.Fatalf("free shares: the status does not name the covered rest: %q", st.Reason)
	}
	assertMandateStepsLowerRisk(t, in, rows)
}

// A long put that protects its own stock, on a name no Rulebook lists as a
// hedge, is protection: the batch never sells it, and never cuts the shares
// below what the puts deliver at exercise, so exercising every put leaves
// the line at zero or long, never short. Every sendable step lowers the
// book's risk.
func TestMandateReductionProtectivePutKeepsItsDeliverable(t *testing.T) {
	t.Parallel()
	for _, puts := range []float64{10, 30} {
		for _, ratio := range []float64{1.3, 2, 4} {
			put := mandateReductionCall("SYNA", 9011, puts, 95, 100, 3, "20261120")
			put.Right = "P"
			pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), put, mandateReductionStock("SYNB", 9002, 500, 80))
			in := mandateReductionFixture(t, pos, ratio, 1e7, 2)
			rows, st := mandateReductionRun(t, in, pos)
			assertMandateRowsReduceOnly(t, rows, pos)
			assertMandateStepsLowerRisk(t, in, rows)
			if _, sold := mandateRowFor(rows, 9011); sold || !slices.ContainsFunc(st.LeftOut, func(s string) bool { return strings.Contains(s, "P 95") && strings.Contains(s, "protection") }) {
				t.Fatalf("puts %g ratio %g: the protective put is a candidate: rows %+v left out %v", puts, ratio, rows, st.LeftOut)
			}
			shares := 3000.0
			if plan, ok := mandatePlanFor(st, 9001); ok {
				shares -= float64(plan.Units)
			}
			// Exercise every put: the delivered short shares against what is
			// left.
			if after := shares - puts*100; after < 0 {
				t.Fatalf("puts %g ratio %g: %g shares left under %g puts: exercise flips the line to %g", puts, ratio, shares, puts, after)
			}
			if puts == 30 && shares != 3000 {
				t.Fatalf("ratio %g: shares the puts deliver were cut: %g left", ratio, shares)
			}
		}
	}
}

// A line a working stop or trail holds leaves the plan: netting would hold
// its row for as long as the stop works, so the next contributor is cut
// instead and the line is named. A working immediate order (a sale in
// flight, partly filled) keeps its line in the plan: the cut it asks for is
// on its way.
func TestMandateReductionWorkingOrdersAndThePlan(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	in := mandateReductionFixture(t, pos, 1.1, 1e7, 2)
	in.WorkingOrder = func(row rpc.PositionView, _ string) (bool, bool) { return row.ConID == 9001, row.ConID == 9001 }
	rows, st := mandateReductionRun(t, in, pos)
	if _, ok := mandatePlanFor(st, 9001); ok || len(rows) == 0 || rows[0].Contract.ConID != 9002 {
		t.Fatalf("a stop-held line stayed in the plan: rows %+v plan %+v", rows, st.Plan)
	}
	if !slices.ContainsFunc(st.LeftOut, func(s string) bool {
		return strings.HasPrefix(s, "SYNA") && strings.Contains(s, "working stop or trail")
	}) {
		t.Fatalf("the held line is not named: %v", st.LeftOut)
	}
	if st.TargetReached != (mandateLossAfter(t, in, map[int]float64{9002: float64(rows[0].Mandate.Units)}) <= 0.95*mandateLossRoom(in)+1e-6) {
		t.Fatalf("target reached %v disagrees with the plan without SYNA", st.TargetReached)
	}

	// Partial fill: 100 of a 200-share sale filled, the rest still working.
	// The next verdict measures the 2,900 shares; the sale in flight keeps
	// SYNA in the plan, so SYNB is not cut for the shares already going.
	filled := mandateReductionBook(mandateReductionStock("SYNA", 9001, 2900, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	next := mandateReductionFixture(t, filled, 1.1, 1e7, 3)
	next.Input.Capital = in.Input.Capital
	next = mandateReductionVerdict(*next.Input, 3)
	next.WorkingOrder = func(row rpc.PositionView, _ string) (bool, bool) { return row.ConID == 9001, false }
	rows, st = mandateReductionRun(t, next, filled)
	if syna, ok := mandateRowFor(rows, 9001); !ok || !st.TargetReached || syna.Mandate.Rank != 1 {
		t.Fatalf("partial fill: rows %+v status %+v", rows, st)
	}
	if _, ok := mandateRowFor(rows, 9002); ok {
		t.Fatalf("SYNB cut while SYNA's sale is in flight and suffices: %+v", rows)
	}
}

// At the maximum loss the room is nil, so are the loss room, the budget
// and every gap cap: the batch reduces the book to what it may not cut (a
// hedge-listed put stays), and a verdict back within withdraws it. The
// verdict encodes.
func TestMandateReductionAtTheMaximumLoss(t *testing.T) {
	t.Parallel()
	hedge := mandateReductionCall("SPY", 9020, 10, 400, 420, 5, "20261120")
	hedge.Right = "P"
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80), hedge)
	fixture := mandateReductionFixture(t, pos, 1.1, 1e7, 2)
	book := *fixture.Input
	book.Capital.Room, book.Capital.BudgetAnnualSD = 0, 0
	in := mandateReductionVerdict(book, 2)
	v := in.Result.Verdict
	if v.Status != mandate.StatusReduce || !math.IsInf(v.Risk.RatioToBudget, 1) || v.Loss == nil || v.Loss.RoomBase != 0 || !math.IsInf(v.Loss.RatioToRoom, 1) || v.Stresses == nil || len(v.Stresses.GapBreaches) == 0 {
		t.Fatalf("verdict at room 0: %s ratio %v loss %+v breaches %+v", v.Status, v.Risk.RatioToBudget, v.Loss, v.Stresses)
	}
	raw, err := json.Marshal(in.Result)
	if err != nil || !strings.Contains(string(raw), `"ratio_to_budget":null`) || !strings.Contains(string(raw), `"ratio_to_room":null`) {
		t.Fatalf("encode at room 0: %v", err)
	}
	var back rpc.MandateRiskResult
	if err := json.Unmarshal(raw, &back); err != nil || back.Verdict.Status != mandate.StatusReduce || !math.IsInf(back.Verdict.Risk.RatioToBudget, 1) || !math.IsInf(back.Verdict.Loss.RatioToRoom, 1) {
		t.Fatalf("round trip at room 0: %+v %v", back.Verdict, err)
	}
	if !strings.Contains(mandateStatusSentence(v, "EUR"), "maximum") {
		t.Fatalf("sentence %q", mandateStatusSentence(v, "EUR"))
	}
	rows, st := mandateReductionRun(t, in, pos)
	assertMandateStepsLowerRisk(t, in, rows)
	for _, conID := range []int{9001, 9002} {
		if row, ok := mandateRowFor(rows, conID); !ok || row.PositionEffect != rpc.OrderPositionEffectClose {
			t.Fatalf("line %d not closed at the maximum loss: %+v (status %+v)", conID, row, st)
		}
	}
	if _, ok := mandateRowFor(rows, 9020); ok || !slices.ContainsFunc(st.LeftOut, func(s string) bool { return strings.HasPrefix(s, "SPY") }) {
		t.Fatalf("the hedge was cut: rows %+v left out %v", rows, st.LeftOut)
	}
	// One reading of a collapsed room (a glitch) only watches.
	glitch := mandateReductionVerdict(book, 1)
	if rows, st := mandateReductionRun(t, glitch, pos); len(rows) != 0 || st.State != rpc.MandateReductionWatching {
		t.Fatalf("one reading at room 0: rows %d status %+v", len(rows), st)
	}
	// Once the batch runs, a verdict back within withdraws it all.
	within := mandateReductionFixture(t, pos, 0.9, 1e7, 0)
	within.Previous, within.Episode = rows, st.Episode
	if rows, st := mandateReductionRun(t, within, pos); len(rows) != 0 || st.State != rpc.MandateReductionWithin || st.Episode != "" {
		t.Fatalf("after the glitch: rows %d status %+v", len(rows), st)
	}
}

// Under a latched drawdown brake a pre-authorised stock trailing stop is
// placed at once, and a mandate sale waits its full window: the sale never
// covers the stop then, so protection is not delayed. Without the latch an
// immediate sale goes first, as the owner decided. Two pre-authorised
// reductions of one line merge into the larger.
func TestMandateReductionMergeWithAutomaticChannels(t *testing.T) {
	t.Parallel()
	row := mandateReductionStock("SYNA", 9001, 3000, 100)
	policy := defaultProtectionPolicy()
	build := func(bucket string, qty int) rpc.TradeProposal {
		p := baseProposal(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, mandateTestNow, bucket, row, rpc.OrderActionSell, qty, rpc.OrderPositionEffectReduce, bucket)
		if bucket == rpc.TradeProposalBucketTrailingStop {
			p.OrderType, p.PositionEffect = rpc.OrderTypeTRAIL, rpc.OrderPositionEffectClose
		}
		if bucket == rpc.TradeProposalBucketMandateReduction {
			p.NeverSkipVeto = true
			p.Mandate = &rpc.TradeProposalMandate{Rank: 1, BatchRows: 1, Phase: rpc.MandatePhaseRisk, Units: qty}
		}
		return p
	}
	automatic := func(rpc.TradeProposal) sameContractChannel { return sameContractAutomatic }
	live := func(rows []rpc.TradeProposal) []string {
		var out []string
		for _, p := range rows {
			if len(p.Blockers) == 0 {
				out = append(out, p.Bucket)
			}
		}
		return out
	}
	for _, latched := range []bool{false, true} {
		rows := []rpc.TradeProposal{build(rpc.TradeProposalBucketTrailingStop, 3000), build(rpc.TradeProposalBucketMandateReduction, 600)}
		mergeSameContractProposalsLatched(rows, automatic, latched)
		got := live(rows)
		want := []string{rpc.TradeProposalBucketMandateReduction}
		if latched {
			want = []string{rpc.TradeProposalBucketTrailingStop, rpc.TradeProposalBucketMandateReduction}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("latched %v: live %v, want %v", latched, got, want)
		}
	}
	rows := []rpc.TradeProposal{build(rpc.TradeProposalBucketRiskReduction, 400), build(rpc.TradeProposalBucketMandateReduction, 600)}
	mergeSameContractProposals(rows, automatic)
	if got := live(rows); !slices.Equal(got, []string{rpc.TradeProposalBucketMandateReduction}) || rows[1].Quantity != 600 {
		t.Fatalf("two automatic reductions: live %v", got)
	}
}

// mandateNoticeKey is the alert episode a record's notice opens.
func mandateNoticeKey(t *testing.T, rec automaticSubmissionRecord) string {
	t.Helper()
	key, err := alertShadowAutomaticEpisodeKey(alertShadowBrokerScope{account: "DU1234567", mode: "paper"},
		automaticNoticeKey{Key: rec.Key, Revision: rec.Revision, Episode: rec.Episode, ClockStart: rec.CreatedAt, ClockQuantity: rec.clockQuantity()})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Re-sized within its breach, a row keeps its veto clock and its notice
// only while it stays within one lot above the size that clock was started
// for: shrinking, or growing by a lot, keeps them; growing further starts a
// fresh notice (a new alert episode carrying the size) and a full window,
// once per such growth, not per verdict, so "sell 20" can never become
// "close 40" on the old clock.
func TestMandateReductionGrowthBeyondALotGetsAFreshWindow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	resize := func(qty int) automaticSubmissionRecord {
		t.Helper()
		p := rig.mandateProposal(automaticTestStockRow(), qty, 1, 1)
		rev := rig.install(p)
		rig.cycle()
		return rig.record(p.Key, rev)
	}
	first := resize(20)
	rig.notice(first)
	first = rig.record(first.Key, first.Revision)
	rig.advance(5 * time.Minute)
	for _, qty := range []int{15, 21, 18} { // shrink, one lot up, back down
		rec := resize(qty)
		if !rec.SubmitAt.Equal(first.SubmitAt) || !rec.NoticedAt.Equal(first.NoticedAt) || rec.clockQuantity() != 20 || mandateNoticeKey(t, rec) != mandateNoticeKey(t, first) {
			t.Fatalf("re-size to %d: %+v, first %+v", qty, rec, first)
		}
		rig.advance(time.Minute)
	}
	grown := resize(40) // material growth at minute 8
	if grown.CreatedAt.Equal(first.CreatedAt) || !grown.NoticedAt.IsZero() || grown.clockQuantity() != 40 || !grown.SubmitAt.Equal(rig.now.Add(30*time.Minute)) ||
		mandateNoticeKey(t, grown) == mandateNoticeKey(t, first) {
		t.Fatalf("growth past a lot kept the old clock or notice: %+v", grown)
	}
	rig.notice(grown)
	grown = rig.record(grown.Key, grown.Revision)
	rig.advance(2 * time.Minute)
	again := resize(41) // within a lot of the new clock: no new notice
	if !again.CreatedAt.Equal(grown.CreatedAt) || mandateNoticeKey(t, again) != mandateNoticeKey(t, grown) {
		t.Fatalf("a re-size within a lot of the new clock re-noticed: %+v", again)
	}
	// The first clock's end passes: nothing goes out on it.
	rig.advance(25 * time.Minute)
	rig.cycle()
	if rig.revalidations != 0 {
		t.Fatalf("the grown row went out on the old clock: %d attempts", rig.revalidations)
	}
	rig.advance(10 * time.Minute)
	rig.cycle()
	if rig.revalidations != 1 {
		t.Fatalf("attempts after the fresh window = %d, want 1", rig.revalidations)
	}
}

// A row whose veto clock started in an earlier session, as after an
// overnight hold, never goes out on it: once its session opens it gets a
// fresh notice and a full window.
func TestMandateReductionNewSessionGetsAFreshWindow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	held := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	held.Mandate.Held, held.Mandate.HoldReason = true, "the verdict is 6m0s old; the batch trusts one up to 5m0s"
	rev := rig.install(held)
	rig.cycle()
	rec := rig.record(held.Key, rev)
	rig.notice(rec)
	rig.advance(31 * time.Minute)
	rig.cycle()
	if rec = rig.record(held.Key, rev); rec.HeldAt.IsZero() || rig.revalidations != 0 {
		t.Fatalf("the held record was not held: %+v", rec)
	}
	// The next day, ten minutes into the session, the verdict confirms it.
	rig.advance(24 * time.Hour)
	unheld := held
	unheld.Mandate = rpc.CloneProposalMandate(held.Mandate)
	unheld.Mandate.Held, unheld.Mandate.HoldReason = false, ""
	if got := rig.install(unheld); got != rev {
		t.Fatalf("confirming changed the revision")
	}
	rig.cycle()
	rec = rig.record(held.Key, rev)
	if rig.revalidations != 0 || !rec.NoticedAt.IsZero() || !rec.CreatedAt.Equal(rig.now) || !rec.SubmitAt.Equal(rig.now.Add(30*time.Minute)) || rec.HeldAt != (time.Time{}) {
		t.Fatalf("a clock from the last session fired or kept its notice: attempts %d record %+v", rig.revalidations, rec)
	}
	rig.notice(rec)
	rig.advance(31 * time.Minute)
	rig.cycle()
	if rig.revalidations != 1 {
		t.Fatalf("attempts after the fresh window = %d, want 1", rig.revalidations)
	}
}

// A mandate row is one session's order: after its DAY order expired unfilled
// (its record terminal), the next session's row is a new revision and gets
// a record, so the batch is never stranded on a revision already spent.
func TestMandateReductionNextSessionIsANewRevision(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	today := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev := rig.install(today)
	rig.cycle()
	if err := rig.engine.automatic.update(context.Background(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r := records[automaticRecordKey(today.Key, rev)]
		r.State, r.SubmittedAt, r.ResolvedAt, r.OrderRef = rpc.TradeProposalAutomaticSubmitted, rig.now, rig.now, "canary-synthetic-day"
		return []automaticSubmissionEvent{{At: rig.now, Type: automaticEventSubmitted, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State}}
	}); err != nil {
		t.Fatal(err)
	}
	rig.advance(24 * time.Hour)
	tomorrow := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	if slices.Equal(proposalRevisionTerms(today), proposalRevisionTerms(tomorrow)) {
		t.Fatal("the next session's row has today's revision terms")
	}
	revNext := rig.install(tomorrow)
	rig.cycle()
	if rec := rig.record(tomorrow.Key, revNext); rec.State != rpc.TradeProposalAutomaticPending || rec.SubmitAt.Before(rig.now.Add(30*time.Minute)) {
		t.Fatalf("the next session's record = %+v", rec)
	}
}

// A new breach episode starts a new window, and the owner's veto holds the
// line for the rest of an episode, whatever its size.
func TestMandateReductionEpisodeWindowAndVeto(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	a := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	revA := rig.install(a)
	rig.cycle()
	recA := rig.record(a.Key, revA)
	rig.advance(10 * time.Minute)
	b := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	b.Mandate.Episode = "mandate-synthetic-2"
	b.Mandate.Units = 19
	b.Quantity = 19
	revB := rig.install(b)
	rig.cycle()
	if recB := rig.record(b.Key, revB); !recB.SubmitAt.After(recA.SubmitAt) {
		t.Fatalf("a new episode kept the old clock: %s vs %s", recB.SubmitAt, recA.SubmitAt)
	}

	rig = newAutomaticTestRig(t, automaticMandateAuthority)
	v1 := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	revV := rig.install(v1)
	rig.cycle()
	if res, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: v1.Key, Revision: revV, Origin: rpc.OrderOriginHumanTTY}); err != nil || !res.Accepted {
		t.Fatalf("veto: %+v %v", res, err)
	}
	v2 := rig.mandateProposal(automaticTestStockRow(), 15, 1, 1)
	revV2 := rig.install(v2)
	rig.cycle()
	rig.noRecord(v2.Key, revV2)
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a == nil || a.State != rpc.TradeProposalAutomaticVetoed {
		t.Fatalf("the re-sized row does not show the episode's veto: %+v", a)
	}
	if rig.engine.sameContractChannels()(v2) != sameContractExcluded {
		t.Fatal("the merge counts on a vetoed line being placed")
	}
	rig.advance(time.Hour)
	rig.cycle()
	if rig.revalidations != 0 {
		t.Fatalf("a vetoed line was attempted in its episode: %d", rig.revalidations)
	}
}
