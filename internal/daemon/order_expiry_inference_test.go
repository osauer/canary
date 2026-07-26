package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load Berlin tz: %v", err)
	}
	return loc
}

func xetraDayOrderView(ref string, tif string) rpc.OrderView {
	return rpc.OrderView{
		OrderRef:    ref,
		Symbol:      "SAP",
		SecType:     "STK",
		ConID:       14204,
		Exchange:    "SMART",
		PrimaryExch: "IBIS",
		Currency:    "EUR",
		Action:      rpc.OrderActionSell,
		OrderType:   rpc.OrderTypeTRAIL,
		TIF:         tif,
		Open:        true,
		SendState:   orderSendStateSendAttempted,
		LastEvent:   orderJournalEventSendAttempted,
	}
}

func TestInferDayOrderExpiry(t *testing.T) {
	t.Parallel()
	loc := berlin(t)
	// Tuesday 2026-06-09 was a regular Xetra trading day (close 17:30 CEST).
	placedIntraday := time.Date(2026, 6, 9, 14, 21, 0, 0, loc)
	placedAfterClose := time.Date(2026, 6, 9, 19, 50, 0, 0, loc)

	eventsFor := func(ref string, at time.Time) map[string][]rpc.OrderEvent {
		return map[string][]rpc.OrderEvent{"ref:" + ref: {{At: at, Type: orderJournalEventSendAttempted}}}
	}

	t.Run("intraday DAY order expires after close plus margin", func(t *testing.T) {
		views := []rpc.OrderView{xetraDayOrderView("a", rpc.OrderTIFDay)}
		now := time.Date(2026, 6, 9, 19, 0, 0, 0, loc) // 17:30 close + 1h margin passed
		inferDayOrderExpiry(views, eventsFor("a", placedIntraday), now)
		v := views[0]
		if v.LifecycleStatus != rpc.OrderLifecycleExpiredInferred || v.Open || v.CancelEligible || v.ModifyEligible {
			t.Fatalf("view = %+v, want expired_inferred closed", v)
		}
	})

	t.Run("after-close placement works the next session", func(t *testing.T) {
		views := []rpc.OrderView{xetraDayOrderView("b", rpc.OrderTIFDay)}
		now := time.Date(2026, 6, 10, 10, 0, 0, 0, loc) // next session open and running
		inferDayOrderExpiry(views, eventsFor("b", placedAfterClose), now)
		if !views[0].Open || views[0].LifecycleStatus == rpc.OrderLifecycleExpiredInferred {
			t.Fatalf("view = %+v, want still open during its effective session", views[0])
		}
		// ...and expires after that next session closes.
		nowLate := time.Date(2026, 6, 10, 19, 0, 0, 0, loc)
		inferDayOrderExpiry(views, eventsFor("b", placedAfterClose), nowLate)
		if views[0].LifecycleStatus != rpc.OrderLifecycleExpiredInferred {
			t.Fatalf("view = %+v, want expired after effective session close", views[0])
		}
	})

	t.Run("six-day-old zombie expires", func(t *testing.T) {
		views := []rpc.OrderView{xetraDayOrderView("c", rpc.OrderTIFDay)}
		now := time.Date(2026, 6, 15, 10, 0, 0, 0, loc)
		inferDayOrderExpiry(views, eventsFor("c", time.Date(2026, 6, 4, 14, 21, 0, 0, loc)), now)
		if views[0].LifecycleStatus != rpc.OrderLifecycleExpiredInferred {
			t.Fatalf("view = %+v, want expired_inferred", views[0])
		}
	})

	t.Run("GTC and options are never inferred", func(t *testing.T) {
		gtc := xetraDayOrderView("d", "GTC")
		opt := xetraDayOrderView("e", rpc.OrderTIFDay)
		opt.SecType = "OPT"
		views := []rpc.OrderView{gtc, opt}
		events := eventsFor("d", placedIntraday)
		events["ref:e"] = []rpc.OrderEvent{{At: placedIntraday}}
		inferDayOrderExpiry(views, events, time.Date(2026, 6, 15, 10, 0, 0, 0, loc))
		for _, v := range views {
			if !v.Open || v.LifecycleStatus == rpc.OrderLifecycleExpiredInferred {
				t.Fatalf("view %s = %+v, want untouched", v.OrderRef, v)
			}
		}
	})
}

func TestDuplicateProtectiveBlockersCouplesWithExpiryInference(t *testing.T) {
	t.Parallel()
	loc := berlin(t)
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	engine := &proposalEngine{server: srv, now: srv.now}

	appendOpenTrail := func(ref string, at time.Time) {
		t.Helper()
		err := srv.orderJournal.Append(orderJournalEvent{
			Version: 1, At: at, Type: orderJournalEventSendAttempted,
			OrderRef: ref, Symbol: "MBG", SecType: "STK", ConID: 29622935,
			Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR",
			Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFDay,
			SendState: orderSendStateSendAttempted, Source: proposalOrderSource,
			Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU1234567", Mode: rpc.AccountModePaper, Quantity: 1,
		})
		if err != nil {
			t.Fatalf("append journal: %v", err)
		}
	}
	proposal := rpc.TradeProposal{
		Symbol: "MBG", SecType: "STK", Action: rpc.OrderActionSell,
		Bucket: rpc.TradeProposalBucketTrailingStop, OrderType: rpc.OrderTypeTRAIL,
		PositionQuantity: 1,
		Contract:         rpc.ContractParams{ConID: 29622935},
	}

	// Fresh open trail on the same contract+side blocks the duplicate…
	appendOpenTrail("ibkr-live-trail", srv.now())
	blockers := engine.duplicateProtectiveBlockers(context.Background(), proposal)
	if len(blockers) != 1 || blockers[0].Code != "existing_protective_order" {
		t.Fatalf("blockers = %+v, want existing_protective_order", blockers)
	}

	// …the opposite side does not…
	buy := proposal
	buy.Action = rpc.OrderActionBuy
	if blockers := engine.duplicateProtectiveBlockers(context.Background(), buy); len(blockers) != 0 {
		t.Fatalf("opposite-side blockers = %+v, want none", blockers)
	}

	// …and an inferred-expired zombie must NOT block re-protection: rebuild
	// the journal with only an ancient DAY order and check the row expires
	// out of the duplicate check (critic's coupling requirement).
	srv2 := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	engine2 := &proposalEngine{server: srv2, now: srv2.now}
	// The harness clock is fixed at 2026-05-28 08:45 UTC; place the zombie a
	// week earlier (Wed 2026-05-20, a regular Xetra day) so its effective
	// session is long closed relative to the fixture's "now".
	old := time.Date(2026, 5, 20, 14, 21, 0, 0, loc)
	if err := srv2.orderJournal.Append(orderJournalEvent{
		Version: 1, At: old, Type: orderJournalEventSendAttempted,
		OrderRef: "ibkr-zombie", Symbol: "MBG", SecType: "STK", ConID: 29622935,
		Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR",
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		SendState: orderSendStateSendAttempted, Source: proposalOrderSource,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU1234567", Mode: rpc.AccountModePaper, Quantity: 1,
	}); err != nil {
		t.Fatalf("append zombie: %v", err)
	}
	if blockers := engine2.duplicateProtectiveBlockers(context.Background(), proposal); len(blockers) != 0 {
		t.Fatalf("zombie blockers = %+v, want none (expired_inferred must not block re-protection)", blockers)
	}
	views, _, err := srv2.loadOrderViews()
	if err != nil {
		t.Fatalf("loadOrderViews: %v", err)
	}
	if len(views) != 1 || views[0].LifecycleStatus != rpc.OrderLifecycleExpiredInferred {
		t.Fatalf("zombie view = %+v, want expired_inferred", views)
	}
}

func TestDuplicateProtectiveBlockersIgnorePlainLimitAndMismatchedStops(t *testing.T) {
	t.Parallel()
	proposal := rpc.TradeProposal{
		Key:              "trailing_stop:mbg",
		Bucket:           rpc.TradeProposalBucketTrailingStop,
		Symbol:           "MBG",
		SecType:          "STK",
		Action:           rpc.OrderActionSell,
		OrderType:        rpc.OrderTypeTRAIL,
		PositionQuantity: 100,
		Contract:         rpc.ContractParams{ConID: 29622935},
	}
	appendOrder := func(t *testing.T, srv *Server, ref, orderType string, qty float64) {
		t.Helper()
		if err := srv.orderJournal.Append(orderJournalEvent{
			Version: 1, At: srv.orderNow(), Type: orderJournalEventSendAttempted,
			OrderRef: ref, Symbol: "MBG", SecType: "STK", ConID: 29622935,
			Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR",
			Action: rpc.OrderActionSell, OrderType: orderType, TIF: rpc.OrderTIFGTC,
			SendState: orderSendStateSendAttempted, Source: proposalOrderSource,
			Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU1234567", Mode: rpc.AccountModePaper, Quantity: qty,
		}); err != nil {
			t.Fatalf("append %s: %v", ref, err)
		}
	}

	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	engine := &proposalEngine{server: srv, now: srv.now}
	appendOrder(t, srv, "ibkr-limit-profit", rpc.OrderTypeLMT, 100)
	if blockers := engine.duplicateProtectiveBlockers(context.Background(), proposal); len(blockers) != 0 {
		t.Fatalf("plain limit blockers = %+v, want none", blockers)
	}

	srv2 := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	engine2 := &proposalEngine{server: srv2, now: srv2.now}
	appendOrder(t, srv2, "ibkr-oversized-trail", rpc.OrderTypeTRAIL, 200)
	if blockers := engine2.duplicateProtectiveBlockers(context.Background(), proposal); len(blockers) != 0 {
		t.Fatalf("oversized stale trail blockers = %+v, want none", blockers)
	}
}

// GTC orders never expiry-infer, so the broker's "can't find order" reply
// (error 135) is their only self-heal: it must close the row and unblock
// re-protection, while ordinary broker noise must not.
func TestBrokerCantFindOrderHealsGTCZombie(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	engine := &proposalEngine{server: srv, now: srv.now}
	base := orderJournalEvent{
		Version: 1, OrderRef: "ibkr-gtc-trail", Symbol: "MBG", SecType: "STK",
		ConID: 29622935, Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR",
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC,
		Source:   proposalOrderSource,
		Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU1234567", Mode: rpc.AccountModePaper,
		Quantity: 1, ReservedOrderID: 42,
	}
	at := srv.now()
	appendEv := func(mut func(*orderJournalEvent)) {
		t.Helper()
		ev := base
		at = at.Add(time.Second)
		ev.At = at
		mut(&ev)
		if err := srv.orderJournal.Append(ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	appendEv(func(ev *orderJournalEvent) {
		ev.Type = orderJournalEventSendAttempted
		ev.SendState = orderSendStateSendAttempted
	})
	appendEv(func(ev *orderJournalEvent) {
		ev.Type = orderJournalEventBrokerAcknowledged
		ev.SendState = orderSendStateBrokerAcknowledged
		ev.Status = "Submitted"
	})

	proposal := rpc.TradeProposal{Symbol: "MBG", SecType: "STK", Action: rpc.OrderActionSell, Bucket: rpc.TradeProposalBucketTrailingStop, OrderType: rpc.OrderTypeTRAIL, PositionQuantity: 1, Contract: rpc.ContractParams{ConID: 29622935}}
	if blockers := engine.duplicateProtectiveBlockers(context.Background(), proposal); len(blockers) != 1 {
		t.Fatalf("working GTC blockers = %+v, want existing_protective_order", blockers)
	}

	appendEv(func(ev *orderJournalEvent) { ev.Type = orderJournalEventCancelRequested })
	appendEv(func(ev *orderJournalEvent) {
		ev.Type = orderJournalEventBrokerError
		ev.Message = "broker error 2110: connectivity between TWS and server is broken"
	})
	if blockers := engine.duplicateProtectiveBlockers(context.Background(), proposal); len(blockers) != 1 {
		t.Fatalf("post-noise blockers = %+v, want still blocked", blockers)
	}

	appendEv(func(ev *orderJournalEvent) { ev.Type = orderJournalEventCancelRequested })
	appendEv(func(ev *orderJournalEvent) {
		ev.Type = orderJournalEventBrokerError
		ev.ErrorCode = 135
		ev.Message = "broker error 135: Can't find order with id =42"
	})
	views, _, err := srv.loadOrderViews()
	if err != nil {
		t.Fatalf("loadOrderViews: %v", err)
	}
	if len(views) != 1 || views[0].LifecycleStatus != rpc.OrderLifecycleInactive || views[0].Open {
		t.Fatalf("healed view = %+v, want inactive closed", views)
	}
	if blockers := engine.duplicateProtectiveBlockers(context.Background(), proposal); len(blockers) != 0 {
		t.Fatalf("healed blockers = %+v, want none", blockers)
	}
}
