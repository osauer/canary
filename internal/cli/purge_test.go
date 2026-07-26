package cli

import (
	"bytes"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestBuildPurgeBookFromPositionsCreatesClosingAndRestoreSides(t *testing.T) {
	t.Parallel()

	stockQuote := 101.25
	optionBid := 4.80
	optionAsk := 5.20
	now := time.Date(2026, 6, 4, 15, 30, 12, 0, time.UTC)
	pos := rpc.PositionsResult{
		AccountID: "DU123",
		AsOf:      now.Add(-time.Second),
		Portfolio: &rpc.PositionsPortfolio{
			BaseCurrency: "USD",
		},
		Stocks: []rpc.PositionView{
			{
				Symbol:      "AAPL",
				SecType:     rpc.SecTypeStock,
				Quantity:    10,
				Multiplier:  1,
				Currency:    "USD",
				Mark:        101,
				QuotePrice:  &stockQuote,
				MarketValue: 1010,
			},
			{
				Symbol:      "TSLA",
				SecType:     rpc.SecTypeStock,
				Quantity:    -3,
				Multiplier:  1,
				Currency:    "USD",
				Mark:        250,
				MarketValue: -750,
			},
		},
		Options: []rpc.PositionView{
			{
				Symbol:       "NVDA",
				SecType:      rpc.SecTypeOption,
				Quantity:     2,
				Multiplier:   100,
				Currency:     "USD",
				Expiry:       "20260717",
				Right:        "C",
				Strike:       120,
				Mark:         5,
				OptionBid:    &optionBid,
				OptionAsk:    &optionAsk,
				MarketValue:  1000,
				TradingClass: "NVDA",
			},
		},
	}

	book := buildPurgeBookFromPositions(pos, now)

	if book.Kind != purgeBookKind || book.SchemaVersion != purgeBookSchemaVersion {
		t.Fatalf("book identity = %s/%s", book.Kind, book.SchemaVersion)
	}
	if book.PurgeID != "purge_20260604_153012" {
		t.Fatalf("purge id = %q", book.PurgeID)
	}
	if len(book.Legs) != 3 {
		t.Fatalf("legs = %d, want 3", len(book.Legs))
	}
	aapl := findPurgeLeg(t, book, "AAPL")
	if aapl.OriginalSide != purgeOriginalSideLong || aapl.PurgeAction != rpc.OrderActionSell || aapl.RestoreAction != rpc.OrderActionBuy {
		t.Fatalf("AAPL sides/actions = %+v", aapl)
	}
	if aapl.ExitPrice != stockQuote || aapl.ExitPriceSource != "quote_price" {
		t.Fatalf("AAPL exit = %.2f/%s", aapl.ExitPrice, aapl.ExitPriceSource)
	}
	tsla := findPurgeLeg(t, book, "TSLA")
	if tsla.OriginalSide != purgeOriginalSideShort || tsla.PurgeAction != rpc.OrderActionBuy || tsla.RestoreAction != rpc.OrderActionSell {
		t.Fatalf("TSLA sides/actions = %+v", tsla)
	}
	nvda := findPurgeLeg(t, book, "NVDA")
	if nvda.Contract.SecType != "OPT" || nvda.ExitPrice != optionBid || nvda.ExitValue != 960 {
		t.Fatalf("NVDA option leg = %+v", nvda)
	}
}

func TestApplyQuoteToPurgeLegComputesShadowSaved(t *testing.T) {
	t.Parallel()

	leg := purgeBookLeg{
		Symbol:           "AAPL",
		OriginalQuantity: 10,
		Quantity:         10,
		Multiplier:       1,
		ExitPrice:        100,
		ExitValue:        1000,
		RestoreAction:    rpc.OrderActionBuy,
		Status:           purgeLegStatusDraft,
	}
	ask := 92.50
	bid := 92.40
	applyQuoteToPurgeLeg(&leg, rpc.Quote{Ask: &ask, Bid: &bid, QuoteQuality: "firm", DataType: rpc.MarketDataLive})

	if leg.CurrentPrice == nil || *leg.CurrentPrice != ask {
		t.Fatalf("current price = %v, want ask %.2f", leg.CurrentPrice, ask)
	}
	if leg.ShadowSaved == nil || math.Abs(*leg.ShadowSaved-75) > 1e-9 {
		t.Fatalf("shadow saved = %v, want 75", leg.ShadowSaved)
	}
	if leg.ShadowSavedPctExit == nil || math.Abs(*leg.ShadowSavedPctExit-7.5) > 1e-9 {
		t.Fatalf("shadow saved pct = %v, want 7.5", leg.ShadowSavedPctExit)
	}
	if leg.LowPrice == nil || *leg.LowPrice != ask {
		t.Fatalf("low price = %v, want %.2f", leg.LowPrice, ask)
	}
}

func TestRenderPurgeBookTextUsesReviewLanguage(t *testing.T) {
	t.Parallel()

	saved := 9.0
	book := purgeBook{
		PurgeID:      "active",
		Status:       purgeBookStatusDraft,
		BaseCurrency: "EUR",
		Totals: purgeBookTotals{
			ExitValue:   242.02,
			ShadowSaved: &saved,
		},
		NotExecution: "Dry-run review only.",
	}
	var out bytes.Buffer
	env := &Env{Stdout: &out, Stderr: &bytes.Buffer{}}

	renderPurgeBookText(env, &out, &book)

	got := out.String()
	for _, bad := range []string{"HELPED", "Status", "draft", "Purge Book"} {
		if strings.Contains(got, bad) {
			t.Fatalf("render output contains stale execution language %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "Canary Purge Review") || !strings.Contains(got, "REVIEW") {
		t.Fatalf("render output missing review language:\n%s", got)
	}
}

func TestMergePurgeBookReconcilesExistingInstrumentQuantity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 4, 15, 30, 0, 0, time.UTC)
	firstPrice := 100.0
	secondPrice := 90.0
	active := newActivePurgeBook(now)
	first := buildPurgeBookFromPositions(rpc.PositionsResult{
		AccountID: "DU123",
		Portfolio: &rpc.PositionsPortfolio{
			BaseCurrency: "USD",
		},
		Stocks: []rpc.PositionView{{
			Symbol:      "AAPL",
			SecType:     rpc.SecTypeStock,
			Quantity:    10,
			Multiplier:  1,
			Currency:    "USD",
			QuotePrice:  &firstPrice,
			MarketValue: 1000,
		}},
	}, now)
	second := buildPurgeBookFromPositions(rpc.PositionsResult{
		AccountID: "DU123",
		Portfolio: &rpc.PositionsPortfolio{
			BaseCurrency: "USD",
		},
		Stocks: []rpc.PositionView{{
			Symbol:      "AAPL",
			SecType:     rpc.SecTypeStock,
			Quantity:    5,
			Multiplier:  1,
			Currency:    "USD",
			QuotePrice:  &secondPrice,
			MarketValue: 450,
		}},
	}, now.Add(time.Minute))

	if err := mergePurgeBook(&active, first, now); err != nil {
		t.Fatalf("merge first: %v", err)
	}
	if err := mergePurgeBook(&active, second, now.Add(time.Minute)); err != nil {
		t.Fatalf("merge second: %v", err)
	}

	if active.PurgeID != activePurgeBookID {
		t.Fatalf("purge id = %q, want active", active.PurgeID)
	}
	if len(active.Legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(active.Legs))
	}
	leg := active.Legs[0]
	if leg.Quantity != 5 || leg.OriginalQuantity != 5 {
		t.Fatalf("reconciled quantity = %.2f/%.2f, want 5/5", leg.Quantity, leg.OriginalQuantity)
	}
	if math.Abs(leg.ExitValue-500) > 1e-9 {
		t.Fatalf("exit value = %.2f, want original exit price scaled to 500", leg.ExitValue)
	}
	if math.Abs(leg.ExitPrice-firstPrice) > 1e-9 {
		t.Fatalf("exit price = %.6f, want original purge reference %.2f", leg.ExitPrice, firstPrice)
	}
}

func TestMergePurgeBookRejectsOppositeSideForSameInstrument(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 4, 15, 30, 0, 0, time.UTC)
	price := 100.0
	active := newActivePurgeBook(now)
	longBook := buildPurgeBookFromPositions(rpc.PositionsResult{
		Stocks: []rpc.PositionView{{
			Symbol: "AAPL", SecType: rpc.SecTypeStock, Quantity: 10, Multiplier: 1, Currency: "USD", QuotePrice: &price,
		}},
	}, now)
	shortBook := buildPurgeBookFromPositions(rpc.PositionsResult{
		Stocks: []rpc.PositionView{{
			Symbol: "AAPL", SecType: rpc.SecTypeStock, Quantity: -3, Multiplier: 1, Currency: "USD", QuotePrice: &price,
		}},
	}, now)

	if err := mergePurgeBook(&active, longBook, now); err != nil {
		t.Fatalf("merge long: %v", err)
	}
	if err := mergePurgeBook(&active, shortBook, now); err == nil {
		t.Fatalf("merge opposite side succeeded, want error")
	}
}

func TestRecordPurgeRestorePlanSubtractsAndRemoves(t *testing.T) {
	t.Parallel()

	current := 92.0
	restoreValue := 920.0
	saved := 80.0
	lowRestore := 900.0
	highRestore := 950.0
	book := newActivePurgeBook(time.Date(2026, 6, 4, 15, 30, 0, 0, time.UTC))
	book.Legs = []purgeBookLeg{{
		LegID:               "leg_aapl",
		Symbol:              "AAPL",
		SecType:             rpc.SecTypeStock,
		Contract:            rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Currency: "USD"},
		OriginalSide:        purgeOriginalSideLong,
		OriginalQuantity:    10,
		PurgeAction:         rpc.OrderActionSell,
		RestoreAction:       rpc.OrderActionBuy,
		Quantity:            10,
		Multiplier:          1,
		ExitPrice:           100,
		ExitValue:           1000,
		CurrentPrice:        &current,
		CurrentRestoreValue: &restoreValue,
		ShadowSaved:         &saved,
		LowRestoreValue:     &lowRestore,
		HighRestoreValue:    &highRestore,
		Status:              purgeLegStatusPriced,
	}}
	recomputePurgeBookTotals(&book)

	plan := buildPurgeRestorePlan(book, 0.4, []string{"AAPL"}, time.Date(2026, 6, 4, 16, 0, 0, 0, time.UTC))
	if err := recordPurgeRestorePlan(&book, plan, time.Date(2026, 6, 4, 16, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("record partial restore: %v", err)
	}

	if len(book.Legs) != 1 {
		t.Fatalf("legs after partial = %d, want 1", len(book.Legs))
	}
	leg := book.Legs[0]
	if leg.Quantity != 6 || leg.OriginalQuantity != 6 {
		t.Fatalf("remaining quantity = %.2f/%.2f, want 6/6", leg.Quantity, leg.OriginalQuantity)
	}
	if math.Abs(leg.ExitValue-600) > 1e-9 {
		t.Fatalf("remaining exit value = %.2f, want 600", leg.ExitValue)
	}
	if leg.CurrentRestoreValue == nil || math.Abs(*leg.CurrentRestoreValue-552) > 1e-9 {
		t.Fatalf("remaining restore value = %v, want 552", leg.CurrentRestoreValue)
	}
	if leg.ShadowSaved == nil || math.Abs(*leg.ShadowSaved-48) > 1e-9 {
		t.Fatalf("remaining shadow saved = %v, want 48", leg.ShadowSaved)
	}

	plan = buildPurgeRestorePlan(book, 1, []string{"AAPL"}, time.Date(2026, 6, 4, 16, 1, 0, 0, time.UTC))
	if err := recordPurgeRestorePlan(&book, plan, time.Date(2026, 6, 4, 16, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("record full restore: %v", err)
	}
	if len(book.Legs) != 0 {
		t.Fatalf("legs after full restore = %d, want 0", len(book.Legs))
	}
}

func TestBuildPurgeRestorePlanScalesAndFilters(t *testing.T) {
	t.Parallel()

	aaplPrice := 92.50
	aaplSaved := 75.0
	tslaPrice := 255.0
	book := purgeBook{
		PurgeID:      "purge_20260604_153012",
		AccountID:    "DU123",
		BaseCurrency: "USD",
		Legs: []purgeBookLeg{
			{
				LegID:            "leg_aapl",
				Symbol:           "AAPL",
				SecType:          rpc.SecTypeStock,
				Contract:         rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Currency: "USD"},
				RestoreAction:    rpc.OrderActionBuy,
				Quantity:         10,
				Multiplier:       1,
				CurrentPrice:     &aaplPrice,
				ShadowSaved:      &aaplSaved,
				Status:           purgeLegStatusPriced,
				OriginalQuantity: 10,
			},
			{
				LegID:         "leg_tsla",
				Symbol:        "TSLA",
				SecType:       rpc.SecTypeStock,
				Contract:      rpc.ContractParams{Symbol: "TSLA", SecType: "STK", Currency: "USD"},
				RestoreAction: rpc.OrderActionSell,
				Quantity:      3,
				Multiplier:    1,
				CurrentPrice:  &tslaPrice,
				Status:        purgeLegStatusPriced,
			},
		},
	}

	plan := buildPurgeRestorePlan(book, 0.5, []string{"AAPL"}, time.Date(2026, 6, 4, 16, 0, 0, 0, time.UTC))

	if len(plan.Legs) != 1 {
		t.Fatalf("restore legs = %d, want 1", len(plan.Legs))
	}
	leg := plan.Legs[0]
	if leg.Symbol != "AAPL" || leg.Action != rpc.OrderActionBuy || leg.Quantity != 5 {
		t.Fatalf("restore leg = %+v", leg)
	}
	if plan.Totals.EstimatedValue == nil || math.Abs(*plan.Totals.EstimatedValue-462.50) > 1e-9 {
		t.Fatalf("estimated value = %v, want 462.50", plan.Totals.EstimatedValue)
	}
	if plan.Totals.ShadowSavedUsed == nil || math.Abs(*plan.Totals.ShadowSavedUsed-37.50) > 1e-9 {
		t.Fatalf("shadow saved used = %v, want 37.50", plan.Totals.ShadowSavedUsed)
	}
	if plan.PreviewAvailable {
		t.Fatalf("preview should remain unavailable in the first CLI-only slice")
	}
}

func TestPurgeBookStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := purgeBookDefaultDir
	purgeBookDefaultDir = func() (string, error) { return dir, nil }
	defer func() { purgeBookDefaultDir = old }()

	book := purgeBook{
		Kind:          purgeBookKind,
		SchemaVersion: purgeBookSchemaVersion,
		PurgeID:       "purge_test",
		Status:        purgeBookStatusDraft,
		CreatedAt:     time.Date(2026, 6, 4, 15, 30, 0, 0, time.UTC),
		Source:        "test",
		NotExecution:  "test",
	}
	path, err := savePurgeBook(&book)
	if err != nil {
		t.Fatalf("savePurgeBook: %v", err)
	}
	if path != filepath.Join(dir, "purge_test.json") {
		t.Fatalf("path = %s", path)
	}
	loaded, err := loadPurgeBook("purge_test")
	if err != nil {
		t.Fatalf("loadPurgeBook: %v", err)
	}
	if loaded.PurgeID != book.PurgeID || loaded.BookPath != path {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestPurgeTargetArgAllDoesNotCollideWithTickerALL(t *testing.T) {
	t.Parallel()

	star, err := purgeTargetArg(false, []string{"*"}, "usage")
	if err != nil {
		t.Fatalf("star target: %v", err)
	}
	if !star.All || star.Symbol != "" {
		t.Fatalf("star target = %+v, want all", star)
	}

	flag, err := purgeTargetArg(true, nil, "usage")
	if err != nil {
		t.Fatalf("--all target: %v", err)
	}
	if !flag.All || flag.Symbol != "" {
		t.Fatalf("--all target = %+v, want all", flag)
	}

	ticker, err := purgeTargetArg(false, []string{"all"}, "usage")
	if err != nil {
		t.Fatalf("ALL ticker target: %v", err)
	}
	if ticker.All || ticker.Symbol != "ALL" {
		t.Fatalf("ALL target = %+v, want ticker ALL", ticker)
	}

	_, err = purgeTargetArg(false, []string{"README.md", "cmd"}, "usage")
	if err == nil {
		t.Fatal("expanded glob target unexpectedly succeeded")
	}
	if got := err.Error(); !strings.Contains(got, "unquoted *") || !strings.Contains(got, "--all") {
		t.Fatalf("expanded glob error = %q, want shell wildcard guidance", got)
	}
}

func TestPurgeLegIDStableAcrossQuantity(t *testing.T) {
	t.Parallel()

	base := rpc.PositionView{
		Symbol: "AAPL", SecType: rpc.SecTypeStock, Quantity: 1, Multiplier: 1, Currency: "USD",
	}
	changed := base
	changed.Quantity = 25

	if purgeLegID(1, base) != purgeLegID(2, changed) {
		t.Fatalf("leg id changed across quantity")
	}
}

func TestPurgeContractInstrumentKeyIncludesConIDAndMultiplier(t *testing.T) {
	t.Parallel()

	standard := rpc.ContractParams{
		ConID:        0,
		Symbol:       "SPY",
		SecType:      "OPT",
		Exchange:     "SMART",
		Currency:     "USD",
		LocalSymbol:  "SPY  260619C00520000",
		TradingClass: "SPY",
		Expiry:       "20260619",
		Strike:       520,
		Right:        "C",
		Multiplier:   100,
	}
	mini := standard
	mini.Multiplier = 10
	if purgeContractInstrumentKey(standard) == purgeContractInstrumentKey(mini) {
		t.Fatalf("instrument key should distinguish option multipliers")
	}
	withConID := standard
	withConID.ConID = 777001
	otherConID := standard
	otherConID.ConID = 777002
	if purgeContractInstrumentKey(withConID) == purgeContractInstrumentKey(otherConID) {
		t.Fatalf("instrument key should distinguish option ConIDs")
	}
}

func TestPurgeSubcommandIndexHandlesHoistedFlags(t *testing.T) {
	t.Parallel()

	args := hoistFlags([]string{"dry-run", "--save", "--json"})
	idx := purgeSubcommandIndex(args)
	if idx < 0 || args[idx] != "dry-run" {
		t.Fatalf("purgeSubcommandIndex(%v) = %d", args, idx)
	}
}

func TestPurgeSubcommandIndexIgnoresExpandedGlobTokens(t *testing.T) {
	t.Parallel()

	args := hoistFlags([]string{"README.md", "status", "cmd"})
	idx := purgeSubcommandIndex(args)
	if idx >= 0 {
		t.Fatalf("purgeSubcommandIndex(%v) = %d, want no subcommand", args, idx)
	}
}

func findPurgeLeg(t *testing.T, book purgeBook, sym string) purgeBookLeg {
	t.Helper()
	for _, leg := range book.Legs {
		if leg.Symbol == sym {
			return leg
		}
	}
	t.Fatalf("missing purge leg %s in %+v", sym, book.Legs)
	return purgeBookLeg{}
}
