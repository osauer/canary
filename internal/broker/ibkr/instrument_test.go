package ibkr

import (
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// daemonContracts is one contract per security type the daemon subscribes,
// resolves or reads positions for, with every identity field set the way
// the daemon sets it.
var daemonContracts = []ibkrlib.Contract{
	{ConID: 265598, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", LocalSymbol: "AAPL", TradingClass: "NMS"},
	{Symbol: "SPY", SecType: "STK"},
	{ConID: 123, Symbol: "SPY", SecType: "OPT", Expiry: "20261219", Strike: 450, Right: "P", Multiplier: 100, Exchange: "SMART", Currency: "USD", TradingClass: "SPY", LocalSymbol: "SPY   261219P00450000"},
	{ConID: 456, Symbol: "ES", SecType: "FUT", Expiry: "202612", Multiplier: 50, Exchange: "CME", Currency: "USD", LocalSymbol: "ESZ6"},
	{ConID: 13455763, Symbol: "VIX", SecType: "IND", Exchange: "CBOE", Currency: "USD"},
	{ConID: 789, Symbol: "DE000SYNTH01", SecType: "BOND", Exchange: "SMART", Currency: "EUR", SecIDType: "ISIN", SecID: "DE000SYNTH01"},
	{ConID: 790, Symbol: "912797SYN", SecType: "BILL", Exchange: "SMART", Currency: "USD", SecIDType: "CUSIP", SecID: "912797SYN"},
	{ConID: 12087792, Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD", LocalSymbol: "EUR.USD"},
	{Symbol: "SPY", SecType: "BAG", Exchange: "SMART", Currency: "USD", ComboLegs: []ibkrlib.ComboLeg{{ConID: 1, Ratio: 1, Action: "BUY", Exchange: "SMART"}, {ConID: 2, Ratio: 1, Action: "SELL", Exchange: "SMART", OpenClose: 1}}},
	{Symbol: "SAP", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR"},
	{Symbol: "7203", SecType: "STK", Exchange: "TSEJ", Currency: "JPY"},
}

func TestInstrumentMappingIsTotalForEverySecType(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range daemonContracts {
		seen[c.SecType] = true
		in := Instrument(c)
		if in.Broker != broker.KindIBKR {
			t.Errorf("%s: broker %q", c.SecType, in.Broker)
		}
		back := Contract(in)
		if !reflect.DeepEqual(back, c) {
			t.Errorf("%s: contract round trip differs:\n got %+v\nwant %+v", c.SecType, back, c)
		}
		if got, want := ibkrlib.MarketDataKeyForContract(back), ibkrlib.MarketDataKeyForContract(c); got != want {
			t.Errorf("%s: market-data key moved: %q != %q", c.SecType, got, want)
		}
		again := Instrument(back)
		if !reflect.DeepEqual(again, in) {
			t.Errorf("%s: instrument round trip differs:\n got %+v\nwant %+v", c.SecType, again, in)
		}
	}
	for _, want := range []string{"STK", "OPT", "FUT", "IND", "BOND", "BILL", "CASH", "BAG"} {
		if !seen[want] {
			t.Errorf("table has no %s contract", want)
		}
	}
}

func TestInstrumentIDIsTheConIDAsText(t *testing.T) {
	in := Instrument(ibkrlib.Contract{ConID: 265598, Symbol: "AAPL", SecType: "STK"})
	if in.ID != "265598" || in.String() != "ibkr:265598" {
		t.Fatalf("got id %q string %q", in.ID, in.String())
	}
	if Contract(broker.Instrument{ID: "HK.00700", Symbol: "00700"}).ConID != 0 {
		t.Fatal("a foreign code must not become a ConID")
	}
	if Contract(broker.Instrument{Symbol: "SPY"}).ConID != 0 {
		t.Fatal("no id, no ConID")
	}
}

func TestQuoteMapping(t *testing.T) {
	at := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	md := &ibkrlib.MarketData{FeedType: 3, Symbol: "AAPL", Timestamp: at, Bid: 100, Ask: 101, Last: 100.5, Mid: 100.5, MarkPrice: 100.4,
		Open: 99, High: 102, Low: 98, Close: 99.5, VWAP: 100.2, Volume: 12345, VolumeObserved: true, VolumeAt: at, AvgVolume: 9999,
		BidSize: 3, AskSize: 4, LastTickAt: at, LastPriceTickAt: at, LastTradeTime: at, LastAt: at, BidAt: at, AskAt: at, MarkAt: at, CloseAt: at}
	q := Quote(md)
	want := broker.Quote{FeedType: 3, Symbol: "AAPL", Timestamp: at, Bid: 100, Ask: 101, Last: 100.5, Mid: 100.5, MarkPrice: 100.4,
		Open: 99, High: 102, Low: 98, Close: 99.5, VWAP: 100.2, Volume: 12345, VolumeObserved: true, VolumeAt: at, AvgVolume: 9999,
		BidSize: 3, AskSize: 4, LastTickAt: at, LastPriceTick: at, LastTradeTime: at, LastAt: at, BidAt: at, AskAt: at, MarkAt: at, CloseAt: at}
	if q != want {
		t.Fatalf("quote mapping:\n got %+v\nwant %+v", q, want)
	}
	if (Quote(nil) != broker.Quote{}) {
		t.Fatal("nil market data maps to the zero quote")
	}
}

func TestBarMapping(t *testing.T) {
	at := time.Date(2026, 11, 4, 14, 30, 0, 0, time.UTC)
	hb := ibkrlib.HistoricalBar{Time: at, Date: "20261104 14:30:00", Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 10, Average: 1.2, BarCount: -1}
	if got := Bar(hb); got != (broker.Bar{Time: at, Date: "20261104 14:30:00", Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 10, Average: 1.2, BarCount: -1}) {
		t.Fatalf("bar mapping: %+v", got)
	}
	if Bars(nil) != nil {
		t.Fatal("nil bars stay nil")
	}
	if got := Bars([]ibkrlib.HistoricalBar{}); got == nil || len(got) != 0 {
		t.Fatal("empty bars stay empty, not nil")
	}
	s := Series(ibkrlib.ChartSeries{Contract: daemonContracts[0], Bars: []ibkrlib.HistoricalBar{hb}, WhatToShow: "TRADES"})
	if s.Instrument.ID != "265598" || len(s.Bars) != 1 || s.WhatToShow != "TRADES" {
		t.Fatalf("series mapping: %+v", s)
	}
}

func TestAdapterWithoutConnectorIsUnavailable(t *testing.T) {
	var a *Adapter
	if _, ok := a.Capture(); ok {
		t.Fatal("nil adapter captures nothing")
	}
	if a.Current(broker.SessionBinding{Kind: broker.KindIBKR, Generation: 1, Epoch: 1}) {
		t.Fatal("nil adapter has no current session")
	}
	a = New(nil, 1, nil)
	if _, ok := a.Capture(); ok {
		t.Fatal("adapter over a nil connector captures nothing")
	}
	if a.Kind() != broker.KindIBKR || a.Generation() != 1 || a.Connector() != nil {
		t.Fatal("identity")
	}
	if _, _, err := a.SubscribeSymbol(t.Context(), "SPY", nil); err == nil {
		t.Fatal("subscribe without a connector must fail")
	}
	if _, err := a.ChartBars(t.Context(), broker.Instrument{Symbol: "SPY"}, 1, "1 day", time.Second); err == nil {
		t.Fatal("chart read without a connector must fail")
	}
	if _, err := a.Resolve(t.Context(), broker.SessionBinding{}, broker.Instrument{Symbol: "SPY"}, time.Second); err == nil {
		t.Fatal("resolve with a zero binding must fail")
	}
	if a.Snapshot() != nil {
		t.Fatal("no connector, no snapshot")
	}
}
