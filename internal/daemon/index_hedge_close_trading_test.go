//go:build trading

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The hedge's close travels the production write path: waits the full veto
// window, then one BUY at the ask under the daemon's pre-authorised origin. It
// shrinks the short and never turns it long.
func TestHedgeClosePreAuthorisedSubmitsAfterTheVeto(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticMandateAuthority)
	rig.server.previewSessionAt = func(m marketcal.Market, _ time.Time) (marketcal.Session, error) {
		return marketcal.Session{Market: m, Label: "US equities", State: marketcal.StateRegular, IsOpen: true}, nil
	}
	rig.server.orderPreviewPositionImpact = fixedPreviewPosition(-5, -1, rpc.OrderPositionEffectReduce)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	row := mandateReductionFuture(hedgeTestConID, -5, 6000)
	row.Exchange, row.LocalSymbol, row.TradingClass, row.Expiry = "CME", "MESZ6", "MES", "20261218"
	prop := rig.mandateProposal(row, 4, 1, 1)
	prop.Action, prop.Mandate.Rank, prop.Mandate.Phase = rpc.OrderActionBuy, 0, rpc.MandatePhaseHedge
	prop.Key = proposalKey(prop.Bucket, prop.Contract, prop.Action)
	prop.PositionEffect = rpc.OrderPositionEffectReduce
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.Bucket != preAuthorisedBucketMandateReduction || rec.LatchSkippedWindow {
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
	got := broker.orders[0]
	if got.TotalQty != 4 || !strings.EqualFold(got.Action, rpc.OrderActionBuy) || got.LmtPrice != 25.1 {
		t.Fatalf("broker order = %+v, want BUY 4 at the ask", got)
	}
	rec = rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticSubmitted {
		t.Fatalf("record after submission = %+v", rec)
	}
	for _, ev := range journalEventsForToken(t, rig.server, rec.PreviewTokenID) {
		if ev.Type == orderJournalEventSendAttempted && (ev.Origin != rpc.OrderOriginDaemonPreAuthorised || ev.Source != proposalOrderSource) {
			t.Fatalf("journaled attempt origin %q source %q", ev.Origin, ev.Source)
		}
	}
}
