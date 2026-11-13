package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

// Dispatched under an OpenD daemon, market.snapshot prices the held names in
// Hong Kong, the US and Singapore and the US ETF references from OpenD, and
// names the index and futures rows OpenD does not carry without asking OpenD
// for them. The overview reads partial, not refused. The book lists HYG as
// OpenD does, and the regime's own refresh runs first, so its requests are on
// the simulator beside the overview's.
func TestOpenDMarketOverviewPricesWhatOpenDCarriesAndNamesTheRest(t *testing.T) {
	sim := regimeSimWithHYG(t)
	for _, etf := range []struct {
		code, name string
		start      float64
	}{{"US.DIA", "SYNTH DOW ETF", 441.2}, {"US.GLD", "SYNTH GOLD ETF", 248.7}, {"US.QQQ", "SYNTH NASDAQ 100 ETF", 512.9}} {
		sim.Book().ListInstrument(futusim.Instrument{
			Code: etf.code, Name: etf.name, Type: "etf", LotSize: 1, Currency: "USD",
			Exchange: "AMEX", Listed: "1999-03-10", Start: etf.start, Tick: 0.01, Volatility: 0.0003,
		})
	}
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	refreshRegimeFromOpenD(t, srv, sim)
	if !slices.Contains(sim.SnapshotCodes(), "US.HYG") {
		t.Fatalf("the regime acquisition asked no HYG snapshot: %v", sim.SnapshotCodes())
	}

	before := len(sim.SnapshotCodes())
	res := dispatchRaw(t, srv, rpc.MethodMarketSnapshot, `{}`)
	if !res.Ok || res.Error != nil {
		t.Fatalf("market.snapshot under OpenD: %+v", res.Error)
	}
	var result rpc.MarketSnapshotResult
	if err := json.Unmarshal(res.Result, &result); err != nil {
		t.Fatal(err)
	}
	priced := func(row rpc.MarketInstrument) bool {
		return row.Error == "" && row.Quote != nil && row.Quote.Price != nil && *row.Quote.Price > 0
	}

	var etfs, named []string
	for _, row := range result.Instruments {
		switch row.Kind {
		case "etf":
			if !priced(row) {
				t.Errorf("%s: not priced from OpenD: error %q quote %+v", row.Name, row.Error, row.Quote)
			}
			etfs = append(etfs, row.Quote.Symbol)
		case "index", "future":
			want := "OpenD has no " + marketRowKindWords(row.Kind) + ", so " + row.Name + " is missing from this overview."
			if row.Quote != nil || row.Error != want {
				t.Errorf("%s: want the gap named, got error %q quote %+v", row.Name, row.Error, row.Quote)
			}
			named = append(named, row.Name)
		default:
			t.Errorf("%s: unexpected reference kind %q", row.Name, row.Kind)
		}
	}
	slices.Sort(etfs)
	if !slices.Equal(etfs, []string{"DIA", "GLD", "QQQ", "SPY"}) {
		t.Errorf("ETF references priced: %v", etfs)
	}
	if len(named) != 9 {
		t.Errorf("index and futures rows named as missing: %d (%v), want 9", len(named), named)
	}

	var held []string
	for _, row := range result.Underlyings {
		if !priced(row) {
			t.Errorf("held %s: not priced from OpenD: error %q quote %+v", row.Underlying, row.Error, row.Quote)
		}
		held = append(held, row.Underlying)
	}
	slices.Sort(held)
	if !slices.Equal(held, []string{"00700", "AAPL", "ES3"}) {
		t.Errorf("held names in the overview: %v", held)
	}
	if result.CoverageStatus != "partial" || result.Authority == nil || result.Authority.Availability != rpc.AccountDataAvailable {
		t.Errorf("coverage %q, authority %+v", result.CoverageStatus, result.Authority)
	}
	if len(result.Delayed) != 0 {
		t.Errorf("rows marked delayed on OpenD: %v", result.Delayed)
	}

	// No request named an index or a future: a reference row is read as one
	// snapshot on OpenD, and the simulator lists no index or future. Every
	// code named while the overview was asked is one the book lists, and the
	// ETF references are among them, so these are the overview's requests.
	asked := sim.SnapshotCodes()[before:]
	for _, code := range asked {
		if _, err := sim.Book().Instrument(code); err != nil {
			t.Errorf("a snapshot request named %s, which OpenD does not list", code)
		}
	}
	for _, code := range []string{"US.DIA", "US.GLD", "US.QQQ", "US.SPY"} {
		if !slices.Contains(asked, code) {
			t.Errorf("the overview asked OpenD for no %s snapshot: %v", code, asked)
		}
	}
	t.Logf("OpenD overview: %d ETF references and %d held names priced, %d rows named as missing: %s", len(etfs), len(held), len(named), strings.Join(named, ", "))
}
