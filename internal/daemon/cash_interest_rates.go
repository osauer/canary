package daemon

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Broker cash interest rates (rpc.CashInterestRates; owner decision
// 2026-10-11 14:07 CEST: credit and debit rates are the broker's live rates,
// an entered number is a bug). They are read from the "Interest Details
// (Tiers)" rows of the retained Flex statements, the latest generation of
// each row winning, and kept per broker scope for an hour or until a Flex
// fetch brings a new statement. The account read annotates every currency
// row with them, so the cash sweep's economics, the policy check and Desk
// read the broker's rate from one place. Reading the statements takes the
// retained-statement lock, so the account read never waits for it: a cold
// or expired cache is refreshed in the background and, until the first read
// completes, the rates report that the statements have not been read yet.

const (
	cashInterestRatesTTL       = time.Hour
	cashInterestRefreshTimeout = 2 * time.Minute

	cashInterestSideCredit = "credit"
	cashInterestSideDebit  = "debit"
)

// cashInterestTierRow is one parsed tier with its side and the broker's
// generation time of the statement it came from.
type cashInterestTierRow struct {
	flexstmt.InterestTier
	Side        string
	GeneratedAt time.Time
}

// cashInterestTiersCache keeps the parsed tiers for one broker scope.
type cashInterestTiersCache struct {
	mu          sync.Mutex
	scope       brokerStateScope
	at          time.Time
	stale, busy bool
	rows        []cashInterestTierRow
	sectionSeen bool
	reason      string
	// statements replaces the retained-statement read in tests.
	statements func(ctx context.Context) ([]flexstmt.Statement, error)
}

// invalidate marks the cache for a re-read: a Flex fetch brought a new
// statement. The rows it holds stay served meanwhile; their value dates
// carry the clock.
func (c *cashInterestTiersCache) invalidate() {
	c.mu.Lock()
	c.stale = true
	c.mu.Unlock()
}

// cashInterestTiers returns the cached tiers for the current scope and
// starts a background read when the cache is cold, expired, stale or for
// another scope. readAt is when the rows were read; a zero readAt with a
// reason means nothing has been read for this scope yet.
func (s *Server) cashInterestTiers(now time.Time) (rows []cashInterestTierRow, sectionSeen bool, reason string, readAt time.Time) {
	c := &s.cashInterest
	scope := s.currentBrokerStateScope()
	c.mu.Lock()
	defer c.mu.Unlock()
	same := !c.at.IsZero() && sameBrokerScope(c.scope, scope)
	fresh := same && !c.stale && now.Sub(c.at) >= 0 && now.Sub(c.at) < cashInterestRatesTTL
	if !fresh && !c.busy {
		c.busy = true
		go s.refreshCashInterestTiers(scope)
	}
	if !same {
		return nil, false, "the broker's statements have not been read for the rates yet", time.Time{}
	}
	return c.rows, c.sectionSeen, c.reason, c.at
}

func (s *Server) refreshCashInterestTiers(scope brokerStateScope) {
	parent := context.Background()
	s.mu.Lock()
	if s.serverCtx != nil {
		parent = s.serverCtx
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, cashInterestRefreshTimeout)
	defer cancel()
	s.readCashInterestTiers(ctx, scope)
}

// readCashInterestTiers reads the retained statements for scope now and
// replaces the cache; tests call it directly.
func (s *Server) readCashInterestTiers(ctx context.Context, scope brokerStateScope) {
	c := &s.cashInterest
	c.mu.Lock()
	read := c.statements
	c.mu.Unlock()
	if read == nil {
		read = s.fxStatements
	}
	var rows []cashInterestTierRow
	seen, reason := false, ""
	if statements, err := read(ctx); err != nil {
		reason = "the broker's statements are unavailable (" + err.Error() + ")"
	} else {
		rows, seen = cashInterestTiersFrom(statements)
	}
	c.mu.Lock()
	c.scope, c.at, c.rows, c.sectionSeen, c.reason = scope, s.nowUTC(), rows, seen, reason
	c.stale, c.busy = false, false
	c.mu.Unlock()
}

// cashInterestTiersFrom collects every tier row of every statement that
// carries the section, the latest generation winning per currency, type,
// value date and tier, sorted for stable output. sectionSeen is false while
// no statement carries the section at all.
func cashInterestTiersFrom(statements []flexstmt.Statement) (rows []cashInterestTierRow, sectionSeen bool) {
	latest := map[string]cashInterestTierRow{}
	for _, st := range statements {
		t := st.InterestTiers
		if t == nil || !t.Present {
			continue
		}
		sectionSeen = true
		for _, tier := range t.Rows {
			side := cashInterestTierSide(tier)
			if side == "" {
				continue
			}
			key := strings.Join([]string{tier.Currency, tier.Type, tier.ValueDate.Format(time.DateOnly), tier.Tier}, "|")
			if old, ok := latest[key]; ok && !st.WhenGenerated.After(old.GeneratedAt) {
				continue
			}
			latest[key] = cashInterestTierRow{InterestTier: tier, Side: side, GeneratedAt: st.WhenGenerated}
		}
	}
	for _, row := range latest {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b cashInterestTierRow) int {
		return cmp.Or(cmp.Compare(a.Currency, b.Currency), cmp.Compare(a.Side, b.Side), a.ValueDate.Compare(b.ValueDate), cashInterestTierOrder(a.InterestTier, b.InterestTier))
	})
	return rows, sectionSeen
}

// cashInterestTierSide tells a credit tier from a debit tier by the
// broker's signs: a negative principal or interest is a loan. A row that
// carries neither sign is classified by its interest type; interest on
// short-sale proceeds is not cash-balance interest and is left out.
func cashInterestTierSide(tier flexstmt.InterestTier) string {
	typ := strings.ToLower(tier.Type)
	if strings.Contains(typ, "short") {
		return ""
	}
	for _, v := range []*float64{tier.Principal, tier.Interest} {
		switch {
		case v == nil || *v == 0:
			continue
		case *v < 0:
			return cashInterestSideDebit
		default:
			return cashInterestSideCredit
		}
	}
	switch {
	case strings.Contains(typ, "debit"):
		return cashInterestSideDebit
	case strings.Contains(typ, "credit"):
		return cashInterestSideCredit
	}
	return ""
}

// cashInterestTierOrder sorts tiers by threshold, then by tier number.
func cashInterestTierOrder(a, b flexstmt.InterestTier) int {
	ta, tb := math.Inf(1), math.Inf(1)
	if a.Threshold != nil {
		ta = math.Abs(*a.Threshold)
	}
	if b.Threshold != nil {
		tb = math.Abs(*b.Threshold)
	}
	if c := cmp.Compare(ta, tb); c != 0 {
		return c
	}
	na, ea := strconv.Atoi(a.Tier)
	nb, eb := strconv.Atoi(b.Tier)
	if ea == nil && eb == nil {
		return cmp.Compare(na, nb)
	}
	return cmp.Compare(a.Tier, b.Tier)
}

// cashInterestRatesFor is one currency's rates from the cached tiers: the
// credit side for cash (a negative balance holds no cash) and the debit side
// for the amount borrowed. cash is the ledger's current balance, nil when
// not observed.
func cashInterestRatesFor(rows []cashInterestTierRow, sectionSeen bool, reason, ccy string, cash *float64, readAt, now time.Time) rpc.CashInterestRates {
	var credit, debit *float64
	if cash != nil && finiteProtectionOptionPolicyValue(*cash) {
		credit, debit = new(max(*cash, 0)), new(max(-*cash, 0))
	}
	return rpc.CashInterestRates{Source: rpc.CashInterestSourceFlexTiers, ReadAt: readAt,
		Credit: cashInterestSideFor(rows, sectionSeen, reason, ccy, cashInterestSideCredit, credit, now),
		Debit:  cashInterestSideFor(rows, sectionSeen, reason, ccy, cashInterestSideDebit, debit, now)}
}

func cashInterestSideFor(rows []cashInterestTierRow, sectionSeen bool, reason, ccy, side string, balance *float64, now time.Time) rpc.CashInterestSide {
	out := rpc.CashInterestSide{State: rpc.CashInterestStateUnavailable}
	if reason != "" {
		out.Reason = reason
		return out
	}
	if !sectionSeen {
		out.Reason = "no retained statement carries the Interest Details (Tiers) section yet"
		return out
	}
	var mine []cashInterestTierRow
	var latest time.Time
	for _, row := range rows {
		if row.Currency == ccy && row.Side == side {
			mine = append(mine, row)
			if row.ValueDate.After(latest) {
				latest = row.ValueDate
			}
		}
	}
	if len(mine) == 0 {
		if side == cashInterestSideDebit {
			out.Reason = fmt.Sprintf("no %s debit row in the broker's statements: the account has not borrowed %s on a statement day", ccy, ccy)
		} else {
			out.Reason = fmt.Sprintf("no %s credit row in the broker's statements: the account held no %s cash on a statement day", ccy, ccy)
		}
		return out
	}
	out.LastValueDate = latest.Format(time.DateOnly)
	if age := businessDaysApart(latest, now); age > rpc.CashInterestMaxValueDateBusinessDays {
		out.Reason = fmt.Sprintf("the latest %s %s row is dated %s, %d business days ago, so the live rate is unknown", ccy, side, out.LastValueDate, age)
		return out
	}
	var tiers []rpc.CashInterestTier
	var generated time.Time
	principal := 0.0
	for _, row := range mine {
		if !row.ValueDate.Equal(latest) {
			continue
		}
		tiers = append(tiers, rpc.CashInterestTier{Tier: row.Tier, Threshold: cloneFloat64Ptr(row.Threshold), Rate: *row.RatePct / 100,
			Principal: cloneFloat64Ptr(row.Principal), Interest: cloneFloat64Ptr(row.Interest)})
		if row.Principal != nil {
			principal += math.Abs(*row.Principal)
		}
		if row.GeneratedAt.After(generated) {
			generated = row.GeneratedAt
		}
	}
	source := rpc.CashInterestBalanceLedger
	if balance == nil {
		balance, source = new(principal), rpc.CashInterestBalanceStatement
	}
	pick := cashInterestPick(tiers, *balance)
	out.State, out.Reason, out.LastValueDate = rpc.CashInterestStateAvailable, "", ""
	out.Rate, out.Tier, out.Balance, out.BalanceSource = new(pick.Rate), pick.Tier, balance, source
	out.ValueDate, out.GeneratedAt, out.Tiers = latest.Format(time.DateOnly), generated, tiers
	return out
}

// cashInterestPick is the tier a balance's top unit falls in: the highest
// threshold at or below the balance. Below every threshold it is the lowest
// tier; without thresholds it is the highest tier the broker put principal
// in on the value date.
func cashInterestPick(tiers []rpc.CashInterestTier, balance float64) rpc.CashInterestTier {
	chosen, thresholds := -1, false
	for i, t := range tiers {
		if t.Threshold == nil {
			continue
		}
		thresholds = true
		if math.Abs(*t.Threshold) <= balance+1e-9 {
			chosen = i
		}
	}
	if !thresholds {
		for i, t := range tiers {
			if t.Principal != nil && *t.Principal != 0 {
				chosen = i
			}
		}
	}
	return tiers[max(chosen, 0)]
}

// annotateCashInterest attaches the broker's rates to every currency row of
// an account read, for the ledger's observed cash.
func (s *Server) annotateCashInterest(res *rpc.AccountResult) {
	if s == nil || res == nil {
		return
	}
	now := s.nowUTC()
	rows, seen, reason, readAt := s.cashInterestTiers(now)
	annotateCashInterestRows(res, rows, seen, reason, readAt, now)
}

func annotateCashInterestRows(res *rpc.AccountResult, rows []cashInterestTierRow, sectionSeen bool, reason string, readAt, now time.Time) {
	annotate := func(row *rpc.CurrencyExposure) {
		var cash *float64
		if row.CashObserved {
			cash = new(row.CashCcy)
		}
		rates := cashInterestRatesFor(rows, sectionSeen, reason, normCcy(row.Currency), cash, readAt, now)
		row.Interest = &rates
	}
	for i := range res.CurrencyExposure {
		annotate(&res.CurrencyExposure[i])
	}
	if res.BaseCurrencyLedger != nil {
		annotate(res.BaseCurrencyLedger)
	}
}

// cashInterestCreditSide is the credit side a sweep row and the policy check
// read; a currency whose rates were not read with the account says so.
func cashInterestCreditSide(rates *rpc.CashInterestRates, ccy string) *rpc.CashInterestSide {
	if rates == nil {
		return &rpc.CashInterestSide{State: rpc.CashInterestStateUnavailable, Reason: fmt.Sprintf("the broker's %s rates were not read with the account", ccy)}
	}
	return rpc.CloneCashInterestSide(&rates.Credit)
}

// cashInterestRate is the usable annual decimal rate of a side, or why there
// is none. A negative broker rate is real but the benefit estimate does not
// model it.
func cashInterestRate(side *rpc.CashInterestSide, ccy string) (*float64, string) {
	switch {
	case side == nil:
		return nil, fmt.Sprintf("the broker's %s rates were not read with the account", ccy)
	case side.State != rpc.CashInterestStateAvailable || side.Rate == nil || !finiteProtectionOptionPolicyValue(*side.Rate):
		return nil, nonEmptyString(side.Reason, fmt.Sprintf("the broker's %s cash rate is unavailable", ccy))
	case *side.Rate < 0:
		return nil, fmt.Sprintf("the broker's %s cash rate is negative (%s a year, value date %s); the benefit estimate does not model a negative rate", ccy, policyCheckPct(*side.Rate), side.ValueDate)
	}
	return new(*side.Rate), ""
}

// cashInterestWords is one line about a side for a row's details.
func cashInterestWords(side *rpc.CashInterestSide, ccy string) string {
	rate, reason := cashInterestRate(side, ccy)
	if rate == nil {
		return "cash rate unavailable: " + reason
	}
	return fmt.Sprintf("cash rate %s a year (broker, tier %s, value date %s)", policyCheckPct(*rate), side.Tier, side.ValueDate)
}
