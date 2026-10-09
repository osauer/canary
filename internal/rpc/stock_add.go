package rpc

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
)

// Stock-add methods share the existing exact-order execution path. Planning
// never mints a token; preview still needs the normal separate confirmation.
const (
	MethodAddPlan    = "add.plan"
	MethodAddPreview = "add.preview"
)

// AddParams requires an explicit maximum or exact additional quantity.
// Current size is daemon-owned.
type AddParams struct {
	Max        bool           `json:"max,omitempty"`
	Contract   ContractParams `json:"contract"`
	LimitPrice float64        `json:"limit_price"`
	Quantity   int            `json:"quantity,omitempty"`
}

// NormalizeAddParams rejects unsupported entry types and invented position sizes.
func NormalizeAddParams(p AddParams) (AddParams, error) {
	p.Contract.Symbol = strings.ToUpper(strings.TrimSpace(p.Contract.Symbol))
	p.Contract.Currency = strings.ToUpper(strings.TrimSpace(p.Contract.Currency))
	if p.Contract.SecType == "" {
		p.Contract.SecType = "STK"
	}
	if p.Contract.Exchange == "" {
		p.Contract.Exchange = "SMART"
	}
	if p.Contract.Symbol == "" || p.Contract.Currency == "" || p.Contract.ConID < 0 || p.Contract.SecType != "STK" || p.Contract.Expiry != "" || p.Contract.Right != "" || p.Contract.Strike != 0 || (p.Contract.Multiplier != 0 && p.Contract.Multiplier != 1) {
		return p, fmt.Errorf("stock addition currently supports an exact stock identity and explicit currency only")
	}
	if math.IsNaN(p.LimitPrice) || math.IsInf(p.LimitPrice, 0) || p.LimitPrice <= 0 || p.Quantity < 0 || p.Quantity > 1000000 {
		return p, fmt.Errorf("stock addition needs a positive finite limit and a quantity from 0 to 1000000 (maximum requires max=true)")
	}
	if p.Max == (p.Quantity > 0) {
		return p, fmt.Errorf("choose exactly one sizing intent: max or a positive additional quantity")
	}
	return p, nil
}

// AddReview is immutable sizing evidence carried by the signed order draft.
// No field is submission authority; Canary recomputes capacity at admission.
type AddReview struct {
	RegimeStage           string            `json:"regime_stage,omitempty"`
	RegimeAsOf            time.Time         `json:"regime_as_of,omitzero"`
	PolicyFingerprint     string            `json:"policy_fingerprint"`
	RulebookFingerprint   string            `json:"rulebook_fingerprint"`
	CashPolicyFingerprint string            `json:"cash_policy_fingerprint"`
	AsOf                  time.Time         `json:"as_of"`
	Plan                  risk.StockAddPlan `json:"plan"`
}

// AddPlanResult exposes daemon-owned planning evidence without a preview token.
type AddPlanResult struct {
	risk.StockAddPlan
	Contract      ContractParams `json:"contract"`
	LimitPrice    float64        `json:"limit_price"`
	Currency      string         `json:"currency"`
	BaseCurrency  string         `json:"base_currency,omitempty"`
	MaxCommission *float64       `json:"max_commission,omitempty"`
	Review        *AddReview     `json:"review,omitempty"`
	AsOf          time.Time      `json:"as_of"`
}

// CloneAddReview detaches the audit evidence from a caller's mutable draft.
func CloneAddReview(in *AddReview) *AddReview {
	if in == nil {
		return nil
	}
	out := *in
	out.Plan.Blockers = slices.Clone(in.Plan.Blockers)
	out.Plan.Warnings = slices.Clone(in.Plan.Warnings)
	out.Plan.Allowances = slices.Clone(in.Plan.Allowances)
	out.Plan.RiskChecks = slices.Clone(in.Plan.RiskChecks)
	if in.Plan.Cash != nil {
		out.Plan.Cash = new(*in.Plan.Cash)
	}
	if in.Plan.Protection != nil {
		out.Plan.Protection = new(*in.Plan.Protection)
	}
	if in.Plan.Margin != nil {
		out.Plan.Margin = new(*in.Plan.Margin)
		if in.Plan.Margin.LookAheadExcessBase != nil {
			out.Plan.Margin.LookAheadExcessBase = new(*in.Plan.Margin.LookAheadExcessBase)
		}
	}
	return &out
}
