package risk

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PortfolioPlanContract names an advisory target-band plan, never order authority.
const PortfolioPlanContract = "target-band-plan-v1"

func portfolioPlanLimits(c *Constitution) []ConstitutionLimit {
	if c == nil || c.PortfolioPlan == nil {
		return nil
	}
	p := c.PortfolioPlan
	rows := []ConstitutionLimit{}
	add := func(key, value, meaning string) {
		rows = append(rows, ConstitutionLimit{Key: key, Value: value, Meaning: meaning, Source: "file", Enforcement: "advisory"})
	}
	add("portfolio_plan.contract", p.Contract, "Advisory target-band planning only; grants no order authority.")
	add("portfolio_plan.valid_until", p.ValidUntil.Format(time.RFC3339), "No plan is selected at or after this mandate expiry.")
	for i, t := range p.Targets {
		prefix := fmt.Sprintf("portfolio_plan.targets[%d].", i)
		add(prefix+"symbol", t.Symbol, "Exact long stock held or on the accepted watchlist.")
		add(prefix+"con_id", strconv.Itoa(t.ConID), "Broker contract identity; a ticker alone cannot match.")
		add(prefix+"currency", t.Currency, "Trading currency; the planner creates no conversion.")
		add(prefix+"priority", strconv.Itoa(t.Priority), "Lower positive priority is considered first; priorities must be distinct.")
		for _, band := range []struct {
			key     string
			value   *float64
			meaning string
		}{
			{"lower_pct_nlv", t.LowerPctNLV, "Below this stock market-value share of NLV, consider an addition."},
			{"target_pct_nlv", t.TargetPctNLV, "Desired resulting stock market-value share of NLV; order checks may permit less."},
			{"upper_pct_nlv", t.UpperPctNLV, "Above this band, withhold additions pending a supported reduction review."},
			{"limit_price", t.LimitPrice, "Explicit maximum purchase price per share in the trading currency."},
		} {
			value := "unapproved"
			if band.value != nil {
				value = strconv.FormatFloat(*band.value, 'g', -1, 64)
			}
			add(prefix+band.key, value, band.meaning)
		}
		add(prefix+"entry_regimes", strings.Join(t.EntryRegimes, ", "), "Only these current regime stages may admit this target's additions.")
		add(prefix+"reason", t.Reason, "Owner's stated reason for this target; never supplied by a model.")
	}
	return rows
}

// PortfolioPlanPolicy is an optional owner-authored planning mandate. Its expiry,
// targets, entry regimes and priority are explicit; installation supplies none.
type PortfolioPlanPolicy struct {
	Contract   string                `toml:"contract" json:"contract"`
	ValidUntil time.Time             `toml:"valid_until" json:"valid_until"`
	Targets    []PortfolioPlanTarget `toml:"targets" json:"targets"`
}

// PortfolioPlanTarget applies to one exact long stock. Bands measure stock
// market value / NLV; existing option exposure is checked separately by Add.
type PortfolioPlanTarget struct {
	Symbol       string   `toml:"symbol" json:"symbol"`
	ConID        int      `toml:"con_id" json:"con_id"`
	Currency     string   `toml:"currency" json:"currency"`
	Priority     int      `toml:"priority" json:"priority"`
	LowerPctNLV  *float64 `toml:"lower_pct_nlv" json:"lower_pct_nlv"`
	TargetPctNLV *float64 `toml:"target_pct_nlv" json:"target_pct_nlv"`
	UpperPctNLV  *float64 `toml:"upper_pct_nlv" json:"upper_pct_nlv"`
	LimitPrice   *float64 `toml:"limit_price" json:"limit_price"`
	EntryRegimes []string `toml:"entry_regimes" json:"entry_regimes"`
	Reason       string   `toml:"reason" json:"reason"`
}

func (p *PortfolioPlanPolicy) validate() error {
	if p == nil {
		return nil
	}
	if p.Contract != PortfolioPlanContract || p.ValidUntil.IsZero() || len(p.Targets) == 0 {
		return fmt.Errorf("portfolio_plan requires target-band-plan-v1, valid_until and explicit targets")
	}
	ids, symbols, priorities := map[int]bool{}, map[string]bool{}, map[int]bool{}
	for _, t := range p.Targets {
		if t.Symbol == "" || t.Symbol != strings.ToUpper(strings.TrimSpace(t.Symbol)) || t.ConID <= 0 || t.Priority <= 0 || len(t.Currency) != 3 || t.Currency != strings.ToUpper(t.Currency) || strings.TrimSpace(t.Reason) == "" {
			return fmt.Errorf("portfolio_plan target needs exact identity, uppercase currency, positive priority and reason")
		}
		if ids[t.ConID] || symbols[t.Symbol] || priorities[t.Priority] {
			return fmt.Errorf("portfolio_plan target identities and priorities must be distinct")
		}
		ids[t.ConID], symbols[t.Symbol], priorities[t.Priority] = true, true, true
		if t.LowerPctNLV == nil || t.TargetPctNLV == nil || t.UpperPctNLV == nil || t.LimitPrice == nil {
			return fmt.Errorf("portfolio_plan target bands and price ceiling must be explicit")
		}
		for _, v := range []float64{*t.LowerPctNLV, *t.TargetPctNLV, *t.UpperPctNLV, *t.LimitPrice} {
			if !finiteStockAdd(v) || v < 0 {
				return fmt.Errorf("portfolio_plan target numbers must be finite and nonnegative")
			}
		}
		if *t.LowerPctNLV > *t.TargetPctNLV || *t.TargetPctNLV > *t.UpperPctNLV || *t.UpperPctNLV > 100 || *t.LimitPrice <= 0 {
			return fmt.Errorf("portfolio_plan requires 0 <= lower <= target <= upper <= 100 and a positive limit")
		}
		if len(t.EntryRegimes) == 0 {
			return fmt.Errorf("portfolio_plan target requires explicit entry_regimes")
		}
		seen := map[string]bool{}
		for _, stage := range t.EntryRegimes {
			if !slices.Contains([]string{RegimeBucketCalm, RegimeBucketEarlyWarning, RegimeBucketConfirmed}, stage) || seen[stage] {
				return fmt.Errorf("portfolio_plan entry_regimes must be distinct canonical regime stages")
			}
			seen[stage] = true
		}
	}
	return nil
}

// PortfolioTargetEvidence is assembled by the daemon, not accepted from callers.
// A missing current stock holding can be zero only after the whole book is proved.
type PortfolioTargetEvidence struct {
	ConID      int
	Symbol     string
	Currency   string
	Quantity   float64
	Mark       float64
	FX         float64
	InUniverse bool
	Complete   bool
}

// PortfolioIntent explains a target-relative decision before order sizing.
// DesiredQuantity is not an executable quantity and consumes no allowance.
type PortfolioIntent struct {
	ConID           int      `json:"con_id"`
	Symbol          string   `json:"symbol"`
	Currency        string   `json:"currency"`
	Priority        int      `json:"priority"`
	Decision        string   `json:"decision"`
	Reason          string   `json:"reason"`
	QuantityBefore  *float64 `json:"quantity_before,omitempty"`
	StockPctNLV     *float64 `json:"stock_pct_nlv,omitempty"`
	TargetPctNLV    *float64 `json:"target_pct_nlv,omitempty"`
	DesiredQuantity int      `json:"desired_quantity,omitempty"`
	LimitPrice      *float64 `json:"limit_price,omitempty"`
}

// EvaluatePortfolioTargets orders explicit targets by owner priority. It never
// chooses a maximum, assumes missing holdings are empty, or evaluates execution.
func EvaluatePortfolioTargets(policy *PortfolioPlanPolicy, now time.Time, nlv *float64, regime string, evidence []PortfolioTargetEvidence) ([]PortfolioIntent, error) {
	if policy == nil {
		return nil, fmt.Errorf("portfolio target ranges, entry conditions and priorities have not been specified")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	targets := slices.Clone(policy.Targets)
	slices.SortFunc(targets, func(a, b PortfolioPlanTarget) int { return cmp.Compare(a.Priority, b.Priority) })
	rows := make([]PortfolioIntent, 0, len(targets))
	for _, t := range targets {
		r := PortfolioIntent{ConID: t.ConID, Symbol: t.Symbol, Currency: t.Currency, Priority: t.Priority, Decision: "cannot_evaluate", TargetPctNLV: new(*t.TargetPctNLV), LimitPrice: new(*t.LimitPrice)}
		switch {
		case now.IsZero() || !now.Before(policy.ValidUntil):
			r.Reason = "The portfolio planning mandate has expired."
		case nlv == nil || !finiteStockAdd(*nlv) || *nlv <= 0:
			r.Reason = "Current account value is unavailable."
		default:
			matches := []PortfolioTargetEvidence{}
			for _, e := range evidence {
				if e.ConID == t.ConID || e.Symbol == t.Symbol {
					matches = append(matches, e)
				}
			}
			if len(matches) != 1 || matches[0].ConID != t.ConID || matches[0].Symbol != t.Symbol || matches[0].Currency != t.Currency || !matches[0].Complete || !matches[0].InUniverse {
				r.Reason = "An exact current holding or watched stock and complete portfolio evidence are required."
				break
			}
			e := matches[0]
			if !finiteStockAdd(e.Quantity) || e.Quantity < 0 || !finiteStockAdd(e.FX) || e.FX <= 0 || !finiteStockAdd(e.Mark) || (e.Quantity > 0 && e.Mark <= 0) {
				r.Reason = "Current long-stock valuation and currency conversion are unavailable."
				break
			}
			pct := e.Quantity * e.Mark * e.FX / *nlv * 100
			if !finiteStockAdd(pct) {
				r.Reason = "Stock allocation could not be measured."
				break
			}
			r.QuantityBefore, r.StockPctNLV = new(e.Quantity), new(pct)
			switch {
			case pct > *t.UpperPctNLV:
				r.Decision, r.Reason = "review_reduction", "Stock allocation exceeds its target band; use a supported reduction review before adding exposure."
			case pct >= *t.LowerPctNLV:
				r.Decision, r.Reason = "hold", "Stock allocation is within its approved target band."
			case !slices.Contains([]string{RegimeBucketCalm, RegimeBucketEarlyWarning, RegimeBucketConfirmed}, regime):
				r.Reason = "Current regime evidence is unavailable."
			case !slices.Contains(t.EntryRegimes, regime):
				r.Decision, r.Reason = "entry_withheld", "The current regime does not permit this target's entry rule."
			default:
				// Measure the desired resulting stock position at the greater of
				// its current mark and the owner's ceiling. Round the remaining
				// addition down so fractional holdings cannot overshoot the target.
				unit := max(e.Mark, *t.LimitPrice) * e.FX
				q := math.Floor(*nlv**t.TargetPctNLV/100/unit - e.Quantity)
				if !finiteStockAdd(q) || q > 1000000 {
					r.Reason = "The desired addition exceeds the order contract's quantity representation."
					break
				}
				if q < 1 {
					r.Decision, r.Reason = "hold", "The remaining target allocation cannot accommodate a whole share at the price ceiling."
					break
				}
				r.Decision, r.Reason, r.DesiredQuantity = "add", t.Reason, int(q)
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}
