package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestOptionLegDataStatus(t *testing.T) {
	t.Parallel()
	delta := 0.42
	cases := []struct {
		name string
		bid  float64
		ask  float64
		last float64
		prev float64
		iv   float64
		d    *float64
		want string
	}{
		{name: "quoted bid", bid: 1.2, want: "quoted"},
		{name: "quoted ask", ask: 1.3, want: "quoted"},
		{name: "quoted last", last: 1.25, want: "quoted"},
		{name: "previous close price anchor", prev: 1.12, want: "prev_close"},
		{name: "model IV only", iv: 0.31, want: "model_only"},
		{name: "model delta only", d: &delta, want: "model_only"},
		{name: "no data", want: "no_quote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := optionLegDataStatus(tc.bid, tc.ask, tc.last, tc.prev, tc.iv, tc.d); got != tc.want {
				t.Fatalf("optionLegDataStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChainWarningDetailsSummarizesUnavailableLegs(t *testing.T) {
	t.Parallel()
	res := &rpc.ChainResult{
		Symbol:   "BB",
		DataType: rpc.MarketDataClosed,
		Strikes: []rpc.ChainStrike{
			{CallDataStatus: "prev_close", CallIVStatus: "ok", CallOIStatus: "unavailable"},
			{CallDataStatus: "model_only", CallIVStatus: "ok", CallOIStatus: "unavailable"},
			{CallDataStatus: "no_quote", CallIVStatus: "unavailable", CallOIStatus: "unavailable"},
		},
	}

	got := chainWarningDetails(res, true, false)

	for _, want := range []string{"options_closed", "prev_close_legs", "model_only_legs", "oi_unavailable", "iv_unavailable"} {
		if !hasWarningCode(got, want) {
			t.Fatalf("warning %q missing from %+v", want, got)
		}
	}
	if !strings.Contains(warningMessage(got, "oi_unavailable"), "3 option legs") {
		t.Fatalf("oi warning should summarize all requested legs: %+v", got)
	}
	if !strings.Contains(warningImpact(got, "options_closed"), "extended or curb trading may still exist") {
		t.Fatalf("options_closed should distinguish regular-hours surface from SPX/VIX extended trading: %+v", got)
	}
}

func TestChainSpotFromSnapshotLabelsPreviousClose(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, 5, 26, 8, 5, 0, 0, time.UTC)

	got := chainSpotFromSnapshot(0, 0, 0, 0, 637.42, rpc.MarketDataFrozen, asOf)

	if got.Price != 637.42 {
		t.Fatalf("Price = %v, want 637.42", got.Price)
	}
	if got.Source != "prev_close" {
		t.Fatalf("Source = %q, want prev_close", got.Source)
	}
	if got.DataType != rpc.MarketDataPrevClose {
		t.Fatalf("DataType = %q, want prev_close", got.DataType)
	}
	if got.FeedType != rpc.MarketDataFrozen {
		t.Fatalf("FeedType = %q, want frozen", got.FeedType)
	}
	if !got.AsOf.Equal(asOf) {
		t.Fatalf("AsOf = %v, want %v", got.AsOf, asOf)
	}
}

func TestChainSpotFromQuoteUsesSelectedHistoricalClose(t *testing.T) {
	t.Parallel()
	price := 637.42
	priceAt := time.Date(2026, 5, 22, 20, 0, 0, 0, time.UTC)
	got := chainSpotFromQuote(&rpc.Quote{
		Price:       &price,
		PriceSource: "historical_close",
		DataType:    rpc.MarketDataPrevClose,
		PriceAt:     priceAt,
	})

	if got.Price != price {
		t.Fatalf("Price = %v, want %v", got.Price, price)
	}
	if got.Source != "historical_close" {
		t.Fatalf("Source = %q, want historical_close", got.Source)
	}
	if got.DataType != rpc.MarketDataPrevClose {
		t.Fatalf("DataType = %q, want prev_close", got.DataType)
	}
	if !got.AsOf.Equal(priceAt) {
		t.Fatalf("AsOf = %v, want %v", got.AsOf, priceAt)
	}
}

func TestChainWarningDetailsMarksPreviousCloseSpotAnchor(t *testing.T) {
	t.Parallel()
	res := &rpc.ChainResult{
		Symbol:     "SPY",
		Spot:       637.42,
		SpotSource: "historical_close",
		Strikes:    []rpc.ChainStrike{},
	}

	got := chainWarningDetails(res, true, true)

	if !hasWarningCode(got, "selected_chain_spot_prev_close") {
		t.Fatalf("previous-close spot warning missing from %+v", got)
	}
}

func TestChainExpiryIVWarningsMarksNoUsableMoves(t *testing.T) {
	t.Parallel()
	got := chainExpiryIVWarnings("SPY", []rpc.ChainExpiry{
		{Date: "2026-06-19", IVStatus: "timeout", IVQuality: "unavailable"},
		{Date: "2026-09-18", IVStatus: "unavailable", IVQuality: "unavailable"},
	}, true)

	if !hasWarningCode(got, "live_option_iv_unavailable") {
		t.Fatalf("live IV warning missing from %+v", got)
	}
}

func TestSelectChainExpiriesByDTE(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	expiries := []string{
		"2026-05-29",
		"2026-06-19",
		"2026-08-31",
		"2026-09-18",
		"2026-11-20",
		"2027-01-15",
	}

	got := selectChainExpiriesByDTE(expiries, today, 90, 180, 0)
	want := []string{"2026-08-31", "2026-09-18", "2026-11-20"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("range filtered expiries = %v, want %v", got, want)
	}

	got = selectChainExpiriesByDTE(expiries, today, 90, 180, 120)
	if len(got) != 1 || got[0] != "2026-09-18" {
		t.Fatalf("target filtered expiries = %v, want [2026-09-18]", got)
	}
}

func hasWarningCode(warnings []rpc.DataWarning, code string) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func warningMessage(warnings []rpc.DataWarning, code string) string {
	for _, w := range warnings {
		if w.Code == code {
			return w.Message
		}
	}
	return ""
}

func warningImpact(warnings []rpc.DataWarning, code string) string {
	for _, w := range warnings {
		if w.Code == code {
			return w.Impact
		}
	}
	return ""
}
