//go:build trading

package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestArmedRetryTransientPreviewKeepsWindowAndSubmitsOnce(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	quote := rig.server.orderPreviewQuote
	available, reads := false, 0
	rig.server.orderPreviewQuote = func(ctx context.Context, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
		reads++
		if !available {
			return rpc.OrderQuoteSnapshot{}, errors.New("synthetic temporary quote outage")
		}
		return quote(ctx, contract, timeout)
	}
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	before := rig.record(prop.Key, revision)
	rig.advance(31 * time.Minute)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticPending || !rec.ResubmitAt.Equal(rig.now.Add(automaticDeferRetry)) || !rec.ResolvedAt.IsZero() {
		t.Fatalf("definitely-unsent transient preview must keep a paced retry: %+v", rec)
	}
	if !rec.CreatedAt.Equal(before.CreatedAt) || !rec.NoticedAt.Equal(before.NoticedAt) || !rec.SubmitAt.Equal(before.SubmitAt) {
		t.Fatalf("retry changed the owner's existing window: before=%+v after=%+v", before, rec)
	}
	if reads != 1 || broker.count() != 0 || rec.PreviewTokenID != "" || !rec.SubmittingAt.IsZero() {
		t.Fatalf("transient refusal crossed the submission boundary: reads=%d broker=%d record=%+v", reads, broker.count(), rec)
	}
	// The persisted retry floor survives a process restart and frequent ticks.
	rig.restart()
	rig.install(prop)
	available = true
	rig.advance(automaticDeferRetry - time.Second)
	rig.cycle()
	rig.cycle()
	if reads != 1 || broker.count() != 0 {
		t.Fatalf("retried before the persisted floor: reads=%d broker=%d", reads, broker.count())
	}
	rig.advance(time.Second)
	rig.cycle()
	if rec = rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticSubmitted || reads < 2 || broker.count() != 1 {
		t.Fatalf("recovered preview did not submit once: reads=%d broker=%d record=%+v", reads, broker.count(), rec)
	}
	if rec.PreviewTokenID == "" || rec.OrderRef == "" {
		t.Fatalf("submission lost its durable identity: %+v", rec)
	}
	rig.advance(time.Hour)
	rig.restart()
	rig.install(prop)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("completed retry sent again across restart: %d broker calls", broker.count())
	}
}

func TestArmedRetrySavedUnsentStopGetsFreshNoticeBeforeRecovery(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	// Existing releases saved a terminal failure before minting a preview
	// token when the quote for notional conversion was unavailable.
	if err := rig.engine.automatic.update(context.Background(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		rec := records[automaticRecordKey(prop.Key, revision)]
		rec.State, rec.ResolvedAt = rpc.TradeProposalAutomaticFailed, rig.now
		rec.Reason = previewNotionalUnavailableCode + ": synthetic FX quote unavailable"
		return []automaticSubmissionEvent{{At: rig.now, Type: automaticEventFailed, Key: rec.Key, Revision: rec.Revision, State: rec.State, Reason: rec.Reason}}
	}); err != nil {
		t.Fatal(err)
	}
	rig.advance(time.Hour)
	rig.latched = true
	rig.restart()
	rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticPending || !rec.CreatedAt.Equal(rig.now) || !rec.SubmitAt.Equal(rig.now.Add(30*time.Minute)) || !rec.NoticedAt.IsZero() || !rec.ResolvedAt.IsZero() {
		t.Fatalf("saved pre-preview failure did not regain a fresh notice and window: %+v", rec)
	}
	rig.advance(31 * time.Minute)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatal("saved failure submitted without a renewed notice")
	}
	rig.notice(rec)
	rig.advance(29 * time.Minute)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatal("saved failure submitted before the renewed notice's full veto window")
	}
	rig.advance(time.Minute)
	rig.cycle()
	if broker.count() != 1 || rig.record(prop.Key, revision).State != rpc.TradeProposalAutomaticSubmitted {
		t.Fatalf("saved stop was not recovered once after its renewed window: broker=%d record=%+v", broker.count(), rig.record(prop.Key, revision))
	}
}

func TestArmedRetryRemainsVetoableDuringTransientWait(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	quote := rig.server.orderPreviewQuote
	rig.server.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		return rpc.OrderQuoteSnapshot{}, errors.New("synthetic temporary quote outage")
	}
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	rig.cycle()
	res, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: prop.Key, Revision: revision, Origin: rpc.OrderOriginPairedDevice})
	if err != nil || !res.Accepted {
		t.Fatalf("owner could not veto while a transient refusal waited: result=%+v err=%v", res, err)
	}
	rig.server.orderPreviewQuote = quote
	rig.advance(time.Hour)
	rig.restart()
	rig.install(prop)
	rig.cycle()
	if broker.count() != 0 || rig.record(prop.Key, revision).State != rpc.TradeProposalAutomaticVetoed {
		t.Fatalf("retry escaped the owner's veto: broker=%d record=%+v", broker.count(), rig.record(prop.Key, revision))
	}
}

func TestArmedRetryNeverReplaysAStagedBrokerOutcome(t *testing.T) {
	t.Parallel()
	for _, disposition := range []ibkrlib.SendDisposition{ibkrlib.SendDispositionUnknown, ibkrlib.SendDispositionMayHaveWritten, ibkrlib.SendDispositionDefinitelyUnsent} {
		t.Run(string(disposition), func(t *testing.T) {
			rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
			calls := 0
			rig.server.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
				calls++
				return ibkrlib.WithSendDisposition(errors.New("synthetic transport failure"), disposition)
			}
			prop := rig.stopProposal()
			revision := rig.install(prop)
			rig.cycle()
			rig.notice(rig.record(prop.Key, revision))
			rig.advance(31 * time.Minute)
			rig.cycle()
			before := rig.record(prop.Key, revision)
			if calls != 1 || before.State != rpc.TradeProposalAutomaticFailed || before.PreviewTokenID == "" || before.SubmittingAt.IsZero() {
				t.Fatalf("broker failure lost its staged identity: calls=%d record=%+v", calls, before)
			}
			rig.server.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
				calls++
				return nil
			}
			rig.advance(time.Hour)
			rig.restart()
			rig.install(prop)
			rig.cycle()
			after := rig.record(prop.Key, revision)
			if calls != 1 || after.State != rpc.TradeProposalAutomaticFailed || after.PreviewTokenID != before.PreviewTokenID || !after.SubmittingAt.Equal(before.SubmittingAt) {
				t.Fatalf("staged outcome was replayed or replaced: calls=%d before=%+v after=%+v", calls, before, after)
			}
		})
	}
}

func TestArmedRetryChangedHoldingCannotDuplicateWorkingOrUncertainSale(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"working remainder", "uncertain send"} {
		t.Run(outcome, func(t *testing.T) {
			rig := newAutomaticTradingRig(t, automaticMandateAuthority)
			var sends []ibkrlib.RawOrder
			rig.server.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
				sends = append(sends, *order)
				if outcome == "uncertain send" && len(sends) == 1 {
					return ibkrlib.WithSendDisposition(errors.New("synthetic uncertain send"), ibkrlib.SendDispositionMayHaveWritten)
				}
				return nil
			}
			position := automaticTestStockRow()
			position.Quantity = 300
			rig.server.orderPreviewPositionImpact = fixedPreviewPosition(300, 200, rpc.OrderPositionEffectReduce)
			first := rig.mandateProposal(position, 100, 1, 1)
			first.Mandate.Units = 300
			first = armedSelectionInstall(t, rig, first, rpc.TradeProposalSourceFingerprints{})
			rig.cycle()
			rig.notice(rig.record(first.Key, first.Revision))
			rig.advance(31 * time.Minute)
			rig.cycle()
			original := rig.record(first.Key, first.Revision)
			if len(sends) != 1 || sends[0].TotalQty != 100 || original.PreviewTokenID == "" {
				t.Fatalf("first capped sale did not reach the synthetic wire once: sends=%d record=%+v", len(sends), original)
			}
			if outcome == "working remainder" {
				// The broker confirms only ten shares filled; ninety of the
				// original order still work. No second order may compete.
				order := sends[0]
				rig.brokerOrders = []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder,
					OrderID: order.OrderID, PermID: 91001, ClientID: order.ClientID, ClientIDPresent: true,
					Account: rig.scope.Account, Symbol: first.Symbol, SecType: first.SecType, ConID: first.Contract.ConID,
					Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, Status: "Submitted", TotalQuantity: 100, Filled: 10, Remaining: 90,
					OrderRef: order.OrderRef, LimitPrice: order.LmtPrice, TIF: order.TIF}}
			}
			// A fresh held quantity changes the production mandate revision,
			// even though the cap still chooses another hundred shares.
			position.Quantity = 290
			rig.server.orderPreviewPositionImpact = fixedPreviewPosition(290, 190, rpc.OrderPositionEffectReduce)
			next := rig.mandateProposal(position, 100, 1, 1)
			next.Mandate.Units = 290
			next = armedSelectionInstall(t, rig, next, rpc.TradeProposalSourceFingerprints{})
			if next.Revision == first.Revision {
				t.Fatal("changed held quantity did not exercise the new revision binding")
			}
			rig.cycle()
			rig.notice(rig.record(next.Key, next.Revision))
			rig.advance(31 * time.Minute)
			rig.cycle()
			after := rig.record(next.Key, next.Revision)
			if len(sends) != 1 || after.PreviewTokenID != "" || !after.SubmittingAt.IsZero() {
				t.Fatalf("new revision duplicated the unsettled first order: sends=%d record=%+v", len(sends), after)
			}
			if !strings.Contains(after.Reason, reductionOrderExistingCode) && after.HoldReason != automaticHoldHandOrder {
				t.Fatalf("test did not reach the broker/journal duplicate fence: %+v", after)
			}
			rig.advance(time.Hour)
			rig.restart()
			armedSelectionInstall(t, rig, next, rpc.TradeProposalSourceFingerprints{})
			rig.cycle()
			if len(sends) != 1 || rig.record(first.Key, first.Revision).PreviewTokenID != original.PreviewTokenID {
				t.Fatal("restart replayed or replaced the unsettled order identity")
			}
		})
	}
}
