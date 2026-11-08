package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/chainrec"
	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

// replayHistoryLeadDays is how far before the window the underlying history
// must reach: the stock trail's 20-session average true range needs about a
// month of sessions before the first day.
const replayHistoryLeadDays = 45

// handleEdgeReplay replays long-option exit rules over the owner's own option
// trades (long-option-exits LO-V1). It reads only what Canary retains: the
// Flex statement projection Edge reads, the retained daily history of each
// underlying (replay_history.go), the recorded option chains and the
// measured fill costs. The
// rule numbers come from the Rulebook and protection policies in force. It
// is read-only and never reaches the order path.
func (s *Server) handleEdgeReplay(ctx context.Context, req *rpc.Request) (*rpc.EdgeReplayResult, error) {
	var params rpc.EdgeReplayParams
	if err := decodeFor(api.EdgeReplay, req.Params, &params); err != nil {
		return nil, err
	}
	params, err := rpc.NormalizeEdgeReplayParams(params)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	stateOnly := func(state, reason string) *rpc.EdgeReplayResult {
		return &rpc.EdgeReplayResult{SchemaVersion: rpc.EdgeReplaySchemaVersion, State: state, Reason: reason, NotExecution: true,
			Runs: []rpc.EdgeReplayRun{}, Scope: rpc.EdgeReplayScope{Underlyings: []string{}, ExcludedOther: map[string]int{}},
			Reconciliation: rpc.EdgeReplayReconciliation{ReconciliationNotes: []string{}, NotCompared: []rpc.EdgeReplayReconContract{}, ResidualContracts: []rpc.EdgeReplayReconContract{}}}
	}
	if s.cfg == nil || !s.cfg.Flex.Enabled || strings.TrimSpace(s.cfg.Flex.QueryID) == "" {
		return stateOnly(rpc.EdgeStateActionRequired, "flex_configuration_required"), nil
	}
	if s.coreStore == nil {
		return nil, fmt.Errorf("edge authority is unavailable")
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return stateOnly(rpc.EdgeStateUnavailable, "account_scope_unavailable"), nil
	}
	loadCtx, cancel := context.WithTimeout(ctx, edgeStatementLoadTimeout)
	projected, err := s.loadEdgeProjectionEvidence(loadCtx, scope, edgeScopeFingerprint(scope))
	cancel()
	if err != nil {
		return nil, err
	}
	statements := projected.Statements
	if len(statements) == 0 {
		return stateOnly(rpc.EdgeStateBackfilling, "statement_account_pending"), nil
	}
	var from, to time.Time
	if params.From != "" {
		from, _ = time.Parse(time.DateOnly, params.From)
	}
	if params.To != "" {
		to, _ = time.Parse(time.DateOnly, params.To)
	}

	policy, _ := s.protectionPolicies.Active()
	rulebook, rulebookStatus := s.activeRulebookPolicy()
	options := policy.Buckets.TrailingStop.Options
	replayPolicy := edgecore.ExitReplayPolicy{
		Exit: risk.OptionExitPolicy{
			MinDTE: options.MinDTE, LossExitPct: rulebook.ExitActLossPct, ProfitArmGainPct: options.ProfitArmGainPct,
			ProfitTrailPct: options.DefaultPct, LockedGainPct: options.LockedGainPct, MinTrailPct: options.MinPct, MaxTrailPct: options.MaxPct,
			MaxSpreadPctOfMid: options.MaxSpreadPctOfMid, MinTrailAbs: options.MinTrailAbs, SpreadMultiple: options.SpreadMultiple,
			ExpiryCloseDTE: rulebook.RunwayActDTE,
		},
		RecoverCostGainPct: options.RecoverCostGainPct, OptionNameActPct: rulebook.OptionNameActPct,
		StockTrailRangeMultiple: options.StockTrailRangeMultiple, HedgeSymbols: slices.Clone(rulebook.HedgeSymbols),
		Source: fmt.Sprintf("Rulebook %s v%d (%s) · protection %s v%d", rulebookStatus.PolicyID, rulebookStatus.PolicyVersion, rulebookStatus.Source, policy.PolicyID, policy.PolicyVersion),
	}

	underlyings := edgecore.ExitReplayUnderlyings(statements, from, to, replayPolicy.HedgeSymbols)
	historyRange := replayHistoryRange(statements, from, s.edgeNow())
	bars := map[string][]edgecore.UnderlyingBar{}
	notes := map[string]edgecore.ExitReplayHistoryNote{}
	symbols := make([]string, 0, len(underlyings))
	// Broker reads start together and get a bounded share of the request's
	// budget; a name whose read is still running when it ends is left out
	// as still fetching, and its read goes on to keep the bars.
	reads := make([]*replayRead, 0, len(underlyings))
	for _, underlying := range underlyings {
		symbols = append(symbols, underlying.Symbol)
		reads = append(reads, s.replayHistoryRetained(ctx, underlying, historyRange))
	}
	wait, cancelWait := context.WithTimeout(ctx, replayHistoryBudget(ctx))
	for _, read := range reads {
		series, note := s.replayHistoryFinish(ctx, wait, read)
		if ctx.Err() != nil {
			cancelWait()
			return nil, ctx.Err()
		}
		// A history that does not span the name's days leaves it out of
		// every run; the note says why.
		bars[read.underlying.Symbol] = series
		if note.Reason != "" {
			notes[read.underlying.Symbol] = note
		}
	}
	cancelWait()
	closed := replayClosedSessions(underlyings)
	chainMarks := s.replayChainMarks(ctx, symbols, from, to)
	halfSpread, samples, source := s.replayHalfSpread()

	result := edgecore.ExitReplay(edgecore.ExitReplayInput{
		Statements: statements, BaseCurrency: inferEdgeBaseCurrency(statements), From: from, To: to, Rules: params.Rules,
		Policy: replayPolicy, Underlyings: bars, HistoryNotes: notes, HistorySource: replayHistorySource, ClosedSessions: closed, ChainMarks: chainMarks,
		HalfSpreadPct: halfSpread, HalfSpreadSamples: samples, HalfSpreadSource: source,
	})
	if !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return stateOnly(rpc.EdgeStateUnavailable, "account_scope_changed"), nil
	}
	if err := rpc.ValidateEdgeReplayResult(result); err != nil {
		return nil, fmt.Errorf("invalid Edge replay: %w", err)
	}
	return &result, nil
}

// replayHistoryRange picks the shortest daily-history range that reaches
// replayHistoryLeadDays before the window's first day, so a range already
// cached for the market view is reused.
func replayHistoryRange(statements []flexstmt.Statement, from, now time.Time) string {
	first := from
	if first.IsZero() {
		for _, st := range statements {
			if !st.FromDate.IsZero() && (first.IsZero() || st.FromDate.Before(first)) {
				first = st.FromDate
			}
		}
	}
	need := first.AddDate(0, 0, -replayHistoryLeadDays)
	for _, label := range []string{"6M", "1Y", "5Y"} {
		days, _, err := marketHistoryWindow(label, now)
		if err == nil && !now.AddDate(0, 0, -days).After(need) {
			return label
		}
	}
	return "5Y"
}

// replayClosedSessions lists, for each underlying, the days from its first
// to its last needed day on which its exchange held no session (a holiday or
// a weekend) by Canary's official exchange calendar (internal/marketcal, the
// calendar behind options_session and the market sessions). A name whose
// exchange the trade currency does not identify, or a day outside the
// calendar's verified coverage, lists nothing: such a day without a bar
// stays untestable.
func replayClosedSessions(underlyings []edgecore.ExitReplayUnderlying) map[string][]time.Time {
	const span = 366
	cal := marketcal.New()
	out := map[string][]time.Time{}
	for _, u := range underlyings {
		market, ok := replayUnderlyingMarket(u.Currency)
		if !ok || u.First.IsZero() || u.Last.Before(u.First) {
			continue
		}
		first, last := u.First.Format(time.DateOnly), u.Last.Format(time.DateOnly)
		for day := u.First; !day.After(u.Last); day = day.AddDate(0, 0, span) {
			res, err := cal.Query(marketcal.Query{Market: market, Date: day.Format(time.DateOnly), Days: span})
			if err != nil {
				break
			}
			for _, session := range res.Sessions {
				if session.Date < first || session.Date > last || session.State != marketcal.StateHoliday && session.State != marketcal.StateClosed {
					continue
				}
				if at, err := time.Parse(time.DateOnly, session.Date); err == nil {
					out[u.Symbol] = append(out[u.Symbol], at)
				}
			}
		}
	}
	return out
}

// replayUnderlyingMarket is the exchange calendar of an underlying traded in
// a currency that names one: the US for dollars (stocks, ETFs and the Cboe
// indexes keep the same holidays), London, Tokyo and Hong Kong. The euro
// spans exchanges with different holidays and names none.
func replayUnderlyingMarket(currency string) (marketcal.Market, bool) {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "", "USD":
		return marketcal.MarketUSEquity, true
	case "GBP":
		return marketcal.MarketUKLSE, true
	case "JPY":
		return marketcal.MarketJPTSE, true
	case "HKD":
		return marketcal.MarketHKHKEX, true
	}
	return "", false
}

// replayChainMarks reads the recorded two-sided quotes of the underlyings in
// the window. A recorder that never ran supplies none.
func (s *Server) replayChainMarks(ctx context.Context, symbols []string, from, to time.Time) []edgecore.RecordedOptionMark {
	store, err := s.chainRecorderStoreHandle(ctx, false)
	if err != nil || store == nil || len(symbols) == 0 {
		return nil
	}
	first, last := "0000-01-01", "9999-12-31"
	if !from.IsZero() {
		first = from.AddDate(0, 0, -10).Format(time.DateOnly)
	}
	if !to.IsZero() {
		last = to.Format(time.DateOnly)
	}
	quotes, err := store.TwoSidedQuotes(ctx, first, last, symbols)
	if err != nil {
		return nil
	}
	return replayMarksFromQuotes(quotes)
}

func replayMarksFromQuotes(quotes []chainrec.TwoSidedQuote) []edgecore.RecordedOptionMark {
	out := make([]edgecore.RecordedOptionMark, 0, len(quotes))
	for _, q := range quotes {
		day, err := time.Parse(time.DateOnly, q.SessionDate)
		if err != nil {
			continue
		}
		out = append(out, edgecore.RecordedOptionMark{ConID: q.ConID, Underlying: q.Symbol, Expiry: q.Expiry, Strike: q.Strike, Right: q.Right, Day: day, Bid: q.Bid, Ask: q.Ask})
	}
	return out
}

// replayHalfSpread is the median fill-vs-mid half-spread of the retained
// measured option fills (the same records canary reporting costs reads), in
// percent of the fill price.
func (s *Server) replayHalfSpread() (*float64, int, string) {
	const none = "retained execution costs hold no measured option fill; rule exits sell at the mark"
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.fillQuoteAccountStatus(ep)
	if s.fillQuotes == nil || !brokerScopeAccountConcrete(status.Account) {
		return nil, 0, none
	}
	var pcts []float64
	for _, record := range s.fillQuotes.list(status.Account, status.Mode) {
		fill := costFillFromRecord(record)
		if fill.SecType != "OPT" || fill.Status != rpc.CostFillMeasured || fill.HalfSpread == nil || fill.Price <= 0 || fill.Quantity <= 0 || fill.Multiplier <= 0 {
			continue
		}
		pcts = append(pcts, *fill.HalfSpread/(fill.Price*fill.Quantity*fill.Multiplier)*100)
	}
	if len(pcts) == 0 {
		return nil, 0, none
	}
	slices.Sort(pcts)
	median := pcts[len(pcts)/2]
	if len(pcts)%2 == 0 {
		median = (pcts[len(pcts)/2-1] + pcts[len(pcts)/2]) / 2
	}
	return &median, len(pcts), fmt.Sprintf("median fill-vs-mid half-spread of %d measured option fills in the retained execution costs (canary reporting costs)", len(pcts))
}
