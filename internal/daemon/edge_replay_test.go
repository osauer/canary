package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
)

// replayFlexDay is one synthetic daily statement: the SYNX call position and
// mark on that day, and the executions reported on it.
type replayFlexDay struct {
	day      string
	quantity float64
	mark     float64
	trades   string
}

func replayFlexXML(account string, days []replayFlexDay) string {
	var b strings.Builder
	b.WriteString("<FlexQueryResponse><FlexStatements>")
	for _, d := range days {
		compact := strings.ReplaceAll(d.day, "-", "")
		fmt.Fprintf(&b, `<FlexStatement accountId="%s" fromDate="%s" toDate="%s" whenGenerated="%s;230000">`, account, compact, compact, compact)
		fmt.Fprintf(&b, `<Trades>%s</Trades><OpenPositions>`, d.trades)
		if d.quantity != 0 {
			fmt.Fprintf(&b, `<OpenPosition accountId="%s" assetCategory="OPT" currency="USD" fxRateToBase="0.9" symbol="SYNX  261016C00100000" conid="9001" underlyingSymbol="SYNX" reportDate="%s" position="%g" multiplier="100" markPrice="%g" costBasisMoney="401" levelOfDetail="SUMMARY" />`, account, compact, d.quantity, d.mark)
		}
		b.WriteString(`</OpenPositions><SecuritiesInfo><SecurityInfo assetCategory="OPT" currency="USD" symbol="SYNX  261016C00100000" conid="9001" underlyingSymbol="SYNX" multiplier="100" strike="100" expiry="20261016" putCall="C" /></SecuritiesInfo>`)
		fmt.Fprintf(&b, `<ConversionRates><ConversionRate dateTime="%s;000000" fromCurrency="USD" toCurrency="EUR" rate="0.9" /></ConversionRates>`, compact)
		fmt.Fprintf(&b, `<EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="%s" total="100000" /></EquitySummaryInBase>`, compact)
		b.WriteString("</FlexStatement>")
	}
	b.WriteString("</FlexStatements></FlexQueryResponse>")
	return b.String()
}

func replayFlexTrade(account, id, day, side string, quantity, price, net, realized float64, openClose string) string {
	compact := strings.ReplaceAll(day, "-", "")
	return fmt.Sprintf(`<Trade accountId="%s" assetCategory="OPT" currency="USD" fxRateToBase="0.9" symbol="SYNX  261016C00100000" conid="9001" underlyingSymbol="SYNX" multiplier="100" tradeID="t-%s" ibOrderID="o-%s" ibExecID="e-%s" transactionID="x-%s" tradeDate="%s" tradeTime="150000" reportDate="%s" buySell="%s" quantity="%g" tradePrice="%g" proceeds="0" IBCommission="-1" IBCommissionCurrency="USD" taxes="0" openCloseIndicator="%s" cost="0" fifoPnlRealized="%g" mtmPnl="0" netCash="%g" levelOfDetail="EXECUTION" />`,
		account, id, id, id, id, compact, compact, side, quantity, price, openClose, realized, net)
}

func TestEdgeReplayReadsTheProjectionHistoryAndPolicyInForce(t *testing.T) {
	const account = "UREPLAYSYNTH"
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	raw := []byte(replayFlexXML(account, []replayFlexDay{
		{day: "2026-09-01"},
		{day: "2026-09-02", quantity: 2, mark: 2.00, trades: replayFlexTrade(account, "buy", "2026-09-02", "BUY", 2, 2.00, -401, 0, "O")},
		{day: "2026-09-03", quantity: 2, mark: 1.50},
		{day: "2026-09-04", quantity: 2, mark: 0.70},
		{day: "2026-09-07", trades: replayFlexTrade(account, "sell", "2026-09-07", "SELL", -2, 0.50, 99, -302, "C")},
	}))
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	queryFingerprint := flexQueryFingerprint("424242")
	files, days, records, versions, err := buildStatementProjection([]statementProjectionFile{{name: "replay.xml", size: int64(len(raw)), digest: sha256.Sum256(raw), data: raw, statements: statements}}, now, queryFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ReplaceStatementProjection(t.Context(), statementProjectionScopeForSelection(flexEvidenceSelection{ActiveQueryFingerprint: queryFingerprint}), files, days, records, versions); err != nil {
		t.Fatal(err)
	}
	port := 4001
	srv := &Server{
		cfg:       &config.Resolved{Gateway: config.Gateway{Account: account, Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "424242"}},
		coreStore: store, now: func() time.Time { return now },
	}
	seedReplayHistory(t, srv, store, now)

	result, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != rpc.EdgeStateCurrent || !result.NotExecution || result.BaseCurrency != "EUR" || result.Scope.Contracts != 1 {
		t.Fatalf("replay = %+v", result)
	}
	if loss := result.Parameters.Rules[0]; loss.Name != "loss_exit_pct" || loss.Value != 60 || !strings.Contains(result.Parameters.PolicySource, "Rulebook") {
		t.Fatalf("parameters = %+v", result.Parameters)
	}
	if result.Parameters.HalfSpreadPct != nil || !strings.Contains(result.Parameters.HalfSpreadSource, "no measured option fill") {
		t.Fatalf("half-spread without fills = %v %q", result.Parameters.HalfSpreadPct, result.Parameters.HalfSpreadSource)
	}
	var run *rpc.EdgeReplayRun
	for i := range result.Runs {
		if result.Runs[i].ID == rpc.EdgeReplayRuleLossExit && result.Runs[i].Prices == rpc.EdgeReplayPricesRecorded {
			run = &result.Runs[i]
		}
	}
	if run == nil || len(run.ExitsByReason) != 1 || run.ExitsByReason[0].Units != 2 || run.Coverage.StatementDays != 3 {
		t.Fatalf("loss-exit run = %+v; the cached history must decide 2026-09-04", run)
	}
	// 0.70 x 100 less the 0.50 average commission, at 0.9, against a cost of 401 x 0.9.
	if want := (2*(70-0.5) - 401) * 0.9; math.Abs(run.PNLBase-want) > 1e-6 {
		t.Fatalf("loss-exit P/L = %.4f, want %.4f", run.PNLBase, want)
	}
	if !result.Reconciliation.Reconciled || result.Reconciliation.ComparedContracts != 1 {
		t.Fatalf("reconciliation = %+v", result.Reconciliation)
	}
}

// seedReplayHistory retains a daily SYNX series under the key the replay
// reads, as the market view would have left it.
func seedReplayHistory(t *testing.T, srv *Server, store *corestore.Store, now time.Time) {
	t.Helper()
	seedReplayHistoryWithout(t, srv, store, now, nil)
}

// seedReplayHistoryWithout is seedReplayHistory without a bar on the days
// named.
func seedReplayHistoryWithout(t *testing.T, srv *Server, store *corestore.Store, now time.Time, without map[string]bool) {
	t.Helper()
	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "6M"}
	key, normalized, err := marketHistoryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	result := rpc.MarketHistoryResult{TimestampKind: "session_date", PriceBasis: "TRADES", RegularHoursOnly: true, Contract: normalized.Contract, Range: "6M", Interval: "1 day", Source: "hmds", AsOf: now, CoverageStatus: "complete"}
	result.Contract.ConID = 4242
	for day := now.AddDate(0, -5, 0); day.Before(now.AddDate(0, 0, -1)); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday || without[day.Format(time.DateOnly)] {
			continue
		}
		at := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		result.Points = append(result.Points, rpc.MarketHistoryPoint{At: at, Value: 95, Open: new(95.0), High: new(96.0), Low: new(94.0)})
	}
	result.RequestedStart, result.Start, result.End = result.Points[0].At, result.Points[0].At, result.Points[len(result.Points)-1].At
	payload, err := json.Marshal(storedMarketHistory{Version: 1, Identity: key, FullReadAt: now, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveChartCache(t.Context(), key, payload); err != nil {
		t.Fatal(err)
	}
	if got := replayHistoryRange(nil, time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC), srv.edgeNow()); got != "6M" {
		t.Fatalf("history range = %s, want the cached 6M", got)
	}
}

func TestEdgeReplayNeedsFlex(t *testing.T) {
	srv := &Server{cfg: &config.Resolved{}}
	result, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil || result.State != rpc.EdgeStateActionRequired || result.Reason != "flex_configuration_required" || !result.NotExecution {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if _, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["E"]}`)}); err == nil {
		t.Fatal("unknown rule accepted")
	}
}

// replayTestServer is the SYNX replay book of
// TestEdgeReplayReadsTheProjectionHistoryAndPolicyInForce on a server
// without a broker gateway.
func replayTestServer(t *testing.T, now time.Time) (*Server, *corestore.Store) {
	t.Helper()
	const account = "UREPLAYSYNTH"
	raw := []byte(replayFlexXML(account, []replayFlexDay{
		{day: "2026-09-01"},
		{day: "2026-09-02", quantity: 2, mark: 2.00, trades: replayFlexTrade(account, "buy", "2026-09-02", "BUY", 2, 2.00, -401, 0, "O")},
		{day: "2026-09-03", quantity: 2, mark: 1.50},
		{day: "2026-09-04", quantity: 2, mark: 0.70},
		{day: "2026-09-07", trades: replayFlexTrade(account, "sell", "2026-09-07", "SELL", -2, 0.50, 99, -302, "C")},
	}))
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	queryFingerprint := flexQueryFingerprint("424242")
	files, days, records, versions, err := buildStatementProjection([]statementProjectionFile{{name: "replay.xml", size: int64(len(raw)), digest: sha256.Sum256(raw), data: raw, statements: statements}}, now, queryFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ReplaceStatementProjection(t.Context(), statementProjectionScopeForSelection(flexEvidenceSelection{ActiveQueryFingerprint: queryFingerprint}), files, days, records, versions); err != nil {
		t.Fatal(err)
	}
	port := 4001
	return &Server{
		cfg:       &config.Resolved{Gateway: config.Gateway{Account: account, Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "424242"}},
		coreStore: store, now: func() time.Time { return now },
	}, store
}

func replayLossRun(t *testing.T, result *rpc.EdgeReplayResult) rpc.EdgeReplayRun {
	t.Helper()
	for _, run := range result.Runs {
		if run.ID == rpc.EdgeReplayRuleLossExit && run.Prices == rpc.EdgeReplayPricesRecorded {
			return run
		}
	}
	t.Fatalf("no recorded loss-exit run in %+v", result)
	return rpc.EdgeReplayRun{}
}

// A replay run once must give the same answer later with the gateway
// offline, after the market-history cache has evicted the series it read.
func TestEdgeReplayReproducesAfterTheChartCacheEvicts(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, store := replayTestServer(t, now)
	seedReplayHistory(t, srv, store, now)
	params := &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)}
	first, err := srv.handleEdgeReplay(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	before := replayLossRun(t, first)
	if before.Coverage.StatementDays != 3 || len(before.ExitsByReason) != 1 {
		t.Fatalf("first run = %+v; the cached history must decide 2026-09-04", before)
	}

	// The display cache evicts its oldest series at 750; fill it past that
	// with other symbols so the SYNX series goes.
	for i := range 750 {
		p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: fmt.Sprintf("SYNF%03d", i), SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "6M"}
		key, _, err := marketHistoryIdentity(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SaveChartCache(t.Context(), key, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	synx, _, _ := marketHistoryIdentity(rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "6M"})
	if saved, _, err := srv.loadMarketHistory(t.Context(), synx); err != nil || saved != nil {
		t.Fatalf("SYNX chart series still cached (%v); the fixture did not evict it", err)
	}

	second, err := srv.handleEdgeReplay(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	after := replayLossRun(t, second)
	if math.Abs(after.PNLBase-before.PNLBase) > 1e-9 || after.Coverage != before.Coverage {
		t.Fatalf("replay after eviction = %.4f %+v, want the first run's %.4f %+v", after.PNLBase, after.Coverage, before.PNLBase, before.Coverage)
	}
}

// An index underlying's history is an index series, as the market tape reads
// it, never a stock of the same name.
func TestEdgeReplayAsksIndexHistoryAsAnIndex(t *testing.T) {
	index := func(symbol string) (rpc.ContractParams, bool) {
		if symbol == "SYNIX" {
			return rpc.ContractParams{Symbol: "SYNIX", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"}, true
		}
		return rpc.ContractParams{}, false
	}
	got := replayHistoryContract(edgecore.ExitReplayUnderlying{Symbol: "SYNIX", Currency: "USD"}, index)
	if want := (rpc.ContractParams{Symbol: "SYNIX", SecType: "IND", Exchange: "CBOE", Currency: "USD"}); got != want {
		t.Fatalf("index history contract = %+v, want the index on CBOE as the market tape names it", got)
	}
	if stock := replayHistoryContract(edgecore.ExitReplayUnderlying{Symbol: "SYNX", Currency: "USD"}, index); stock.SecType != "STK" || stock.Exchange != "SMART" {
		t.Fatalf("stock history contract = %+v", stock)
	}
}

// With the gateway offline and nothing retained for SYNX, the replay names
// it and why instead of keeping the owner's outcome for its days.
func TestEdgeReplayNamesHistoryMissingWithTheGatewayOffline(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, _ := replayTestServer(t, now)
	result, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != rpc.EdgeStateInsufficient || result.Reason != "underlying_history_unavailable" || !result.Incomplete || len(result.MissingHistory) != 1 {
		t.Fatalf("replay = %s %s incomplete=%v missing=%+v", result.State, result.Reason, result.Incomplete, result.MissingHistory)
	}
	gap := result.MissingHistory[0]
	if gap.Underlying != "SYNX" || gap.Reason != rpc.EdgeReplayHistoryGatewayOffline || gap.Detail != "no retained history" || gap.Contracts != 1 {
		t.Fatalf("missing history = %+v", gap)
	}
	if err := rpc.ValidateEdgeReplayResult(*result); err != nil {
		t.Fatal(err)
	}
}

// replayMandateSeries is a daily SYNX close series as the mandate keeps it,
// under key and for contract.
func replayMandateSeries(now time.Time, key string, contract rpc.ContractParams) *mandateHistorySeries {
	series := &mandateHistorySeries{Version: mandateHistoryDocVer, Key: key, Contract: contract, FetchedAt: now, Source: "synthetic"}
	for day := now.AddDate(0, -5, 0); day.Before(now.AddDate(0, 0, -1)); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		series.Dates, series.Closes = append(series.Dates, day.Format(time.DateOnly)), append(series.Closes, 95)
	}
	return series
}

// The mandate's decision-grade closes are retained and never evicted: with
// no chart series and the gateway offline the replay reads them, ranges
// from closes only, and keeps the bars it used. The mandate keeps a series
// under its identity-bound key (bindMandateHistoryNeed), here as its
// background read leaves it for an underlying held only through options,
// and written by an earlier process so the replay reads it from the store.
func TestEdgeReplayReadsTheMandatesRetainedClosesFirst(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, store := replayTestServer(t, now)
	need := bindMandateHistoryNeed(mandateHistoryNeed{Key: "u:SYNX", Contract: mandateOptionUnderlyingContract("SYNX", "USD")})
	series := replayMandateSeries(now, need.Key, need.Contract)
	earlier := &Server{coreStore: store, now: srv.now}
	if err := earlier.saveMandateSeries(t.Context(), series); err != nil {
		t.Fatal(err)
	}
	result, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit","stock_trail"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != rpc.EdgeStateCurrent || result.Incomplete || len(result.MissingHistory) != 0 {
		t.Fatalf("replay = %s %s missing=%+v; the mandate's identity-bound closes must serve it", result.State, result.Reason, result.MissingHistory)
	}
	if run := replayLossRun(t, result); run.Coverage.StatementDays != 3 || len(run.ExitsByReason) != 1 {
		t.Fatalf("loss-exit run = %+v; the mandate's closes must decide 2026-09-04", run)
	}
	for _, run := range result.Runs {
		if run.ID == rpc.EdgeReplayRuleStockTrail && run.Prices == rpc.EdgeReplayPricesRecorded && run.Coverage.CloseOnlyRangeDays == 0 {
			t.Fatalf("stock trail over closes only counts no close-only range: %+v", run.Coverage)
		}
	}
	doc, ok, err := store.GetStateDocument(t.Context(), replayHistoryScope(rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Currency: "USD"}), stateKindReplayHistory)
	if err != nil || !ok {
		t.Fatalf("replay history not retained: %v %v", ok, err)
	}
	var kept replayHistoryDoc
	if err := json.Unmarshal(doc.JSON, &kept); err != nil || len(kept.Bars) != len(series.Dates) {
		t.Fatalf("retained %d bars (%v), want the mandate's %d", len(kept.Bars), err, len(series.Dates))
	}
}

// A held stock's series is bound to the position's contract ID and listing,
// which the replay does not know; the series the mandate has loaded serves
// it when one contract of that symbol, type and currency matches. A legacy
// symbol-only document serves only when its contract matches, as the
// mandate accepts it; one for another currency does not.
func TestEdgeReplayReadsTheMandatesHeldStockAndLegacyCloses(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	replay := func(t *testing.T, srv *Server) *rpc.EdgeReplayResult {
		t.Helper()
		result, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	t.Run("held stock", func(t *testing.T) {
		srv, _ := replayTestServer(t, now)
		held := rpc.ContractParams{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
		need := bindMandateHistoryNeed(mandateHistoryNeed{Key: "u:SYNX", Contract: held})
		if err := srv.saveMandateSeries(t.Context(), replayMandateSeries(now, need.Key, need.Contract)); err != nil {
			t.Fatal(err)
		}
		if result := replay(t, srv); result.State != rpc.EdgeStateCurrent || result.Incomplete {
			t.Fatalf("replay = %s missing=%+v; the loaded held-stock series must serve it", result.State, result.MissingHistory)
		}
		// A fresh book (the first run kept its bars) with two SYNX contracts.
		srv, _ = replayTestServer(t, now)
		other := held
		other.ConID = 4343
		for _, contract := range []rpc.ContractParams{held, other} {
			need := bindMandateHistoryNeed(mandateHistoryNeed{Key: "u:SYNX", Contract: contract})
			if err := srv.saveMandateSeries(t.Context(), replayMandateSeries(now, need.Key, need.Contract)); err != nil {
				t.Fatal(err)
			}
		}
		if result := replay(t, srv); !result.Incomplete {
			t.Fatal("two SYNX contracts' series served the replay; distinct contracts must not")
		}
	})
	t.Run("legacy", func(t *testing.T) {
		srv, _ := replayTestServer(t, now)
		if err := srv.saveMandateSeries(t.Context(), replayMandateSeries(now, "u:SYNX", rpc.ContractParams{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"})); err != nil {
			t.Fatal(err)
		}
		if result := replay(t, srv); result.State != rpc.EdgeStateCurrent || result.Incomplete {
			t.Fatalf("replay = %s missing=%+v; a matching legacy document serves", result.State, result.MissingHistory)
		}
	})
	t.Run("legacy other currency", func(t *testing.T) {
		srv, _ := replayTestServer(t, now)
		if err := srv.saveMandateSeries(t.Context(), replayMandateSeries(now, "u:SYNX", rpc.ContractParams{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "EUR"})); err != nil {
			t.Fatal(err)
		}
		if result := replay(t, srv); !result.Incomplete {
			t.Fatal("a euro SYNX series served a dollar underlying")
		}
	})
}

// A fresher read that restates an overlapping close (a split) replaces what
// was read before it; a close-only bar with the same close keeps the range.
func TestMergeReplaySourcesRestatementAndRanges(t *testing.T) {
	day := func(s string) time.Time { v, _ := time.Parse(time.DateOnly, s); return v }
	bar := func(d string, c, h, l float64) edgecore.UnderlyingBar {
		return edgecore.UnderlyingBar{Day: day(d), Close: c, High: h, Low: l}
	}
	src := func(asOf string, bars ...edgecore.UnderlyingBar) replaySource {
		out := replaySource{asOf: day(asOf), bars: map[string]edgecore.UnderlyingBar{}}
		for _, b := range bars {
			out.bars[b.Day.Format(time.DateOnly)] = b
		}
		return out
	}
	kept := mergeReplaySources([]replaySource{
		src("2026-09-10", bar("2026-09-01", 100.2, 0, 0), bar("2026-09-02", 101, 0, 0)),
		src("2026-09-05", bar("2026-09-01", 100, 101, 99), bar("2026-08-31", 99, 100, 98)),
	})
	if len(kept) != 3 || kept[1].High != 101 || kept[1].Close != 100 || kept[2].Close != 101 {
		t.Fatalf("merged = %+v; the close-only 09-01 bar must keep the range", kept)
	}
	split := mergeReplaySources([]replaySource{
		src("2026-09-05", bar("2026-08-31", 200, 0, 0), bar("2026-09-01", 200, 0, 0)),
		src("2026-09-10", bar("2026-09-01", 100, 0, 0), bar("2026-09-02", 101, 0, 0)),
	})
	if len(split) != 2 || split[0].Close != 100 {
		t.Fatalf("merged across a restatement = %+v; the older basis must go", split)
	}
}

// replayServerWith is replayTestServer over the statement days given.
func replayServerWith(t *testing.T, now time.Time, days []replayFlexDay) (*Server, *corestore.Store) {
	t.Helper()
	const account = "UREPLAYSYNTH"
	for i := range days {
		days[i].trades = strings.ReplaceAll(days[i].trades, "ACCOUNT", account)
	}
	raw := []byte(replayFlexXML(account, days))
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	queryFingerprint := flexQueryFingerprint("424242")
	files, projectionDays, records, versions, err := buildStatementProjection([]statementProjectionFile{{name: "replay.xml", size: int64(len(raw)), digest: sha256.Sum256(raw), data: raw, statements: statements}}, now, queryFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ReplaceStatementProjection(t.Context(), statementProjectionScopeForSelection(flexEvidenceSelection{ActiveQueryFingerprint: queryFingerprint}), files, projectionDays, records, versions); err != nil {
		t.Fatal(err)
	}
	port := 4001
	return &Server{
		cfg:       &config.Resolved{Gateway: config.Gateway{Account: account, Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "424242"}},
		coreStore: store, now: func() time.Time { return now },
	}, store
}

// A statement day on which the underlying's exchange was closed (Labor Day
// 2026-09-07 on the US calendar) has no daily bar. No rule can act on a
// closed session, so the day carries the previous close and option mark and
// the replay goes on: the loss line still fires on 2026-09-08. A session the
// exchange held without a bar (2026-09-09 here) stays untestable.
func TestEdgeReplayCarriesAClosedExchangeSession(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	held := func(day string, mark float64) replayFlexDay { return replayFlexDay{day: day, quantity: 2, mark: mark} }
	srv, store := replayServerWith(t, now, []replayFlexDay{
		{day: "2026-09-01"},
		{day: "2026-09-02", quantity: 2, mark: 2.00, trades: replayFlexTrade("ACCOUNT", "buy", "2026-09-02", "BUY", 2, 2.00, -401, 0, "O")},
		held("2026-09-03", 1.80),
		held("2026-09-04", 1.70),
		held("2026-09-07", 1.70),
		held("2026-09-08", 0.70),
		{day: "2026-09-10", trades: replayFlexTrade("ACCOUNT", "sell", "2026-09-10", "SELL", -2, 0.50, 99, -302, "C")},
	})
	seedReplayHistoryWithout(t, srv, store, now, map[string]bool{"2026-09-07": true})
	result, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != rpc.EdgeStateCurrent || result.Incomplete {
		t.Fatalf("replay = %s %s missing=%+v", result.State, result.Reason, result.MissingHistory)
	}
	run := replayLossRun(t, result)
	if run.Coverage.UntestableEvents != 0 || len(run.ExitsByReason) != 1 || run.ExitsByReason[0].Units != 2 {
		t.Fatalf("loss-exit run = %+v; the holiday must carry, and the loss line fire on 2026-09-08", run)
	}
	if want := (2*(70-0.5) - 401) * 0.9; math.Abs(run.PNLBase-want) > 1e-6 {
		t.Fatalf("loss-exit P/L = %.4f, want %.4f", run.PNLBase, want)
	}
	if run.Coverage.ClosedSessionDays != 1 || run.Coverage.StatementDays != 4 {
		t.Fatalf("coverage = %+v, want one closed-session day carried and four marked days", run.Coverage)
	}

	// The same book without a bar on an open session stays untestable.
	srv, store = replayServerWith(t, now, []replayFlexDay{
		{day: "2026-09-01"},
		{day: "2026-09-02", quantity: 2, mark: 2.00, trades: replayFlexTrade("ACCOUNT", "buy", "2026-09-02", "BUY", 2, 2.00, -401, 0, "O")},
		held("2026-09-03", 1.80),
		held("2026-09-08", 0.70),
		{day: "2026-09-10", trades: replayFlexTrade("ACCOUNT", "sell", "2026-09-10", "SELL", -2, 0.50, 99, -302, "C")},
	})
	seedReplayHistoryWithout(t, srv, store, now, map[string]bool{"2026-09-08": true})
	result, err = srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if run := replayLossRun(t, result); run.Coverage.UntestableEvents != 1 || run.Coverage.ClosedSessionDays != 0 || len(run.ExitsByReason) != 0 {
		t.Fatalf("open session without a bar = %+v, want untestable", run)
	}
}

// replayHistoryResult is a daily SYNX series as the broker's chart read
// answers it.
func replayHistoryResult(t *testing.T, now time.Time) *rpc.MarketHistoryResult {
	t.Helper()
	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "6M"}
	_, normalized, err := marketHistoryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	result := &rpc.MarketHistoryResult{TimestampKind: "session_date", PriceBasis: "TRADES", RegularHoursOnly: true, Contract: normalized.Contract, Range: "6M", Interval: "1 day", Source: "hmds", AsOf: now, CoverageStatus: "complete"}
	for day := now.AddDate(0, -5, 0); day.Before(now.AddDate(0, 0, -1)); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		at := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		result.Points = append(result.Points, rpc.MarketHistoryPoint{At: at, Value: 95, Open: new(95.0), High: new(96.0), Low: new(94.0)})
	}
	return result
}

// After a history gap the broker may take longer to send a name's history
// than the replay's budget allows. The replay answers within its budget
// with the name left out as still fetching, the read goes on, and the next
// run finds the bars kept without asking the broker again.
func TestEdgeReplayAnswersWhileTheBrokerIsStillSendingHistory(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, store := replayTestServer(t, now)
	release := make(chan struct{})
	reads := make(chan struct{}, 4)
	srv.replayHistoryFetchForTest = func(ctx context.Context, p rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		reads <- struct{}{}
		select {
		case <-release:
			return replayHistoryResult(t, now), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	result, err := srv.handleEdgeReplay(ctx, &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)})
	if err != nil {
		t.Fatalf("replay with a slow history read = %v after %s; it must answer within its budget", err, time.Since(started).Round(time.Millisecond))
	}
	if !result.Incomplete || len(result.MissingHistory) != 1 || result.MissingHistory[0].Reason != rpc.EdgeReplayHistoryFetching || result.MissingHistory[0].Underlying != "SYNX" {
		t.Fatalf("replay = %s %s missing=%+v, want SYNX left out as still fetching", result.State, result.Reason, result.MissingHistory)
	}
	if err := rpc.ValidateEdgeReplayResult(*result); err != nil {
		t.Fatal(err)
	}

	// The read finishes in the background and its bars are kept.
	close(release)
	scope := replayHistoryScope(rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Currency: "USD"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok, err := store.GetStateDocument(t.Context(), scope, stateKindReplayHistory); err == nil && ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background history read kept nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := len(reads)
	second, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != rpc.EdgeStateCurrent || second.Incomplete || len(reads) != before {
		t.Fatalf("second replay = %s incomplete=%v broker reads %d→%d; the kept bars must serve it", second.State, second.Incomplete, before, len(reads))
	}
	if run := replayLossRun(t, second); run.Coverage.StatementDays != 3 || len(run.ExitsByReason) != 1 {
		t.Fatalf("loss-exit run = %+v", run)
	}
}

// A broker read that answers within the budget serves the same run.
func TestEdgeReplayUsesABrokerReadThatAnswersInTime(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	srv, _ := replayTestServer(t, now)
	srv.replayHistoryFetchForTest = func(context.Context, rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		return replayHistoryResult(t, now), nil
	}
	result, err := srv.handleEdgeReplay(t.Context(), &rpc.Request{Params: json.RawMessage(`{"rules":["loss_exit"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != rpc.EdgeStateCurrent || result.Incomplete {
		t.Fatalf("replay = %s %s missing=%+v", result.State, result.Reason, result.MissingHistory)
	}
	if run := replayLossRun(t, result); run.Coverage.StatementDays != 3 || len(run.ExitsByReason) != 1 {
		t.Fatalf("loss-exit run = %+v", run)
	}
}
