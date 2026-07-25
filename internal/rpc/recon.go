package rpc

import (
	"encoding/json"
	"errors"
	"time"
)

// Post-trade reconciliation contract (internal-docs/design/post-trade-truth.md).
// recon.snapshot is read-only and works from retained statement files;
// recon.dismiss is a human-only governance write. Nothing here touches
// broker writes, submit eligibility, or the order path.

const (
	// MethodReconSnapshot regenerates and returns the reconciliation
	// report from retained Flex statements and the declared capital-event
	// ledger. Params may request a background statement fetch.
	MethodReconSnapshot = "recon.snapshot"
	// MethodReconCheck requests one broker-report check and returns an
	// immediate, typed receipt. It is broker-read-only and never signs off,
	// dismisses an exception, or changes trading controls.
	MethodReconCheck = "recon.check"
	// MethodReconStatus returns only the redacted daily automation state. It
	// deliberately omits report rows, amounts, account data, and identifiers.
	MethodReconStatus = "recon.status"
	// MethodReconBacktest builds the full-window backtest report: every
	// statement flow labeled for the operator's flow-list review, plus the
	// capital-ladder replay over the statement equity series
	// (internal-docs/design/operator-ergonomics.md, accelerated R3 gate).
	// Measurement only — it changes no matching, sign-off, or enforcement.
	MethodReconBacktest = "recon.backtest"
	// MethodReconDismiss records a human resolution for one exception
	// line: reviewed, explained, deliberately not a ledger event.
	MethodReconDismiss = "recon.dismiss"
)

const (
	// ReconCheckOutcomeStarted means a new asynchronous check was accepted.
	ReconCheckOutcomeStarted = "started"
	// ReconCheckOutcomeAlreadyChecking means an existing check remains active.
	ReconCheckOutcomeAlreadyChecking = "already_checking"
	// ReconCheckOutcomeCooldown means retry is deferred by daemon cadence.
	ReconCheckOutcomeCooldown = "cooldown"
	// ReconCheckOutcomeActionRequired means automation cannot proceed without
	// resolving the redacted status reason.
	ReconCheckOutcomeActionRequired = "action_required"
)

// ReconCheckParams is deliberately an exact empty object. The paired app
// cannot smuggle report, account, policy, or trading instructions into this
// read-only action.
type ReconCheckParams struct{}

// MarshalJSON emits the canonical empty-object request.
func (ReconCheckParams) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

// UnmarshalJSON accepts only an exact empty object.
func (params *ReconCheckParams) UnmarshalJSON(data []byte) error {
	type wire ReconCheckParams
	var decoded wire
	if err := decodeExactNudgeJSONObject(data, nil, &decoded); err != nil {
		return err
	}
	*params = ReconCheckParams(decoded)
	return nil
}

// ReconStatusParams is an exact empty object because status scope is
// daemon-owned and callers cannot request private report detail.
type ReconStatusParams struct{}

// MarshalJSON emits the canonical empty-object request.
func (ReconStatusParams) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

// UnmarshalJSON accepts only an exact empty object.
func (params *ReconStatusParams) UnmarshalJSON(data []byte) error {
	type wire ReconStatusParams
	var decoded wire
	if err := decodeExactNudgeJSONObject(data, nil, &decoded); err != nil {
		return err
	}
	*params = ReconStatusParams(decoded)
	return nil
}

// Daily broker-report automation states. These values are deliberately
// narrow and prose-free so paired surfaces can render their own plain copy
// without exposing broker responses, local paths, or statement contents.
const (
	ReconReportStateWaiting        = "waiting"
	ReconReportStateDue            = "due"
	ReconReportStateChecking       = "checking"
	ReconReportStateCurrent        = "current"
	ReconReportStateRetryScheduled = "retry_scheduled"
	ReconReportStateActionRequired = "action_required"
	ReconReportStateUnavailable    = "unavailable"

	ReconReportReasonNone                 = ""
	ReconReportReasonBeforeDailyWindow    = "before_daily_window"
	ReconReportReasonCoveragePending      = "coverage_pending"
	ReconReportReasonReportNotReady       = "report_not_ready"
	ReconReportReasonServiceBusy          = "service_busy"
	ReconReportReasonRateLimited          = "rate_limited"
	ReconReportReasonNetworkUnavailable   = "network_unavailable"
	ReconReportReasonFlexDisabled         = "flex_disabled"
	ReconReportReasonQueryMissing         = "query_missing"
	ReconReportReasonTokenMissing         = "token_missing"
	ReconReportReasonTokenInvalid         = "token_invalid"
	ReconReportReasonTokenExpired         = "token_expired"
	ReconReportReasonQueryInvalid         = "query_invalid"
	ReconReportReasonIPRestricted         = "ip_restricted"
	ReconReportReasonServiceInactive      = "service_inactive"
	ReconReportReasonResponseInvalid      = "response_invalid"
	ReconReportReasonReportInvalid        = "report_invalid"
	ReconReportReasonStorageFailed        = "storage_failed"
	ReconReportReasonProjectionFailed     = "projection_failed"
	ReconReportReasonAuthorityUnavailable = "authority_unavailable"

	ReconEvaluationStateWaiting           = "waiting"
	ReconEvaluationStateChecking          = "checking"
	ReconEvaluationStateComplete          = "complete"
	ReconEvaluationStateAttentionRequired = "attention_required"
	ReconEvaluationStateFailed            = "failed"

	ReconEvaluationReasonNone                 = ""
	ReconEvaluationReasonReportPending        = "report_pending"
	ReconEvaluationReasonAccountValuePending  = "account_value_pending"
	ReconEvaluationReasonExceptionsNeedReview = "exceptions_need_review"
	ReconEvaluationReasonAccountValueMismatch = "account_value_mismatch"
	ReconEvaluationReasonEvaluationFailed     = "evaluation_failed"
	ReconEvaluationReasonPolicyUnapproved     = "policy_unapproved"
)

// Recon report statuses.
const (
	ReconStatusActive      = "active"      // report produced under approved recon keys
	ReconStatusUnapproved  = "unapproved"  // [recon] policy keys missing; no matching possible
	ReconStatusUnavailable = "unavailable" // no retained statements yet
	ReconStatusDegraded    = "degraded"    // report produced but some retained files failed to parse
)

// Recon exception categories.
const (
	ReconMissingFromLedger = "missing_from_ledger"
	ReconLedgerOnly        = "ledger_only"
	ReconAmountMismatch    = "amount_mismatch"
	ReconDateMismatch      = "date_mismatch"
	ReconAmbiguous         = "ambiguous"
	ReconUncategorized     = "uncategorized"
)

// ReconBaseline is not an exception category. It identifies pre-genesis
// statement flows whose one valid treatment is inclusion in the seeded
// baseline.
const ReconBaseline = "baseline"

// ReconConfirmed is a normal v3 statement-authoritative flow. It is
// disclosed and report-id-pinned, but is not an exception or a signature
// target because declarations are optional after the authority flip.
const ReconConfirmed = "confirmed"

// ReconSnapshotParams tunes one snapshot call.
type ReconSnapshotParams struct {
	// Refresh kicks one background statement fetch (single-flight); the
	// returned report is still built from already-retained files.
	Refresh bool `json:"refresh,omitempty"`
}

// ReconException is the shared row shape for an exception or disclosed flow.
// Amounts are base-currency and stay on this local surface.
type ReconException struct {
	LineID      string    `json:"line_id"`
	Category    string    `json:"category"`
	Type        string    `json:"type,omitempty"`
	Description string    `json:"description,omitempty"`
	ValueDate   time.Time `json:"value_date,omitzero"`
	AmountBase  *float64  `json:"amount_base,omitempty"`
	// EventAt/EventAmountBase reference the declared event side of a
	// mismatch or ledger_only exception.
	EventAt         time.Time `json:"event_at,omitzero"`
	EventAmountBase *float64  `json:"event_amount_base,omitempty"`
	// PreGenesis marks a flow value-dated before the runtime capital
	// state's genesis. Such usable statement flows are returned in
	// ReconResult.Baseline when the runtime state is seeded.
	PreGenesis    bool   `json:"pre_genesis,omitempty"`
	Note          string `json:"note,omitempty"`
	Dismissed     bool   `json:"dismissed,omitempty"`
	DismissReason string `json:"dismiss_reason,omitempty"`
}

// ReconEquityCheck compares the statement equity series with the runtime
// capital state — a data-quality disclosure, not an exception. Divergence
// is computed only from a same-day pair; when SameDay is false,
// RuntimeEquityBase and RuntimeAsOf are the latest observation for context
// only and DivergencePct is absent.
type ReconEquityCheck struct {
	StatementDate      time.Time `json:"statement_date,omitzero"`
	StatementTotalBase float64   `json:"statement_total_base"`
	RuntimeEquityBase  *float64  `json:"runtime_equity_base,omitempty"`
	RuntimeAsOf        time.Time `json:"runtime_as_of,omitzero"`
	DivergencePct      *float64  `json:"divergence_pct,omitempty"`
	SameDay            bool      `json:"same_day"`
}

// ReconBacktestFlow labels one statement flow for the operator's full-window
// flow-list review.
type ReconBacktestFlow struct {
	LineID      string    `json:"line_id"`
	Type        string    `json:"type,omitempty"`
	Description string    `json:"description,omitempty"`
	ValueDate   time.Time `json:"value_date,omitzero"`
	AmountBase  *float64  `json:"amount_base,omitempty"`
	PreGenesis  bool      `json:"pre_genesis,omitempty"`
	// Status is "matched", ReconBaseline, or the recon exception category
	// the flow carries on the current report.
	Status    string `json:"status"`
	Dismissed bool   `json:"dismissed,omitempty"`
}

// ReconBacktestCrossing compares the first replayed crossing of one capital
// tier with the runtime journal observation, when recorded.
type ReconBacktestCrossing struct {
	Tier                string    `json:"tier"` // warn | block
	ReplayedAt          time.Time `json:"replayed_at,omitzero"`
	ReplayedConsumedPct float64   `json:"replayed_consumed_pct"`
	RuntimeAt           time.Time `json:"runtime_at,omitzero"`
}

// ReconBacktestReplay is the capital-ladder replay over broker statement EOD
// equity, with the comparable runtime observations disclosed alongside it.
type ReconBacktestReplay struct {
	Days                    int                     `json:"days"`
	FirstDay                time.Time               `json:"first_day,omitzero"`
	LastDay                 time.Time               `json:"last_day,omitzero"`
	ReplayedPeakBase        float64                 `json:"replayed_peak_base"`
	ReplayedPeakAt          time.Time               `json:"replayed_peak_at,omitzero"`
	RuntimePeakBase         *float64                `json:"runtime_peak_base,omitempty"`
	RuntimePeakAt           time.Time               `json:"runtime_peak_at,omitzero"`
	PeakDivergencePct       *float64                `json:"peak_divergence_pct,omitempty"`
	Crossings               []ReconBacktestCrossing `json:"crossings,omitempty"`
	SameDayComparisons      int                     `json:"same_day_comparisons"`
	MaxSameDayDivergencePct *float64                `json:"max_same_day_divergence_pct,omitempty"`
	Notes                   []string                `json:"notes,omitempty"`
}

// ReconBacktestResult is the full-window recon backtest payload. It is
// read-only measurement and changes no matching, sign-off, or enforcement.
type ReconBacktestResult struct {
	AsOf               time.Time            `json:"as_of"`
	Status             string               `json:"status"`
	ReportID           string               `json:"report_id,omitempty"`
	StatementAsOf      time.Time            `json:"statement_as_of,omitzero"`
	CoverageFrom       time.Time            `json:"coverage_from,omitzero"`
	CoverageTo         time.Time            `json:"coverage_to,omitzero"`
	GenesisAt          time.Time            `json:"genesis_at,omitzero"`
	PolicyFingerprint  *Fingerprint         `json:"policy_fingerprint,omitempty"`
	Flows              []ReconBacktestFlow  `json:"flows,omitempty"`
	FlowCounts         map[string]int       `json:"flow_counts,omitempty"`
	ClassifiedCounts   map[string]int       `json:"classified_counts,omitempty"`
	UncategorizedCount int                  `json:"uncategorized_count"`
	EquityDays         int                  `json:"equity_days"`
	Replay             *ReconBacktestReplay `json:"replay,omitempty"`
	Message            string               `json:"message,omitempty"`
	InputHealth        []SourceHealth       `json:"input_health,omitempty"`
}

// ReconFetchStatus reports statement-source health. It never carries the
// token or any request detail.
type ReconFetchStatus struct {
	Configured         bool      `json:"configured"`
	State              string    `json:"state"`
	Reason             string    `json:"reason,omitempty"`
	ExpectedCoverageTo time.Time `json:"expected_coverage_to,omitzero"`
	CoverageTo         time.Time `json:"coverage_to,omitzero"`
	LastSuccess        time.Time `json:"last_success,omitzero"`
	LastAttempt        time.Time `json:"last_attempt,omitzero"`
	NextAttempt        time.Time `json:"next_attempt,omitzero"`
	RetryAutomatic     bool      `json:"retry_automatic"`
	CanCheckNow        bool      `json:"can_check_now"`
	Busy               bool      `json:"busy"`
	// LastError is retained for CLI compatibility, but is now derived only
	// from Reason. It never contains broker prose, paths, URLs, or parser
	// details and is not forwarded by the paired-app DTO.
	LastError string `json:"last_error,omitempty"`
}

// ReconEvaluationStatus reports what happened after the broker report was
// acquired. It intentionally does not expose report ids, amounts, or policy
// thresholds.
type ReconEvaluationStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// ReconAutomationStatus keeps acquisition and evaluation separate so a
// report outage is never presented as a policy/evaluation failure (or vice
// versa).
type ReconAutomationStatus struct {
	Report     ReconFetchStatus      `json:"report"`
	Evaluation ReconEvaluationStatus `json:"evaluation"`
}

// ReconCheckResult is the immediate receipt returned by recon.check. Status
// is a fresh redacted snapshot; completion is observed by polling status,
// never inferred from the receipt alone.
type ReconCheckResult struct {
	Outcome string                `json:"outcome"`
	Status  ReconAutomationStatus `json:"status"`
}

// ReconStatusResult wraps the redacted automation status returned by
// MethodReconStatus.
type ReconStatusResult struct {
	Status ReconAutomationStatus `json:"status"`
}

// MarshalJSON validates automation-state coherence before encoding.
func (result ReconStatusResult) MarshalJSON() ([]byte, error) {
	if err := ValidateReconAutomationStatus(result.Status); err != nil {
		return nil, err
	}
	type wire ReconStatusResult
	return json.Marshal(wire(result))
}

// UnmarshalJSON accepts only the exact validated status wrapper.
func (result *ReconStatusResult) UnmarshalJSON(data []byte) error {
	type wire ReconStatusResult
	var decoded wire
	if err := decodeExactNudgeJSONObject(data, []string{"status"}, &decoded); err != nil {
		return err
	}
	value := ReconStatusResult(decoded)
	if err := ValidateReconAutomationStatus(value.Status); err != nil {
		return err
	}
	*result = value
	return nil
}

// MarshalJSON validates the outcome and automation state before encoding.
func (result ReconCheckResult) MarshalJSON() ([]byte, error) {
	if err := ValidateReconCheckResult(result); err != nil {
		return nil, err
	}
	type wire ReconCheckResult
	return json.Marshal(wire(result))
}

// UnmarshalJSON accepts only the exact validated check receipt.
func (result *ReconCheckResult) UnmarshalJSON(data []byte) error {
	type wire ReconCheckResult
	var decoded wire
	if err := decodeExactNudgeJSONObject(data, []string{"outcome", "status"}, &decoded); err != nil {
		return err
	}
	value := ReconCheckResult(decoded)
	if err := ValidateReconCheckResult(value); err != nil {
		return err
	}
	*result = value
	return nil
}

// ValidateReconCheckResult rejects unknown outcomes and incoherent status.
func ValidateReconCheckResult(result ReconCheckResult) error {
	switch result.Outcome {
	case ReconCheckOutcomeStarted, ReconCheckOutcomeAlreadyChecking,
		ReconCheckOutcomeCooldown, ReconCheckOutcomeActionRequired:
	default:
		return errors.New("invalid reconciliation check outcome")
	}
	return ValidateReconAutomationStatus(result.Status)
}

// ValidateReconAutomationStatus rejects unknown or incoherent values before
// an adapter can publish them. The contract is intentionally exact: callers
// must map new internal failures to an existing safe reason or revise every
// consumer deliberately.
func ValidateReconAutomationStatus(status ReconAutomationStatus) error {
	reportReasons := map[string]bool{
		ReconReportReasonNone: true, ReconReportReasonBeforeDailyWindow: true,
		ReconReportReasonCoveragePending: true, ReconReportReasonReportNotReady: true,
		ReconReportReasonServiceBusy: true, ReconReportReasonRateLimited: true,
		ReconReportReasonNetworkUnavailable: true, ReconReportReasonFlexDisabled: true,
		ReconReportReasonQueryMissing: true, ReconReportReasonTokenMissing: true,
		ReconReportReasonTokenInvalid: true, ReconReportReasonTokenExpired: true,
		ReconReportReasonQueryInvalid: true, ReconReportReasonIPRestricted: true,
		ReconReportReasonServiceInactive: true, ReconReportReasonResponseInvalid: true,
		ReconReportReasonReportInvalid: true, ReconReportReasonStorageFailed: true,
		ReconReportReasonProjectionFailed: true, ReconReportReasonAuthorityUnavailable: true,
	}
	reportStates := map[string]bool{
		ReconReportStateWaiting: true, ReconReportStateDue: true,
		ReconReportStateChecking: true, ReconReportStateCurrent: true,
		ReconReportStateRetryScheduled: true, ReconReportStateActionRequired: true,
		ReconReportStateUnavailable: true,
	}
	evaluationStates := map[string]bool{
		ReconEvaluationStateWaiting: true, ReconEvaluationStateChecking: true,
		ReconEvaluationStateComplete: true, ReconEvaluationStateAttentionRequired: true,
		ReconEvaluationStateFailed: true,
	}
	evaluationReasons := map[string]bool{
		ReconEvaluationReasonNone: true, ReconEvaluationReasonReportPending: true,
		ReconEvaluationReasonAccountValuePending:  true,
		ReconEvaluationReasonExceptionsNeedReview: true,
		ReconEvaluationReasonAccountValueMismatch: true,
		ReconEvaluationReasonEvaluationFailed:     true,
		ReconEvaluationReasonPolicyUnapproved:     true,
	}
	if !reportStates[status.Report.State] || !reportReasons[status.Report.Reason] {
		return errors.New("invalid reconciliation report automation state")
	}
	if !evaluationStates[status.Evaluation.State] || !evaluationReasons[status.Evaluation.Reason] {
		return errors.New("invalid reconciliation evaluation automation state")
	}
	if status.Report.Busy != (status.Report.State == ReconReportStateChecking) {
		return errors.New("reconciliation report busy flag and state disagree")
	}
	if status.Report.State == ReconReportStateCurrent && status.Report.Reason != ReconReportReasonNone {
		return errors.New("current reconciliation report carries a failure reason")
	}
	switch status.Report.State {
	case ReconReportStateWaiting:
		if status.Report.Reason != ReconReportReasonBeforeDailyWindow {
			return errors.New("waiting reconciliation report has an invalid reason")
		}
	case ReconReportStateDue:
		if status.Report.Reason != ReconReportReasonCoveragePending {
			return errors.New("due reconciliation report has an invalid reason")
		}
	case ReconReportStateChecking:
		if status.Report.Reason != ReconReportReasonNone && status.Report.Reason != ReconReportReasonCoveragePending {
			return errors.New("checking reconciliation report has an invalid reason")
		}
	case ReconReportStateRetryScheduled:
		switch status.Report.Reason {
		case ReconReportReasonCoveragePending, ReconReportReasonReportNotReady,
			ReconReportReasonServiceBusy, ReconReportReasonRateLimited,
			ReconReportReasonNetworkUnavailable, ReconReportReasonResponseInvalid,
			ReconReportReasonReportInvalid, ReconReportReasonStorageFailed,
			ReconReportReasonProjectionFailed:
		default:
			return errors.New("retrying reconciliation report has an invalid reason")
		}
	case ReconReportStateActionRequired:
		switch status.Report.Reason {
		case ReconReportReasonFlexDisabled, ReconReportReasonQueryMissing,
			ReconReportReasonTokenMissing, ReconReportReasonTokenInvalid,
			ReconReportReasonTokenExpired, ReconReportReasonQueryInvalid,
			ReconReportReasonIPRestricted, ReconReportReasonServiceInactive:
		default:
			return errors.New("action-required reconciliation report has an invalid reason")
		}
	case ReconReportStateUnavailable:
		if status.Report.Reason != ReconReportReasonAuthorityUnavailable && status.Report.Reason != ReconReportReasonNetworkUnavailable {
			return errors.New("unavailable reconciliation report has an invalid reason")
		}
	}
	switch status.Evaluation.State {
	case ReconEvaluationStateWaiting:
		if status.Evaluation.Reason != ReconEvaluationReasonReportPending && status.Evaluation.Reason != ReconEvaluationReasonAccountValuePending {
			return errors.New("waiting reconciliation evaluation has an invalid reason")
		}
	case ReconEvaluationStateChecking:
		if status.Evaluation.Reason != ReconEvaluationReasonReportPending {
			return errors.New("checking reconciliation evaluation has an invalid reason")
		}
	case ReconEvaluationStateComplete:
		if status.Evaluation.Reason != ReconEvaluationReasonNone {
			return errors.New("complete reconciliation evaluation carries a reason")
		}
	case ReconEvaluationStateAttentionRequired:
		if status.Evaluation.Reason != ReconEvaluationReasonExceptionsNeedReview && status.Evaluation.Reason != ReconEvaluationReasonAccountValueMismatch {
			return errors.New("attention-required reconciliation evaluation has an invalid reason")
		}
	case ReconEvaluationStateFailed:
		if status.Evaluation.Reason != ReconEvaluationReasonEvaluationFailed && status.Evaluation.Reason != ReconEvaluationReasonPolicyUnapproved {
			return errors.New("failed reconciliation evaluation has an invalid reason")
		}
	}
	if status.Report.State != ReconReportStateCurrent && status.Evaluation.State == ReconEvaluationStateComplete {
		return errors.New("reconciliation evaluation is complete without a current report")
	}
	return nil
}

// ReconResult is the recon.snapshot payload.
type ReconResult struct {
	AsOf   time.Time `json:"as_of"`
	Status string    `json:"status"`
	// ReportID pins the exact exception and baseline sets; the reconcile
	// verb must reference it and refuses when unresolved exceptions remain.
	ReportID string `json:"report_id,omitempty"`
	// StatementAsOf is when the newest ingested statement was generated
	// by IBKR — the freshness the max_report_age_days policy key bounds.
	StatementAsOf          time.Time             `json:"statement_as_of,omitzero"`
	CoverageFrom           time.Time             `json:"coverage_from,omitzero"`
	CoverageTo             time.Time             `json:"coverage_to,omitzero"`
	GenesisAt              time.Time             `json:"genesis_at,omitzero"`
	Counts                 map[string]int        `json:"counts,omitempty"`
	Exceptions             []ReconException      `json:"exceptions,omitempty"`
	Baseline               []ReconException      `json:"baseline,omitempty"`
	Confirmed              []ReconException      `json:"confirmed,omitempty"`
	Unresolved             int                   `json:"unresolved"`
	StatementCumFlowsBase  *float64              `json:"statement_cum_flows_base,omitempty"`
	LastAutoExtendReportID string                `json:"last_auto_extend_report_id,omitempty"`
	LastAutoExtendedAt     time.Time             `json:"last_auto_extended_at,omitzero"`
	Equity                 *ReconEquityCheck     `json:"equity,omitempty"`
	Fetch                  ReconFetchStatus      `json:"fetch"`
	Automation             ReconAutomationStatus `json:"automation"`
	Message                string                `json:"message,omitempty"`
	InputHealth            []SourceHealth        `json:"input_health,omitempty"`
}

// ReconDismissParams records one human resolution.
type ReconDismissParams struct {
	LineID string `json:"line_id"`
	Reason string `json:"reason"`
	Origin string `json:"origin,omitempty"`
}
