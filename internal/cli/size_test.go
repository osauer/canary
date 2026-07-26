package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
)

// The sizing math and its validations live in internal/risk (risk.ComputeSize);
// these tests cover the CLI wiring: flag-level fail-fast and the text renderer.
func TestRunSizeValidatesLocalInputBeforeAccountRPC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing symbol",
			args: []string{"--entry", "200", "--stop", "195"},
			want: "symbol is required",
		},
		{
			name: "invalid side",
			args: []string{"--symbol", "AAPL", "--entry", "200", "--stop", "195", "--side", "sideways"},
			want: "side must be long or short",
		},
		{
			name: "invalid entry",
			args: []string{"--symbol", "AAPL", "--entry", "0", "--stop", "195"},
			want: "entry must be > 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			env := &Env{Stdout: &bytes.Buffer{}, Stderr: &stderr}
			if code := runSize(context.Background(), env, tc.args); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr missing %q: %s", tc.want, stderr.String())
			}
		})
	}
}

// renderSizeText must surface the user-facing fields and only mention status
// when it isn't ok (avoid noise on the happy path).
func TestRenderSizeText(t *testing.T) {
	t.Parallel()

	t.Run("happy path stays quiet on status", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		r := &risk.SizeResult{
			Symbol: "AAPL", Side: "long", Entry: 207.50, Stop: 202.50,
			RiskPct: 1.0, Lot: 1, FX: 1.0, NLV: 100_000, BaseCurrency: "EUR",
			RiskBase: 1000, RiskQuote: 1000, PerShareRisk: 5,
			Shares: 200, Notional: 41500, MaxLoss: 1000, Status: "ok",
		}
		if code := renderSizeText(env, r); code != 0 {
			t.Fatalf("code = %d", code)
		}
		out := stdout.String()
		for _, want := range []string{"AAPL", "long", "EUR", "200", "Net liquidation", "Risk budget", "Per-share risk", "Notional", "Max loss"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q\n%s", want, out)
			}
		}
		if strings.Contains(out, "status:") {
			t.Errorf("happy path should not surface status line:\n%s", out)
		}
		// fx=1.0 → suppress the fx-specific row to keep output clean.
		if strings.Contains(out, "Risk in quote ccy") {
			t.Errorf("fx=1.0 should suppress Risk-in-quote-ccy row:\n%s", out)
		}
	})

	t.Run("fx != 1 surfaces quote-ccy row", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		r := &risk.SizeResult{
			Symbol: "AAPL", Side: "long", Entry: 207.50, Stop: 202.50,
			RiskPct: 1.0, Lot: 1, FX: 1.085, NLV: 100_000, BaseCurrency: "EUR",
			RiskBase: 1000, RiskQuote: 1085, PerShareRisk: 5,
			Shares: 217, Notional: 45028, MaxLoss: 1085, Status: "ok",
		}
		_ = renderSizeText(env, r)
		out := stdout.String()
		if !strings.Contains(out, "Risk in quote ccy") || !strings.Contains(out, "1.0850") {
			t.Errorf("expected Risk-in-quote-ccy row with fx 1.0850:\n%s", out)
		}
	})

	t.Run("target -> reward block rendered", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		tgt, r, rew, be := 210.0, 2.0, 2000.0, 1.0/3.0
		res := &risk.SizeResult{
			Symbol: "AAPL", Side: "long", Entry: 200, Stop: 195, Target: &tgt,
			RiskPct: 1.0, Lot: 1, FX: 1.0, NLV: 100_000, BaseCurrency: "EUR",
			RiskBase: 1000, RiskQuote: 1000, PerShareRisk: 5,
			Shares: 200, Notional: 40000, MaxLoss: 1000,
			R: &r, RewardQuote: &rew, BreakevenWinRate: &be, Status: "ok",
		}
		_ = renderSizeText(env, res)
		out := stdout.String()
		for _, want := range []string{"target 210", "Max gain at target", "Reward:risk", "2.00R", "Breakeven win rate", "33.3%"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in output:\n%s", want, out)
			}
		}
	})

	t.Run("no target -> reward block suppressed", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		res := &risk.SizeResult{
			Symbol: "AAPL", Side: "long", Entry: 200, Stop: 195,
			RiskPct: 1.0, Lot: 1, FX: 1.0, NLV: 100_000, BaseCurrency: "EUR",
			RiskBase: 1000, RiskQuote: 1000, PerShareRisk: 5,
			Shares: 200, Notional: 40000, MaxLoss: 1000, Status: "ok",
		}
		_ = renderSizeText(env, res)
		out := stdout.String()
		for _, banned := range []string{"target", "Reward:risk", "Breakeven", "Max gain at target"} {
			if strings.Contains(out, banned) {
				t.Errorf("expected reward block suppressed; found %q:\n%s", banned, out)
			}
		}
	})

	t.Run("non-ok status surfaced with hint", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		r := &risk.SizeResult{
			Symbol: "AAPL", Side: "long", Entry: 5, Stop: 4.99,
			RiskPct: 1.0, Lot: 1, FX: 1.0, NLV: 100, BaseCurrency: "EUR",
			RiskBase: 1, RiskQuote: 1, PerShareRisk: 0.01,
			Shares: 0, Notional: 0, MaxLoss: 0, Status: "tight_risk",
		}
		_ = renderSizeText(env, r)
		out := stdout.String()
		if !strings.Contains(out, "tight_risk") || !strings.Contains(out, "widen the stop") {
			t.Errorf("expected tight_risk hint in output:\n%s", out)
		}
	})
}
