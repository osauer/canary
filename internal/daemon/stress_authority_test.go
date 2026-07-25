package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

type stressAuthorityTestReader struct {
	at            time.Time
	accountResult *rpc.AccountResult
	positionsBook *rpc.PositionsResult
	regimeResult  *rpc.RegimeSnapshotResult
	eventsResult  *rpc.MarketEventsResult
	eventSymbols  []string
}

func (r *stressAuthorityTestReader) ready() bool { return true }

func (r *stressAuthorityTestReader) account(context.Context) (*rpc.AccountResult, error) {
	return r.accountResult, nil
}

func (r *stressAuthorityTestReader) positions(context.Context) (*rpc.PositionsResult, error) {
	return r.positionsBook, nil
}

func (r *stressAuthorityTestReader) regime(context.Context) (*rpc.RegimeSnapshotResult, error) {
	return r.regimeResult, nil
}

func (r *stressAuthorityTestReader) marketEvents(_ context.Context, symbols []string) (*rpc.MarketEventsResult, error) {
	r.eventSymbols = slices.Clone(symbols)
	return r.eventsResult, nil
}

func (r *stressAuthorityTestReader) now() time.Time { return r.at }

func TestStressDecisionDTOAuthorityTimestamps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	accountAsOf := now.Add(-time.Second)

	accountCases := []struct {
		name      string
		authority accountSummaryAuthority
		want      time.Time
	}{
		{name: "current request", authority: accountSummaryAuthority{Provenance: ibkrlib.AccountSummaryProvenanceRequest, AsOf: accountAsOf}, want: accountAsOf},
		{name: "cached fallback", authority: accountSummaryAuthority{Provenance: ibkrlib.AccountSummaryProvenanceCachedFallback, AsOf: now}},
		{name: "unstamped request", authority: accountSummaryAuthority{Provenance: ibkrlib.AccountSummaryProvenanceRequest}},
		{name: "future request", authority: accountSummaryAuthority{Provenance: ibkrlib.AccountSummaryProvenanceRequest, AsOf: now.Add(time.Second)}},
	}
	for _, test := range accountCases {
		t.Run("account/"+test.name, func(t *testing.T) {
			t.Parallel()
			if got := accountResultAuthorityAsOf(test.authority, now); !got.Equal(test.want) {
				t.Fatalf("account authority as_of = %s, want %s", got, test.want)
			}
		})
	}

	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	currentAt := now.Add(-time.Minute)
	positionCases := []struct {
		name   string
		health ibkrlib.PortfolioStreamHealth
		want   time.Time
	}{
		{name: "current complete stream", health: ibkrlib.PortfolioStreamHealth{Account: scope.Account, InitialCompletedAt: currentAt}, want: currentAt},
		{name: "unprimed stream", health: ibkrlib.PortfolioStreamHealth{Account: scope.Account, RequestedAt: now.Add(-time.Minute)}},
		{name: "stale stream", health: ibkrlib.PortfolioStreamHealth{Account: scope.Account, InitialCompletedAt: now.Add(-portfolioStreamReceiptMaxAge - time.Second)}},
		{name: "foreign stream", health: ibkrlib.PortfolioStreamHealth{Account: "DU999", InitialCompletedAt: currentAt}},
	}
	for _, test := range positionCases {
		t.Run("positions/"+test.name, func(t *testing.T) {
			t.Parallel()
			if got := positionsResultAuthorityAsOf(scope, test.health, now); !got.Equal(test.want) {
				t.Fatalf("positions authority as_of = %s, want %s", got, test.want)
			}
		})
	}
}

func TestStressEvaluationTickRejectsRestampedCachedAccountFallback(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	account := stressAuthorityTestAccount(now)
	// Preserve the fallback's known exposure as context. Its ratio would be
	// actionable if fresh, so the authority loss must turn fit unknown rather
	// than interpreting the current positions DTO as a clean empty book.
	account.GrossPositionValue = 200_000
	// CachedAccountSummary reparses streaming rows and stamps them at read
	// time. The daemon DTO boundary must discard that display timestamp.
	account.AsOf = accountResultAuthorityAsOf(accountSummaryAuthority{
		Provenance: ibkrlib.AccountSummaryProvenanceCachedFallback,
		AsOf:       now,
	}, now)
	positions := &rpc.PositionsResult{
		AsOf:      positionsResultAuthorityAsOf(stressAuthorityTestScope(), stressAuthorityCurrentPortfolioHealth(now), now),
		Stocks:    []rpc.PositionView{},
		Options:   []rpc.PositionView{},
		Portfolio: &rpc.PositionsPortfolio{},
	}
	reader := &stressAuthorityTestReader{
		at: now, accountResult: account, positionsBook: positions,
		regimeResult: stressAuthorityHealthyRegime(now),
		eventsResult: stressAuthorityHealthyMarketEvents(now),
	}

	line := runStressAuthorityTick(t, reader)
	if !line.SourceAsOf.Account.IsZero() {
		t.Fatalf("cached account fallback source_as_of = %s, want unavailable", line.SourceAsOf.Account)
	}
	if line.InputHealth != "degraded" || line.Action != "confirm_inputs" || line.PortfolioFit != "unknown" {
		t.Fatalf("cached account fallback decision = %s/%s fit=%s, want degraded/confirm_inputs/unknown", line.InputHealth, line.Action, line.PortfolioFit)
	}
	if line.PortfolioAlertRelevant == nil || !*line.PortfolioAlertRelevant {
		t.Fatalf("cached account fallback was treated as a clean irrelevant book: %+v", line.PortfolioAlertRelevant)
	}
}

func TestStressEvaluationTickRejectsUnprimedAndStaleKnownPortfolio(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		health ibkrlib.PortfolioStreamHealth
	}{
		{name: "unprimed", health: ibkrlib.PortfolioStreamHealth{Account: "DU123", RequestedAt: now.Add(-time.Minute)}},
		{name: "stale", health: ibkrlib.PortfolioStreamHealth{Account: "DU123", InitialCompletedAt: now.Add(-portfolioStreamReceiptMaxAge - time.Second)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			positions := stressAuthorityKnownPortfolio(now)
			positions.AsOf = positionsResultAuthorityAsOf(stressAuthorityTestScope(), test.health, now)
			reader := &stressAuthorityTestReader{
				at: now, accountResult: stressAuthorityTestAccount(now), positionsBook: positions,
				regimeResult: stressAuthorityHealthyRegime(now),
				eventsResult: stressAuthorityHealthyMarketEvents(now, "EARN"),
			}

			line := runStressAuthorityTick(t, reader)
			if !line.SourceAsOf.Positions.IsZero() {
				t.Fatalf("%s positions source_as_of = %s, want unavailable", test.name, line.SourceAsOf.Positions)
			}
			if line.InputHealth != "degraded" || line.Action != "confirm_inputs" || line.PortfolioFit != "unknown" {
				t.Fatalf("%s positions decision = %s/%s fit=%s, want degraded/confirm_inputs/unknown", test.name, line.InputHealth, line.Action, line.PortfolioFit)
			}
			if !slices.Equal(reader.eventSymbols, []string{"EARN"}) {
				t.Fatalf("%s positions market-event symbols = %v, want known holding preserved", test.name, reader.eventSymbols)
			}
			if line.PortfolioAlertRelevant == nil || !*line.PortfolioAlertRelevant {
				t.Fatalf("%s positions were treated as a clean irrelevant book: %+v", test.name, line.PortfolioAlertRelevant)
			}
		})
	}
}

func TestStressEvaluationTickPreservesCurrentAuthority(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	account := stressAuthorityTestAccount(now)
	account.AsOf = accountResultAuthorityAsOf(accountSummaryAuthority{
		Provenance: ibkrlib.AccountSummaryProvenanceRequest,
		AsOf:       now.Add(-time.Second),
	}, now)
	positions := &rpc.PositionsResult{
		AsOf:      positionsResultAuthorityAsOf(stressAuthorityTestScope(), stressAuthorityCurrentPortfolioHealth(now), now),
		Stocks:    []rpc.PositionView{},
		Options:   []rpc.PositionView{},
		Portfolio: &rpc.PositionsPortfolio{},
	}
	reader := &stressAuthorityTestReader{
		at: now, accountResult: account, positionsBook: positions,
		regimeResult: stressAuthorityHealthyRegime(now),
		eventsResult: stressAuthorityHealthyMarketEvents(now),
	}

	line := runStressAuthorityTick(t, reader)
	if !line.SourceAsOf.Account.Equal(now.Add(-time.Second)) || !line.SourceAsOf.Positions.Equal(now.Add(-time.Minute)) {
		t.Fatalf("current source times = %+v, want request and portfolio receipts", line.SourceAsOf)
	}
	if line.InputHealth != "ok" || line.Action != "stand_down" {
		t.Fatalf("current empty portfolio decision = %s/%s, want ok/stand_down", line.InputHealth, line.Action)
	}
}

func runStressAuthorityTick(t *testing.T, reader *stressAuthorityTestReader) stressDecisionLine {
	t.Helper()
	server := &Server{
		logger:                              NewLogger(&bytes.Buffer{}, "error"),
		stressDecisions:                     &stressDecisionJournal{path: filepath.Join(t.TempDir(), "canary-decisions.jsonl")},
		stressEvaluationSourceReaderForTest: reader,
	}
	if !server.stressEvaluationTick(t.Context()) {
		t.Fatal("canary evaluation tick did not publish")
	}
	raw, err := os.ReadFile(server.stressDecisions.path)
	if err != nil {
		t.Fatalf("read canary decision: %v", err)
	}
	var line stressDecisionLine
	if err := json.Unmarshal(bytes.TrimSpace(raw), &line); err != nil {
		t.Fatalf("decode canary decision: %v", err)
	}
	return line
}

func stressAuthorityTestScope() brokerStateScope {
	return brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
}

func stressAuthorityCurrentPortfolioHealth(now time.Time) ibkrlib.PortfolioStreamHealth {
	return ibkrlib.PortfolioStreamHealth{Account: "DU123", InitialCompletedAt: now.Add(-time.Minute)}
}

func stressAuthorityTestAccount(now time.Time) *rpc.AccountResult {
	dailyPnL := 0.0
	return &rpc.AccountResult{
		AccountID: "DU123", BaseCurrency: "USD", NetLiquidation: 100_000,
		DailyPnL: &dailyPnL, AsOf: now,
	}
}

func stressAuthorityKnownPortfolio(now time.Time) *rpc.PositionsResult {
	marketPct := 40.0
	stock := rpc.PositionView{Symbol: "EARN", SecType: rpc.SecTypeStock, Quantity: 200}
	return &rpc.PositionsResult{
		AsOf: now, Stocks: []rpc.PositionView{stock}, Options: []rpc.PositionView{},
		ByUnderlying: []rpc.PositionGroup{{Underlying: "EARN", Stock: &stock, Options: []rpc.PositionView{}}},
		Portfolio: &rpc.PositionsPortfolio{ExposureBase: []rpc.UnderlyingExposure{{
			Underlying: "EARN", MarketValueBase: 40_000, MarketValuePctNLV: &marketPct,
		}}},
	}
}

func stressAuthorityHealthyRegime(now time.Time) *rpc.RegimeSnapshotResult {
	green := rpc.RegimeIndicatorMeta{Band: "green"}
	return &rpc.RegimeSnapshotResult{
		AsOf:             now,
		Composite:        rpc.RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		VIXTermStructure: rpc.RegimeVIXTerm{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		VolOfVol:         rpc.RegimeVolOfVol{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		CreditSpreads:    rpc.RegimeCreditSpreads{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		FundingStress:    rpc.RegimeFundingStress{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		USDJPY:           rpc.RegimeUSDJPY{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
		GammaZero: rpc.RegimeGammaZero{
			RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: &rpc.GammaZeroComputed{
				Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable},
			}},
		},
		Breadth: rpc.RegimeBreadth{RegimeIndicatorMeta: green, Status: rpc.RegimeStatusOK},
	}
}

func stressAuthorityHealthyMarketEvents(now time.Time, symbols ...string) *rpc.MarketEventsResult {
	return &rpc.MarketEventsResult{
		Kind: rpc.MarketEventsKind, SchemaVersion: rpc.MarketEventsSchemaVersion,
		AsOf: now, Symbols: slices.Clone(symbols), BySymbol: map[string][]rpc.MarketEventFlag{},
		SourceHealth: []rpc.SourceHealth{
			{Source: "reg_sho_threshold", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: 3600},
			{Source: "trading_halts", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: 3600},
		},
	}
}
