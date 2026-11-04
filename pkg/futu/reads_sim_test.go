package futu_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

var bg = context.Background()

// clocked connects with the book's own clock, so hold times move when the test
// moves them.
func clocked(t *testing.T, srv *futusim.Server, account uint64) *futu.Connector {
	t.Helper()
	c := futu.NewConnector(futu.Config{
		Addr:          srv.Addr(),
		Account:       account,
		ClientVersion: 333,
		RedialDelay:   20 * time.Millisecond,
		Now:           srv.Book().Clock.Now,
	})
	c.Start(bg)
	t.Cleanup(c.Stop)
	waitFor(t, "a session", func() bool { _, err := c.Accounts(bg); return err == nil })
	return c
}

func TestStreamingLineFillsTheCacheAndHonoursTheHold(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := clocked(t, srv, simAccount)

	if _, err := c.BasicQuotes(bg, "HK.00700"); err == nil {
		t.Fatal("a basic quote without a line")
	}
	binding, err := c.Subscribe(bg, "HK.00700", "US.AAPL")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Epoch == 0 || len(c.Lines()) != 2 {
		t.Fatalf("binding %+v lines %v", binding, c.Lines())
	}
	quotes, err := c.BasicQuotes(bg, "HK.00700")
	if err != nil || len(quotes) != 1 {
		t.Fatalf("basic: %+v %v", quotes, err)
	}
	q := quotes[0]
	if q.Code != "HK.00700" || q.Last <= 0 || q.Delivery != futu.DeliveryStream || q.Name != "SYNTH TENCENT" || q.At.IsZero() || q.Suspended {
		t.Fatalf("basic quote %+v", q)
	}
	if q.Bid != 0 || q.Ask != 0 {
		t.Fatalf("a basic quote has no bid or ask: %+v", q)
	}

	srv.Book().Clock.Advance(5 * time.Second)
	srv.PushQuotes()
	waitFor(t, "the pushed quotes", func() bool { return len(c.QuoteCache()) == 2 })
	if cached := c.QuoteCache()["HK.00700"]; cached.Volume == 0 && cached.Last == 0 {
		t.Fatalf("cached quote %+v", cached)
	}

	// A release inside the hold is refused before anything is sent, and the line
	// keeps streaming; a line never taken costs nothing to release.
	err = c.Unsubscribe(bg, "HK.00700")
	var held *futu.SubscriptionHeldError
	if !errors.As(err, &held) || !errors.Is(err, futu.ErrSubscriptionHeld) || held.Code != "HK.00700" || held.RetryAfter != 55*time.Second {
		t.Fatalf("release inside the hold: %v", err)
	}
	if err := c.Unsubscribe(bg, "SG.D05"); err != nil {
		t.Fatalf("release of a line never taken: %v", err)
	}
	srv.Book().Clock.Advance(futu.SubscriptionMinimumHold)
	if err := c.Unsubscribe(bg, "HK.00700"); err != nil {
		t.Fatal(err)
	}
	if _, kept := c.Lines()["HK.00700"]; kept {
		t.Fatal("the released line is still held")
	}
	if _, cached := c.QuoteCache()["HK.00700"]; cached {
		t.Fatal("the released line is still cached")
	}
	used, _ := srv.Book().SubscriptionUsage()
	if used != 1 {
		t.Fatalf("quota used %d", used)
	}
}

// A new session holds no lines: a line belongs to the session that took it.
func TestANewSessionHoldsNoLines(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{KeepAliveSeconds: 1})
	c := clocked(t, srv, simAccount)
	first, err := c.Subscribe(bg, "HK.00700")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetSilent(true)
	waitFor(t, "the lost session", func() bool { return !c.Current(first) && len(c.Lines()) >= 0 && !c.IsReady() })
	srv.SetSilent(false)
	waitReady(t, c)
	waitFor(t, "the empty line table", func() bool { return len(c.Lines()) == 0 })
	second, err := c.Subscribe(bg, "HK.00700")
	if err != nil || second == first {
		t.Fatalf("second session %+v %v", second, err)
	}
	// Closing a connection released its lines: only the new one holds this one.
	waitFor(t, "the release on close", func() bool { used, _ := srv.Book().SubscriptionUsage(); return used == 1 })
}

// Quota exhaustion, spent by another client on the same OpenD, refuses the next
// subscribe typed, and the load reports it.
func TestQuotaExhaustionRefusesTypedAndTheLoadReportsIt(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	srv.Book().SetSubscriptionQuota(2)
	other := clocked(t, srv, simAccount)
	if _, err := other.Subscribe(bg, "SG.D05", "SG.ES3"); err != nil {
		t.Fatal(err)
	}
	c := clocked(t, srv, simAccount)
	_, err := c.Subscribe(bg, "HK.00700")
	if !errors.Is(err, futu.ErrQuotaExhausted) {
		t.Fatalf("subscribe past the quota: %v", err)
	}
	var reply *futu.Error
	if !errors.As(err, &reply) || srv.Refusals(futu.ProtoQotSub) != 1 {
		t.Fatalf("reply %v refusals %d", err, srv.Refusals(futu.ProtoQotSub))
	}
	load, err := c.RefreshLoad(bg)
	if err != nil {
		t.Fatal(err)
	}
	if load.SubscriptionsUsed != 2 || load.SubscriptionsRemaining != 0 || load.SubscriptionQuota != 2 || load.LastRefusalAt.IsZero() || load.LastRefusal == "" {
		t.Fatalf("load %+v", load)
	}

	// A refusal that is not the quota stays what it is.
	srv.Book().SetSubscriptionQuota(100)
	if _, err := c.Subscribe(bg, "HK.99999"); err == nil || errors.Is(err, futu.ErrQuotaExhausted) {
		t.Fatalf("unknown instrument: %v", err)
	}
	if _, err := c.Subscribe(bg, "nocode"); !errors.Is(err, futu.ErrInvalidCode) {
		t.Fatalf("invalid code: %v", err)
	}

	// Pushed quota figures overlay what was read.
	srv.PushQuotaState()
	waitFor(t, "the pushed quota", func() bool { return c.Load().SubscriptionQuota == 100 && c.Health().SpendPushed })
	if load := c.Load(); load.HistoryQuota != 100 || load.SubscriptionsUsed != 2 || load.SubscriptionsRemaining != 98 {
		t.Fatalf("pushed load %+v", load)
	}
}

// A snapshot carries a bid and an ask, and 401 instruments are split into two
// requests, as OpenD's limit of 400 requires.
func TestSnapshotsCarryTheBookAndSplitPastTheLimit(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := clocked(t, srv, simAccount)
	quotes, err := c.Snapshots(bg, "HK.00700", "US.AAPL")
	if err != nil || len(quotes) != 2 {
		t.Fatalf("snapshots %+v %v", quotes, err)
	}
	for _, q := range quotes {
		if q.Delivery != futu.DeliverySnapshot || q.Bid <= 0 || q.Ask <= q.Bid || q.LotSize <= 0 || q.BidSize <= 0 || q.AskSize <= 0 || q.PrevClose <= 0 || q.Open <= 0 {
			t.Fatalf("snapshot %+v", q)
		}
	}
	if quotes[0].LotSize != 100 || quotes[1].LotSize != 1 {
		t.Fatalf("lot sizes %+v", quotes)
	}

	var rows strings.Builder
	codes := make([]string, 0, 401)
	for i := range 401 {
		code := fmt.Sprintf("HK.%05d", i+1)
		codes = append(codes, code)
		if i > 0 {
			rows.WriteString(",")
		}
		fmt.Fprintf(&rows, `{"code":%q,"name":"SYNTH","lot_size":100,"currency":"HKD","start":10,"tick":0.01}`, code)
	}
	big, err := futusim.Parse([]byte(`{"clock":"2026-11-04T01:30:00Z","accounts":[{"acc_id":1000001,"env":"simulate","markets":["HK"]}],"instruments":[` + rows.String() + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	recorder := &futusim.Recorder{}
	bigSrv := futusim.Start(t, futusim.Options{Book: big, Record: recorder})
	bigClient := clocked(t, bigSrv, simAccount)
	all, err := bigClient.Snapshots(bg, codes...)
	if err != nil || len(all) != 401 {
		t.Fatalf("401 snapshots: %d %v", len(all), err)
	}
	if got := framesOf(recorder, futu.ProtoQotGetSecuritySnapshot); got != 2 {
		t.Fatalf("%d snapshot requests for 401 instruments", got)
	}
	if _, err := bigClient.Snapshots(bg, "bad"); !errors.Is(err, futu.ErrInvalidCode) {
		t.Fatalf("invalid code: %v", err)
	}
}

func framesOf(r *futusim.Recorder, protoID uint32) int {
	n := 0
	for _, step := range r.Steps() {
		if step.Direction == futusim.DirectionIn && futu.FrameProtocol(step.Raw) == protoID {
			n++
		}
	}
	return n
}

// The client's pacer and the simulator read one table: the client refuses
// request 61 itself and the simulator never saw a request to refuse, while a
// raw client past the same number is refused by the simulator at that number.
func TestThePacerAndTheSimulatorAgree(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := clocked(t, srv, simAccount)
	limit, _ := futu.LimitFor(futu.ProtoQotGetSecuritySnapshot)
	for i := range limit.Requests {
		if _, err := c.Snapshots(bg, "HK.00700"); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	_, err := c.Snapshots(bg, "HK.00700")
	if !errors.Is(err, futu.ErrPaced) {
		t.Fatalf("request past the window: %v", err)
	}
	if srv.Refusals(futu.ProtoQotGetSecuritySnapshot) != 0 {
		t.Fatal("the simulator had to refuse what the client should have")
	}

	// Past the window the client admits again.
	srv.Book().Clock.Advance(limit.Window)
	if _, err := c.Snapshots(bg, "HK.00700"); err != nil {
		t.Fatalf("after the window: %v", err)
	}

	raw := futusim.Start(t, futusim.Options{})
	nc, err := net.Dial("tcp", raw.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	for i := range limit.Requests + 1 {
		if _, err := nc.Write(futu.FixtureRequest("qot_snapshot", uint32(i+1), time.Time{})); err != nil {
			t.Fatal(err)
		}
		if _, err := futu.ReadFrame(nc); err != nil {
			t.Fatal(err)
		}
	}
	if got := raw.Refusals(futu.ProtoQotGetSecuritySnapshot); got != 1 {
		t.Fatalf("the simulator refused %d of %d requests", got, limit.Requests+1)
	}
}

// History pages until OpenD has no more; a repeat inside the seven-day window
// costs no quota; only the first page spends the pacing window.
func TestHistoryPagesAndARepeatCostsNoQuota(t *testing.T) {
	recorder := &futusim.Recorder{}
	srv := futusim.Start(t, futusim.Options{Record: recorder})
	c := clocked(t, srv, simAccount)
	zone := time.FixedZone("HKT", 8*3600)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, zone)
	to := time.Date(2026, 10, 31, 23, 59, 59, 0, zone)

	bars, err := c.HistoryBars(bg, "HK.00700", futu.Bar1Min, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) < 2000 {
		t.Fatalf("%d one-minute bars in a month", len(bars))
	}
	pages := framesOf(recorder, futu.ProtoQotRequestHistoryKL)
	if pages < 2 {
		t.Fatalf("%d pages for %d bars", pages, len(bars))
	}
	for i := 1; i < len(bars); i++ {
		if !bars[i].Time.After(bars[i-1].Time) {
			t.Fatalf("bar %d does not follow its predecessor: %s then %s", i, bars[i-1].Time, bars[i].Time)
		}
	}
	first := bars[0]
	if first.Blank || first.Open <= 0 || first.Close <= 0 || first.High < first.Low || first.Volume <= 0 || first.Text == "" {
		t.Fatalf("first bar %+v", first)
	}

	load, err := c.RefreshLoad(bg)
	if err != nil || load.HistoryUsed != 1 || load.HistoryRemaining != 99 || load.HistoryQuota != 100 {
		t.Fatalf("after the first read: %+v %v", load, err)
	}
	if _, err := c.HistoryBars(bg, "HK.00700", futu.BarDay, from, to); err != nil {
		t.Fatal(err)
	}
	if load, _ := c.RefreshLoad(bg); load.HistoryUsed != 1 {
		t.Fatalf("a repeat inside the window cost quota: %+v", load)
	}
	daily, err := c.HistoryBars(bg, "HK.00700", futu.BarDay, from, to)
	if err != nil || len(daily) < 20 || len(daily) > 23 {
		t.Fatalf("daily bars in October: %d %v", len(daily), err)
	}
	if weekly, err := c.HistoryBars(bg, "HK.00700", futu.BarWeek, from, to); err != nil || len(weekly) != 5 {
		t.Fatalf("weekly bars: %d %v", len(weekly), err)
	}
	if monthly, err := c.HistoryBars(bg, "HK.00700", futu.BarMonth, from, to); err != nil || len(monthly) != 1 {
		t.Fatalf("monthly bars: %d %v", len(monthly), err)
	}

	srv.Book().SetHistoryQuota(1)
	_, err = c.HistoryBars(bg, "US.AAPL", futu.BarDay, from, to)
	if _, refused := errors.AsType[*futu.Error](err); !refused {
		t.Fatalf("history past the quota: %v", err)
	}
	if _, err := c.HistoryBars(bg, "bad", futu.BarDay, from, to); !errors.Is(err, futu.ErrInvalidCode) {
		t.Fatalf("invalid code: %v", err)
	}
	if _, err := c.HistoryBars(bg, "HK.00700", futu.BarPeriod("9y"), from, to); err == nil {
		t.Fatal("an unknown period was accepted")
	}
}

// Recent candles need a candle line, and the line costs quota.
func TestRecentBarsNeedACandleLine(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := clocked(t, srv, simAccount)
	if _, err := c.RecentBars(bg, "HK.00700", futu.Bar5Min, 10); err == nil {
		t.Fatal("candles without a line")
	}
	if err := c.SubscribeBars(bg, "HK.00700", futu.Bar5Min); err != nil {
		t.Fatal(err)
	}
	if used, _ := srv.Book().SubscriptionUsage(); used != 1 {
		t.Fatalf("quota used %d", used)
	}
	srv.Book().Clock.Advance(3 * time.Hour)
	bars, err := c.RecentBars(bg, "HK.00700", futu.Bar5Min, 10)
	if err != nil || len(bars) != 10 {
		t.Fatalf("recent: %d %v", len(bars), err)
	}
	if err := c.SubscribeBars(bg, "HK.00700", futu.BarPeriod("9y")); err == nil {
		t.Fatal("an unknown period was accepted")
	}
	if err := c.SubscribeBars(bg, "bad", futu.Bar5Min); !errors.Is(err, futu.ErrInvalidCode) {
		t.Fatalf("invalid code: %v", err)
	}
	if _, err := c.RecentBars(bg, "bad", futu.Bar5Min, 1); !errors.Is(err, futu.ErrInvalidCode) {
		t.Fatalf("invalid code: %v", err)
	}
	if _, err := c.RecentBars(bg, "HK.00700", futu.BarPeriod("9y"), 1); err == nil {
		t.Fatal("an unknown period was accepted")
	}
}

func TestInstrumentLookup(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := clocked(t, srv, simAccount)
	got, err := c.Instruments(bg, "HK.00700", "US.SPY")
	if err != nil || len(got) != 2 {
		t.Fatalf("instruments %+v %v", got, err)
	}
	if got[0].Code != "HK.00700" || got[0].LotSize != 100 || got[0].Type != "Eqty" || got[0].Name != "SYNTH TENCENT" || got[0].Listed != "2004-06-16" {
		t.Fatalf("first %+v", got[0])
	}
	if got[1].Type != "Trust" || got[1].LotSize != 1 {
		t.Fatalf("second %+v", got[1])
	}
	if _, err := c.Instruments(bg, "HK.99999"); err == nil {
		t.Fatal("an unknown instrument resolved")
	}
	if _, err := c.Instruments(bg, "bad"); !errors.Is(err, futu.ErrInvalidCode) {
		t.Fatalf("invalid code: %v", err)
	}
}

// Funds value the whole account in the first market's currency; a cached read
// is not paced and a refresh is.
func TestFundsReadTheBookAndOnlyARefreshIsPaced(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := clocked(t, srv, simAccount)
	waitReady(t, c)
	funds, err := c.Funds(bg, false)
	if err != nil {
		t.Fatal(err)
	}
	view, err := srv.Book().Funds(simAccount, "HKD")
	if err != nil {
		t.Fatal(err)
	}
	if funds.Currency != "HKD" || funds.TotalAssets != view.TotalAssets || funds.Cash != view.Cash || funds.MarketValue != view.MarketValue {
		t.Fatalf("funds %+v view %+v", funds, view)
	}
	if funds.InitialMargin == nil || funds.MaintenanceMargin == nil || funds.AvailableFunds == nil || funds.UnrealizedPnL == nil {
		t.Fatalf("optional fields missing: %+v", funds)
	}
	if funds.RealizedPnL != nil {
		t.Fatal("a field OpenD did not send must stay nil, never zero")
	}
	if funds.RiskStatus != "Level1" || len(funds.CashRows) != 3 || funds.CashRows[0].Currency != "HKD" || funds.CashRows[0].Cash == nil {
		t.Fatalf("risk and cash rows %+v", funds)
	}

	limit, _ := futu.LimitFor(futu.ProtoTrdGetFunds)
	for range limit.Requests + 5 {
		if _, err := c.Funds(bg, false); err != nil {
			t.Fatalf("a cached read was paced: %v", err)
		}
	}
	for i := range limit.Requests {
		if _, err := c.Funds(bg, true); err != nil {
			t.Fatalf("refresh %d: %v", i+1, err)
		}
	}
	if _, err := c.Funds(bg, true); !errors.Is(err, futu.ErrPaced) {
		t.Fatalf("refresh past the window: %v", err)
	}
	if srv.Refusals(futu.ProtoTrdGetFunds) != 0 {
		t.Fatal("the simulator had to refuse what the client should have")
	}
}

// Every trade-class read refuses typed before the list reported the pin.
func TestTradeReadsRefuseWithoutAScope(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := connect(t, srv, 42)
	waitFor(t, "the unscoped phase", func() bool { return c.Health().Phase == futu.PhaseUnscoped })
	if _, err := c.Funds(bg, false); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("funds: %v", err)
	}
	if _, err := c.Positions(bg, false); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("positions: %v", err)
	}
	if _, err := c.Orders(bg, false); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("orders: %v", err)
	}
	if _, err := c.Fills(bg, false); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("fills: %v", err)
	}
	if _, err := c.HistoryOrders(bg, "2026-11-01 00:00:00", "2026-11-05 00:00:00"); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("history orders: %v", err)
	}
	if _, err := c.HistoryFills(bg, "2026-11-01 00:00:00", "2026-11-05 00:00:00"); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("history fills: %v", err)
	}
	if _, err := c.MaxQuantities(bg, "HK.00700", 1); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("max quantities: %v", err)
	}
}

func TestPositionsSpanEveryAuthorisedMarket(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := clocked(t, srv, simAccount)
	waitReady(t, c)
	positions, err := c.Positions(bg, false)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]futu.Position{}
	for _, p := range positions {
		byCode[p.Code] = p
	}
	if len(byCode) != 3 {
		t.Fatalf("positions %+v", positions)
	}
	hk, us, sg := byCode["HK.00700"], byCode["US.AAPL"], byCode["SG.ES3"]
	if hk.Quantity != 300 || hk.Currency != "HKD" || hk.AverageCost == nil || *hk.AverageCost != 398.4 || hk.Price <= 0 || hk.Value != hk.Price*300 || hk.ID == 0 {
		t.Fatalf("HK %+v", hk)
	}
	if us.Quantity != 25 || us.Currency != "USD" || sg.Quantity != 1300 || sg.Currency != "SGD" {
		t.Fatalf("US %+v SG %+v", us, sg)
	}
	if hk.UnrealizedPnL == nil || hk.RealizedPnL == nil {
		t.Fatalf("P&L %+v", hk)
	}

	// A short line reads negative.
	if _, _, err := srv.Book().PlaceOrder(simAccount, "HK.00700", futusim.Sell, 500, nil); err != nil {
		t.Fatal(err)
	}
	positions, _ = c.Positions(bg, true)
	for _, p := range positions {
		if p.Code == "HK.00700" && p.Quantity != -200 {
			t.Fatalf("short line %+v", p)
		}
	}
}

func TestOrdersFillsAndTheirPushes(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	var orders, fills atomic.Int32
	var lastOrder atomic.Value
	c := futu.NewConnector(futu.Config{
		Addr:          srv.Addr(),
		Account:       simAccount,
		ClientVersion: 333,
		OnOrder: func(o futu.Order) {
			lastOrder.Store(o)
			orders.Add(1)
		},
		OnFill: func(futu.Fill) { fills.Add(1) },
	})
	c.Start(bg)
	t.Cleanup(c.Stop)
	waitReady(t, c)

	order, placed, err := srv.Book().PlaceOrder(simAccount, "HK.00700", futusim.Buy, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	limit := 1.0
	resting, _, err := srv.Book().PlaceOrder(simAccount, "US.AAPL", futusim.Buy, 1, &limit)
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.Orders(bg, false)
	if err != nil || len(list) != 2 {
		t.Fatalf("orders %+v %v", list, err)
	}
	if list[0].ID != order.ID || list[0].Code != "HK.00700" || list[0].Side != "Buy" || list[0].Type != "Market" || list[0].Status != "Filled_All" || list[0].Quantity != 100 || list[0].Currency != "HKD" || list[0].CreatedAt == "" {
		t.Fatalf("first order %+v", list[0])
	}
	if list[1].ID != resting.ID || list[1].Type != "Normal" || list[1].Status != "Submitted" || list[1].Price == nil || *list[1].Price != 1 {
		t.Fatalf("resting order %+v", list[1])
	}
	fillRows, err := c.Fills(bg, true)
	if err != nil || len(fillRows) != 1 || fillRows[0].OrderID != order.ID || fillRows[0].Quantity != 100 || fillRows[0].Code != "HK.00700" || fillRows[0].Side != "Buy" || fillRows[0].Price <= 0 {
		t.Fatalf("fills %+v %v", fillRows, err)
	}

	from, to := "2026-11-01 00:00:00", "2026-11-05 00:00:00"
	past, err := c.HistoryOrders(bg, from, to)
	if err != nil || len(past) != 2 {
		t.Fatalf("history orders %+v %v", past, err)
	}
	pastFills, err := c.HistoryFills(bg, from, to)
	if err != nil || len(pastFills) != 1 {
		t.Fatalf("history fills %+v %v", pastFills, err)
	}
	if before, err := c.HistoryOrders(bg, "2020-01-01 00:00:00", "2020-01-02 00:00:00"); err != nil || len(before) != 0 {
		t.Fatalf("orders outside the window %+v %v", before, err)
	}
	if before, err := c.HistoryFills(bg, "2020-01-01 00:00:00", "2020-01-02 00:00:00"); err != nil || len(before) != 0 {
		t.Fatalf("fills outside the window %+v %v", before, err)
	}

	// Pushes reach the callbacks once the session asked for them.
	waitFor(t, "the account push subscription", func() bool {
		srv.PushOrder(order)
		srv.PushFill(placed[0])
		time.Sleep(5 * time.Millisecond)
		return orders.Load() > 0 && fills.Load() > 0
	})
	if o := lastOrder.Load().(futu.Order); o.ID != order.ID || o.Code != "HK.00700" {
		t.Fatalf("pushed order %+v", o)
	}

	// A push for another account is dropped: this session only asked for the pin.
	otherOrders := orders.Load()
	other, err := srv.Book().Orders(realAccount)
	if err != nil || len(other) != 0 {
		t.Fatalf("other orders %+v %v", other, err)
	}
	srv.PushOrder(futusim.Order{AccountID: realAccount, Code: "HK.02800", Side: futusim.Buy, Quantity: 500, Status: futusim.OrderOpen})
	time.Sleep(20 * time.Millisecond)
	if orders.Load() != otherOrders {
		t.Fatal("a push for another account reached the callback")
	}
}

func TestMaxQuantitiesReadsCapacityAndCashAccountsAreRefused(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := clocked(t, srv, simAccount)
	waitReady(t, c)
	got, err := c.MaxQuantities(bg, "HK.00700", 400)
	if err != nil {
		t.Fatal(err)
	}
	if got.CashBuy <= 0 || got.CashAndMarginBuy == nil || *got.CashAndMarginBuy < got.CashBuy || got.PositionSell != 300 || got.SellShort == nil || got.BuyBack == nil {
		t.Fatalf("max quantities %+v", got)
	}
	if got.LongRequiredIM != nil {
		t.Fatal("a field OpenD did not send must stay nil")
	}
	if int64(got.CashBuy)%100 != 0 {
		t.Fatalf("not whole lots: %v", got.CashBuy)
	}
	if _, err := c.MaxQuantities(bg, "bad", 1); !errors.Is(err, futu.ErrInvalidCode) {
		t.Fatalf("invalid code: %v", err)
	}
	if _, err := c.MaxQuantities(bg, "AU.BHP", 1); !errors.Is(err, futu.ErrMarketNotAuthorised) {
		t.Fatalf("unauthorised market: %v", err)
	}
	if _, err := c.MaxQuantities(bg, "HK.00700", 0); err == nil {
		t.Fatal("a zero price was answered")
	}

	cash := clocked(t, srv, realAccount)
	waitReady(t, cash)
	var reply *futu.Error
	if _, err := cash.MaxQuantities(bg, "HK.02800", 23); !errors.As(err, &reply) {
		t.Fatalf("cash account: %v", err)
	}
	if reply.Session() {
		t.Fatal("a cash refusal must not end the session")
	}
	if !cash.IsReady() {
		t.Fatal("the session ended on a request-level refusal")
	}

	limit, _ := futu.LimitFor(futu.ProtoTrdGetMaxTrdQtys)
	// The good read and the zero price spent two of the window.
	for i := range limit.Requests - 2 {
		if _, err := c.MaxQuantities(bg, "HK.00700", 400); err != nil {
			t.Fatalf("request %d: %v", i+3, err)
		}
	}
	if _, err := c.MaxQuantities(bg, "HK.00700", 400); !errors.Is(err, futu.ErrPaced) {
		t.Fatalf("past the window: %v", err)
	}
}

// A simulator read with a header its list could not have given is refused.
func TestSimulatorRefusesAForeignHeader(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	nc, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := nc.Write(futu.FixtureRequest("trd_max_cash_wrong_env", 1, time.Time{})); err != nil {
		t.Fatal(err)
	}
	if _, err := futu.ReadFrame(nc); err != nil {
		t.Fatal(err)
	}
	if srv.Refusals(futu.ProtoTrdGetMaxTrdQtys) != 1 {
		t.Fatalf("refusals %d", srv.Refusals(futu.ProtoTrdGetMaxTrdQtys))
	}
}
