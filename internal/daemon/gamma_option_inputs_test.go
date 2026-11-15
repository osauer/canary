package daemon

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestGammaCollectsModelTickAfterEarlyOpenInterest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		read := gammaOptionInputReader{
			model: func() (float64, int, bool) {
				return .25, ibkr.OptionModelDataTypeLive, time.Since(start) >= 1600*time.Millisecond
			},
			greeks: func() (ibkr.Greeks, bool) { return ibkr.Greeks{Gamma: .02}, true },
			oi:     func() (int64, bool) { return 42, time.Since(start) >= 100*time.Millisecond },
		}
		got, err := collectGammaOptionInputs(t.Context(), rpc.MarketDataFrozen, "SYNTH", nil, read)
		if err != nil || !got.OK || got.IVSource != gammaIVSourceModelTick || got.IV != .25 || got.Gamma != .02 || !got.OIObserved || got.OI != 42 {
			t.Fatalf("late valid model tick lost: %+v, %v (wait %s)", got, err, time.Since(start))
		}
		if time.Since(start) > 2250*time.Millisecond {
			t.Fatal("collection exceeded the existing combined IV/OI budget")
		}
	})
}

func TestGammaOptionInputCollectionBoundsAndSourceClocks(t *testing.T) {
	for _, tc := range []struct {
		name          string
		modelAt, oiAt time.Duration
		spotType      string
		modelType     int
		wantModel     bool
		wantOI        bool
		maxWait       time.Duration
	}{
		{"prompt inputs", 150 * time.Millisecond, 300 * time.Millisecond, rpc.MarketDataLive, ibkr.OptionModelDataTypeLive, true, true, 300 * time.Millisecond},
		{"missing OI keeps original grace", 0, time.Hour, rpc.MarketDataLive, ibkr.OptionModelDataTypeLive, true, false, optionOpenInterestGrace},
		{"late model cannot restart OI grace", 2200 * time.Millisecond, time.Hour, rpc.MarketDataLive, ibkr.OptionModelDataTypeLive, true, false, gammaOptionInputBudget},
		{"missing model stays missing", time.Hour, 0, rpc.MarketDataFrozen, ibkr.OptionModelDataTypeLive, false, true, gammaOptionInputBudget},
		{"live spot rejects delayed model", 0, 0, rpc.MarketDataLive, ibkr.OptionModelDataTypeDelayed, false, true, gammaOptionInputBudget},
		{"delayed spot rejects live model", 0, 0, rpc.MarketDataDelayed, ibkr.OptionModelDataTypeLive, false, true, gammaOptionInputBudget},
		{"aligned delayed inputs", 150 * time.Millisecond, 0, rpc.MarketDataDelayed, ibkr.OptionModelDataTypeDelayed, true, true, 150 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				modelReads, oiReads := 0, 0
				read := gammaOptionInputReader{
					model: func() (float64, int, bool) {
						modelReads++
						return .25, tc.modelType, time.Since(start) >= tc.modelAt
					},
					greeks: func() (ibkr.Greeks, bool) { return ibkr.Greeks{Gamma: .02}, true },
					oi: func() (int64, bool) {
						oiReads++
						return 0, time.Since(start) >= tc.oiAt
					},
				}
				got, err := collectGammaOptionInputs(t.Context(), tc.spotType, "SYNTH", nil, read)
				if err != nil || got.OK != tc.wantModel || got.OIObserved != tc.wantOI {
					t.Fatalf("collection = %+v, %v", got, err)
				}
				if elapsed := time.Since(start); elapsed > tc.maxWait || elapsed == 0 {
					t.Fatalf("wait %s, want positive and <= %s", elapsed, tc.maxWait)
				}
				if modelReads > 32 || oiReads > 12 || !tc.wantModel && oiReads != 1 {
					t.Fatalf("unbounded polling: model=%d OI=%d", modelReads, oiReads)
				}
			})
		})
	}
}

func TestGammaOptionInputCollectionStopsOnCancellationAndRejection(t *testing.T) {
	for _, rejection := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reject := make(chan ibkr.SubscriptionRejection, 1)
			start := time.Now()
			go func() {
				time.Sleep(100 * time.Millisecond)
				if rejection {
					reject <- ibkr.SubscriptionRejection{Code: 354}
				} else {
					cancel()
				}
			}()
			read := gammaOptionInputReader{
				model:  func() (float64, int, bool) { return 0, 0, false },
				greeks: func() (ibkr.Greeks, bool) { t.Fatal("read absent model Greeks"); return ibkr.Greeks{}, false },
				oi: func() (int64, bool) {
					t.Fatal("acquired fallback inputs after cancellation/rejection")
					return 0, false
				},
			}
			got, err := collectGammaOptionInputs(ctx, rpc.MarketDataLive, "SYNTH", reject, read)
			if got != (legResult{}) || rejection && !IsSubscriptionRejected(err) || !rejection && !errors.Is(err, context.Canceled) {
				t.Fatalf("collection continued: %+v, %v", got, err)
			}
			if time.Since(start) != 100*time.Millisecond {
				t.Fatal("cancellation/rejection waited out the collection budget")
			}
		})
	}
}

func TestGammaWeekendDoesNotEraseMissingModelQuality(t *testing.T) {
	friday := time.Date(2026, 10, 9, 19, 0, 0, 0, time.UTC)
	saturday := friday.Add(24 * time.Hour)
	for _, missing := range []bool{false, true} {
		result := rankableGammaFixture(rpc.GammaZeroScopeSPX, friday)
		want := rpc.GammaRankabilityRankable
		if missing {
			result.DerivedIVLegs = result.PricedLegCount
			result.Warnings = []string{"all_iv_derived"}
			want = rpc.GammaRankabilityBlocked
		}
		annotateGammaQuality(result, saturday)
		if !gammaQualityHasGate(result.Quality, rpc.GammaQualityGateFreshness, rpc.GammaQualityGatePass) || result.Quality.Rankability != want {
			t.Fatalf("weekend confused freshness with model quality: %+v", result.Quality)
		}
	}
}
