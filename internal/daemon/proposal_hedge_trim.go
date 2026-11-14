package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The hedge trim (owner decision 2026-11-14 05:45 CET, "Trim card for my
// tap") is rule 12's remedy in the risk-reduction bucket, beside rule 1's
// issuer trim. The forced reduction sells the calls and never a put, so the
// index puts can end worth more than what is left to protect. Once their
// short delta passes the over-hedge line the bucket offers their sale back to
// the band top (risk.PlanHedgeTrim, on rule 12's own measurement and the
// policy's own numbers).
//
// It amends "flatten to hedges" (2026-10-14) and the owner's rule of
// 2026-11-04 21:04 CET, that a hedge-listed put is never sold, for one case
// only: a sale the owner confirms on the device. Nothing sells a put
// unattended: the bucket is outside the pre-authorised vocabulary,
// automaticBucket refuses a hedge-listed put in every bucket, and Desk's
// controller is refused one under its standing mandate (deskSubmitBlockers).
//
// What bounds it: the owner's confirmation for every order; the order caps,
// sized as a forced cut's are; whole contracts rounded down, so the hedge
// never ends under the band top; and no row at all unless the positions read
// is current and complete, every line of the book is measured and a long
// book is left to compare with. Worst case: the owner confirms the trim and the index
// then falls, with the remaining calls losing more than the smaller hedge
// pays. Those calls can lose no more than their value, which the loss
// measure counts.

// hedgeTrimProposals offers rule 12's trim on the book as read.
func (e *proposalEngine) hedgeTrimProposals(ctx context.Context, policy protectionPolicy, status rpc.ProtectionPolicyStatus, acct *rpc.AccountResult, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, now time.Time) []rpc.TradeProposal {
	plan, pol, base, ok := e.hedgeTrimPlan(ctx, acct, pos, now)
	if !ok {
		return nil
	}
	sizing := mandateReductionInput{MaxOrderNotional: policy.Buckets.RiskReduction.MaxOrderNotional}
	if e != nil && e.server != nil {
		if limits := e.server.orderLimitsInForce(base); limits.Complete {
			sizing.OrderCapBase, sizing.MaxOptionContracts = limits.CapBase, limits.MaxOptionContracts
			sizing.Exits = &deltaExitPlan{pos: pos, pol: pol}
		}
	}
	return hedgeTrimRows(policy, status, sources, now, pos, pol, plan, sizing, base)
}

// hedgeTrimPlan is rule 12's trim on the book as read, for the card and for
// the mandate's loss measure, which counts the contracts it names
// (mandateMarkProtectionExcess): one measurement, so the card and the
// measure never disagree about how much protection is too much.
func (e *proposalEngine) hedgeTrimPlan(ctx context.Context, acct *rpc.AccountResult, pos *rpc.PositionsResult, now time.Time) (risk.HedgeTrimPlan, risk.RulebookPolicy, string, bool) {
	// A positions read that is not current and whole can have lost its long
	// lines, which reads as an over-sized hedge (2026-11-04).
	if pos == nil || !currentPortfolioAuthority(pos.Authority) || !pos.Authority.PortfolioComplete {
		return risk.HedgeTrimPlan{}, risk.RulebookPolicy{}, "", false
	}
	in, pol, ok := e.issuerConcentrationInputs(ctx, acct, pos, now)
	if !ok {
		return risk.HedgeTrimPlan{}, pol, "", false
	}
	if e != nil && e.server != nil {
		if st, carried := e.server.rulebookRegimeStage(pol, now); st.Bucket != "" {
			in.RegimeStage, in.RegimeStageAsOf, in.RegimeStageCarried = st.Bucket, st.AsOf, carried
		}
	}
	plan, ok := risk.PlanHedgeTrim(in, pol)
	return plan, pol, in.BaseCurrency, ok
}

// mandateMarkProtectionExcess writes onto the protection legs how many
// contracts the book has outgrown: the ones rule 12's trim would sell (owner
// decision 2026-11-14 06:49 CET, "Count the excess"). The loss measure counts
// them; the puts the band still allows stay beside it. With no plan nothing
// is marked, which is the measure as it was.
func mandateMarkProtectionExcess(legs []mandate.Leg, pos *rpc.PositionsResult, pol risk.RulebookPolicy, plan risk.HedgeTrimPlan) {
	for _, line := range plan.Lines {
		row, found := hedgeTrimRow(pos, pol, line)
		if !found {
			continue
		}
		id := mandateLegID(row)
		for i := range legs {
			if legs[i].ID == id && legs[i].Protection {
				legs[i].ProtectionExcess = line.Sell
			}
		}
	}
}

// hedgeTrimRows turns the plan into one sale per put line, each held to the
// caps a forced cut is held to (mandateCycleQuantity: the money caps, and the
// option contract cap unless the order gate would pass the sale). A sale of
// protection is judged against the whole book, and each row against the book
// as the rows before it leave it.
func hedgeTrimRows(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, pos *rpc.PositionsResult, pol risk.RulebookPolicy, plan risk.HedgeTrimPlan, sizing mandateReductionInput, baseCcy string) []rpc.TradeProposal {
	lead := fmt.Sprintf("index protection is %.0f%% of the long book, past the %s%% over-hedge line (%s× the %s%% band top)",
		plan.RatioPct, rulebookLimitText(plan.ActPct), rulebookLimitText(pol.OverhedgeMultiple), rulebookLimitText(plan.TopPct))
	var out []rpc.TradeProposal
	for _, line := range plan.Lines {
		row, found := hedgeTrimRow(pos, pol, line)
		if !found {
			continue
		}
		full := int(math.Floor(row.Quantity + 1e-9))
		units := min(int(math.Floor(line.Sell+1e-9)), full)
		qty, waits := mandateCycleQuantity(&mandateCut{row: row, full: full, step: 1, units: units}, sizing, baseCcy)
		if qty <= 0 {
			continue
		}
		sizing.Exits.record(row, rpc.OrderActionSell, qty)
		effect := rpc.OrderPositionEffectReduce
		if qty == full {
			effect = rpc.OrderPositionEffectClose
		}
		reason := fmt.Sprintf("%s; the trim back to the band top sells %d of this line's %d %s", lead, units, full, pluralNoun(full, "contract"))
		if qty < units {
			reason += fmt.Sprintf(", %d in this order", qty)
		}
		p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketRiskReduction, row, rpc.OrderActionSell, qty, effect, reason)
		p.Details = append(p.Details, fmt.Sprintf("the puts' short delta is %s against a long book of %s; the %s%% band top allows %s (Rulebook rule 12)",
			mandateMoney(plan.ShortBase, baseCcy), mandateMoney(plan.GrossLongBase, baseCcy), rulebookLimitText(plan.TopPct), mandateMoney(plan.TargetBase, baseCcy)))
		if waits != "" {
			p.Details = append(p.Details, waits)
		}
		p.Details = append(p.Details, "offered for your confirmation only: Canary never sells protection unattended")
		p.Score = plan.RatioPct
		out = append(out, p)
	}
	return out
}

// hedgeTrimRow finds the held put line a trim line sells.
func hedgeTrimRow(pos *rpc.PositionsResult, pol risk.RulebookPolicy, line risk.HedgeTrimLine) (rpc.PositionView, bool) {
	if pos == nil {
		return rpc.PositionView{}, false
	}
	for _, row := range pos.Options {
		if mandateHedgePut(pol, row) && strings.EqualFold(strings.TrimSpace(row.Symbol), line.Symbol) && legDesc(row) == line.Leg {
			return row, true
		}
	}
	return rpc.PositionView{}, false
}

// hedgeTrimProposal reports rule 12's trim among the bucket's rows: a sale of
// a put that names no issuer, which the issuer trim always does.
func hedgeTrimProposal(p rpc.TradeProposal) bool {
	return p.Bucket == rpc.TradeProposalBucketRiskReduction && p.Issuer == "" &&
		strings.EqualFold(p.Action, rpc.OrderActionSell) && strings.EqualFold(strings.TrimSpace(p.Contract.Right), "P")
}

// hedgePutSale reports a sale of a put on a hedge-listed index: protection,
// or what was bought as protection, which only the owner sells.
func hedgePutSale(pol risk.RulebookPolicy, p rpc.TradeProposal) bool {
	return strings.EqualFold(p.Action, rpc.OrderActionSell) && strings.EqualFold(strings.TrimSpace(p.Contract.Right), "P") &&
		pol.IsHedgeSymbol(p.Contract.Symbol)
}
