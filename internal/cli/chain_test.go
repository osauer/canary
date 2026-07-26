package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// fmtOI renders open interest compactly for the chain table. Mirrors the
// abbreviation policy used for bid/ask sizes (formatSize), but with the
// chain's "0 = unavailable" convention so empty cells match how zero
// bid/ask render in the same row.
func TestFmtOI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "—"},
		{-1, "—"},
		{1, "1"},
		{42, "42"},
		{999, "999"},
		{1234, "1.2K"},
		{9999, "10.0K"},
		{12345, "12K"},
		{999_999, "999K"},
		{1_234_567, "1.2M"},
		{12_500_000, "12M"},
	}
	for _, tc := range cases {
		got := fmtOI(tc.in)
		if got != tc.want {
			t.Errorf("fmtOI(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRunChainValidatesLocalFlagsBeforeRPC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "invalid side",
			args: []string{"--expiry", "2026-06-19", "--side", "sideways", "AAPL"},
			want: "--side must be calls, puts, or both",
		},
		{
			name: "negative width",
			args: []string{"--expiry", "2026-06-19", "--width", "-1", "AAPL"},
			want: "--width must be >= 0",
		},
		{
			name: "negative dte filter",
			args: []string{"--min-dte", "-1", "AAPL"},
			want: "--min-dte, --max-dte, and --target-dte must be >= 0",
		},
		{
			name: "dte filter only applies to expiry list",
			args: []string{"--expiry", "2026-06-19", "--target-dte", "120", "AAPL"},
			want: "DTE filters only apply when --expiry is omitted",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			env := &Env{Stdout: &bytes.Buffer{}, Stderr: &stderr}
			if code := runChain(context.Background(), env, tc.args); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr missing %q: %s", tc.want, stderr.String())
			}
		})
	}
}

func TestRenderChainDecisionSummary(t *testing.T) {
	t.Parallel()
	spread := 0.12
	res := &rpc.ChainResult{
		Symbol: "IREN", Spot: 55, Expiry: "2026-09-18", DTE: 115,
		TradableSummary: &rpc.ChainTradableSummary{
			TotalLegs:       6,
			LiveBidAskLegs:  0,
			OICoveragePct:   0.33,
			OptionsTradable: false,
			FeedGap:         "thin_contract",
		},
		LiquiditySummary: &rpc.ChainLiquiditySummary{
			LiquidityGrade:           "untradable",
			ATMSpreadPct:             &spread,
			RecommendedStructureHint: "untradable_chain",
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderChainDecisionSummary(env, res)
	out := stdout.String()
	for _, want := range []string{"Tradability", "0/6 live bid/ask", "not executable", "thin_contract", "Liquidity", "untradable chain"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestFmtIVQuality(t *testing.T) {
	t.Parallel()
	if got := fmtIVQuality(rpc.ChainExpiry{IVQuality: "reused_fallback"}); got != "reused_fallback" {
		t.Fatalf("quality = %q, want reused_fallback", got)
	}
	if got := fmtIVQuality(rpc.ChainExpiry{IVSource: "cached"}); got != "cached" {
		t.Fatalf("source fallback = %q, want cached", got)
	}
	if got := fmtIVQuality(rpc.ChainExpiry{IVStatus: "timeout"}); got != "unavailable" {
		t.Fatalf("timeout quality = %q, want unavailable", got)
	}
}

func TestFmtChainSpotSource(t *testing.T) {
	t.Parallel()
	if got := fmtChainSpotSource("prev_close"); got != " (prev close)" {
		t.Fatalf("prev_close source = %q", got)
	}
	if got := fmtChainSpotSource("historical_close"); got != " (hist close)" {
		t.Fatalf("historical_close source = %q", got)
	}
	if got := fmtChainSpotSource("last"); got != "" {
		t.Fatalf("live source should not render suffix, got %q", got)
	}
}
