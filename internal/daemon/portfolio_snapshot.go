package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func currentPortfolioAuthority(a *rpc.AccountDataAuthority) bool {
	return a != nil && a.Availability == rpc.AccountDataAvailable && a.Freshness == rpc.AccountDataFreshnessCurrent && a.Scope.AccountID != "" && a.Scope.AccountMode != ""
}
func (s *Server) handlePortfolioSnapshot(ctx context.Context) (*rpc.PortfolioSnapshotResult, error) {
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return nil, s.gatewayUnavailableError()
	}
	p, err := s.handlePositionsList(ctx, &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		return nil, err
	}
	a, err := s.handleAccountSummary(ctx)
	if err != nil {
		return nil, err
	}
	if !currentPortfolioAuthority(p.Authority) || !currentPortfolioAuthority(a.Authority) || p.Authority.Scope != a.Authority.Scope || a.BaseCurrency == "" || p.Portfolio == nil || p.Portfolio.BaseCurrency != a.BaseCurrency {
		return nil, errors.New("current consistent portfolio scope unavailable")
	}
	broker := map[string]ibkrlib.MarketClassification{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	lookups := 0
	for _, g := range p.ByUnderlying {
		// Free sources settle S&P names and the embedded funds; the broker
		// round-trip is spent only where they are silent, and bounded.
		if classificationSettledWithoutBroker(g.Underlying) {
			continue
		}
		if lookups >= 12 {
			break
		}
		if !portfolioClassificationUnambiguous(g.Underlying, p) {
			continue
		}
		contract, ok := rpc.UnderlyingMarketContract(g)
		if !ok || !rpc.ExpectsMarketDataGroup(g) {
			continue
		}
		lookups++
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			route, _, _, e := normaliseStockQuoteContract(contract)
			if e != nil {
				return
			}
			mc, e := c.MarketClassification(ctx, route, 3*time.Second)
			if e != nil {
				return
			}
			mu.Lock()
			broker[strings.ToUpper(g.Underlying)] = mc
			mu.Unlock()
		})
	}
	wg.Wait()
	if !c.SessionCurrent(binding) {
		return nil, errors.New("broker session changed during portfolio observation")
	}
	// Account selection is distinct from connector lifetime.
	if !sameBrokerScope(brokerStateScope{Account: a.Authority.Scope.AccountID, Mode: a.Authority.Scope.AccountMode}, s.currentBrokerStateScope()) {
		return nil, errors.New("portfolio scope changed")
	}
	classes := map[string]underlyingClassification{}
	for _, g := range p.ByUnderlying {
		key := strings.ToUpper(g.Underlying)
		classes[key] = classifyUnderlying(key, broker[key])
	}
	return projectPortfolio(a, p, classes), nil
}

// projectPortfolio builds two tables from one book. Asset classes carry
// signed market value, the balance-sheet view where a long put is an asset.
// Sectors carry delta-adjusted notional, the exposure view where that same
// put is short the market. A holding the broker no longer quotes has no
// exposure and is counted out rather than drawn as a zero row.
func projectPortfolio(a *rpc.AccountResult, p *rpc.PositionsResult, classes map[string]underlyingClassification) *rpc.PortfolioSnapshotResult {
	r := &rpc.PortfolioSnapshotResult{
		AsOf: time.Now(), AccountAsOf: a.AsOf, PositionsAsOf: p.AsOf, Authority: p.Authority, BaseCurrency: a.BaseCurrency,
		AssetClassMeasure: rpc.AllocationMeasureMarketValue,
		SectorMeasure:     rpc.AllocationMeasureDeltaNotional,
		SectorBasis:       "GICS sector · delta-adjusted notional / NLV. Stocks at market value; options at delta × contracts × multiplier × underlying; index funds spread over published sector weights. S&P 500 names take Wikipedia's GICS sector, other stocks map from the broker's industry.",
		CoverageStatus:    "complete",
	}
	valid := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if a.Authority.Fields != nil && a.Authority.Fields.NetLiquidation && valid(a.NetLiquidation) && a.NetLiquidation > 0 {
		v := a.NetLiquidation
		r.NetLiquidation = &v
	}
	assetRows := map[string]*rpc.PortfolioAllocation{}
	sectorRows := map[string]*rpc.PortfolioAllocation{}
	add := func(rows map[string]*rpc.PortfolioAllocation, name string, v *float64) {
		x := rows[name]
		if x == nil {
			x = &rpc.PortfolioAllocation{Name: name}
			rows[name] = x
		}
		if v == nil || !valid(*v) {
			x.Missing++
			r.CoverageStatus = "partial"
			return
		}
		x.Observed++
		if x.ValueBase == nil {
			z := 0.0
			x.ValueBase = &z
		}
		*x.ValueBase += *v
	}
	var cash *float64
	if a.Authority.Fields != nil && a.Authority.Fields.TotalCash && valid(a.TotalCash) {
		v := a.TotalCash
		cash = &v
	}
	add(assetRows, "Cash", cash)
	lookThrough := map[string]rpc.PortfolioLookThrough{}
	cost := 0.0
	visit := func(row rpc.PositionView, isOption bool) {
		if !isOption && row.QuoteExpectation == rpc.QuoteExpectationNone {
			r.DefunctExcluded++
			return
		}
		cls := classes[strings.ToUpper(row.Symbol)]
		class := "Other"
		switch {
		case isOption:
			class = "Options"
		case strings.EqualFold(row.SecType, "STK"), strings.EqualFold(row.SecType, "STOCK"):
			class = "Stocks"
			if cls.Fund {
				class = "Funds"
			}
		}
		stale := row.Stale && rpc.ExpectsMarketData(row)
		value := row.MarketValueBase
		if stale {
			value = nil
		}
		add(assetRows, class, value)

		rate, rateOK := positionBaseRate(row, a.BaseCurrency)
		var delta *float64
		if local, ok := positionDollarDelta(row, isOption); ok && rateOK && valid(rate) && !stale {
			v := local * rate
			delta = &v
		}
		switch {
		case cls.LookThrough != nil:
			lookThrough[strings.ToUpper(row.Symbol)] = rpc.PortfolioLookThrough{Symbol: strings.ToUpper(row.Symbol), AsOf: cls.LookThrough.AsOf, Source: cls.LookThrough.Source}
			for sector, weight := range cls.LookThrough.Weights {
				var part *float64
				if delta != nil {
					v := *delta * weight / 100
					part = &v
				}
				add(sectorRows, sector, part)
			}
		case cls.Fund:
			add(sectorRows, sectorFunds, delta)
		case cls.Sector != "":
			add(sectorRows, cls.Sector, delta)
		default:
			add(sectorRows, sectorUnclassified, delta)
			r.CoverageStatus = "partial"
		}

		// IBKR average option cost already includes its contract multiplier.
		if class == "Other" || !rateOK || !valid(rate) || !valid(row.AvgCost) || row.AvgCost <= 0 || !valid(row.Quantity) {
			r.CostBasisMissing++
			return
		}
		v := row.Quantity * row.AvgCost * rate
		if !valid(v) {
			r.CostBasisMissing++
			return
		}
		cost += v
		r.CostBasisObserved++
	}
	for _, row := range p.Stocks {
		visit(row, false)
	}
	for _, row := range p.Options {
		visit(row, true)
	}
	finish := func(rows map[string]*rpc.PortfolioAllocation) []rpc.PortfolioAllocation {
		out := []rpc.PortfolioAllocation{}
		for _, x := range rows {
			if x.Missing == 0 && x.ValueBase != nil && r.NetLiquidation != nil {
				v := *x.ValueBase / *r.NetLiquidation * 100
				x.PercentNLV = &v
			}
			out = append(out, *x)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	r.AssetClasses = finish(assetRows)
	r.Sectors = finish(sectorRows)
	for _, symbol := range slices.Sorted(maps.Keys(lookThrough)) {
		r.LookThrough = append(r.LookThrough, lookThrough[symbol])
	}
	if r.NetLiquidation == nil || r.CostBasisMissing > 0 {
		r.CoverageStatus = "partial"
	}
	if r.CostBasisMissing == 0 && valid(cost) {
		r.CostBasisBase = &cost
	}
	if !spx.SectorsAsOf().IsZero() {
		r.SectorBasis += " S&P sectors as of " + spx.SectorsAsOf().Format("2006-01-02") + "."
	}
	return r
}

// Do not spread one exact listing's classification across ambiguous inventory.
func portfolioClassificationUnambiguous(symbol string, p *rpc.PositionsResult) bool {
	stockIDs := map[int]bool{}
	currencies := map[string]bool{}
	for _, row := range append(append([]rpc.PositionView{}, p.Stocks...), p.Options...) {
		if !strings.EqualFold(row.Symbol, symbol) {
			continue
		}
		if row.Currency == "" {
			return false
		}
		currencies[row.Currency] = true
		if row.SecType == "STOCK" || row.SecType == "STK" {
			if row.ConID <= 0 {
				return false
			}
			stockIDs[row.ConID] = true
		}
	}
	return len(currencies) == 1 && len(stockIDs) <= 1
}
