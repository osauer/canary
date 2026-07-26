package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestGammaOperationalCadenceTracksCompletedOptionsSessions(t *testing.T) {
	ny := newYorkLocation()
	envelope := func(at time.Time) *rpc.GammaZeroSPXResult {
		return &rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{AsOf: at},
		}
	}
	mondayPreopen := time.Date(2026, 7, 20, 1, 5, 0, 0, ny)
	friday := time.Date(2026, 7, 17, 15, 0, 0, 0, ny)
	thursday := time.Date(2026, 7, 16, 15, 0, 0, 0, ny)

	cases := []struct {
		name string
		env  *rpc.GammaZeroSPXResult
		now  time.Time
		want string
	}{
		{name: "prior completed session before Monday open", env: envelope(friday), now: mondayPreopen, want: rpc.DataCadenceNotDue},
		{name: "older than last completed session", env: envelope(thursday), now: mondayPreopen, want: rpc.DataCadenceMissedSession},
		{name: "no last good", env: nil, now: mondayPreopen, want: rpc.DataCadenceNoLastGood},
		{name: "prior session during active options session", env: envelope(friday), now: time.Date(2026, 7, 20, 10, 0, 0, 0, ny), want: rpc.DataCadenceMissedSession},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gammaOperationalCadence(tc.env, tc.now); got != tc.want {
				t.Fatalf("cadence=%q, want %q", got, tc.want)
			}
		})
	}
	qualityEnvelope := envelope(friday)
	qualityEnvelope.Result.Quality = &rpc.GammaSignalQuality{
		Rankability: rpc.GammaRankabilityBlocked, RankabilityReason: "freshness: computed for prior session",
	}
	if health, ok := gammaStatusQualityAt(*qualityEnvelope, mondayPreopen); !ok || health.CadenceState != rpc.DataCadenceNotDue {
		t.Fatalf("status quality cadence=%+v ok=%v", health, ok)
	}
	if health, ok := gammaStatusQualityAt(rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusCold}, mondayPreopen); !ok || health.CadenceState != rpc.DataCadenceNoLastGood {
		t.Fatalf("cold status quality cadence=%+v ok=%v", health, ok)
	}
}
