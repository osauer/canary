package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

// dispatchRaw runs one request through the real dispatcher.
func dispatchRaw(t *testing.T, srv *Server, method, params string) rpc.Response {
	t.Helper()
	var out bytes.Buffer
	srv.dispatch(t.Context(), &rpc.Request{ID: "wp10", Method: method, Params: json.RawMessage(params)}, json.NewEncoder(&out), bufio.NewReader(strings.NewReader("")))
	var res rpc.Response
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("%s: %s: %v", method, out.String(), err)
	}
	return res
}

// Every row with an identity carries the join: its symbol, and a listing venue.
func requireJoin(t *testing.T, where, symbol string, id rpc.InstrumentID) {
	t.Helper()
	if symbol == "" || id.Venue == "" || id.JoinKey(symbol) == "" {
		t.Errorf("%s: an identity without its join: symbol %q %+v", where, symbol, id)
	}
}

func TestOpenDRowsCarryAnIdentityAndTheJoin(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)

	res := dispatchRaw(t, srv, rpc.MethodPositionsList, `{}`)
	if !res.Ok {
		t.Fatalf("positions: %+v", res.Error)
	}
	var positions rpc.PositionsResult
	if err := json.Unmarshal(res.Result, &positions); err != nil {
		t.Fatal(err)
	}
	if len(positions.Stocks) == 0 {
		t.Fatal("the simulator holds names")
	}
	byCode := map[string]rpc.InstrumentID{}
	for _, row := range positions.Stocks {
		if row.Instrument.Broker != rpc.InstrumentBrokerOpenD || row.ConID != 0 {
			t.Errorf("%s: %+v con_id %d", row.Symbol, row.Instrument, row.ConID)
		}
		requireJoin(t, "position "+row.Symbol, row.Symbol, row.Instrument)
		byCode[row.Instrument.ID] = row.Instrument
	}
	if got := byCode["US.AAPL"]; got.Venue != "NASDAQ" || got.Market != "us" || got.Currency != "USD" {
		t.Fatalf("US.AAPL: %+v", got)
	}
	if got := byCode["HK.00700"]; got.Venue != "SEHK" || got.Market != "hk" || got.Currency != "HKD" {
		t.Fatalf("HK.00700: %+v", got)
	}
	for _, group := range positions.ByUnderlying {
		if group.Stock != nil {
			requireJoin(t, "group "+group.Stock.Symbol, group.Stock.Symbol, group.Stock.Instrument)
		}
	}

	// The same stock read by symbol, by venue and by identity is one identity
	// with one symbol, and the echo does not carry it back.
	for _, params := range []string{
		`{"contract":{"symbol":"00700","exchange":"SEHK"}}`,
		`{"contract":{"symbol":"HK.00700"}}`,
		`{"contract":{"instrument":{"broker":"opend","id":"HK.00700"}}}`,
		`{"contract":{"symbol":"700","instrument":{"broker":"opend","id":"HK.00700"}}}`,
	} {
		res := dispatchRaw(t, srv, rpc.MethodQuoteSnapshot, params)
		if !res.Ok {
			t.Fatalf("%s: %+v", params, res.Error)
		}
		var quote rpc.Quote
		if err := json.Unmarshal(res.Result, &quote); err != nil {
			t.Fatal(err)
		}
		if quote.Instrument != byCode["HK.00700"] || quote.Symbol != "00700" || quote.Contract.Instrument != (rpc.InstrumentID{}) || quote.Last == nil {
			t.Fatalf("%s: %+v", params, quote)
		}
		requireJoin(t, "quote", quote.Symbol, quote.Instrument)
	}

	// The bar history answers by symbol and by identity, and names its broker.
	for _, params := range []string{
		`{"contract":{"symbol":"00700","exchange":"SEHK","currency":"HKD"},"range":"1M"}`,
		`{"contract":{"instrument":{"broker":"opend","id":"HK.00700"}},"range":"1M"}`,
	} {
		res = dispatchRaw(t, srv, rpc.MethodMarketHistory, params)
		if !res.Ok {
			t.Fatalf("%s: %+v", params, res.Error)
		}
		var history rpc.MarketHistoryResult
		if err := json.Unmarshal(res.Result, &history); err != nil || len(history.Points) == 0 || !strings.HasPrefix(history.Source, "OpenD ") {
			t.Fatalf("%s: %s %v", params, history.Source, err)
		}
	}
}

// A request without the field resolves as it always did: the dispatcher leaves
// its bytes alone.
func TestARequestWithoutAnIdentityIsUntouched(t *testing.T) {
	srv := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	const params = `{"contract":{"symbol":"AAPL","con_id":265598},"timeout_ms":1}`
	req := &rpc.Request{Method: rpc.MethodQuoteSnapshot, Params: json.RawMessage(params)}
	if err := srv.resolveRequestInstrument(t.Context(), req); err != nil || string(req.Params) != params {
		t.Fatalf("%s %v", req.Params, err)
	}
}

func TestAnIdentityThatDisagreesIsRefused(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	for name, params := range map[string]string{
		"another broker's id": `{"contract":{"symbol":"AAPL","instrument":{"broker":"ibkr","id":"265598"}}}`,
		"another symbol":      `{"contract":{"symbol":"MSFT","instrument":{"broker":"opend","id":"US.AAPL"}}}`,
		"a con_id on OpenD":   `{"contract":{"con_id":265598,"instrument":{"broker":"opend","id":"US.AAPL"}}}`,
		"no such listing":     `{"contract":{"instrument":{"broker":"opend","id":"HK.99999"}}}`,
	} {
		if res := dispatchRaw(t, srv, rpc.MethodQuoteSnapshot, params); res.Ok {
			t.Errorf("%s was answered: %s", name, res.Result)
		}
	}
}

// The order paths never gain the identity: they refuse it rather than ignore it,
// so a symbol can never decide what an identity named. WP10 changes no broker path.
func TestOrderPathsRefuseAnIdentity(t *testing.T) {
	srv := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	const params = `{"contract":{"symbol":"AAPL","instrument":{"broker":"ibkr","id":"265598"}},"action":"BUY","quantity":1}`
	for _, method := range []string{rpc.MethodOrderPreview, rpc.MethodOrderPlace, rpc.MethodAddPreview, rpc.MethodQuoteSubscribe} {
		res := dispatchRaw(t, srv, method, params)
		if res.Ok || res.Error == nil || res.Error.Code != rpc.CodeBadRequest || !strings.Contains(res.Error.Message, "instrument") {
			t.Errorf("%s: %+v", method, res)
		}
	}
	// A string under the same key, as on a bond row, is no identity.
	req := &rpc.Request{Method: rpc.MethodOrderPreview, Params: json.RawMessage(`{"bond":{"instrument":"us_tbill"}}`)}
	if err := srv.resolveRequestInstrument(t.Context(), req); err != nil {
		t.Fatalf("bond row: %v", err)
	}
}

type fakeInstruments struct {
	got broker.Instrument
	out broker.Instrument
	err error
}

func (f *fakeInstruments) Resolve(_ context.Context, _ broker.SessionBinding, in broker.Instrument, _ time.Duration) (broker.Instrument, error) {
	f.got = in
	return f.out, f.err
}

func TestAnIBKRIdentityResolvesThroughTheBroker(t *testing.T) {
	reads := &fakeInstruments{out: broker.Instrument{Broker: broker.KindIBKR, ID: "265598", Symbol: "AAPL", SecType: "STK", Venue: "SMART", Primary: "NASDAQ", Currency: "USD"}}
	contract := rpc.ContractParams{Instrument: rpc.InstrumentID{Broker: "ibkr", ID: "265598"}}
	got, err := resolveContractInstrument(t.Context(), reads, broker.SessionBinding{}, broker.KindIBKR, contract)
	if err != nil {
		t.Fatal(err)
	}
	want := rpc.ContractParams{ConID: 265598, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	if got != want || reads.got.ID != "265598" {
		t.Fatalf("%+v asked %+v", got, reads.got)
	}
	for name, c := range map[string]rpc.ContractParams{
		"another con_id": {ConID: 1, Instrument: contract.Instrument},
		"another symbol": {Symbol: "MSFT", Instrument: contract.Instrument},
		"OpenD's id":     {Instrument: rpc.InstrumentID{Broker: "opend", ID: "US.AAPL"}},
	} {
		if _, err := resolveContractInstrument(t.Context(), reads, broker.SessionBinding{}, broker.KindIBKR, c); err == nil {
			t.Errorf("%s resolved", name)
		}
	}
	reads.err = errors.New("no session")
	if _, err := resolveContractInstrument(t.Context(), reads, broker.SessionBinding{}, broker.KindIBKR, contract); err == nil {
		t.Fatal("a failed lookup resolved")
	}
}

// IBKR rows gain the identity on the way out and only when a listing venue is
// known; the daemon's own rows, which revisions and caches hold, do not change.
func TestIBKRRowsCarryAnIdentityOnACopy(t *testing.T) {
	srv := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	stock := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeStock, ConID: 265598, PrimaryExch: "NASDAQ", Currency: "USD"}
	smart := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeOption, ConID: 7, Exchange: "SMART", Currency: "USD"}
	in := &rpc.PositionsResult{Stocks: []rpc.PositionView{stock}, Options: []rpc.PositionView{smart}, ByUnderlying: []rpc.PositionGroup{{Stock: &stock, Options: []rpc.PositionView{smart}}}}
	before, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := srv.withInstrumentIDs(in).(*rpc.PositionsResult)
	if !ok {
		t.Fatal("not positions")
	}
	want := rpc.InstrumentID{Broker: "ibkr", ID: "265598", Market: "us", Venue: "NASDAQ", Currency: "USD"}
	if out.Stocks[0].Instrument != want || out.ByUnderlying[0].Stock.Instrument != want {
		t.Fatalf("%+v", out.Stocks[0].Instrument)
	}
	if out.Options[0].Instrument != (rpc.InstrumentID{}) {
		t.Fatalf("a smart route has no listing: %+v", out.Options[0].Instrument)
	}
	requireJoin(t, "position", out.Stocks[0].Symbol, out.Stocks[0].Instrument)
	after, err := json.Marshal(in)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the daemon's own rows changed: %s", after)
	}

	orders := srv.withInstrumentIDs(&rpc.OrdersOpenResult{Orders: []rpc.OrderView{{Symbol: "AAPL", ConID: 265598, PrimaryExch: "NASDAQ"}, {Symbol: "AAPL", Exchange: "SMART"}}}).(*rpc.OrdersOpenResult)
	if orders.Orders[0].Instrument.ID != "265598" || orders.Orders[1].Instrument != (rpc.InstrumentID{}) {
		t.Fatalf("%+v", orders.Orders)
	}
	quote := srv.withInstrumentIDs(&rpc.Quote{Symbol: "AAPL", Contract: rpc.ContractParams{Symbol: "AAPL", ConID: 265598, PrimaryExch: "NASDAQ"}}).(*rpc.Quote)
	if quote.Instrument.ID != "265598" || quote.Contract.Instrument != (rpc.InstrumentID{}) {
		t.Fatalf("%+v", quote)
	}
	// An answer with no rows of this kind passes through as it came.
	other := &rpc.HealthResult{}
	if srv.withInstrumentIDs(other) != any(other) {
		t.Fatal("an unrelated answer was copied")
	}
	var missing *rpc.Quote
	if srv.withInstrumentIDs(missing) != any(missing) {
		t.Fatal("a nil row answer")
	}
}

func TestAWatchlistRoundTripStoresNoIdentity(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	add := dispatchRaw(t, srv, rpc.MethodWatchlistReplace, `{"symbols":[{"symbol":"AAPL"}],"expected_revision":0,"request_id":"first"}`)
	if !add.Ok {
		t.Fatalf("%+v", add.Error)
	}
	read := dispatchRaw(t, srv, rpc.MethodWatchlistList, `{}`)
	var list rpc.Watchlist
	if err := json.Unmarshal(read.Result, &list); err != nil || !read.Ok {
		t.Fatalf("%s %v", read.Result, err)
	}
	if got := list.Symbols[0].Instrument; got.ID != "US.AAPL" || got.Venue != "NASDAQ" {
		t.Fatalf("%+v", got)
	}
	// Written back as it was read.
	body, err := json.Marshal(rpc.WatchlistReplaceRequest{Symbols: list.Symbols, ExpectedRevision: list.Revision, RequestID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if res := dispatchRaw(t, srv, rpc.MethodWatchlistReplace, string(body)); !res.Ok {
		t.Fatalf("%+v", res.Error)
	}
	again, err := srv.handleWatchlistList(t.Context())
	if err != nil || !slices.Equal(again.Symbols, []rpc.WatchlistContract{{Symbol: "AAPL", SecType: "STK", Currency: "USD", Exchange: "SMART"}}) {
		t.Fatalf("stored %+v %v", again, err)
	}
}
