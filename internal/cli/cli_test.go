package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/osauer/canary/v2/internal/rpc"
)

// fail() should append a troubleshooting hint when the underlying daemon
// error carries the gateway_unavailable code prefix. Other error shapes
// pass through unchanged.
func TestFailAppendsHintForGatewayUnavailable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		format     string
		args       []any
		wantHint   bool
		wantSubstr string
	}{
		{
			name:       "gateway unavailable triggers hint",
			format:     "account: %v",
			args:       []any{"gateway_unavailable: IBKR connection unavailable"},
			wantHint:   true,
			wantSubstr: "gateway_unavailable",
		},
		{
			name:     "bad request does not trigger hint",
			format:   "scan: %v",
			args:     []any{"bad_request: unknown preset"},
			wantHint: false,
		},
		{
			name:     "internal error does not trigger hint",
			format:   "%s",
			args:     []any{"internal: marshal failed"},
			wantHint: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			env := &Env{Stdout: &bytes.Buffer{}, Stderr: &stderr}
			code := fail(env, tc.format, tc.args...)
			if code != 1 {
				t.Fatalf("expected exit code 1, got %d", code)
			}
			out := stderr.String()
			if tc.wantSubstr != "" && !strings.Contains(out, tc.wantSubstr) {
				t.Fatalf("stderr missing %q: %s", tc.wantSubstr, out)
			}
			hasHint := strings.Contains(out, "canary status")
			if hasHint != tc.wantHint {
				t.Fatalf("wantHint=%v, gotHint=%v, stderr=%q", tc.wantHint, hasHint, out)
			}
		})
	}
}

// hoistFlags moves -flag tokens (and their values) ahead of positional args
// so users can write `canary quote AAPL --json`.
func TestHoistFlagsReordersValueFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "trailing --json hoisted",
			in:   []string{"AAPL", "--json"},
			want: []string{"--json", "AAPL"},
		},
		{
			name: "value flag with following value hoisted as pair",
			in:   []string{"AAPL", "--expiry", "2026-06-19"},
			want: []string{"--expiry", "2026-06-19", "AAPL"},
		},
		{
			name: "gamma only value flag hoisted as pair",
			in:   []string{"--only", "spy", "--no-wait"},
			want: []string{"--only", "spy", "--no-wait"},
		},
		{
			name: "size target value flag hoisted as pair",
			in:   []string{"--symbol", "SPY", "--entry", "740", "--stop", "720", "--target", "780", "--risk-pct", "0.5"},
			want: []string{"--symbol", "SPY", "--entry", "740", "--stop", "720", "--target", "780", "--risk-pct", "0.5"},
		},
		{
			name: "value flag with = form hoisted as one token",
			in:   []string{"AAPL", "--expiry=2026-06-19"},
			want: []string{"--expiry=2026-06-19", "AAPL"},
		},
		{
			name: "quote market flag hoisted as pair",
			in:   []string{"MBG", "--market", "de", "--json"},
			want: []string{"--market", "de", "--json", "MBG"},
		},
		{
			name: "calendar date and next flags hoisted as pairs",
			in:   []string{"SPY", "--date", "2026-05-25", "--next", "3"},
			want: []string{"--date", "2026-05-25", "--next", "3", "SPY"},
		},
		{
			name: "chain dte filters hoisted as pairs",
			in:   []string{"SPY", "--min-dte", "90", "--max-dte", "180", "--target-dte", "120"},
			want: []string{"--min-dte", "90", "--max-dte", "180", "--target-dte", "120", "SPY"},
		},
		{
			name: "orders history range flags hoisted as pairs",
			in:   []string{"history", "--since", "2026-06-18", "--until", "2026-06-19", "--limit", "5", "--event-limit", "10"},
			want: []string{"--since", "2026-06-18", "--until", "2026-06-19", "--limit", "5", "--event-limit", "10", "history"},
		},
		{
			name: "flag before positional preserved",
			in:   []string{"--json", "AAPL"},
			want: []string{"--json", "AAPL"},
		},
		{
			name: "purely positional unchanged",
			in:   []string{"AAPL", "MSFT"},
			want: []string{"AAPL", "MSFT"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hoistFlags(tc.in)
			if !equalSlice(got, tc.want) {
				t.Fatalf("hoistFlags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// dataTypeBadge stays silent on the happy path (live/empty) and surfaces
// only when the data is delayed/frozen. Color is left off so the visible
// text is the bare badge — color wrapping is covered separately.
func TestDataTypeBadgeQuietOnLive(t *testing.T) {
	t.Parallel()
	env := &Env{}
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"live", ""},
		{"delayed", "data=delayed ⚠"},
		{"frozen", "data=frozen ⚠"},
		{"delayed-frozen", "data=delayed-frozen ⚠"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := env.dataTypeBadge(tc.in); got != tc.want {
				t.Fatalf("dataTypeBadge(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// lookupCommand must find every entry in commands, with no extras.
func TestCommandRegistryConsistency(t *testing.T) {
	t.Parallel()
	for _, c := range commands {
		got, ok := lookupCommand(c.Name)
		if !ok {
			t.Errorf("lookupCommand missing %q", c.Name)
			continue
		}
		if got.Summary != c.Summary {
			t.Errorf("%s: summary mismatch", c.Name)
		}
	}
	if _, ok := lookupCommand("definitely-not-a-command"); ok {
		t.Errorf("lookupCommand returned true for unknown name")
	}
	// status must be the first entry — the help table ordering is load-bearing
	// for the "run this first" UX.
	if commands[0].Name != "status" {
		t.Errorf("commands[0] = %q, want status", commands[0].Name)
	}
}

// The help listing is the discovery surface: every command must appear
// exactly once, under its own group, on a line that fits an 80-column
// terminal without wrapping.
func TestPrintUsageGroupsEveryCommandOnOneLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	PrintUsage(&buf)

	group := ""
	listed := map[string]string{}
	for line := range strings.SplitSeq(buf.String(), "\n") {
		for _, spec := range HelpGroups() {
			if strings.HasPrefix(line, spec.Title+" — ") {
				group = string(spec.Group)
			}
		}
		fields := strings.Fields(line)
		if !strings.HasPrefix(line, "  ") || len(fields) == 0 {
			continue
		}
		if _, known := lookupCommand(fields[0]); !known {
			continue
		}
		if width := utf8.RuneCountInString(line); width > 80 {
			t.Errorf("%s wraps an 80-column terminal at %d columns: %q", fields[0], width, line)
		}
		if previous, dupe := listed[fields[0]]; dupe {
			t.Errorf("%s is listed twice (%s and %s)", fields[0], previous, group)
		}
		listed[fields[0]] = group
	}

	for _, spec := range Catalog() {
		got, ok := listed[spec.Name]
		if !ok {
			t.Errorf("%s is missing from the help listing", spec.Name)
			continue
		}
		if got != string(spec.Group) {
			t.Errorf("%s listed under %q, catalog says %q", spec.Name, got, spec.Group)
		}
	}
}

func TestRetiredCanaryCommandIsRejected(t *testing.T) {
	t.Parallel()
	if command, ok := lookupCommand("canary"); ok {
		t.Fatalf("retired command lookup = %+v, true; want absent", command)
	}
	if IsKnown("canary") {
		t.Fatal("retired canary command passed the known-command gate")
	}
	for _, command := range Commands() {
		if command.Name == "canary" {
			t.Fatal("retired canary command leaked into the advertised registry")
		}
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), &Env{Stdout: &stdout, Stderr: &stderr}, "canary", nil)
	if code != 2 || !strings.Contains(stderr.String(), `unknown subcommand "canary"`) {
		t.Fatalf("retired command exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// Run must intercept --help and -h, exit 0, and not invoke the handler.
func TestRunInterceptsHelp(t *testing.T) {
	t.Parallel()
	for _, helpFlag := range []string{"--help", "-h", "-help"} {
		t.Run(helpFlag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr}
			code := Run(context.Background(), env, "account", []string{helpFlag})
			if code != 0 {
				t.Fatalf("Run(account, %s) = %d, want 0", helpFlag, code)
			}
			out := stdout.String()
			if !strings.Contains(out, "canary account") {
				t.Errorf("help output missing `canary account` header: %q", out)
			}
			if !strings.Contains(out, "Account summary") {
				t.Errorf("help output missing summary: %q", out)
			}
			if stderr.Len() != 0 {
				t.Errorf("expected no stderr on --help, got %q", stderr.String())
			}
		})
	}
}

// formatTLSField must surface fallback when configured ≠ negotiated, so the
// user can spot a silent downgrade attempt or a successful TLS upgrade,
// and learn whether the endpoint was pinned in config or auto-discovered.
func TestFormatGatewayBadge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		want string
	}{
		{"disconnected, pinned tls", rpc.HealthResult{Connected: false, GatewayTLS: true, PortOrigin: "pinned"}, "(tls=true, pinned)"},
		{"connected, matching false, discovered", rpc.HealthResult{Connected: true, GatewayTLS: false, NegotiatedTLS: false, PortOrigin: "discovered"}, "(tls=false, discovered)"},
		{"connected, matching true, pinned", rpc.HealthResult{Connected: true, GatewayTLS: true, NegotiatedTLS: true, PortOrigin: "pinned"}, "(tls=true, pinned)"},
		{"fallback up to TLS", rpc.HealthResult{Connected: true, GatewayTLS: false, NegotiatedTLS: true, PortOrigin: "discovered"}, "(tls=true, configured=false ⚠ fallback, discovered)"},
		{"empty origin (legacy daemon)", rpc.HealthResult{Connected: true, GatewayTLS: false, NegotiatedTLS: false}, "(tls=false)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatGatewayBadge(tc.in); got != tc.want {
				t.Fatalf("formatGatewayBadge = %q, want %q", got, tc.want)
			}
		})
	}
}

// formatSize renders bid/ask sizes and volume compactly. Zero/nil → "—";
// numbers are abbreviated K/M past the threshold so columns stay legible.
func TestFormatSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   *int
		want string
	}{
		{nil, "—"},
		{new(0), "—"},
		{new(-5), "—"},
		{new(42), "42"},
		{new(999), "999"},
		{new(1200), "1.2K"},
		{new(12345), "12K"},
		{new(1_400_000), "1.4M"},
		{new(12_500_000), "12M"},
	}
	for _, tc := range cases {
		got := formatSize(tc.in)
		if got != tc.want {
			t.Errorf("formatSize(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderQuoteSnapshotIncludesSizeColumns(t *testing.T) {
	t.Parallel()
	bid, ask, last := 207.85, 207.88, 207.87
	bidSz, askSz := 100, 200
	vol := int64(12_400_000)
	q := rpc.Quote{
		Symbol: "AAPL", Bid: &bid, Ask: &ask, Last: &last,
		BidSize: &bidSz, AskSize: &askSz, Volume: &vol,
		IVStatus: "unavailable", DataType: "live",
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderQuoteSnapshotText(env, []rpc.Quote{q}); code != 0 {
		t.Fatalf("renderQuoteSnapshotText: code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{"BID_SZ", "ASK_SZ", "VOLUME", "100", "200", "12M"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// renderPositionsByUnderlying produces one block per underlying showing
// stock + option legs and a Group line. Pure-options underlyings omit the
// Stock line.
func TestRenderPositionsByUnderlying(t *testing.T) {
	t.Parallel()
	stock := rpc.PositionView{Symbol: "AAPL", Quantity: 100, AvgCost: 192, Mark: 207, MarketValue: 20700, UnrealizedPnL: 1500}
	opt := rpc.PositionView{Symbol: "AAPL", Right: "C", Strike: 215, Expiry: "20260619", Quantity: 5, MarketValue: 4700, UnrealizedPnL: 1290}
	tslaPut := rpc.PositionView{Symbol: "TSLA", Right: "P", Strike: 200, Expiry: "20260516", Quantity: 2, MarketValue: 800, UnrealizedPnL: -90}
	res := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{
			{Underlying: "AAPL", Stock: &stock, Options: []rpc.PositionView{opt}, GroupMarketValue: 25400, GroupUnrealizedPnL: 2790},
			{Underlying: "TSLA", Options: []rpc.PositionView{tslaPut}, GroupMarketValue: 800, GroupUnrealizedPnL: -90},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderPositionsByUnderlying(env, res); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "AAPL") || !strings.Contains(out, "TSLA") {
		t.Errorf("missing underlying headers:\n%s", out)
	}
	if !strings.Contains(out, "Stock") {
		t.Errorf("AAPL block missing Stock leg:\n%s", out)
	}
	// TSLA has no stock leg — no "Stock" line should appear under it.
	tslaIdx := strings.Index(out, "TSLA")
	if tslaIdx < 0 {
		t.Fatalf("TSLA block not found")
	}
	tslaBlock := out[tslaIdx:]
	if strings.Contains(tslaBlock, "Stock  ") {
		t.Errorf("TSLA pure-options group should not have Stock line:\n%s", tslaBlock)
	}
	// Multi-leg AAPL group renders a Total row; single-leg TSLA does not.
	if !strings.Contains(out, "Total") {
		t.Errorf("multi-leg AAPL group should carry a Total row:\n%s", out)
	}
}

// The DAY $ column on the flat positions view is gone (v0.18+) — DAY P&L
// (sourced from reqPnLSingle) is the single per-position day metric so
// stocks and options answer "today's P&L" the same way. The DayChangeMoney
// / DayChangePct fields stay on the wire (JSON consumers may still want
// them) and the by-underlying view renders them in CHANGE/GREEKS.
func TestRenderPositions_DayDollarColumnRemoved(t *testing.T) {
	t.Parallel()
	money, pct := 142.00, 0.99
	d := 142.00
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", Quantity: 100, AvgCost: 134.20, Mark: 145.22,
				DayChangeMoney: &money, DayChangePct: &pct,
				MarketValue: 14522, UnrealizedPnL: 1102, DailyPnL: &d},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	if strings.Contains(out, "DAY $") {
		t.Errorf("flat view should no longer carry the DAY $ column:\n%s", out)
	}
	if !strings.Contains(out, "DAY P&L") {
		t.Errorf("flat view should carry the unified DAY P&L column:\n%s", out)
	}
}

func TestRenderPositionsShowsProtectionCoverage(t *testing.T) {
	t.Parallel()
	unprotected := 7500.0
	res := &rpc.PositionsResult{
		Portfolio: &rpc.PositionsPortfolio{},
		ProtectionCoverage: &rpc.ProtectionCoverageSummary{
			Status:                          "review",
			UnprotectedNotionalBase:         &unprotected,
			UnprotectedNotionalBaseCurrency: "USD",
			Counts:                          rpc.ProtectionCoverageCounts{Unprotected: 1, OrphanedOrder: 1},
			LargestUnprotected:              []rpc.ProtectionCoverageRow{{Underlying: "MSFT"}},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderPortfolioSummaryTo(env, &stdout, res)
	out := stdout.String()
	for _, want := range []string{"Protection coverage", "unprotected $ 7,500.00", "1 orphaned", "largest MSFT"} {
		if !strings.Contains(out, want) {
			t.Fatalf("positions summary missing %q:\n%s", want, out)
		}
	}
}

func TestRenderPositionsStockQuoteContext(t *testing.T) {
	t.Parallel()
	loc := mustTestLocation(t, "America/New_York")
	nextOpen := time.Date(2026, 5, 26, 9, 30, 0, 0, loc)
	daily := 48.00
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{
				Symbol: "AAPL", SecType: rpc.SecTypeStock, Currency: "USD",
				Quantity: 25, AvgCost: 188.00, Mark: 190.12,
				DataType: rpc.MarketDataDelayed, PriceSource: "last",
				RegularClose: new(188.20), RegularCloseAt: time.Date(2026, 5, 22, 16, 0, 0, 0, loc),
				PriorRegularClose: new(186.28), RegularChange: new(1.92), RegularChangePct: new(1.03),
				QuotePrice: new(190.12), QuotePriceSource: "last", QuotePriceAt: time.Date(2026, 5, 22, 16, 1, 2, 0, loc),
				QuoteChangePct: new(1.02),
				PrevClose:      new(188.20),
				DayChange:      new(1.92),
				DayChangePct:   new(1.02),
				DayLow:         new(187.55), DayHigh: new(191.30), Week52Low: new(164.08), Week52High: new(199.62),
				Volume: new(int64(41762007)), AvgVolume: new(int64(58900000)),
				PriceAt: time.Date(2026, 5, 22, 16, 1, 2, 0, loc),
				Stale:   true,
				SessionContext: &rpc.MarketSession{
					Label:    "US equities",
					Timezone: "America/New_York",
					State:    "holiday",
					IsOpen:   false,
					Reason:   "Memorial Day",
					NextOpen: &nextOpen,
				},
				MarketValue: 4753.00, UnrealizedPnL: 53.00, DailyPnL: &daily,
			},
		},
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
	_ = renderPositionsTextTo(env, &stdout, res, true)
	out := stdout.String()
	for _, want := range []string{
		"SYMBOL", "POS", "CCY", "MARK", "CLOSE", "CHG", "CHG%", "QUOTE", "PRIOR", "DAY", "52W", "VOL/AVG", "DATA", "AS OF",
		"AAPL", "25 sh", "USD", "190.12", "188.20", "+1.92", "+1.02%", "186.28",
		"187.55-191.30", "164.08-199.62", "41.8M/58.9M", "stale", "closed close May22 / quote May22 16:01 EDT",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("positions quote output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Memorial Day") || strings.Contains(out, "next open") {
		t.Fatalf("positions should not print per-row calendar prose:\n%s", out)
	}
	if !strings.Contains(out, ansiGreen) {
		t.Fatalf("positive movement should be green:\n%q", out)
	}
}

func TestRenderPositionsDefaultOmitsWideQuoteColumns(t *testing.T) {
	t.Parallel()
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AAPL", Currency: "USD", Quantity: 25, AvgCost: 188.00, Mark: 190.12, MarketValue: 4753.00},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	for _, want := range []string{"DATA", "AS OF", "pos"} {
		if !strings.Contains(out, want) {
			t.Fatalf("default positions output missing %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"VOL/AVG", "52W"} {
		if strings.Contains(out, notWant) {
			t.Fatalf("default positions output should omit wide quote column %q:\n%s", notWant, out)
		}
	}
}

// Realized P&L is rendered only when at least one row carries a non-zero
// value, otherwise the column is omitted to avoid dead width.
func TestPositionsRealizedColumnOnlyShownWhenNonZero(t *testing.T) {
	t.Parallel()
	t.Run("all zero realized → no column", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		res := &rpc.PositionsResult{
			Stocks: []rpc.PositionView{
				{Symbol: "AAPL", Quantity: 100, AvgCost: 192, Mark: 207, MarketValue: 20700, UnrealizedPnL: 1500},
			},
		}
		_ = renderPositionsText(env, res)
		// "UNREAL" contains "REAL" as a substring, so look for the realized
		// header as a full table column.
		if strings.Contains(stdout.String(), "  REAL  DATA") {
			t.Errorf("expected REAL P&L column to be hidden when all zero:\n%s", stdout.String())
		}
	})
	t.Run("one non-zero realized → column present", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		res := &rpc.PositionsResult{
			Stocks: []rpc.PositionView{
				{Symbol: "AAPL", Quantity: 100, AvgCost: 192, Mark: 207, MarketValue: 20700, UnrealizedPnL: 1500, RealizedPnL: 220.50},
			},
		}
		_ = renderPositionsText(env, res)
		out := stdout.String()
		if !strings.Contains(out, "  REAL  DATA") {
			t.Errorf("expected REAL P&L column when row carries non-zero:\n%s", out)
		}
		if !strings.Contains(out, "220.50") {
			t.Errorf("expected realized value rendered:\n%s", out)
		}
	})
}

// The by-underlying view shows the Summary aggregate block (effective
// delta, dollar delta, theta, gamma, vega, FX sensitivity, greeks
// coverage) at the bottom — same as the flat positions view — so the
// conclusion numbers are visible in both layouts.
func TestRenderPositionsByUnderlying_IncludesSummary(t *testing.T) {
	t.Parallel()
	delta, theta := 1847.0, -42.18
	res := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{
			{
				Underlying:         "AAPL",
				Options:            []rpc.PositionView{{Symbol: "AAPL", Right: "C", Strike: 210, Expiry: "20251219", Quantity: 1, Mark: 5}},
				GroupMarketValue:   500,
				GroupUnrealizedPnL: 0,
			},
		},
		Portfolio: &rpc.PositionsPortfolio{
			EffectiveDelta: &delta,
			DailyTheta:     &theta,
			GreeksCoverage: 1,
			GreeksTotal:    1,
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsByUnderlying(env, res)
	out := stdout.String()
	for _, want := range []string{"Summary", "Effective delta", "+1,847.0", "Daily theta", "Greeks coverage", "1 / 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// Full-coverage Greeks gets a positive checkmark line (not the partial-
// coverage caveat). The checkmark is green when Color is enabled so the
// screen reads as "everything captured" at a glance.
func TestPortfolioFullGreeksCoverageMark(t *testing.T) {
	t.Parallel()
	delta := 1500.0
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
	res := &rpc.PositionsResult{
		Portfolio: &rpc.PositionsPortfolio{
			EffectiveDelta: &delta,
			GreeksCoverage: 12,
			GreeksTotal:    12,
		},
	}
	renderPortfolioSummary(env, res)
	out := stdout.String()
	for _, want := range []string{"Greeks coverage", "12 / 12", "✓"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "partial") {
		t.Errorf("full-coverage line should not carry the partial-coverage caveat:\n%s", out)
	}
	if !strings.Contains(out, ansiGreen) {
		t.Errorf("expected green wrap on the checkmark:\n%s", out)
	}
}

// Option legs in the by-underlying view get a Greeks suffix line beneath
// them when at least one Greek landed in budget. No suffix when all four
// are nil so non-greek-bearing positions stay tight.
func TestRenderPositionsByUnderlying_GreeksSuffix(t *testing.T) {
	t.Parallel()
	t.Run("greeks present → suffix line rendered", func(t *testing.T) {
		delta, gamma, theta, vega := 0.42, 0.018, -0.08, 0.42
		opt := rpc.PositionView{
			Symbol: "AAPL", Right: "C", Strike: 210, Expiry: "20251219",
			Quantity: 2, AvgCost: 5.10, Mark: 7.85, UnrealizedPnL: 550,
			Delta: &delta, Gamma: &gamma, Theta: &theta, Vega: &vega,
		}
		res := &rpc.PositionsResult{
			ByUnderlying: []rpc.PositionGroup{
				{Underlying: "AAPL", Options: []rpc.PositionView{opt}, GroupMarketValue: 1570, GroupUnrealizedPnL: 550},
			},
		}
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		if code := renderPositionsByUnderlying(env, res); code != 0 {
			t.Fatalf("code = %d", code)
		}
		out := stdout.String()
		for _, want := range []string{"Δ", "+0.42", "Γ", "Θ", "ν"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing greek symbol %q:\n%s", want, out)
			}
		}
	})
	t.Run("all greeks nil → placeholders rendered, column shape preserved", func(t *testing.T) {
		opt := rpc.PositionView{
			Symbol: "AAPL", Right: "C", Strike: 210, Expiry: "20251219",
			Quantity: 2, AvgCost: 5.10, Mark: 7.85, UnrealizedPnL: 550,
		}
		res := &rpc.PositionsResult{
			ByUnderlying: []rpc.PositionGroup{
				{Underlying: "AAPL", Options: []rpc.PositionView{opt}, GroupMarketValue: 1570, GroupUnrealizedPnL: 550},
			},
		}
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		_ = renderPositionsByUnderlying(env, res)
		out := stdout.String()
		// Symbols stay visible so a reader can see Greeks aren't yet
		// available rather than missing them as a blank cell.
		for _, want := range []string{"Δ", "Γ", "Θ", "ν", "—"} {
			if !strings.Contains(out, want) {
				t.Errorf("placeholder greek glyph %q missing:\n%s", want, out)
			}
		}
		// No actual values should have been fabricated.
		if strings.Contains(out, "+0.00") || strings.Contains(out, "-0.00") {
			t.Errorf("nil greeks must not render as a zero substitute:\n%s", out)
		}
	})
}

// chain SYM with no --expiry should render an expiry list (one row per
// expiry), not the strike-table header. --with-iv adds the ATM IV column.
func TestRenderChainExpiriesText(t *testing.T) {
	t.Parallel()
	t.Run("plain list", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		res := &rpc.ChainExpiriesResult{
			Symbol: "AAPL",
			Expiries: []rpc.ChainExpiry{
				{Date: "2026-01-16"},
				{Date: "2026-06-19"},
				{Date: "2026-09-18"},
			},
		}
		if code := renderChainExpiriesText(env, res, false); code != 0 {
			t.Fatalf("code = %d", code)
		}
		out := stdout.String()
		for _, want := range []string{"AAPL", "3 expiries", "2026-01-16", "2026-06-19", "2026-09-18", "canary chain AAPL --expiry"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q\n%s", want, out)
			}
		}
		if strings.Contains(out, "ATM IV") {
			t.Errorf("plain list should not show ATM IV column:\n%s", out)
		}
	})

	t.Run("with-iv mixed statuses", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		ivOK := 0.284
		res := &rpc.ChainExpiriesResult{
			Symbol: "AAPL",
			Expiries: []rpc.ChainExpiry{
				{Date: "2026-01-16", IV: &ivOK, IVStatus: "ok"},
				{Date: "2026-06-19", IVStatus: "timeout"},
				{Date: "2026-09-18", IVStatus: "unavailable"},
			},
		}
		if code := renderChainExpiriesText(env, res, true); code != 0 {
			t.Fatalf("code = %d", code)
		}
		out := stdout.String()
		for _, want := range []string{"ATM IV", "QUALITY", "28.4%", "(timeout)", "(unavailable)"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q\n%s", want, out)
			}
		}
	})

	t.Run("empty list shows guidance", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		res := &rpc.ChainExpiriesResult{Symbol: "ZZZ"}
		_ = renderChainExpiriesText(env, res, false)
		out := stdout.String()
		if !strings.Contains(out, "no option expiries available") {
			t.Errorf("missing empty-state guidance:\n%s", out)
		}
	})

	t.Run("with-iv shows DTE + implied move column", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		iv := 0.30
		mv := 17.19
		pct := 0.0860
		res := &rpc.ChainExpiriesResult{
			Symbol: "AAPL",
			Spot:   200.00,
			Expiries: []rpc.ChainExpiry{
				{Date: "2026-06-19", DTE: 30, IV: &iv, IVStatus: "ok", ImpliedMove: &mv, ImpliedMovePct: &pct},
				{Date: "2026-09-18", DTE: 120, IVStatus: "timeout"},
			},
		}
		_ = renderChainExpiriesText(env, res, true)
		out := stdout.String()
		for _, want := range []string{"DTE", "EXPECTED MOVE", "spot", "30", "120", "8.6%", "(timeout)"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in output:\n%s", want, out)
			}
		}
	})

	// The formula caption under the expiry-with-IV table is a deliberate
	// educational anchor — it teaches first-time readers what EXPECTED MOVE
	// is computed from and aligns with the CBOE option-calculator convention.
	t.Run("with-iv shows formula caption", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		iv, mv, pct := 0.30, 17.19, 0.0860
		res := &rpc.ChainExpiriesResult{
			Symbol: "AAPL",
			Spot:   200.00,
			Expiries: []rpc.ChainExpiry{
				{Date: "2026-06-19", DTE: 30, IV: &iv, IVStatus: "ok", ImpliedMove: &mv, ImpliedMovePct: &pct},
			},
		}
		_ = renderChainExpiriesText(env, res, true)
		out := stdout.String()
		for _, want := range []string{"spot × IV × √(DTE/365)", "CBOE convention"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q:\n%s", want, out)
			}
		}
	})
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
