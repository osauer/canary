package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// ConstitutionKind identifies the operator-authored risk constitution schema.
const ConstitutionKind = "ibkr.risk_policy"

// CapitalTierOK and the related constants are capital-evaluation outcomes.
const (
	CapitalTierOK         = "ok"
	CapitalTierWarn       = "warn"
	CapitalTierBlock      = "block"
	CapitalTierUnknown    = "unknown"
	CapitalTierUnapproved = "unapproved"
)

// EnforcementShadow and EnforcementAdvisory are the enforcement classes a
// constitution control may declare. Validation rejects unsupported classes.
const (
	EnforcementShadow   = "shadow"
	EnforcementAdvisory = "advisory"
)

// Constitution is the typed operator-authored capital policy. Material limits
// are pointers: nil means unapproved, and validation never backfills them.
type Constitution struct {
	Kind          string `toml:"kind" json:"kind"`
	SchemaVersion int    `toml:"schema_version" json:"schema_version"`
	PolicyID      string `toml:"policy_id" json:"policy_id"`
	PolicyVersion int    `toml:"policy_version" json:"policy_version"`

	Capital   ConstitutionCapital   `toml:"capital" json:"capital"`
	Drawdown  ConstitutionDrawdown  `toml:"drawdown" json:"drawdown"`
	Override  ConstitutionOverride  `toml:"override" json:"override"`
	Recon     ConstitutionRecon     `toml:"recon" json:"recon"`
	Cadence   ConstitutionCadence   `toml:"cadence" json:"cadence"`
	Inventory ConstitutionInventory `toml:"inventory" json:"inventory"`
}

// ConstitutionCapital anchors the capital authority: an internal protected
// account base currency. Effective risk capital =
type ConstitutionCapital struct {
	BaseCurrency        string   `toml:"base_currency" json:"base_currency"`
	ProtectedFloor      *float64 `toml:"protected_floor" json:"protected_floor"`
	DeclaredRiskCapital *float64 `toml:"declared_risk_capital" json:"declared_risk_capital"`
	// MaxEquityAgeMinutes bounds trust in the last equity observation;
	MaxEquityAgeMinutes *int `toml:"max_equity_age_minutes" json:"max_equity_age_minutes"`
	// MaxUnreconciledDays bounds trust in the declared capital-event ledger
	MaxUnreconciledDays *int `toml:"max_unreconciled_days" json:"max_unreconciled_days"`
}

// ConstitutionDrawdown is the two-tier response ladder. Both thresholds are
// percentages of declared risk capital consumed from the cash-flow-adjusted
// equity peak. Warn is advisory and self-clearing; block latches in daemon
// state and clears only through a journaled human reset that re-bases the
// peak.
type ConstitutionDrawdown struct {
	WarnConsumedPct  *float64 `toml:"warn_consumed_pct" json:"warn_consumed_pct"`
	BlockConsumedPct *float64 `toml:"block_consumed_pct" json:"block_consumed_pct"`
	// BlockEnforcement is shadow (default when empty) or advisory in v1.
	BlockEnforcement string `toml:"block_enforcement" json:"block_enforcement"`
}

// ConstitutionOverride caps the one-shot exception mechanism: human-only,
// single named control, reason required, hard expiry. The mechanism itself
// (origin gating, journaling) is code-owned; only the lifetime cap is
// policy.
type ConstitutionOverride struct {
	MaxDurationHours *int `toml:"max_duration_hours" json:"max_duration_hours"`
}

// ConstitutionRecon sets what counts as a reconciliation exception when
// broker statement flows are matched against the declared capital-event
// plumbing: they decide which differences the operator must look at.
type ConstitutionRecon struct {
	// A statement flow and a declared event match on amount when they
	AmountTolerancePct *float64 `toml:"amount_tolerance_pct" json:"amount_tolerance_pct"`
	AmountToleranceMin *float64 `toml:"amount_tolerance_min" json:"amount_tolerance_min"`
	// DateWindowBusinessDays bounds how far apart the statement value
	DateWindowBusinessDays *int `toml:"date_window_business_days" json:"date_window_business_days"`
	// MaxReportAgeDays bounds how old the newest ingested statement may
	// be for a recon report to back a reconcile sign-off.
	MaxReportAgeDays *int `toml:"max_report_age_days" json:"max_report_age_days"`
	// MaxEquityDivergencePct bounds the absolute same-day difference between
	// broker statement equity and the runtime observation before v3 may
	MaxEquityDivergencePct *float64 `toml:"max_equity_divergence_pct" json:"max_equity_divergence_pct"`
}

// ConstitutionCadence retains the v2 daily declaration shape for policy-file
// parsing while v3 uses only the automated nudge and monthly clocks below.
type ConstitutionCadence struct {
	Morning ConstitutionArtefact `toml:"morning" json:"morning"`
	EOD     ConstitutionArtefact `toml:"eod" json:"eod"`
	Weekly  ConstitutionArtefact `toml:"weekly" json:"weekly"`
	// Nudges and Monthly are policy-version-4-only. Pointers preserve the
	// distinction between an absent table and an explicitly authored one, so
	// old policies can reject the new keys and v4 can report missing material.
	Nudges  *ConstitutionNudgeCadence   `toml:"nudges" json:"nudges,omitempty"`
	Monthly *ConstitutionMonthlyCadence `toml:"monthly" json:"monthly,omitempty"`
}

// ConstitutionNudgeCadence carries optional overrides for cadence-driven
type ConstitutionNudgeCadence struct {
	Timezone             *string `toml:"timezone" json:"timezone"`
	ReconcileWarningDays *int    `toml:"reconcile_warning_days" json:"reconcile_warning_days"`
}

// ConstitutionMonthlyCadence declares the one standing monthly touchpoint.
type ConstitutionMonthlyCadence struct {
	Class        *string `toml:"class" json:"class"`
	DayOfMonth   *int    `toml:"day_of_month" json:"day_of_month"`
	NudgeAtLocal *string `toml:"nudge_at_local" json:"nudge_at_local"`
}

// Cadence defaults (operator-accepted 2026-08-03): every cadence key
// defaults in code so a policy file never needs the exact key phrases; an
// authored key is an override, never approval material.
const (
	// DefaultReconcileWarningDays is the rolling reconcile warning horizon.
	DefaultReconcileWarningDays = 2
	// DefaultMonthlyPulseWorkingDay is the Nth working day of the month the
	// monthly pulse becomes due (Monday through Friday, weeks start Monday).
	DefaultMonthlyPulseWorkingDay = 1
	// DefaultMonthlyPulseAtLocal is the local wall time the pulse fires.
	DefaultMonthlyPulseAtLocal = "09:00"
)

// NudgeLocation resolves the clock cadence-driven nudges run on: the
func (c ConstitutionCadence) NudgeLocation() (*time.Location, error) {
	if c.Nudges != nil && c.Nudges.Timezone != nil {
		return loadConstitutionLocation(*c.Nudges.Timezone)
	}
	return time.Local, nil
}

// ResolvedReconcileWarningDays returns the authored override when present,
// else the code default.
func (c ConstitutionCadence) ResolvedReconcileWarningDays() int {
	if c.Nudges != nil && c.Nudges.ReconcileWarningDays != nil {
		return *c.Nudges.ReconcileWarningDays
	}
	return DefaultReconcileWarningDays
}

// ResolvedMonthlyWorkingDay returns the authored override when present, else
func (c ConstitutionCadence) ResolvedMonthlyWorkingDay() int {
	if c.Monthly != nil && c.Monthly.DayOfMonth != nil {
		return *c.Monthly.DayOfMonth
	}
	return DefaultMonthlyPulseWorkingDay
}

// ResolvedMonthlyNudgeAtLocal returns the authored override when present,
// else the code default.
func (c ConstitutionCadence) ResolvedMonthlyNudgeAtLocal() string {
	if c.Monthly != nil && c.Monthly.NudgeAtLocal != nil {
		return *c.Monthly.NudgeAtLocal
	}
	return DefaultMonthlyPulseAtLocal
}

// ConstitutionArtefact declares one cadence artefact. An empty Class means the
// artefact is undeclared; validation accepts advisory as the only non-empty
type ConstitutionArtefact struct {
	Class string `toml:"class" json:"class,omitempty"`
}

// ConstitutionInventory pins the sibling policies by identity so the policy
type ConstitutionInventory struct {
	Rulebook   *ConstitutionPolicyPin `toml:"rulebook" json:"rulebook,omitempty"`
	Protection *ConstitutionPolicyPin `toml:"protection" json:"protection,omitempty"`
	Stress     *ConstitutionPolicyPin `toml:"stress" json:"stress,omitempty"`
}

// ConstitutionPolicyPin identifies one sibling policy version. Version is a
// string so integer-versioned (rulebook, protection) and string-versioned
// (stress) policies pin uniformly.
type ConstitutionPolicyPin struct {
	ID      string `toml:"id" json:"id"`
	Version string `toml:"version" json:"version"`
}

// Validate rejects a structurally unusable constitution. It never backfills
func (c Constitution) Validate() error {
	if c.Kind != ConstitutionKind {
		return fmt.Errorf("risk policy kind %q is invalid (want %s)", c.Kind, ConstitutionKind)
	}
	if c.SchemaVersion != 1 {
		return fmt.Errorf("risk policy schema_version %d is unsupported", c.SchemaVersion)
	}
	if strings.TrimSpace(c.PolicyID) == "" {
		return fmt.Errorf("risk policy policy_id is required")
	}
	if c.PolicyVersion <= 0 {
		return fmt.Errorf("risk policy policy_version must be positive")
	}
	if cur := strings.TrimSpace(c.Capital.BaseCurrency); cur != "" && len(cur) != 3 {
		return fmt.Errorf("capital.base_currency %q must be a 3-letter currency code", c.Capital.BaseCurrency)
	}
	if v := c.Capital.ProtectedFloor; v != nil && *v < 0 {
		return fmt.Errorf("capital.protected_floor must not be negative")
	}
	if v := c.Capital.DeclaredRiskCapital; v != nil && *v <= 0 {
		return fmt.Errorf("capital.declared_risk_capital must be positive")
	}
	if v := c.Capital.MaxEquityAgeMinutes; v != nil && *v <= 0 {
		return fmt.Errorf("capital.max_equity_age_minutes must be positive")
	}
	if v := c.Capital.MaxUnreconciledDays; v != nil && *v <= 0 {
		return fmt.Errorf("capital.max_unreconciled_days must be positive")
	}
	warn, block := c.Drawdown.WarnConsumedPct, c.Drawdown.BlockConsumedPct
	if warn != nil && (*warn <= 0 || *warn > 100) {
		return fmt.Errorf("drawdown.warn_consumed_pct must be in (0, 100]")
	}
	if block != nil && (*block <= 0 || *block > 100) {
		return fmt.Errorf("drawdown.block_consumed_pct must be in (0, 100]")
	}
	if warn != nil && block != nil && *warn >= *block {
		return fmt.Errorf("drawdown.warn_consumed_pct must be below block_consumed_pct")
	}
	switch c.Drawdown.BlockEnforcement {
	case "", EnforcementShadow, EnforcementAdvisory:
	case "hard":
		return fmt.Errorf("drawdown.block_enforcement %q is not promotable in schema v1; promotion to a pre-trade gate is a later human policy revision", c.Drawdown.BlockEnforcement)
	default:
		return fmt.Errorf("drawdown.block_enforcement %q is invalid; use shadow or advisory", c.Drawdown.BlockEnforcement)
	}
	if v := c.Override.MaxDurationHours; v != nil && *v <= 0 {
		return fmt.Errorf("override.max_duration_hours must be positive")
	}
	if v := c.Recon.AmountTolerancePct; v != nil && (*v < 0 || *v > 100) {
		return fmt.Errorf("recon.amount_tolerance_pct must be in [0, 100]")
	}
	if v := c.Recon.AmountToleranceMin; v != nil && *v < 0 {
		return fmt.Errorf("recon.amount_tolerance_min must not be negative")
	}
	if v := c.Recon.DateWindowBusinessDays; v != nil && *v <= 0 {
		return fmt.Errorf("recon.date_window_business_days must be positive")
	}
	if v := c.Recon.MaxReportAgeDays; v != nil && *v <= 0 {
		return fmt.Errorf("recon.max_report_age_days must be positive")
	}
	if v := c.Recon.MaxEquityDivergencePct; v != nil {
		if c.PolicyVersion < 3 {
			return fmt.Errorf("recon.max_equity_divergence_pct requires policy_version >= 3")
		}
		if math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
			return fmt.Errorf("recon.max_equity_divergence_pct must be positive and finite")
		}
	}
	for _, a := range []struct {
		key   string
		class string
	}{
		{"cadence.morning", c.Cadence.Morning.Class},
		{"cadence.eod", c.Cadence.EOD.Class},
		{"cadence.weekly", c.Cadence.Weekly.Class},
	} {
		if a.class != "" && a.class != EnforcementAdvisory {
			return fmt.Errorf("%s.class %q is invalid; only advisory is accepted in v1", a.key, a.class)
		}
	}
	if c.PolicyVersion < 4 {
		if c.Cadence.Nudges != nil || c.Cadence.Monthly != nil {
			return fmt.Errorf("cadence v4 key set requires policy_version >= 4")
		}
	} else {
		if cadence := c.Cadence.Nudges; cadence != nil {
			if cadence.Timezone != nil {
				if _, err := loadConstitutionLocation(*cadence.Timezone); err != nil {
					return fmt.Errorf("cadence.nudges.timezone %q is invalid: %w", *cadence.Timezone, err)
				}
			}
			if days := cadence.ReconcileWarningDays; days != nil && *days <= 0 {
				return fmt.Errorf("cadence.nudges.reconcile_warning_days must be positive")
			}
		}
		if cadence := c.Cadence.Monthly; cadence != nil {
			if cadence.Class != nil && *cadence.Class != EnforcementAdvisory {
				return fmt.Errorf("cadence.monthly.class %q is invalid; only advisory is accepted", *cadence.Class)
			}
			if day := cadence.DayOfMonth; day != nil && (*day < 1 || *day > 20) {
				return fmt.Errorf("cadence.monthly.day_of_month is the Nth working day of the month and must be in [1, 20]")
			}
			if local := cadence.NudgeAtLocal; local != nil {
				parsed, err := time.Parse("15:04", *local)
				if err != nil || parsed.Format("15:04") != *local {
					return fmt.Errorf("cadence.monthly.nudge_at_local %q must use HH:MM", *local)
				}
			}
		}
	}
	for _, p := range []struct {
		key string
		pin *ConstitutionPolicyPin
	}{
		{"inventory.rulebook", c.Inventory.Rulebook},
		{"inventory.protection", c.Inventory.Protection},
		{"inventory.stress", c.Inventory.Stress},
	} {
		if p.pin != nil && (strings.TrimSpace(p.pin.ID) == "" || strings.TrimSpace(p.pin.Version) == "") {
			return fmt.Errorf("%s pin needs both id and version", p.key)
		}
	}
	return nil
}

// loadConstitutionLocation rejects process-local and non-canonical raw input.
func loadConstitutionLocation(raw string) (*time.Location, error) {
	if raw == "" {
		return nil, fmt.Errorf("must be a non-empty IANA timezone")
	}
	if raw != strings.TrimSpace(raw) {
		return nil, fmt.Errorf("must not contain leading or trailing whitespace")
	}
	if strings.EqualFold(raw, "Local") {
		return nil, fmt.Errorf("local is process-dependent")
	}
	location, err := time.LoadLocation(raw)
	if err != nil {
		return nil, fmt.Errorf("unknown IANA timezone")
	}
	return location, nil
}

// EffectiveBlockEnforcement resolves the block tier's enforcement class;
// empty defaults to shadow — the fail-safe direction (observe and journal,
// never gate).
func (c Constitution) EffectiveBlockEnforcement() string {
	if c.Drawdown.BlockEnforcement == "" {
		return EnforcementShadow
	}
	return c.Drawdown.BlockEnforcement
}

// UnapprovedKeys lists the material keys the operator has not chosen yet.
func (c Constitution) UnapprovedKeys() []string {
	var out []string
	if strings.TrimSpace(c.Capital.BaseCurrency) == "" {
		out = append(out, "capital.base_currency")
	}
	if c.Capital.ProtectedFloor == nil {
		out = append(out, "capital.protected_floor")
	}
	if c.Capital.DeclaredRiskCapital == nil {
		out = append(out, "capital.declared_risk_capital")
	}
	if c.Capital.MaxEquityAgeMinutes == nil {
		out = append(out, "capital.max_equity_age_minutes")
	}
	if c.Capital.MaxUnreconciledDays == nil {
		out = append(out, "capital.max_unreconciled_days")
	}
	if c.Drawdown.WarnConsumedPct == nil {
		out = append(out, "drawdown.warn_consumed_pct")
	}
	if c.Drawdown.BlockConsumedPct == nil {
		out = append(out, "drawdown.block_consumed_pct")
	}
	if c.Override.MaxDurationHours == nil {
		out = append(out, "override.max_duration_hours")
	}
	if c.Recon.AmountTolerancePct == nil {
		out = append(out, "recon.amount_tolerance_pct")
	}
	if c.Recon.AmountToleranceMin == nil {
		out = append(out, "recon.amount_tolerance_min")
	}
	if c.Recon.DateWindowBusinessDays == nil {
		out = append(out, "recon.date_window_business_days")
	}
	if c.Recon.MaxReportAgeDays == nil {
		out = append(out, "recon.max_report_age_days")
	}
	if (c.PolicyVersion == 0 || c.PolicyVersion >= 3) && c.Recon.MaxEquityDivergencePct == nil {
		out = append(out, "recon.max_equity_divergence_pct")
	}
	// cadence.* keys are deliberately not approval material: the timezone
	// overrides fail Validate, never this list.
	return out
}

// FingerprintKey hashes an explicit JSON projection of the full policy
func (c Constitution) FingerprintKey() string {
	type fingerprintBase struct {
		Kind          string
		SchemaVersion int
		PolicyID      string
		PolicyVersion int
		Capital       ConstitutionCapital
		Drawdown      ConstitutionDrawdown
		Override      ConstitutionOverride
		Inventory     ConstitutionInventory
	}
	base := fingerprintBase{
		Kind: strings.TrimSpace(c.Kind), SchemaVersion: c.SchemaVersion,
		PolicyID: strings.TrimSpace(c.PolicyID), PolicyVersion: c.PolicyVersion,
		Capital: c.Capital, Drawdown: c.Drawdown, Override: c.Override,
		Inventory: c.Inventory,
	}
	legacyCadence := struct {
		Morning ConstitutionArtefact `json:"morning"`
		EOD     ConstitutionArtefact `json:"eod"`
		Weekly  ConstitutionArtefact `json:"weekly"`
	}{c.Cadence.Morning, c.Cadence.EOD, c.Cadence.Weekly}
	type v4NudgeCadence struct {
		Timezone             *string `json:"timezone"`
		ReconcileWarningDays *int    `json:"reconcile_warning_days"`
	}
	type v4MonthlyCadence struct {
		Class        *string `json:"class"`
		DayOfMonth   *int    `json:"day_of_month"`
		NudgeAtLocal *string `json:"nudge_at_local"`
	}
	v4Cadence := struct {
		Morning ConstitutionArtefact `json:"morning"`
		EOD     ConstitutionArtefact `json:"eod"`
		Weekly  ConstitutionArtefact `json:"weekly"`
		Nudges  v4NudgeCadence       `json:"nudges"`
		Monthly v4MonthlyCadence     `json:"monthly"`
	}{Morning: c.Cadence.Morning, EOD: c.Cadence.EOD, Weekly: c.Cadence.Weekly}
	if c.Cadence.Nudges != nil {
		v4Cadence.Nudges = v4NudgeCadence{
			Timezone: c.Cadence.Nudges.Timezone, ReconcileWarningDays: c.Cadence.Nudges.ReconcileWarningDays,
		}
	}
	if c.Cadence.Monthly != nil {
		v4Cadence.Monthly = v4MonthlyCadence{
			Class: c.Cadence.Monthly.Class, DayOfMonth: c.Cadence.Monthly.DayOfMonth,
			NudgeAtLocal: c.Cadence.Monthly.NudgeAtLocal,
		}
	}
	var raw []byte
	if c.PolicyVersion < 3 {
		// Preserve the pre-v3 projection byte-for-byte: adding a nil v3-only
		// field must not change an existing policy fingerprint.
		recon := struct {
			AmountTolerancePct     *float64 `json:"amount_tolerance_pct"`
			AmountToleranceMin     *float64 `json:"amount_tolerance_min"`
			DateWindowBusinessDays *int     `json:"date_window_business_days"`
			MaxReportAgeDays       *int     `json:"max_report_age_days"`
		}{c.Recon.AmountTolerancePct, c.Recon.AmountToleranceMin, c.Recon.DateWindowBusinessDays, c.Recon.MaxReportAgeDays}
		normalized := struct {
			Kind          string                `json:"kind"`
			SchemaVersion int                   `json:"schema_version"`
			PolicyID      string                `json:"policy_id"`
			PolicyVersion int                   `json:"policy_version"`
			Capital       ConstitutionCapital   `json:"capital"`
			Drawdown      ConstitutionDrawdown  `json:"drawdown"`
			Override      ConstitutionOverride  `json:"override"`
			Recon         any                   `json:"recon"`
			Cadence       any                   `json:"cadence"`
			Inventory     ConstitutionInventory `json:"inventory"`
		}{
			Kind: base.Kind, SchemaVersion: base.SchemaVersion, PolicyID: base.PolicyID, PolicyVersion: base.PolicyVersion,
			Capital: base.Capital, Drawdown: base.Drawdown, Override: base.Override, Recon: recon,
			Cadence: legacyCadence, Inventory: base.Inventory,
		}
		raw, _ = json.Marshal(normalized)
	} else if c.PolicyVersion < 4 {
		normalized := struct {
			Kind          string                `json:"kind"`
			SchemaVersion int                   `json:"schema_version"`
			PolicyID      string                `json:"policy_id"`
			PolicyVersion int                   `json:"policy_version"`
			Capital       ConstitutionCapital   `json:"capital"`
			Drawdown      ConstitutionDrawdown  `json:"drawdown"`
			Override      ConstitutionOverride  `json:"override"`
			Recon         ConstitutionRecon     `json:"recon"`
			Cadence       any                   `json:"cadence"`
			Inventory     ConstitutionInventory `json:"inventory"`
		}{
			Kind:          strings.TrimSpace(c.Kind),
			SchemaVersion: c.SchemaVersion,
			PolicyID:      strings.TrimSpace(c.PolicyID),
			PolicyVersion: c.PolicyVersion,
			Capital:       c.Capital,
			Drawdown:      c.Drawdown,
			Override:      c.Override,
			Recon:         c.Recon,
			Cadence:       legacyCadence,
			Inventory:     c.Inventory,
		}
		raw, _ = json.Marshal(normalized)
	} else {
		normalized := struct {
			Kind          string                `json:"kind"`
			SchemaVersion int                   `json:"schema_version"`
			PolicyID      string                `json:"policy_id"`
			PolicyVersion int                   `json:"policy_version"`
			Capital       ConstitutionCapital   `json:"capital"`
			Drawdown      ConstitutionDrawdown  `json:"drawdown"`
			Override      ConstitutionOverride  `json:"override"`
			Recon         ConstitutionRecon     `json:"recon"`
			Cadence       any                   `json:"cadence"`
			Inventory     ConstitutionInventory `json:"inventory"`
		}{
			Kind: strings.TrimSpace(c.Kind), SchemaVersion: c.SchemaVersion,
			PolicyID: strings.TrimSpace(c.PolicyID), PolicyVersion: c.PolicyVersion,
			Capital: c.Capital, Drawdown: c.Drawdown, Override: c.Override,
			Recon: c.Recon, Cadence: v4Cadence, Inventory: c.Inventory,
		}
		raw, _ = json.Marshal(normalized)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CapitalObservation is one equity reading in base currency.
type CapitalObservation struct {
	EquityBase float64
	AsOf       time.Time
}

// CapitalRuntime is the daemon-owned runtime state the evaluator consumes:
// evaluator only reads.
type CapitalRuntime struct {
	// AdjustedPeakBase is the peak of (equity − cumulative external flows).
	AdjustedPeakBase float64
	PeakAsOf         time.Time
	// CumExternalFlowsBase is the policy-version-selected cumulative flow
	CumExternalFlowsBase float64
	// Seeded is false until the first equity observation establishes the
	// peak; an unseeded state evaluates unknown, never ok.
	Seeded bool
	// BlockLatched persists across restarts and mark recovery; only a
	// journaled human reset clears it.
	BlockLatched bool
	// LastReconciledAt is the last human or automatic reconcile evidence;
	// zero means never reconciled.
	LastReconciledAt time.Time
	// UnreconciledOverrideUntil is populated only from an active, unexpired
	UnreconciledOverrideUntil time.Time
}

// CapitalVerdict is the pure evaluation result.
type CapitalVerdict struct {
	Tier string
	// EffectiveRiskCapitalBase = min(declared, equity − floor); nil when
	// unapproved inputs or no usable equity observation.
	EffectiveRiskCapitalBase *float64
	// DrawdownBase and ConsumedPct measure from the cash-flow-adjusted
	DrawdownBase *float64
	ConsumedPct  *float64
	EquityStale  bool
	// ReconcileStale means the declared-events ledger is older than
	// capital.max_unreconciled_days (or never attested).
	ReconcileStale bool
	Unapproved     []string
	Reasons        []string
}

// UnreconciledClock is the shared pure projection of the constitution's
type UnreconciledClock struct {
	Approved      bool
	Deadline      time.Time
	DaysRemaining *int
	Stale         bool
}

// EvaluateUnreconciledClock computes the deadline used by both capital
// evaluation and reporting. The one-shot outage override may only extend the
// ordinary deadline; it can never shorten it.
func EvaluateUnreconciledClock(maxDays *int, lastReconciledAt, overrideUntil, now time.Time) UnreconciledClock {
	if maxDays == nil {
		return UnreconciledClock{}
	}
	out := UnreconciledClock{Approved: true}
	if lastReconciledAt.IsZero() {
		out.Stale = true
		return out
	}
	out.Deadline = lastReconciledAt.Add(time.Duration(*maxDays) * 24 * time.Hour)
	if overrideUntil.After(out.Deadline) {
		out.Deadline = overrideUntil
	}
	remaining := int(math.Ceil(out.Deadline.Sub(now).Hours() / 24))
	out.DaysRemaining = &remaining
	out.Stale = now.After(out.Deadline)
	return out
}

// EvaluateCapital applies the constitution to the runtime state and the
// latest observation. Invariants: absence of data or of approved numbers
// never yields ok; the latch dominates everything except unapproved
func EvaluateCapital(c *Constitution, rt CapitalRuntime, obs *CapitalObservation, now time.Time) CapitalVerdict {
	v := CapitalVerdict{Tier: CapitalTierUnknown}
	if c == nil {
		v.Tier = CapitalTierUnapproved
		v.Reasons = append(v.Reasons, "no risk policy file loaded; every capital control is unapproved")
		return v
	}
	v.Unapproved = c.UnapprovedKeys()

	// Reconciliation recency is reportable whenever its horizon exists.
	if clock := EvaluateUnreconciledClock(c.Capital.MaxUnreconciledDays, rt.LastReconciledAt, rt.UnreconciledOverrideUntil, now); clock.Approved {
		if clock.Stale {
			v.ReconcileStale = true
			reason := "capital ledger is past its reconcile horizon; declared events are unattested"
			if c.PolicyVersion >= 3 {
				reason = "reconcile evidence is past capital.max_unreconciled_days; no current automatic clean-report extension or human sign-off"
			}
			v.Reasons = append(v.Reasons, reason)
		}
	}

	usableObs := obs != nil && !obs.AsOf.IsZero() && obs.EquityBase > 0
	if usableObs && c.Capital.MaxEquityAgeMinutes != nil {
		if now.Sub(obs.AsOf) > time.Duration(*c.Capital.MaxEquityAgeMinutes)*time.Minute {
			v.EquityStale = true
			v.Reasons = append(v.Reasons, "equity observation is older than capital.max_equity_age_minutes")
		}
	}

	floor, declared := c.Capital.ProtectedFloor, c.Capital.DeclaredRiskCapital
	if usableObs && floor != nil && declared != nil {
		eff := min(*declared, obs.EquityBase-*floor)
		v.EffectiveRiskCapitalBase = &eff
	}
	if usableObs && rt.Seeded && declared != nil {
		adjusted := obs.EquityBase - rt.CumExternalFlowsBase
		dd := max(rt.AdjustedPeakBase-adjusted, 0)
		pct := dd / *declared * 100
		v.DrawdownBase = &dd
		v.ConsumedPct = &pct
	}

	// The latch dominates: a breached block stays block until a human
	if rt.BlockLatched {
		v.Tier = CapitalTierBlock
		v.Reasons = append(v.Reasons, "drawdown block is latched; a journaled human reset (with re-based peak) is required to resume risk")
		return v
	}
	if len(v.Unapproved) > 0 {
		v.Tier = CapitalTierUnapproved
		return v
	}
	if !usableObs || !rt.Seeded {
		v.Reasons = append(v.Reasons, "no usable equity observation; capital tier is unknown, never ok")
		return v
	}
	if v.EquityStale || v.ReconcileStale {
		return v // tier stays unknown: stale inputs never pass (decision 7)
	}
	switch {
	case v.ConsumedPct != nil && *v.ConsumedPct >= *c.Drawdown.BlockConsumedPct:
		v.Tier = CapitalTierBlock
	case v.ConsumedPct != nil && *v.ConsumedPct >= *c.Drawdown.WarnConsumedPct:
		v.Tier = CapitalTierWarn
	default:
		v.Tier = CapitalTierOK
	}
	return v
}
