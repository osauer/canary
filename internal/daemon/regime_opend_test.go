package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

// Under OpenD the regime reads its listed inputs (HYG, SPY) from OpenD and
// asks nothing for the kinds OpenD does not quote: the volatility indices and
// the yen pair come back as not supplied, without a request that could read
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
	deps := productionRegimeDeps(reads, offlineRegimeSources(), t.Logf, nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	for _, sym := range []string{"HYG", "SPY"} {
		if q := deps.snapshot(ctx, sym, 5*time.Second); q.price <= 0 || q.refusal != nil {
			t.Errorf("%s snapshot on OpenD: %+v", sym, q)
		}
		bars, err := deps.history(ctx, sym, 60)
		if err != nil || len(bars) == 0 {
			t.Errorf("%s daily bars on OpenD: %d bars, %v", sym, len(bars), err)
		}
	}
	for _, sym := range []string{"VIX", "VIX3M", "USD.JPY"} {
		if q := deps.snapshot(ctx, sym, 5*time.Second); !errors.Is(q.refusal, broker.ErrNotSupplied) || q.price != 0 {
			t.Errorf("%s snapshot on OpenD is not refused as not supplied: %+v", sym, q)
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
		t.Errorf("refused inputs %v", got)
	}
	// Gamma needs option chains, which OpenD does not serve; the fan-out names
	// it from the coverage table, as acquireRegimeSnapshot does.
	if srv.notServedHere(api.GammaZeroSPX.Name) {
		deps.gaps.add(regimeGammaInput, broker.ErrNotSupplied)
	}
	warnings := regimeGapWarnings(reads.Kind(), deps.gaps)
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

// offlineRegimeSources stand in for the regime's public sites in a test
// daemon: a steady, current daily series for every official series and for
// Cboe's VVIX and VIX3M closes, so no test reaches the network.
func offlineRegimeSources() regimeSources {
	series := func(level float64) []regimeSeriesPoint {
		var points []regimeSeriesPoint
		day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
		for len(points) < 400 {
			if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
				points = append(points, regimeSeriesPoint{Date: day, Value: level})
			}
			day = day.AddDate(0, 0, -1)
		}
		slices.Reverse(points)
		return points
	}
	levels := map[string]float64{
		fredSeriesHYOAS: 3.1, fredSeriesIGOAS: 0.95, fredSeriesCP3M: 4.32, fredSeriesTBill3M: 4.18,
	}
	return regimeSources{
		official: func(_ context.Context, id string) ([]regimeSeriesPoint, error) {
			level, ok := levels[id]
			if !ok {
				return nil, errors.New("offline regime sources: no series " + id)
			}
			return series(level), nil
		},
		vvix:  func(context.Context) ([]regimeSeriesPoint, error) { return series(88), nil },
		vix3m: func(context.Context) ([]regimeSeriesPoint, error) { return series(19.4), nil },
	}
}

// regimeSimWithHYG is the simulator's book with HYG listed beside SPY.
func regimeSimWithHYG(t *testing.T) *futusim.Server {
	t.Helper()
	sim := futusim.Start(t, futusim.Options{})
	sim.Book().ListInstrument(futusim.Instrument{
		Code: "US.HYG", Name: "SYNTH HIGH YIELD ETF", Type: "etf", LotSize: 1, Currency: "USD",
		Exchange: "AMEX", Listed: "2007-04-04", Start: 79.4, Tick: 0.01, Volatility: 0.0002,
	})
	return sim
}

// dispatchedRegime asks an OpenD daemon for regime.snapshot as a client does.
func dispatchedRegime(t *testing.T, srv *Server) rpc.RegimeSnapshotResult {
	t.Helper()
	// The first refresh runs behind the first ask, as for any client.
	var res rpc.Response
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		res = dispatchRaw(t, srv, rpc.MethodRegimeSnapshot, `{}`)
		if res.Ok || time.Now().After(deadline) {
			break
		}
	}
	if !res.Ok || res.Error != nil {
		t.Fatalf("regime.snapshot under OpenD: %+v", res.Error)
	}
	var regime rpc.RegimeSnapshotResult
	if err := json.Unmarshal(res.Result, &regime); err != nil {
		t.Fatal(err)
	}
	return regime
}

func regimeWarning(r rpc.RegimeSnapshotResult, code, scope string) (rpc.RegimeWarning, bool) {
	for _, w := range r.WarningDetails {
		if w.Code == code && w.Scope == scope {
			return w, true
		}
	}
	return rpc.RegimeWarning{}, false
}

// Dispatched under an OpenD daemon, regime.snapshot and stress.snapshot answer
// with figures: HYG and SPY from OpenD, the official series and Cboe closes
// from their sources, and the inputs OpenD does not carry named in the
// reader's words.
func TestOpenDRegimeAndStressAnswerEndToEnd(t *testing.T) {
	sim := regimeSimWithHYG(t)
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)

	regime := dispatchedRegime(t, srv)
	hs := regime.HYGSPYDivergence
	if hs.HYGPrice == nil || *hs.HYGPrice <= 0 || hs.SPYPrice == nil || *hs.SPYPrice <= 0 {
		t.Errorf("HYG/SPY on OpenD: %+v", hs)
	}
	if v := regime.VolOfVol.Last; v == nil || *v <= 0 {
		t.Errorf("VVIX row: %+v", regime.VolOfVol)
	}
	if regime.Composite.Verdict == "" {
		t.Error("regime published without a verdict line")
	}
	for _, scope := range []string{"gamma_zero", "usd_jpy", "vix_term_structure"} {
		w, ok := regimeWarning(regime, "broker_input_not_supplied", scope)
		if !ok || !strings.Contains(w.Message, "OpenD has no") || !strings.Contains(w.Message, "missing from this regime") {
			t.Errorf("%s: no named gap in %+v", scope, regime.WarningDetails)
		}
	}
	t.Logf("OpenD regime: %s; %d of %d clusters ranked", regime.Composite.Verdict, regime.Composite.ClusterRankedCount, regime.Composite.ClusterRankedCount+regime.Composite.ClusterUnrankedCount)

	res := dispatchRaw(t, srv, rpc.MethodStressSnapshot, `{}`)
	if !res.Ok || res.Error != nil {
		t.Fatalf("stress.snapshot under OpenD: %+v", res.Error)
	}
	var stress rpc.StressSnapshotResult
	if err := json.Unmarshal(res.Result, &stress); err != nil {
		t.Fatal(err)
	}
	if stress.Stress.Summary == "" || len(stress.Stress.Rows) == 0 {
		t.Errorf("stress under OpenD answered without figures: %+v", stress.Stress)
	}
	t.Logf("OpenD stress: %s (%d rows, input health %q)", stress.Stress.Summary, len(stress.Stress.Rows), stress.Stress.InputHealth)
}

// A login without the US quote right gets no live HYG or SPY quote; the
// regime still publishes, names that the login lacks the right and says what
// to do.
func TestOpenDRegimeNamesAMissingQuoteRight(t *testing.T) {
	sim := regimeSimWithHYG(t)
	sim.SetQuoteRight("US", 5) // QotRight_No
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)

	regime := dispatchedRegime(t, srv)
	w, ok := regimeWarning(regime, "broker_quote_right_missing", "hyg_spy_divergence")
	if !ok {
		t.Fatalf("no missing quote right named: %+v", regime.WarningDetails)
	}
	for _, want := range []string{"no US quote right", "HYG and SPY are missing from this regime"} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("message %q lacks %q", w.Message, want)
		}
	}
	if !strings.Contains(w.Action, "Add the US quote right") {
		t.Errorf("action %q", w.Action)
	}
	// No live quote came back. The row's own fallback may still show HYG's
	// latest official close from daily bars, which the simulator serves
	// without a quote right; the row is in error and does not rank.
	hs := regime.HYGSPYDivergence
	if hs.SPYPrice != nil || hs.Status != rpc.RegimeStatusError || !slices.Contains(hs.FieldsMissing, "hyg_spot_tick") {
		t.Errorf("HYG/SPY without the quote right: status %q, SPY %v, missing %v", hs.Status, hs.SPYPrice, hs.FieldsMissing)
	}
}
