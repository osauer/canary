package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func reviewSweepRow(t *testing.T, now time.Time, cash float64) rpc.TradeProposal {
	t.Helper()
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": cash}), now)
	cashSweepResolveBills(context.Background(), usBillSource(now), policy.Buckets.CashSweep, &plan, now)
	return cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cashSweepCurrencyOf(t, plan, "USD"))
}

func TestCashSweepAmountChangeGetsANewNoticeAndVetoWindow(t *testing.T) {
	rig := newAutomaticTestRig(t, `pre_authorised = ["cash_sweep"]`)
	row := reviewSweepRow(t, rig.now, 10000)
	revision := rig.install(row)
	rig.engine.reconcileAutomatic(context.Background())
	old := rig.record(row.Key, revision)
	rig.notice(old)
	rig.advance(20 * time.Minute)
	row = reviewSweepRow(t, rig.now, 60000)
	next := rig.install(row)
	rig.engine.reconcileAutomatic(context.Background())
	if next == revision || rig.record(row.Key, revision).State != rpc.TradeProposalAutomaticSuperseded {
		t.Fatal("a changed sweep order kept the old automatic window")
	}
	rec := rig.record(row.Key, next)
	if rec.Quantity != row.Quantity || !rec.NoticedAt.IsZero() || rec.SubmitAt.Before(rig.now.Add(30*time.Minute)) {
		t.Fatalf("new sweep did not receive a full notice/window: %+v", rec)
	}
	// The same whole order units keep the record and window despite cash
	// changing by one currency unit.
	row = reviewSweepRow(t, rig.now, 60001)
	if same := rig.install(row); same != next {
		t.Fatal("sub-unit cash movement changed the order revision")
	}
	rig.engine.reconcileAutomatic(context.Background())
	if !rig.record(row.Key, next).CreatedAt.Equal(rec.CreatedAt) {
		t.Fatal("unchanged order restarted its automatic window")
	}
}

func TestCashSweepSameOrderReappearingAfterATerminalRecordGetsANewWindow(t *testing.T) {
	rig := newAutomaticTestRig(t, `pre_authorised = ["cash_sweep"]`)
	row := reviewSweepRow(t, rig.now, 10000)
	revision := rig.install(row)
	rig.engine.reconcileAutomatic(context.Background())
	if err := rig.engine.automatic.update(context.Background(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		records[automaticRecordKey(row.Key, revision)].State = rpc.TradeProposalAutomaticSubmitted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The order disappears after filling; later cash arrives and the same
	// bill and capped whole-unit quantity become eligible again.
	rig.install()
	rig.engine.reconcileAutomatic(context.Background())
	rig.restart()
	row = reviewSweepRow(t, rig.now, 10000)
	next := rig.install(row)
	rig.engine.reconcileAutomatic(context.Background())
	if next == revision || rig.record(row.Key, next).State != rpc.TradeProposalAutomaticPending {
		t.Fatal("the old terminal record prevented a new sweep order")
	}
	// Restarting during this new episode cannot manufacture another window
	// or erase its existing notice/outcome identity.
	rec := rig.record(row.Key, next)
	rig.restart()
	if restored := rig.install(row); restored != next {
		t.Fatal("unchanged sweep changed its episode across restart")
	}
	rig.engine.reconcileAutomatic(context.Background())
	if !rig.record(row.Key, next).CreatedAt.Equal(rec.CreatedAt) {
		t.Fatal("restart recreated the same sweep episode")
	}
}

func TestCashSweepSourceOutageCannotManufactureANewEpisode(t *testing.T) {
	rig := newAutomaticTestRig(t, `pre_authorised = ["cash_sweep"]`)
	row := reviewSweepRow(t, rig.now, 10000)
	revision := rig.install(row)
	rig.engine.reconcileAutomatic(context.Background())
	rec := rig.record(row.Key, revision)
	_, status := rig.server.protectionPolicies.Active()
	retained, ok := rig.engine.preserveSnapshotOnRefreshFailure(rig.scope, rpc.AutoTradeStatus{}, status,
		[]rpc.TradingBlocker{{Code: "account_unavailable", Message: "synthetic broker read outage"}}, false)
	if !ok || retained.Revision != revision || len(retained.Proposals) != 1 || len(retained.Blockers) == 0 {
		t.Fatalf("source outage erased the reviewed episode: %+v", retained)
	}
	rig.engine.reconcileAutomatic(context.Background())
	if rig.record(row.Key, revision).State != rpc.TradeProposalAutomaticSuperseded {
		t.Fatal("blocked source retained an actionable automatic record")
	}
	if restored := rig.install(row); restored != revision {
		t.Fatal("same order after source recovery manufactured a new episode")
	}
	rig.engine.reconcileAutomatic(context.Background())
	got := rig.record(row.Key, revision)
	if got.State != rpc.TradeProposalAutomaticSuperseded || !got.CreatedAt.Equal(rec.CreatedAt) {
		t.Fatal("source recovery reset a terminal record or retried an order")
	}
}
