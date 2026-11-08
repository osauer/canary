package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/chainrec"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/register"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The option-chain recorder (internal-docs/design/option-chain-recorder.md)
// is off by default. While off it opens no file and sends nothing; the loop
// below only reads the setting.
const (
	chainRecorderTick = 30 * time.Second
	// chainRecorderGatewayGrace is how long a due run waits for the gateway
	// before it starts anyway and records the outage as its coverage.
	chainRecorderGatewayGrace = 5 * time.Minute
)

// chainRecorderRuntime owns the recorder's store handle and run state.
type chainRecorderRuntime struct {
	mu      sync.Mutex
	store   *chainrec.Store
	running atomic.Bool
	active  atomic.Int64
	// openErr is the last store-open failure, logged once (guarded by mu).
	openErr string
}

// chainRecorderSettingsFrom is the recorder's settings: on unless the owner
// switched it off (strategy-register.md, section 4: collection needs no
// switch; the owner keeps the off switch), with the research list.
func chainRecorderSettingsFrom(data platformSettingsData) chainrec.Settings {
	out := chainrec.Settings{Enabled: true, Symbols: slices.Clone(chainrec.DefaultSymbols)}
	if v := data.Chains.Recorder.Enabled; v != nil {
		out.Enabled = *v
	}
	if v := data.Chains.Recorder.Symbols; v != nil {
		out.Symbols = slices.Clone(*v)
	}
	return out
}

func chainRecorderPlatformSettings(data platformSettingsData) rpc.PlatformChainsSettings {
	settings := chainRecorderSettingsFrom(data)
	symbolsReason := "research names, recorded after held option underlyings and registered strategies' declared chains"
	if data.Chains.Recorder.Symbols == nil {
		symbolsReason = "no research names (the default); held option underlyings and registered strategies' declared chains are recorded"
	}
	return rpc.PlatformChainsSettings{Recorder: rpc.ChainRecorderSettings{
		Enabled: settingsBool(settings.Enabled, rpc.SettingsAccessWrite, rpc.SettingsSourceRuntime,
			"daily option-chain snapshot 30 minutes before the US close into option-chains.db of what holdings and registered strategies declare; unset records, false is the owner's off switch"),
		Symbols: rpc.SettingsStringList{Value: settings.Symbols, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime, Reason: symbolsReason},
	}}
}

func (s *Server) chainRecorderStorePath() string {
	if s == nil || s.coreStorePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.coreStorePath), chainrec.StoreFile)
}

// chainRecorderStoreHandle returns the open store. With create false it opens
// only an existing file, so reading an unused recorder creates nothing.
func (s *Server) chainRecorderStoreHandle(ctx context.Context, create bool) (*chainrec.Store, error) {
	rt := &s.chainRecorder
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.store != nil {
		return rt.store, nil
	}
	path := s.chainRecorderStorePath()
	if path == "" {
		return nil, errors.New("option-chain store has no state directory")
	}
	var store *chainrec.Store
	var err error
	if create {
		store, err = chainrec.Open(ctx, path)
	} else {
		store, err = chainrec.OpenExisting(ctx, path)
	}
	if err != nil || store == nil {
		return nil, err
	}
	rt.store = store
	return store, nil
}

func (s *Server) closeChainRecorderStore() {
	rt := &s.chainRecorder
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.store != nil {
		_ = rt.store.Close()
		rt.store = nil
	}
}

// startChainRecorder runs the schedule loop. Each tick reads the setting;
// nothing else happens while the recorder is off.
func (s *Server) startChainRecorder(ctx context.Context) {
	go func() {
		defer s.closeChainRecorderStore()
		ticker := time.NewTicker(chainRecorderTick)
		defer ticker.Stop()
		for {
			s.maybeRunChainRecorder(ctx, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// maybeRunChainRecorder starts the session's run when the recorder is on,
// the slot is due and the session has no run yet. A session is recorded at
// most once; a run cut short is never retried.
func (s *Server) maybeRunChainRecorder(ctx context.Context, now time.Time) bool {
	settings := chainRecorderSettingsFrom(s.platformSettings.snapshot())
	if !settings.Enabled {
		return false
	}
	slot, due := chainrec.DueSlot(marketcal.New(), now)
	if !due {
		return false
	}
	settings.Declared = s.registerChainDeclarations()
	if len(settings.Declared) == 0 && len(settings.Symbols) == 0 {
		// Nothing is declared but what holdings hold: with no held option
		// underlying there is nothing to record, and no store is opened.
		if held, err := (&chainRecorderBroker{s: s}).HeldUnderlyings(ctx); err != nil || len(held) == 0 {
			return false
		}
	}
	if c := s.gatewayConnector(); (c == nil || !c.IsReady()) && now.Before(slot.ScheduledAt.Add(chainRecorderGatewayGrace)) {
		return false
	}
	store, err := s.chainRecorderStoreHandle(ctx, true)
	if err != nil {
		rt := &s.chainRecorder
		rt.mu.Lock()
		repeat := rt.openErr == err.Error()
		rt.openErr = err.Error()
		rt.mu.Unlock()
		if !repeat {
			s.logger.Warnf("chain recorder: store unavailable: %v", err)
		}
		return false
	}
	if has, err := store.HasRun(ctx, slot.SessionDate); err != nil || has {
		return false
	}
	if !s.chainRecorder.running.CompareAndSwap(false, true) {
		return false
	}
	defer s.chainRecorder.running.Store(false)
	defer s.chainRecorder.active.Store(0)
	runner := &chainrec.Runner{
		Broker: &chainRecorderBroker{s: s},
		Sink:   &chainRecorderSink{Store: store, active: &s.chainRecorder.active},
		Days:   chainrec.NewUSEquityDays(marketcal.New()),
		Logf:   s.logger.Warnf,
	}
	s.logger.Infof("chain recorder: session %s run starting (%d explicit symbols)", slot.SessionDate, len(settings.Symbols))
	out, err := runner.Run(ctx, slot, settings)
	if err != nil {
		s.logger.Warnf("chain recorder: session %s: %v", slot.SessionDate, err)
		return true
	}
	s.logger.Infof("chain recorder: session %s %s: %d of %d contracts quoted, %d attempted (stop: %s)",
		slot.SessionDate, out.Status, out.Quoted, out.Planned, out.Attempted, nonEmptyString(out.StopReason, "none"))
	return true
}

// chainRecorderSink records the active run id so reads can tell a running
// run from an interrupted one.
type chainRecorderSink struct {
	*chainrec.Store
	active *atomic.Int64
}

func (k *chainRecorderSink) BeginRun(ctx context.Context, start chainrec.RunStart) (int64, error) {
	id, err := k.Store.BeginRun(ctx, start)
	if err == nil {
		k.active.Store(id)
	}
	return id, err
}

// chainRecorderBroker is the recorder's read-only gateway adapter. Option
// lines, the spot read and the bulk contract resolution ride the background
// pacing lane; nothing here can reach an order path.
type chainRecorderBroker struct{ s *Server }

func (b *chainRecorderBroker) conn() (*ibkrlib.Connector, error) {
	c := b.s.gatewayConnector()
	if c == nil || !c.IsReady() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	return c, nil
}

func (b *chainRecorderBroker) HeldUnderlyings(context.Context) ([]string, error) {
	c, err := b.conn()
	if err != nil {
		return nil, err
	}
	positions, err := c.CachedPositions()
	if err != nil {
		return nil, err
	}
	return heldOptionUnderlyings(positions), nil
}

// heldOptionUnderlyings lists the underlyings of open USD option positions.
func heldOptionUnderlyings(positions []*ibkrlib.RawPosition) []string {
	var out []string
	for _, p := range positions {
		if p == nil || p.Position == 0 || !strings.EqualFold(p.Contract.SecType, "OPT") {
			continue
		}
		if ccy := strings.TrimSpace(p.Contract.Currency); ccy != "" && !strings.EqualFold(ccy, "USD") {
			continue
		}
		if sym := normSym(p.Contract.Symbol); sym != "" && !slices.Contains(out, sym) {
			out = append(out, sym)
		}
	}
	slices.Sort(out)
	return out
}

func (b *chainRecorderBroker) ListedExpiries(_ context.Context, symbol string) ([]chainrec.ListedExpiry, error) {
	c, err := b.conn()
	if err != nil {
		return nil, err
	}
	classed, err := c.FetchOptionExpiryStrikesClassed(symbol, gammaExpiriesFetchTimeout)
	if err != nil {
		return nil, err
	}
	var out []chainrec.ListedExpiry
	for date, entries := range classed {
		for _, e := range normalisedSPXChainEntries(entries, symbol) {
			out = append(out, chainrec.ListedExpiry{Date: date, TradingClass: e.TradingClass, Strikes: e.Strikes})
		}
	}
	return out, nil
}

func (b *chainRecorderBroker) Spot(ctx context.Context, symbol string) (chainrec.Spot, error) {
	c, err := b.conn()
	if err != nil {
		return chainrec.Spot{}, err
	}
	spot := b.s.chainSpotForATM(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), c, symbol, 5*time.Second)
	if spot.Price <= 0 {
		return chainrec.Spot{}, fmt.Errorf("no %s price", symbol)
	}
	return chainrec.Spot{Price: spot.Price, Source: spot.Source, DataType: spot.DataType, AsOf: spot.AsOf}, nil
}

func (b *chainRecorderBroker) Prepare(ctx context.Context, symbol, tradingClass string, expiries []string) chainrec.ListedFilter {
	c, err := b.conn()
	if err != nil {
		return nil
	}
	ymds := make([]string, 0, len(expiries))
	for _, e := range expiries {
		ymds = append(ymds, compactExpiry(e))
	}
	// The gamma fan-out's rule: a complete resolution, or a failure that
	// rules out resolving contracts one by one, proves listing from the
	// cache; any other failure leaves listing unknown and the strike kept.
	proven := map[string]bool{}
	for _, r := range c.PrewarmOptionChain(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), symbol, ymds, tradingClass, 30*time.Second) {
		proven[r.Expiry] = (r.Err == nil && r.Dropped == 0) || r.Dropped > 0 || gammaPrewarmFailureBlocksFallback(r.Err)
	}
	return func(expiry string, strike float64, right string) (bool, bool) {
		ymd := compactExpiry(expiry)
		if !proven[ymd] {
			return false, false
		}
		return c.IsOptionContractCached(symbol, tradingClass, ymd, strike, right), true
	}
}

func (b *chainRecorderBroker) Load() (chainrec.Load, bool) {
	c := b.s.gatewayConnector()
	if c == nil {
		return chainrec.Load{}, false
	}
	load, ok := c.MarketDataLoad()
	return chainrec.Load{LinesHeld: load.LinesHeld, LinesCapacity: load.LinesCapacity, LastPacingError: load.LastPacingError}, ok
}

// Quote opens one option line on the background lane, collects until a
// two-sided quote, a model tick and open interest have all arrived or the
// window ends, and releases the line.
func (b *chainRecorderBroker) Quote(ctx context.Context, k chainrec.Contract, window time.Duration) (chainrec.Quote, error) {
	c, err := b.conn()
	if err != nil {
		return chainrec.Quote{}, chainrec.Fail(chainrec.StatusUnavailable, err)
	}
	if gammaBackendLinkDown(c) {
		return chainrec.Quote{}, chainrec.Fail(chainrec.StatusUnavailable, errors.New("TWS lost its IBKR backend link"))
	}
	ctx = ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground)
	q := chainrec.Quote{RequestedAt: time.Now()}
	key, release, err := c.HoldOption(ctx, k.Symbol, k.TradingClass, k.ExpiryYMD(), k.Strike, k.Right)
	if err != nil {
		if ctx.Err() != nil {
			return chainrec.Quote{}, ctx.Err()
		}
		return chainrec.Quote{}, chainrec.Fail(chainRecorderFailureStatus(err), err)
	}
	defer func() { _ = release() }()
	err = pollUntilWithReject(ctx, q.RequestedAt.Add(window), c.SubscriptionRejectCh(key), key, func() bool {
		readChainRecorderQuote(c, key, &q)
		return q.Bid != nil && q.Ask != nil && q.IV != nil && q.OpenInterest != nil
	})
	q.ObservedAt = time.Now()
	switch {
	case ctx.Err() != nil:
		return q, ctx.Err()
	case err == nil || errors.Is(err, context.DeadlineExceeded):
		return q, nil
	default:
		return chainrec.Quote{}, chainrec.Fail(chainRecorderFailureStatus(err), err)
	}
}

func readChainRecorderQuote(c *ibkrlib.Connector, key string, q *chainrec.Quote) {
	if md, ok := c.MarketDataSnapshot()[key]; ok && md != nil {
		q.Bid, q.Ask, q.Last = positivePtr(md.Bid), positivePtr(md.Ask), positivePtr(md.Last)
		q.BidSize, q.AskSize = positivePtr(float64(md.BidSize)), positivePtr(float64(md.AskSize))
		if md.VolumeObserved {
			q.Volume = new(md.Volume)
		}
		if md.OpenIntObserved {
			q.OpenInterest = new(md.OpenInt)
		}
		q.Feed = marketDataTypeName(md.FeedType)
		q.QuoteReceivedAt = md.BidAt
		if md.AskAt.After(q.QuoteReceivedAt) {
			q.QuoteReceivedAt = md.AskAt
		}
		q.LastTradeAt = md.LastTradeTime
	}
	if iv, kind, ok := c.OptionIVWithDataType(key); ok && iv > 0 {
		q.IV = new(iv)
		q.ModelSource = "live"
		if kind == ibkrlib.OptionModelDataTypeDelayed {
			q.ModelSource = "delayed"
		}
	}
	if g, ok := c.OptionGreeks(key); ok {
		q.Delta, q.Gamma, q.Vega, q.Theta = new(g.Delta), new(g.Gamma), new(g.Vega), new(g.Theta)
	}
	if px, ok := c.OptionUnderlyingPrice(key); ok && px > 0 {
		q.UnderlyingPrice = new(px)
	}
}

func positivePtr(v float64) *float64 {
	if v > 0 {
		return new(v)
	}
	return nil
}

// chainRecorderFailureStatus maps a refused option line onto a row status,
// reusing the gamma fan-out's classification of the same broker errors.
func chainRecorderFailureStatus(err error) string {
	if errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
		return chainrec.StatusUnavailable
	}
	switch classifyGammaLegFailure(err) {
	case gammaLegFailureContractMissing:
		return chainrec.StatusNoContract
	case gammaLegFailureEntitlement:
		return chainrec.StatusEntitlement
	case gammaLegFailurePacing:
		return chainrec.StatusPacing
	case gammaLegFailureTimeout:
		return chainrec.StatusTimeout
	default:
		return chainrec.StatusRejected
	}
}

func (s *Server) handleChainsRecorded(ctx context.Context, req *rpc.Request) (*rpc.ChainsRecordedResult, error) {
	var p rpc.ChainsRecordedParams
	if err := decodeFor(api.ChainsRecorded, req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeChainsRecordedParams(p)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	now := time.Now()
	data := s.platformSettings.snapshot()
	store, err := s.chainRecorderStoreHandle(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("open option-chain store: %w", err)
	}
	ran := func(date string) bool {
		has, err := store.HasRun(ctx, date)
		return err == nil && has
	}
	if store == nil {
		ran = func(string) bool { return false }
	}
	res := &rpc.ChainsRecordedResult{Version: 1, AsOf: now.UTC(), Filter: p, Runs: []rpc.ChainRecorderRun{},
		Recorder: chainRecorderState(s.chainRecorderPlan(data), data.Chains.Recorder.Symbols == nil, s.chainRecorder.running.Load(), now, ran)}
	if store == nil {
		return res, nil
	}
	if res.Store, err = store.Stats(ctx); err != nil {
		return nil, err
	}
	runs, err := store.Runs(ctx, p, s.chainRecorder.active.Load())
	if err != nil {
		return nil, err
	}
	if runs != nil {
		res.Runs = runs
	}
	return res, nil
}

// chainRecorderState is the recorder's switch and plan. NextRun skips a
// session that already has a run.
func chainRecorderState(settings chainrec.Settings, symbolsDefault, running bool, now time.Time, ran func(date string) bool) rpc.ChainRecorderState {
	st := rpc.ChainRecorderState{
		Enabled: settings.Enabled, Symbols: settings.Symbols, SymbolsDefault: symbolsDefault, MaxSymbols: chainrec.MaxSymbols,
		Declared:        []rpc.ChainRecorderDeclaration{},
		Expiries:        "two nearest standard monthlies and the nearest weekly",
		StrikeWindowPct: chainrec.StrikeWindowPct, MaxStrikesPerExpiry: chainrec.MaxStrikesPerExpiry,
		LeadMinutes: int(chainrec.SnapshotLead / time.Minute), MaxLines: chainrec.MaxLines, LineReserve: chainrec.LineReserve,
		Running: running,
	}
	if st.Symbols == nil {
		st.Symbols = []string{}
	}
	for _, d := range settings.Declared {
		st.Declared = append(st.Declared, rpc.ChainRecorderDeclaration{Symbol: d.Symbol, Source: d.Source})
	}
	if settings.Enabled {
		cal := marketcal.New()
		slot, ok := chainrec.NextSlot(cal, now)
		if ok && ran(slot.SessionDate) {
			slot, ok = chainrec.NextSlot(cal, slot.Close)
		}
		if ok {
			st.NextRun = new(slot.ScheduledAt.UTC())
		}
	}
	return st
}

func (s *Server) handleChainsRecordedRows(ctx context.Context, req *rpc.Request) (*rpc.ChainsRecordedRowsResult, error) {
	var p rpc.ChainsRecordedRowsParams
	if err := decodeFor(api.ChainsRecordedRows, req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeChainsRecordedRowsParams(p)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	store, err := s.chainRecorderStoreHandle(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("open option-chain store: %w", err)
	}
	if store == nil {
		return &rpc.ChainsRecordedRowsResult{Rows: []rpc.ChainRecordedRow{}}, nil
	}
	rows, next, err := store.Rows(ctx, p)
	if err != nil {
		return nil, err
	}
	return &rpc.ChainsRecordedRowsResult{Rows: rows, Next: next}, nil
}

// chainRecorderPlan is the recorder's settings with the registered
// strategies' declared chains.
func (s *Server) chainRecorderPlan(data platformSettingsData) chainrec.Settings {
	settings := chainRecorderSettingsFrom(data)
	settings.Declared = s.registerChainDeclarations()
	return settings
}

// registerChainDeclarations is the option chains the registered strategies
// declare (internal-docs/design/strategy-register.md, section 4): every
// entry whose rule is compiled, in any stage, held or not, live strategies
// first, then shadow, suspended and held ones, each named as its source.
func (s *Server) registerChainDeclarations() []chainrec.SymbolChoice {
	if s == nil || s.riskPolicies == nil {
		return nil
	}
	resolved := s.resolvedRegister(s.riskPolicies.snapshot().policy)
	rank := map[register.State]int{register.StateLive: 0, register.StateShadow: 1, register.StateSuspended: 2, register.StateHold: 3}
	slices.SortStableFunc(resolved, func(a, b register.Resolved) int { return rank[a.State] - rank[b.State] })
	var out []chainrec.SymbolChoice
	for _, r := range resolved {
		if r.Rule == nil {
			continue
		}
		for _, c := range r.Data.Chains {
			out = append(out, chainrec.SymbolChoice{Symbol: c.Underlying, Source: chainrec.SourceStrategyPrefix + r.Entry.ID})
		}
	}
	return out
}
