package rpc

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
)

// Risk-constitution contract (internal-docs/design/risk-policy.md). policy.snapshot
// is read-only; the four write methods are governance acts, not broker
// writes — they are human-origin-only (originIsHuman), journaled, and none
// of them can touch submit eligibility, blockers, freeze, pins, tokens, or
// any gated broker-write path.

const (
	// MethodRiskPolicySnapshot returns the effective constitution, capital
	// state, drawdown tier, overrides, cadence records, and sibling-policy
	// pin drift. Works without gateway connectivity (state/config-only;
	// the equity observation degrades to the persisted last reading).
	MethodRiskPolicySnapshot = "policy.snapshot"
	// MethodRiskPolicyCapitalEvent declares a capital fact: deposit,
	// withdrawal, or reconcile attestation. Human-only.
	MethodRiskPolicyCapitalEvent = "policy.capital_event"
	// MethodRiskPolicyOverride grants a one-shot, expiring, single-control
	// override. Human-only.
	MethodRiskPolicyOverride = "policy.override"
	// MethodRiskPolicyResetDrawdown clears a latched drawdown block and
	// re-bases the adjusted peak. Human-only.
	MethodRiskPolicyResetDrawdown = "policy.reset_drawdown"
	// MethodRiskPolicyCorrectPeak lowers a corrupted adjusted peak to an
	// evidence-anchored value without touching the drawdown latch.
	// Corrections may only lower the peak; higher peaks are what the
	// observation path is for. Human-only.
	MethodRiskPolicyCorrectPeak = "policy.correct_peak"
	// MethodRiskPolicyArtefact records completion of a declared cadence
	// artefact. Human-only.
	MethodRiskPolicyArtefact = "policy.artefact"
)

// RiskConstitutionFingerprintVersion labels the constitution fingerprint.
// Distinct from the stress threshold policy's StressPolicyFingerprintVersion
// so the two identities can never be conflated in journals.
// RiskConstitutionFingerprintVersion identifies a semantic fingerprint projection.
const RiskConstitutionFingerprintVersion = "risk-constitution-fp-v1"

// Capital-flow and reconciliation source values distinguish declared facts,
// broker-statement evidence, and human or automated reconciliation.
const (
	CapitalFlowSourceDeclared  = "declared"
	CapitalFlowSourceStatement = "statement"
	ReconcileSourceHuman       = "human"
	ReconcileSourceAutomatic   = "automatic"
)

// Risk-policy statuses include absent because the constitution has no embedded
// default; a missing file is disclosed rather than silently substituted.
const (
	RiskPolicyStatusActive = "active"
	RiskPolicyStatusAbsent = "absent"
	RiskPolicyStatusDrift  = "drift"
	RiskPolicyStatusError  = "error"
)

// CapitalEventParams declares one capital fact in base currency.
type CapitalEventParams struct {
	// Type is deposit | withdrawal | reconcile.
	Type string `json:"type"`
	// AmountBase is required for deposit/withdrawal (positive), ignored
	// for reconcile.
	AmountBase float64 `json:"amount_base,omitempty"`
	// EffectiveAt is when the flow hit the account; zero means now. A
	// deposit declared after the peak already reflected it corrects the
	// peak downward (never-inflate discipline).
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	Note        string    `json:"note,omitempty"`
	// Report is required for type reconcile since phase 3a: the recon
	// report id being signed off. The daemon refuses a reconcile whose
	// report is missing, stale, superseded, or carries unresolved
	// exceptions (internal-docs/design/post-trade-truth.md).
	Report string `json:"report,omitempty"`
	// Origin is the write-origin claim; the daemon rejects non-human
	// origins for every risk-policy write.
	Origin string `json:"origin,omitempty"`
}

// OverrideParams grants a one-shot exception against one named control.
type OverrideParams struct {
	// Control is the constitution key being excepted (e.g.
	// "drawdown.warn_consumed_pct").
	Control string `json:"control"`
	Reason  string `json:"reason"`
	// Hours must be positive and at most override.max_duration_hours.
	Hours  int    `json:"hours"`
	Origin string `json:"origin,omitempty"`
}

// CorrectPeakParams repairs a poisoned adjusted peak. Exactly one anchor must
// be chosen: FromStatements re-derives the peak from the retained-statement
// replay (evidence-based), or PeakBase supplies an explicit value. The latch
// is deliberately untouched — clearing it stays reset_drawdown's job.
type CorrectPeakParams struct {
	FromStatements bool    `json:"from_statements,omitempty"`
	PeakBase       float64 `json:"peak_base,omitempty"`
	Reason         string  `json:"reason"`
	Origin         string  `json:"origin,omitempty"`
}

// ResetDrawdownParams clears the latch with a mandatory reason. The reset
// re-bases the adjusted peak to the current observation; reducing declared
// risk capital afterwards is a policy revision, and the result message says
// so.
type ResetDrawdownParams struct {
	Reason string `json:"reason"`
	Origin string `json:"origin,omitempty"`
}

// ArtefactParams records one completed cadence artefact.
type ArtefactParams struct {
	// Artefact is morning | eod | weekly.
	Artefact string `json:"artefact"`
	Note     string `json:"note,omitempty"`
	Origin   string `json:"origin,omitempty"`
	// BriefFingerprint is set only by brief.ack. The existing policy
	// artefact verb leaves it empty and remains wire-compatible.
	BriefFingerprint string `json:"brief_fingerprint,omitempty"`
}

// OverrideRecord is one override, active or expired, as journaled.
type OverrideRecord struct {
	ID                string    `json:"id"`
	Control           string    `json:"control"`
	Reason            string    `json:"reason"`
	GrantedAt         time.Time `json:"granted_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	PolicyFingerprint string    `json:"policy_fingerprint,omitempty"`
	Active            bool      `json:"active"`
}

// CapitalStateReport is the runtime capital state, evaluated.
type CapitalStateReport struct {
	Tier string `json:"tier"` // ok | warn | block | unknown | unapproved
	// Enforcement echoes the block tier's class so a "block" tier is
	// legible as shadow/advisory until promotion.
	Enforcement              string    `json:"enforcement"`
	EquityBase               *float64  `json:"equity_base,omitempty"`
	EquityAsOf               time.Time `json:"equity_as_of,omitzero"`
	EquityStale              bool      `json:"equity_stale,omitempty"`
	EffectiveRiskCapitalBase *float64  `json:"effective_risk_capital_base,omitempty"`
	AdjustedPeakBase         *float64  `json:"adjusted_peak_base,omitempty"`
	PeakAsOf                 time.Time `json:"peak_as_of,omitzero"`
	CumExternalFlowsBase     *float64  `json:"cum_external_flows_base,omitempty"`
	DeclaredCumFlowsBase     *float64  `json:"declared_cum_flows_base,omitempty"`
	StatementCumFlowsBase    *float64  `json:"statement_cum_flows_base,omitempty"`
	FlowSource               string    `json:"flow_source,omitempty"` // declared | statement
	DrawdownBase             *float64  `json:"drawdown_base,omitempty"`
	ConsumedPct              *float64  `json:"consumed_pct,omitempty"`
	BlockLatched             bool      `json:"block_latched"`
	LatchedAt                time.Time `json:"latched_at,omitzero"`
	// LatchConsumedPct is the consumed share at the moment the latch engaged.
	// Disclosed so a later data glitch inflating the live ConsumedPct cannot
	// retroactively misrepresent why the latch fired.
	LatchConsumedPct      *float64  `json:"latch_consumed_pct,omitempty"`
	LastReconciledAt      time.Time `json:"last_reconciled_at,omitzero"`
	LastReconcileReportID string    `json:"last_reconcile_report_id,omitempty"`
	LastReconcileSource   string    `json:"last_reconcile_source,omitempty"` // human | automatic
	ReconcileStale        bool      `json:"reconcile_stale,omitempty"`
	Reasons               []string  `json:"reasons,omitempty"`
	BaseCurrency          string    `json:"base_currency,omitempty"`
}

// PolicyPinStatus compares one constitution inventory pin with the live
// sibling policy identity.
type PolicyPinStatus struct {
	Policy        string `json:"policy"` // rulebook | protection | stress
	PinnedID      string `json:"pinned_id,omitempty"`
	PinnedVersion string `json:"pinned_version,omitempty"`
	LiveID        string `json:"live_id,omitempty"`
	LiveVersion   string `json:"live_version,omitempty"`
	// Status is match | drift | unpinned | unavailable.
	Status string `json:"status"`
}

// ArtefactRecord is the latest journaled completion of one cadence
// artefact.
type ArtefactRecord struct {
	Artefact         string    `json:"artefact"`
	Class            string    `json:"class,omitempty"`
	CompletedAt      time.Time `json:"completed_at,omitzero"`
	Note             string    `json:"note,omitempty"`
	Origin           string    `json:"origin,omitempty"`
	BriefFingerprint string    `json:"brief_fingerprint,omitempty"`
	// PolicyFingerprint is daemon-authored by later monthly brief.ack handling.
	// A monthly completion applies only to this policy identity; a changed
	// policy reopens the local month. The generic policy artefact verb does not
	// populate monthly metadata.
	PolicyFingerprint string `json:"policy_fingerprint,omitempty"`
	// Evidence is render-only monthly metadata populated by later brief.ack
	// handling. Origin alone is never stronger attention proof.
	Evidence string `json:"evidence,omitempty"`
}

// RiskPolicyResult is the policy.snapshot payload.
type RiskPolicyResult struct {
	AsOf time.Time `json:"as_of"`
	// Status is the manager state: active | absent | drift | error.
	Status  string `json:"status"`
	Source  string `json:"source,omitempty"` // file | none
	Path    string `json:"path,omitempty"`
	Message string `json:"message,omitempty"`

	PolicyID          string       `json:"policy_id,omitempty"`
	PolicyVersion     int          `json:"policy_version,omitempty"`
	PolicyFingerprint *Fingerprint `json:"policy_fingerprint,omitempty"`

	// Unapproved lists material keys the operator has not chosen; every
	// dependent control renders unapproved until they exist in the file.
	Unapproved []string `json:"unapproved,omitempty"`

	Capital   CapitalStateReport       `json:"capital"`
	Limits    []risk.ConstitutionLimit `json:"limits,omitempty"`
	Overrides []OverrideRecord         `json:"overrides,omitempty"`
	Cadence   []ArtefactRecord         `json:"cadence,omitempty"`
	Inventory []PolicyPinStatus        `json:"inventory,omitempty"`

	InputHealth []SourceHealth `json:"input_health,omitempty"`
}

// RiskPolicyWriteResult acknowledges one governance write.
type RiskPolicyWriteResult struct {
	OK       bool            `json:"ok"`
	At       time.Time       `json:"at"`
	Message  string          `json:"message,omitempty"`
	Override *OverrideRecord `json:"override,omitempty"`
}
