package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Owner report 2026-09-28: "the reduction of one call pops up twice, for
// different reasons but same action." These witnesses cover the merge and
// the reduction netting in proposal_same_contract.go. Every book is
// synthetic.

// sameContractInventory seams the broker's complete, current open-order
// inventory (Server.brokerOpenOrderInventory) to orders in the synthetic
// account.
func sameContractInventory(orders ...ibkrlib.OrderLifecycleEvent) func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
	return func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: optionExitTestTime(), Orders: orders}, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModeLive}, nil
	}
}

// unavailableInventory is an inventory the broker could not supply complete
// and current.
func unavailableInventory(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
	return ibkrlib.OpenOrderSnapshot{}, brokerStateScope{}, errBrokerOpenOrderInventoryUnavailable
}

// workingOrder is an order still working at the broker for qty of conID.
func workingOrder(conID int, secType, action string, qty float64, orderType string) ibkrlib.OrderLifecycleEvent {
	return ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, ConID: conID, SecType: secType, Action: action, OrderType: orderType, TotalQuantity: qty, Remaining: qty}
}

// approvableFor lists the rows for conID that a human could approve or
// Canary could place: generated, unblocked, not shadow.
func approvableFor(rows []rpc.TradeProposal, conID int) []rpc.TradeProposal {
	var out []rpc.TradeProposal
	for _, p := range rows {
		if p.Contract.ConID == conID && p.State == rpc.TradeProposalStateGenerated && len(p.Blockers) == 0 && !p.Shadow {
			out = append(out, p)
		}
	}
	return out
}

func rowsByBucket(t *testing.T, rows []rpc.TradeProposal, buckets ...string) map[string]rpc.TradeProposal {
	t.Helper()
	out := map[string]rpc.TradeProposal{}
	for _, p := range rows {
		out[p.Bucket] = p
	}
	for _, b := range buckets {
		if _, ok := out[b]; !ok {
			t.Fatalf("no %s row in %+v", b, rows)
		}
	}
	return out
}

func blockerCodes(p rpc.TradeProposal) []string {
	var out []string
	for _, b := range p.Blockers {
		out = append(out, b.Code)
	}
	return out
}

// lossExitThetaFixture is the owner's case: one standing directional long
// call (two contracts, cost 1.00 per share) 18 days from expiry and out of
// the money, its fresh bid 72% below cost. The Rulebook's loss exit and
// theta hygiene both close it in full. The broker has no working order.
func lossExitThetaFixture(t *testing.T) (*proposalEngine, protectionPolicy, *rpc.PositionsResult, time.Time) {
	t.Helper()
	engine, _, pos, now := expiryCallFixture(t, 18, 90, 0.28, 0.30)
	row := &pos.Options[0]
	row.Mark, row.Theta, row.Underlying = 0.29, new(-0.03), new(90.0)
	row.OptionBid, row.OptionAsk = new(0.28), new(0.30)
	policy := standingOptionExitPolicy()
	policy.Buckets.ThetaHygiene.Enabled = true
	engine.server.openOrderInventoryForTest = sameContractInventory()
	return engine, policy, pos, now
}

// budgetThetaFixture: one long call, five contracts at 2,000 EUR each, 18
// days from expiry and out of the money, against 50,000 EUR declared risk
// capital with a 15% per-line cap. The governor sells 2 to keep 3; theta
// hygiene closes all 5. The broker has no working order.
func budgetThetaFixture() (*proposalEngine, protectionPolicy, *rpc.PositionsResult, time.Time) {
	now := optionExitTestTime()
	leg := budgetOptionLeg("AAA", 601, "C", 5, 2000, -1000)
	leg.Expiry = now.AddDate(0, 0, 18).Format("20060102")
	leg.LocalSymbol = "AAA   " + leg.Expiry[2:] + "C00100000"
	leg.Theta, leg.Underlying = new(-0.5), new(95.0)
	leg.OptionBid, leg.OptionAsk = new(19.8), new(20.2)
	pos := &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"}, Options: []rpc.PositionView{leg}}
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 40, 15)
	policy.Buckets.ThetaHygiene.Enabled = true
	engine := &proposalEngine{server: withTestOrderLimits(&Server{openOrderInventoryForTest: sameContractInventory()}), now: func() time.Time { return now },
		budgetInput: func(*rpc.AccountResult, time.Time) budgetGovernorInput { return budgetLatchedInput() }}
	return engine, policy, pos, now
}

func generateSameContract(t *testing.T, engine *proposalEngine, policy protectionPolicy, pos *rpc.PositionsResult, now time.Time) []rpc.TradeProposal {
	t.Helper()
	rows, _ := engine.generate(context.Background(), policy, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	return rows
}

func TestSameContractLossExitAndThetaShowOneCandidate(t *testing.T) {
	engine, policy, pos, now := lossExitThetaFixture(t)
	rows := generateSameContract(t, engine, policy, pos, now)
	by := rowsByBucket(t, rows, rpc.TradeProposalBucketOptionLossExit, rpc.TradeProposalBucketThetaHygiene)
	loss, theta := by[rpc.TradeProposalBucketOptionLossExit], by[rpc.TradeProposalBucketThetaHygiene]

	got := approvableFor(rows, 42)
	if len(got) != 1 || got[0].Key != loss.Key {
		t.Fatalf("approvable rows for the call = %d (%+v), want the loss exit alone", len(got), got)
	}
	// Both close the whole position: the one order sells 2, never 4.
	if loss.Action != rpc.OrderActionSell || loss.Quantity != 2 || loss.PositionEffect != rpc.OrderPositionEffectClose || loss.OptionExit.Readiness != "ready" {
		t.Fatalf("candidate = %+v", loss)
	}
	if len(loss.Covers) != 1 || loss.Covers[0] != (rpc.TradeProposalCoverage{Bucket: rpc.TradeProposalBucketThetaHygiene, Key: theta.Key, Quantity: 2, OrderType: rpc.OrderTypeLMT, Reason: theta.Reason}) {
		t.Fatalf("candidate does not carry theta's reason: %+v", loss.Covers)
	}
	if theta.State != rpc.TradeProposalStateBlocked || theta.CoveredBy != loss.Key || len(theta.Blockers) != 1 || theta.Blockers[0].Code != "covered_by_proposal" ||
		!strings.Contains(theta.Blockers[0].Message, loss.Key) || theta.Blockers[0].Action == "" {
		t.Fatalf("theta row is not covered by the loss exit: %+v", theta)
	}
	// Desk and the companion read these names.
	raw, err := json.Marshal([]rpc.TradeProposal{loss, theta})
	if err != nil || !strings.Contains(string(raw), `"covers":[{"bucket":"theta_hygiene"`) || !strings.Contains(string(raw), `"covered_by":"`+loss.Key+`"`) {
		t.Fatalf("wire names changed: %s (%v)", raw, err)
	}
}

func TestSameContractSizeIsTheLargestRequirementNeverTheSum(t *testing.T) {
	engine, policy, pos, now := budgetThetaFixture()
	rows := generateSameContract(t, engine, policy, pos, now)
	by := rowsByBucket(t, rows, rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketThetaHygiene)
	budget, theta := by[rpc.TradeProposalBucketBudgetReduction], by[rpc.TradeProposalBucketThetaHygiene]
	if budget.Quantity != 2 || budget.PositionEffect != rpc.OrderPositionEffectReduce {
		t.Fatalf("fixture: governor sells %d (%s), want 2 of 5", budget.Quantity, budget.PositionEffect)
	}
	got := approvableFor(rows, 601)
	if len(got) != 1 || got[0].Key != theta.Key || got[0].Quantity != 5 || got[0].PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatalf("approvable rows = %+v, want theta's close of 5 (the larger requirement, not 7)", got)
	}
	if len(theta.Covers) != 1 || theta.Covers[0].Bucket != rpc.TradeProposalBucketBudgetReduction || theta.Covers[0].Quantity != 2 || theta.Covers[0].Reason != budget.Reason {
		t.Fatalf("candidate does not carry the governor's reason: %+v", theta.Covers)
	}
	if budget.CoveredBy != theta.Key || !hasTradingBlocker(budget.Blockers, "covered_by_proposal") {
		t.Fatalf("governor row is still actionable: %+v", budget)
	}

	t.Run("the largest wins whatever the bucket", func(t *testing.T) {
		rows := []rpc.TradeProposal{
			sameContractRow(rpc.TradeProposalBucketThetaHygiene, 7, 10, rpc.OrderTypeLMT),
			sameContractRow(rpc.TradeProposalBucketBudgetReduction, 7, 2, rpc.OrderTypeLMT),
			sameContractRow(rpc.TradeProposalBucketRiskReduction, 7, 4, rpc.OrderTypeLMT),
		}
		// Theta's own spread gate refuses it, so it neither covers nor is covered.
		rows[0].State, rows[0].Blockers = rpc.TradeProposalStateBlocked, []rpc.TradingBlocker{{Code: "wide_spread"}}
		mergeSameContractProposals(rows, nil)
		got := approvableFor(rows, 7)
		if len(got) != 1 || got[0].Bucket != rpc.TradeProposalBucketRiskReduction || got[0].Quantity != 4 || len(got[0].Covers) != 1 || got[0].Covers[0].Bucket != rpc.TradeProposalBucketBudgetReduction {
			t.Fatalf("approvable = %+v", got)
		}
		if rows[0].CoveredBy != "" || len(rows[0].Blockers) != 1 {
			t.Fatalf("a blocked row joined the merge: %+v", rows[0])
		}
	})
	t.Run("a tie goes to the Rulebook's exit", func(t *testing.T) {
		rows := []rpc.TradeProposal{
			sameContractRow(rpc.TradeProposalBucketThetaHygiene, 7, 3, rpc.OrderTypeLMT),
			sameContractRow(rpc.TradeProposalBucketOptionLossExit, 7, 3, rpc.OrderTypeLMT),
		}
		mergeSameContractProposals(rows, nil)
		if got := approvableFor(rows, 7); len(got) != 1 || got[0].Bucket != rpc.TradeProposalBucketOptionLossExit {
			t.Fatalf("approvable = %+v", got)
		}
	})
	t.Run("other contracts and sides stay apart", func(t *testing.T) {
		rows := []rpc.TradeProposal{
			sameContractRow(rpc.TradeProposalBucketThetaHygiene, 7, 3, rpc.OrderTypeLMT),
			sameContractRow(rpc.TradeProposalBucketBudgetReduction, 8, 2, rpc.OrderTypeLMT),
			sameContractRow(rpc.TradeProposalBucketRiskReduction, 7, 1, rpc.OrderTypeLMT),
		}
		rows[2].Action = rpc.OrderActionBuy
		mergeSameContractProposals(rows, nil)
		for _, p := range rows {
			if len(p.Blockers) != 0 || len(p.Covers) != 0 {
				t.Fatalf("rows for different contracts or sides merged: %+v", rows)
			}
		}
	})
}

func TestSameContractAuthorisingTheCandidateLeavesNoSecondApprovableRow(t *testing.T) {
	t.Run("loss exit and theta", func(t *testing.T) {
		engine, policy, pos, now := lossExitThetaFixture(t)
		before := rowsByBucket(t, generateSameContract(t, engine, policy, pos, now), rpc.TradeProposalBucketOptionLossExit, rpc.TradeProposalBucketThetaHygiene)
		// The owner authorises the one candidate; its order now works at the
		// broker and the position is unchanged until it fills.
		engine.server.openOrderInventoryForTest = sameContractInventory(workingOrder(42, "OPT", rpc.OrderActionSell, 2, rpc.OrderTypeLMT))
		rows := generateSameContract(t, engine, policy, pos, now)
		if got := approvableFor(rows, 42); len(got) != 0 {
			t.Fatalf("after authorising the loss exit, %d rows are still approvable: %+v", len(got), got)
		}
		by := rowsByBucket(t, rows, rpc.TradeProposalBucketOptionLossExit, rpc.TradeProposalBucketThetaHygiene)
		if !hasTradingBlocker(by[rpc.TradeProposalBucketThetaHygiene].Blockers, "existing_reduction_order") || !hasTradingBlocker(by[rpc.TradeProposalBucketOptionLossExit].Blockers, "existing_option_exit_order") {
			t.Fatalf("rows do not name the working order: theta %v, loss exit %v", blockerCodes(by[rpc.TradeProposalBucketThetaHygiene]), blockerCodes(by[rpc.TradeProposalBucketOptionLossExit]))
		}
		// A client still holding the earlier theta row is refused at preview and
		// submit, which read the broker afresh.
		if b := engine.duplicateProtectiveBlockers(context.Background(), before[rpc.TradeProposalBucketThetaHygiene]); !hasTradingBlocker(b, "existing_reduction_order") {
			t.Fatalf("submit-time netting let a second sale through: %+v", b)
		}
	})
	t.Run("theta and the budget governor", func(t *testing.T) {
		engine, policy, pos, now := budgetThetaFixture()
		before := rowsByBucket(t, generateSameContract(t, engine, policy, pos, now), rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketThetaHygiene)
		engine.server.openOrderInventoryForTest = sameContractInventory(workingOrder(601, "OPT", rpc.OrderActionSell, 5, rpc.OrderTypeLMT))
		rows := generateSameContract(t, engine, policy, pos, now)
		if got := approvableFor(rows, 601); len(got) != 0 {
			t.Fatalf("after authorising theta's close, %d rows are still approvable: %+v", len(got), got)
		}
		for _, p := range rows {
			if !hasTradingBlocker(p.Blockers, "existing_reduction_order") {
				t.Fatalf("%s row does not name the working order: %v", p.Bucket, blockerCodes(p))
			}
		}
		if b := engine.duplicateProtectiveBlockers(context.Background(), before[rpc.TradeProposalBucketBudgetReduction]); !hasTradingBlocker(b, "existing_reduction_order") {
			t.Fatalf("submit-time netting let the governor's sale through: %+v", b)
		}
	})
}

func TestSameContractWorkingOrderBlocksReductionRows(t *testing.T) {
	engine, policy, pos, now := budgetThetaFixture()
	other := workingOrder(602, "OPT", rpc.OrderActionSell, 5, rpc.OrderTypeLMT)
	unknown := workingOrder(0, "OPT", rpc.OrderActionSell, 1, rpc.OrderTypeLMT)
	unknown.Symbol = "AAA"
	filled := workingOrder(601, "OPT", rpc.OrderActionSell, 5, rpc.OrderTypeLMT)
	filled.Remaining, filled.Filled = 0, 5
	withStatus := func(status string) ibkrlib.OrderLifecycleEvent {
		o := workingOrder(601, "OPT", rpc.OrderActionSell, 5, rpc.OrderTypeLMT)
		o.Status = status
		return o
	}
	elsewhere := workingOrder(601, "OPT", rpc.OrderActionSell, 5, rpc.OrderTypeLMT)
	elsewhere.Account = "DU7654321"
	untyped := workingOrder(601, "", rpc.OrderActionSell, 5, rpc.OrderTypeLMT)
	for name, tc := range map[string]struct {
		inventory func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error)
		want      string
	}{
		"hand-placed partial sale":    {sameContractInventory(workingOrder(601, "OPT", rpc.OrderActionSell, 1, rpc.OrderTypeLMT)), "existing_reduction_order"},
		"standing trailing stop":      {sameContractInventory(workingOrder(601, "OPT", rpc.OrderActionSell, 5, rpc.OrderTypeTRAILLIMIT)), "existing_reduction_order"},
		"submitted at the broker":     {sameContractInventory(withStatus("Submitted")), "existing_reduction_order"},
		"listed without a sec type":   {sameContractInventory(untyped), "existing_reduction_order"},
		"order without contract id":   {sameContractInventory(unknown), "reduction_order_identity_unknown"},
		"inventory unavailable":       {unavailableInventory, "reduction_order_evidence_unavailable"},
		"buying the contract":         {sameContractInventory(workingOrder(601, "OPT", rpc.OrderActionBuy, 1, rpc.OrderTypeLMT)), ""},
		"another contract":            {sameContractInventory(other), ""},
		"another account":             {sameContractInventory(elsewhere), ""},
		"filled order":                {sameContractInventory(filled), ""},
		"cancelled order":             {sameContractInventory(withStatus("Cancelled")), ""},
		"inactive order":              {sameContractInventory(withStatus("Inactive")), ""},
		"rejected order":              {sameContractInventory(withStatus("Rejected")), ""},
		"what-if preview is no order": {sameContractInventory(whatIf(workingOrder(601, "OPT", rpc.OrderActionSell, 5, rpc.OrderTypeLMT))), ""},
	} {
		t.Run(name, func(t *testing.T) {
			engine.server.openOrderInventoryForTest = tc.inventory
			rows := generateSameContract(t, engine, policy, pos, now)
			by := rowsByBucket(t, rows, rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketThetaHygiene)
			for _, p := range by {
				if tc.want == "" {
					if p.Bucket == rpc.TradeProposalBucketThetaHygiene && len(p.Blockers) != 0 {
						t.Fatalf("theta blocked without a competing order: %v", blockerCodes(p))
					}
					continue
				}
				if !hasTradingBlocker(p.Blockers, tc.want) || p.State != rpc.TradeProposalStateBlocked {
					t.Fatalf("%s row = %v, want %s", p.Bucket, blockerCodes(p), tc.want)
				}
			}
			if tc.want != "" && len(approvableFor(rows, 601)) != 0 {
				t.Fatal("a row stayed approvable beside the working order")
			}
		})
	}
	// Option exits share the matching: an order listed without a security
	// type but with the exact contract id is a working close.
	exit, exitPolicy, exitPos, exitNow := lossExitThetaFixture(t)
	exit.server.openOrderInventoryForTest = sameContractInventory(workingOrder(42, "", rpc.OrderActionSell, 2, rpc.OrderTypeLMT))
	if by := rowsByBucket(t, generateSameContract(t, exit, exitPolicy, exitPos, exitNow), rpc.TradeProposalBucketOptionLossExit); !hasTradingBlocker(by[rpc.TradeProposalBucketOptionLossExit].Blockers, "existing_option_exit_order") {
		t.Fatalf("the loss exit missed a working close listed without a security type: %v", blockerCodes(by[rpc.TradeProposalBucketOptionLossExit]))
	}
}

func whatIf(o ibkrlib.OrderLifecycleEvent) ibkrlib.OrderLifecycleEvent {
	o.WhatIf = true
	return o
}

// sameContractRow is a synthetic unblocked row selling qty of one contract.
func sameContractRow(bucket string, conID, qty int, orderType string) rpc.TradeProposal {
	return rpc.TradeProposal{
		Key: bucket + ":" + strconv.Itoa(conID), State: rpc.TradeProposalStateGenerated, Bucket: bucket, Symbol: "SYNX", SecType: "OPT",
		Action: rpc.OrderActionSell, Quantity: qty, MaxQuantity: 10, PositionQuantity: 10, PositionEffect: rpc.OrderPositionEffectReduce, OrderType: orderType,
		Contract: rpc.ContractParams{ConID: conID, Symbol: "SYNX", SecType: "OPT"}, Reason: bucket + " reason",
	}
}

// Owner decision 2026-09-28 ("Sell now; trail waits"): an immediate sale is
// the candidate whatever the sizes, and the stop re-proposes for what is left.
func TestSameContractImmediateSaleGoesBeforeTrailingStop(t *testing.T) {
	trail := sameContractRow(rpc.TradeProposalBucketTrailingStop, 7, 10, rpc.OrderTypeTRAILLIMIT)
	trail.OptionExit = &rpc.TradeProposalOptionExit{Kind: risk.OptionExitActionProfitTrail, Readiness: "ready"}
	rows := []rpc.TradeProposal{trail, sameContractRow(rpc.TradeProposalBucketBudgetReduction, 7, 3, rpc.OrderTypeLMT)}
	mergeSameContractProposals(rows, nil)
	got := approvableFor(rows, 7)
	if len(got) != 1 || got[0].Bucket != rpc.TradeProposalBucketBudgetReduction || got[0].Quantity != 3 {
		t.Fatalf("approvable = %+v, want the governor's sale of 3 alone", got)
	}
	covered := rows[0]
	if covered.CoveredBy != got[0].Key || covered.OptionExit.Readiness != "blocked" || !hasTradingBlocker(covered.Blockers, "covered_by_proposal") ||
		!strings.Contains(covered.Blockers[0].Message, "re-proposes for what is left") {
		t.Fatalf("trail is not waiting for the sale: %+v", covered)
	}

	// While Canary's sale works, a stock trail waits for it rather than
	// standing oversized beside it; a hand-placed limit keeps the old rule.
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	for source, want := range map[string]bool{proposalOrderSource: true, "": false} {
		srv := newOrderReconcileTestServer(t, now)
		if err := srv.orderJournal.Append(orderJournalEvent{
			At: now.Add(-time.Minute), Type: orderJournalEventBrokerAcknowledged, OrderRef: "synthetic-sale", ReservedOrderID: 82, ClientID: 15, PermID: 882,
			Account: "DU1234567", Endpoint: "127.0.0.1:4001", Mode: "paper", Symbol: "SYNX", SecType: "STK", ConID: 880, Action: rpc.OrderActionSell,
			OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Quantity: 30, Status: "Submitted", Remaining: 30, SendState: orderSendStateBrokerAcknowledged, Source: source,
		}); err != nil {
			t.Fatal(err)
		}
		engine := &proposalEngine{server: srv, now: func() time.Time { return now }}
		stop := rpc.TradeProposal{Key: "trailing_stop:880", State: rpc.TradeProposalStateGenerated, Bucket: rpc.TradeProposalBucketTrailingStop, Symbol: "SYNX", SecType: "STK",
			Action: rpc.OrderActionSell, Quantity: 100, PositionQuantity: 100, OrderType: rpc.OrderTypeTRAIL, Contract: rpc.ContractParams{ConID: 880, Symbol: "SYNX", SecType: "STK"}}
		pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "SYNX", SecType: "STK", ConID: 880, Quantity: 100}}}
		if got := hasTradingBlocker(engine.duplicateProtectiveBlockers(context.Background(), stop, pos), "existing_reduction_order"); got != want {
			t.Fatalf("source %q: stop waits = %v, want %v", source, got, want)
		}
	}
}

// Owner decision 2026-09-28 ("Keep the automatic one"): a pre-authorised row
// is never covered by a row that needs approval.
func TestSameContractPreAuthorisedRowIsNeverCoveredByOneNeedingApproval(t *testing.T) {
	automatic := func(p rpc.TradeProposal) sameContractChannel {
		if p.Bucket == rpc.TradeProposalBucketBudgetReduction {
			return sameContractAutomatic
		}
		return sameContractManual
	}

	rows := []rpc.TradeProposal{
		sameContractRow(rpc.TradeProposalBucketBudgetReduction, 7, 2, rpc.OrderTypeLMT),
		sameContractRow(rpc.TradeProposalBucketThetaHygiene, 7, 5, rpc.OrderTypeLMT),
		sameContractRow(rpc.TradeProposalBucketRiskReduction, 7, 3, rpc.OrderTypeLMT),
	}
	mergeSameContractProposals(rows, automatic)
	budget, theta, trim := rows[0], rows[1], rows[2]
	if len(budget.Blockers) != 0 || budget.CoveredBy != "" {
		t.Fatalf("the smaller automatic order was taken away: %+v", budget)
	}
	if len(theta.Blockers) != 0 || len(theta.Covers) != 2 || theta.Covers[0].Key != trim.Key || theta.Covers[0].Automatic ||
		theta.Covers[1] != (rpc.TradeProposalCoverage{Bucket: budget.Bucket, Key: budget.Key, Quantity: 2, OrderType: rpc.OrderTypeLMT, Reason: budget.Reason, Automatic: true}) {
		t.Fatalf("the one candidate needing approval = %+v", theta)
	}
	if trim.CoveredBy != theta.Key {
		t.Fatalf("a second candidate needs approval: %+v", trim)
	}

	// An automatic row that meets the other rows covers them: nothing is left
	// to approve, and a covered exit is not ready.
	exit := sameContractRow(rpc.TradeProposalBucketOptionLossExit, 7, 5, rpc.OrderTypeLMT)
	exit.OptionExit = &rpc.TradeProposalOptionExit{Kind: risk.OptionExitActionLoss, Readiness: "ready"}
	rows = []rpc.TradeProposal{sameContractRow(rpc.TradeProposalBucketBudgetReduction, 7, 5, rpc.OrderTypeLMT), exit}
	mergeSameContractProposals(rows, automatic)
	if got := approvableFor(rows, 7); len(got) != 1 || got[0].Bucket != rpc.TradeProposalBucketBudgetReduction || len(got[0].Covers) != 1 {
		t.Fatalf("approvable = %+v", got)
	}
	if rows[1].CoveredBy != rows[0].Key || rows[1].OptionExit.Readiness != "blocked" {
		t.Fatalf("covered exit = %+v", rows[1])
	}
}

// Review of b3ce0cfe: a pre-authorised row counts as Canary's own order only
// while Canary will still place it. Once its automatic submission has ended,
// it may not cover the rows the owner can approve, because every owner
// surface hides a pre-authorised row from approval; the served snapshot asks
// for a refresh as soon as the record ends.
func TestSameContractEndedAutomaticRowNeverStrandsTheOthers(t *testing.T) {
	rig := newAutomaticTestRig(t, `pre_authorised = ["option_loss_exit"]`)
	e := rig.engine
	rows := func() []rpc.TradeProposal {
		exit := sameContractRow(rpc.TradeProposalBucketOptionLossExit, 7, 2, rpc.OrderTypeLMT)
		exit.OptionExit = &rpc.TradeProposalOptionExit{Kind: risk.OptionExitActionLoss, Readiness: "ready"}
		theta := sameContractRow(rpc.TradeProposalBucketThetaHygiene, 7, 2, rpc.OrderTypeLMT)
		exit.Revision, theta.Revision = "r1", "r1"
		return []rpc.TradeProposal{exit, theta}
	}
	exitKey := rows()[0].Key
	record := func(state string) {
		e.automatic.mu.Lock()
		defer e.automatic.mu.Unlock()
		if e.automatic.records == nil {
			e.automatic.records = map[string]*automaticSubmissionRecord{}
		}
		e.automatic.records[automaticRecordKey(exitKey, "r1")] = &automaticSubmissionRecord{Version: automaticDocumentVersion, Key: exitKey, Revision: "r1",
			Bucket: preAuthorisedBucketOptionLossExit, State: state, AccountID: rig.scope.Account, AccountMode: rig.scope.Mode}
	}
	kicked := func() bool {
		select {
		case <-e.kickCh():
			return true
		default:
			return false
		}
	}

	// No record yet: the next automatic cycle creates one, so Canary places
	// the loss exit and it covers theta.
	got := rows()
	e.mergeSameContract(got)
	if got[1].CoveredBy != exitKey || len(approvableFor(got, 7)) != 1 {
		t.Fatalf("a live automatic exit did not stand for theta: %+v", got)
	}
	for _, state := range []string{rpc.TradeProposalAutomaticPending, rpc.TradeProposalAutomaticDeferred, rpc.TradeProposalAutomaticSubmitting} {
		record(state)
		got := rows()
		e.mergeSameContract(got)
		if got[1].CoveredBy != exitKey {
			t.Fatalf("%s: an automatic exit Canary will still place stopped covering theta: %+v", state, got[1])
		}
		if e.kickIfCoverageStale(got); kicked() {
			t.Fatalf("%s: a consistent snapshot asked for a refresh", state)
		}
	}
	for _, state := range []string{rpc.TradeProposalAutomaticVetoed, rpc.TradeProposalAutomaticFailed, rpc.TradeProposalAutomaticSuperseded, rpc.TradeProposalAutomaticSubmitted} {
		// The record ends after the merge: the served snapshot asks for a
		// fresh one at once.
		record(rpc.TradeProposalAutomaticPending)
		stale := rows()
		e.mergeSameContract(stale)
		record(state)
		if e.kickIfCoverageStale(stale); !kicked() {
			t.Fatalf("%s: an ended automatic record left theta covered until the next cadence", state)
		}
		got := rows()
		e.mergeSameContract(got)
		if got[1].CoveredBy != "" || len(got[1].Blockers) != 0 || got[0].CoveredBy != "" || len(got[0].Covers) != 0 {
			t.Fatalf("%s: an exit Canary will no longer place still merged: %+v", state, got)
		}
		if a := approvableFor(got, 7); len(a) != 2 || a[1].Bucket != rpc.TradeProposalBucketThetaHygiene {
			t.Fatalf("%s: theta is no longer approvable: %+v", state, got)
		}
	}
	// Without an automatic store nothing is placed automatically either.
	e.automatic = &automaticSubmissionStore{}
	got = rows()
	e.mergeSameContract(got)
	if got[1].CoveredBy != "" || len(got[0].Covers) != 0 {
		t.Fatalf("a pre-authorised row with no automatic store covered theta: %+v", got)
	}
}

// Review of b3ce0cfe: the brief counted a covered row as blocked. It is
// another rule's reason on a proposal the brief already counts.
func TestSameContractBriefCountsCoveredRowsApart(t *testing.T) {
	rows := []rpc.TradeProposal{
		sameContractRow(rpc.TradeProposalBucketOptionLossExit, 7, 2, rpc.OrderTypeLMT),
		sameContractRow(rpc.TradeProposalBucketThetaHygiene, 7, 2, rpc.OrderTypeLMT),
		sameContractRow(rpc.TradeProposalBucketRiskReduction, 8, 1, rpc.OrderTypeLMT),
	}
	rows[2].State, rows[2].Blockers = rpc.TradeProposalStateBlocked, []rpc.TradingBlocker{{Code: "wide_spread"}}
	mergeSameContractProposals(rows, nil)
	counts := proposalCounts(rows, "")
	if counts.Total != 3 || counts.Actionable != 1 || counts.Covered != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	e := &proposalEngine{snapshot: rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Revision: "r1", Counts: counts}}
	srv := &Server{tradeProposals: e}
	row := srv.briefReadyProposals()
	if row.Actionable != 1 || row.Blocked != 1 || row.Covered != 1 || row.Total != 3 ||
		row.Detail != "1 protection proposal(s) ready to act, 1 blocked; 1 more rule(s) covered by them" {
		t.Fatalf("brief row = %+v", row)
	}
}
