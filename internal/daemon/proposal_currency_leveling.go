package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Currency leveling (internal-docs/design/currency-leveling.md; owner
// decisions 2026-10-05 21:28 and 22:02 CEST, settings settled 2026-10-06
// 05:44–06:15 CEST, the several-currency plan confirmed 2026-10-06 07:11
// CEST, "all six confirmed").
//
// From the broker ledger's trade-date cash, a currency below minus
// trigger_base (at the ledger rate) is a loan worth repaying. Loans are taken
// in order of what each unit costs, the loan rate measured from the broker's
// statements (currency_leveling_rates.go), highest first. Each is repaid back
// into the band from zero to cushion_base, never more, by the currencies
// whose cash earns least, and only by one whose cash earns less than the loan
// costs. Every conversion must earn back its worst-case cost (the commission
// bound plus the slippage bound) in interest saved within payback_days; of
// the combinations of payers that do, the one that repays the loan in full
// and saves the most within that window wins, fewer conversions on a tie. A
// payer keeps the cushion and never spends cash that working and armed buys
// already hold; what one loan takes is deducted before the next, and each row
// carries its allotment, so rows approved in any order cannot overdraw a
// payer. One loan's conversions form a bundle, approved as a whole and held
// together to the order cap in force of [order_limits]; the next cycle
// repays the rest. A currency the owner marks deliberate_carry is never repaid
// and never pays. Unknown inputs generate nothing and say which.
//
// The cash is trade-date, not the sweep's lower of trade-date and settled:
// a conversion moves trade-date cash the moment it fills while settled cash
// follows two days later, so planning on settled cash would convert the same
// debit again until it settles.
//
// Each conversion's quantity is chosen inside its safe band, at its middle,
// and the previous generation's quantity is kept while it stays inside: an
// ordinary ledger-rate update then leaves the row, its revision and its
// review alone.

// The commission bound of a conversion (assumption A1 of the design note):
// IBKR charges 0.2 bp of value on IDEALPRO, at least USD 2.
const (
	currencyLevelingCommissionBP     = 0.2
	currencyLevelingCommissionMinUSD = 2.0
	// currencyLevelingMaxPayers bounds the payers one loan's combinations
	// are drawn from: the five cheapest allowed.
	currencyLevelingMaxPayers = 5
)

// currencyLevelingInput is everything the pure planner reads.
type currencyLevelingInput struct {
	BaseCurrency string
	// Ledger is the account ledger per currency (cashSweepLedgerAt); only
	// TradeDate, Observed and ExchangeRate are read. LedgerReason is set
	// when the ledger is unavailable, or older than a confirmed fill.
	Ledger       map[string]cashSweepLedgerRow
	LedgerReason string
	// Working names every currency a working CASH order at the broker buys
	// or sells; WorkingReason is set when the complete open-order list
	// cannot be read.
	Working       map[string]bool
	WorkingReason string
	// Committed is the cash working buy orders and armed queued buys hold
	// per currency; CommittedUnknown names a currency whose commitments have
	// no price bound (key "" applies to every currency).
	Committed        map[string]float64
	CommittedUnknown map[string]string
	// OrderCapBase is the order cap in force of [order_limits] in base
	// currency; OrderCapReason says why it is unavailable.
	OrderCapBase   float64
	OrderCapReason string
	// Rates are each currency's interest rates measured from the broker's
	// statements; RatesThrough is the latest statement day read and
	// RatesReason says why none are available.
	Rates        map[string]currencyLevelingRate
	RatesThrough string
	RatesReason  string
	// Unpaired names the IDEALPRO pairs (EUR.USD) that did not resolve to a
	// contract this generation, with the reason; a payer that needs one does
	// not repay that loan.
	Unpaired map[string]string
	// Previous is each conversion's quantity in the previous generation, by
	// currencyLevelingIdentity.
	Previous map[string]int
}

// currencyLevelingLeg is one planned conversion.
type currencyLevelingLeg struct {
	block    *rpc.TradeProposalCurrencyLeveling
	contract rpc.ContractParams
	action   string
	quantity int
}

// currencyLevelingBundle is one loan's conversions, cheapest payer first.
type currencyLevelingBundle struct {
	currency string
	legs     []currencyLevelingLeg
}

type currencyLevelingPlan struct {
	status  rpc.TradeProposalCurrencyLevelingStatus
	bundles []currencyLevelingBundle
}

// currencyLevelingPlanner carries one generation's arithmetic.
type currencyLevelingPlanner struct {
	bucket                 *protectionCurrencyLevelingPolicy
	in                     currencyLevelingInput
	base                   string
	st                     *rpc.TradeProposalCurrencyLevelingStatus
	idx                    map[string]int
	cash, rate             map[string]float64
	trigger, cushion, slip float64
	payback                int
	usdRate                float64
}

// currencyLevelingPlanFor plans every ledger currency. It generates no
// proposals; rows carry the pair, not yet its contract id.
func currencyLevelingPlanFor(bucket *protectionCurrencyLevelingPolicy, in currencyLevelingInput) currencyLevelingPlan {
	base := normCcy(in.BaseCurrency)
	plan := currencyLevelingPlan{status: rpc.TradeProposalCurrencyLevelingStatus{
		BaseCurrency: base, BalanceSource: rpc.CurrencyLevelingBalanceTradeDate, NeedsYourNumber: bucket.missingNumbers(),
		Reason: in.LedgerReason, RatesSource: rpc.CurrencyLevelingRatesBrokerStatements, RatesThrough: in.RatesThrough, RatesReason: in.RatesReason,
		Currencies: []rpc.TradeProposalCurrencyLevelingCurrency{},
	}}
	if bucket != nil {
		st := &plan.status
		st.TriggerBase, st.CushionBase, st.MaxSlippageBP = cloneFloat64Ptr(bucket.TriggerBase), cloneFloat64Ptr(bucket.CushionBase), cloneFloat64Ptr(bucket.MaxSlippageBP)
		if bucket.PaybackDays != nil {
			st.PaybackDays = new(*bucket.PaybackDays)
		}
	}
	if in.OrderCapReason == "" && positiveFinite(in.OrderCapBase) {
		plan.status.OrderCapBase = new(in.OrderCapBase)
	}
	p := &currencyLevelingPlanner{bucket: bucket, in: in, base: base, st: &plan.status, idx: map[string]int{}, cash: map[string]float64{}, rate: map[string]float64{}}
	plan.bundles = p.run()
	return plan
}

// currencyLevelingCash reads ccy's trade-date cash and ledger rate; the
// reason says why either is unavailable.
func currencyLevelingCash(in currencyLevelingInput, ccy string) (cash, rate float64, reason string) {
	row, ok := in.Ledger[ccy]
	switch {
	case !ok:
		return 0, 0, fmt.Sprintf("the account ledger has no %s row; its cash is unavailable, not zero", ccy)
	case !row.Observed || !finiteProtectionOptionPolicyValue(row.TradeDate):
		return 0, 0, fmt.Sprintf("the account ledger's %s row carries no cash balance; unavailable, not zero", ccy)
	case !positiveFinite(row.ExchangeRate):
		return 0, 0, fmt.Sprintf("the account ledger carries no exchange rate for %s, so its cash cannot be compared with the trigger", ccy)
	}
	return row.TradeDate, row.ExchangeRate, ""
}

func (p *currencyLevelingPlanner) cur(ccy string) *rpc.TradeProposalCurrencyLevelingCurrency {
	return &p.st.Currencies[p.idx[ccy]]
}

func (p *currencyLevelingPlanner) set(ccy, state, reason string) {
	c := p.cur(ccy)
	c.State, c.Reason = state, reason
}

// borrowed describes a loan for its reason line.
func (p *currencyLevelingPlanner) borrowed(ccy string) string {
	cash, rate := p.cash[ccy], p.rate[ccy]
	return fmt.Sprintf("%s is borrowed: %s (%s), beyond the %s band", ccy, currencyLevelingMoney(cash, ccy, true), currencyLevelingMoney(cash*rate, p.base, true), currencyLevelingMoney(p.trigger, p.base, false))
}

func (p *currencyLevelingPlanner) run() []currencyLevelingBundle {
	for _, ccy := range slices.Sorted(maps.Keys(p.in.Ledger)) {
		c := rpc.TradeProposalCurrencyLevelingCurrency{Currency: ccy, DeliberateCarry: p.bucket.deliberateCarry(ccy)}
		if cash, rate, reason := currencyLevelingCash(p.in, ccy); reason != "" {
			c.State, c.Reason = rpc.CurrencyLevelingStateCashUnavailable, reason
		} else {
			c.Cash, c.CashBase, c.ExchangeRate = new(cash), new(cash*rate), new(rate)
			p.cash[ccy], p.rate[ccy] = cash, rate
		}
		p.idx[ccy] = len(p.st.Currencies)
		p.st.Currencies = append(p.st.Currencies, c)
	}
	available := slices.Sorted(maps.Keys(p.cash))
	if missing := p.bucket.missingNumbers(); len(missing) > 0 {
		for _, ccy := range available {
			p.set(ccy, rpc.CurrencyLevelingStateHold, "needs your number: "+strings.Join(missing, ", "))
		}
		return nil
	}
	p.trigger, p.cushion, p.slip, p.payback = *p.bucket.TriggerBase, *p.bucket.CushionBase, *p.bucket.MaxSlippageBP/10000, *p.bucket.PaybackDays
	var loans, candidates []string
	for _, ccy := range available {
		cash, rate := p.cash[ccy], p.rate[ccy]
		switch {
		case p.cur(ccy).DeliberateCarry:
			p.set(ccy, rpc.CurrencyLevelingStateDeliberateCarry, fmt.Sprintf("deliberate_carry = true: %s stays at %s on purpose; it is never repaid and never pays", ccy, currencyLevelingMoney(cash, ccy, true)))
		case cash*rate < -p.trigger:
			p.cur(ccy).Role = rpc.CurrencyLevelingRoleLoan
			loans = append(loans, ccy)
		case cash < 0:
			p.set(ccy, rpc.CurrencyLevelingStateInBand, fmt.Sprintf("%s is borrowed, %s (%s), but within the %s band, so it is not repaid",
				ccy, currencyLevelingMoney(cash, ccy, true), currencyLevelingMoney(cash*rate, p.base, true), currencyLevelingMoney(p.trigger, p.base, false)))
		default:
			p.set(ccy, rpc.CurrencyLevelingStateInBand, fmt.Sprintf("%s is not borrowed (%s)", ccy, currencyLevelingMoney(cash, ccy, true)))
			candidates = append(candidates, ccy)
		}
	}
	if len(loans) == 0 {
		return nil
	}
	if why := p.bucketHold(); why != "" {
		for _, ccy := range loans {
			p.set(ccy, rpc.CurrencyLevelingStateHold, p.borrowed(ccy)+"; "+why)
		}
		return nil
	}
	rates := map[string]float64{}
	var ready []string
	for _, ccy := range loans {
		rate, through, bound, ok := p.in.Rates[ccy].loanRate()
		c := p.cur(ccy)
		switch {
		case !ok:
			p.set(ccy, rpc.CurrencyLevelingStateHold, p.borrowed(ccy)+fmt.Sprintf("; what the %s loan costs is not in the broker's statements yet, so no repayment can be weighed; it is once the first statement shows its interest", ccy))
			continue
		case p.in.Working[ccy]:
			p.set(ccy, rpc.CurrencyLevelingStateHold, p.borrowed(ccy)+"; a conversion in "+ccy+" is already working at the broker, so nothing more is proposed until it fills or is cancelled")
		}
		c.LoanRate, c.LoanRateThrough, c.LoanRateBound = new(rate), through, bound
		if !p.in.Working[ccy] {
			rates[ccy] = rate
			ready = append(ready, ccy)
		}
	}
	spendable := p.payers(candidates)
	// Dearest loan first: each unit of cash saves most where the loan rate is
	// highest. At equal rates the larger loan, then the name.
	slices.SortFunc(ready, func(a, b string) int {
		switch {
		case rates[a] != rates[b]:
			return cmpFloatDesc(rates[a], rates[b])
		case p.cash[a]*p.rate[a] != p.cash[b]*p.rate[b]:
			return cmpFloatDesc(-p.cash[a]*p.rate[a], -p.cash[b]*p.rate[b])
		}
		return strings.Compare(a, b)
	})
	pays := map[string][]string{}
	var bundles []currencyLevelingBundle
	for _, loan := range ready {
		bundle, why := p.planLoan(loan, rates[loan], spendable)
		if bundle == nil {
			p.set(loan, rpc.CurrencyLevelingStateHold, p.borrowed(loan)+"; "+why)
			continue
		}
		n := len(bundle.legs)
		conversions := "a conversion follows"
		if n > 1 {
			conversions = fmt.Sprintf("%d conversions follow, approved together", n)
		}
		p.set(loan, rpc.CurrencyLevelingStateConvert, p.borrowed(loan)+fmt.Sprintf(", costing %s a year; %s", currencyLevelingPct(rates[loan]), conversions))
		for _, leg := range bundle.legs {
			b := leg.block
			p.cur(b.FundingCurrency).PaysBase += b.Spent * p.rate[b.FundingCurrency]
			pays[b.FundingCurrency] = append(pays[b.FundingCurrency], loan)
		}
		bundles = append(bundles, *bundle)
	}
	for _, ccy := range slices.Sorted(maps.Keys(pays)) {
		c := p.cur(ccy)
		c.State = rpc.CurrencyLevelingStatePays
		c.Reason = fmt.Sprintf("%s pays about %s of the %s %s, earning %s a year", ccy, currencyLevelingMoney(c.PaysBase, p.base, false), strings.Join(pays[ccy], " and "), plural(len(pays[ccy]), "loan", "loans"), currencyLevelingPct(*c.CashRate))
	}
	for _, ccy := range slices.Sorted(maps.Keys(spendable)) {
		if c := p.cur(ccy); c.State != rpc.CurrencyLevelingStatePays && c.CashRate != nil {
			c.Reason = fmt.Sprintf("%s is not borrowed (%s) and earns %s a year; it does not pay: %s", ccy, currencyLevelingMoney(p.cash[ccy], ccy, true), currencyLevelingPct(*c.CashRate), p.notPaying(ccy, rates, ready))
		}
	}
	return bundles
}

// bucketHold is why no loan can be planned at all.
func (p *currencyLevelingPlanner) bucketHold() string {
	in := p.in
	switch {
	case in.LedgerReason != "":
		return in.LedgerReason
	case in.WorkingReason != "":
		return in.WorkingReason
	case in.OrderCapReason != "" || !positiveFinite(in.OrderCapBase):
		return "the order cap in force is unavailable (" + nonEmptyString(in.OrderCapReason, "no cap") + "), so no repayment can be sized"
	case in.RatesReason != "":
		return in.RatesReason
	case p.base == "":
		return "the account's base currency is unknown"
	}
	if p.base == "USD" {
		p.usdRate = 1
	} else if row, ok := in.Ledger["USD"]; ok && positiveFinite(row.ExchangeRate) {
		p.usdRate = row.ExchangeRate
	} else {
		return "the ledger carries no USD rate, so a conversion's minimum commission (USD 2) cannot be valued"
	}
	return ""
}

// payers measures what each candidate may spend, in base currency: its
// trade-date cash less what working and armed buys hold, less the cushion it
// keeps. A currency in motion, with unbounded commitments or no measured cash
// rate does not pay; its status says why.
func (p *currencyLevelingPlanner) payers(candidates []string) map[string]float64 {
	out := map[string]float64{}
	for _, ccy := range candidates {
		c := p.cur(ccy)
		note := func(why string) {
			c.Reason = fmt.Sprintf("%s is not borrowed (%s); it does not pay: %s", ccy, currencyLevelingMoney(p.cash[ccy], ccy, true), why)
		}
		switch {
		case p.in.Working[ccy]:
			note("a conversion in " + ccy + " is already working at the broker, so its cash is in motion")
			continue
		case p.in.CommittedUnknown[ccy] != "":
			note(p.in.CommittedUnknown[ccy])
			continue
		case p.in.CommittedUnknown[""] != "":
			note(p.in.CommittedUnknown[""])
			continue
		}
		rate, through, bound, ok := p.in.Rates[ccy].cashRate()
		if !ok {
			note("what its cash earns is not in the broker's statements yet")
			continue
		}
		c.CashRate, c.CashRateThrough, c.CashRateBound = new(rate), through, bound
		spend := (p.cash[ccy]-p.in.Committed[ccy])*p.rate[ccy] - p.cushion
		c.SpendableBase = new(max(spend, 0))
		if spend <= 0 {
			note(fmt.Sprintf("after working and armed buys it holds no more than the %s cushion it keeps", currencyLevelingMoney(p.cushion, p.base, false)))
			continue
		}
		c.Role = rpc.CurrencyLevelingRolePayer
		out[ccy] = spend
	}
	return out
}

// notPaying says why a payer with cash took no part.
func (p *currencyLevelingPlanner) notPaying(ccy string, rates map[string]float64, loans []string) string {
	cashRate := *p.cur(ccy).CashRate
	dearer := false
	for _, loan := range loans {
		if cashRate < rates[loan] {
			dearer = true
		}
	}
	if !dearer {
		return "its cash earns at least what any loan costs, and leveling never sells a currency that earns more than the loan costs"
	}
	return "cheaper currencies cover the loans, or a conversion from it would not pay back within the payback window"
}

// currencyLevelingCandidate is one combination of payers for a loan.
type currencyLevelingCandidate struct {
	payers []string
	values []float64
	full   bool
	score  float64
}

// cost is a conversion's worst-case cost in base currency: the commission
// bound and the slippage bound.
func (p *currencyLevelingPlanner) cost(v float64) float64 {
	return max(currencyLevelingCommissionBP/10000*v, currencyLevelingCommissionMinUSD*p.usdRate) + p.slip*v
}

// saving is the interest a conversion of v saves within the payback window.
func (p *currencyLevelingPlanner) saving(v, spread float64) float64 {
	return v * spread * float64(p.payback) / 365
}

// breakeven is the smallest conversion that pays back at spread; false when
// none does.
func (p *currencyLevelingPlanner) breakeven(spread float64) (float64, bool) {
	per := spread*float64(p.payback)/365 - p.slip - currencyLevelingCommissionBP/10000
	if per <= 0 {
		return 0, false
	}
	return currencyLevelingCommissionMinUSD * p.usdRate / (spread*float64(p.payback)/365 - p.slip), true
}

// need is the loan's planned repayment in base currency, the middle of the
// band from zero to the cushion, held to the order cap; capped reports the
// cap held it.
func (p *currencyLevelingPlanner) need(loan string) (need float64, capped bool) {
	debt := -p.cash[loan] * p.rate[loan]
	need = debt + p.cushion/2
	if limit := p.in.OrderCapBase - p.cushion/2; need > limit {
		return max(limit, 0), true
	}
	return need, false
}

// planLoan picks the loan's conversions from what the payers have left and
// deducts their allotments; nil and the reason when none qualifies.
func (p *currencyLevelingPlanner) planLoan(loan string, loanRate float64, spendable map[string]float64) (*currencyLevelingBundle, string) {
	var allowed, dearer, unpaired []string
	for _, ccy := range slices.Sorted(maps.Keys(spendable)) {
		if spendable[ccy] <= 0 {
			continue
		}
		pair, _, err := canonicalOrderFXContract(loan, ccy)
		name := pair.Symbol + "." + pair.Currency
		switch {
		case err != nil:
			unpaired = append(unpaired, fmt.Sprintf("%s has no pair Canary can trade on IDEALPRO", ccy))
		case p.in.Unpaired[name] != "":
			unpaired = append(unpaired, fmt.Sprintf("the %s contract cannot be resolved (%s)", name, p.in.Unpaired[name]))
		case *p.cur(ccy).CashRate >= loanRate:
			dearer = append(dearer, fmt.Sprintf("%s earns %s", ccy, currencyLevelingPct(*p.cur(ccy).CashRate)))
		default:
			allowed = append(allowed, ccy)
		}
	}
	if len(allowed) == 0 {
		switch {
		case len(dearer) > 0:
			return nil, fmt.Sprintf("the loan costs %s a year and every currency with cash to spare earns at least that (%s); leveling never sells a currency that earns more than the loan costs", currencyLevelingPct(loanRate), strings.Join(dearer, ", "))
		case len(unpaired) > 0:
			return nil, "no conversion can be named: " + strings.Join(unpaired, "; ")
		}
		return nil, fmt.Sprintf("no other currency holds cash above the %s cushion to repay it, and leveling never borrows one currency to repay another", currencyLevelingMoney(p.cushion, p.base, false))
	}
	// Cheapest cash first: at equal rates the base currency, then the larger.
	slices.SortStableFunc(allowed, func(a, b string) int {
		ra, rb := *p.cur(a).CashRate, *p.cur(b).CashRate
		switch {
		case ra != rb:
			return cmpFloatDesc(rb, ra)
		case (a == p.base) != (b == p.base):
			if a == p.base {
				return -1
			}
			return 1
		}
		return cmpFloatDesc(spendable[a], spendable[b])
	})
	allowed = allowed[:min(len(allowed), currencyLevelingMaxPayers)]
	need, capped := p.need(loan)
	var best *currencyLevelingCandidate
	for mask := 1; mask < 1<<len(allowed); mask++ {
		var subset []string
		for i, ccy := range allowed {
			if mask&(1<<i) != 0 {
				subset = append(subset, ccy)
			}
		}
		c, ok := p.candidate(loanRate, need, subset, spendable)
		if !ok {
			continue
		}
		if best == nil || c.full && !best.full || c.full == best.full && (c.score > best.score+1e-9 || math.Abs(c.score-best.score) <= 1e-9 && len(c.payers) < len(best.payers)) {
			best = &c
		}
	}
	if best == nil {
		cheapest := allowed[0]
		spread := loanRate - *p.cur(cheapest).CashRate
		return nil, fmt.Sprintf("no conversion pays for itself within %d days: the cheapest currency allowed to pay, %s, earns %s against the loan's %s, which saves about %s on %s against a cost of up to %s",
			p.payback, cheapest, currencyLevelingPct(*p.cur(cheapest).CashRate), currencyLevelingPct(loanRate),
			currencyLevelingMoney(p.saving(min(need, spendable[cheapest]), spread), p.base, false), currencyLevelingMoney(min(need, spendable[cheapest]), p.base, false), currencyLevelingMoney(p.cost(min(need, spendable[cheapest])), p.base, false))
	}
	return p.legs(loan, loanRate, need, capped, *best, spendable), ""
}

// candidate fills subset, cheapest first: each payer gives all it can and
// the last gives the rest; the last conversion is never smaller than what
// pays back, the one before it giving up the difference. Every conversion
// must pay back on its own.
func (p *currencyLevelingPlanner) candidate(loanRate, need float64, subset []string, spendable map[string]float64) (currencyLevelingCandidate, bool) {
	c := currencyLevelingCandidate{payers: subset, values: make([]float64, len(subset))}
	rest := need
	for i, ccy := range subset {
		take := min(spendable[ccy], rest)
		if take <= cashSweepMoneyEpsilon {
			return c, false
		}
		c.values[i], rest = take, rest-take
	}
	c.full = rest <= cashSweepMoneyEpsilon
	if last := len(subset) - 1; last > 0 {
		if m, ok := p.breakeven(loanRate - *p.cur(subset[last]).CashRate); ok && c.values[last] < m {
			shift := min(m-c.values[last], c.values[last-1], spendable[subset[last]]-c.values[last])
			c.values[last-1] -= shift
			c.values[last] += shift
		}
	}
	for i, ccy := range subset {
		v, spread := c.values[i], loanRate-*p.cur(ccy).CashRate
		gain := p.saving(v, spread) - p.cost(v)
		if gain <= 0 {
			return c, false
		}
		c.score += gain
	}
	return c, true
}

// legs sizes the chosen combination's conversions and deducts each one's
// allotment from its payer. Each conversion gets its share of the target: its
// planned value plus a share of half the cushion as its ceiling, minus the
// same as its floor, so the bundle lands between zero and the cushion and
// never above, whatever order its conversions fill in.
func (p *currencyLevelingPlanner) legs(loan string, loanRate, need float64, capped bool, c currencyLevelingCandidate, spendable map[string]float64) *currencyLevelingBundle {
	total := 0.0
	for _, v := range c.values {
		total += v
	}
	cash, rate := p.cash[loan], p.rate[loan]
	bundle := &currencyLevelingBundle{currency: loan}
	for i, payer := range c.payers {
		v := c.values[i]
		share := p.cushion / 2 * v / total
		ceiling, floor := (v+share)/rate, max(v-share, 0)/rate
		allot := min(spendable[payer], (v+share)*(1+p.slip))
		spendable[payer] -= allot
		payerRate := p.rate[payer]
		allotUnits := allot / payerRate
		pair, inverted, _ := canonicalOrderFXContract(loan, payer)
		pair.PrimaryExch, pair.LocalSymbol = "", pair.Symbol+"."+pair.Currency
		symbolRate, pairRate := rate, payerRate
		if inverted {
			symbolRate, pairRate = payerRate, rate
		}
		price := symbolRate / pairRate
		far, near := price*(1+p.slip), price*(1-p.slip)
		leg := currencyLevelingLeg{contract: pair}
		var lo, hiBand, hiFunding int
		if pair.Currency == loan {
			// SELL the payer symbol for the borrowed pair currency: the
			// quantity counts what is spent; q × fill comes in.
			leg.action = rpc.OrderActionSell
			lo = int(math.Ceil(floor/near - 1e-9))
			hiBand = int(math.Floor(ceiling/far + 1e-9))
			hiFunding = int(math.Floor(allotUnits + 1e-9))
		} else {
			// BUY the borrowed symbol with the payer currency: the quantity
			// counts what comes in; at most q × the bound's far edge is spent.
			leg.action = rpc.OrderActionBuy
			lo = int(math.Ceil(floor - 1e-9))
			hiBand = int(math.Floor(ceiling + 1e-9))
			hiFunding = int(math.Floor(allotUnits/far + 1e-9))
		}
		hi := min(hiBand, hiFunding)
		qty := max(hi, 0)
		if hi >= lo {
			qty = lo + (hi-lo)/2
			if prev, ok := p.in.Previous[currencyLevelingIdentity(loan, pair.LocalSymbol, leg.action)]; ok && prev >= lo && prev <= hi {
				qty = prev
			}
		}
		payerCash, committed := p.cash[payer], p.in.Committed[payer]
		cashRate := p.cur(payer).CashRate
		loanStatus := p.cur(loan)
		b := &rpc.TradeProposalCurrencyLeveling{
			Leg: i + 1, Legs: len(c.payers),
			Currency: loan, FundingCurrency: payer, Pair: pair.LocalSymbol, PairSymbol: pair.Symbol, PairCurrency: pair.Currency,
			BalanceSource: rpc.CurrencyLevelingBalanceTradeDate, Cash: cash, Target: cash + ceiling, FundingCash: payerCash, FundingCommitted: committed,
			Allotment: allotUnits, ExchangeRate: rate, PlanningPrice: price,
			TriggerBase: p.trigger, CushionBase: p.cushion, MaxSlippageBP: p.slip * 10000, OrderCapBase: p.in.OrderCapBase,
			LoanRate: loanRate, LoanRateThrough: loanStatus.LoanRateThrough, LoanRateBound: loanStatus.LoanRateBound,
			FundingRate: *cashRate, FundingRateThrough: p.cur(payer).CashRateThrough, FundingRateBound: p.cur(payer).CashRateBound,
			PaybackDays: p.payback, HeldToCap: capped, FundingShort: !c.full,
		}
		if leg.action == rpc.OrderActionSell {
			b.Received, b.Spent = float64(qty)*price, float64(qty)
		} else {
			b.Received, b.Spent = float64(qty), float64(qty)*price
		}
		b.FundingAfter = payerCash - committed - b.Spent
		b.ValueBase = b.Received * rate
		// Published in cents, saving down and cost up: a full-precision float can
		// differ in its last bit between machines, and the rows feed signed terms.
		b.SavingBase, b.CostBase = levelingCentsDown(p.saving(b.ValueBase, loanRate-*cashRate)), levelingCentsUp(p.cost(b.ValueBase))
		leg.block, leg.quantity = b, qty
		bundle.legs = append(bundle.legs, leg)
	}
	return bundle
}

// currencyLevelingPct is an annual decimal rate as a percentage.
func currencyLevelingPct(r float64) string {
	return strconv.FormatFloat(r*100, 'f', 2, 64) + "%"
}

// currencyLevelingMoney is an amount with thousands separators and a true
// minus sign; signed adds a plus to a positive balance.
func currencyLevelingMoney(v float64, ccy string, signed bool) string {
	rounded := math.Round(v)
	text := briefThousands(rounded, 0)
	switch {
	case rounded == 0:
		text = "0"
	case strings.HasPrefix(text, "-"):
		text = "−" + text[1:]
	case signed:
		text = "+" + text
	}
	return strings.TrimSpace(text + " " + ccy)
}

// currencyLevelingProposals plans and builds the bucket's rows; nil while
// the table is absent or disabled.
func (e *proposalEngine) currencyLevelingProposals(ctx context.Context, policy protectionPolicy, status rpc.ProtectionPolicyStatus, acct *rpc.AccountResult, sources rpc.TradeProposalSourceFingerprints, scope brokerStateScope, now time.Time) ([]rpc.TradeProposal, *rpc.TradeProposalCurrencyLevelingStatus) {
	bucket := policy.Cash.Leveling
	if !bucket.enabled() {
		return nil, nil
	}
	plan := e.currencyLevelingResolvedPlan(ctx, bucket, e.currencyLevelingInput(ctx, acct, scope, now))
	var out []rpc.TradeProposal
	for _, bundle := range plan.bundles {
		rows := make([]rpc.TradeProposal, 0, len(bundle.legs))
		ignored := false
		for _, leg := range bundle.legs {
			row := currencyLevelingRow(policy, status, sources, now, plan.status, bundle, leg)
			ignored = ignored || e.isIgnored(scope, row.Key)
			rows = append(rows, row)
		}
		if ignored {
			// A bundle is approved whole: one conversion set aside holds the
			// loan until the plan changes.
			for i := range plan.status.Currencies {
				if c := &plan.status.Currencies[i]; c.Currency == bundle.currency {
					borrowed, _, _ := strings.Cut(c.Reason, ", costing")
					c.State, c.Reason = rpc.CurrencyLevelingStateHold, borrowed+"; you set a conversion of this repayment aside, so it waits until the plan changes"
				}
			}
			continue
		}
		plan.status.Bundles = append(plan.status.Bundles, currencyLevelingBundleEntry(rows))
		out = append(out, rows...)
	}
	st := plan.status
	st.Rows = len(out)
	return out, &st
}

// currencyLevelingBundleEntry is one loan's served bundle from its rows in
// send order: the keys, what the conversions save and cost together within
// the payback window, where the loan lands at the planning prices and the
// cushion in the loan's unit. Its revision is filled once the rows have
// theirs (currencyLevelingBundleRevisions).
func currencyLevelingBundleEntry(rows []rpc.TradeProposal) rpc.TradeProposalCurrencyLevelingBundle {
	b := rows[0].CurrencyLeveling
	entry := rpc.TradeProposalCurrencyLevelingBundle{ID: b.BundleID, Currency: b.Currency, PaybackDays: b.PaybackDays, LandsAt: b.Cash}
	if b.ExchangeRate > 0 {
		entry.Cushion = b.CushionBase / b.ExchangeRate
	}
	for _, row := range rows {
		entry.Keys = append(entry.Keys, row.Key)
		entry.SavingBase += row.CurrencyLeveling.SavingBase
		entry.CostBase += row.CurrencyLeveling.CostBase
		entry.LandsAt += row.CurrencyLeveling.Received
	}
	return entry
}

// currencyLevelingResolvedPlan plans, resolves every planned pair to its
// exact contract and plans again without a pair that does not resolve, until
// every conversion names its contract. Bundle ids bind the contract ids.
func (e *proposalEngine) currencyLevelingResolvedPlan(ctx context.Context, bucket *protectionCurrencyLevelingPolicy, in currencyLevelingInput) currencyLevelingPlan {
	in.Unpaired = maps.Clone(in.Unpaired)
	if in.Unpaired == nil {
		in.Unpaired = map[string]string{}
	}
	// Every round removes at least one pair, so the rounds are bounded by
	// the pairs a ledger can name.
	for {
		plan := currencyLevelingPlanFor(bucket, in)
		failed := false
		for bi := range plan.bundles {
			for li := range plan.bundles[bi].legs {
				leg := &plan.bundles[bi].legs[li]
				resolved, err := e.currencyLevelingPair(ctx, leg.contract)
				if err != nil {
					in.Unpaired[leg.contract.LocalSymbol] = err.Error()
					failed = true
					continue
				}
				leg.contract = resolved
			}
		}
		if !failed {
			for bi := range plan.bundles {
				id := currencyLevelingBundleID(plan.bundles[bi])
				for li := range plan.bundles[bi].legs {
					plan.bundles[bi].legs[li].block.BundleID = id
				}
			}
			return plan
		}
	}
}

// currencyLevelingInput gathers the planner's inputs for the connected
// scope: the ledger, held while a confirmed fill is newer than it; the order
// cap in force; the interest rates from the broker's statements; the working
// conversions and buy commitments from the complete all-client open-order
// list and the armed queue; and the previous generation's quantities.
func (e *proposalEngine) currencyLevelingInput(ctx context.Context, acct *rpc.AccountResult, scope brokerStateScope, now time.Time) currencyLevelingInput {
	in := currencyLevelingInput{Previous: map[string]int{}}
	e.mu.Lock()
	previous := e.snapshot
	e.mu.Unlock()
	if sameBrokerScope(brokerStateScope{Account: previous.AccountID, Mode: previous.AccountMode}, scope) {
		for _, p := range previous.Proposals {
			if b := p.CurrencyLeveling; p.Bucket == rpc.TradeProposalBucketCurrencyLeveling && b != nil {
				in.Previous[currencyLevelingIdentity(b.Currency, b.Pair, p.Action)] = p.Quantity
			}
		}
	}
	cashNow := now
	if e.server != nil {
		cashNow = e.server.nowUTC()
	}
	in.BaseCurrency, in.Ledger, in.LedgerReason = cashSweepLedgerAt(acct, cashNow)
	toBase := func(ccy string) (float64, bool) {
		row, ok := in.Ledger[ccy]
		return row.ExchangeRate, ok && positiveFinite(row.ExchangeRate)
	}
	in.Rates, in.RatesThrough, in.RatesReason = e.currencyLevelingRates(ctx, scope, toBase, now)
	if e.server == nil {
		in.WorkingReason = "no broker open-order list is attached, so a conversion already working cannot be ruled out"
		in.OrderCapReason = "no daemon"
		return in
	}
	if in.LedgerReason == "" {
		cutoff, err := e.server.cashLedgerFillCutoff(scope)
		switch {
		case errors.Is(err, ErrTradingDisabled):
			// A build without trading keeps no order journal and sends
			// nothing, so no fill of Canary's can be newer than the ledger.
		case err != nil:
			in.LedgerReason = "Canary's order journal cannot show whether a fill is newer than the ledger, so the balances may already be out of date"
		case !cutoff.IsZero() && (acct.Authority.AsOf.IsZero() || acct.Authority.AsOf.Before(cutoff)):
			in.LedgerReason = "the ledger predates a confirmed fill; waiting for the broker's post-fill balances"
		}
	}
	if limits := e.server.orderLimitsInForce(in.BaseCurrency); limits.Complete {
		in.OrderCapBase = limits.CapBase
	} else {
		in.OrderCapReason = nonEmptyString(limits.Summary, "[order_limits] is incomplete")
	}
	snapshot, snapScope, err := e.server.brokerOpenOrderInventory(ctx, false)
	if err != nil || !sameBrokerScope(snapScope, scope) {
		in.WorkingReason = "the broker's complete open-order list is unavailable, so a conversion already working cannot be ruled out"
		return in
	}
	in.Working = currencyLevelingWorkingFrom(snapshot.Orders, scope)
	in.Committed, in.CommittedUnknown = currencyLevelingCommitted(snapshot.Orders, e.queued.list(), scope)
	return in
}

// currencyLevelingPairs caches resolved IDEALPRO pairs by pair name.
type currencyLevelingPairs struct {
	mu     sync.Mutex
	byPair map[string]rpc.ContractParams
}

// currencyLevelingPairWait bounds one contract-details read for a pair.
const currencyLevelingPairWait = 3 * time.Second

// currencyLevelingPair resolves pair to its exact IDEALPRO contract once per
// daemon and keeps it.
func (e *proposalEngine) currencyLevelingPair(ctx context.Context, pair rpc.ContractParams) (rpc.ContractParams, error) {
	e.levelingPairs.mu.Lock()
	cached, ok := e.levelingPairs.byPair[pair.LocalSymbol]
	e.levelingPairs.mu.Unlock()
	if ok {
		return cached, nil
	}
	if e.server == nil {
		return rpc.ContractParams{}, errors.New("no broker connection")
	}
	resolved := pair
	if seam := e.server.orderContractResolverForTest; seam != nil {
		r, err := seam(ctx, pair, currencyLevelingPairWait)
		if err != nil {
			return rpc.ContractParams{}, err
		}
		resolved.ConID, resolved.MinTick = r.ConID, r.MinTick
	} else {
		c := e.server.gatewayConnector()
		if c == nil {
			return rpc.ContractParams{}, errors.New("no broker connection")
		}
		detail, err := c.ContractDetailsFirst(ctx, *previewIBKRContract(pair), currencyLevelingPairWait)
		switch {
		case err != nil:
			return rpc.ContractParams{}, err
		case detail == nil || !strings.EqualFold(detail.SecType, "CASH") || normCcy(detail.Symbol) != pair.Symbol || normCcy(detail.Currency) != pair.Currency:
			return rpc.ContractParams{}, errors.New("the broker answered with another contract")
		}
		resolved.ConID, resolved.MinTick = detail.ConID, detail.MinTick
	}
	if resolved.ConID <= 0 {
		return rpc.ContractParams{}, errors.New("the broker returned no contract id")
	}
	e.levelingPairs.mu.Lock()
	if e.levelingPairs.byPair == nil {
		e.levelingPairs.byPair = map[string]rpc.ContractParams{}
	}
	e.levelingPairs.byPair[pair.LocalSymbol] = resolved
	e.levelingPairs.mu.Unlock()
	return resolved, nil
}

// currencyLevelingWorkingFrom names both currencies of every working CASH
// order in scope, whoever placed it.
func currencyLevelingWorkingFrom(orders []ibkrlib.OrderLifecycleEvent, scope brokerStateScope) map[string]bool {
	out := map[string]bool{}
	for _, o := range orders {
		if !brokerOrderWorking(o) || !strings.EqualFold(strings.TrimSpace(o.SecType), "CASH") {
			continue
		}
		if account := strings.TrimSpace(o.Account); account != "" && !strings.EqualFold(account, strings.TrimSpace(scope.Account)) {
			continue
		}
		for _, ccy := range []string{normCcy(o.Symbol), normCcy(o.Currency)} {
			if ccy != "" {
				out[ccy] = true
			}
		}
		if sym, quote, ok := strings.Cut(normCcy(o.LocalSymbol), "."); ok {
			out[sym], out[quote] = true, true
		}
	}
	return out
}

// currencyLevelingCommitted is the principal working buy orders and armed
// queued buys hold per currency: what a conversion must leave in its
// funding currency so those buys do not turn it into a new debit. A buy
// without a fixed price bound makes its currency unknown. Commissions are
// small beside the cushion and are not counted, unlike the sweep's
// fee-inclusive commitments, which would hold leveling behind every resting
// buy whose fee bound Canary did not record.
func currencyLevelingCommitted(orders []ibkrlib.OrderLifecycleEvent, queued []queuedAuthRecord, scope brokerStateScope) (map[string]float64, map[string]string) {
	committed, unknown := map[string]float64{}, map[string]string{}
	add := func(ccy, secType string, quantity, price float64, multiplier int, bounded bool) {
		mult, ok := cashSweepMultiplier(secType, multiplier, ccy)
		amount := quantity * price * mult
		switch {
		case ccy == "":
			unknown[""] = "a working buy carries no currency, so the cash it commits is unknown"
		case !bounded || !ok || !positiveFinite(amount):
			unknown[ccy] = fmt.Sprintf("a working buy in %s has no price bound, so the cash it commits is unknown", ccy)
		default:
			committed[ccy] += amount
		}
	}
	for _, o := range orders {
		secType := strings.ToUpper(strings.TrimSpace(o.SecType))
		if !brokerOrderWorking(o) || secType == "CASH" || !strings.EqualFold(strings.TrimSpace(o.Action), rpc.OrderActionBuy) {
			continue
		}
		if account := strings.TrimSpace(o.Account); account != "" && !strings.EqualFold(account, strings.TrimSpace(scope.Account)) {
			continue
		}
		remaining := o.Remaining
		if remaining <= 0 {
			remaining = o.TotalQuantity - o.Filled
		}
		orderType := strings.ToUpper(strings.TrimSpace(o.OrderType))
		add(normCcy(o.Currency), secType, remaining, o.LimitPrice, o.Multiplier, orderType == "LMT" || orderType == "STP LMT")
	}
	for _, rec := range queued {
		if !rec.liveIntent() || !sameBrokerScope(rec.scope(), scope) || !strings.EqualFold(rec.Terms.Action, rpc.OrderActionBuy) {
			continue
		}
		add(normCcy(nonEmptyString(rec.Terms.Currency, rec.Terms.Contract.Currency)), strings.ToUpper(strings.TrimSpace(rec.Terms.Contract.SecType)),
			float64(rec.Terms.MaxQuantity), rec.Terms.WorstPrice, rec.Terms.Contract.Multiplier, true)
	}
	return committed, unknown
}

// currencyLevelingRow builds one conversion proposal on the pair's exact
// contract.
func currencyLevelingRow(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, st rpc.TradeProposalCurrencyLevelingStatus, bundle currencyLevelingBundle, leg currencyLevelingLeg) rpc.TradeProposal {
	b := rpc.CloneProposalCurrencyLeveling(leg.block)
	base := st.BaseCurrency
	received := 0.0
	for _, l := range bundle.legs {
		received += l.block.Received
	}
	p := rpc.TradeProposal{
		Key: currencyLevelingKey(b.Currency, b.Pair, leg.action, leg.contract.ConID), State: rpc.TradeProposalStateGenerated, Bucket: rpc.TradeProposalBucketCurrencyLeveling,
		Symbol: b.Pair, SecType: "CASH", Action: leg.action, Quantity: leg.quantity, MaxQuantity: leg.quantity,
		PositionEffect: rpc.OrderPositionEffectReduce, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: leg.contract,
		Reason:   currencyLevelingReason(b, base, received),
		PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, SourceFingerprints: sources, CreatedAt: now,
		CurrencyLeveling: b, NeverSkipVeto: true,
	}
	funding := fmt.Sprintf("%s trade-date cash %s funds it", b.FundingCurrency, currencyLevelingMoney(b.FundingCash, b.FundingCurrency, true))
	if b.FundingCommitted > 0 {
		funding += fmt.Sprintf(" (%s of it backs working and armed buys and is left alone)", currencyLevelingMoney(b.FundingCommitted, b.FundingCurrency, false))
	}
	p.Details = []string{
		fmt.Sprintf("%s trade-date cash %s (%s at the ledger rate %s); the repayment lands it between 0 and %s (cushion %s), never above",
			b.Currency, currencyLevelingMoney(b.Cash, b.Currency, true), currencyLevelingMoney(b.Cash*b.ExchangeRate, base, true), strconv.FormatFloat(b.ExchangeRate, 'f', 6, 64),
			currencyLevelingMoney(b.CushionBase/b.ExchangeRate, b.Currency, true), currencyLevelingMoney(b.CushionBase, base, false)),
		funding + fmt.Sprintf("; this conversion may spend at most %s of it and leaves it about %s", currencyLevelingMoney(b.Allotment, b.FundingCurrency, false), currencyLevelingMoney(b.FundingCash-b.Spent, b.FundingCurrency, true)),
		fmt.Sprintf("%s %s %s on IDEALPRO (contract %d), sized at %s from the ledger rates; the preview reads a live bid and ask and sends a limit at most %s bp from the mid, which can bring in no more than this conversion's share of the target",
			leg.action, briefThousands(float64(leg.quantity), 0), b.Pair, leg.contract.ConID, strconv.FormatFloat(b.PlanningPrice, 'f', 5, 64), strconv.FormatFloat(b.MaxSlippageBP, 'f', -1, 64)),
		currencyLevelingRatesLine(b, base),
		fmt.Sprintf("the loan's conversions together are held to the order cap in force, %s; FX spot settles in two days and the margin interest stops on the settled balance", currencyLevelingMoney(b.OrderCapBase, base, false)),
	}
	if b.Legs > 1 {
		p.Details = append(p.Details, fmt.Sprintf("conversion %d of %d repaying the %s loan, cheapest currency first; the %d are approved and sent together", b.Leg, b.Legs, b.Currency, b.Legs))
	}
	if b.HeldToCap {
		p.Details = append(p.Details, "held to the order cap in force; the next cycle repays the rest")
	}
	if b.FundingShort {
		p.Details = append(p.Details, "the currencies allowed to pay hold less than the loan; leveling never borrows one currency to repay another")
	}
	approval := "you approve each conversion; leveling is never pre-authorised"
	if b.Legs > 1 {
		approval = "you approve the repayment as a whole; leveling is never pre-authorised"
	}
	p.Details = append(p.Details, approval)
	return p
}

// currencyLevelingRatesLine states the rates a conversion was weighed on,
// where they come from and what it saves against what it can cost.
func currencyLevelingRatesLine(b *rpc.TradeProposalCurrencyLeveling, base string) string {
	source := "the broker's statements to " + b.LoanRateThrough
	if b.FundingRateThrough != b.LoanRateThrough {
		source = fmt.Sprintf("the broker's statements (%s to %s, %s to %s)", b.Currency, b.LoanRateThrough, b.FundingCurrency, b.FundingRateThrough)
	}
	line := fmt.Sprintf("the %s loan costs %s a year and %s cash earns %s, from %s; within %d days this conversion saves about %s and costs at most %s",
		b.Currency, currencyLevelingPct(b.LoanRate), b.FundingCurrency, currencyLevelingPct(b.FundingRate), source,
		b.PaybackDays, currencyLevelingMoney(b.SavingBase, base, false), currencyLevelingMoney(b.CostBase, base, false))
	if b.LoanRateBound {
		line += fmt.Sprintf("; %s was not borrowed lately, so what its cash earns stands in for what the loan costs", b.Currency)
	}
	if b.FundingRateBound {
		line += fmt.Sprintf("; %s held no cash lately, so what its loan costs stands in for what its cash earns", b.FundingCurrency)
	}
	return line
}

// currencyLevelingReason is the row's one short line for Desk: what, why.
// received is what the loan's conversions bring in together.
func currencyLevelingReason(b *rpc.TradeProposalCurrencyLeveling, base string, received float64) string {
	part := ""
	if b.Legs > 1 {
		part = fmt.Sprintf(" (%d of %d for the %s loan)", b.Leg, b.Legs, b.Currency)
	}
	brings := "this brings it"
	if b.Legs > 1 {
		brings = fmt.Sprintf("the %d bring it", b.Legs)
	}
	return fmt.Sprintf("Convert %s into about %s%s: %s cash is %s, a margin loan beyond the %s band; %s to about %s",
		currencyLevelingMoney(b.Spent, b.FundingCurrency, false), currencyLevelingMoney(b.Received, b.Currency, false), part, b.Currency,
		currencyLevelingMoney(b.Cash, b.Currency, true), currencyLevelingMoney(b.TriggerBase, base, false), brings, currencyLevelingMoney(b.Cash+received, b.Currency, true))
}

// currencyLevelingIdentity names a conversion across generations: the
// borrowed currency, the pair and the side.
func currencyLevelingIdentity(ccy, pair, action string) string {
	return ccy + "|" + pair + "|" + action
}

// currencyLevelingKey is a row's stable key: its identity and the pair's
// contract id, so a preview or submit of a key converts on the contract the
// owner saw. Amounts change the row's revision, never its key.
func currencyLevelingKey(ccy, pair, action string, conID int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{rpc.TradeProposalBucketCurrencyLeveling, currencyLevelingIdentity(ccy, pair, action), strconv.Itoa(conID)}, "|")))
	return rpc.TradeProposalBucketCurrencyLeveling + ":" + hex.EncodeToString(sum[:8])
}

// currencyLevelingBundleID names a loan's set of conversions: the loan and
// each conversion's identity and contract id, in order.
func currencyLevelingBundleID(bundle currencyLevelingBundle) string {
	parts := []string{rpc.TradeProposalBucketCurrencyLeveling, "bundle", bundle.currency}
	for _, leg := range bundle.legs {
		parts = append(parts, currencyLevelingIdentity(bundle.currency, leg.contract.LocalSymbol, leg.action), strconv.Itoa(leg.contract.ConID))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return rpc.TradeProposalBucketCurrencyLeveling + "-bundle:" + hex.EncodeToString(sum[:8])
}

// currencyLevelingBundleRevision binds a bundle to its rows' keys and
// revisions, in send order.
func currencyLevelingBundleRevision(rows []rpc.TradeProposal) string {
	parts := []string{"currency-leveling-bundle-v1"}
	for _, row := range rows {
		parts = append(parts, row.Key, row.Revision)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:16])
}

// currencyLevelingBundleRevisions fills each served bundle's revision once
// the rows' revisions are known.
func currencyLevelingBundleRevisions(st *rpc.TradeProposalCurrencyLevelingStatus, proposals []rpc.TradeProposal) {
	if st == nil {
		return
	}
	byKey := map[string]rpc.TradeProposal{}
	for _, p := range proposals {
		if p.Bucket == rpc.TradeProposalBucketCurrencyLeveling {
			byKey[p.Key] = p
		}
	}
	for i := range st.Bundles {
		rows := make([]rpc.TradeProposal, 0, len(st.Bundles[i].Keys))
		for _, key := range st.Bundles[i].Keys {
			rows = append(rows, byKey[key])
		}
		st.Bundles[i].Revision = currencyLevelingBundleRevision(rows)
	}
}

// currencyLevelingCounts counts the bucket's rows.
func currencyLevelingCounts(proposals []rpc.TradeProposal) int {
	n := 0
	for _, p := range proposals {
		if p.Bucket == rpc.TradeProposalBucketCurrencyLeveling {
			n++
		}
	}
	return n
}
