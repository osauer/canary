package daemon

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestAddClosedPlanningRetainsPricesWithoutGrantingLiveEligibility(t *testing.T) {
	for _, feed := range []string{rpc.MarketDataFrozen, rpc.MarketDataLive} {
		t.Run(feed, func(t *testing.T) {
			// Decode the additive planning contract as a consumer would. The old
			// model silently loses it, discards frozen candidates, and cannot keep
			// a closed-session live-labelled receipt out of live eligibility.
			raw, _ := json.Marshal(map[string]any{
				"contract":         rpc.ContractParams{ConID: 10110, Symbol: "SYNTH", SecType: "OPT", Expiry: "20270219", Strike: 110, Right: "C", Multiplier: 100, Currency: "USD", Exchange: "SMART", TradingClass: "SYNTH"},
				"quote":            rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, Bid: new(1.0), Ask: new(1.02), DataType: feed, ModelDataType: feed, Delta: new(.6), IV: new(.2), AsOf: time.Now(), ModelAsOf: time.Now()},
				"price_increments": []rpc.SetupOptionPriceIncrement{{Increment: .01}},
				"planning":         map[string]any{"price": 1.01, "price_source": "broker_midpoint", "observed_at": time.Now(), "delta_status": "in_range"},
			})
			var row rpc.AddCallCandidate
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			got := selectAddCall(testCallSide, []ibkr.ResolvedOrderContract{{Contract: ibkr.Contract{Strike: 110}}}, 110, func(ibkr.ResolvedOrderContract) rpc.AddCallCandidate { return row })
			if got.Contract == nil {
				t.Fatal("closed-session planning price was discarded")
			}
			if eligibleAddCall(testCallSide, got, time.Now()) {
				t.Fatal("planning receipt acquired live eligibility")
			}
		})
	}
}

func TestAddOptionsCalendarSchedulesClosedPeriodsAndFailsSafeOutsideCoverage(t *testing.T) {
	for _, tc := range []struct {
		at, mode, nextOpen, lastDate string
	}{
		{"2026-03-07T12:00:00-05:00", "planning", "2026-03-09T13:30:00Z", "2026-03-06"},
		{"2026-03-09T09:00:00-04:00", "planning", "2026-03-09T13:30:00Z", "2026-03-06"},
		{"2026-03-09T09:30:00-04:00", "live", "", "2026-03-06"},
		{"2026-11-27T13:15:00-05:00", "planning", "2026-11-30T14:30:00Z", "2026-11-27"},
		{"2026-09-07T12:00:00-04:00", "planning", "2026-09-08T13:30:00Z", "2026-09-04"},
		{"2029-01-02T12:00:00-05:00", "unavailable", "", ""},
	} {
		t.Run(tc.at, func(t *testing.T) {
			at, _ := time.Parse(time.RFC3339, tc.at)
			got := addOptionsCalendar(at)
			next, last := "", ""
			if got.current.NextOpen != nil {
				next = got.current.NextOpen.UTC().Format(time.RFC3339)
			}
			if got.last != nil {
				last = got.last.Date
			}
			if got.mode != tc.mode || next != tc.nextOpen || last != tc.lastDate || got.current.Market != "us_options" || got.current.SourceURL == "" {
				t.Fatalf("lost calendar authority: mode=%s next=%s last=%s session=%+v", got.mode, next, last, got.current)
			}
			if tc.mode == "unavailable" && (got.current.IsOpen || !got.current.Open.IsZero() || got.last != nil) {
				t.Fatal("invented a schedule outside coverage")
			}
		})
	}
}

func TestAddPlanningPricesFromTheBrokerQuoteReceipt(t *testing.T) {
	received := time.Date(2026, 3, 7, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		quote *rpc.SetupOptionQuote
		want  *float64
	}{
		{"two-sided", &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, Bid: new(1.0), Ask: new(1.2), DataType: rpc.MarketDataFrozen, AsOf: received}, new(1.1)},
		{"one-sided", &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, Bid: new(1.0), DataType: rpc.MarketDataFrozen, AsOf: received}, nil},
		{"missing", &rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			planning := addPlanningObservation(testCallSide, tc.quote, nil)
			if tc.want == nil {
				if planning.Price != nil || planning.PriceSource != "" {
					t.Fatalf("priced an incomplete quote: %+v", planning)
				}
			} else if planning.Price == nil || *planning.Price != *tc.want || planning.PriceSource != "broker_midpoint" || !planning.ObservedAt.Equal(received) {
				t.Fatalf("receipt price lost or relabelled: %+v", planning)
			}
			if planning.DeltaStatus != "unavailable" {
				t.Fatal("a quote without a model manufactured delta qualification")
			}
		})
	}
}

func TestAddPlanningSkipsKnownOutOfRangeAndKeepsMissingDeltaUnqualified(t *testing.T) {
	calls := []ibkr.ResolvedOrderContract{{Contract: ibkr.Contract{Strike: 100}}, {Contract: ibkr.Contract{Strike: 105}}, {Contract: ibkr.Contract{Strike: 110}}}
	var quoted []float64
	var mu sync.Mutex
	got := selectAddCall(testCallSide, calls, 105, func(exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
		strike := exact.Contract.Strike
		mu.Lock()
		quoted = append(quoted, strike)
		mu.Unlock()
		q := &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, Bid: new(1.0), Ask: new(1.2), DataType: rpc.MarketDataFrozen, AsOf: time.Now()}
		if strike == 105 {
			q.Delta = new(.49)
		}
		return rpc.AddCallCandidate{Contract: &rpc.ContractParams{ConID: 10000 + int(strike), Strike: strike}, Quote: q, Planning: addPlanningObservation(testCallSide, q, nil)}
	})
	if got.Contract == nil || got.Contract.Strike != 100 || !reflect.DeepEqual(sortedStrikes(quoted), []float64{100, 105, 110}) || got.Planning == nil || got.Planning.DeltaStatus != "unavailable" || eligibleAddCall(testCallSide, got, time.Now()) {
		t.Fatalf("planning invented qualification or retained known out-of-range delta: %+v quoted=%v", got, quoted)
	}
	// The same frozen observation outside the explicit planning path remains
	// unusable for live selection and the call-plan's unchanged evidence gate.
	got.Planning = nil
	if eligibleAddCall(testCallSide, got, time.Now()) {
		t.Fatal("frozen planning quote became executable")
	}
}

func TestAddPlanningPrefersKnownInRangeDeltaOverNearerUnknown(t *testing.T) {
	calls := []ibkr.ResolvedOrderContract{{Contract: ibkr.Contract{Strike: 100}}, {Contract: ibkr.Contract{Strike: 105}}, {Contract: ibkr.Contract{Strike: 110}}}
	var quoted []float64
	var mu sync.Mutex
	got := selectAddCall(testCallSide, calls, 105, func(exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
		strike := exact.Contract.Strike
		mu.Lock()
		quoted = append(quoted, strike)
		mu.Unlock()
		q := &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, Bid: new(1.0), Ask: new(1.2), DataType: rpc.MarketDataFrozen, AsOf: time.Now()}
		if strike == 100 {
			q.Delta = new(.6)
		}
		return rpc.AddCallCandidate{Contract: &rpc.ContractParams{ConID: 10000 + int(strike), Strike: strike}, Quote: q, Planning: addPlanningObservation(testCallSide, q, nil)}
	})
	if got.Contract == nil || got.Contract.Strike != 100 || got.Planning.DeltaStatus != "in_range" || !reflect.DeepEqual(sortedStrikes(quoted), []float64{100, 105, 110}) {
		t.Fatalf("nearer unqualified contract hid an in-range call: %+v quoted=%v", got, quoted)
	}
}

func TestAddCallPlanRejectsClosedOrUnknownCalendarBeforePreview(t *testing.T) {
	for _, at := range []time.Time{time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC), time.Date(2029, 1, 2, 16, 0, 0, 0, time.UTC)} {
		t.Run(at.Format(time.RFC3339), func(t *testing.T) {
			s := newTestServer(t)
			s.now = func() time.Time { return at }
			raw, _ := json.Marshal(rpc.AddCallPlanParams{Contract: rpc.ContractParams{ConID: 10110, Symbol: "SYNTH", SecType: "OPT", Expiry: time.Now().AddDate(0, 0, 30).Format("20060102"), Strike: 110, Right: "C", Multiplier: 100, Currency: "USD", Exchange: "SMART", TradingClass: "SYNTH"}, Budget: 5000, LimitPrice: 1.01})
			got, err := s.handleAddCallPlan(t.Context(), &rpc.Request{Params: raw})
			if got != nil || err == nil || !strings.Contains(err.Error(), "Options session must be open to plan a call") {
				t.Fatalf("closed or unknown calendar reached preview authority: got=%+v err=%v", got, err)
			}
		})
	}
}
