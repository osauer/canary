package risk

import (
	"fmt"
	"math"
	"strings"
)

// SizeInput is the validated input to ComputeSize. Fields mirror the CLI
// flags one-for-one so the pure function is testable without the runner.
//
// Target is optional. When set, ComputeSize derives the R-multiple
// (reward-to-risk ratio = profit-to-stop distance / entry-to-stop
// distance) and the breakeven win rate. Long trades require target >
// entry; short trades require target < entry.
type SizeInput struct {
	Symbol      string
	Side        string  // "long" | "short"
	Entry       float64 // quote currency
	Stop        float64 // quote currency
	Target      float64 // optional take-profit; 0 disables the R block
	RiskPct     float64 // percent of NLV; (0, 100]
	Lot         int     // round shares down to this multiple; >= 1
	FX          float64 // quote-currency units per 1 base-currency unit; > 0
	NLV         float64 // base currency
	BuyingPower float64 // base currency (0 disables BP check)
	Currency    string  // base currency code, surfaced in output only
}

// SizeResult is the wire shape of `canary size --json` and the input to the CLI
// text renderer.
//
// Target / R / RewardQuote / RewardBase / BreakevenWinRate are populated
// only when the input carries a non-zero Target. R is the reward-to-risk
// ratio (|target − entry| / |entry − stop|). BreakevenWinRate is 1 / (1 + R).
type SizeResult struct {
	Symbol           string   `json:"symbol"`
	Side             string   `json:"side"`
	Entry            float64  `json:"entry"`
	Stop             float64  `json:"stop"`
	Target           *float64 `json:"target,omitempty"`
	RiskPct          float64  `json:"risk_pct"`
	Lot              int      `json:"lot"`
	FX               float64  `json:"fx"`
	NLV              float64  `json:"nlv"`
	BaseCurrency     string   `json:"base_currency,omitempty"`
	RiskBase         float64  `json:"risk_base"`  // NLV * pct/100
	RiskQuote        float64  `json:"risk_quote"` // RiskBase * fx
	PerShareRisk     float64  `json:"per_share_risk"`
	Shares           int      `json:"shares"`
	Notional         float64  `json:"notional"`    // shares * entry
	MaxLoss          float64  `json:"max_loss"`    // shares * per_share_risk (quote ccy)
	R                *float64 `json:"r,omitempty"` // (|target-entry|) / (|entry-stop|)
	RewardQuote      *float64 `json:"reward_quote,omitempty"`
	RewardBase       *float64 `json:"reward_base,omitempty"`
	BreakevenWinRate *float64 `json:"breakeven_win_rate,omitempty"` // 1 / (1+R), fraction
	Status           string   `json:"status"`                       // "ok" | "tight_risk" | "exceeds_buying_power"
}

// ComputeSize validates and applies fixed-fractional sizing. It returns a
// result with zero shares and status tight_risk when the risk budget is below
// one lot; buying power excess is reported in Status rather than as an error.
func ComputeSize(in SizeInput) (SizeResult, error) {
	side, err := validateSizePlan(in, true)
	if err != nil {
		return SizeResult{}, err
	}

	perShare := math.Abs(in.Entry - in.Stop)
	if perShare == 0 {
		// Defensive — the side-vs-stop checks in validateSizePlan should make this unreachable.
		return SizeResult{}, fmt.Errorf("per-share risk is zero (entry == stop)")
	}

	riskBase := in.NLV * in.RiskPct / 100
	riskQuote := riskBase * in.FX
	rawShares := riskQuote / perShare
	shares := int(math.Floor(rawShares/float64(in.Lot))) * in.Lot

	res := SizeResult{
		Symbol:       strings.ToUpper(in.Symbol),
		Side:         side,
		Entry:        in.Entry,
		Stop:         in.Stop,
		RiskPct:      in.RiskPct,
		Lot:          in.Lot,
		FX:           in.FX,
		NLV:          in.NLV,
		BaseCurrency: in.Currency,
		RiskBase:     riskBase,
		RiskQuote:    riskQuote,
		PerShareRisk: perShare,
		Shares:       shares,
		Notional:     float64(shares) * in.Entry,
		MaxLoss:      float64(shares) * perShare,
		Status:       "ok",
	}

	if in.Target != 0 {
		tgt := in.Target
		res.Target = &tgt
		perShareReward := math.Abs(in.Target - in.Entry)
		r := perShareReward / perShare
		res.R = &r
		rewardQuote := float64(shares) * perShareReward
		rewardBase := rewardQuote / in.FX
		res.RewardQuote = &rewardQuote
		res.RewardBase = &rewardBase
		be := 1.0 / (1.0 + r)
		res.BreakevenWinRate = &be
	}

	if shares == 0 {
		res.Status = "tight_risk"
		return res, nil
	}
	if in.BuyingPower > 0 && res.Notional > in.BuyingPower*in.FX {
		res.Status = "exceeds_buying_power"
	}
	return res, nil
}

// ValidateSizePlan checks the flag-level plan shape (side, prices, target,
// risk percent, lot, fx) without requiring account data. ComputeSize re-runs
// the full validation with NLV required; this exists so a runner can fail
// fast on bad input before paying an account-snapshot round-trip.
func ValidateSizePlan(in SizeInput) error {
	_, err := validateSizePlan(in, false)
	return err
}

func validateSizePlan(in SizeInput, requireAccount bool) (string, error) {
	side := strings.ToLower(strings.TrimSpace(in.Side))
	if side == "" {
		side = "long"
	}
	if side != "long" && side != "short" {
		return "", fmt.Errorf("side must be long or short (got %q)", in.Side)
	}
	if in.Symbol == "" {
		return "", fmt.Errorf("symbol is required")
	}
	if in.Entry <= 0 {
		return "", fmt.Errorf("entry must be > 0 (got %v)", in.Entry)
	}
	if in.Stop <= 0 {
		return "", fmt.Errorf("stop must be > 0 (got %v)", in.Stop)
	}
	if side == "long" && in.Stop >= in.Entry {
		return "", fmt.Errorf("long trade requires stop (%v) < entry (%v)", in.Stop, in.Entry)
	}
	if side == "short" && in.Stop <= in.Entry {
		return "", fmt.Errorf("short trade requires stop (%v) > entry (%v)", in.Stop, in.Entry)
	}
	if in.Target != 0 {
		if in.Target < 0 {
			return "", fmt.Errorf("target must be > 0 (got %v)", in.Target)
		}
		if side == "long" && in.Target <= in.Entry {
			return "", fmt.Errorf("long trade requires target (%v) > entry (%v)", in.Target, in.Entry)
		}
		if side == "short" && in.Target >= in.Entry {
			return "", fmt.Errorf("short trade requires target (%v) < entry (%v)", in.Target, in.Entry)
		}
	}
	if in.RiskPct <= 0 || in.RiskPct > 100 {
		return "", fmt.Errorf("risk-pct must be in (0, 100] (got %v)", in.RiskPct)
	}
	if in.Lot < 1 {
		return "", fmt.Errorf("lot must be >= 1 (got %v)", in.Lot)
	}
	if in.FX <= 0 {
		return "", fmt.Errorf("fx must be > 0 (got %v)", in.FX)
	}
	if requireAccount && in.NLV <= 0 {
		return "", fmt.Errorf("nlv must be > 0 (got %v) — is the gateway connected?", in.NLV)
	}
	return side, nil
}
