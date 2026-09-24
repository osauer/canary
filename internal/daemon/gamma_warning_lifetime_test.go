package daemon

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestGammaTypedWarningsSurviveCacheRehydration(t *testing.T) {
	codes := slices.Clone(gammaPlainWarningCodes)
	codes = append(codes, "expiries_stale:2d", "expiries_stale:4d", "expiries_stale:9999d", "skew_fallback:20260925", "skew_fallback:spy:20260925", "skew_fallback:spx:20260925", "skew_fallback:spxw:20260925")
	for _, prefix := range gammaFailureWarningPrefixes {
		for _, failure := range gammaFailureTokens {
			codes = append(codes, prefix+failure)
		}
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			c := &rpc.GammaZeroComputed{Warnings: []string{code}}
			for range 3 {
				c = hydrateGammaComputed(c)
				if !slices.Equal(c.Warnings, []string{code}) || len(c.WarningDetails) != 1 || c.WarningDetails[0].Code != code {
					t.Fatalf("typed warning lost during cache hydration: warnings=%v details=%+v", c.Warnings, c.WarningDetails)
				}
				raw, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				c = new(rpc.GammaZeroComputed)
				if err := json.Unmarshal(raw, c); err != nil {
					t.Fatal(err)
				}
			}
			q := new(rpc.GammaSignalQuality)
			gammaQualityWarningGates(q, c)
			want := rpc.GammaQualityGateBlock
			if code == "expiries_stale:2d" {
				want = rpc.GammaQualityGatePass
			}
			if code == "expiries_stale:4d" {
				want = rpc.GammaQualityGateContext
			}
			if !slices.Contains([]string{"expiries_stale:2d", "expiries_stale:4d", "session_closed_no_cache", "persisted_cache_rejected", "unclassified_data_warning"}, code) {
				return
			}
			if len(q.Gates) != 1 || q.Gates[0].Status != want {
				t.Fatalf("quality policy changed for %s: %+v", code, q.Gates)
			}
		})
	}
}

func TestGammaPollutedCacheCannotInventRecovery(t *testing.T) {
	for _, warnings := range [][]string{{"expiries_stale:2d", "unclassified_data_warning"}, {"arbitrary upstream prose"}} {
		c := hydrateGammaComputed(&rpc.GammaZeroComputed{Warnings: warnings})
		q := new(rpc.GammaSignalQuality)
		gammaQualityWarningGates(q, c)
		found := false
		for _, gate := range q.Gates {
			if gate.Name == "warning_contract" && gate.Status == rpc.GammaQualityGateBlock {
				found = true
			}
		}
		if !found {
			t.Fatalf("unclassified failure erased: %+v", c.Warnings)
		}
	}
}
