package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func portfolioActionFixture() *rpc.PortfolioPlanResult {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	return &rpc.PortfolioPlanResult{AsOf: now, BaseCurrency: "USD", Regime: risk.RegimeBucketCalm, RegimeAsOf: now, PolicyFingerprint: "synthetic-policy", Targets: []risk.PortfolioIntent{
		{ConID: 101, Symbol: "SYNA", Currency: "USD", Priority: 1, Decision: "add", QuantityBefore: new(10.), TargetPctNLV: new(5.), LimitPrice: new(100.), DesiredQuantity: 20},
		{ConID: 102, Symbol: "SYNB", Currency: "USD", Priority: 2, Decision: "add", QuantityBefore: new(0.), TargetPctNLV: new(5.), LimitPrice: new(50.), DesiredQuantity: 40},
	}}
}

func portfolioCheckedAdd(out *rpc.PortfolioPlanResult, p rpc.AddParams, q int) *rpc.AddPlanResult {
	return &rpc.AddPlanResult{Contract: p.Contract, LimitPrice: p.LimitPrice, Currency: "USD", BaseCurrency: "USD", Review: &rpc.AddReview{PolicyFingerprint: out.PolicyFingerprint, RegimeStage: out.Regime, RegimeAsOf: out.RegimeAsOf}, Quantity: q, Before: 10, After: 10 + float64(q), UnderlyingPctAfter: 4, MaxQuantity: q, MaximumKnown: p.Max, Protection: &risk.StockAddProtection{}, Blockers: []risk.StockAddBlocker{}}
}

func TestPortfolioPlanSizesOnlyOneTargetAndRequotesTheSmallerQuantity(t *testing.T) {
	out := portfolioActionFixture()
	var calls []rpc.AddParams
	selectPortfolioAction(t.Context(), out, rpc.TradeProposalSnapshot{}, func(_ context.Context, p rpc.AddParams) (*rpc.AddPlanResult, error) {
		calls = append(calls, p)
		q := 25
		if !p.Max {
			q = p.Quantity
		}
		return portfolioCheckedAdd(out, p, q), nil
	})
	if len(calls) != 2 || calls[0].Contract.ConID != 101 || !calls[0].Max || calls[1].Max || calls[1].Quantity != 20 || out.Next == nil || out.Next.Add.Quantity != 20 || out.Targets[1].Decision != "wait_for_replan" {
		t.Fatalf("oversized or duplicated allocation: calls=%+v out=%+v", calls, out)
	}
	raw, _ := json.Marshal(out)
	for _, field := range []string{"preview_token", "confirmation", "authorization", "reserved_cash"} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("plan minted authority: %s", field)
		}
	}
}

func TestPortfolioPlanDoesNotReuseFeesOrIgnorePendingPurchases(t *testing.T) {
	for _, reason := range []string{"pending", "incomplete maximum", "smaller fee rejected", "changed position", "changed policy", "changed regime", "wrong listing", "changed FX", "changed base"} {
		t.Run(reason, func(t *testing.T) {
			out := portfolioActionFixture()
			calls := 0
			selectPortfolioAction(t.Context(), out, rpc.TradeProposalSnapshot{}, func(_ context.Context, p rpc.AddParams) (*rpc.AddPlanResult, error) {
				calls++
				q := 25
				if !p.Max {
					q = p.Quantity
				}
				a := portfolioCheckedAdd(out, p, q)
				switch reason {
				case "pending":
					a.Protection.PendingQuantity = 3
				case "incomplete maximum":
					a.MaximumKnown = false
				case "smaller fee rejected":
					if !p.Max {
						a.Blockers = []risk.StockAddBlocker{{Code: "fee", Message: "Exact fee exceeds allowance"}}
					}
				case "changed position":
					a.Before = 12
				case "changed policy":
					a.Review.PolicyFingerprint = "changed"
				case "changed regime":
					a.Review.RegimeStage = risk.RegimeBucketConfirmed
				case "wrong listing":
					a.Contract.ConID = 999
				case "changed FX":
					a.UnderlyingPctAfter = 5.01
				case "changed base":
					a.BaseCurrency = "EUR"
				}
				return a, nil
			})
			if out.State != "held" || out.Next != nil || len(out.Blockers) == 0 || calls > 2 {
				t.Fatalf("unsafe plan: %+v", out)
			}
		})
	}
}

func TestPortfolioPlanPreservesReductionScopeAndNeverCreditsProceeds(t *testing.T) {
	for _, state := range []string{"reduce", "stop", "blocked", "shadow", "queued", "automatic"} {
		t.Run(state, func(t *testing.T) {
			out := portfolioActionFixture()
			p := rpc.TradeProposal{Key: "synthetic-exact-row", Revision: "r1", PositionEffect: rpc.OrderPositionEffectReduce, State: rpc.TradeProposalStateGenerated, Quantity: 2, Action: "SELL", OrderType: "LMT", Contract: rpc.ContractParams{ConID: 999, Symbol: "SYNX", SecType: "OPT"}}
			switch state {
			case "stop":
				p.OrderType = "STP"
			case "blocked":
				p.Blockers = []rpc.TradingBlocker{{Code: "quote", Message: "Quote unavailable"}}
			case "shadow":
				p.Shadow = true
			case "queued":
				p.Queued = &rpc.TradeProposalQueued{}
			case "automatic":
				p.Automatic = &rpc.TradeProposalAutomatic{PreAuthorised: true}
			}
			snap := rpc.TradeProposalSnapshot{Revision: "book-r1", Proposals: []rpc.TradeProposal{p, p}}
			selectPortfolioAction(t.Context(), out, snap, func(context.Context, rpc.AddParams) (*rpc.AddPlanResult, error) {
				t.Fatal("sized a buy before reduction outcome")
				return nil, nil
			})
			if len(out.Reductions) != 2 {
				t.Fatal("later reduction context hidden")
			}
			if state == "reduce" || state == "stop" {
				if out.Next == nil || out.Next.ProposalKey != p.Key || out.Next.ProposalRevision != p.Revision || out.Next.Add != nil {
					t.Fatalf("scope lost: %+v", out)
				}
				if state == "stop" && out.Next.Kind != "protection" {
					t.Fatal("stop classified as immediate sale")
				}
			} else if out.Next != nil || out.State != "held" {
				t.Fatalf("blocked reduction admitted addition: %+v", out)
			}
		})
	}
}

func TestPortfolioPlanRequiresExactBookAndOriginalSessionEvidence(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, change := range []string{"none", "refresh time", "quantity", "cash", "missing authority", "session", "expired", "future", "loaded", "partial", "missing fingerprint", "policy"} {
		t.Run(change, func(t *testing.T) {
			auth := &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent, PortfolioComplete: true, Scope: rpc.AccountDataScope{AccountID: "SYNTHETIC", AccountMode: "paper"}, BrokerReadSession: rpc.BrokerReadSession{DaemonStartedAt: now.Add(-time.Hour), ConnectorGeneration: 2, SocketEpoch: 3}}
			a := &rpc.AccountResult{AccountID: "SYNTHETIC", BaseCurrency: "USD", NetLiquidation: 100000, TotalCash: 20000, AsOf: now, Authority: auth}
			p := &rpc.PositionsResult{AsOf: now, Authority: auth, Stocks: []rpc.PositionView{{ConID: 101, Symbol: "SYNA", SecType: "STK", Currency: "USD", Quantity: 10, Mark: 100}}}
			copyAuth := *auth
			s := rpc.TradeProposalSnapshot{AccountID: "SYNTHETIC", AccountMode: "paper", PlanningAuthority: &copyAuth, PlanningEvidence: portfolioPlanBookFingerprint(a, p), PlanningPolicyFingerprint: "policy", AsOf: now}
			switch change {
			case "refresh time":
				a.AsOf, p.AsOf = now.Add(time.Second), now.Add(time.Second)
			case "quantity":
				p.Stocks[0].Quantity++
			case "cash":
				a.TotalCash++
			case "missing authority":
				s.PlanningAuthority = nil
			case "session":
				s.PlanningAuthority.BrokerReadSession.SocketEpoch++
			case "expired":
				s.AsOf = now.Add(-time.Minute)
			case "future":
				s.AsOf = now.Add(time.Second)
			case "loaded":
				s.LoadedFromState = true
			case "partial":
				s.PlanningAuthority.PortfolioComplete = false
			case "missing fingerprint":
				s.PlanningEvidence = ""
			case "policy":
				s.PlanningPolicyFingerprint = "earlier policy or regime"
			}
			got := portfolioProposalEvidenceCurrent(s, a, p, now, 30*time.Second, "policy")
			if got != (change == "none" || change == "refresh time") {
				t.Fatalf("%s current = %v", change, got)
			}
		})
	}
}

func TestPortfolioPlanReadClocksDoNotInvalidateUnchangedFinancialEvidence(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	a := &rpc.AccountResult{NetLiquidation: 100000, StreamObservation: &rpc.AccountStreamObservation{ReadAt: now, LastCallbackAt: now.Add(-time.Second)}, DailyPnLObservation: &rpc.DailyPnLObservation{Status: rpc.DailyPnLObservationNotDue, AsOf: now}}
	p := &rpc.PositionsResult{Stocks: []rpc.PositionView{{ConID: 101, Quantity: 10, Mark: 100, PriceAt: now.Add(-time.Second)}}}
	before := portfolioPlanBookFingerprint(a, p)
	a.StreamObservation.ReadAt = now.Add(time.Second)
	a.DailyPnLObservation.AsOf = now.Add(time.Second)
	if portfolioPlanBookFingerprint(a, p) != before {
		t.Fatal("query clocks changed the financial book fingerprint")
	}
	if !a.StreamObservation.ReadAt.Equal(now.Add(time.Second)) {
		t.Fatal("hashing mutated producer evidence")
	}
	a.StreamObservation.LastCallbackAt = now
	if portfolioPlanBookFingerprint(a, p) == before {
		t.Fatal("original producer receipt changes were ignored")
	}
}

func TestPortfolioPlanNoPolicyNeedsNoBrokerAndRejectsCallerAuthority(t *testing.T) {
	s := &Server{}
	for _, raw := range []string{`{"submit":true}`, `{"targets":[]}`, `{"positions":[]}`, `{} {}`} {
		if _, err := s.handlePortfolioPlan(t.Context(), &rpc.Request{Params: json.RawMessage(raw)}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	out, err := s.handlePortfolioPlan(t.Context(), &rpc.Request{})
	if err != nil || out.Next != nil || len(out.Blockers) != 1 || out.Blockers[0].Code != "portfolio_policy_missing" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestPortfolioPlanReadCannotWakeAutomaticExecutor(t *testing.T) {
	e := &proposalEngine{now: time.Now, kick: make(chan struct{}, 1), mergedAutomatic: map[string]bool{"synthetic": true}, scope: func() brokerStateScope { return brokerStateScope{Account: "SYNTHETIC", Mode: "paper"} }, snapshot: rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, AccountID: "SYNTHETIC", AccountMode: "paper", Proposals: []rpc.TradeProposal{{Key: "synthetic", Revision: "r1"}}}}
	_ = e.snapshotForRead(false, false)
	if len(e.kick) != 0 {
		t.Fatal("read-only plan woke the executor")
	}
	_ = e.Snapshot(false)
	if len(e.kick) != 1 {
		t.Fatal("test did not exercise an obsolete coverage generation")
	}
}

func TestPortfolioPlanPendingSweepCannotCompeteForTheSameCash(t *testing.T) {
	out := portfolioActionFixture()
	p := rpc.TradeProposal{Key: "synthetic-sweep", Revision: "r1", Action: "BUY", PositionEffect: "open", Automatic: &rpc.TradeProposalAutomatic{State: rpc.TradeProposalAutomaticPending}}
	snap := rpc.TradeProposalSnapshot{Proposals: []rpc.TradeProposal{p}}
	selectPortfolioAction(t.Context(), out, snap, func(context.Context, rpc.AddParams) (*rpc.AddPlanResult, error) {
		t.Fatal("sized against an automatic pending purchase")
		return nil, nil
	})
	if out.Next != nil || out.State != "held" {
		t.Fatal("pending purchase ignored")
	}
	before := portfolioPlanProposalFingerprint(snap)
	snap.Proposals[0].Automatic = &rpc.TradeProposalAutomatic{State: rpc.TradeProposalAutomaticSubmitting}
	if portfolioPlanProposalFingerprint(snap) == before {
		t.Fatal("executor state changed without invalidating the plan")
	}
}

func TestPortfolioTargetUniverseNeverTurnsAnUnresolvedOrConflictingWatchIntoZero(t *testing.T) {
	p := &risk.PortfolioPlanPolicy{Targets: []risk.PortfolioPlanTarget{{Symbol: "SYNA", ConID: 101, Currency: "USD"}}}
	a := &rpc.AccountResult{BaseCurrency: "USD"}
	positions := &rpc.PositionsResult{Stocks: []rpc.PositionView{{ConID: 101, Symbol: "SYNA", SecType: "STK", Currency: "USD", Quantity: 10, Mark: 100}}}
	w := &rpc.Watchlist{Symbols: []rpc.WatchlistContract{{ConID: 101, Symbol: "SYNA", SecType: "STK", Currency: "USD"}, {Symbol: "SYNB", SecType: "STK", Currency: "USD"}}}
	e, u := portfolioTargetEvidence(p, a, positions, w, time.Now())
	if len(e) != 2 || e[0].Quantity != 10 || e[1].Complete || len(u) != 1 {
		t.Fatalf("%+v %+v", e, u)
	}
	w.Symbols[0].ConID = 999
	e, _ = portfolioTargetEvidence(p, a, positions, w, time.Now())
	if len(e) != 3 {
		t.Fatal("conflicting listing was silently combined")
	}
}
