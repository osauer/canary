package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// TestRenderAccount_DailyPnLBlock pins the Daily P&L row + reqPnL-stream
// totals: the headline figure sits on the hero row beneath Net liquidation;
// the account's TOTAL unrealized/realized P&L (when supplied) renders as a
// "Total P&L (reqPnL stream)" sub-block below the rest of the snapshot.
// The totals are NOT a breakdown of the daily figure — the fixture picks
// values that do not sum to dailyPnL.
func TestRenderAccount_DailyPnLBlock(t *testing.T) {
	t.Parallel()
	daily := 621.30
	unreal := -44485.00
	real_ := 1830.00
	a := &rpc.AccountResult{
		AccountID:          "U1234567",
		BaseCurrency:       "USD",
		NetLiquidation:     100000,
		DailyPnL:           &daily,
		PnLUnrealizedTotal: &unreal,
		PnLRealizedTotal:   &real_,
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderAccountText(env, a); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	// "621.30" is the hero daily value; "44,485.00" / "1,830.00" appear only
	// in the reqPnL-totals block, so they pin that block distinctly.
	for _, want := range []string{"Daily P&L", "621.30", "Total P&L (reqPnL stream)", "44,485.00", "1,830.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestRenderAccount_DailyPnLPlaceholderWhenNil — the row always renders
// (so a first-time user sees the field exists) but a nil DailyPnL paints
// a dim em-dash plus a "subscribing — value lands on next call" hint.
// This matches the lazy reqPnL kickoff documented in CHANGELOG v0.17.0:
// the first call subscribes, the next one carries values.
func TestRenderAccount_DailyPnLPlaceholderWhenNil(t *testing.T) {
	t.Parallel()
	a := &rpc.AccountResult{
		AccountID:      "U1234567",
		BaseCurrency:   "USD",
		NetLiquidation: 100000,
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderAccountText(env, a); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Daily P&L") {
		t.Errorf("Daily P&L row should always render so the field is discoverable:\n%s", out)
	}
	if !strings.Contains(out, "subscribing") {
		t.Errorf("nil Daily P&L should carry a subscribing hint:\n%s", out)
	}
	if strings.Contains(out, "Total P&L (reqPnL stream)") {
		t.Errorf("reqPnL-totals block should stay hidden when no totals are supplied:\n%s", out)
	}
}

// TestRenderAccount_DailyPnLOnlyBareDaily — older gateway versions
// emit just the bare dailyPnL field. The headline row renders; the
// reqPnL-totals sub-block stays hidden.
func TestRenderAccount_DailyPnLOnlyBareDaily(t *testing.T) {
	t.Parallel()
	daily := 50.00
	a := &rpc.AccountResult{
		AccountID:      "U1234567",
		BaseCurrency:   "USD",
		NetLiquidation: 100000,
		DailyPnL:       &daily,
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderAccountText(env, a)
	out := stdout.String()
	if !strings.Contains(out, "Daily P&L") {
		t.Errorf("Daily P&L line should render:\n%s", out)
	}
	if strings.Contains(out, "Total P&L (reqPnL stream)") {
		t.Errorf("reqPnL-totals block should be hidden on short-form payloads:\n%s", out)
	}
}

// TestRenderPositions_DayPnLColumnAlwaysVisible — the DAY P&L column
// renders unconditionally so the field is discoverable on the very
// first call. Populated rows show the value; nil rows show em-dash so
// the column shape stays honest.
func TestRenderPositions_DayPnLColumnAlwaysVisible(t *testing.T) {
	t.Parallel()
	d := 47.30
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", Quantity: 100, AvgCost: 134.20, Mark: 145.22,
				MarketValue: 14522, UnrealizedPnL: 1102, DailyPnL: &d},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	if !strings.Contains(out, "DAY P&L") {
		t.Errorf("missing DAY P&L column header:\n%s", out)
	}
	if !strings.Contains(out, "47.30") {
		t.Errorf("missing daily value:\n%s", out)
	}
}

// TestRenderPositions_DayPnLColumnRendersWhenAllNil — every row has
// DailyPnL == nil. The column still renders (always-on policy), with
// an em-dash placeholder per row. Pre-fix the column was suppressed,
// which made the field invisible on the first call after a daemon
// restart and led to "where is the daily P&L?" confusion.
func TestRenderPositions_DayPnLColumnRendersWhenAllNil(t *testing.T) {
	t.Parallel()
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", Quantity: 100, AvgCost: 134.20, Mark: 145.22,
				MarketValue: 14522, UnrealizedPnL: 1102},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	if !strings.Contains(out, "DAY P&L") {
		t.Errorf("DAY P&L column should always render:\n%s", out)
	}
	if !strings.Contains(out, "—") {
		t.Errorf("expected em-dash placeholder when DailyPnL is nil:\n%s", out)
	}
}

// TestRenderPositions_DayPnLColumnEmDashOnMixed — at least one row has
// a value and another row has nil (so it shows em-dash). Pins the
// "honest mixed state" contract.
func TestRenderPositions_DayPnLColumnEmDashOnMixed(t *testing.T) {
	t.Parallel()
	d := 100.00
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", Quantity: 100, AvgCost: 100, Mark: 105,
				MarketValue: 10500, UnrealizedPnL: 500, DailyPnL: &d},
			{Symbol: "NVDA", Quantity: 50, AvgCost: 400, Mark: 410,
				MarketValue: 20500, UnrealizedPnL: 500}, // DailyPnL nil
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	if !strings.Contains(out, "DAY P&L") {
		t.Errorf("DAY P&L column should appear:\n%s", out)
	}
	if !strings.Contains(out, "100.00") {
		t.Errorf("missing populated daily value:\n%s", out)
	}
	// NVDA's line carries an em-dash for daily — the same glyph the
	// rest of the renderer uses for nil values.
	if !strings.Contains(out, "—") {
		t.Errorf("expected em-dash for NVDA's nil daily P&L:\n%s", out)
	}
}

// TestRenderPositions_OptionsCarryDayPnL — the DAY P&L column appears
// on the options table too, sourced from the same reqPnLSingle stream
// as stocks. Pre-v0.18 there was no per-position day metric on options.
func TestRenderPositions_OptionsCarryDayPnL(t *testing.T) {
	t.Parallel()
	d := -245.00
	res := &rpc.PositionsResult{
		Options: []rpc.PositionView{
			{Symbol: "XYZ", Right: "C", Strike: 165, Expiry: "20260618",
				Quantity: 5, AvgCost: 8.11, Mark: 7.62,
				MarketValue: 3810, UnrealizedPnL: -245, DailyPnL: &d},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	if !strings.Contains(out, "DAY P&L") {
		t.Errorf("options table should carry DAY P&L column:\n%s", out)
	}
	if !strings.Contains(out, "245.00") {
		t.Errorf("missing daily value on option row:\n%s", out)
	}
}

// TestFormatPnLCcyRight_RendersSignedAndPrefixed pins the helper used by
// renderDailyPnL: positive renders with currency prefix, negative is
// preserved, exactly-zero is a real number (not an em-dash).
func TestFormatPnLCcyRight_RendersSignedAndPrefixed(t *testing.T) {
	t.Parallel()
	env := &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	// Color off so we test the bare string layer.
	if got := env.formatPnLCcyRight(1247.30, "USD", 0); !strings.Contains(got, "$ 1,247.30") {
		t.Errorf("positive USD = %q, want contains \"$ 1,247.30\"", got)
	}
	if got := env.formatPnLCcyRight(-50.00, "EUR", 0); !strings.Contains(got, "-€ 50.00") {
		t.Errorf("negative EUR = %q, want contains \"-€ 50.00\"", got)
	}
	if got := env.formatPnLCcyRight(0, "USD", 0); strings.Contains(got, "—") {
		t.Errorf("zero P&L should render as \"$ 0.00\", not em-dash: %q", got)
	}
}

// TestFormatPnLCcyPtrRight_NilEmDash pins the nil-as-em-dash path.
func TestFormatPnLCcyPtrRight_NilEmDash(t *testing.T) {
	t.Parallel()
	env := &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	got := env.formatPnLCcyPtrRight(nil, "USD", 14)
	if !strings.Contains(got, "—") {
		t.Errorf("nil should produce em-dash placeholder: %q", got)
	}
}
