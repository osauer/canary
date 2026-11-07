package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	brokerOpenD "github.com/osauer/canary/v2/internal/broker/opend"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// opendStatusPoll is how often the daemon re-reads OpenD's state and account
// list; both are free of the pacing table's windows.
const opendStatusPoll = 30 * time.Second

// newOpenDBroker builds the one broker of a daemon whose kind is opend, nil
// under any other kind. The configuration was validated when it was resolved,
// so an account that does not parse cannot arrive here from a real start.
func newOpenDBroker(cfg *config.Resolved, version string) *brokerOpenD.Adapter {
	if cfg == nil || !cfg.IsOpenD() {
		return nil
	}
	account, err := opendPin(cfg.OpenD.Account)
	if err != nil {
		return nil
	}
	conn := futu.NewConnector(futu.Config{
		Addr:          fmt.Sprintf("%s:%d", cfg.OpenD.HostOrDefault(), cfg.OpenD.PortOrDefault()),
		Account:       account,
		ClientID:      cfg.OpenD.ClientIDOrDefault(),
		ClientVersion: clientVersionFor(version),
	})
	return brokerOpenD.New(conn, nil)
}

// opendPin reads [opend] account: empty detects, anything else is the full id.
func opendPin(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseUint(raw, 10, 64)
}

// opendUnscopedWords says why no account is scoped and how to settle it. It goes
// to the log, so it names no account: status lists the choices.
func opendUnscopedWords(health futu.Health) string {
	if health.Account != 0 {
		return "OpenD's account list does not report [opend] account"
	}
	if len(health.AccountChoices) == 0 {
		return "OpenD lists no real account on this login. Canary only uses a simulated account if you ask: set [opend] account to its id, as OpenD shows it"
	}
	return fmt.Sprintf("OpenD lists %d real accounts on this login: run `canary status` to see them, then set [opend] account to the one to use", len(health.AccountChoices))
}

// errOpenDUnavailable is the IBKR sentinel's meaning in OpenD's name. The daemon's
// callers test for that sentinel to join an outage and to pick the wire code, so
// it answers to it.
var errOpenDUnavailable error = openDUnavailableError{}

type openDUnavailableError struct{}

func (openDUnavailableError) Error() string { return "OpenD connection unavailable" }

func (openDUnavailableError) Is(target error) bool { return target == ibkrlib.ErrIBKRUnavailable }

// ibkrOnlyMessage is the answer to a read this daemon's broker does not serve.
const ibkrOnlyMessage = "IBKR only: this daemon serves OpenD"

// errIBKROnly is a read that needs the IBKR connector, asked of a daemon serving
// OpenD. It answers to the IBKR sentinel, so a caller that skips an unavailable
// source still skips it, and the wire reads it as not applicable: OpenD can be
// up, and the reader must not be sent to look at it.
var errIBKROnly error = ibkrOnlyError{}

type ibkrOnlyError struct{}

func (ibkrOnlyError) Error() string { return ibkrOnlyMessage }

func (ibkrOnlyError) Is(target error) bool { return target == ibkrlib.ErrIBKRUnavailable }

// clientVersionFor is the daemon's own version as InitConnect wants it:
// major*100+minor. A build without a release version reports zero's successor,
// which OpenD treats like any low version.
func clientVersionFor(version string) int32 {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	major, rest, _ := strings.Cut(version, ".")
	minor, _, _ := strings.Cut(rest, ".")
	a, errA := strconv.Atoi(major)
	b, errB := strconv.Atoi(minor)
	if errA != nil || errB != nil {
		return 1
	}
	return int32(a*100 + b)
}

// disableIBKRHosts turns off the features that dial an IBKR host other than the
// API socket, on the daemon's private copy of the resolved configuration: the
// Flex statement fetch, and the Web API cash ledger. The borrow-fee FTP dial is
// gated where it runs.
func disableIBKRHosts(cfg *config.Resolved) *config.Resolved {
	if cfg == nil || !cfg.IsOpenD() {
		return cfg
	}
	clone := *cfg
	clone.Flex.Enabled = false
	clone.CashLedger.URL = ""
	return &clone
}

// opendEndpoint names the OpenD session the way status reads an endpoint.
func opendEndpoint(cfg *config.Resolved) discover.Endpoint {
	return discover.Endpoint{
		Host:       cfg.OpenD.HostOrDefault(),
		Port:       cfg.OpenD.PortOrDefault(),
		Account:    strings.TrimSpace(cfg.OpenD.Account),
		PortOrigin: discover.OriginFor(cfg.OpenD.Port != nil),
	}
}

// runOpenDSession holds the OpenD session for the daemon's life: it starts the
// connector, re-reads the state, the account list and the quota on the status
// poll, and keeps the last connect's verdict in the words the status surface
// shows.
func (s *Server) runOpenDSession(ctx context.Context) {
	conn := s.opend.Connector()
	conn.Start(ctx)
	defer conn.Stop()
	tick := time.NewTicker(opendStatusPoll)
	defer tick.Stop()
	verdict := time.NewTicker(time.Second)
	defer verdict.Stop()
	ep := opendEndpoint(s.cfg)
	for {
		select {
		case <-ctx.Done():
			return
		case <-verdict.C:
			s.recordOpenDVerdict(ctx, conn, ep)
		case <-tick.C:
			if conn.Health().Connected {
				poll, cancel := context.WithTimeout(ctx, 10*time.Second)
				_ = conn.RefreshSession(poll)
				_, _ = conn.RefreshLoad(poll)
				s.refreshOpenDReads(poll)
				cancel()
			}
		}
	}
}

// refreshOpenDReads reads the account and the book as a client would, so the
// observations stay current and a new holding reaches its line with no client
// asking. Both reads are answered from OpenD's cache and spend no pacing window.
func (s *Server) refreshOpenDReads(ctx context.Context) {
	if _, _, err := s.buildAccountSummaryWithAuthority(ctx, false); err != nil {
		s.logger.Debugf("opend: account read: %v", err)
	}
	if _, err := s.handlePositionsList(ctx, &rpc.Request{}); err != nil {
		s.logger.Debugf("opend: positions read: %v", err)
	}
}

// recordOpenDVerdict publishes why the session is not ready, once per phase, so
// the process list is read only when the verdict changes.
func (s *Server) recordOpenDVerdict(ctx context.Context, conn *futu.Connector, ep discover.Endpoint) {
	health := conn.Health()
	wasReady := s.opendWasReady.Load()
	s.opendWasReady.Store(health.Ready)
	if health.Ready && !wasReady {
		s.postConnectSetupDone.Store(true)
		s.mu.Lock()
		s.lastConnectError = ""
		s.mu.Unlock()
		if _, err := conn.RefreshLoad(ctx); err != nil {
			s.logger.Debugf("opend: quota read: %v", err)
		}
		// The first check runs as the session comes up, not at the next schedule.
		s.runOpenDDataCheck(ctx)
		s.refreshOpenDReads(ctx)
	}
	if health.Ready {
		return
	}
	if !health.Ready && wasReady {
		s.postConnectSetupDone.Store(false)
	}
	phase, _ := s.opendVerdictPhase.Swap(string(health.Phase)).(string)
	verdictText := opendVerdict(ctx, health, ep, phase != string(health.Phase))
	if verdictText == "" {
		return
	}
	s.mu.Lock()
	s.lastConnectError = verdictText
	s.mu.Unlock()
}

// opendVerdict is the last attempt in the reader's words.
func opendVerdict(ctx context.Context, health futu.Health, ep discover.Endpoint, phaseChanged bool) string {
	var version *futu.ServerVersionError
	var handshake *futu.HandshakeError
	var end *futu.SessionEndError
	switch {
	case errors.As(health.LastError, &version):
		return fmt.Sprintf("OpenD %d is below the %d this Canary was built against: update OpenD", version.Server, version.Minimum)
	case errors.As(health.LastError, &handshake):
		return fmt.Sprintf("OpenD refused the connection (%s); OpenD refuses unencrypted connections while it has an RSA key: remove the RSA key from its configuration", handshake.Reply.Message)
	case errors.As(health.LastError, &end):
		switch end.Reason {
		case futu.EndForceUpdate:
			return "OpenD requires a newer Canary"
		case futu.EndKickedOut:
			return "the Futu account logged in elsewhere and OpenD dropped this session"
		case futu.EndPasswordChanged, futu.EndLoginBanned, futu.EndDeviceLock:
			return "OpenD lost its login: log in to OpenD again"
		case futu.EndScopeLost:
			return "OpenD's account list no longer reports the account this session was using"
		case futu.EndHeartbeat:
			return "OpenD stopped answering"
		}
	}
	switch health.Phase {
	case futu.PhasePortDown:
		if phaseChanged {
			return discover.OpenDPortDownHint(ctx, ep.Host, ep.Port)
		}
	case futu.PhaseProgramStatus:
		return "OpenD is not ready: its program status is " + health.State.ProgramStatus
	case futu.PhaseNotLoggedIn:
		return "OpenD is up but its quote or trade server is not logged in"
	case futu.PhaseUnscoped:
		return opendUnscopedWords(health)
	}
	return ""
}

// opendAccountSummary serves the account read from OpenD's funds.
func (s *Server) opendAccountSummary(ctx context.Context) (*rpc.AccountResult, accountSummaryAuthority, error) {
	scope := s.currentBrokerStateScope()
	binding, ok := s.opend.Capture()
	if !ok {
		return nil, accountSummaryAuthority{}, s.sessionUnavailableError()
	}
	if !brokerScopeConcrete(scope) {
		return nil, accountSummaryAuthority{}, errors.New("account summary requires OpenD's account list to settle the account")
	}
	snapshot, err := s.readAccountSnapshot(ctx, s.opend)
	if err != nil {
		return nil, accountSummaryAuthority{}, err
	}
	raw := snapshot.raw
	if raw == nil {
		return nil, accountSummaryAuthority{}, errors.New("account summary unavailable")
	}
	authority := accountSummaryAuthority{
		Provenance: snapshot.provenance, AsOf: snapshot.observedAt.UTC(),
		NetLiquidationAvailable:    raw.NetLiquidation != nil,
		TotalCashAvailable:         raw.TotalCashValue != nil,
		AvailableFundsAvailable:    raw.AvailableFunds != nil,
		BaseCurrencyAvailable:      raw.BaseCurrencyProvenance.Proven() && strings.TrimSpace(raw.BaseCurrency) != "",
		InitialMarginAvailable:     raw.InitMarginReq != nil,
		MaintenanceMarginAvailable: raw.MaintenanceMargin != nil,
	}
	res := &rpc.AccountResult{
		AccountID:    raw.AccountID,
		AccountType:  raw.AccountType,
		BaseCurrency: normCcy(raw.BaseCurrency),
		AsOf:         accountResultAuthorityAsOf(authority, time.Now().UTC()),
	}
	assign := func(dst *float64, src *float64) {
		if src != nil {
			*dst = *src
		}
	}
	assign(&res.NetLiquidation, raw.NetLiquidation)
	assign(&res.BuyingPower, raw.BuyingPower)
	assign(&res.AvailableFunds, raw.AvailableFunds)
	assign(&res.TotalCash, raw.TotalCashValue)
	assign(&res.MaintenanceMargin, raw.MaintenanceMargin)
	assign(&res.InitialMargin, raw.InitMarginReq)
	assign(&res.GrossPositionValue, raw.GrossPositionValue)
	assign(&res.UnrealizedPnL, raw.UnrealizedPnL)
	assign(&res.RealizedPnL, raw.RealizedPnL)
	if res.BaseCurrency != "" {
		res.CurrencyExposure = buildCurrencyExposure(raw.CurrencyLedger, res.BaseCurrency)
		annotateLedgerCash(res, raw.CurrencyLedger, raw.Raw)
	}
	res.Authority = accountResultDataAuthority(scope, raw, snapshot.provenance, res)
	current := s.opend.Current(binding) && sameBrokerScope(scope, s.currentBrokerStateScope())
	authority = finalizeAccountSummarySession(res, authority, current)
	s.observeAccountRPC(res, s.opend, binding)
	return res, authority, nil
}

// opendPositions serves the positions read from OpenD's position list.
func (s *Server) opendPositions(ctx context.Context, p rpc.PositionsListParams, portfolioHealth *ibkrlib.PortfolioStreamHealth, expectedScope brokerStateScope) (*rpc.PositionsResult, error) {
	binding, ok := s.opend.Capture()
	if !ok {
		return nil, s.sessionUnavailableError()
	}
	positions, receipt, err := s.opend.Positions()
	if err != nil {
		return nil, err
	}
	health := ibkrlib.PortfolioStreamHealth(receipt)
	if portfolioHealth != nil {
		*portfolioHealth = health
	}
	res := newPositionsResult(expectedScope, health, time.Now().UTC())
	if !brokerScopeConcrete(expectedScope) {
		return res, nil
	}
	wantSym := normSym(p.Symbol)
	codes := make([]string, 0, len(positions))
	for _, pos := range positions {
		if pos.Instrument.SecType == "STK" {
			codes = append(codes, pos.Instrument.ID)
		}
	}
	if wantSym == "" {
		s.refreshOpenDLines(ctx, codes)
	}
	marks, fromBatch := s.opendMarks(ctx, codes)
	if wantSym == "" {
		s.opendBudget.recordMarks(len(codes), len(fromBatch))
	}
	for _, pos := range positions {
		in := pos.Instrument
		// A contract's value carries a multiplier a quote does not, so it keeps
		// the value OpenD gives it and no analysis reads it as shares.
		if in.SecType != "STK" {
			if wantSym == "" || wantSym == strings.ToUpper(in.Symbol) {
				res.NotServed = append(res.NotServed, rpc.PositionView{
					Symbol:        in.Symbol,
					SecType:       positionSecType(in.SecType),
					Exchange:      in.Venue,
					Currency:      in.Currency,
					Quantity:      pos.Quantity,
					AvgCost:       pos.AverageCost,
					Mark:          pos.MarketPrice,
					ValuationMark: pos.MarketPrice,
					MarketValue:   pos.MarketValue,
					UnrealizedPnL: pos.UnrealizedPnL,
					RealizedPnL:   pos.RealizedPnL,
				})
			}
			continue
		}
		priceSource := ""
		if quote := marks[in.ID]; quote != nil && quote.Last > 0 {
			pos.MarketPrice = quote.Last
			pos.MarketValue = quote.Last * pos.Quantity
			if fromBatch[in.ID] {
				priceSource = "snapshot"
			}
		} else {
			// No quote, for want of a right or otherwise: the price is OpenD's own.
			priceSource = "position"
		}
		if wantSym != "" && wantSym != strings.ToUpper(in.Symbol) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(p.Type), "opt") {
			continue
		}
		res.Stocks = append(res.Stocks, rpc.PositionView{
			Symbol:        in.Symbol,
			SecType:       positionSecType(in.SecType),
			Exchange:      in.Venue,
			Currency:      in.Currency,
			Quantity:      pos.Quantity,
			Multiplier:    positionViewMultiplier(in.SecType, in.Multiplier),
			AvgCost:       pos.AverageCost,
			Mark:          pos.MarketPrice,
			ValuationMark: pos.MarketPrice,
			MarketValue:   pos.MarketValue,
			UnrealizedPnL: pos.UnrealizedPnL,
			RealizedPnL:   pos.RealizedPnL,
			PriceSource:   priceSource,
		})
	}
	flagZeroValueStockPositions(res.Stocks)
	baseCcy := ""
	var netLiquidationBase *float64
	var ledger map[string]broker.CurrencyLedger
	refresh, cancel := context.WithTimeout(ctx, 3*time.Second)
	snapshot, snapErr := s.readAccountSnapshot(refresh, s.opend)
	cancel()
	if raw := snapshot.raw; snapErr == nil && raw != nil {
		baseCcy = normCcy(raw.BaseCurrency)
		netLiquidationBase = raw.NetLiquidation
		ledger = raw.CurrencyLedger
	}
	fillFXRates(res.Stocks, ledger, baseCcy)
	fillBaseValues(res.Stocks, baseCcy)
	fillBaseValues(res.NotServed, baseCcy)
	res.ByUnderlying = groupByUnderlying(res.Stocks, res.Options, baseCcy, netLiquidationBase)
	res.Portfolio = buildPortfolioAggregatesWithBase(res.Stocks, res.Options, baseCcy)
	addPortfolioBaseContext(res.Portfolio, res.ByUnderlying, baseCcy, netLiquidationBase)
	addFXSensitivity(res.Portfolio, ledger, baseCcy)
	completedAt := time.Now().UTC()
	res.Authority = positionsResultDataAuthority(expectedScope, health, completedAt)
	res.Authority.PortfolioComplete = wantSym == "" && strings.TrimSpace(p.Type) == "" && len(res.NotServed) == 0 && currentPortfolioAuthority(res.Authority)
	authorityScope := expectedScope
	if !s.opend.Current(binding) || !sameBrokerScope(expectedScope, s.currentBrokerStateScope()) {
		authorityScope = brokerStateScope{}
		res.Authority.Availability = rpc.AccountDataUnavailable
		res.Authority.Freshness = rpc.AccountDataFreshnessUnknown
		res.Authority.Reason = rpc.AccountDataReasonSessionChanged
	}
	res.AsOf = positionsResultAuthorityAsOf(authorityScope, health, completedAt)
	s.observeAccountRPC(res, s.opend, binding)
	return res, nil
}

// opendQuote serves one quote: the streaming line when the session holds one,
// else one snapshot request.
func (s *Server) opendQuote(ctx context.Context, p rpc.QuoteSnapshotParams) (*rpc.Quote, error) {
	quote, binding, err := s.readOpenDQuote(ctx, p)
	if !binding.IsZero() {
		s.recordFeedHealth(projectQuoteSource(quote, err, s.orderNow()), s.opend, binding)
	}
	return quote, err
}

func (s *Server) readOpenDQuote(ctx context.Context, p rpc.QuoteSnapshotParams) (*rpc.Quote, broker.SessionBinding, error) {
	binding, ok := s.opend.Capture()
	if !ok {
		return nil, broker.SessionBinding{}, s.sessionUnavailableError()
	}
	code, err := s.opend.CodeFor(instrumentOfContract(p.Contract))
	if err != nil {
		return nil, binding, err
	}
	line := s.opend.Snapshot()[code]
	if line == nil {
		read, err := s.opend.ReadSnapshots(ctx, []string{code})
		if err != nil {
			return nil, binding, err
		}
		line = read[code]
	}
	if line == nil {
		return nil, binding, fmt.Errorf("OpenD has no quote for %s", code)
	}
	if !s.opend.Current(binding) {
		return nil, broker.SessionBinding{}, s.sessionUnavailableError()
	}
	return rpcQuoteOf(p.Contract, line), binding, nil
}

// instrumentOfContract reads a request's contract as the seam's identity.
func instrumentOfContract(contract rpc.ContractParams) broker.Instrument {
	return broker.Instrument{
		Symbol:   contract.Symbol,
		SecType:  contract.SecType,
		Market:   contract.Market,
		Venue:    contract.Exchange,
		Primary:  contract.PrimaryExch,
		Currency: contract.Currency,
	}
}

// rpcQuoteOf maps a seam quote to the wire. A value OpenD did not send stays nil.
func rpcQuoteOf(contract rpc.ContractParams, q *broker.Quote) *rpc.Quote {
	positive := func(v float64) *float64 {
		if v > 0 {
			return &v
		}
		return nil
	}
	symbol := contract.Symbol
	if _, listed, coded := strings.Cut(symbol, "."); coded {
		symbol = listed
	}
	out := &rpc.Quote{
		Symbol:       symbol,
		Contract:     contract,
		IVStatus:     "unavailable",
		DataType:     "live",
		FeedType:     "live",
		Delivery:     quoteDeliveryOf(q),
		ReceivedAt:   q.Timestamp,
		AsOf:         q.Timestamp,
		Bid:          positive(q.Bid),
		Ask:          positive(q.Ask),
		Last:         positive(q.Last),
		Price:        positive(q.Last),
		PriceSource:  "last",
		PriceAt:      q.LastAt,
		QuotePrice:   positive(q.Last),
		QuotePriceAt: q.LastAt,
		PrevClose:    positive(q.Close),
		DayHigh:      positive(q.High),
		DayLow:       positive(q.Low),
	}
	if q.VolumeObserved {
		out.Volume = new(q.Volume)
	}
	if q.BidSize > 0 {
		out.BidSize = new(q.BidSize)
	}
	if q.AskSize > 0 {
		out.AskSize = new(q.AskSize)
	}
	if out.Last != nil && out.PrevClose != nil {
		change := *out.Last - *out.PrevClose
		out.Change = &change
		out.ChangePct = new(change / *out.PrevClose * 100)
	}
	return out
}
