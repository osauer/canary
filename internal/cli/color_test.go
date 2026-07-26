package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// ShouldColor returns false for non-*os.File writers (bytes.Buffer is the
// canonical test/output target). NO_COLOR and CANARY_COLOR=never also force
// off; CANARY_COLOR=always overrides the TTY check. The variable order
// (always > never > NO_COLOR > TTY) lets users force-on for piping into
// `less -R` without losing the global NO_COLOR opt-out elsewhere.
func TestShouldColorPolicy(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		wantFalse bool
	}{
		{"buffer alone is not a TTY", nil, true},
		{"NO_COLOR set forces off", map[string]string{"NO_COLOR": "1"}, true},
		{"CANARY_COLOR=never forces off", map[string]string{"CANARY_COLOR": "never"}, true},
		{"CANARY_COLOR=always forces on", map[string]string{"CANARY_COLOR": "always"}, false},
		{"CANARY_COLOR=always wins over NO_COLOR", map[string]string{"CANARY_COLOR": "always", "NO_COLOR": "1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := ShouldColor(&bytes.Buffer{})
			if tc.wantFalse && got {
				t.Fatalf("ShouldColor = true, want false")
			}
			if !tc.wantFalse && !got {
				t.Fatalf("ShouldColor = false, want true")
			}
		})
	}
}

// formatPnL leaves output uncolored when env.Color is off (the default for
// tests so substring assertions on column text remain stable). When on,
// positive values get green, negative red, zero dim. Padding is applied
// before the ANSI wrap so visible width is preserved regardless of color.
func TestFormatPnLColoring(t *testing.T) {
	t.Parallel()
	t.Run("color off → no escapes", func(t *testing.T) {
		env := &Env{Color: false}
		got := env.formatPnL(123.45, 0)
		if strings.Contains(got, "\x1b[") {
			t.Fatalf("expected no ANSI in %q", got)
		}
	})
	t.Run("positive → green", func(t *testing.T) {
		env := &Env{Color: true}
		got := env.formatPnL(123.45, 0)
		if !strings.HasPrefix(got, ansiGreen) || !strings.HasSuffix(got, ansiReset) {
			t.Fatalf("expected green wrap, got %q", got)
		}
	})
	t.Run("negative → red", func(t *testing.T) {
		env := &Env{Color: true}
		got := env.formatPnL(-50.00, 0)
		if !strings.HasPrefix(got, ansiRed) || !strings.HasSuffix(got, ansiReset) {
			t.Fatalf("expected red wrap, got %q", got)
		}
	})
	t.Run("zero → dim", func(t *testing.T) {
		env := &Env{Color: true}
		got := env.formatPnL(0, 0)
		if !strings.HasPrefix(got, ansiDim) || !strings.HasSuffix(got, ansiReset) {
			t.Fatalf("expected dim wrap, got %q", got)
		}
	})
	t.Run("width pads before color so visible width matches", func(t *testing.T) {
		envOff := &Env{Color: false}
		envOn := &Env{Color: true}
		off := envOff.formatPnL(123.45, 14)
		on := envOn.formatPnL(123.45, 14)
		// Stripping ANSI from `on` should give exactly `off`.
		stripped := strings.TrimSuffix(strings.TrimPrefix(on, ansiGreen), ansiReset)
		if stripped != off {
			t.Fatalf("padding mismatch:\n  off=%q\n  on (stripped)=%q", off, stripped)
		}
		if len(off) != 14 {
			t.Fatalf("uncolored padded width = %d, want 14", len(off))
		}
	})
}

// dataTypeBadge with color enabled wraps the badge in yellow; the visible
// text inside the wrap is the same as the uncolored form. This guards
// against regressions where someone changes the badge text without
// updating the color form, or vice versa.
func TestDataTypeBadgeColored(t *testing.T) {
	t.Parallel()
	env := &Env{Color: true}
	got := env.dataTypeBadge("delayed")
	if !strings.HasPrefix(got, ansiYellow) || !strings.HasSuffix(got, ansiReset) {
		t.Fatalf("expected yellow wrap, got %q", got)
	}
	if !strings.Contains(got, "data=delayed ⚠") {
		t.Fatalf("badge text missing inside wrap: %q", got)
	}
}

// formatMoneyNeg paints negative values red and zero values dim, leaving
// positive balances uncolored. The contract differs from formatPnL so
// account balance views don't look celebratory when every line is in
// the green.
func TestFormatMoneyNeg(t *testing.T) {
	t.Parallel()
	t.Run("color off → no escapes anywhere", func(t *testing.T) {
		env := &Env{Color: false}
		for _, v := range []float64{-100, 0, 100} {
			if got := env.formatMoneyNeg(v); strings.Contains(got, "\x1b[") {
				t.Errorf("color off should not emit ANSI for %v: %q", v, got)
			}
		}
	})
	t.Run("color on: negative red, zero dim, positive plain", func(t *testing.T) {
		env := &Env{Color: true}
		neg := env.formatMoneyNeg(-100)
		if !strings.HasPrefix(neg, ansiRed) {
			t.Errorf("negative should be red: %q", neg)
		}
		zero := env.formatMoneyNeg(0)
		if !strings.HasPrefix(zero, ansiDim) {
			t.Errorf("zero should be dim: %q", zero)
		}
		pos := env.formatMoneyNeg(100)
		if strings.Contains(pos, "\x1b[") {
			t.Errorf("positive should be uncolored: %q", pos)
		}
	})
}

// padDash returns the right-aligned em-dash placeholder of the requested
// VISIBLE width — em-dash counts as 1 column, not its 3 UTF-8 bytes.
// This is the contract every fixed-width numeric column relies on, so
// missing values don't shift downstream columns to the left.
func TestPadDashVisibleWidth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		w       int
		wantVis int
	}{
		{1, 1}, // just em-dash
		{6, 6},
		{10, 10},
	}
	for _, tc := range cases {
		got := padDash(tc.w)
		// Visible width = byte length minus 2 (em-dash is 3 bytes vs 1 col).
		visWidth := len(got) - 2
		if !strings.HasSuffix(got, "—") {
			t.Errorf("padDash(%d) should end with em-dash: %q", tc.w, got)
		}
		if visWidth != tc.wantVis {
			t.Errorf("padDash(%d) visible width = %d, want %d (raw=%q)", tc.w, visWidth, tc.wantVis, got)
		}
	}
}

// orDash with an int width returns a string of exactly that visible width,
// whether the value is present or nil. Pre-fix, the placeholder was 8
// visible cols regardless of requested width AND its UTF-8 byte length
// confused %Ns padding downstream — both gone now.
func TestOrDashFixedVisibleWidth(t *testing.T) {
	t.Parallel()
	v := 292.17
	got := orDash(&v, 10)
	if got != "    292.17" {
		t.Errorf("orDash with value: got %q, want %q", got, "    292.17")
	}
	if visLen(got) != 10 {
		t.Errorf("value visible width = %d, want 10: %q", visLen(got), got)
	}
	dash := orDash(nil, 10)
	if visLen(dash) != 10 {
		t.Errorf("nil visible width = %d, want 10: %q", visLen(dash), dash)
	}
	if !strings.HasSuffix(dash, "—") {
		t.Errorf("nil should be right-aligned em-dash: %q", dash)
	}
}

// visLen counts visible terminal columns for ASCII + em-dash strings.
// Em-dash is 3 UTF-8 bytes but 1 column; everything else here is 1:1.
func visLen(s string) int {
	return len([]rune(s))
}

// Quote snapshot header and data rows must end at the same column boundaries
// regardless of whether values are present. Pre-fix, an all-nil row was
// 8 chars short on each numeric column because the em-dash placeholder
// was undersized and the header was hand-spaced incorrectly.
func TestQuoteSnapshotAlignment(t *testing.T) {
	t.Parallel()
	bid := 292.17
	ask := 292.31
	last := 292.17
	bidSz := 100
	askSz := 200
	vol := int64(12_000_000)
	iv := 0.284
	qFull := rpc.Quote{Symbol: "AAPL", Bid: &bid, Ask: &ask, Last: &last, BidSize: &bidSz, AskSize: &askSz, Volume: &vol, IV: &iv, DataType: "live"}
	qNil := rpc.Quote{Symbol: "TSLA", DataType: "live"}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: false}
	if code := renderQuoteSnapshotText(env, []rpc.Quote{qFull, qNil}); code != 0 {
		t.Fatalf("render: code=%d", code)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	// First line is blank (Fprintln(out)), then header, rule, 2 data rows.
	if len(lines) < 5 {
		t.Fatalf("expected 5 lines, got %d:\n%s", len(lines), stdout.String())
	}
	header := lines[1]
	rowFull := lines[3]
	rowNil := lines[4]
	if visLen(header) != visLen(rowFull) {
		t.Errorf("header (%d) vs full row (%d) width mismatch:\n%s\n%s", visLen(header), visLen(rowFull), header, rowFull)
	}
	if visLen(header) != visLen(rowNil) {
		t.Errorf("header (%d) vs nil row (%d) width mismatch:\n%s\n%s", visLen(header), visLen(rowNil), header, rowNil)
	}
}

// Chain table data rows must be the same visible width regardless of
// whether the strike has quotes — pre-fix, fmt2/fmtPct returned a 5-col
// placeholder vs a 6-col value, shifting downstream columns left whenever
// a leg was illiquid. Also verifies the column-2 header right-aligns its
// labels over the data.
func TestChainAlignment(t *testing.T) {
	t.Parallel()
	cb := 5.10
	ca := 5.30
	cl := 5.20
	civ := 0.42
	pb := 4.80
	pa := 5.00
	pl := 4.90
	piv := 0.40

	res := &rpc.ChainResult{
		Symbol: "AAPL", Spot: 200.0, Expiry: "2026-06-19", DTE: 30, DataType: "live",
		Strikes: []rpc.ChainStrike{
			{Strike: 195, CallBid: &cb, CallAsk: &ca, CallLast: &cl, CallIV: &civ, PutBid: &pb, PutAsk: &pa, PutLast: &pl, PutIV: &piv},
			{Strike: 200, IsATM: true}, // empty leg — placeholder must be 6-wide
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: false}
	if code := renderChainText(env, res); code != 0 {
		t.Fatalf("render: code=%d", code)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	// blank, header (symbol/spot), blank, group-header (CALLS/PUTS),
	// col-header (BID/ASK/...), rule, 2 strike rows, ...
	if len(lines) < 8 {
		t.Fatalf("expected >=8 lines, got %d:\n%s", len(lines), stdout.String())
	}
	colHeader := lines[4]
	rowFull := lines[6]
	rowEmpty := lines[7]
	// All three lines must end at the same visible column. The trailing
	// " ← ATM" marker only attaches to ATM strikes, so trim it before
	// comparing widths.
	emptyClean := strings.TrimSuffix(rowEmpty, " ← ATM")
	if visLen(colHeader) != visLen(rowFull) {
		t.Errorf("col header (%d) vs full row (%d) width mismatch:\n%s\n%s", visLen(colHeader), visLen(rowFull), colHeader, rowFull)
	}
	if visLen(colHeader) != visLen(emptyClean) {
		t.Errorf("col header (%d) vs empty-leg row (%d) width mismatch:\n%s\n%s", visLen(colHeader), visLen(emptyClean), colHeader, emptyClean)
	}
	// CALLS / PUTS / STRIKE labels all present in the group-header line.
	groupHeader := lines[3]
	for _, want := range []string{"CALLS", "PUTS"} {
		if !strings.Contains(groupHeader, want) {
			t.Errorf("group header missing %q:\n%s", want, groupHeader)
		}
	}
}

// History header must end at the same column as a data row, so OPEN/HIGH/
// LOW/CLOSE labels sit precisely above the numbers.
func TestHistoryAlignment(t *testing.T) {
	t.Parallel()
	res := &rpc.HistoryDailyResult{
		Symbol: "AAPL", Days: 5,
		Bars: []rpc.HistoryBar{
			{Date: "2026-05-01", Open: 200.50, High: 205.10, Low: 199.95, Close: 203.00, Volume: 12_000_000},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: false}
	if code := renderHistoryText(env, res); code != 0 {
		t.Fatalf("render: code=%d", code)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	// blank, summary, blank, header, rule, row
	if len(lines) < 6 {
		t.Fatalf("expected >=6 lines, got %d:\n%s", len(lines), stdout.String())
	}
	header := lines[3]
	row := lines[5]
	// Header right-aligns OPEN to col 23 (the right edge of the OPEN column).
	if !strings.Contains(header, "      OPEN") {
		t.Errorf("expected right-aligned OPEN label, got: %q", header)
	}
	// "200.50" should appear right-aligned to the same right edge as "OPEN".
	openIdx := strings.Index(header, "OPEN")
	rowOpenEnd := strings.Index(row, "200.50") + len("200.50")
	if rowOpenEnd != openIdx+len("OPEN") {
		t.Errorf("OPEN label end (%d) does not align with data end (%d):\n%s\n%s",
			openIdx+len("OPEN"), rowOpenEnd, header, row)
	}
}

// Account view colors negative balances red and zero balances dim, while
// positive balances stay uncolored. With color off no ANSI appears at all.
func TestAccountColoring(t *testing.T) {
	t.Parallel()
	res := &rpc.AccountResult{
		AccountID: "U1", BaseCurrency: "USD",
		NetLiquidation: 100_000, BuyingPower: 50_000,
		AvailableFunds: 25_000, ExcessLiquidity: 30_000,
		TotalCash:         -5_000, // margin debit
		MaintenanceMargin: 0,      // dim placeholder when zero
		InitialMargin:     20_000,
	}
	t.Run("color off", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: false}
		_ = renderAccountText(env, res)
		if strings.Contains(stdout.String(), "\x1b[") {
			t.Fatalf("expected no ANSI when color off:\n%s", stdout.String())
		}
	})
	t.Run("color on", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
		_ = renderAccountText(env, res)
		out := stdout.String()
		// The negative TotalCash row must carry the red wrap.
		if !strings.Contains(out, ansiRed) {
			t.Errorf("expected red wrap on negative TotalCash:\n%s", out)
		}
		// Zero MaintenanceMargin renders as the dim em-dash placeholder.
		if !strings.Contains(out, ansiDim) {
			t.Errorf("expected dim wrap on zero MaintenanceMargin:\n%s", out)
		}
		// Positive balances must NOT be green-painted (they're not P&L).
		if strings.Contains(out, ansiGreen) {
			t.Errorf("balance view must not paint positives green:\n%s", out)
		}
	})
}

// Run on an unknown subcommand prints the full top-level usage to stderr
// and returns exit code 2. This guards the "discoverability on typo"
// behavior — pre-fix only the bare error line was shown and users had
// to know to type `canary --help` next.
func TestRunUnknownPrintsUsage(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "quotee", nil)
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
	out := stderr.String()
	if !strings.Contains(out, `unknown subcommand "quotee"`) {
		t.Errorf("missing error line:\n%s", out)
	}
	if !strings.Contains(out, "Subcommands:") {
		t.Errorf("expected full usage in stderr:\n%s", out)
	}
	if !strings.Contains(out, "canary <subcommand> --help") {
		t.Errorf("expected --help hint in stderr:\n%s", out)
	}
}

// PrintUsage advertises the per-command --help, the --json switch, and
// the color env-var policy. These are the discoverability hooks the
// patch-release help rework was meant to add.
func TestPrintUsageMentionsDiscoveryHints(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	PrintUsage(&w)
	out := w.String()
	for _, want := range []string{
		"canary <subcommand> --help",
		"--json",
		"NO_COLOR",
		"CANARY_COLOR",
		"canary status",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PrintUsage missing %q:\n%s", want, out)
		}
	}
}

// Positions table emits ANSI on the P&L columns when color is enabled, and
// emits NO ANSI when color is off. The off-mode assertion guards every
// other test in this package — they all rely on Color=false producing
// substring-stable output.
func TestRenderPositionsHonorsColorFlag(t *testing.T) {
	t.Parallel()
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AAPL", Quantity: 100, AvgCost: 192, Mark: 207, MarketValue: 20700, UnrealizedPnL: 1500},
			{Symbol: "TSLA", Quantity: 50, AvgCost: 250, Mark: 230, MarketValue: 11500, UnrealizedPnL: -1000},
		},
	}
	t.Run("color off → plain text", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: false}
		_ = renderPositionsText(env, res)
		if strings.Contains(stdout.String(), "\x1b[") {
			t.Fatalf("unexpected ANSI escapes in output:\n%s", stdout.String())
		}
	})
	t.Run("color on → green for gain, red for loss", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
		_ = renderPositionsText(env, res)
		out := stdout.String()
		if !strings.Contains(out, ansiGreen) {
			t.Errorf("expected green for AAPL gain:\n%s", out)
		}
		if !strings.Contains(out, ansiRed) {
			t.Errorf("expected red for TSLA loss:\n%s", out)
		}
	})
}
