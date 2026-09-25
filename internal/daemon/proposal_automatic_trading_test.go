//go:build trading

package daemon

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// newAutomaticTradingRig builds the hermetic rig over the trading-build
// test server: real preview tokens, a real order journal in daemon.db and a
// fake broker socket, so a submission travels the production write path up
// to the wire.
func newAutomaticTradingRig(t *testing.T, authority string) *automaticTestRig {
	t.Helper()
	// A protective SELL on a long stock passes the daemon's conservative
	// short check only with allow_stock_short, as on the desk's own config.
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, AllowStockShort: true})
	ctx := context.Background()
	if err := initializeCleanProposalOpportunityAuthority(ctx, srv.coreStore); err != nil {
		t.Fatalf("initialize clean authority: %v", err)
	}
	// The preview-token signer runs on the server's fixed clock; the rig
	// clock starts there and advances from it.
	rig := &automaticTestRig{t: t, core: srv.coreStore, now: srv.orderNow(), scope: brokerStateScope{Account: "DU1234567", Mode: "paper"}}
	srv.now = func() time.Time { return rig.now }
	rig.installInventory(srv)
	srv.protectionPolicies = writePreAuthPolicy(t, preAuthPolicyTOML(authority, 1))
	srv.protectionPolicies.reload()
	if st := srv.protectionPolicies.Status(); st.Status != rpc.ProtectionPolicyStatusActive {
		t.Fatalf("test policy status = %+v", st)
	}
	srv.orderPreviewQuote = func(_ context.Context, contract rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 24.9, 25.1
		mid := 25.0
		return rpc.OrderQuoteSnapshot{Symbol: contract.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(40, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	nextID := 1000
	srv.orderReserveBrokerID = func(context.Context) (int, error) { nextID++; return nextID, nil }
	rig.server = srv
	rig.engine = rig.newEngine()
	srv.tradeProposals = rig.engine
	return rig
}

type brokerCallLog struct {
	mu     sync.Mutex
	orders []ibkrlib.RawOrder
}

func (l *brokerCallLog) install(srv *Server) {
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.orders = append(l.orders, *order)
		return nil
	}
}

func (l *brokerCallLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.orders)
}

func journalEventsForToken(t *testing.T, srv *Server, tokenID string) []orderJournalEvent {
	t.Helper()
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var out []orderJournalEvent
	for _, ev := range events {
		if ev.PreviewTokenID == tokenID {
			out = append(out, ev)
		}
	}
	return out
}

func TestAutomaticPreAuthorisedBucketSubmitsExactlyOncePerKeyAndRevision(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	rig.notice(rec)
	// Inside the window nothing is placed, however many cycles run.
	rig.advance(29 * time.Minute)
	rig.cycle()
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("broker calls inside the window = %d", broker.count())
	}
	// The window closes: one order, journaled with the daemon origin.
	rig.advance(2 * time.Minute)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after the window = %d, want 1; record = %+v", broker.count(), rig.record(prop.Key, revision))
	}
	rec = rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticSubmitted || rec.SubmittedAt.IsZero() || rec.OrderRef == "" || rec.PreviewTokenID == "" {
		t.Fatalf("record after submission = %+v", rec)
	}
	attempted := false
	for _, ev := range journalEventsForToken(t, rig.server, rec.PreviewTokenID) {
		if ev.Type == orderJournalEventSendAttempted {
			attempted = true
			if ev.Origin != rpc.OrderOriginDaemonPreAuthorised || ev.Source != proposalOrderSource || ev.OrderRef != rec.OrderRef {
				t.Fatalf("journaled attempt = origin %q source %q ref %q", ev.Origin, ev.Source, ev.OrderRef)
			}
		}
	}
	if !attempted {
		t.Fatal("no send-attempted journal event for the automatic submission")
	}
	if got := broker.orders[0]; got.TotalQty != 40 || !strings.EqualFold(got.Action, rpc.OrderActionSell) {
		t.Fatalf("broker order = %+v, want SELL 40", got)
	}
	// Later cycles at the same key and revision never place again, and the
	// served row shows the outcome.
	rig.advance(time.Hour)
	rig.cycle()
	rig.restart()
	rig.install(prop)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after restart = %d, want still 1", broker.count())
	}
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a == nil || a.State != rpc.TradeProposalAutomaticSubmitted || a.OrderReference != rec.OrderRef {
		t.Fatalf("served automatic after submission = %+v", a)
	}
	if rig.server.automaticGrant.Load() != nil {
		t.Fatal("write grant left installed after the submission")
	}
}

func TestAutomaticLatchedBrakeSubmitsWithoutWaiting(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	rig.latched = true
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls with the brake latched = %d, want 1 in the same cycle", broker.count())
	}
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticSubmitted || !rec.LatchSkippedWindow || !rec.NoticedAt.IsZero() {
		t.Fatalf("latched record = %+v", rec)
	}
}

// frozenPlatformSettings is a runtime settings store with trading.freeze set.
func frozenPlatformSettings() *platformSettingsStore {
	return &platformSettingsStore{data: platformSettingsData{Version: platformSettingsDocVersion, Trading: platformTradingSettingsData{Freeze: new(true)}}}
}

// deferUnderFreeze drives one pre-authorised record to its closed window on
// a frozen desk and returns the proposal and revision.
func deferUnderFreeze(t *testing.T, rig *automaticTestRig) (rpc.TradeProposal, string) {
	t.Helper()
	rig.server.platformSettings = frozenPlatformSettings()
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticDeferred {
		t.Fatalf("record under freeze = %+v, want deferred", rec)
	}
	return prop, revision
}

// C1: a pre-authorised submission refused by the freeze is deferred, not
// failed. It keeps its revision, names the freeze, journals the deferral and
// is not attempted again while the desk stays frozen; once the freeze lifts
// within the revision it is submitted exactly once.
func TestAutomaticFreezeDefersAndSubmitsOnceAfterTheFreezeLifts(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop, revision := deferUnderFreeze(t, rig)
	if broker.count() != 0 {
		t.Fatalf("frozen desk placed %d order(s)", broker.count())
	}
	rec := rig.record(prop.Key, revision)
	if rec.DeferredAt.IsZero() || !rec.ResubmitAt.Equal(rig.now.Add(automaticDeferRetry)) || !rec.ResolvedAt.IsZero() ||
		!strings.Contains(rec.Reason, "trading.freeze") || rec.PreviewTokenID != "" {
		t.Fatalf("deferred record = %+v", rec)
	}
	if got := rig.eventCount(prop.Key, revision, automaticEventDeferred); got != 1 {
		t.Fatalf("deferred events = %d, want 1", got)
	}
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a == nil || a.State != rpc.TradeProposalAutomaticDeferred || a.ResubmitAt.IsZero() || a.DeferredAt.IsZero() {
		t.Fatalf("served automatic = %+v", a)
	}
	if st := rig.server.autoTradeStatus(); st.AutomaticDeferred != 1 || st.AutomaticPending != 0 {
		t.Fatalf("status pending/deferred = %d/%d, want 0/1", st.AutomaticPending, st.AutomaticDeferred)
	}
	// Still frozen well past the resubmit time: no attempt, no new deferral.
	attempts := rig.revalidations
	rig.advance(2 * time.Hour)
	rig.cycle()
	rig.cycle()
	if broker.count() != 0 || rig.revalidations != attempts || rig.eventCount(prop.Key, revision, automaticEventDeferred) != 1 {
		t.Fatalf("frozen scheduler retried: calls=%d attempts=%d->%d", broker.count(), attempts, rig.revalidations)
	}
	// The freeze lifts while the revision is current: exactly one order.
	rig.server.platformSettings = nil
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after the freeze lifted = %d, want 1; record = %+v", broker.count(), rig.record(prop.Key, revision))
	}
	rec = rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticSubmitted || rec.OrderRef == "" || rec.DeferredAt.IsZero() {
		t.Fatalf("record after resubmission = %+v", rec)
	}
	for range 3 {
		rig.advance(time.Hour)
		rig.cycle()
	}
	rig.restart()
	rig.install(prop)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after further cycles and a restart = %d, want still 1", broker.count())
	}
}

// C1: a freeze that lands after the write gate admitted the submission is
// refused before any broker frame, typed as the freeze, so it defers rather
// than fails. The spent attempt stays in the order journal as definitely
// unsent, and the resubmission after the freeze mints its own preview.
func TestAutomaticFreezeAfterAdmissionDefersWithTheAttemptProvenUnsent(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	armed := true
	rig.server.orderWriteBeforeBrokerSend = func() {
		if armed {
			armed = false
			rig.server.platformSettings = frozenPlatformSettings()
		}
	}
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("broker calls after a late freeze = %d", broker.count())
	}
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticDeferred || rig.eventCount(prop.Key, revision, automaticEventSubmitting) != 1 {
		t.Fatalf("record after a late freeze = %+v", rec)
	}
	events, err := rig.server.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	spent := ""
	for _, ev := range events {
		if ev.Type == orderJournalEventSendError {
			if ev.SendDisposition != ibkrlib.SendDispositionDefinitelyUnsent || !strings.Contains(ev.Message, tradingFrozenBlockerMessage) {
				t.Fatalf("late-freeze send error = %+v", ev)
			}
			spent = ev.PreviewTokenID
		}
	}
	if spent == "" {
		t.Fatal("the refused attempt left no journaled send error")
	}
	rig.server.platformSettings = nil
	rig.advance(automaticDeferRetry)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after the freeze lifted = %d, want 1", broker.count())
	}
	if rec = rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticSubmitted || rec.PreviewTokenID == "" || rec.PreviewTokenID == spent {
		t.Fatalf("resubmitted record = %+v, want a fresh preview token", rec)
	}
}

// The freeze refusal is typed wherever the write path meets it before the
// wire: admission, the operation's own gate and the wire guard all return
// errTradingFrozen inside ErrTradingDisabled with the unchanged text, and a
// refusal with any other cause is not typed as the freeze.
func TestTradingFreezeRefusalIsTypedAtEveryPreSendGate(t *testing.T) {
	t.Parallel()
	frozen := tradingBlockersError([]rpc.TradingBlocker{{Code: tradingFrozenBlockerCode, Message: tradingFrozenBlockerMessage}})
	if !errors.Is(frozen, errTradingFrozen) || !errors.Is(frozen, ErrTradingDisabled) || frozen.Error() != "trading disabled: "+tradingFrozenBlockerMessage {
		t.Fatalf("freeze refusal = %v", frozen)
	}
	committing := tradingBlockersError([]rpc.TradingBlocker{{Code: tradingFrozenBlockerCode, Message: tradingFrozenBlockerMessage}, {Code: tradingControlsChangedBlockerCode, Message: "changed"}})
	if !errors.Is(committing, errTradingFrozen) {
		t.Fatalf("freeze committed during admission = %v, want typed", committing)
	}
	mixed := tradingBlockersError([]rpc.TradingBlocker{{Code: "gateway_unavailable", Message: "down"}, {Code: tradingFrozenBlockerCode, Message: tradingFrozenBlockerMessage}})
	if errors.Is(mixed, errTradingFrozen) || !errors.Is(mixed, ErrTradingDisabled) {
		t.Fatalf("multi-cause refusal = %v, must not read as the freeze alone", mixed)
	}
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	_, binding, err := rig.server.authorizeBrokerWriteTransaction(rpc.OrderOriginHumanTTY, false)
	if err != nil {
		t.Fatalf("admission on a ready desk: %v", err)
	}
	rig.server.platformSettings = frozenPlatformSettings()
	guard, release := rig.server.brokerWireGuard(binding, rig.server.currentTradingStatus(), false)
	defer release()
	if err := guard(); !errors.Is(err, errTradingFrozen) {
		t.Fatalf("wire guard under freeze = %v, want the typed freeze", err)
	}
	if _, err := rig.server.placeOrder(context.Background(), rpc.OrderPlaceParams{Origin: rpc.OrderOriginHumanTTY}); !errors.Is(err, errTradingFrozen) {
		t.Fatalf("place admission under freeze = %v, want the typed freeze", err)
	}
}

// C1: a revision change while deferred supersedes the deferred record as a
// pending one would be; lifting the freeze afterwards never submits it.
func TestAutomaticRevisionChangeWhileDeferredSupersedes(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop, first := deferUnderFreeze(t, rig)
	grown := prop
	grown.Quantity = 60
	second := rig.install(grown)
	if second == first {
		t.Fatal("test expects a new revision")
	}
	rig.cycle()
	if rec := rig.record(prop.Key, first); rec.State != rpc.TradeProposalAutomaticSuperseded || !strings.Contains(rec.Reason, "revision changed") {
		t.Fatalf("deferred record after a revision change = %+v", rec)
	}
	if fresh := rig.record(prop.Key, second); fresh.State != rpc.TradeProposalAutomaticPending || !fresh.SubmitAt.Equal(rig.now.Add(30*time.Minute)) {
		t.Fatalf("new revision record = %+v, want a fresh window", fresh)
	}
	rig.server.platformSettings = nil
	rig.advance(10 * time.Minute)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("a superseded deferred record was submitted: %d call(s)", broker.count())
	}
}

// C1: the veto still works while a record is deferred, and holds after the
// freeze lifts.
func TestAutomaticVetoWorksWhileDeferred(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop, revision := deferUnderFreeze(t, rig)
	if _, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: prop.Key, Origin: rpc.OrderOriginAgent}); err == nil {
		t.Fatal("an agent vetoed a deferred record")
	}
	res, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: prop.Key, Origin: rpc.OrderOriginPairedDevice, Reason: "not after the freeze"})
	if err != nil || !res.Accepted || res.State != rpc.TradeProposalAutomaticVetoed || res.Revision != revision {
		t.Fatalf("veto while deferred = %+v err = %v", res, err)
	}
	rig.server.platformSettings = nil
	rig.advance(time.Hour)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("a vetoed deferred record was submitted: %d call(s)", broker.count())
	}
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticVetoed || rec.Origin != rpc.OrderOriginPairedDevice {
		t.Fatalf("record after veto = %+v", rec)
	}
}

// seedWorkingJournalOrder journals an order on the trading rig's route with
// the given request origin, acknowledged by the broker as working.
func seedWorkingJournalOrder(t *testing.T, srv *Server, ref string, reservedID, permID int, origin string) {
	t.Helper()
	base := orderJournalEvent{
		At: srv.orderNow(), OrderRef: ref, ReservedOrderID: reservedID, ClientID: 31,
		Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper",
		Symbol: "OTH", SecType: "STK", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFGTC, Quantity: 3, LimitPrice: 50,
	}
	attempt := base
	attempt.Type, attempt.AttemptID, attempt.ActionKind = orderJournalEventSendAttempted, "attempt-"+ref, corestore.ActionPlace
	attempt.SendState, attempt.Origin = orderSendStateSendAttempted, origin
	ack := base
	ack.Type, ack.PermID, ack.Status, ack.Remaining, ack.SendState = orderJournalEventBrokerAcknowledged, permID, "Submitted", 3, orderSendStateBrokerAcknowledged
	if err := srv.orderJournal.AppendAll([]orderJournalEvent{attempt, ack}); err != nil {
		t.Fatalf("seed working order %s: %v", ref, err)
	}
}

func workingBrokerOrder(reservedID, permID int) ibkrlib.OrderLifecycleEvent {
	return ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: reservedID, PermID: permID, ClientID: 31, ClientIDPresent: true, Account: "DU1234567",
		Symbol: "OTH", SecType: "STK", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, TotalQuantity: 3, LimitPrice: 50, Status: "Submitted",
	}
}

// C1 settling rule: a due pre-authorised submission does not fire while a
// hand order placed after its record was created is working at the broker,
// and fires exactly once as soon as that order is cancelled or filled — on
// its original window, which the wait never restarts. It holds under a
// latched brake too: the latch skips the veto window, not the settling. In
// the latched case the order is placed moments before the submission, so
// only the fresh broker read taken before firing shows it.
func TestAutomaticSettlingHoldsBehindANewHandOrderUntilItSettles(t *testing.T) {
	t.Parallel()
	for _, settle := range []string{"cancelled", "filled", "latched-then-cancelled"} {
		t.Run(settle, func(t *testing.T) {
			t.Parallel()
			rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
			broker := &brokerCallLog{}
			broker.install(rig.server)
			rig.latched = settle == "latched-then-cancelled"
			prop := rig.stopProposal()
			revision := rig.install(prop)
			if rig.latched {
				rig.freshOrders = []ibkrlib.OrderLifecycleEvent{handOrder(7777)}
				rig.cycle()
			} else {
				rig.cycle()
				rig.notice(rig.record(prop.Key, revision))
				rig.advance(10 * time.Minute)
				rig.brokerOrders = []ibkrlib.OrderLifecycleEvent{handOrder(7777)}
				rig.advance(21 * time.Minute)
				rig.cycle()
			}
			window := rig.record(prop.Key, revision).SubmitAt
			held := rig.record(prop.Key, revision)
			if broker.count() != 0 || held.State != rpc.TradeProposalAutomaticPending || held.HeldAt.IsZero() || held.HoldReason != automaticHoldHandOrder {
				t.Fatalf("due record behind a new hand order: calls=%d record=%+v", broker.count(), held)
			}
			rig.advance(2 * time.Hour)
			rig.cycle()
			rig.cycle()
			if broker.count() != 0 || rig.eventCount(prop.Key, revision, automaticEventHeld) != 1 {
				t.Fatalf("while the hand order works: calls=%d held events=%d", broker.count(), rig.eventCount(prop.Key, revision, automaticEventHeld))
			}
			switch settle {
			case "filled":
				filled := handOrder(7777)
				filled.Status, filled.Filled, filled.Remaining = "Filled", 5, 0
				rig.brokerOrders = []ibkrlib.OrderLifecycleEvent{filled}
			default:
				rig.brokerOrders, rig.freshOrders = nil, nil
			}
			rig.cycle()
			if broker.count() != 1 {
				t.Fatalf("broker calls once the hand order settled = %d, want 1; record = %+v", broker.count(), rig.record(prop.Key, revision))
			}
			rec := rig.record(prop.Key, revision)
			if rec.State != rpc.TradeProposalAutomaticSubmitted || !rec.SubmitAt.Equal(window) || !rec.HeldAt.IsZero() || rec.HoldReason != "" {
				t.Fatalf("record after settling = %+v, want submitted on window %s", rec, window)
			}
			rig.cycle()
			if broker.count() != 1 {
				t.Fatalf("broker calls after another cycle = %d, want still 1", broker.count())
			}
		})
	}
}

// C1 settling rule: only a hand order that is new relative to the record
// holds it. An order already working unmodified when the record was created
// — a standing stop from last week, even one whose trailing trigger has
// moved since — does not; the same order modified afterwards does, whether
// the modify is seen at the broker or requested through Canary; and when the
// broker could not be read at creation, no order can be proven standing.
func TestAutomaticSettlingIgnoresStandingOrdersButNotNewOrModifiedOnes(t *testing.T) {
	t.Parallel()
	standingTrail := func() ibkrlib.OrderLifecycleEvent {
		order := handOrder(8801)
		order.Action, order.OrderType, order.TIF = rpc.OrderActionSell, rpc.OrderTypeTRAIL, rpc.OrderTIFGTC
		order.TrailingPercent, order.TrailStopPrice, order.LimitPrice = 5, 95, 0
		return order
	}
	for name, tc := range map[string]struct {
		standing   []ibkrlib.OrderLifecycleEvent
		journaled  bool
		unreadable bool
		after      func(t *testing.T, rig *automaticTestRig)
		wantHold   bool
	}{
		"a standing order": {standing: []ibkrlib.OrderLifecycleEvent{handOrder(8800)}},
		"a standing trailing stop whose trigger moved": {
			standing: []ibkrlib.OrderLifecycleEvent{standingTrail()},
			after: func(_ *testing.T, rig *automaticTestRig) {
				moved := standingTrail()
				moved.TrailStopPrice = 97.5
				rig.brokerOrders = []ibkrlib.OrderLifecycleEvent{moved}
			},
		},
		"a standing order modified in TWS": {
			standing: []ibkrlib.OrderLifecycleEvent{handOrder(8800)},
			after: func(_ *testing.T, rig *automaticTestRig) {
				modified := handOrder(8800)
				modified.LimitPrice = 13
				rig.brokerOrders = []ibkrlib.OrderLifecycleEvent{modified}
			},
			wantHold: true,
		},
		"a standing terminal order modified through Canary": {
			standing:  []ibkrlib.OrderLifecycleEvent{workingBrokerOrder(600, 9600)},
			journaled: true,
			after: func(t *testing.T, rig *automaticTestRig) {
				modify := orderJournalEvent{
					At: rig.now, Type: orderJournalEventModifyRequested, OrderRef: "standing-terminal", ReservedOrderID: 600, PermID: 9600,
					ClientID: 31, Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", Symbol: "OTH", SecType: "STK",
					Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFGTC, Quantity: 3, LimitPrice: 51,
					AttemptID: "modify-standing-terminal", ActionKind: corestore.ActionModify, Origin: rpc.OrderOriginHumanTTY,
				}
				if err := rig.server.orderJournal.Append(modify); err != nil {
					t.Fatalf("journal the modify: %v", err)
				}
			},
			wantHold: true,
		},
		"a standing order when the broker was unreadable at creation": {
			standing:   []ibkrlib.OrderLifecycleEvent{handOrder(8800)},
			unreadable: true,
			wantHold:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
			broker := &brokerCallLog{}
			broker.install(rig.server)
			if tc.journaled {
				seedWorkingJournalOrder(t, rig.server, "standing-terminal", 600, 9600, rpc.OrderOriginHumanTTY)
			}
			rig.brokerOrders = tc.standing
			if tc.unreadable {
				rig.inventoryErr = errors.New("gateway reconnecting")
			}
			rig.advance(time.Minute)
			prop := rig.stopProposal()
			revision := rig.install(prop)
			rig.cycle()
			rig.inventoryErr = nil
			rec := rig.record(prop.Key, revision)
			if rec.SettlingBaselineKnown == tc.unreadable || (!tc.unreadable && len(rec.SettlingBaseline) != len(tc.standing)) {
				t.Fatalf("baseline at creation = known %v marks %v", rec.SettlingBaselineKnown, rec.SettlingBaseline)
			}
			rig.notice(rec)
			rig.advance(10 * time.Minute)
			if tc.after != nil {
				tc.after(t, rig)
			}
			rig.advance(21 * time.Minute)
			rig.cycle()
			rec = rig.record(prop.Key, revision)
			if tc.wantHold {
				if broker.count() != 0 || rec.HoldReason != automaticHoldHandOrder {
					t.Fatalf("calls=%d record=%+v, want held behind the hand order", broker.count(), rec)
				}
				// The held order settles: the record fires once on its window.
				rig.brokerOrders = nil
				rig.cycle()
			}
			if broker.count() != 1 || rig.record(prop.Key, revision).State != rpc.TradeProposalAutomaticSubmitted {
				t.Fatalf("calls=%d record=%+v, want submitted once", broker.count(), rig.record(prop.Key, revision))
			}
		})
	}
}

// C1 settling rule: working orders of the daemon's scheduler and of the
// agent-origin gate are the machine's own and hold nothing, however new; a
// new order a human placed from a terminal or the paired app, or one the
// journal holds with no origin, did not pass the gate and holds.
func TestAutomaticSettlingTellsTheMachineFromAHand(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		origins  []string
		wantHold bool
	}{
		"daemon and gate orders":         {origins: []string{rpc.OrderOriginDaemonPreAuthorised, rpc.OrderOriginAgent}},
		"a terminal order":               {origins: []string{rpc.OrderOriginDaemonPreAuthorised, rpc.OrderOriginHumanTTY}, wantHold: true},
		"a paired-device order":          {origins: []string{rpc.OrderOriginPairedDevice}, wantHold: true},
		"a journaled order of no origin": {origins: []string{""}, wantHold: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
			broker := &brokerCallLog{}
			broker.install(rig.server)
			prop := rig.stopProposal()
			revision := rig.install(prop)
			rig.cycle()
			rig.notice(rig.record(prop.Key, revision))
			rig.advance(10 * time.Minute)
			for i, origin := range tc.origins {
				seedWorkingJournalOrder(t, rig.server, "working-"+strconv.Itoa(i), 500+i, 9100+i, origin)
				rig.brokerOrders = append(rig.brokerOrders, workingBrokerOrder(500+i, 9100+i))
			}
			rig.advance(21 * time.Minute)
			rig.cycle()
			rec := rig.record(prop.Key, revision)
			if tc.wantHold {
				if broker.count() != 0 || rec.HoldReason != automaticHoldHandOrder {
					t.Fatalf("calls=%d record=%+v, want held", broker.count(), rec)
				}
				return
			}
			if broker.count() != 1 || rec.State != rpc.TradeProposalAutomaticSubmitted {
				t.Fatalf("calls=%d record=%+v, want submitted", broker.count(), rec)
			}
		})
	}
}

// C1 settling rule: the veto still works while a record is held, and an
// unreadable broker inventory holds until it can be read.
func TestAutomaticSettlingHoldKeepsTheVetoAndFailsClosed(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	rig.inventoryErr = errors.New("gateway down")
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); broker.count() != 0 || rec.HoldReason != automaticHoldUnavailable {
		t.Fatalf("unreadable inventory: calls=%d record=%+v", broker.count(), rec)
	}
	res, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: prop.Key, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || !res.Accepted || res.State != rpc.TradeProposalAutomaticVetoed {
		t.Fatalf("veto while held = %+v err = %v", res, err)
	}
	rig.inventoryErr = nil
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("a vetoed held record was submitted: %d call(s)", broker.count())
	}
}

func TestAutomaticOriginIsAcceptedOnlyUnderAGrantForAListedBucket(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, `pre_authorised = ["option_loss_exit"]`)
	status := rig.server.currentTradingStatus()
	// No scheduler grant: any caller claiming the daemon origin is refused.
	if blockers := rig.server.brokerWriteOriginBlockers(status, rpc.OrderOriginDaemonPreAuthorised); len(blockers) != 1 || blockers[0].Code != "daemon_origin_unauthorised" {
		t.Fatalf("origin blockers without grant = %+v", blockers)
	}
	auth := rig.server.brokerWriteAuthorizationForRequest(rpc.OrderOriginDaemonPreAuthorised)
	if auth.Allowed {
		t.Fatal("daemon origin authorised without a grant")
	}
	// A grant for a bucket the policy does not list is refused too.
	rig.server.automaticGrant.Store(&automaticWriteGrant{Key: "k", Revision: "r", Bucket: preAuthorisedBucketTrailingStop})
	if blockers := rig.server.brokerWriteOriginBlockers(status, rpc.OrderOriginDaemonPreAuthorised); len(blockers) != 1 || blockers[0].Code != "bucket_not_pre_authorised" {
		t.Fatalf("origin blockers for unlisted bucket = %+v", blockers)
	}
	// A grant for the listed bucket passes the origin gate; the other
	// gates are untouched by the origin.
	rig.server.automaticGrant.Store(&automaticWriteGrant{Key: "k", Revision: "r", Bucket: preAuthorisedBucketOptionLossExit})
	if blockers := rig.server.brokerWriteOriginBlockers(status, rpc.OrderOriginDaemonPreAuthorised); len(blockers) != 0 {
		t.Fatalf("origin blockers for listed bucket = %+v", blockers)
	}
	if !rig.server.brokerWriteAuthorizationForRequest(rpc.OrderOriginDaemonPreAuthorised).Allowed {
		t.Fatal("listed bucket under grant not authorised on a ready paper desk")
	}
	// Human origins never see the daemon-origin blockers.
	for _, origin := range []string{rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice, rpc.OrderOriginAgent} {
		if blockers := rig.server.brokerWriteOriginBlockers(status, origin); len(blockers) != 0 {
			t.Fatalf("origin %q blockers = %+v", origin, blockers)
		}
	}
	rig.server.automaticGrant.Store(nil)
	// A human submit of a proposal whose bucket is not listed still works
	// exactly as before: the automatic machinery never touches it.
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.noRecord(prop.Key, revision)
	res, err := rig.engine.Submit(context.Background(), rpc.TradeProposalSubmitParams{Key: prop.Key, Revision: revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || !res.Accepted || broker.count() != 1 {
		t.Fatalf("human submit = %+v err = %v calls = %d", res, err, broker.count())
	}
}

func TestAutomaticRestartBetweenIntentAndBrokerAcknowledgementPlacesNoSecondOrder(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	release := make(chan struct{})
	reached := make(chan struct{})
	calls := 0
	var callMu sync.Mutex
	rig.server.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, _ *ibkrlib.RawOrder) error {
		callMu.Lock()
		calls++
		first := calls == 1
		callMu.Unlock()
		if first {
			close(reached)
			<-release
		}
		return nil
	}
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	first := rig.engine
	done := make(chan struct{})
	go func() {
		defer close(done)
		first.runAutomaticCycle(context.Background())
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		close(release)
		<-done
		t.Fatalf("first process never reached the broker; record = %+v", rig.record(prop.Key, revision))
	}
	// The first process has persisted its intent and staged the attempt in
	// the journal, and is now waiting on the broker. A second process
	// starts on the same daemon.db.
	stuck, ok := first.automatic.get(prop.Key, revision)
	if !ok || stuck.State != rpc.TradeProposalAutomaticSubmitting || stuck.PreviewTokenID == "" {
		t.Fatalf("record at broker-call time = %+v, want submitting with a token id", stuck)
	}
	rig.advance(time.Minute)
	rig.restart()
	rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticFailed || !strings.Contains(rec.Reason, "unknown across the restart") || rec.OrderRef == "" {
		t.Fatalf("recovered record = %+v", rec)
	}
	rig.advance(time.Hour)
	rig.cycle()
	close(release)
	<-done
	callMu.Lock()
	defer callMu.Unlock()
	if calls != 1 {
		t.Fatalf("broker calls across the restart = %d, want exactly 1", calls)
	}
	if final := rig.record(prop.Key, revision); final.State != rpc.TradeProposalAutomaticFailed {
		t.Fatalf("first process overwrote the recovered outcome: %+v", final)
	}
}
