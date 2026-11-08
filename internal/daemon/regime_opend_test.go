package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc/api"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

// Under OpenD the regime reads its listed inputs (HYG, SPY) from OpenD and
// asks nothing for the kinds OpenD does not quote: the volatility indices and
// the yen pair come back unsupplied, without a request that could read
// another security under the same letters.
func TestOpenDRegimeReadsListedInputsAndRefusesIndicesAndFX(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	sim.Book().ListInstrument(futusim.Instrument{
		Code: "US.HYG", Name: "SYNTH HIGH YIELD ETF", Type: "etf", LotSize: 1, Currency: "USD",
		Exchange: "AMEX", Listed: "2007-04-04", Start: 79.4, Tick: 0.01, Volatility: 0.0002,
	})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)

	reads := srv.brokerReads()
	if reads == nil || reads.Kind() != broker.KindOpenD {
		t.Fatalf("broker reads under OpenD: %v", reads)
	}
	deps := productionRegimeDeps(reads, t.Logf, nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	for _, sym := range []string{"HYG", "SPY"} {
		if q := deps.snapshot(ctx, sym, 5*time.Second); q.price <= 0 || q.unsupplied {
			t.Errorf("%s snapshot on OpenD: %+v", sym, q)
		}
		bars, err := deps.history(ctx, sym, 60)
		if err != nil || len(bars) == 0 {
			t.Errorf("%s daily bars on OpenD: %d bars, %v", sym, len(bars), err)
		}
	}
	for _, sym := range []string{"VIX", "VIX3M", "USD.JPY"} {
		if q := deps.snapshot(ctx, sym, 5*time.Second); !q.unsupplied || q.price != 0 {
			t.Errorf("%s snapshot on OpenD is not refused as unsupplied: %+v", sym, q)
		}
		if _, err := deps.history(ctx, sym, 60); !errors.Is(err, broker.ErrNotSupplied) {
			t.Errorf("%s daily bars on OpenD: %v, want not supplied", sym, err)
		}
	}
	// The SPY 52-week high is in OpenD's snapshot only when the book carries
	// it; the simulator's does not, so the regime's bar fallback supplies it.
	if q := deps.snapshotWith52WHigh(ctx, "SPY", 5*time.Second); q.price <= 0 {
		t.Errorf("SPY with 52-week high on OpenD: %+v", q)
	}

	// The refused inputs are named for the publication, one warning per row.
	if got := deps.gaps.list(); !slices.Equal(got, []string{"USD.JPY", "VIX", "VIX3M"}) {
		t.Errorf("unsupplied inputs %v", got)
	}
	// Gamma needs option chains, which OpenD does not serve; the fan-out names
	// it from the coverage table, as acquireRegimeSnapshot does.
	if srv.notServedHere(api.GammaZeroSPX.Name) {
		deps.gaps.add(regimeGammaInput)
	}
	warnings := regimeGapWarnings(reads.Kind(), deps.gaps.list())
	var scopes []string
	for _, w := range warnings {
		scopes = append(scopes, w.Scope)
		if w.Code != "broker_input_not_supplied" || !strings.Contains(w.Message, "OpenD") {
			t.Errorf("gap warning %+v", w)
		}
	}
	if !slices.Equal(scopes, []string{"gamma_zero", "usd_jpy", "vix_term_structure"}) {
		t.Errorf("gap warnings for %v", scopes)
	}

	// Only the listed inputs drew history: no request named an index or a pair.
	_, _, drawn := sim.Book().HistoryUsage()
	codes := make([]string, 0, len(drawn))
	for code := range drawn {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, []string{"US.HYG", "US.SPY"}) {
		t.Errorf("history drawn for %v, want US.HYG and US.SPY only", codes)
	}
}
