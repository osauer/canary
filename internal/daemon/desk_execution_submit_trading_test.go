//go:build trading

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A served row is sent exactly once under the standing mandate through the
// production write path (preview token, order journal, fake broker socket):
// accepted with its order reference and journaled with the desk-automatic
// origin; a retry answers from the journal and sends nothing; a second
// request of the same episode is refused; the grant is cleared afterwards.
// With the controller no longer running, the next row is refused before any
// journaling and the broker is not called.
func TestDeskExecutionSubmitSendsAServedRowOnceUnderTheMandate(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, `pre_authorised = []`)
	s := rig.server
	s.startedAt = rig.now
	scope, err := s.deskExecutionScope()
	if err != nil || scope != rig.scope {
		t.Fatalf("scope %+v %v", scope, err)
	}
	broker := &brokerCallLog{}
	broker.install(s)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.noRecord(prop.Key, revision) // nothing pre-authorised: the scheduler leaves it alone
	full := installDeskMandate(t, s, scope, "full", true)
	item := rpc.DeskExecutionItem{RequestID: "req-stop-1", EpisodeID: "stop:" + prop.Key + ":2026-09-21", Intent: rpc.DeskExecutionIntent{Class: rpc.DeskClassProtect, Proposal: &rpc.DeskExecutionProposal{Key: prop.Key, Revision: revision}}}
	got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || len(got.Receipts) != 1 {
		t.Fatalf("submit: %+v %v", got, err)
	}
	r := got.Receipts[0]
	if r.Outcome != rpc.DeskOutcomeAccepted || r.OrderRef == "" || r.Validate(item, false) != nil || !strings.Contains(r.Message, full.ID) || !strings.Contains(r.Message, "Fully armed") {
		t.Fatalf("receipt %+v", r)
	}
	if broker.count() != 1 || !strings.EqualFold(broker.orders[0].Action, rpc.OrderActionSell) || broker.orders[0].TotalQty != 40 {
		t.Fatalf("broker calls %d orders %+v", broker.count(), broker.orders)
	}
	if s.deskExecutionGrant.Load() != nil {
		t.Fatal("grant left installed after the submission")
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	attempted := false
	for _, ev := range events {
		if ev.Type == orderJournalEventSendAttempted && ev.OrderRef == r.OrderRef {
			attempted = true
			if ev.Origin != rpc.OrderOriginDeskAutomatic {
				t.Fatalf("journaled origin %q", ev.Origin)
			}
		}
	}
	if !attempted {
		t.Fatal("no send-attempted journal event with the desk-automatic origin")
	}
	if look := deskExecutionLookup(t, s, item.RequestID)[0]; look.Outcome != rpc.DeskOutcomeAccepted || look.OrderRef != r.OrderRef {
		t.Fatalf("lookup %+v", look)
	}
	// Retry: the same receipt, no second order.
	again, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || again.Receipts[0].Outcome != rpc.DeskOutcomeAccepted || again.Receipts[0].OrderRef != r.OrderRef || broker.count() != 1 {
		t.Fatalf("retry %+v %v calls %d", again, err, broker.count())
	}
	// The episode yields no second order under a new request ID.
	second := item
	second.RequestID = "req-stop-2"
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{second}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerEpisodeConsumed || broker.count() != 1 {
		t.Fatalf("consumed episode %+v %v calls %d", got, err, broker.count())
	}
	// A class Canary does not give the row is refused and recorded.
	wrong := rpc.DeskExecutionItem{RequestID: "req-stop-3", EpisodeID: "stop:" + prop.Key + ":2026-09-22", Intent: rpc.DeskExecutionIntent{Class: rpc.DeskClassAdd, Proposal: &rpc.DeskExecutionProposal{Key: prop.Key, Revision: revision}}}
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{wrong}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerClassMismatch || broker.count() != 1 {
		t.Fatalf("class mismatch %+v %v calls %d", got, err, broker.count())
	}
	if look := deskExecutionLookup(t, s, wrong.RequestID)[0]; look.Outcome != rpc.DeskOutcomeRefused {
		t.Fatalf("the class refusal is not journaled: %+v", look)
	}
	// The controller stops (a new daemon process, or Desk in MANUAL never
	// confirms running): the next row is refused before journaling.
	s.startedAt = s.startedAt.Add(time.Second)
	third := item
	third.RequestID, third.EpisodeID = "req-stop-4", "stop:"+prop.Key+":2026-09-23"
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{third}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerAuthorityChanged || broker.count() != 1 {
		t.Fatalf("not running %+v %v calls %d", got, err, broker.count())
	}
	if look := deskExecutionLookup(t, s, third.RequestID)[0]; look.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("an authority refusal was journaled: %+v", look)
	}
}

// The write transaction carries the mandate's fence to the wire guard for
// the desk-automatic origin, and only while a grant is held; the guard then
// refuses an entry fence once the mandate has dropped to Protection, with
// the broker never called.
func TestDeskExecutionBindingCarriesTheFenceToTheWire(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, `pre_authorised = []`)
	s := rig.server
	s.startedAt = rig.now
	s.orderPlaceBroker = nil
	s.orderReserveBrokerID = nil
	scope, _ := s.deskExecutionScope()
	auth, _, err := s.authorizeBrokerWriteTransaction(rpc.OrderOriginDeskAutomatic, false)
	if err != nil || auth.Allowed {
		t.Fatalf("without a grant: allowed %v err %v", auth.Allowed, err)
	}
	full := installDeskMandate(t, s, scope, "full", true)
	record, err := s.readDeskAuthority(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence := deskAuthorityFence{ID: full.ID, Generation: record.Generation, PreferencesHash: record.PreferencesHash, AdaptivePriority: record.AdaptivePriority, Action: "entry", ValidUntil: rig.now.Add(time.Minute)}
	s.deskExecutionGrant.Store(&deskExecutionGrant{Key: "k", Revision: "r", Class: rpc.DeskClassAdd, Fence: fence})
	defer s.deskExecutionGrant.Store(nil)
	s.orderWriteBindingForTest = func(rpc.TradingStatus) (*ibkrlib.Connector, uint64, ibkrlib.ConnectorSessionBinding, brokerStateScope) {
		return nil, 0, ibkrlib.ConnectorSessionBinding{}, rig.scope
	}
	auth, binding, err := s.authorizeBrokerWriteTransaction(rpc.OrderOriginDeskAutomatic, false)
	if err != nil || !auth.Allowed || binding.deskAuthority == nil || *binding.deskAuthority != fence {
		t.Fatalf("with a grant: allowed %v err %v fence %+v", auth.Allowed, err, binding.deskAuthority)
	}
	// The final wire check also requires the order's independently verified
	// contract and risk authority, as a real placeOrder call supplies them.
	prop := rig.stopProposal()
	preview, err := s.previewOrder(t.Context(), proposalOrderPreviewParams(prop, prop.Quantity, 0))
	if err != nil || preview == nil || !preview.SubmitEligible {
		t.Fatalf("preview: %+v err %v", preview, err)
	}
	payload, err := s.verifyPreviewTokenForPlace(preview.PreviewToken)
	if err != nil {
		t.Fatalf("verify preview token: %v", err)
	}
	if err := s.bindPreviewOrderRiskAuthority(t.Context(), &binding, auth.Status, payload, payload.Draft); err != nil {
		t.Fatalf("bind preview risk authority: %v", err)
	}
	guard, release := s.brokerWireGuard(binding, auth.Status, false)
	defer release()
	if err := guard(); err != nil {
		t.Fatalf("a current entry fence refused at the wire: %v", err)
	}
	release()
	// The mandate drops to Protection: the entry fence is refused at the wire.
	record.Scope = "protect"
	if _, err := s.saveDeskAuthority(t.Context(), record.Generation, record, "test-downgrade"); err != nil {
		t.Fatal(err)
	}
	guard, release = s.brokerWireGuard(binding, auth.Status, false)
	defer release()
	if err := guard(); err == nil || !strings.Contains(err.Error(), "automatic trading authority changed") {
		t.Fatalf("an entry fence passed the wire under Protection: %v", err)
	}
}

// coreBuyProposal is the core's monthly buy of a synthetic London line as
// the engine serves it: a core_rebalance row carrying its typed exception's
// figures (the sized quantity within the frozen target, the decision close),
// for a session whose due time (13:45Z, fifteen minutes after the US open on
// 28 May 2026) the rig's clock sits before at 08:45Z.
func (r *automaticTestRig) coreBuyProposal() rpc.TradeProposal {
	contract := rpc.ContractParams{ConID: 900001, Symbol: "SYNX", SecType: "STK", Currency: "USD", Exchange: "SMART", PrimaryExch: "LSEETF", Market: "uk", Multiplier: 1}
	terms := rpc.CoreRowTerms{OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Limit: coreLimitRule, Exchange: "SMART", PrimaryExchange: "LSEETF",
		Episode: "core:DU1234567:paper:2026-04", Window: []string{"2026-05-01", "2026-05-04", "2026-05-05", "2026-05-06", "2026-05-07"}}
	return rpc.TradeProposal{Key: proposalKey(rpc.TradeProposalBucketCoreRebalance, contract, rpc.OrderActionBuy), State: rpc.TradeProposalStateGenerated,
		Bucket: rpc.TradeProposalBucketCoreRebalance, Symbol: "SYNX", SecType: "STK", Action: rpc.OrderActionBuy, Quantity: 10, MaxQuantity: 10,
		PositionEffect: rpc.OrderPositionEffectOpen, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: contract,
		Reason: "core sleeve, decision 2026-04-30 (trend on, target 10 shares, 0 shares held): buy 10 shares for the 2026-05-28 session, a DAY limit at the live mid",
		Core: &rpc.TradeProposalCore{Decision: "2026-04-30", TrendOn: true, Kind: core.ActionBuy, Target: 10, Held: 0, ScalePct: 25, ConID: 900001, Currency: "USD",
			DecisionClose: 25, Session: "2026-05-28", Sleeve: "core", Stage: "live", Terms: terms}}
}

// The core's buy goes through the production write path under the standing
// mandate by behaviour: before its due time the row is refused opening_window
// and nothing is journaled or sent; at the due time with a delayed quote the
// preview refuses quote_unusable before journaling; with a live quote the buy
// passes close_reduce_only under its typed exception, is placed once with the
// desk-automatic origin, and a retry answers from the journal.
func TestDeskExecutionSubmitSendsTheCoreBuyUnderItsTypedException(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, `pre_authorised = []`)
	s := rig.server
	s.startedAt = rig.now
	s.previewSessionAt = officialCalendar
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 10, rpc.OrderPositionEffectOpen)
	scope, err := s.deskExecutionScope()
	if err != nil {
		t.Fatal(err)
	}
	broker := &brokerCallLog{}
	broker.install(s)
	prop := rig.coreBuyProposal()
	revision := rig.install(prop)
	full := installDeskMandate(t, s, scope, "full", true)
	item := rpc.DeskExecutionItem{RequestID: "req-core-1", EpisodeID: deskExecutionStrategyEpisode(prop), Intent: rpc.DeskExecutionIntent{Class: rpc.DeskClassAdd, Proposal: &rpc.DeskExecutionProposal{Key: prop.Key, Revision: revision}}}

	// 08:45Z: London is open, the US is not; the row waits for its due time.
	got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.ReadinessOpeningWindow || broker.count() != 0 {
		t.Fatalf("before the due time: %+v %v, broker calls %d", got, err, broker.count())
	}
	if r := deskExecutionLookup(t, s, item.RequestID)[0]; r.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("journaled before the due time: %+v", r)
	}

	// 14:00Z, a delayed quote: the preview refuses before journaling.
	rig.now = time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	live := s.orderPreviewQuote
	s.orderPreviewQuote = func(ctx context.Context, contract rpc.ContractParams, budget time.Duration) (rpc.OrderQuoteSnapshot, error) {
		q, err := live(ctx, contract, budget)
		q.DataType = rpc.MarketDataDelayed
		return q, err
	}
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.ReadinessQuoteUnusable || broker.count() != 0 {
		t.Fatalf("delayed quote: %+v %v, broker calls %d", got, err, broker.count())
	}
	if r := deskExecutionLookup(t, s, item.RequestID)[0]; r.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("a delayed-quote refusal was journaled: %+v", r)
	}

	// A live quote: the buy is placed once, under the typed exception.
	s.orderPreviewQuote = live
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || len(got.Receipts) != 1 {
		t.Fatalf("submit: %+v %v", got, err)
	}
	r := got.Receipts[0]
	if r.Outcome != rpc.DeskOutcomeAccepted || r.OrderRef == "" || !strings.Contains(r.Message, core.RuleID) {
		t.Fatalf("receipt %+v", r)
	}
	if broker.count() != 1 || !strings.EqualFold(broker.orders[0].Action, rpc.OrderActionBuy) || broker.orders[0].TotalQty != 10 {
		t.Fatalf("broker calls %d orders %+v", broker.count(), broker.orders)
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	attempted := false
	for _, ev := range events {
		if ev.Type == orderJournalEventSendAttempted && ev.OrderRef == r.OrderRef {
			attempted = true
			if ev.Origin != rpc.OrderOriginDeskAutomatic {
				t.Fatalf("journaled origin %q", ev.Origin)
			}
		}
	}
	if !attempted {
		t.Fatal("no send-attempted journal event with the desk-automatic origin")
	}
	again, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || again.Receipts[0].Outcome != rpc.DeskOutcomeAccepted || again.Receipts[0].OrderRef != r.OrderRef || broker.count() != 1 {
		t.Fatalf("retry %+v %v calls %d", again, err, broker.count())
	}
}
