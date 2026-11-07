package opend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

const (
	simAccount  = 1000001
	realAccount = 1000002
)

func connected(t *testing.T, sim *futusim.Server, account uint64) *Adapter {
	t.Helper()
	conn := futu.NewConnector(futu.Config{Addr: sim.Addr(), Account: account, ClientVersion: 333, RedialDelay: 20 * time.Millisecond, Now: sim.Book().Clock.Now})
	conn.Start(context.Background())
	t.Cleanup(conn.Stop)
	waitUntil(t, "a ready session", conn.IsReady)
	return New(conn, sim.Book().Clock.Now)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// unlisted connects an account the list does not report; it never gets ready.
func unlisted(t *testing.T, sim *futusim.Server) *Adapter {
	t.Helper()
	conn := futu.NewConnector(futu.Config{Addr: sim.Addr(), Account: 42, ClientVersion: 333})
	conn.Start(context.Background())
	t.Cleanup(conn.Stop)
	waitUntil(t, "the unscoped phase", func() bool { return conn.Health().Phase == futu.PhaseUnscoped })
	return New(conn, nil)
}

func TestScopeIsThePinWithTheEnvironmentTheListReported(t *testing.T) {
	idle := New(futu.NewConnector(futu.Config{Addr: "127.0.0.1:1", Account: simAccount, ClientVersion: 333}), nil)
	if got := idle.Scope(); got.Account != "1000001" || got.Mode != modeUnknown || got.Kind != broker.KindOpenD {
		t.Fatalf("before any session %+v", got)
	}
	sim := futusim.Start(t, futusim.Options{})
	if got := connected(t, sim, simAccount).Scope(); got.Mode != modePaper {
		t.Fatalf("simulate %+v", got)
	}
	if got := connected(t, sim, realAccount).Scope(); got.Mode != modeLive {
		t.Fatalf("real %+v", got)
	}
}

func TestSessionFollowsTheConnector(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	a := connected(t, sim, simAccount)
	if a.Kind() != broker.KindOpenD || a.Connector() == nil {
		t.Fatal("kind or connector")
	}
	binding, ok := a.Capture()
	if !ok || binding.Kind != broker.KindOpenD || binding.Generation != generation || binding.Epoch == 0 || !a.Current(binding) {
		t.Fatalf("binding %+v %v", binding, ok)
	}
	if a.Current(broker.SessionBinding{}) || a.Current(broker.SessionBinding{Kind: broker.KindIBKR, Generation: generation, Epoch: binding.Epoch}) {
		t.Fatal("a foreign binding is current")
	}
	h := a.Health()
	if !h.Connected || !h.Ready || h.BackendDown || h.Account != "1000001" || h.ServerVersion != int(futu.MinServerVersion) || h.LastError != "" {
		t.Fatalf("health %+v", h)
	}

	sim.SetLoggedIn(false, true)
	waitUntil(t, "the logged-out link", func() bool { return a.Health().BackendDown })
	if a.Current(binding) {
		t.Fatalf("a logged-out OpenD is current: %+v", a.Health())
	}
	if _, ok := a.Capture(); ok {
		t.Fatal("a session out of ready was captured")
	}
	sim.SetLoggedIn(true, true)
	waitUntil(t, "the login back", a.Connector().IsReady)

	sim.KickOut()
	waitUntil(t, "the reason", func() bool { return a.Health().LastError != "" })
	if !errors.Is(a.Connector().Health().LastError, futu.ErrSessionEnded) {
		t.Fatalf("end %v", a.Connector().Health().LastError)
	}
}

func TestSummaryMapsFundsAndNilStaysNil(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	a := connected(t, sim, simAccount)
	raw, provenance, err := a.Summary(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	view, _ := sim.Book().Funds(simAccount, "HKD")
	if provenance != broker.ProvenanceRequest || raw.AccountID != "1000001" || raw.AccountType != "Margin" || *raw.NetLiquidation != view.TotalAssets || *raw.TotalCashValue != view.Cash || *raw.GrossPositionValue != view.MarketValue {
		t.Fatalf("summary %+v, book %+v", raw, view)
	}
	if raw.BaseCurrency != "HKD" || !raw.BaseCurrencyProvenance.Proven() || raw.AsOf.IsZero() || len(raw.CurrencyLedger) != 3 || raw.CurrencyLedger["USD"].CashBalance != view.ByCurrency["USD"] {
		t.Fatalf("currency %+v", raw)
	}
	if raw.RealizedPnL != nil || raw.ExcessLiquidity != nil || raw.Cushion != nil {
		t.Fatal("a field OpenD did not send must stay nil")
	}
	if raw.Raw["RiskStatus"] != "Level1" {
		t.Fatalf("raw %v", raw.Raw)
	}
	if _, _, err := unlisted(t, sim).Summary(context.Background(), time.Second); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("unscoped: %v", err)
	}
}

func TestPositionsMapRowsAndTheirReceipt(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	a := connected(t, sim, simAccount)
	rows, health, err := a.Positions()
	if err != nil || len(rows) != 3 {
		t.Fatalf("positions %+v %v", rows, err)
	}
	if health.Account != "1000001" || health.InitialCompletedAt.IsZero() || health.LastUpdateAt.IsZero() || health.RequestedAt.IsZero() {
		t.Fatalf("receipt %+v", health)
	}
	for _, row := range rows {
		if row.Account != "1000001" || row.Quantity <= 0 || row.MarketPrice <= 0 || row.MarketValue <= 0 || row.AverageCost <= 0 || row.ValuationAt.IsZero() {
			t.Fatalf("row %+v", row)
		}
		in := row.Instrument
		if in.Broker != broker.KindOpenD || in.ID == "" || in.Symbol == "" || in.Venue == "" || in.Market == "" || in.Currency == "" {
			t.Fatalf("instrument %+v", in)
		}
	}
	if _, _, err := unlisted(t, sim).Positions(); !errors.Is(err, futu.ErrNoScope) {
		t.Fatalf("unscoped: %v", err)
	}
}

// A code is found by the instrument's own id, its venue, market or currency, then
// by the bare symbol rule; what OpenD's seam does not serve is refused typed.
func TestCodeOfFindsTheMarket(t *testing.T) {
	for _, tc := range []struct {
		in   broker.Instrument
		want string
	}{
		{broker.Instrument{ID: "SG.D05", Broker: broker.KindOpenD}, "SG.D05"},
		{broker.Instrument{Symbol: "hk.00700"}, "HK.00700"},
		{broker.Instrument{Symbol: "D05", Venue: "SGX"}, "SG.D05"},
		{broker.Instrument{Symbol: "D05", Primary: "SGX"}, "SG.D05"},
		{broker.Instrument{Symbol: "D05", Market: "sg"}, "SG.D05"},
		{broker.Instrument{Symbol: "D05", Currency: "SGD"}, "SG.D05"},
		{broker.Instrument{Symbol: "00700"}, "HK.00700"},
		{broker.Instrument{Symbol: "aapl"}, "US.AAPL"},
		{broker.Instrument{Symbol: "AAPL", Venue: "SMART", Currency: "USD"}, "US.AAPL"},
		{broker.Instrument{Symbol: "SPY", SecType: "ETF"}, "US.SPY"},
		{broker.Instrument{Broker: broker.KindIBKR, ID: "265.598", Symbol: "AAPL"}, "US.AAPL"},
	} {
		got, err := codeOf(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%+v: %s %v, want %s", tc.in, got, err, tc.want)
		}
	}
	for _, sec := range []string{"OPT", "FUT", "BOND", "BAG", "CASH"} {
		if _, err := codeOf(broker.Instrument{Symbol: "SPY", SecType: sec}); !errors.Is(err, ErrNotServed) {
			t.Errorf("%s: %v", sec, err)
		}
	}
	if _, err := codeOf(broker.Instrument{}); !errors.Is(err, futu.ErrInvalidCode) {
		t.Errorf("no symbol: %v", err)
	}
	a := New(nil, nil)
	if a.Key(broker.Instrument{Symbol: "SPY", SecType: "OPT"}) != "" || a.Key(broker.Instrument{Symbol: "AAPL"}) != "US.AAPL" {
		t.Error("the subscription key")
	}
}

func TestStreamingLinesAndTheirHold(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	a := connected(t, sim, simAccount)
	ctx := context.Background()
	key, binding, err := a.SubscribeSymbol(ctx, "00700", nil)
	if err != nil || key != "HK.00700" || !a.Current(binding) {
		t.Fatalf("by symbol: %s %+v %v", key, binding, err)
	}
	key2, _, err := a.Subscribe(ctx, broker.Instrument{Symbol: "D05", Venue: "SGX"}, nil)
	if err != nil || key2 != "SG.D05" {
		t.Fatalf("exact: %s %v", key2, err)
	}
	if _, _, err := a.Subscribe(ctx, broker.Instrument{Symbol: "SPY", SecType: "OPT"}, nil); !errors.Is(err, ErrNotServed) {
		t.Fatalf("an option: %v", err)
	}

	sim.Book().Clock.Advance(5 * time.Second)
	sim.PushQuotes()
	waitUntil(t, "the pushed lines", func() bool { return len(a.Snapshot()) == 2 })
	q := a.Snapshot()["HK.00700"]
	if q == nil || q.Last <= 0 || q.Delivery != string(futu.DeliveryStream) || q.Bid != 0 || q.Ask != 0 || !q.VolumeObserved || q.FeedType != 1 {
		t.Fatalf("line %+v", q)
	}

	snap, err := a.ReadSnapshots(ctx, []string{"US.AAPL"})
	if err != nil || snap["US.AAPL"] == nil || snap["US.AAPL"].Bid <= 0 || snap["US.AAPL"].Mid <= 0 || snap["US.AAPL"].Delivery != string(futu.DeliverySnapshot) {
		t.Fatalf("snapshot %+v %v", snap, err)
	}

	if _, held := errors.AsType[*broker.ReleaseHeldError](a.Unsubscribe(ctx, "HK.00700")); !held {
		t.Fatal("a release inside the hold was not refused")
	}
	sim.Book().Clock.Advance(futu.SubscriptionMinimumHold)
	if err := a.Unsubscribe(ctx, "HK.00700"); err != nil {
		t.Fatal(err)
	}
	if err := a.UnsubscribeForSession(ctx, binding, "SG.D05"); err != nil {
		t.Fatal(err)
	}
	if err := a.UnsubscribeForSession(ctx, broker.SessionBinding{}, "US.AAPL"); err != nil {
		t.Fatalf("a stale session's release is a no-op: %v", err)
	}

	if _, ok := a.Load(); ok {
		t.Fatal("a load before OpenD said anything")
	}
	if _, err := a.Connector().RefreshLoad(ctx); err != nil {
		t.Fatal(err)
	}
	load, ok := a.Load()
	if !ok || load.SubscriptionQuota != 100 || load.HistoryQuota != 100 {
		t.Fatalf("load %+v %v", load, ok)
	}
}

func TestBarsAndResolve(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	a := connected(t, sim, simAccount)
	ctx := context.Background()
	series, err := a.ChartBars(ctx, broker.Instrument{Symbol: "00700"}, 14, "1 day", 5*time.Second)
	if err != nil || len(series.Bars) < 5 || series.Instrument.ID != "HK.00700" || series.WhatToShow != "TRADES" {
		t.Fatalf("chart %+v %v", series, err)
	}
	if bar := series.Bars[0]; bar.Close <= 0 || bar.Volume <= 0 || bar.Time.IsZero() || bar.Date == "" {
		t.Fatalf("bar %+v", bar)
	}
	if _, err := a.ChartBars(ctx, broker.Instrument{Symbol: "00700"}, 1, "9 years", time.Second); err == nil {
		t.Fatal("an unknown interval was accepted")
	}
	if _, err := a.ChartBars(ctx, broker.Instrument{SecType: "OPT", Symbol: "x"}, 1, "1 day", time.Second); !errors.Is(err, ErrNotServed) {
		t.Fatalf("an option: %v", err)
	}
	to := sim.Book().Clock.Now()
	bars, err := a.SessionBars(ctx, broker.Instrument{Symbol: "AAPL"}, to.AddDate(0, 0, -7), to, 5*time.Second)
	if err != nil || len(bars) == 0 {
		t.Fatalf("session bars %d %v", len(bars), err)
	}
	if _, err := a.SessionBars(ctx, broker.Instrument{SecType: "OPT", Symbol: "x"}, to, to, time.Second); !errors.Is(err, ErrNotServed) {
		t.Fatalf("an option: %v", err)
	}

	binding, _ := a.Capture()
	resolved, err := a.Resolve(ctx, binding, broker.Instrument{Symbol: "SPY"}, time.Second)
	if err != nil || resolved.ID != "US.SPY" || resolved.Broker != broker.KindOpenD || resolved.Venue != "US" || resolved.Currency != "USD" {
		t.Fatalf("resolve %+v %v", resolved, err)
	}
	if _, err := a.Resolve(ctx, broker.SessionBinding{}, broker.Instrument{Symbol: "SPY"}, time.Second); !errors.Is(err, futu.ErrNotConnected) {
		t.Fatalf("a stale binding: %v", err)
	}
	if _, err := a.Resolve(ctx, binding, broker.Instrument{Symbol: "HK.99999"}, time.Second); err == nil {
		t.Fatal("an unknown instrument resolved")
	}
}

func TestListedNamesTheExchangeAndReadsItOnce(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	a := connected(t, sim, simAccount)
	ctx := t.Context()
	in := []broker.Instrument{
		{Symbol: "AAPL", Venue: "US"},
		{Symbol: "SPY", SecType: "ETF", Venue: "US"},
		{ID: "HK.00700", Broker: broker.KindOpenD},
		{Symbol: "D05", Venue: "SGX"},
		{Symbol: "x", SecType: "OPT"},
	}
	got := a.Listed(ctx, in)
	for i, want := range []struct{ id, venue string }{{"US.AAPL", "NASDAQ"}, {"US.SPY", "AMEX"}, {"HK.00700", "SEHK"}, {"SG.D05", "SGX"}, {"", ""}} {
		if got[i].ID != want.id || got[i].Venue != want.venue {
			t.Errorf("%d: %+v, want %+v", i, got[i], want)
		}
	}
	// The exchange was read: with OpenD gone it still answers.
	sim.Close()
	waitUntil(t, "the session to drop", func() bool { return !a.conn.IsReady() })
	if again := a.Listed(ctx, in[:1]); again[0].Venue != "NASDAQ" {
		t.Fatalf("not kept: %+v", again[0])
	}
}

// A US name OpenD has not named has no join, so it has no identity rather than
// the family "US", which matches no other broker.
func TestListedGivesNoIdentityWithoutTheExchange(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	a := connected(t, sim, simAccount)
	if got := a.Listed(t.Context(), []broker.Instrument{{Symbol: "NOSUCH", Venue: "US"}}); got[0].ID != "" || got[0].Venue != "" {
		t.Fatalf("%+v", got[0])
	}
}

// Shanghai and Shenzhen share a market family and a currency. A digit symbol in
// that family reads by its venue, never as a Hong Kong code.
func TestCodeOfTellsTheChinaVenuesApart(t *testing.T) {
	for _, tc := range []struct {
		in   broker.Instrument
		want string
	}{
		{broker.Instrument{Symbol: "600519", Venue: "SSE"}, "SH.600519"},
		{broker.Instrument{Symbol: "000001", Primary: "SZSE"}, "SZ.000001"},
		{broker.Instrument{Symbol: "600519", Market: "cn"}, ""},
		{broker.Instrument{Symbol: "600519", Currency: "CNH"}, ""},
		{broker.Instrument{Symbol: "00700"}, "HK.00700"},
	} {
		got, err := codeOf(tc.in)
		if got != tc.want || (tc.want == "") != (err != nil) {
			t.Errorf("%+v: %q %v, want %q", tc.in, got, err, tc.want)
		}
	}
}

// bookWithContracts is the default book holding what OpenD's position rows do not
// tell apart from a stock.
func bookWithContracts() *futusim.Book {
	book := futusim.Default()
	book.AddHolding(futusim.Instrument{Code: "HK.TCH261230C400000", Name: "SYNTH CALL", Type: "option", LotSize: 100, Currency: "HKD", Start: 12.5, Tick: 0.01}, simAccount, 2, 10)
	book.AddHolding(futusim.Instrument{Code: "HK.HSI2612", Name: "SYNTH FUTURE", Type: "future", LotSize: 50, Currency: "HKD", Start: 25000, Tick: 1}, simAccount, 1, 24000)
	book.AddHolding(futusim.Instrument{Code: "HK.12345", Name: "SYNTH WARRANT", Type: "warrant", LotSize: 1000, Currency: "HKD", Start: 0.2, Tick: 0.001}, simAccount, 1000, 0.1)
	return book
}

// A position row names no type, so each code's type comes from OpenD's static
// information; a contract keeps the value OpenD gives it, multiplier included.
func TestPositionsTakeTheirTypeFromOpenD(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{Book: bookWithContracts()})
	a := connected(t, sim, simAccount)
	rows, _, err := a.Positions()
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, row := range rows {
		types[row.Instrument.ID] = row.Instrument.SecType
		if row.Instrument.ID == "HK.TCH261230C400000" && row.MarketValue != row.MarketPrice*2*100 {
			t.Errorf("option value %v at price %v", row.MarketValue, row.MarketPrice)
		}
	}
	want := map[string]string{"HK.00700": "STK", "US.AAPL": "STK", "SG.ES3": "STK", "HK.TCH261230C400000": "OPT", "HK.HSI2612": "FUT", "HK.12345": "WAR"}
	for code, secType := range want {
		if types[code] != secType {
			t.Errorf("%s reads %q, want %q", code, types[code], secType)
		}
	}
}

// A type that could not be read is unknown and read again next time, and one
// that was read does not need OpenD a second time.
func TestPositionsKeepAnUnreadTypeUnknown(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{Book: bookWithContracts()})
	a := connected(t, sim, simAccount)
	typeOf := func(code string) string {
		t.Helper()
		rows, _, err := a.Positions()
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Instrument.ID == code {
				return row.Instrument.SecType
			}
		}
		t.Fatalf("%s not held", code)
		return ""
	}
	sim.SetStaticInfoDown(true)
	if got := typeOf("HK.00700"); got != secTypeUnknown {
		t.Fatalf("with the lookup refused: %q", got)
	}
	sim.SetStaticInfoDown(false)
	if got := typeOf("HK.00700"); got != "STK" {
		t.Fatalf("after the refusal: %q", got)
	}
	sim.SetStaticInfoDown(true)
	if got := typeOf("HK.00700"); got != "STK" {
		t.Fatalf("a type already read: %q", got)
	}
}

// An identity resolved through OpenD says what the instrument is.
func TestResolveNamesTheTypeOpenDReports(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{Book: bookWithContracts()})
	a := connected(t, sim, simAccount)
	binding, ok := a.Capture()
	if !ok {
		t.Fatal("no session")
	}
	got, err := a.Resolve(t.Context(), binding, broker.Instrument{Broker: broker.KindOpenD, ID: "HK.TCH261230C400000"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecType != "OPT" {
		t.Fatalf("resolved %+v", got)
	}
}
