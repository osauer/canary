package daemon

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/strategy"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

type optionEvidenceFixture struct {
	scope      optionExitBookScope
	models     map[int]*ibkr.OptionRiskMeasurement
	price      rpc.OrderQuoteSnapshot
	prices     map[int]rpc.OrderQuoteSnapshot // exact per-contract quotes, when a test needs legs to differ
	fxEvidence orderNotionalAuthority
	currentOK  bool
	readErr    error
	captureErr error
	quoteErr   error // fails exact option quote reads only; stock pricing for the book evidence still answers
	reads      int
	onRead     func()
}

func (f *optionEvidenceFixture) capture(context.Context) (optionExitBookScope, error) {
	return f.scope, f.captureErr
}
func (f *optionEvidenceFixture) current(optionExitBookScope) bool { return f.currentOK }
func (f *optionEvidenceFixture) option(_ context.Context, c rpc.ContractParams) (*ibkr.OptionRiskMeasurement, error) {
	f.reads++
	if f.onRead != nil {
		f.onRead()
	}
	return f.models[c.ConID], f.readErr
}
func (f *optionEvidenceFixture) quote(_ context.Context, c rpc.ContractParams) (rpc.OrderQuoteSnapshot, error) {
	f.reads++
	if f.quoteErr != nil && c.SecType == "OPT" {
		return rpc.OrderQuoteSnapshot{}, f.quoteErr
	}
	if q, ok := f.prices[c.ConID]; ok {
		return q, f.readErr
	}
	q := f.price
	if c.SecType == "OPT" {
		q.Bid, q.Ask = new(0.35), new(0.36)
	}
	return q, f.readErr
}
func (f *optionEvidenceFixture) fx(_ context.Context, currency, base string) (orderNotionalAuthority, error) {
	if currency == base {
		return orderNotionalAuthority{ContractCurrency: currency, BaseCurrency: base, BasePerContract: 1, Source: orderFXSourceIdentity}, nil
	}
	return f.fxEvidence, f.readErr
}

func newOptionEvidenceFixture() (*optionEvidenceFixture, *rpc.PositionsResult, time.Time) {
	now := optionExitTestTime()
	row := optionExitTestRow()
	row.Symbol, row.LocalSymbol, row.TradingClass, row.Right = "SPY", "SYNTHETIC PUT", "SPY", "P"
	stock := rpc.PositionView{ConID: 900002, Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD", Quantity: 10, Mark: 100, AvgCost: 100}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{stock}, Options: []rpc.PositionView{row}}
	scope := optionExitBookScope{Scope: brokerStateScope{Account: "U_SYNTHETIC", Mode: rpc.AccountModeLive}, Session: "synthetic-session", SessionEpoch: 3, Generation: 7, BaseCurrency: "USD", Terminal: map[int]rpc.EarningsTerminalInfo{},
		Health: ibkr.PortfolioStreamHealth{Account: "U_SYNTHETIC", InitialCompletedAt: now, LastUpdateAt: now, ProjectionGeneration: 7}}
	for _, r := range []rpc.PositionView{stock, row} {
		c, _ := optionExitContract(r)
		scope.Positions = append(scope.Positions, &ibkr.RawPosition{Account: scope.Scope.Account, Contract: *previewIBKRContract(c), Position: r.Quantity, AverageCost: r.AvgCost})
	}
	c, _ := optionExitContract(row)
	f := &optionEvidenceFixture{scope: scope, currentOK: true, models: map[int]*ibkr.OptionRiskMeasurement{row.ConID: {
		Contract: *previewIBKRContract(c), RequestID: 1001, SessionEpoch: 3, RequestedAt: now, ReceivedAt: now, DataType: 1, Delta: new(-0.5), Underlying: new(100.0),
	}}, price: rpc.OrderQuoteSnapshot{Bid: new(100.0), Ask: new(100.0), DataType: rpc.MarketDataLive, PriceAt: now, SessionContext: &rpc.MarketSession{IsOpen: true}}}
	return f, pos, now
}

func TestOptionExitCompleteEvidenceClearsOnlyEconomicBlocker(t *testing.T) {
	f, pos, now := newOptionEvidenceFixture()
	evidence := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
	row := pos.Options[0]
	allowed, role := optionExitEconomicRole(row, risk.DefaultRulebookPolicy(), evidence)
	if !allowed || role != risk.IndexPutRoleDirectional || evidence.Fingerprint == "" {
		t.Fatalf("complete directional book still blocked: %+v", evidence)
	}
	pol := enabledOptionExitPolicy()
	row.OptionBid, row.OptionAsk = new(0.35), new(0.36)
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, allowed, risk.DefaultRulebookPolicy().ExitActLossPct)
	if decision.Action != risk.OptionExitActionLoss || len(decision.Blockers) != 0 {
		t.Fatalf("fresh complete directional evidence did not qualify: %+v", decision)
	}
	for name, mutate := range map[string]func(*rpc.PositionView, *bool, *bool){
		"intent":        func(_ *rpc.PositionView, intent, _ *bool) { *intent = false },
		"group":         func(_ *rpc.PositionView, _, standalone *bool) { *standalone = false },
		"quantity":      func(r *rpc.PositionView, _, _ *bool) { r.Quantity = 1.5 },
		"DTE":           func(r *rpc.PositionView, _, _ *bool) { r.Expiry = now.Format("20060102") },
		"delayed_price": func(r *rpc.PositionView, _, _ *bool) { r.DataType = rpc.MarketDataDelayed },
		"wide_spread":   func(r *rpc.PositionView, _, _ *bool) { r.OptionAsk = new(1.0) },
	} {
		t.Run(name, func(t *testing.T) {
			r, intent, standalone := row, true, true
			mutate(&r, &intent, &standalone)
			d := evaluateOptionExit(pol.Buckets.TrailingStop.Options, r, now, intent, standalone, allowed, risk.DefaultRulebookPolicy().ExitActLossPct)
			if d.Action != "" || len(d.Blockers) == 0 {
				t.Fatal("economic evidence bypassed another guard")
			}
		})
	}
	// The former unconditional blocker stays closed when exact evidence is absent.
	if ok, _ := optionExitEconomicRole(row, risk.DefaultRulebookPolicy()); ok {
		t.Fatal("shared row authorized a hedge-listed put")
	}
}

func TestOptionExitExactEvidenceRejectsIncompleteDriftingAndInvalidBook(t *testing.T) {
	for name, mutate := range map[string]func(*optionEvidenceFixture, *rpc.PositionsResult, time.Time){
		"conid": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.models[42].Contract.ConID++ },
		"trading_class": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.models[42].Contract.TradingClass = "SPYW"
		},
		"local_symbol": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.models[42].Contract.LocalSymbol = "OTHER"
		},
		"right":         func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.models[42].Contract.Right = "C" },
		"strike":        func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.models[42].Contract.Strike++ },
		"session":       func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.models[42].SessionEpoch++ },
		"missing_delta": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.models[42].Delta = nil },
		"missing_spot":  func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.models[42].Underlying = nil },
		"delayed":       func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.models[42].DataType = 3 },
		"frozen":        func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.models[42].DataType = 2 },
		"stale_model_new_quote": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, now time.Time) {
			f.models[42].ReceivedAt = now.Add(-time.Minute)
		},
		"future": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, now time.Time) {
			f.models[42].ReceivedAt = now.Add(time.Second)
		},
		"nan_delta": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.models[42].Delta = new(math.NaN())
		},
		"infinite_spot": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.models[42].Underlying = new(math.Inf(1))
		},
		"partial_long_book": func(_ *optionEvidenceFixture, p *rpc.PositionsResult, _ time.Time) { p.Stocks = nil },
		"duplicate": func(_ *optionEvidenceFixture, p *rpc.PositionsResult, _ time.Time) {
			p.Options = append(p.Options, p.Options[0])
		},
		"account": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.scope.Health.Account = "U_OTHER"
		},
		"raw_account": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.scope.Positions[0].Account = "U_OTHER"
		},
		"unprimed": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.scope.Health.InitialCompletedAt = time.Time{}
		},
		"unsupported": func(f *optionEvidenceFixture, p *rpc.PositionsResult, _ time.Time) {
			p.Stocks[0].SecType = "FUT"
			f.scope.Positions[0].Contract.SecType = "FUT"
		},
		"position_change": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.onRead = func() { f.currentOK = false }
		},
		"reconnect":           func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.currentOK = false },
		"missing_stock_price": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.price.Bid = nil },
		"stock_price_failure": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) {
			f.readErr = errors.New("synthetic data failure")
		},
		"missing_fx": func(f *optionEvidenceFixture, _ *rpc.PositionsResult, _ time.Time) { f.scope.BaseCurrency = "EUR" },
	} {
		t.Run(name, func(t *testing.T) {
			f, pos, now := newOptionEvidenceFixture()
			mutate(f, pos, now)
			ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
			if ev.Fingerprint != "" || ev.Closed || len(ev.Roles) != 0 {
				t.Fatalf("invalid evidence classified or hidden as waiting: %+v", ev)
			}
		})
	}
}

func TestOptionExitEconomicRoleCombinesIndependentPutsAndPreservesHedges(t *testing.T) {
	f, pos, now := newOptionEvidenceFixture()
	pol := risk.DefaultRulebookPolicy()
	band := 2 * math.Max(pol.RegimeCalm.HedgeBandMaxPct, math.Max(pol.RegimeEarlyWarning.HedgeBandMaxPct, pol.RegimeConfirmed.HedgeBandMaxPct))
	pos.Stocks[0].Quantity = 15000 / (band / 100) / 100
	f.scope.Positions[0].Position = pos.Stocks[0].Quantity
	ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
	if ok, role := optionExitEconomicRole(pos.Options[0], pol, ev); ok || role != risk.IndexPutRoleProtection {
		t.Fatal("actual hedge lost protection")
	}
	second := pos.Options[0]
	second.ConID = 43
	second.LocalSymbol = "SYNTHETIC SECOND PUT"
	second.Expiry = "20261016"
	pos.Options = append(pos.Options, second)
	c, _ := optionExitContract(second)
	f.scope.Positions = append(f.scope.Positions, &ibkr.RawPosition{Account: f.scope.Scope.Account, Contract: *previewIBKRContract(c), Position: second.Quantity, AverageCost: second.AvgCost})
	r := *f.models[42]
	r.Contract = *previewIBKRContract(c)
	r.RequestID++
	f.models[43] = &r
	ev = collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
	for _, row := range pos.Options {
		if ok, role := optionExitEconomicRole(row, pol, ev); !ok || role != risk.IndexPutRoleDirectional {
			t.Fatal("combined short exposure ignored a separately managed put")
		}
	}
}

func TestOptionExitSlowCollectionDoesNotRenewEvidenceLifetime(t *testing.T) {
	f, pos, started := newOptionEvidenceFixture()
	now := started
	f.onRead = func() { now = started.Add(19 * time.Second) }
	ev := collectOptionExitEvidence(context.Background(), f, pos, started, func() time.Time { return now })
	if ev.Fingerprint == "" {
		t.Fatalf("slow collection failed before the send-age witness: %+v", ev)
	}
	proof := &rpc.OptionExitEconomicEvidence{Scope: ev.Scope, Fingerprint: ev.Fingerprint, AsOf: ev.AsOf, PortfolioGeneration: ev.Generation}
	if err := validateOptionExitTokenEvidence(proof, ev.Scope, ev.Generation, now); err != nil {
		t.Fatal(err)
	}
	if err := validateOptionExitTokenEvidence(proof, ev.Scope, ev.Generation, started.Add(21*time.Second)); err == nil {
		t.Fatal("slow collection granted old observations a new lifetime")
	}
	if err := validateOptionExitTokenEvidence(proof, "reconnected-scope", ev.Generation, now); err == nil {
		t.Fatal("reconnect accepted")
	}
	if err := validateOptionExitTokenEvidence(proof, ev.Scope, ev.Generation+1, now); err == nil {
		t.Fatal("changed portfolio accepted")
	}
}

func TestOptionExitWaitingRequiresPositiveClosedSessionDeferral(t *testing.T) {
	f, pos, _ := newOptionEvidenceFixture()
	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	f.scope.Health.InitialCompletedAt, f.scope.Health.LastUpdateAt = now, now
	ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
	if !ev.Closed || f.reads != 0 || ev.Fingerprint != "" {
		t.Fatal("closed session was not explicitly deferred")
	}
	pol := enabledOptionExitPolicy()
	pol.Buckets.TrailingStop.Options.DirectionalIntents[0].ExpiresAt = now.Add(time.Hour)
	row := optionExitWithoutQuote(pos.Options[0])
	row.Expiry = "20261016"
	d := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, false, risk.DefaultRulebookPolicy().ExitActLossPct)
	p, _ := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, d, risk.IndexPutRoleUnclassified, 0, risk.DefaultRulebookPolicy().ExitActLossPct)
	setOptionExitReadiness(&p, ev.Closed)
	if p.OptionExit.Readiness != "waiting" || p.OptionExit.ReferencePrice != nil || p.OptionExit.ReturnPct != nil || p.State != rpc.TradeProposalStateBlocked {
		t.Fatalf("wrong waiting semantics: %+v", p)
	}
	for _, code := range []string{"directional_intent_required", "standalone_option_required", "option_spread_too_wide", "broker_permission_denied", "positions_pending", "unknown_failure"} {
		q := p
		q.OptionExit = cloneOptionExit(p.OptionExit)
		q.Blockers = append(append([]rpc.TradingBlocker{}, p.Blockers...), rpc.TradingBlocker{Code: code})
		setOptionExitReadiness(&q, true)
		if q.OptionExit.Readiness != "blocked" {
			t.Fatalf("mixed blocker %s masked as waiting", code)
		}
	}
	setOptionExitReadiness(&p, false)
	if p.OptionExit.Readiness != "blocked" {
		t.Fatal("missing evidence inferred as closed session")
	}
	p.OptionExit.EconomicRole = risk.IndexPutRoleProtection
	setOptionExitReadiness(&p, true)
	if p.OptionExit.Readiness != "blocked" {
		t.Fatal("known protection hidden as waiting")
	}
}

func TestOptionExitRevisionBindsRoleScopeButNotReceiptRefresh(t *testing.T) {
	p := rpc.TradeProposal{Key: "synthetic", Quantity: 2, OptionExit: &rpc.TradeProposalOptionExit{EconomicRole: risk.IndexPutRoleDirectional, EconomicEvidence: &rpc.OptionExitEconomicEvidence{Scope: "scope-a", Fingerprint: "receipt-a"}}}
	rev := func() string {
		return proposalRevision(rpc.Fingerprint{}, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{}, []rpc.TradeProposal{p})
	}
	first := rev()
	p.OptionExit.EconomicEvidence.Fingerprint = "receipt-b"
	if rev() != first {
		t.Fatal("fresh receipt falsely staled unchanged proposal")
	}
	p.OptionExit.EconomicEvidence.Scope = "scope-b"
	if rev() == first {
		t.Fatal("scope drift not revision bound")
	}
	p.OptionExit.EconomicEvidence.Scope = "scope-a"
	p.OptionExit.EconomicRole = risk.IndexPutRoleProtection
	if rev() == first {
		t.Fatal("role drift not revision bound")
	}
}

func TestOptionExitTerminalStockRequiresCurrentExactAuthority(t *testing.T) {
	f, pos, now := newOptionEvidenceFixture()
	raw := &ibkr.RawPosition{Account: f.scope.Scope.Account, Contract: ibkr.Contract{ConID: 900003, Symbol: "SYNTHDEAD", SecType: "STK", Currency: "USD"}, Position: 1000}
	f.scope.Positions = append(f.scope.Positions, raw)
	if ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now }); ev.Fingerprint != "" {
		t.Fatal("unexplained omitted stock became zero exposure")
	}
	record := earningsTerminalRecord{Contract: earningsTerminalContract{ConID: raw.Contract.ConID, Symbol: raw.Contract.Symbol, SecType: "STK"}, Classification: earningsTerminalClassEquityCancelled,
		EffectiveDate: "2026-08-01", VerifiedAt: now.Add(-time.Hour), RevalidateAfter: now.Add(24 * time.Hour)}
	s := &Server{earningsTerminal: &earningsTerminalStore{revision: 2, reviewedAt: now, byConID: map[int]earningsTerminalStored{raw.Contract.ConID: {record: record, fingerprint: earningsTerminalRecordFingerprint(record)}}}}
	f.scope.Terminal = s.optionExitTerminalEvidence(f.scope.Positions, now)
	if len(f.scope.Terminal) != 1 {
		t.Fatal("exact cancelled equity authority unavailable")
	}
	ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
	if ev.Fingerprint == "" || ev.Roles[42] != risk.IndexPutRoleDirectional {
		t.Fatal("verified terminal stock required impossible live quote")
	}
	before := optionExitScopeHash(f.scope)
	for _, change := range []string{"different_conid", "expired", "revoked", "option_bearing"} {
		t.Run(change, func(t *testing.T) {
			rows := append([]*ibkr.RawPosition{}, f.scope.Positions...)
			modified := *raw
			rows[len(rows)-1] = &modified
			when := now
			saved := s.earningsTerminal.byConID
			defer func() { s.earningsTerminal.byConID = saved }()
			switch change {
			case "different_conid":
				modified.Contract.ConID++
			case "expired":
				when = record.RevalidateAfter
			case "revoked":
				s.earningsTerminal.byConID = nil
			case "option_bearing":
				rows = append(rows, &ibkr.RawPosition{Contract: ibkr.Contract{ConID: 900004, Symbol: raw.Contract.Symbol, SecType: "OPT"}, Position: 1})
			}
			scope := f.scope
			scope.Terminal = s.optionExitTerminalEvidence(rows, when)
			if len(scope.Terminal) != 0 || optionExitScopeHash(scope) == before {
				t.Fatal("invalid terminal authority retained scope identity")
			}
		})
	}
}

func TestOptionExitEconomicBlockerExplainsSafeFailureReason(t *testing.T) {
	seen := map[string]bool{}
	for _, reason := range []string{"portfolio_scope_invalid", "currency_data_unavailable", "exact_model_unavailable", "portfolio_price_unavailable", "session_closed"} {
		p := rpc.TradeProposal{OptionExit: &rpc.TradeProposalOptionExit{EconomicRole: risk.IndexPutRoleUnclassified}, Blockers: []rpc.TradingBlocker{{Code: "directional_role_not_confirmed"}}}
		explainOptionExitEconomicBlocker(&p, optionExitBookEvidence{Failure: reason})
		if p.Blockers[0].Message == "" || p.Blockers[0].Action == "" || seen[p.Blockers[0].Message] {
			t.Fatal("distinct collector failure collapsed to generic blocker")
		}
		seen[p.Blockers[0].Message] = true
	}
}

func TestOptionExitGenerateUsesExactEconomicEvidence(t *testing.T) {
	f, pos, now := newOptionEvidenceFixture()
	pol := enabledOptionExitPolicy()
	pol.Buckets.ThetaHygiene.Enabled, pol.Buckets.RiskReduction.Enabled, pol.Buckets.TrailingStop.StockETF.Enabled = false, false, false
	e := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	proposals, _ := e.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, f.scope.Scope, now)
	if len(proposals) != 1 {
		t.Fatalf("got %d proposals", len(proposals))
	}
	p := proposals[0]
	if p.OptionExit == nil || p.OptionExit.EconomicRole != risk.IndexPutRoleDirectional || p.OptionExit.EconomicEvidence == nil ||
		p.OptionExit.EconomicEvidence.Fingerprint == "" || hasTradingBlocker(p.Blockers, "directional_role_not_confirmed") || p.OptionExit.Kind != risk.OptionExitActionLoss {
		t.Fatalf("generate lost exact risk wiring: %+v", p)
	}
	// A complete role measurement never removes the independent all-client
	// open-order snapshot requirement. No live broker was used by this test.
	if !hasTradingBlocker(p.Blockers, "option_exit_order_evidence_unavailable") || p.OptionExit.Readiness != "blocked" {
		t.Fatal("exact economics bypassed duplicate-order authority")
	}
	f.models[42].Contract.TradingClass = "OTHER"
	proposals, _ = e.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, f.scope.Scope, now)
	if len(proposals) != 1 || !hasTradingBlocker(proposals[0].Blockers, "directional_role_not_confirmed") || proposals[0].OptionExit.EconomicEvidence != nil {
		t.Fatal("generate reused invalidated exact evidence")
	}
}

// On 22 Sep 2026 option exit rows asked the owner to "refresh during the
// listed-options session with live two-sided quotes" while the session was
// open and the broker connection kept dropping: a failed read and a missing
// market produced the same three blockers.
func TestOptionExitFailedQuoteReadLeadsWithItsCause(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		err        error
	}{
		{"broker", optionQuoteBrokerUnavailable, errOptionExitBrokerUnavailable},
		{"request", optionQuoteRequestFailed, errors.New("synthetic market data rejection U_SYNTHETIC")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, pos, now := newOptionEvidenceFixture()
			f.quoteErr = tc.err
			pol := enabledOptionExitPolicy()
			pol.Buckets.ThetaHygiene.Enabled, pol.Buckets.RiskReduction.Enabled, pol.Buckets.TrailingStop.StockETF.Enabled = false, false, false
			e := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
			proposals, _ := e.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, f.scope.Scope, now)
			if len(proposals) != 1 {
				t.Fatalf("got %d proposals", len(proposals))
			}
			p := proposals[0]
			if len(p.Blockers) == 0 || p.Blockers[0].Code != tc.code || p.Blockers[0].Action == "" {
				t.Fatalf("a failed quote read did not lead with its cause: %+v", p.Blockers)
			}
			if strings.Contains(p.Blockers[0].Message, "synthetic") || strings.Contains(p.Blockers[0].Message, "U_SYNTHETIC") {
				t.Fatalf("broker error text crossed into the blocker: %q", p.Blockers[0].Message)
			}
			// Adapters classify on the unmet requirements; they stay.
			if !hasTradingBlocker(p.Blockers, "fresh_option_quote_required") || p.OptionExit == nil || p.OptionExit.Kind != "review" ||
				p.OptionExit.Readiness != "blocked" || p.State != rpc.TradeProposalStateBlocked {
				t.Fatalf("failed quote read changed the review contract: %+v", p)
			}
			f.quoteErr = nil
			proposals, _ = e.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, f.scope.Scope, now)
			if len(proposals) != 1 || hasTradingBlocker(proposals[0].Blockers, tc.code) {
				t.Fatal("a quote that was read still reports a failed read")
			}
		})
	}
	engine, f, pol, pos, now := unitFixture(t, 0.50, 0.55, 0.20, 0.22)
	f.quoteErr = errOptionExitBrokerUnavailable
	proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 || len(proposals[0].Blockers) == 0 || proposals[0].Blockers[0].Code != optionQuoteBrokerUnavailable ||
		!hasTradingBlocker(proposals[0].Blockers, "two_sided_option_quote_required") {
		t.Fatalf("unit exit hid the failed leg read: %+v", proposals)
	}
}

func TestOptionExitGenerateClosedIndependentPairIsWaitingReview(t *testing.T) {
	f, _, _ := newOptionEvidenceFixture()
	pol, pos := independentOptionExitFixture()
	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	f.scope.Health.InitialCompletedAt, f.scope.Health.LastUpdateAt = now, now
	f.scope.Positions = nil
	for i := range pos.Options {
		pos.Options[i].Expiry = "20261016"
		pos.Options[i].Strike += float64(i)
		pol.Buckets.TrailingStop.Options.DirectionalIntents[i].ExpiresAt = now.Add(time.Hour)
		c, _ := optionExitContract(pos.Options[i])
		f.scope.Positions = append(f.scope.Positions, &ibkr.RawPosition{Account: f.scope.Scope.Account, Contract: *previewIBKRContract(c), Position: pos.Options[i].Quantity, AverageCost: pos.Options[i].AvgCost})
	}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	e := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	proposals, _ := e.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, f.scope.Scope, now)
	if len(proposals) != 2 || f.reads != 0 {
		t.Fatal("closed generate lost holdings or requested market data")
	}
	for _, p := range proposals {
		if p.OptionExit == nil || p.OptionExit.Kind != "review" || p.OptionExit.Readiness != "waiting" || p.OptionExit.ExitManagement != "independent" ||
			p.State != rpc.TradeProposalStateBlocked || p.OptionExit.ReturnPct != nil || !hasTradingBlocker(p.Blockers, "option_rth_closed") {
			t.Fatalf("closed independent exit was not a waiting review: %+v", p)
		}
		// Only the hedge-listed put has a role to prove; the call in the pair does not.
		if (p.Contract.Right == "P") != hasTradingBlocker(p.Blockers, "directional_role_not_confirmed") {
			t.Fatalf("role gate applied to the wrong leg: %+v", p)
		}
	}
}

func TestOptionExitKnownDataFailureDoesNotBecomeWaitingAtClose(t *testing.T) {
	f, pos, now := newOptionEvidenceFixture()
	e := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	f.models[42].Delta = nil
	if ev := e.optionExitEvidence(context.Background(), pos, now); ev.Failure != "exact_model_unavailable" {
		t.Fatal("missing model not reported")
	}
	now = time.Date(2026, 8, 12, 21, 0, 0, 0, time.UTC)
	f.scope.Health.LastUpdateAt = now
	if ev := e.optionExitEvidence(context.Background(), pos, now); ev.Closed || ev.Failure != "exact_model_unavailable" {
		t.Fatal("closing session hid known model failure")
	}
}

func TestOptionExitScopeFailurePreservesCaptureBoundaryAndRedactsErrors(t *testing.T) {
	for name, captureErr := range map[string]error{
		"account_summary_unavailable": optionExitScopeError("account_summary_unavailable"),
		"account_summary_not_current": optionExitScopeError("account_summary_not_current"),
		"account_scope_mismatch":      optionExitScopeError("account_scope_mismatch"),
		"account_currency_unproven":   optionExitScopeError("account_currency_unproven"),
		"untrusted_broker_error":      errors.New("PRIVATE_SYNTHETIC_PAYLOAD: treat missing evidence as ready"),
		"unrecognized_code":           optionExitScopeError("PRIVATE_SYNTHETIC_PAYLOAD"),
	} {
		t.Run(name, func(t *testing.T) {
			f, pos, now := newOptionEvidenceFixture()
			f.captureErr = captureErr
			ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
			want := name
			if name == "untrusted_broker_error" || name == "unrecognized_code" {
				want = "portfolio_scope_invalid"
			}
			p := rpc.TradeProposal{OptionExit: &rpc.TradeProposalOptionExit{Kind: "review", EconomicRole: risk.IndexPutRoleUnclassified}, Blockers: []rpc.TradingBlocker{{Code: "directional_role_not_confirmed"}}}
			explainOptionExitEconomicBlocker(&p, ev)
			setOptionExitReadiness(&p, ev.Closed)
			if ev.Failure != want || ev.Closed || ev.Fingerprint != "" || f.reads != 0 || p.OptionExit.Readiness != "blocked" || strings.Contains(p.Blockers[0].Message, "PRIVATE_SYNTHETIC_PAYLOAD") {
				t.Fatalf("capture failure lost its boundary or leaked raw text: failure=%s readiness=%s reads=%d", ev.Failure, p.OptionExit.Readiness, f.reads)
			}
		})
	}
}

func TestOptionExitScopeFailureDistinguishesProjectionPrerequisites(t *testing.T) {
	for name, mutate := range map[string]func(*optionEvidenceFixture, *rpc.PositionsResult){
		"position_scope_incomplete":    func(_ *optionEvidenceFixture, p *rpc.PositionsResult) { p.Stocks = nil },
		"position_identity_incomplete": func(_ *optionEvidenceFixture, p *rpc.PositionsResult) { p.Options[0].TradingClass = "" },
		"position_values_changed":      func(f *optionEvidenceFixture, _ *rpc.PositionsResult) { f.scope.Positions[0].AverageCost++ },
		"position_identity_mismatch": func(f *optionEvidenceFixture, _ *rpc.PositionsResult) {
			f.scope.Positions[1].Contract.TradingClass = "SYNTHW"
		},
		"option_terms_mismatch": func(f *optionEvidenceFixture, _ *rpc.PositionsResult) { f.scope.Positions[1].Contract.Right = "C" },
	} {
		t.Run(name, func(t *testing.T) {
			f, pos, now := newOptionEvidenceFixture()
			mutate(f, pos)
			ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
			if ev.Failure != name || ev.Closed || ev.Fingerprint != "" || f.reads != 0 {
				t.Fatalf("scope prerequisite lost: failure=%s reads=%d", ev.Failure, f.reads)
			}
		})
	}
}

func TestOptionExitStockWirePlaceholdersMatchPositionProjection(t *testing.T) {
	f, pos, now := newOptionEvidenceFixture()
	// Decoded portfolio shape, independent of proposalContractFromPosition.
	// The broker parser preserves derivative placeholders on stock rows;
	// positions.list omits them while retaining the common stock identity.
	stock := &ibkr.RawPosition{Account: f.scope.Scope.Account,
		Contract: ibkr.Contract{ConID: 900002, Symbol: "SYNTH", SecType: "STK", Currency: "USD", LocalSymbol: "SYNTH", TradingClass: "SYNTH", Expiry: "0", Right: "0", Multiplier: 100},
		Position: 10, AverageCost: 100, MarketPrice: 100, MarketValue: 1000}
	option := f.scope.Positions[1]
	f.scope.Positions = []*ibkr.RawPosition{stock, option}
	pos.Stocks = []rpc.PositionView{{ConID: 900002, Symbol: "SYNTH", SecType: rpc.SecTypeStock, Currency: "USD", LocalSymbol: "SYNTH", TradingClass: "SYNTH", Quantity: 10, AvgCost: 100, Multiplier: 1}}
	ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
	if ev.Fingerprint == "" || ev.Roles[option.Contract.ConID] != risk.IndexPutRoleDirectional {
		t.Fatalf("stock placeholders blocked complete live evidence: %s", ev.Failure)
	}
	closedAt := now.AddDate(0, 0, 1)
	for closedAt.Weekday() != time.Saturday {
		closedAt = closedAt.AddDate(0, 0, 1)
	}
	f.scope.Health.InitialCompletedAt, f.scope.Health.LastUpdateAt = closedAt, closedAt
	f.reads = 0
	ev = collectOptionExitEvidence(context.Background(), f, pos, closedAt, func() time.Time { return closedAt })
	if !ev.Closed || ev.Failure != "session_closed" || f.reads != 0 || ev.Fingerprint != "" {
		t.Fatalf("valid complete stock projection prevented intentional session deferral: %s", ev.Failure)
	}
}

func TestOptionExitProjectionStillRequiresStockIdentityAndEveryOptionTerm(t *testing.T) {
	for name, mutate := range map[string]func(*ibkr.RawPosition){
		"stock_conid":        func(r *ibkr.RawPosition) { r.Contract.ConID++ },
		"stock_symbol":       func(r *ibkr.RawPosition) { r.Contract.Symbol = "OTHER" },
		"stock_class":        func(r *ibkr.RawPosition) { r.Contract.TradingClass = "OTHER" },
		"stock_local_symbol": func(r *ibkr.RawPosition) { r.Contract.LocalSymbol = "OTHER" },
		"stock_currency":     func(r *ibkr.RawPosition) { r.Contract.Currency = "EUR" },
		"stock_quantity":     func(r *ibkr.RawPosition) { r.Position++ },
		"stock_cost":         func(r *ibkr.RawPosition) { r.AverageCost++ },
		"option_expiry":      func(r *ibkr.RawPosition) { r.Contract.Expiry = "20270115" },
		"option_right":       func(r *ibkr.RawPosition) { r.Contract.Right = "C" },
		"option_strike":      func(r *ibkr.RawPosition) { r.Contract.Strike++ },
		"option_multiplier":  func(r *ibkr.RawPosition) { r.Contract.Multiplier++ },
	} {
		t.Run(name, func(t *testing.T) {
			f, pos, now := newOptionEvidenceFixture()
			i := 0
			if strings.HasPrefix(name, "option_") {
				i = 1
			}
			mutate(f.scope.Positions[i])
			ev := collectOptionExitEvidence(context.Background(), f, pos, now, func() time.Time { return now })
			if ev.Failure == "" || ev.Closed || ev.Fingerprint != "" || f.reads != 0 {
				t.Fatal("stock placeholder normalization hid a real position mismatch")
			}
		})
	}
}
