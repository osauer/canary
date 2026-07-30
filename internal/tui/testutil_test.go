package tui

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/rpc"
)

func testCatalog(t *testing.T) []cli.CommandSpec {
	t.Helper()
	catalog := cli.Catalog()
	if len(catalog) == 0 {
		t.Fatal("empty catalog")
	}
	return catalog
}

func testSnapshot() live.Snapshot {
	pct := 12.5
	price := 501.25
	chg := 0.42
	return live.Snapshot{
		UpdatedAt: time.Now(),
		Status:    &rpc.HealthResult{Connected: true, ConnectedAccount: "DU123", AccountMode: rpc.AccountModePaper},
		Account: &rpc.AccountResult{
			BaseCurrency: "USD",
			CurrencyExposure: []rpc.CurrencyExposure{
				{Currency: "EUR", NetLiquidationCcy: 1000},
			},
		},
		Positions: &rpc.PositionsResult{
			Stocks: []rpc.PositionView{{Symbol: "AAPL"}},
			Portfolio: &rpc.PositionsPortfolio{ExposureBase: []rpc.UnderlyingExposure{
				{Underlying: "AAPL", MarketValueBase: 12500, MarketValuePctNLV: &pct},
			}},
		},
		Quotes: &live.MarketQuotes{Quotes: map[string]rpc.Quote{
			"SPY": {Symbol: "SPY", Price: &price, ChangePct: &chg, DataType: rpc.MarketDataLive},
		}},
	}
}

func TestParseCommandLine(t *testing.T) {
	t.Parallel()
	got, err := parseCommandLine(`canary quote "BRK B" --market de`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"canary", "quote", "BRK B", "--market", "de"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("tokens=%q want %q", got, want)
	}
	if _, err := parseCommandLine(`quote "AAPL`); err == nil {
		t.Fatal("unterminated quote returned nil error")
	}
}

func TestTUICommandCanonicalPrefixIsOptional(t *testing.T) {
	t.Parallel()
	want := []string{"quote", "BRK B", "--market", "de"}
	for _, tc := range []struct {
		name string
		line string
	}{
		{name: "prefixless", line: `quote "BRK B" --market de`},
		{name: "canonical", line: `canary quote "BRK B" --market de`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTUICommandLine(tc.line)
			if err != nil {
				t.Fatalf("parseTUICommandLine: %v", err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("tokens=%q want %q", got, want)
			}
		})
	}
}

func TestCompletionUsesCatalog(t *testing.T) {
	t.Parallel()
	catalog := testCatalog(t)
	if got := completeLine("pos", 3, catalog, nil); len(got) != 1 || got[0] != "positions" {
		t.Fatalf("command completion=%v, want positions", got)
	}
	if got := completeLine("positions --s", len("positions --s"), catalog, nil); !contains(got, "--sort") || !contains(got, "--symbol") {
		t.Fatalf("flag completion=%v, want sort and symbol", got)
	}
	if got := completeLine("positions --type ", len("positions --type "), catalog, nil); !contains(got, "stk") || !contains(got, "opt") {
		t.Fatalf("enum completion=%v, want stk/opt", got)
	}
}

func TestCompletionSupportsCanonicalExecutablePrefix(t *testing.T) {
	t.Parallel()
	catalog := testCatalog(t)
	for _, prefix := range []string{"", "canary "} {
		name := strings.TrimSpace(prefix)
		if name == "" {
			name = "prefixless"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			commandLine := prefix + "pos"
			if got := completeLine(commandLine, len(commandLine), catalog, nil); !slices.Equal(got, []string{"positions"}) {
				t.Fatalf("command completion=%v, want positions", got)
			}
			flagLine := prefix + "positions --s"
			if got := completeLine(flagLine, len(flagLine), catalog, nil); !contains(got, "--sort") || !contains(got, "--symbol") {
				t.Fatalf("flag completion=%v, want sort and symbol", got)
			}
			enumLine := prefix + "positions --type "
			if got := completeLine(enumLine, len(enumLine), catalog, nil); !contains(got, "stk") || !contains(got, "opt") {
				t.Fatalf("enum completion=%v, want stk/opt", got)
			}
		})
	}
}

func TestConfirmationPolicy(t *testing.T) {
	t.Parallel()
	catalog := testCatalog(t)
	cases := []struct {
		line string
		want bool
	}{
		{"status", false},
		{"order preview buy AAPL 1", false},
		{"order place --preview-token tok", true},
		{"order modify --preview-token tok", true},
		{"order cancel --preview-token tok", true},
		{"purge status", false},
		{"purge dry-run", false},
		{"purge dry-run --save", true},
		{"purge restore AAPL", false},
		{"purge restore AAPL --execute", true},
		{"purge execute --all", true},
		{"settings show", false},
		{"settings show --json", false},
		{"settings set trading.freeze=true", true},
		{"restart --timeout 15s", true},
		{"update --check", false},
		{"update --no-restart", true},
		{"trading status", false},
		{"trading paper-smoke", true},
		{"policy show", false},
		{"policy capital-event --amount 100", true},
		{"policy override --control drawdown --reason test", true},
		{"policy reset-drawdown", true},
		{"policy correct-peak --peak 100", true},
		{"policy artefact --report q2", true},
		{"recon show", false},
		{"recon dismiss --line 1 --reason test", true},
		{"proposals", true},
		{"proposals status", false},
		{"proposals refresh", false},
		{"proposals list", false},
		{"proposals preview 1", false},
		{"proposals ignore 1", false},
		{"proposals submit 1", true},
		{"proposals reduce --portfolio --percent 50", true},
		{"opportunities status", false},
		{"opportunities list", false},
		{"opportunities ignore 1", false},
		{"opportunities exercise 1", true},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()
			for _, prefix := range []string{"", "canary "} {
				name := strings.TrimSpace(prefix)
				if name == "" {
					name = "prefixless"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					line := prefix + tc.line
					got, err := confirmationFor(line, catalog)
					if err != nil {
						t.Fatalf("confirmationFor: %v", err)
					}
					if (got != nil) != tc.want {
						t.Fatalf("confirmationFor(%q)=%v, want present=%v", line, got, tc.want)
					}
				})
			}
		})
	}
}

func TestVersionOutputSupportsCanonicalExecutablePrefix(t *testing.T) {
	t.Parallel()
	catalog := testCatalog(t)
	for _, line := range []string{
		"version", "canary version",
		"--version", "canary --version",
	} {
		t.Run(line, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := (commandRunner{version: "v9.8.7"}).runParsed(context.Background(), line, catalog, &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if got, want := stdout.String(), "canary v9.8.7\n"; got != want {
				t.Fatalf("stdout=%q want %q", got, want)
			}
		})
	}
}

func TestExternalCommandHintUsesCanonicalExecutable(t *testing.T) {
	t.Parallel()
	catalog := testCatalog(t)
	for _, command := range []string{"daemon", "app pair", "mcp", "setup"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			for _, prefix := range []string{"", "canary "} {
				name := strings.TrimSpace(prefix)
				if name == "" {
					name = "prefixless"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					var stdout, stderr bytes.Buffer
					code := (commandRunner{}).runParsed(context.Background(), prefix+command, catalog, &stdout, &stderr)
					if code != 0 || stderr.Len() != 0 {
						t.Fatalf("exit=%d stderr=%q", code, stderr.String())
					}
					want := "Run `canary " + command + "` in a normal terminal."
					if !strings.Contains(stdout.String(), want) {
						t.Fatalf("external hint is not canonical: %q", stdout.String())
					}
				})
			}
		})
	}
}

func TestTickerSymbolsIncludeHoldingsHedgesAndFX(t *testing.T) {
	t.Parallel()
	got := tickerSymbols(testSnapshot())
	for _, want := range []string{"AAPL", "USD.EUR", "SPY", "QQQ", "VIX"} {
		if !contains(got, want) {
			t.Fatalf("tickerSymbols missing %q: %v", want, got)
		}
	}
	items := tickerItems(testSnapshot())
	if !containsVisiblePrefix(items, "SPY 501.25") {
		t.Fatalf("tickerItems missing formatted SPY quote: %v", items)
	}
}

func TestTickerLineHiddenUntilConnectedMarketData(t *testing.T) {
	t.Parallel()
	m := newModel(testCatalog(t), Size{Rows: 12, Cols: 32})
	if got := tickerLine(m, 22); got != "" {
		t.Fatalf("ticker before live snapshot = %q, want hidden", got)
	}
	m.snapshot.Status = &rpc.HealthResult{Connected: true}
	if got := tickerLine(m, 22); got != "" {
		t.Fatalf("ticker before quotes = %q, want hidden", got)
	}
	m.snapshot.Quotes = &live.MarketQuotes{Quotes: map[string]rpc.Quote{"SPY": {Symbol: "SPY"}}}
	if got := tickerLine(m, 22); got != "" {
		t.Fatalf("ticker before quote price = %q, want hidden", got)
	}
	m.snapshot.Positions = &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "SAP"}}}
	if got := stripControl(tickerLine(m, 22)); !strings.HasPrefix(got, "MKT SAP") {
		t.Fatalf("ticker with holdings but no quote price = %q, want holdings", got)
	}
	m.snapshot = testSnapshot()
	if got := stripControl(tickerLine(m, 22)); !strings.HasPrefix(got, "MKT ") {
		t.Fatalf("ticker after connected quotes = %q, want market line", got)
	}
}

func TestTickerItemsShowPositionsFirstWithColoredMovement(t *testing.T) {
	t.Parallel()
	up := 1.25
	down := -2.5
	sapPrice := 162.40
	dteClose := 27.88
	optionMark := 7.50
	optionPrevClose := 7.00
	snap := testSnapshot()
	snap.Positions.Stocks = []rpc.PositionView{
		{Symbol: "SAP", QuotePrice: &sapPrice, DayChangePct: &up},
		{Symbol: "DTE", RegularClose: &dteClose, DailyPnL: &down},
	}
	snap.Positions.Options = []rpc.PositionView{
		{Symbol: "NVDA", Mark: optionMark, PrevClose: &optionPrevClose},
	}

	items := tickerItems(snap)
	if len(items) < 3 {
		t.Fatalf("tickerItems length=%d, want positions", len(items))
	}
	if !strings.HasPrefix(stripControl(items[0]), "SAP 162.40 +1.25%") {
		t.Fatalf("first ticker item=%q, want SAP position first", stripControl(items[0]))
	}
	if !strings.Contains(items[0], ansiOK) {
		t.Fatalf("positive position movement not green: %q", items[0])
	}
	if !strings.HasPrefix(stripControl(items[1]), "DTE 27.88 day -2") {
		t.Fatalf("second ticker item=%q, want DTE position second", stripControl(items[1]))
	}
	if !strings.Contains(items[1], ansiDanger) {
		t.Fatalf("negative position movement not red: %q", items[1])
	}
	if !strings.HasPrefix(stripControl(items[2]), "NVDA 7.50 +7.14%") {
		t.Fatalf("third ticker item=%q, want option fallback position", stripControl(items[2]))
	}
	if !strings.Contains(items[2], ansiOK) {
		t.Fatalf("computed option movement not green: %q", items[2])
	}
}

func TestTickerQuoteColorUsesCurrentChangeFields(t *testing.T) {
	t.Parallel()
	quotePrice := 101.0
	prevClose := 100.0
	regularDown := -0.75

	up := formatQuoteTicker("SPY", rpc.Quote{Symbol: "SPY", QuotePrice: &quotePrice, PrevClose: &prevClose})
	if !strings.Contains(up, ansiOK) || !strings.Contains(stripControl(up), "+1.00%") {
		t.Fatalf("fallback quote change not green: %q", up)
	}
	down := formatQuoteTicker("QQQ", rpc.Quote{Symbol: "QQQ", Price: &quotePrice, RegularChangePct: &regularDown})
	if !strings.Contains(down, ansiDanger) || !strings.Contains(stripControl(down), "-0.75%") {
		t.Fatalf("regular quote change not red: %q", down)
	}
}

func TestLiveSnapshotResetsTickerOffsetWhenItemsChange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newModel(testCatalog(t), Size{Rows: 12, Cols: 60})
	base := testSnapshot()
	base.Positions = nil
	handleEvent(ctx, cancel, m, commandRunner{}, make(chan uiEvent, 1), uiEvent{kind: eventLiveSnapshot, snap: base})
	m.tickerIndex = 19

	next := testSnapshot()
	handleEvent(ctx, cancel, m, commandRunner{}, make(chan uiEvent, 1), uiEvent{kind: eventLiveSnapshot, snap: next})
	if m.tickerIndex != 0 {
		t.Fatalf("ticker index=%d, want reset after item set changed", m.tickerIndex)
	}
	m.tickerIndex = 7
	handleEvent(ctx, cancel, m, commandRunner{}, make(chan uiEvent, 1), uiEvent{kind: eventLiveSnapshot, snap: next})
	if m.tickerIndex != 7 {
		t.Fatalf("ticker index=%d, want preserved when item set is unchanged", m.tickerIndex)
	}
}

func TestTickerLineScrollsHorizontally(t *testing.T) {
	t.Parallel()
	m := newModel(testCatalog(t), Size{Rows: 12, Cols: 32})
	m.snapshot = testSnapshot()
	first := stripControl(tickerLine(m, 22))
	m.tickerIndex = 1
	second := stripControl(tickerLine(m, 22))
	if first == second {
		t.Fatalf("ticker did not scroll: %q", first)
	}
	if visibleWidth(second) != 22 {
		t.Fatalf("ticker width = %d, want 22: %q", visibleWidth(second), second)
	}
}

func TestTickerMarqueePreservesANSI(t *testing.T) {
	t.Parallel()
	cells := tickerTapeCells([]string{"ABC " + styleOK("+1.00") + " XYZ"})
	window := renderTickerCells(scrolledTickerCells(cells, 0, 10))
	if !strings.Contains(window, ansiOK) {
		t.Fatalf("ticker marquee stripped semantic color: %q", window)
	}
	if visibleWidth(window) != 10 {
		t.Fatalf("ticker marquee width = %d, want 10: %q", visibleWidth(window), window)
	}
}

func TestLayoutResponsiveCollapse(t *testing.T) {
	t.Parallel()
	wide := computeLayout(Size{Rows: 30, Cols: 120})
	if !wide.showTicker || !wide.showWarning {
		t.Fatalf("wide layout ticker=%v warning=%v, want both", wide.showTicker, wide.showWarning)
	}
	small := computeLayout(Size{Rows: 10, Cols: 60})
	if small.showWarning {
		t.Fatalf("small layout warning=true, want false")
	}
	if small.output.w != 60 {
		t.Fatalf("small output width=%d, want 60", small.output.w)
	}
}

func TestRenderUsesAbsoluteCursorMoves(t *testing.T) {
	t.Parallel()
	m := newModel(testCatalog(t), Size{Rows: 12, Cols: 60})
	m.editor.setLine("status")
	got := render(m)
	if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
		t.Fatalf("render frame contains raw line separator: %q", got)
	}
	if !strings.Contains(got, "\x1b[1;1H") || !strings.Contains(got, "\x1b[11;15H") {
		t.Fatalf("render frame missing absolute cursor moves: %q", got)
	}
}

func TestStartupCanaryIsTerminalNativeAndYellow(t *testing.T) {
	t.Parallel()
	m := newModel(testCatalog(t), Size{Rows: 14, Cols: 72})
	got := render(m)
	plain := stripControl(got)
	if !strings.Contains(plain, "▐████ ███▌▸") || !strings.Contains(plain, "Canary") {
		t.Fatalf("startup canary missing from render: %q", plain)
	}
	if !strings.Contains(plain, "canary> ") || strings.Contains(plain, "ibkr> ") {
		t.Fatalf("prompt does not use canonical command identity: %q", plain)
	}
	if !strings.Contains(got, ansiWarn+"   ▐████ ███▌▸") {
		t.Fatalf("startup canary is not yellow: %q", got)
	}
}

func TestRiskPanelUsesYellowMiniCanary(t *testing.T) {
	t.Parallel()
	m := newModel(testCatalog(t), Size{Rows: 18, Cols: 120})
	lines := riskPanelLines(m, 32, 4)
	if len(lines) == 0 {
		t.Fatal("risk panel returned no lines")
	}
	if !strings.Contains(stripControl(lines[0]), "▐█▌▸ CANARY") {
		t.Fatalf("risk panel canary title missing: %q", lines[0])
	}
	if !strings.Contains(lines[0], ansiWarn+"▐█▌▸") || !strings.Contains(lines[0], ansiWarn+"CANARY") {
		t.Fatalf("risk panel canary is not yellow: %q", lines[0])
	}
}

func TestFitPreservesANSIFormatting(t *testing.T) {
	t.Parallel()
	got := fit("\x1b[31mred text\x1b[0m", 20)
	if !strings.Contains(got, "\x1b[31m") || !strings.Contains(got, "\x1b[0m") {
		t.Fatalf("fit stripped ANSI formatting: %q", got)
	}
	truncated := fit("\x1b[31mred text\x1b[0m", 5)
	if stripControl(truncated) != "red ~" {
		t.Fatalf("truncated visible text = %q, want red ~", stripControl(truncated))
	}
	if !strings.HasSuffix(truncated, "\x1b[0m") {
		t.Fatalf("truncated ANSI text is not reset: %q", truncated)
	}
}

func TestCtrlCExitsIdlePrompt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newModel(testCatalog(t), Size{Rows: 12, Cols: 60})
	handleKey(ctx, cancel, m, commandRunner{}, make(chan uiEvent, 1), keyEvent{kind: keyCtrlC})
	if !m.quitting {
		t.Fatal("idle Ctrl-C did not request quit")
	}
}

func TestUnknownCommandMessageIsConcise(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := commandRunner{}.runParsed(context.Background(), "positons", testCatalog(t), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	got := stderr.String()
	if !strings.Contains(got, `unknown command "positons"`) || !strings.Contains(got, "positions") {
		t.Fatalf("unknown command output = %q", got)
	}
	if strings.Contains(got, "Usage:") || len(strings.Split(strings.TrimSpace(got), "\n")) != 1 {
		t.Fatalf("unknown command output is too noisy: %q", got)
	}
}

func TestDecodeKeyBytes(t *testing.T) {
	t.Parallel()
	got := decodeKeyBytes([]byte{'a', 0x1b, '[', 'A', 0x7f, '\r'})
	kinds := []keyKind{}
	for _, key := range got {
		kinds = append(kinds, key.kind)
	}
	want := []keyKind{keyRune, keyUp, keyBackspace, keyEnter}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds=%v want %v", kinds, want)
		}
	}
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}

func containsVisiblePrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(stripControl(value), prefix) {
			return true
		}
	}
	return false
}
