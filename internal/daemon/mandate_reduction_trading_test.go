//go:build trading

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// Pre-authorised, a mandate reduction travels the production write path
// once its full veto window has closed, even with the drawdown brake
// latched, and only then: one order, journaled with the daemon-preauthorised
// origin, and never a second for the same key and revision.
func TestMandateReductionPreAuthorisedSubmitsAfterTheVeto(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticMandateAuthority)
	rig.latched = true
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.LatchSkippedWindow || rec.Bucket != preAuthorisedBucketMandateReduction {
		t.Fatalf("record = %+v", rec)
	}
	rig.notice(rec)
	rig.advance(29 * time.Minute)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("placed inside the veto window: %d", broker.count())
	}
	rig.advance(2 * time.Minute)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after the window = %d, want 1; record %+v", broker.count(), rig.record(prop.Key, revision))
	}
	rec = rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticSubmitted || rec.OrderRef == "" || rec.PreviewTokenID == "" {
		t.Fatalf("record after submission = %+v", rec)
	}
	attempted := false
	for _, ev := range journalEventsForToken(t, rig.server, rec.PreviewTokenID) {
		if ev.Type == orderJournalEventSendAttempted {
			attempted = true
			if ev.Origin != rpc.OrderOriginDaemonPreAuthorised || ev.Source != proposalOrderSource {
				t.Fatalf("journaled attempt origin %q source %q", ev.Origin, ev.Source)
			}
		}
	}
	if !attempted {
		t.Fatal("no send-attempted journal event")
	}
	if got := broker.orders[0]; got.TotalQty != 40 || !strings.EqualFold(got.Action, rpc.OrderActionSell) {
		t.Fatalf("broker order = %+v, want SELL 40", got)
	}
	rig.advance(time.Hour)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("a second order for the same key and revision: %d", broker.count())
	}
}

// A held row is sent by nobody: the due record holds without a broker call,
// and the owner's own submit is refused too.
func TestMandateReductionHeldRowReachesNoBroker(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticMandateAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	prop.Mandate.Held, prop.Mandate.HoldReason = true, "the verdict is 6m0s old; the batch trusts one up to 5m0s"
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("a held row reached the broker: %d", broker.count())
	}
	if rec := rig.record(prop.Key, revision); !rec.waiting() || rec.HoldReason != automaticHoldMandateVerdict {
		t.Fatalf("record = %+v", rec)
	}
	res, err := rig.engine.Submit(t.Context(), rpc.TradeProposalSubmitParams{Key: prop.Key, Revision: revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || res.Accepted || len(res.Blockers) == 0 || res.Blockers[0].Code != mandateSizingHeldCode || broker.count() != 0 {
		t.Fatalf("hand submit of a held row: %+v err %v broker %d", res, err, broker.count())
	}
}

// The device-confirmed prepared path refuses a row held since it was
// prepared: the hold applies to every way an order leaves.
func TestMandateReductionPreparedSubmitRefusesAHeldRow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, "")
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	revision := rig.install(prop)
	prepared, err := rig.engine.Prepare(t.Context(), rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: revision, FastPath: true})
	if err != nil || !prepared.Accepted || prepared.PreparedRef == "" {
		t.Fatalf("prepare: accepted=%v blockers=%v err=%v", prepared.Accepted, prepared.Blockers, err)
	}
	held := prop
	held.Mandate = rpc.CloneProposalMandate(prop.Mandate)
	held.Mandate.Held, held.Mandate.HoldReason = true, "the mandate risk cannot be measured: daily history for SYN is stale"
	if again := rig.install(held); again != revision {
		t.Fatalf("holding changed the revision")
	}
	out := preparedSubmit(t, rig, prepared)
	if out.Accepted || len(out.Blockers) == 0 || out.Blockers[0].Code != mandateSizingHeldCode || broker.count() != 0 {
		t.Fatalf("prepared submit of a held row: accepted %v blockers %+v broker %d", out.Accepted, out.Blockers, broker.count())
	}
}
