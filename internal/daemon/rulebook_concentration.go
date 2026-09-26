package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Inputs for rule 1's issuer netting and its watches (amendment 15): each
// stock line's share count, mark and FX rate, its 20-day average daily volume
// from the daemon's caches, and the constitution's effective risk capital.

const (
	// rulebookLiquidityWarmBatch bounds one background warm: each symbol costs
	// one paced historical read of 45 daily bars.
	rulebookLiquidityWarmBatch = 8
	// rulebookLiquidityRetryAfter spaces warm attempts for one symbol; a
	// successful read is cached for four hours by the liquidity cache itself.
	rulebookLiquidityRetryAfter = 30 * time.Minute
	rulebookLiquidityReadBudget = 8 * time.Second
)

// rulebookStockLine is one symbol's held stock rows summed: the Rulebook nets
// every share of a symbol, while PositionGroup keeps only one stock pointer.
type rulebookStockLine struct {
	quantity float64
	mark     float64
	fx       *float64
	row      rpc.PositionView
}

// rulebookStockLines sums the stock rows of each symbol in base terms. A
// symbol whose rows carry different FX rates, or any row without one, has no
// FX rate: its value is unmeasured rather than guessed. The mark is the
// share-weighted mark, which values the summed shares exactly.
func rulebookStockLines(pos *rpc.PositionsResult, baseCcy string) map[string]rulebookStockLine {
	out := map[string]rulebookStockLine{}
	if pos == nil {
		return out
	}
	type agg struct {
		qty, value float64
		fx         *float64
		fxConflict bool
		row        rpc.PositionView
		priced     bool
	}
	sums := map[string]*agg{}
	for _, p := range pos.Stocks {
		if p.Quantity == 0 || !isRulebookStockSecurityType(p.SecType) {
			continue
		}
		sym := strings.ToUpper(strings.TrimSpace(p.Symbol))
		a := sums[sym]
		if a == nil {
			a = &agg{row: p, priced: true}
			sums[sym] = a
		}
		a.qty += p.Quantity
		if p.Mark <= 0 {
			a.priced = false
		}
		a.value += p.Quantity * p.Mark
		rate, ok := positionBaseRate(p, baseCcy)
		switch {
		case !ok:
			a.fxConflict = true
		case a.fx == nil:
			a.fx = new(rate)
		case math.Abs(*a.fx-rate) > 1e-9:
			a.fxConflict = true
		}
	}
	for sym, a := range sums {
		line := rulebookStockLine{quantity: a.qty, row: a.row}
		if a.priced && a.qty != 0 {
			line.mark = a.value / a.qty
		}
		if !a.fxConflict {
			line.fx = a.fx
		}
		out[sym] = line
	}
	return out
}

// rulebookLiquidityKey is the liquidity cache key the quote path uses for a
// symbol's stock, so a value either path cached serves both.
func rulebookLiquidityKey(symbol, exchange, currency string) (quoteLiquidityKey, rpc.ContractParams, bool) {
	contract := rpc.ContractParams{Symbol: normSym(symbol), SecType: "STK", Exchange: exchange, Currency: currency}
	_, echo, _, err := normaliseStockQuoteContract(contract)
	if err != nil {
		return quoteLiquidityKey{}, rpc.ContractParams{}, false
	}
	return quoteLiquidityCacheKey(&rpc.Quote{Symbol: echo.Symbol, Contract: echo}), echo, true
}

// completedDailyBars drops a bar for a US session still in progress: its
// volume is partial and would understate the average.
func completedDailyBars(bars []ibkrlib.HistoricalBar, now time.Time) []ibkrlib.HistoricalBar {
	if len(bars) == 0 {
		return bars
	}
	last := bars[len(bars)-1]
	if last.Time.IsZero() {
		return bars
	}
	session, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	if err != nil || session.Close.IsZero() {
		return bars
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return bars
	}
	if last.Time.In(loc).Format(time.DateOnly) == now.In(loc).Format(time.DateOnly) && now.Before(session.Close) {
		return bars[:len(bars)-1]
	}
	return bars
}

// rulebookADV20 reads a stock's 20-day average daily volume from the daemon's
// caches: the quote path's liquidity entry, else the daily bars it already
// holds. It never issues a broker read; a miss is unavailable, never zero.
func (s *Server) rulebookADV20(symbol, exchange, currency string, now time.Time) (float64, bool) {
	if s == nil {
		return 0, false
	}
	key, _, ok := rulebookLiquidityKey(symbol, exchange, currency)
	if !ok {
		return 0, false
	}
	if e, ok := s.quoteLiquidity.get(key, now); ok && e.status == "ok" && e.sampleDays >= 20 && e.avgVolume > 0 {
		return float64(e.avgVolume), true
	}
	if h, ok := s.quoteHistory.get(key, now); ok && h.err == nil {
		liq := computeHistoricalLiquidity20D(completedDailyBars(h.bars, now))
		if liq.sampleDays >= 20 && liq.avgVolume != nil && *liq.avgVolume > 0 {
			return float64(*liq.avgVolume), true
		}
	}
	return 0, false
}

// rulebookLiquidityTarget is one symbol whose volume the Rulebook needs.
type rulebookLiquidityTarget struct {
	symbol, exchange, currency string
}

// attachRulebookLiquidity sets each name's 20-day average volume from the
// caches and, when warm is set, starts one bounded background read for the
// names it could not find. Broad indices trade no shares and are skipped.
func (s *Server) attachRulebookLiquidity(ctx context.Context, names []risk.NameInput, pos *rpc.PositionsResult, now time.Time, warm bool) {
	if s == nil {
		return
	}
	stocks := map[string]rpc.PositionView{}
	options := map[string]rpc.PositionView{}
	if pos != nil {
		for _, p := range pos.Stocks {
			if sym := strings.ToUpper(strings.TrimSpace(p.Symbol)); sym != "" && isRulebookStockSecurityType(p.SecType) {
				if _, seen := stocks[sym]; !seen {
					stocks[sym] = p
				}
			}
		}
		for _, p := range pos.Options {
			if sym := strings.ToUpper(strings.TrimSpace(p.Symbol)); sym != "" {
				if _, seen := options[sym]; !seen {
					options[sym] = p
				}
			}
		}
	}
	var missing []rulebookLiquidityTarget
	for i := range names {
		sym := strings.ToUpper(strings.TrimSpace(names[i].Symbol))
		if secType, index := rulebookIndexUnderlyingSecurityType(sym); index && secType == rpc.SecTypeIndex {
			continue
		}
		target := rulebookLiquidityTarget{symbol: sym, currency: "USD"}
		if row, ok := stocks[sym]; ok {
			target.exchange, target.currency = row.Exchange, row.Currency
		} else if row, ok := options[sym]; ok {
			target.currency = row.Currency
		}
		if adv, ok := s.rulebookADV20(target.symbol, target.exchange, target.currency, now); ok {
			names[i].AvgDailyVolume = new(adv)
			continue
		}
		missing = append(missing, target)
	}
	if warm && len(missing) > 0 {
		s.kickRulebookLiquidityWarm(ctx, missing, now)
	}
}

// rulebookLiquidityWarmer serializes background volume reads and remembers
// when each symbol may be tried again.
type rulebookLiquidityWarmer struct {
	mu       sync.Mutex
	inflight bool
	retryAt  map[quoteLiquidityKey]time.Time
}

func (s *Server) liquidityWarmer() *rulebookLiquidityWarmer {
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	if s.rulebookLiquidity == nil {
		s.rulebookLiquidity = &rulebookLiquidityWarmer{retryAt: map[quoteLiquidityKey]time.Time{}}
	}
	return s.rulebookLiquidity
}

// kickRulebookLiquidityWarm reads 45 daily bars for up to a batch of symbols
// in the background and caches their 20-day volume for the next evaluation.
// It returns at once; a read in flight or a symbol inside its retry spacing is
// skipped.
func (s *Server) kickRulebookLiquidityWarm(ctx context.Context, targets []rulebookLiquidityTarget, now time.Time) {
	w := s.liquidityWarmer()
	type job struct {
		key   quoteLiquidityKey
		quote rpc.Quote
	}
	var jobs []job
	w.mu.Lock()
	if w.inflight {
		w.mu.Unlock()
		return
	}
	for _, t := range targets {
		key, echo, ok := rulebookLiquidityKey(t.symbol, t.exchange, t.currency)
		if !ok || now.Before(w.retryAt[key]) {
			continue
		}
		w.retryAt[key] = now.Add(rulebookLiquidityRetryAfter)
		jobs = append(jobs, job{key: key, quote: rpc.Quote{Symbol: echo.Symbol, Contract: echo, AsOf: now}})
		if len(jobs) == rulebookLiquidityWarmBatch {
			break
		}
	}
	if len(jobs) == 0 {
		w.mu.Unlock()
		return
	}
	w.inflight = true
	w.mu.Unlock()
	go func(ctx context.Context) {
		defer func() {
			w.mu.Lock()
			w.inflight = false
			w.mu.Unlock()
		}()
		c := s.gatewayConnector()
		if c == nil {
			return
		}
		for _, j := range jobs {
			if ctx.Err() != nil {
				return
			}
			q := j.quote
			bars, err := s.fetchQuoteHistoricalBars(ctx, c, &q, rulebookLiquidityReadBudget, 45)
			if err != nil {
				if s.logger != nil {
					s.logger.Debugf("rulebook volume %s: %v", q.Symbol, err)
				}
				continue
			}
			liq := computeHistoricalLiquidity20D(completedDailyBars(bars, time.Now()))
			if liq.sampleDays == 0 || liq.avgVolume == nil {
				continue
			}
			entry := quoteLiquidityEntry{status: "ok", source: "daily_bars", sampleDays: liq.sampleDays, asOf: liq.asOf, avgVolume: *liq.avgVolume}
			if liq.avgDollarVolume != nil {
				entry.avgDollarVolume = *liq.avgDollarVolume
			}
			if liq.sampleDays < 20 {
				entry.status = "partial"
			}
			s.quoteLiquidity.put(j.key, entry, time.Now())
		}
	}(context.WithoutCancel(ctx))
}

// rulebookRiskCapital resolves the constitution's effective risk capital for
// rule 18, min(declared risk capital, equity − protected floor), in the
// account's base currency. Every gap names the number or state that is
// missing, so the row can say "needs your number" precisely.
func (s *Server) rulebookRiskCapital(acct *rpc.AccountResult, acctErr error, baseCcy string, now time.Time) *risk.RiskCapitalInput {
	if s == nil || s.riskPolicies == nil || s.riskCapital == nil {
		return &risk.RiskCapitalInput{Missing: "the risk constitution in risk-policy.toml (none is loaded)"}
	}
	authority := s.acceptedRiskPolicy(now)
	return rulebookRiskCapitalFrom(authority.policy, baseCcy, func() *rpc.CapitalStateReport {
		res := s.policyResultForEvaluation(acct, acctErr, authority, now)
		if res == nil {
			return nil
		}
		return &res.Capital
	})
}

// rulebookRiskCapitalFrom is the pure half of rulebookRiskCapital: it names
// the first missing input and reads the capital report only when the
// constitution carries every number the effective risk capital needs.
func rulebookRiskCapitalFrom(c *risk.Constitution, baseCcy string, report func() *rpc.CapitalStateReport) *risk.RiskCapitalInput {
	if c == nil {
		return &risk.RiskCapitalInput{Missing: "capital.declared_risk_capital and capital.protected_floor in risk-policy.toml (no constitution is loaded)"}
	}
	var missing []string
	if c.Capital.DeclaredRiskCapital == nil {
		missing = append(missing, "capital.declared_risk_capital")
	}
	if c.Capital.ProtectedFloor == nil {
		missing = append(missing, "capital.protected_floor")
	}
	if len(missing) > 0 {
		return &risk.RiskCapitalInput{Missing: strings.Join(missing, " and ") + " in risk-policy.toml"}
	}
	if want, have := normCcy(c.Capital.BaseCurrency), normCcy(baseCcy); want != "" && have != "" && want != have {
		return &risk.RiskCapitalInput{Missing: fmt.Sprintf("risk capital in the account's base currency (the constitution declares %s, the account reports %s)", want, have)}
	}
	rep := report()
	if rep == nil || rep.EffectiveRiskCapitalBase == nil {
		return &risk.RiskCapitalInput{Missing: "a current equity observation to compute effective risk capital"}
	}
	return &risk.RiskCapitalInput{EffectiveBase: new(*rep.EffectiveRiskCapitalBase)}
}
