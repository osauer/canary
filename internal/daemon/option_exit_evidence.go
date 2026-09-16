package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

// This is an evidence collection deadline, not a risk-policy threshold. All
// measurements must have arrived within this one bounded collection window.
const optionExitEvidenceBudget = 20 * time.Second

type optionExitBookScope struct {
	Terminal     map[int]rpc.EarningsTerminalInfo
	Scope        brokerStateScope
	Session      string
	SessionEpoch uint64
	Generation   uint64
	BaseCurrency string
	Positions    []*ibkr.RawPosition
	Health       ibkr.PortfolioStreamHealth
}

type optionExitEvidenceSource interface {
	capture(context.Context) (optionExitBookScope, error)
	current(optionExitBookScope) bool
	option(context.Context, rpc.ContractParams) (*ibkr.OptionRiskMeasurement, error)
	quote(context.Context, rpc.ContractParams) (rpc.OrderQuoteSnapshot, error)
	fx(context.Context, string, string) (orderNotionalAuthority, error)
}

type optionExitBookEvidence struct {
	current             func() bool
	Failure             string
	TerminalFingerprint string
	Scope               string
	Fingerprint         string
	AsOf                time.Time
	Generation          uint64
	Roles               map[int]string
	// Closed is true only for an intentional session deferral after complete
	// structural/account checks. It never describes a failed market-data read.
	Closed bool
}

// Only fixed prerequisite codes cross the evidence boundary; broker errors
// may contain private fields or arbitrary text and are never projected.
type optionExitScopeError string

func (e optionExitScopeError) Error() string { return string(e) }

type optionExitBrokerSource struct {
	server    *Server
	authority *orderPreviewBrokerAuthority
}

func (b optionExitBrokerSource) capture(ctx context.Context) (optionExitBookScope, error) {
	a := b.authority
	if !b.server.orderPreviewBrokerAuthorityCurrent(a) {
		return optionExitBookScope{}, optionExitScopeError("broker_session_unavailable")
	}
	scope := b.server.currentBrokerStateScope()
	projection, ok := a.connector.CapturePortfolioProjectionForSession(a.session)
	if !ok {
		return optionExitBookScope{}, optionExitScopeError("portfolio_session_unavailable")
	}
	account, provenance, err := a.connector.RequestAccountSummaryWithProvenance(ctx, orderFXQuoteBudget)
	if err != nil || account == nil {
		return optionExitBookScope{}, optionExitScopeError("account_summary_unavailable")
	}
	if provenance != ibkr.AccountSummaryProvenanceRequest {
		return optionExitBookScope{}, optionExitScopeError("account_summary_not_current")
	}
	if !strings.EqualFold(account.AccountID, scope.Account) {
		return optionExitBookScope{}, optionExitScopeError("account_scope_mismatch")
	}
	if !account.BaseCurrencyProvenance.Proven() {
		return optionExitBookScope{}, optionExitScopeError("account_currency_unproven")
	}
	base, ok := rulebookBaseCurrency(account.BaseCurrency)
	if !ok {
		return optionExitBookScope{}, optionExitScopeError("account_currency_unproven")
	}
	// Process-local connection identity is hashed before leaving the daemon.
	// It is never reconstructed from persisted evidence or caller input.
	out := optionExitBookScope{Scope: scope, Session: fmt.Sprintf("%p/%d/%v", a.connector, a.connectorEpoch, a.session),
		SessionEpoch: a.session.Epoch(), Generation: projection.Generation, BaseCurrency: base, Positions: projection.Positions, Health: projection.Health,
		Terminal: b.server.optionExitTerminalEvidence(projection.Positions, b.server.orderNow())}
	if failure := b.currentFailure(out); failure != "" {
		return optionExitBookScope{}, optionExitScopeError(failure)
	}
	return out, nil
}

func (b optionExitBrokerSource) current(scope optionExitBookScope) bool {
	return b.currentFailure(scope) == ""
}

func (b optionExitBrokerSource) currentFailure(scope optionExitBookScope) string {
	if !b.server.orderPreviewBrokerAuthorityCurrent(b.authority) || !sameBrokerScope(scope.Scope, b.server.currentBrokerStateScope()) {
		return "broker_session_unavailable"
	}
	p, ok := b.authority.connector.CapturePortfolioProjectionForSession(b.authority.session)
	if !ok {
		return "portfolio_session_unavailable"
	}
	if p.Generation != scope.Generation || optionExitEvidenceHash(scope.Terminal) != optionExitEvidenceHash(b.server.optionExitTerminalEvidence(p.Positions, b.server.orderNow())) {
		return "portfolio_scope_changed"
	}
	if classifyPortfolioStreamHealth(scope.Scope, p.Health, b.server.orderNow()) != orderIntegrityHealthCurrent {
		return "portfolio_stream_unavailable"
	}
	if !cachedPositionsMatchBrokerScope(p.Positions, scope.Scope) {
		return "position_account_mismatch"
	}
	return ""
}

func (b optionExitBrokerSource) option(ctx context.Context, contract rpc.ContractParams) (*ibkr.OptionRiskMeasurement, error) {
	a := b.authority
	ctx, cancel := context.WithTimeout(ctx, optionExitQuoteTimeout)
	defer cancel()
	key, err := a.connector.SubscribeMarketDataWithContractForSession(ctx, a.session, *previewIBKRContract(contract), defaultGenericTicks)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = a.connector.UnsubscribeMarketDataForSession(cleanup, a.session, key)
	}()
	var receipt *ibkr.OptionRiskMeasurement
	err = pollUntilWithReject(ctx, time.Now().Add(optionExitQuoteTimeout), a.connector.SubscriptionRejectCh(key), key, func() bool {
		receipt = a.connector.OptionRiskForSession(a.session, key)
		return receipt != nil && receipt.Delta != nil && receipt.Underlying != nil
	})
	if err != nil || !b.server.orderPreviewBrokerAuthorityCurrent(a) {
		return nil, fmt.Errorf("exact model computation unavailable")
	}
	return receipt, nil
}

func (b optionExitBrokerSource) quote(ctx context.Context, contract rpc.ContractParams) (rpc.OrderQuoteSnapshot, error) {
	return b.server.previewExactSessionContractQuote(ctx, b.authority, contract, optionExitQuoteTimeout, true)
}

func (b optionExitBrokerSource) fx(ctx context.Context, currency, base string) (orderNotionalAuthority, error) {
	if currency == base {
		return orderNotionalAuthority{ContractCurrency: currency, BaseCurrency: base, BasePerContract: 1, Source: orderFXSourceIdentity}, nil
	}
	contract, inverted, err := canonicalOrderFXContract(currency, base)
	if err != nil {
		return orderNotionalAuthority{}, err
	}
	quote, err := b.server.previewExactSessionFXQuote(ctx, b.authority, contract, orderFXQuoteBudget)
	if err != nil || !optionExitLiveQuote(quote) {
		return orderNotionalAuthority{}, fmt.Errorf("live FX unavailable")
	}
	rate := (*quote.Bid + *quote.Ask) / 2
	if inverted {
		rate = 1 / rate
	}
	return newCrossCurrencyOrderNotionalAuthority(1, currency, base, rate, quote.PriceAt, quote.DataType)
}

func optionExitLiveQuote(q rpc.OrderQuoteSnapshot) bool {
	return rpc.IsLiveDataType(q.DataType) && !q.Stale && q.Bid != nil && q.Ask != nil && positiveFinite(*q.Bid) && positiveFinite(*q.Ask) && *q.Ask >= *q.Bid && !q.PriceAt.IsZero()
}

func optionExitEvidenceHash(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Reuse the exact cancelled/dissolved stock authority used by
// analysisPositions. Neither a missing quote nor a zero mark is an exemption.
// Options, aliases with conflicting identity, and duplicate stock identities
// remain in the measured book. Only fingerprints leave this private receipt.
func (s *Server) optionExitTerminalEvidence(rows []*ibkr.RawPosition, now time.Time) map[int]rpc.EarningsTerminalInfo {
	out := make(map[int]rpc.EarningsTerminalInfo)
	if s == nil || s.earningsTerminal == nil {
		return out
	}
	counts := make(map[string]int)
	for _, p := range rows {
		if p != nil && p.Position != 0 {
			counts[normSym(p.Contract.Symbol)]++
		}
	}
	for _, p := range rows {
		if p == nil || p.Position == 0 || p.Contract.SecType != "STK" || counts[normSym(p.Contract.Symbol)] != 1 {
			continue
		}
		match, found := s.earningsTerminal.terminalEarningsFor(risk.NameInput{Symbol: p.Contract.Symbol, StockConID: p.Contract.ConID, StockSecType: p.Contract.SecType}, now)
		info := rpc.EarningsInfo{Symbol: p.Contract.Symbol, Source: "verified_terminal", Status: match.Status, Reason: match.Reason, Terminal: &match.Info}
		if found && validRulebookTerminalEarningsAuthority(info, now) {
			out[p.Contract.ConID] = match.Info
		}
	}
	return out
}

func (s *Server) optionExitBoundTerminalEvidence(binding *brokerWriteTransactionBinding, now time.Time) (map[int]rpc.EarningsTerminalInfo, bool) {
	if binding == nil || binding.connector == nil {
		return nil, false
	}
	p, ok := binding.connector.CapturePortfolioProjectionForBoundSession(binding.session)
	if !ok {
		return nil, false
	}
	return s.optionExitTerminalEvidence(p.Positions, now), true
}

func optionExitScopeHash(scope optionExitBookScope) string {
	return optionExitEvidenceHash(struct {
		Scope      brokerStateScope
		Session    string
		Generation uint64
		Base       string
		Terminal   string
	}{scope.Scope, scope.Session, scope.Generation, scope.BaseCurrency, optionExitEvidenceHash(scope.Terminal)})
}

func optionExitContract(row rpc.PositionView) (rpc.ContractParams, bool) {
	switch strings.ToUpper(strings.TrimSpace(row.SecType)) {
	case "STK", "STOCK", "ETF", "OPT", "OPTION":
	default:
		return rpc.ContractParams{}, false
	}
	typ := positionWireSecType(row.SecType)
	c := proposalContractFromPosition(row, typ)
	if row.ConID <= 0 || strings.TrimSpace(row.Symbol) == "" || strings.TrimSpace(row.Currency) == "" ||
		math.IsNaN(row.Quantity) || math.IsInf(row.Quantity, 0) || row.Quantity == 0 || (typ != "OPT" && typ != "STK") {
		return c, false
	}
	if typ == "OPT" && (row.Multiplier <= 0 || math.Trunc(row.Quantity) != row.Quantity || row.TradingClass == "" || row.LocalSymbol == "" ||
		row.Expiry == "" || !positiveFinite(row.Strike) || (row.Right != "P" && row.Right != "C")) {
		return c, false
	}
	return c, true
}

// optionExitScopeFailure requires every nonzero raw broker position to be
// represented exactly once. Unsupported instruments cannot silently shrink
// the denominator. Average cost and every option identity field must agree.
func optionExitScopeFailure(scope optionExitBookScope, pos *rpc.PositionsResult, now time.Time) string {
	if pos == nil || !brokerScopeConcrete(scope.Scope) || scope.Session == "" || scope.SessionEpoch == 0 || scope.Generation == 0 || scope.BaseCurrency == "" ||
		scope.Generation != scope.Health.ProjectionGeneration || classifyPortfolioStreamHealth(scope.Scope, scope.Health, now) != orderIntegrityHealthCurrent || !cachedPositionsMatchBrokerScope(scope.Positions, scope.Scope) {
		return "portfolio_scope_invalid"
	}
	rows := make(map[int]rpc.PositionView)
	for _, row := range append(slices.Clone(pos.Stocks), pos.Options...) {
		if row.Quantity == 0 {
			continue
		}
		if _, ok := optionExitContract(row); !ok {
			return "position_identity_incomplete"
		}
		if _, exists := rows[row.ConID]; exists {
			return "position_scope_incomplete"
		}
		rows[row.ConID] = row
	}
	seen := make(map[int]bool)
	for _, raw := range scope.Positions {
		if raw == nil {
			return "position_scope_incomplete"
		}
		if raw.Position == 0 {
			continue
		}
		if seen[raw.Contract.ConID] {
			return "position_scope_incomplete"
		}
		seen[raw.Contract.ConID] = true
		row, ok := rows[raw.Contract.ConID]
		if terminal, excluded := scope.Terminal[raw.Contract.ConID]; excluded && terminal.ContractConID == raw.Contract.ConID &&
			strings.EqualFold(raw.Contract.SecType, "STK") && now.Before(terminal.RevalidateAfter) {
			delete(rows, raw.Contract.ConID)
			continue
		}
		if !ok {
			return "position_scope_incomplete"
		}
		if raw.Position != row.Quantity || raw.AverageCost != row.AvgCost {
			return "position_values_changed"
		}
		c, _ := optionExitContract(row)
		wire := raw.Contract
		switch strings.ToUpper(strings.TrimSpace(wire.SecType)) {
		case "STK", "STOCK", "ETF", "OPT", "OPTION":
		default:
			return "position_identity_incomplete"
		}
		wire.SecType = positionWireSecType(wire.SecType)
		if c.ConID != wire.ConID || c.Symbol != strings.ToUpper(strings.TrimSpace(wire.Symbol)) || c.SecType != wire.SecType ||
			c.Currency != wire.Currency || c.LocalSymbol != wire.LocalSymbol || c.TradingClass != wire.TradingClass {
			return "position_identity_mismatch"
		}
		// Match the position projection's security-type semantics: stock
		// portfolio frames may carry derivative placeholders that are omitted
		// from PositionView. Every actual option term remains exact.
		if c.SecType == "OPT" && (c.Expiry != wire.Expiry || c.Right != wire.Right || c.Strike != wire.Strike || c.Multiplier != wire.Multiplier) {
			return "option_terms_mismatch"
		}
		delete(rows, raw.Contract.ConID)
	}
	if len(rows) != 0 {
		return "position_scope_incomplete"
	}
	return ""
}

func collectOptionExitEvidence(ctx context.Context, src optionExitEvidenceSource, pos *rpc.PositionsResult, now time.Time, clock func() time.Time) optionExitBookEvidence {
	out := optionExitBookEvidence{Failure: "portfolio_scope_invalid"}
	fail := func(reason string) optionExitBookEvidence {
		return optionExitBookEvidence{Failure: reason, Scope: out.Scope, Generation: out.Generation}
	}
	if src == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, optionExitEvidenceBudget)
	defer cancel()
	scope, err := src.capture(ctx)
	if err != nil {
		if failure, ok := err.(optionExitScopeError); ok && optionExitScopeFailureMessage(string(failure)) != "" {
			out.Failure = string(failure)
		}
		return out
	}
	if failure := optionExitScopeFailure(scope, pos, clock()); failure != "" {
		return fail(failure)
	}
	out.Scope, out.Generation = optionExitScopeHash(scope), scope.Generation
	out.TerminalFingerprint = optionExitEvidenceHash(scope.Terminal)
	out.current = func() bool { return src.current(scope) }
	// Prove known route support even when collection is deferred. Missing FX
	// identity or an unsupported currency is a blocker, not normal waiting.
	for _, row := range append(slices.Clone(pos.Stocks), pos.Options...) {
		if row.Quantity == 0 {
			continue
		}
		if row.Currency != scope.BaseCurrency {
			if _, _, err := canonicalOrderFXContract(row.Currency, scope.BaseCurrency); err != nil {
				return fail("currency_data_unavailable")
			}
		}
	}
	session, sessionErr := marketcal.NewWithClock(clock).SessionAt(marketcal.MarketUSOptions, now)
	if sessionErr != nil || session.State == marketcal.StateUnknown {
		return fail("session_unknown")
	}
	if !session.IsOpen {
		out.Closed = src.current(scope)
		if out.Closed {
			out.Failure = "session_closed"
		}
		return out
	}
	measured := &rpc.PositionsResult{}
	var receipts []*ibkr.OptionRiskMeasurement
	var quotes []rpc.OrderQuoteSnapshot
	fxs := make(map[string]orderNotionalAuthority)
	started := clock()
	for _, original := range append(slices.Clone(pos.Stocks), pos.Options...) {
		if original.Quantity == 0 {
			continue
		}
		if _, excluded := scope.Terminal[original.ConID]; excluded {
			continue
		}
		row := original
		contract, _ := optionExitContract(row)
		fx, ok := fxs[row.Currency]
		if !ok {
			fx, err = src.fx(ctx, row.Currency, scope.BaseCurrency)
			if err != nil || fx.ContractCurrency != row.Currency || fx.BaseCurrency != scope.BaseCurrency || !positiveFinite(fx.BasePerContract) {
				return fail("currency_data_unavailable")
			}
			if row.Currency == scope.BaseCurrency {
				if fx.Source != orderFXSourceIdentity || fx.BasePerContract != 1 {
					return fail("currency_data_unavailable")
				}
			} else if fx.Source != orderFXSourceExactSessionQuote || !rpc.IsLiveDataType(fx.DataType) || fx.EvidenceAt.Before(started) || fx.EvidenceAt.After(clock()) {
				return fail("currency_data_unavailable")
			}
			fxs[row.Currency] = fx
		}
		row.FXRate = new(fx.BasePerContract)
		if contract.SecType == "OPT" {
			r, err := src.option(ctx, contract)
			if err != nil || !optionExitModelValid(r, contract, started, clock()) || r.SessionEpoch != scope.SessionEpoch {
				return fail("exact_model_unavailable")
			}
			row.Delta, row.Underlying = cloneFloat64Ptr(r.Delta), cloneFloat64Ptr(r.Underlying)
			receipts = append(receipts, r)
			measured.Options = append(measured.Options, row)
		} else {
			q, err := src.quote(ctx, contract)
			if err != nil || !optionExitLiveQuote(q) || q.PriceAt.Before(started) || q.PriceAt.After(clock()) || q.SessionContext == nil || !q.SessionContext.IsOpen {
				return fail("portfolio_price_unavailable")
			}
			row.Mark, row.Stale = (*q.Bid+*q.Ask)/2, false
			quotes = append(quotes, q)
			measured.Stocks = append(measured.Stocks, row)
		}
	}
	completed := clock()
	if ctx.Err() != nil || completed.Sub(started) > optionExitEvidenceBudget || !optionSessionOpen(completed) || !src.current(scope) {
		return fail("portfolio_scope_invalid")
	}
	fillBaseValues(measured.Stocks, scope.BaseCurrency)
	fillBaseValues(measured.Options, scope.BaseCurrency)
	measured.ByUnderlying = groupByUnderlying(measured.Stocks, measured.Options, scope.BaseCurrency, nil)
	classified, ok := risk.ClassifyCompleteIndexPutRoles(risk.RuleInputs{BaseCurrency: scope.BaseCurrency, Positions: risk.SourceState{Healthy: true}, Names: mapRuleNames(measured, risk.DefaultRulebookPolicy(), scope.BaseCurrency)}, risk.DefaultRulebookPolicy())
	if !ok {
		return fail("portfolio_scope_invalid")
	}
	out.Roles = make(map[int]string)
	for ni, group := range measured.ByUnderlying {
		for li, row := range group.Options {
			out.Roles[row.ConID] = classified.Names[ni].Legs[li].IndexPutRole
		}
	}
	// Every model, price and non-identity FX receipt is checked against this
	// request boundary. Expire from the boundary, not completion: a slow
	// collection must not grant its oldest observation another full lifetime.
	out.AsOf = started
	out.Failure = ""
	out.Fingerprint = optionExitEvidenceHash(struct {
		Scope  string
		Models []*ibkr.OptionRiskMeasurement
		Quotes []rpc.OrderQuoteSnapshot
		FX     map[string]orderNotionalAuthority
	}{out.Scope, receipts, quotes, fxs})
	return out
}

func optionExitModelValid(r *ibkr.OptionRiskMeasurement, c rpc.ContractParams, started, now time.Time) bool {
	if r == nil || r.RequestID <= 0 || r.SessionEpoch == 0 || r.DataType != 1 || r.RequestedAt.Before(started) ||
		r.ReceivedAt.Before(r.RequestedAt) || r.ReceivedAt.After(now) || now.Sub(r.ReceivedAt) > optionExitEvidenceBudget ||
		r.Delta == nil || math.IsNaN(*r.Delta) || math.IsInf(*r.Delta, 0) || math.Abs(*r.Delta) > 1.05 ||
		r.Underlying == nil || !positiveFinite(*r.Underlying) {
		return false
	}
	w := r.Contract
	return w.ConID == c.ConID && w.Symbol == c.Symbol && w.SecType == c.SecType && w.Currency == c.Currency && w.Exchange == c.Exchange &&
		w.LocalSymbol == c.LocalSymbol && w.TradingClass == c.TradingClass && w.Expiry == c.Expiry && w.Strike == c.Strike && w.Right == c.Right && w.Multiplier == c.Multiplier
}

func (e *proposalEngine) optionExitEvidence(ctx context.Context, pos *rpc.PositionsResult, now time.Time) optionExitBookEvidence {
	if e == nil || e.server == nil {
		return optionExitBookEvidence{}
	}
	src := e.optionExitSource
	if src == nil {
		a, err := e.server.captureOrderPreviewBrokerAuthority()
		if err != nil || a == nil {
			return optionExitBookEvidence{Failure: "broker_session_unavailable"}
		}
		src = optionExitBrokerSource{server: e.server, authority: a}
	}
	out := collectOptionExitEvidence(ctx, src, pos, now, e.clock)
	e.mu.Lock()
	defer e.mu.Unlock()
	// Closing the exchange does not resolve a known data/permission failure
	// from the same scope. Only a successful live read clears that failure.
	previous := e.lastOptionExitEvidence
	if out.Closed && out.Scope != "" && out.Scope == previous.Scope {
		switch previous.Failure {
		case "exact_model_unavailable", "currency_data_unavailable", "portfolio_price_unavailable":
			out.Closed, out.Failure = false, previous.Failure
		}
	}
	e.lastOptionExitEvidence = out
	return out
}

func optionExitEvidenceAt(evidence optionExitBookEvidence, now time.Time) optionExitBookEvidence {
	if (evidence.current != nil && !evidence.current()) || (evidence.Fingerprint != "" && (evidence.AsOf.After(now) || !now.Before(evidence.AsOf.Add(optionExitEvidenceBudget)))) {
		return optionExitBookEvidence{Failure: "portfolio_scope_invalid"}
	}
	return evidence
}

func sameOptionExitContract(a, b rpc.ContractParams) bool {
	return a.ConID > 0 && a.ConID == b.ConID && a.Symbol == b.Symbol && a.SecType == b.SecType && a.Expiry == b.Expiry &&
		a.Strike == b.Strike && a.Right == b.Right && a.Multiplier == b.Multiplier && a.Currency == b.Currency &&
		a.LocalSymbol == b.LocalSymbol && a.TradingClass == b.TradingClass && a.Exchange == b.Exchange
}

func (e *proposalEngine) optionExitPreviouslyProtection(conID int) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.snapshot.Proposals {
		if p.Contract.ConID == conID && p.OptionExit != nil && p.OptionExit.EconomicRole == risk.IndexPutRoleProtection {
			return true
		}
	}
	return false
}

func setOptionExitReadiness(p *rpc.TradeProposal, deferred bool) {
	if p == nil || p.OptionExit == nil {
		return
	}
	x := p.OptionExit
	x.Readiness = "blocked"
	if len(p.Blockers) == 0 && x.Kind != "review" && p.State == rpc.TradeProposalStateGenerated {
		x.Readiness = "ready"
		return
	}
	closedBlocker := slices.ContainsFunc(p.Blockers, func(b rpc.TradingBlocker) bool { return b.Code == "option_rth_closed" })
	if !deferred || x.Kind != "review" || x.Intent != "directional" || x.EconomicRole == risk.IndexPutRoleProtection || !closedBlocker {
		return
	}
	for _, b := range p.Blockers {
		switch b.Code {
		case "option_rth_closed", "live_option_quote_required", "fresh_option_quote_required", "two_sided_option_quote_required", "directional_role_not_confirmed", "option_exit_measurement_unavailable":
		default:
			return
		}
	}
	x.Readiness = "waiting"
	// No threshold was measured during intentional deferral. Keep all
	// blockers, including the summary consequence, for adapters and audit.
	x.ReferencePrice, x.ReturnPct = nil, nil
}

func optionExitScopeFailureMessage(reason string) string {
	switch reason {
	case "broker_session_unavailable":
		return "the broker connection changed or is not ready"
	case "portfolio_session_unavailable":
		return "positions could not be captured from the current broker connection"
	case "account_summary_unavailable":
		return "the fresh broker account-summary request failed or returned no account"
	case "account_summary_not_current":
		return "the broker returned cached account data instead of a completed fresh summary"
	case "account_scope_mismatch":
		return "the fresh account summary does not match the connected account"
	case "account_currency_unproven":
		return "the fresh account summary does not prove the account base currency"
	case "portfolio_scope_changed":
		return "positions or verified terminal-stock evidence changed during account capture"
	case "portfolio_stream_unavailable":
		return "the account-scoped portfolio stream is not current and complete"
	case "position_account_mismatch":
		return "a broker position does not match the connected account"
	case "position_identity_incomplete":
		return "a portfolio instrument is unsupported or lacks required contract identity"
	case "position_scope_incomplete":
		return "broker positions and the analysis portfolio do not contain the same complete set of unique contracts"
	case "position_values_changed":
		return "position quantity or cost basis differs between the broker and analysis portfolio"
	case "position_identity_mismatch":
		return "contract identity differs between the broker and analysis portfolio"
	case "option_terms_mismatch":
		return "option expiry, right, strike, or multiplier differs between the broker and analysis portfolio"
	}
	return ""
}

func explainOptionExitEconomicBlocker(p *rpc.TradeProposal, evidence optionExitBookEvidence) {
	if p.OptionExit == nil {
		return
	}
	message, action := "", ""
	if p.OptionExit.EconomicRole == risk.IndexPutRoleProtection {
		message = "Rulebook classified this exact put as portfolio protection; an independent directional declaration cannot authorize removing protection"
		action = "Review the combined portfolio hedge before considering an exit; the directional option-exit workflow remains blocked."
	} else {
		switch evidence.Failure {
		case "session_closed":
			message = "economic-role measurement is waiting for the regular listed-options session; the complete position scope was checked and live collection was deferred"
			action = "Canary will re-evaluate with fresh exact-contract risk evidence when the session opens."
		case "currency_data_unavailable":
			message = "economic role is unclassified because current explicit currency-to-base evidence is unavailable"
			action = "Restore live currency data for the complete portfolio and refresh; missing FX cannot be treated as parity."
		case "exact_model_unavailable":
			message = "economic role is unclassified because a portfolio option lacks fresh live broker option-risk data (price sensitivity and underlying price) tied to its exact contract and current broker connection"
			action = "Check option market-data availability and permissions, then refresh during the regular session."
		case "portfolio_price_unavailable":
			message = "economic role is unclassified because a non-exempt portfolio stock lacks a fresh live price"
			action = "Restore current stock pricing for the complete portfolio; a missing price is not zero exposure."
		case "session_unknown":
			message = "economic role is unclassified because the official listed-options session is unknown"
			action = "Resolve market-calendar coverage before refreshing the exit check."
		default:
			message = "economic role is unclassified because the complete account and position scope is unavailable, invalid, or changed during measurement"
			if detail := optionExitScopeFailureMessage(evidence.Failure); detail != "" {
				message = "economic role is unclassified because " + detail
			}
			action = "Refresh broker account and positions, resolve unsupported or incomplete rows, and retry from one unchanged portfolio scope."
		}
	}
	for i := range p.Blockers {
		if p.Blockers[i].Code == "directional_role_not_confirmed" {
			p.Blockers[i].Message, p.Blockers[i].Action = message, action
		}
	}
}

// revalidateOptionExitEconomics runs after the ordinary preview/WhatIf. A
// fresh complete book must retain the reviewed scope and directional role.
// The resulting signed token binds that scope through admission and the
// existing structural portfolio wire guard; no token grants write authority.
func (e *proposalEngine) revalidateOptionExitEconomics(ctx context.Context, prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	if prop.OptionExit == nil || !risk.DefaultRulebookPolicy().IsHedgeSymbol(prop.Symbol) || prop.Contract.Right != "P" {
		return nil
	}
	blocked := func() []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: "option_exit_economic_evidence_changed", Message: "current exact-contract portfolio evidence no longer confirms this directional exit", Action: "Refresh proposals from a complete live portfolio and preview again."}}
	}
	if e.server == nil || e.server.orderTokens == nil || preview == nil || prop.OptionExit.EconomicEvidence == nil {
		return blocked()
	}
	pos, err := e.server.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		return blocked()
	}
	pos = e.server.analysisPositions(pos, e.clock())
	policy, status := e.server.protectionPolicies.Active()
	intents := directionalOptionIntents(policy.Buckets.TrailingStop.Options)
	intent, ok := intents[prop.Contract.ConID]
	legs, ambiguous := optionExitStrategyScope(pos, intents, e.clock())
	if !ok || e.clock().Before(intent.ApprovedAt) || !e.clock().Before(intent.ExpiresAt) ||
		status.Fingerprint != prop.PolicyFingerprint || len(status.Blockers) > 0 || legs[prop.Contract.ConID] || ambiguous[prop.Symbol] {
		return blocked()
	}
	evidence := e.optionExitEvidence(ctx, pos, e.clock())
	if evidence.Scope != prop.OptionExit.EconomicEvidence.Scope || evidence.Fingerprint == "" || evidence.Roles[prop.Contract.ConID] != risk.IndexPutRoleDirectional || evidence.Closed {
		return blocked()
	}
	payload, err := e.server.orderTokens.verify(preview.PreviewToken)
	if err != nil || payload.PortfolioGeneration != evidence.Generation || payload.Draft.Contract != preview.Draft.Contract {
		return blocked()
	}
	proof := &rpc.OptionExitEconomicEvidence{Scope: evidence.Scope, Fingerprint: evidence.Fingerprint, AsOf: evidence.AsOf, PortfolioGeneration: evidence.Generation}
	proof.TerminalFingerprint = evidence.TerminalFingerprint
	if validateOptionExitTokenEvidence(proof, evidence.Scope, payload.PortfolioGeneration, e.clock()) != nil {
		return blocked()
	}
	payload.OptionExitEconomics = proof
	if expires := proof.AsOf.Add(optionExitEvidenceBudget); expires.Before(payload.ExpiresAt) {
		payload.ExpiresAt = expires
	}
	token, id, expires, err := e.server.orderTokens.mint(payload)
	if err != nil {
		return blocked()
	}
	preview.PreviewToken, preview.PreviewTokenID, preview.PreviewTokenExpiresAt = token, id, expires
	return nil
}

func validateOptionExitTokenEvidence(proof *rpc.OptionExitEconomicEvidence, scope string, generation uint64, now time.Time) error {
	if proof == nil || proof.Scope == "" || proof.Fingerprint == "" || proof.Scope != scope || proof.PortfolioGeneration == 0 || proof.PortfolioGeneration != generation ||
		proof.AsOf.IsZero() || proof.AsOf.After(now) || !now.Before(proof.AsOf.Add(optionExitEvidenceBudget)) {
		return fmt.Errorf("%w: option economic-role scope or freshness changed; refresh and preview again", ErrTradingDisabled)
	}
	return nil
}
