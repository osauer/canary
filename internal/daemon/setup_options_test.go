package daemon

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestSetupOptionListingsAreBoundedAndNeverInvented(t *testing.T) {
	now := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	listing := map[string][]ibkr.ExpiryClassedStrikes{}
	for i := -1; i < 70; i++ {
		listing[now.AddDate(0, 0, i).Format("2006-01-02")] = []ibkr.ExpiryClassedStrikes{{TradingClass: "SYNTH", Strikes: []float64{99, 100, 101}}}
	}
	expiries, truncated := setupOptionExpiries(listing, now)
	if len(expiries) != 64 || !truncated || expiries[0].Date != "20261002" {
		t.Fatal(expiries, truncated)
	}
	strikes, err := setupListedStrikes(listing, "SYNTH", "20261002")
	if err != nil || len(strikes) != 3 {
		t.Fatal(strikes, err)
	}
	if _, err := setupListedStrikes(nil, "SYNTH", "20261002"); err == nil {
		t.Fatal("missing listing invented a grid")
	}
	listing["2026-10-02"][0].TradingClass = "SYNTH1"
	if _, err := setupListedStrikes(listing, "SYNTH", "20261002"); err == nil {
		t.Fatal("adjusted class accepted")
	}
	listing["2026-10-02"] = []ibkr.ExpiryClassedStrikes{{TradingClass: "SYNTH", Strikes: []float64{math.NaN()}}}
	if _, err := setupListedStrikes(listing, "SYNTH", "20261002"); err == nil {
		t.Fatal("nonfinite strike accepted")
	}
}

func TestSetupCallQuoteIsQuotedOnlyForDatedLabelledTwoSidedPrices(t *testing.T) {
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	live := func() rpc.OrderQuoteSnapshot {
		return rpc.OrderQuoteSnapshot{Symbol: "SYNX", Bid: new(1.2), Ask: new(1.3), DataType: rpc.MarketDataLive, PriceAt: now.Add(-2 * time.Second), AsOf: now.Add(-time.Second)}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*rpc.OrderQuoteSnapshot)
		asOf   time.Time
	}{
		{"live dated by older side receipt", func(*rpc.OrderQuoteSnapshot) {}, now.Add(-2 * time.Second)},
		{"locked bid equals ask", func(q *rpc.OrderQuoteSnapshot) { q.Ask = new(1.2) }, now.Add(-2 * time.Second)},
		{"exactly sixty seconds old", func(q *rpc.OrderQuoteSnapshot) { q.PriceAt = now.Add(-setupQuoteMaxAge) }, now.Add(-setupQuoteMaxAge)},
		{"delayed dated by its read", func(q *rpc.OrderQuoteSnapshot) { q.DataType, q.PriceAt = rpc.MarketDataDelayed, time.Time{} }, now.Add(-time.Second)},
		{"frozen dated by its read", func(q *rpc.OrderQuoteSnapshot) { q.DataType, q.PriceAt = rpc.MarketDataFrozen, time.Time{} }, now.Add(-time.Second)},
		{"delayed-frozen dated by its read", func(q *rpc.OrderQuoteSnapshot) { q.DataType, q.PriceAt = rpc.MarketDataDelayedFrozen, time.Time{} }, now.Add(-time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := live()
			tc.mutate(&snap)
			got, reason := setupOptionQuote(snap, now)
			if got.Status != rpc.SetupQuoteQuoted || reason != rpc.SetupQuoteQuoted || got.DataType != snap.DataType || !got.AsOf.Equal(tc.asOf) {
				t.Fatalf("usable quote not quoted: %+v (%s)", got, reason)
			}
			if got.Bid == nil || got.Ask == nil || *got.Bid != *snap.Bid || *got.Ask != *snap.Ask || *got.Bid <= 0 || got.Bid == snap.Bid || got.Ask == snap.Ask {
				t.Fatalf("quoted prices lost, zeroed or aliased: %+v", got)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*rpc.OrderQuoteSnapshot)
	}{
		{"no bid", func(q *rpc.OrderQuoteSnapshot) { q.Bid = nil }},
		{"no ask", func(q *rpc.OrderQuoteSnapshot) { q.Ask = nil }},
		{"zero bid", func(q *rpc.OrderQuoteSnapshot) { q.Bid = new(0.0) }},
		{"negative ask", func(q *rpc.OrderQuoteSnapshot) { q.Ask = new(-1.3) }},
		{"NaN bid", func(q *rpc.OrderQuoteSnapshot) { q.Bid = new(math.NaN()) }},
		{"infinite ask", func(q *rpc.OrderQuoteSnapshot) { q.Ask = new(math.Inf(1)) }},
		{"crossed", func(q *rpc.OrderQuoteSnapshot) { q.Bid = new(1.31) }},
		{"unknown data type", func(q *rpc.OrderQuoteSnapshot) { q.DataType = rpc.MarketDataUnknown }},
		{"unlabelled data type", func(q *rpc.OrderQuoteSnapshot) { q.DataType = "" }},
		{"previous close", func(q *rpc.OrderQuoteSnapshot) { q.DataType = rpc.MarketDataPrevClose }},
		{"older than sixty seconds", func(q *rpc.OrderQuoteSnapshot) { q.PriceAt = now.Add(-setupQuoteMaxAge - time.Nanosecond) }},
		{"read older than sixty seconds", func(q *rpc.OrderQuoteSnapshot) {
			q.DataType, q.PriceAt, q.AsOf = rpc.MarketDataFrozen, time.Time{}, now.Add(-2*time.Minute)
		}},
		{"undated", func(q *rpc.OrderQuoteSnapshot) { q.PriceAt, q.AsOf = time.Time{}, time.Time{} }},
		{"future dated", func(q *rpc.OrderQuoteSnapshot) { q.PriceAt = now.Add(time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := live()
			tc.mutate(&snap)
			got, reason := setupOptionQuote(snap, now)
			if got != (rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}) || reason == rpc.SetupQuoteQuoted {
				t.Fatalf("unusable quote leaked prices or a label: %+v (%s)", got, reason)
			}
		})
	}
}

func TestSetupCallQuoteReadIsBoundedAndCannotFailSelection(t *testing.T) {
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	clock := func(times ...time.Time) func() time.Time {
		return func() time.Time {
			next := times[0]
			times = times[1:]
			return next
		}
	}
	var budget time.Duration
	failed, reason, took := readSetupCallQuote(t.Context(), func(_ context.Context, b time.Duration) (rpc.OrderQuoteSnapshot, error) {
		budget = b
		return rpc.OrderQuoteSnapshot{Bid: new(1.2), Ask: new(1.3), DataType: rpc.MarketDataLive, PriceAt: now, AsOf: now}, errors.New("exact contract quote unavailable: context deadline exceeded")
	}, clock(now, now.Add(setupQuoteBudget)))
	if budget != 5*time.Second || failed != (rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}) || !strings.Contains(reason, "deadline exceeded") || took != 5*time.Second {
		t.Fatalf("failed read was not a bounded missing quote: budget=%s quote=%+v reason=%q took=%s", budget, failed, reason, took)
	}
	quoted, reason, took := readSetupCallQuote(t.Context(), func(context.Context, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		return rpc.OrderQuoteSnapshot{Bid: new(1.2), Ask: new(1.3), DataType: rpc.MarketDataLive, PriceAt: now.Add(300 * time.Millisecond), AsOf: now.Add(400 * time.Millisecond)}, nil
	}, clock(now, now.Add(400*time.Millisecond)))
	if quoted.Status != rpc.SetupQuoteQuoted || reason != rpc.SetupQuoteQuoted || took != 400*time.Millisecond || !quoted.AsOf.Equal(now.Add(300*time.Millisecond)) {
		t.Fatalf("successful read was not judged by the rule: %+v (%s) took=%s", quoted, reason, took)
	}
	// Without a broker connection the selection still receives a quote object,
	// missing, instead of an error that would discard the resolved contract.
	call := rpc.ContractParams{ConID: 900002, Symbol: "SYNX", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20261120", Strike: 102.5, Right: "C", Multiplier: 100, TradingClass: "SYNX", LocalSymbol: "SYNX  261120C00102500"}
	got := (&Server{}).setupCallQuote(t.Context(), nil, ibkr.ConnectorSessionBinding{}, call)
	if got == nil || *got != (rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}) {
		t.Fatalf("unavailable broker did not yield a missing quote: %+v", got)
	}
}
