package rpc

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Financing method and bounds cover retained managed-lending evidence only.
const (
	MethodFinancingFees    = "financing.fees"
	FinancingSchemaVersion = "financing.v1"
	FinancingComplete      = "complete"
	FinancingPartial       = "partial"
	FinancingUnavailable   = "unavailable"
	MaxFinancingFees       = 100
)

// LendingPosition is a statement-dated loan annotation on an exact stock.
// NetRatePct is the customer rate from matching fee evidence, not an
// indicative borrow rate. OwnedQuantity refers to the same statement date.
type LendingPosition struct {
	AsOf          time.Time `json:"as_of"`
	State         string    `json:"state"`
	Quantity      float64   `json:"quantity"`
	OwnedQuantity *float64  `json:"owned_quantity,omitempty"`
	Currency      string    `json:"currency"`
	NetRatePct    *float64  `json:"net_rate_pct,omitempty"`
	Collateral    *float64  `json:"collateral,omitempty"`
}

// FinancingCurrencyAmount is the sum of known customer net fees in one
// native currency. Coverage on the enclosing summary qualifies the sum.
type FinancingCurrencyAmount struct {
	Currency string  `json:"currency"`
	Amount   float64 `json:"amount"`
}

// FinancingSummary explains earned fees over exact, exclusive-opening and
// inclusive-closing equity dates. Complete totals are nullable; known sums
// never stand in for a complete range or missing FX evidence.
type FinancingSummary struct {
	SchemaVersion     string                    `json:"schema_version"`
	State             string                    `json:"state"`
	Reason            string                    `json:"reason,omitempty"`
	From              time.Time                 `json:"from"`
	To                time.Time                 `json:"to"`
	AsOf              time.Time                 `json:"as_of,omitzero"`
	BaseCurrency      string                    `json:"base_currency,omitempty"`
	EarnedBase        *float64                  `json:"earned_base,omitempty"`
	KnownEarnedBase   *float64                  `json:"known_earned_base,omitempty"`
	Native            []FinancingCurrencyAmount `json:"native"`
	FeeCount          int                       `json:"fee_count"`
	CoveredDays       int                       `json:"covered_days"`
	ExpectedDays      int                       `json:"expected_days"`
	PNLReconciliation string                    `json:"pnl_reconciliation"`
	PaymentLinkage    string                    `json:"payment_linkage"`
	Fingerprint       string                    `json:"fingerprint"`
}

// FinancingFee is one sanitized earned-fee row; ID is opaque. BaseAmount is
// available only with statement conversion evidence or the same currency.
type FinancingFee struct {
	ID           string    `json:"id"`
	ConID        int64     `json:"con_id"`
	Symbol       string    `json:"symbol"`
	ValueDate    time.Time `json:"value_date"`
	StartDate    time.Time `json:"start_date,omitzero"`
	Currency     string    `json:"currency"`
	Quantity     *float64  `json:"quantity,omitempty"`
	NetFee       *float64  `json:"net_fee,omitempty"`
	NetRatePct   *float64  `json:"net_rate_pct,omitempty"`
	Collateral   *float64  `json:"collateral,omitempty"`
	FXRateToBase *float64  `json:"fx_rate_to_base,omitempty"`
	BaseAmount   *float64  `json:"base_amount,omitempty"`
}

// FinancingFeesParams selects a bounded read, optionally restricted to an
// exact contract. From/To are a paired override of the Edge equity boundaries.
type FinancingFeesParams struct {
	Window      string `json:"window,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	ConID       int64  `json:"con_id,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// FinancingFeesResult carries full-period attribution independently of the
// filtered page. Cursor continuity is bound to scope, period and projection.
type FinancingFeesResult struct {
	Summary       FinancingSummary `json:"summary"`
	Fees          []FinancingFee   `json:"fees"`
	ConID         int64            `json:"con_id,omitempty"`
	FilteredCount int              `json:"filtered_count"`
	NextCursor    string           `json:"next_cursor,omitempty"`
}

var financingFingerprint = regexp.MustCompile(`^finance_[a-f0-9]{32}$`)
var financingFeeID = regexp.MustCompile(`^fee_[a-f0-9]{32}$`)

// ValidateFinancingFeesResponse binds a typed page to the requested filter,
// optional period and prior snapshot; adapters must not accept another read.
func ValidateFinancingFeesResponse(v FinancingFeesResult, p FinancingFeesParams) error {
	if err := ValidateFinancingFeesResult(v); err != nil {
		return err
	}
	if v.ConID != p.ConID || p.Fingerprint != "" && v.Summary.Fingerprint != p.Fingerprint {
		return fmt.Errorf("financing response changed contract or snapshot")
	}
	if p.From != "" && (v.Summary.From.Format(time.DateOnly) != p.From || v.Summary.To.Format(time.DateOnly) != p.To) {
		return fmt.Errorf("financing response changed period")
	}
	return nil
}

// NormalizeFinancingFeesParams is the shared CLI/MCP/HTTP input contract.
func NormalizeFinancingFeesParams(in FinancingFeesParams) (FinancingFeesParams, error) {
	out := in
	out.Window = strings.ToLower(strings.TrimSpace(out.Window))
	if out.Window == "" {
		out.Window = "365d"
	}
	if out.Window != "365d" && out.Window != "90d" {
		return FinancingFeesParams{}, fmt.Errorf("financing window must be 90d or 365d")
	}
	if out.Limit == 0 {
		out.Limit = 25
	}
	if out.Limit < 1 || out.Limit > MaxFinancingFees || out.ConID < 0 {
		return FinancingFeesParams{}, fmt.Errorf("invalid financing limit or contract")
	}
	out.From, out.To = strings.TrimSpace(out.From), strings.TrimSpace(out.To)
	if out.From != "" || out.To != "" {
		from, ferr := time.Parse(time.DateOnly, out.From)
		to, terr := time.Parse(time.DateOnly, out.To)
		if ferr != nil || terr != nil || !to.After(from) || to.Sub(from) > 400*24*time.Hour {
			return FinancingFeesParams{}, fmt.Errorf("financing dates must be paired YYYY-MM-DD boundaries within 400 days")
		}
	}
	out.Cursor, out.Fingerprint = strings.TrimSpace(out.Cursor), strings.TrimSpace(out.Fingerprint)
	if len(out.Cursor) > 256 || out.Fingerprint != "" && !financingFingerprint.MatchString(out.Fingerprint) {
		return FinancingFeesParams{}, fmt.Errorf("invalid financing cursor or fingerprint")
	}
	return out, nil
}

func financingFinite(v *float64) bool { return v == nil || !math.IsNaN(*v) && !math.IsInf(*v, 0) }
func financingCurrency(v string) bool {
	if len(v) != 3 {
		return false
	}
	for _, c := range v {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// ValidateLendingPosition rejects malformed annotations at adapter boundaries.
func ValidateLendingPosition(v LendingPosition) error {
	if v.AsOf.IsZero() || v.State != "reported" && v.State != "stale" || !financingCurrency(v.Currency) ||
		v.Quantity <= 0 || math.IsNaN(v.Quantity) || math.IsInf(v.Quantity, 0) {
		return fmt.Errorf("invalid lending position")
	}
	for _, n := range []*float64{v.OwnedQuantity, v.NetRatePct, v.Collateral} {
		if !financingFinite(n) {
			return fmt.Errorf("nonfinite lending position")
		}
	}
	if v.OwnedQuantity != nil && *v.OwnedQuantity < v.Quantity || v.Collateral != nil && *v.Collateral < 0 {
		return fmt.Errorf("invalid lending position bounds")
	}
	return nil
}

// ValidateFinancingSummary enforces missing-aware attribution without changing
// the enclosing account P/L or accepting an unproved reconciliation claim.
func ValidateFinancingSummary(v FinancingSummary) error {
	if v.SchemaVersion != FinancingSchemaVersion || v.State != FinancingComplete && v.State != FinancingPartial && v.State != FinancingUnavailable ||
		v.From.IsZero() || !v.To.After(v.From) || v.To.Sub(v.From) > 400*24*time.Hour || !financingFingerprint.MatchString(v.Fingerprint) ||
		v.FeeCount < 0 || v.CoveredDays < 0 || v.ExpectedDays < 1 || v.CoveredDays > v.ExpectedDays ||
		v.ExpectedDays != int(v.To.Sub(v.From)/(24*time.Hour)) ||
		v.PNLReconciliation != "unproved" || v.PaymentLinkage != "unavailable" {
		return fmt.Errorf("invalid financing summary")
	}
	if v.BaseCurrency != "" && !financingCurrency(v.BaseCurrency) || !financingFinite(v.EarnedBase) || !financingFinite(v.KnownEarnedBase) {
		return fmt.Errorf("invalid financing amounts")
	}
	if v.EarnedBase != nil && (v.State != FinancingComplete || v.BaseCurrency == "" || v.CoveredDays != v.ExpectedDays) || v.KnownEarnedBase != nil && v.BaseCurrency == "" || v.State == FinancingUnavailable && (v.EarnedBase != nil || v.KnownEarnedBase != nil) {
		return fmt.Errorf("unproved financing total")
	}
	seen := map[string]bool{}
	for _, n := range v.Native {
		if !financingCurrency(n.Currency) || !financingFinite(&n.Amount) || seen[n.Currency] {
			return fmt.Errorf("invalid financing native total")
		}
		seen[n.Currency] = true
	}
	return nil
}

// ValidateFinancingFeesResult checks bounds, chronology, identities and units.
func ValidateFinancingFeesResult(v FinancingFeesResult) error {
	if err := ValidateFinancingSummary(v.Summary); err != nil {
		return err
	}
	if len(v.Fees) > MaxFinancingFees || v.FilteredCount < len(v.Fees) || v.FilteredCount > v.Summary.FeeCount || v.ConID < 0 || len(v.NextCursor) > 256 {
		return fmt.Errorf("invalid financing page")
	}
	seen := map[string]bool{}
	for i, f := range v.Fees {
		if !financingFeeID.MatchString(f.ID) || seen[f.ID] || f.ConID <= 0 || v.ConID != 0 && f.ConID != v.ConID ||
			f.Symbol == "" || len(f.Symbol) > 80 || !financingCurrency(f.Currency) || !f.ValueDate.After(v.Summary.From) || f.ValueDate.After(v.Summary.To) || f.StartDate.After(f.ValueDate) ||
			i > 0 && f.ValueDate.After(v.Fees[i-1].ValueDate) {
			return fmt.Errorf("invalid financing fee identity or date")
		}
		seen[f.ID] = true
		for _, n := range []*float64{f.Quantity, f.NetFee, f.NetRatePct, f.Collateral, f.FXRateToBase, f.BaseAmount} {
			if !financingFinite(n) {
				return fmt.Errorf("nonfinite financing fee")
			}
		}
		if f.Quantity != nil && *f.Quantity < 0 || f.Collateral != nil && *f.Collateral < 0 || f.FXRateToBase != nil && *f.FXRateToBase <= 0 || f.BaseAmount != nil && (f.NetFee == nil || v.Summary.BaseCurrency == "" || f.FXRateToBase == nil) {
			return fmt.Errorf("invalid financing fee units")
		}
		if f.BaseAmount != nil && math.Abs(*f.BaseAmount-*f.NetFee**f.FXRateToBase) > 1e-9*math.Max(1, math.Abs(*f.BaseAmount)) {
			return fmt.Errorf("invalid financing fee conversion")
		}
	}
	return nil
}
