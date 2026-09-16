package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

const displayLimit = 128

type displayInstrument struct {
	contract  ibkr.Contract
	kind, key string
}
type displayHold struct {
	item             displayInstrument
	cacheKey         string
	release          func(context.Context)
	resolvedDay      string
	requiredContract rpc.ContractParams
}

func displayContract(c ibkr.Contract) rpc.ContractParams {
	return rpc.ContractParams{ConID: c.ConID, Symbol: c.Symbol, SecType: c.SecType, Exchange: c.Exchange, PrimaryExch: c.PrimaryExch, Currency: c.Currency, LocalSymbol: c.LocalSymbol, TradingClass: c.TradingClass, Expiry: c.Expiry, Strike: c.Strike, Right: c.Right, Multiplier: c.Multiplier}
}
func displayNumber(n float64) *float64 {
	if math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) >= 1e300 {
		return nil
	}
	return &n
}

func displayUniverse(snapshot ibkr.DisplaySnapshot) ([]displayInstrument, bool) {
	rows := append([]*ibkr.RawPosition(nil), snapshot.Positions...)
	slices.SortFunc(rows, func(a, b *ibkr.RawPosition) int { return a.Contract.ConID - b.Contract.ConID })
	items := []displayInstrument{}
	seen := map[string]bool{}
	add := func(c ibkr.Contract, kind, key string) {
		if c.Symbol == "" || c.Currency == "" {
			return
		}
		if c.Exchange == "" && (c.SecType == "STK" || c.SecType == "OPT") {
			c.Exchange = "SMART"
		}
		id := ibkr.MarketDataKeyForContract(c)
		if !seen[id] {
			items = append(items, displayInstrument{c, kind, key})
			seen[id] = true
		}
	}
	for _, p := range rows {
		if p.Position != 0 && p.Account == snapshot.Health.Account {
			add(p.Contract, "held", p.Contract.Symbol)
		}
	}
	for _, p := range rows {
		if p.Position == 0 || p.Account != snapshot.Health.Account || p.Contract.SecType != "OPT" {
			continue
		}
		// Reuse Canary's canonical underlying routes, including index options.
		if route, ok := rpc.UnderlyingMarketContract(rpc.PositionGroup{Underlying: p.Contract.Symbol, Options: []rpc.PositionView{{Symbol: p.Contract.Symbol, Currency: p.Contract.Currency}}}); ok {
			add(ibkr.Contract{Symbol: route.Symbol, SecType: route.SecType, Exchange: route.Exchange, PrimaryExch: route.PrimaryExch, Currency: route.Currency}, "underlying", p.Contract.Symbol)
		}
	}
	for _, item := range marketReferences() {
		kind := "benchmark"
		if item.Kind == "future" {
			kind = "future"
		}
		p := item.Quote.Contract
		add(ibkr.Contract{Symbol: p.Symbol, SecType: p.SecType, Currency: p.Currency, Exchange: p.Exchange}, kind, item.Key)
	}
	return items[:min(len(items), displayLimit)], len(items) > displayLimit
}

// One worker owns acquisition/release. Full replacement results never block it
// behind a slow writer; market callbacks and cache publication stay independent.
func (s *Server) displayHolds(ctx context.Context, c *ibkr.Connector, jobs <-chan []displayInstrument, results chan []displayHold, account string) {
	held := map[string]displayHold{}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, h := range held {
			h.release(cleanup)
		}
	}()
	publish := func() {
		out := make([]displayHold, 0, len(held))
		for _, h := range held {
			out = append(out, h)
		}
		select {
		case <-results:
		default:
		}
		results <- out
	}
reconcile:
	for {
		select {
		case <-ctx.Done():
			return
		case items := <-jobs:
			desired := map[string]bool{}
			for _, item := range items {
				desired[ibkr.MarketDataKeyForContract(item.contract)] = true
			}
			for key, h := range held {
				if !desired[key] {
					h.release(ctx)
					delete(held, key)
				}
			}
			publish()
			for _, item := range items {
				if len(jobs) > 0 {
					continue reconcile
				}
				if ctx.Err() != nil {
					return
				}
				key := ibkr.MarketDataKeyForContract(item.contract)
				if item.kind == "held" && item.contract.ConID > 0 && c.ActiveDailyPnLSubscriptions() < maxDailyPnLSubscriptions {
					subscribe, cancel := context.WithTimeout(ctx, 3*time.Second)
					_ = c.SubscribePositionDailyPnLContext(subscribe, account, item.contract.ConID)
					cancel()
				}
				if old, ok := held[key]; ok && item.kind == "future" && old.resolvedDay != time.Now().UTC().Format("20060102") {
					old.release(ctx)
					delete(held, key)
				}
				if _, ok := held[key]; ok {
					continue
				}
				acquire, cancel := context.WithTimeout(ctx, 3*time.Second)
				requiredContract := displayContract(item.contract)
				contract := item.contract
				var err error
				if contract.SecType == "FUT" && contract.Expiry == "" {
					contract, err = c.FrontFuture(acquire, contract, time.Now())
				}
				if err == nil {
					var cacheKey string
					var release func(context.Context)
					cacheKey, release, err = s.subs.HoldContract(acquire, contract)
					if err == nil {
						item.contract = contract
						held[key] = displayHold{item: item, cacheKey: cacheKey, release: release, requiredContract: requiredContract, resolvedDay: time.Now().UTC().Format("20060102")}
						publish()
					}
				}

				cancel()
			}
		}
	}
}

func projectDisplay(snapshot ibkr.DisplaySnapshot, holds []displayHold, scope rpc.AccountDataScope) rpc.DisplaySnapshot {
	out := rpc.DisplaySnapshot{Version: 1, Available: true, Scope: scope, Positions: []rpc.DisplayPosition{}, Quotes: []rpc.DisplayQuote{}, PositionsAt: snapshot.Health.LastUpdateAt}
	base := ""
	if a := snapshot.Account; a != nil && a.AccountID == scope.AccountID && a.BaseCurrencyProvenance.Proven() {
		base = a.BaseCurrency
		out.Account.Currency = base
		if !snapshot.NetLiquidationAt.IsZero() {
			out.Account.NetLiquidation = a.NetLiquidation
			out.Account.NetLiquidationAt = snapshot.NetLiquidationAt
		}
		pnl := snapshot.AccountPnL
		if snapshot.PnLAccount != scope.AccountID {
			pnl = ibkr.AccountDailyPnL{}
		}
		out.Account.DailyPnL = pnl.DailyPnL
		out.Account.UnrealizedPnL = pnl.UnrealizedTotalPnL
		out.Account.RealizedPnL = pnl.RealizedTotalPnL
		out.Account.PnLAt = pnl.AsOf
	}
	for _, p := range snapshot.Positions {
		if p.Account != scope.AccountID || p.Position == 0 {
			continue
		}
		row := rpc.DisplayPosition{UnrealizedAt: p.ValuationAt, ValuationAt: p.ValuationAt, Contract: displayContract(p.Contract), Quantity: p.Position, AverageCost: displayNumber(p.AverageCost), Mark: ptrIfPos(p.MarketPrice), MarketValue: displayNumber(p.MarketValue), UnrealizedPnL: displayNumber(p.UnrealizedPNL)}
		if base != "" && snapshot.PnLAccount == scope.AccountID && strings.EqualFold(p.Contract.Currency, base) {
			pnl := snapshot.PositionPnL[p.Contract.ConID]
			row.DailyPnL = pnl.DailyPnL
			if pnl.UnrealizedTotalPnL != nil {
				row.UnrealizedPnL = pnl.UnrealizedTotalPnL
				row.UnrealizedAt = pnl.AsOf
			}
			row.PnLAt = pnl.AsOf
		}
		out.Positions = append(out.Positions, row)
	}
	slices.SortFunc(out.Positions, func(a, b rpc.DisplayPosition) int { return a.Contract.ConID - b.Contract.ConID })
	for _, h := range holds {
		q := rpc.DisplayQuote{Contract: displayContract(h.item.contract), Kind: h.item.kind, Key: h.item.key, DataType: marketDataTypeName(snapshot.DataTypes[h.cacheKey])}
		if md := snapshot.Quotes[h.cacheKey]; md != nil {
			q.Last = ptrIfPos(md.Last)
			q.Bid = ptrIfPos(md.Bid)
			q.Ask = ptrIfPos(md.Ask)
			q.PreviousClose = ptrIfPos(md.Close)
			q.TradeAt = md.LastTradeTime
			q.TradePhase = quoteTradePhase(q.Contract, q.TradeAt)
			q.Price = q.Last
			q.PriceSource = "last"
			q.PriceReceivedAt = md.LastAt
			if q.Price == nil && md.MarkPrice > 0 {
				q.Price = ptrIfPos(md.MarkPrice)
				q.PriceSource = "mark"
				q.PriceReceivedAt = md.MarkAt
			}
			if q.Price == nil && md.Bid > 0 && md.Ask >= md.Bid {
				q.Price = ptrIfPos((md.Bid + md.Ask) / 2)
				q.PriceSource = "midpoint"
				q.PriceReceivedAt = md.BidAt
				if md.AskAt.Before(md.BidAt) {
					q.PriceReceivedAt = md.AskAt
				}
			}
			if q.Price == nil && md.Close > 0 {
				q.Price, q.PriceSource, q.PriceReceivedAt = ptrIfPos(md.Close), "prev_close", md.CloseAt
			}

			if md.VolumeObserved && md.Volume >= 0 {
				v := md.Volume
				q.Volume = &v
				q.VolumeAt = md.VolumeAt
			}
		}
		out.Quotes = append(out.Quotes, q)
	}
	slices.SortFunc(out.Quotes, func(a, b rpc.DisplayQuote) int {
		return strings.Compare(ibkrDisplayKey(a.Contract), ibkrDisplayKey(b.Contract))
	})
	return out
}
func ibkrDisplayKey(c rpc.ContractParams) string { raw, _ := json.Marshal(c); return string(raw) }

func (s *Server) handleDisplaySubscribe(parent context.Context, req *rpc.Request, conn net.Conn, r *bufio.Reader) {
	enc := json.NewEncoder(conn)
	defer recoverHandler(s.logger, enc, req)
	ctx, cancel := context.WithCancel(parent)
	run := &displayRun{cancel: cancel, done: make(chan struct{})}
	s.displayMu.Lock()
	if s.displayStopping || len(s.displayRuns) >= 4 {
		s.displayMu.Unlock()
		cancel()
		writeError(enc, req.ID, rpc.CodeBadRequest, "display reader limit or shutdown")
		return
	}
	if s.displayRuns == nil {
		s.displayRuns = map[*displayRun]struct{}{}
	}
	s.displayRuns[run] = struct{}{}
	s.displayMu.Unlock()
	defer func() { s.displayMu.Lock(); delete(s.displayRuns, run); close(run.done); s.displayMu.Unlock() }()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	eof := make(chan struct{})
	go func() { defer close(eof); _, _ = r.ReadByte(); cancel() }()
	defer func() { cancel(); stop(); _ = conn.Close(); <-eof }()
	c := s.gatewayConnector()
	if c == nil {
		writeError(enc, req.ID, rpc.CodeGatewayUnavailable, "gateway unavailable")
		return
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return
	}
	var random [16]byte
	_, _ = rand.Read(random[:])
	generation := hex.EncodeToString(random[:])
	jobs := make(chan []displayInstrument, 1)
	results := make(chan []displayHold, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				cancel()
			}
		}()
		s.displayHolds(ctx, c, jobs, results, scope.Account)
	}()
	defer func() { cancel(); <-done }()
	var holds []displayHold
	var previousUniverse string
	sequence := uint64(0)
	retry := time.NewTicker(30 * time.Second)
	defer retry.Stop()
	for ctx.Err() == nil {
		changed := c.DisplayChanges()
		snapshot, ready := c.CaptureDisplay()
		ready = ready && c.SessionCurrent(binding) && !c.BackendLink().Down && sameBrokerScope(scope, s.currentBrokerStateScope()) && snapshot.Health.Account == scope.Account && snapshot.Health.ScopeConflictAt.IsZero() && snapshot.Health.InvalidPayloadAt.IsZero() && !snapshot.Health.InitialCompletedAt.IsZero()
		if !ready {
			return
		}
		items, truncated := displayUniverse(snapshot)
		rawUniverse, _ := json.Marshal(itemsForComparison(items))
		if string(rawUniverse) != previousUniverse {
			select {
			case <-jobs:
			default:
			}
			jobs <- items
			previousUniverse = string(rawUniverse)
		}
		s.observeDisplayDataHealth(snapshot, holds, c)
		frame := projectDisplay(snapshot, holds, accountDataScope(scope))
		frame.Truncated = truncated || len(holds) < len(items)
		frame.Generation = generation
		sequence++
		frame.Sequence = sequence
		frame.EmittedAt = time.Now().UTC()
		raw, err := json.Marshal(frame)
		if err != nil || len(raw) > 512<<10 {
			return
		}
		if conn.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
			return
		}
		if enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, Frame: raw}) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case holds = <-results:
		case <-changed:
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case <-retry.C:
			previousUniverse = ""
		}
	}
}
func itemsForComparison(items []displayInstrument) []rpc.ContractParams {
	out := make([]rpc.ContractParams, 0, len(items))
	for _, i := range items {
		out = append(out, displayContract(i.contract))
	}
	return out
}

type displayRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Server) stopDisplay() {
	s.displayMu.Lock()
	s.displayStopping = true
	runs := make([]*displayRun, 0, len(s.displayRuns))
	for run := range s.displayRuns {
		run.cancel()
		runs = append(runs, run)
	}
	s.displayMu.Unlock()
	for _, run := range runs {
		<-run.done
	}
}
