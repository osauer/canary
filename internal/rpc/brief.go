package rpc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/risk"
	"io"
	"math"
	"strings"
	"time"
)

// Brief constants define the daemon methods and the bounded status, kind,
// monthly-pulse, and acknowledgement vocabularies carried on the wire.
const (
	// MethodBriefSnapshot composes the operator's daily brief. It is a pure
	// read: callers do not supply an origin and the daemon must not stamp,
	// journal, or advance any runtime clock while serving it.
	MethodBriefSnapshot = "brief.snapshot"
	// BriefStatusOK is the normal member of the brief row status vocabulary.
	// Brief row statuses separate risk conditions from data conditions:
	// attention means the underlying VALUES describe a state a trader must
	// look at (latched drawdown, breached tier, active override); degraded
	// and unavailable describe input quality only and must never be used to
	// signal a risk condition, nor vice versa.
	BriefStatusOK          = "ok"
	BriefStatusAttention   = "attention"
	BriefStatusDegraded    = "degraded"
	BriefStatusUnavailable = "unavailable"

	// BriefKindMorning identifies the pre-trade morning brief.
	BriefKindMorning = "morning"
	// BriefKindEOD identifies the end-of-day brief.
	BriefKindEOD = "eod"
	// BriefKindMonthly identifies the monthly governance pulse.
	BriefKindMonthly = "monthly"

	// BriefMonthlyPulseNotDue means the monthly pulse has no current action.
	BriefMonthlyPulseNotDue = "not_due"
	// BriefMonthlyPulseCompleted means the current pulse has valid evidence.
	BriefMonthlyPulseCompleted = "completed"
	// BriefMonthlyPulseBlocked means completion prerequisites are unavailable.
	BriefMonthlyPulseBlocked = "blocked"
)

// BriefSnapshotParams is deliberately empty. In particular it carries no
// origin: reads never gain write authority from their caller.
type BriefSnapshotParams struct{}

// BriefRowState is embedded by every brief row and section. Detail is
// human-facing disclosure; Status is one of ok, attention, degraded, or
// unavailable. Sections roll up their worst child (attention outranks
// degraded) and state completeness in Detail.
type BriefRowState struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// BriefMarketSection groups broad-market and Stress rows.
type BriefMarketSection struct {
	BriefRowState
	Regime  BriefRegimeRow  `json:"regime"`
	Breadth BriefBreadthRow `json:"breadth"`
	Gamma   BriefGammaRow   `json:"gamma"`
	Stress  BriefStressRow  `json:"stress"`
}

// BriefRegimeRow summarizes the current regime lifecycle and verdict.
type BriefRegimeRow struct {
	BriefRowState
	Stage   string `json:"stage,omitempty"`
	Verdict string `json:"verdict,omitempty"`
}

// BriefBreadthRow summarizes breadth values and their observation time. Nil
type BriefBreadthRow struct {
	BriefRowState
	PctAbove50DMA  *float64  `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA *float64  `json:"pct_above_200dma,omitempty"`
	NetNewHighsPct *float64  `json:"net_new_highs_pct,omitempty"`
	AsOf           time.Time `json:"as_of,omitzero"`
	DataType       string    `json:"data_type,omitempty"`
}

// BriefGammaRow summarizes the current zero-gamma relationship. Nil values
type BriefGammaRow struct {
	BriefRowState
	Spot      *float64  `json:"spot,omitempty"`
	ZeroGamma *float64  `json:"zero_gamma,omitempty"`
	GapPct    *float64  `json:"gap_pct,omitempty"`
	GammaSign string    `json:"gamma_sign,omitempty"`
	AsOf      time.Time `json:"as_of,omitzero"`
}

// BriefStressRow summarizes the current advisory action and severity.
type BriefStressRow struct {
	BriefRowState
	Action   string `json:"action,omitempty"`
	Severity string `json:"severity,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

// BriefCalendarSection groups session and held-name event context.
type BriefCalendarSection struct {
	BriefRowState
	Session      BriefSessionRow       `json:"session"`
	MarketEvents []BriefMarketEventRow `json:"market_events"`
}

// BriefSessionRow reports the official market session and next opening time.
type BriefSessionRow struct {
	BriefRowState
	Market   string    `json:"market,omitempty"`
	State    string    `json:"state,omitempty"`
	IsOpen   bool      `json:"is_open"`
	Open     time.Time `json:"open,omitzero"`
	Close    time.Time `json:"close,omitzero"`
	NextOpen time.Time `json:"next_open,omitzero"`
}

// BriefMarketEventRow summarizes one approved held-name event family.
type BriefMarketEventRow struct {
	BriefRowState
	Kind    string   `json:"kind"` // earnings | halt | ssr | borrow
	Count   int      `json:"count"`
	Symbols []string `json:"symbols,omitempty"`
}

// BriefPortfolioSection groups account, attribution, option-money, and order
type BriefPortfolioSection struct {
	BriefRowState
	Account       BriefAccountRow       `json:"account"`
	Movers        BriefMoversRow        `json:"movers"`
	PremiumAtRisk BriefMoneyCoverageRow `json:"premium_at_risk"`
	HedgeCost     BriefMoneyCoverageRow `json:"hedge_cost"`
	WorkingOrders BriefCountRow         `json:"working_orders"`
}

// BriefAccountRow reports base-currency equity and P&L. Nil amounts mean the
// observation is unavailable.
type BriefAccountRow struct {
	BriefRowState
	EquityBase   *float64  `json:"equity_base,omitempty"`
	DailyPnLBase *float64  `json:"daily_pnl_base,omitempty"`
	BaseCurrency string    `json:"base_currency,omitempty"`
	AsOf         time.Time `json:"as_of,omitzero"`
}

// BriefMover is one underlying's base-currency daily P&L contribution.
type BriefMover struct {
	Symbol       string  `json:"symbol"`
	DailyPnLBase float64 `json:"daily_pnl_base"`
}

// BriefMoversRow aggregates daily P&L by underlying (stock plus option legs
// so the row's implied total matches the account daily P&L attribution.
type BriefMoversRow struct {
	BriefRowState
	Rows         []BriefMover `json:"rows"`
	OtherPnLBase *float64     `json:"other_daily_pnl_base,omitempty"`
	OtherCount   int          `json:"other_count,omitempty"`
}

// BriefMoneyCoverageRow reports a base-currency aggregate and explicit leg
type BriefMoneyCoverageRow struct {
	BriefRowState
	AmountBase   *float64 `json:"amount_base,omitempty"`
	BaseCurrency string   `json:"base_currency,omitempty"`
	IncludedLegs int      `json:"included_legs"`
	ExcludedLegs int      `json:"excluded_legs"`
}

// BriefCountRow reports an optional count; nil means unavailable, not zero.
type BriefCountRow struct {
	BriefRowState
	Count *int `json:"count,omitempty"`
}

// BriefRiskSection groups capital, latch, override, and policy-drift evidence.
type BriefRiskSection struct {
	BriefRowState
	Capital     BriefCapitalRow     `json:"capital"`
	Latch       BriefLatchRow       `json:"latch"`
	Overrides   BriefOverridesRow   `json:"overrides"`
	PolicyDrift BriefPolicyDriftRow `json:"policy_drift"`
}

// BriefCapitalRow reports drawdown capacity and peak provenance. Pointer
type BriefCapitalRow struct {
	BriefRowState
	Tier             string   `json:"tier,omitempty"`
	Enforcement      string   `json:"enforcement,omitempty"`
	ConsumedPct      *float64 `json:"consumed_pct,omitempty"`
	DrawdownBase     *float64 `json:"drawdown_base,omitempty"`
	AdjustedPeakBase *float64 `json:"adjusted_peak_base,omitempty"`
	// PeakAsOf is when the current adjusted peak was observed. Provenance,
	// not decoration: a peak stamped during a closed session or a reconnect
	// window is the tell that exposes a poisoned observation.
	PeakAsOf     time.Time `json:"peak_as_of,omitzero"`
	BaseCurrency string    `json:"base_currency,omitempty"`
}

// BriefLatchRow reports drawdown-latch state and its original trigger.
type BriefLatchRow struct {
	BriefRowState
	Latched bool      `json:"latched"`
	At      time.Time `json:"latched_at,omitzero"`
	// Provisional means the broker statement covering the latch day has not
	// yet confirmed the latch or dissolved it.
	Provisional bool `json:"provisional,omitempty"`
	AgeDays     *int `json:"age_days,omitempty"`
	// ConsumedPctAtLatch is the consumed share recorded when the latch
	ConsumedPctAtLatch *float64  `json:"consumed_pct_at_latch,omitempty"`
	ReportCoverageTo   time.Time `json:"report_coverage_to,omitzero"`
	ReportCheckedAt    time.Time `json:"report_checked_at,omitzero"`
}

// BriefOverride identifies one active control override and expiry.
type BriefOverride struct {
	Control   string    `json:"control"`
	ExpiresAt time.Time `json:"expires_at"`
}

// BriefOverridesRow lists active overrides; an empty list is conclusive only
// when the embedded row state is OK.
type BriefOverridesRow struct {
	BriefRowState
	Rows []BriefOverride `json:"rows"`
}

// BriefPolicyDriftRow lists sibling-policy pin status.
type BriefPolicyDriftRow struct {
	BriefRowState
	Rows []PolicyPinStatus `json:"rows"`
}

// BriefProcessSection groups reconciliation and recurring process evidence.
type BriefProcessSection struct {
	BriefRowState
	Reconcile    BriefReconcileRow     `json:"reconcile"`
	AutoExtend   BriefAutoExtendRow    `json:"auto_extend"`
	Rules        BriefRulesRow         `json:"rules"`
	MonthlyPulse *BriefMonthlyPulseRow `json:"monthly_pulse,omitempty"`
}

// BriefMonthlyPulseRow has its own status vocabulary rather than embedding
type BriefMonthlyPulseRow struct {
	Status      string    `json:"status"` // not_due | completed | blocked
	Month       string    `json:"month,omitempty"`
	DueAt       time.Time `json:"due_at,omitzero"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

// BriefReconcileRow reports the latest reconciliation and its next deadline.
type BriefReconcileRow struct {
	BriefRowState
	LastReconciledAt time.Time `json:"last_reconciled_at,omitzero"`
	Source           string    `json:"source,omitempty"`
	Deadline         time.Time `json:"deadline,omitzero"`
	DaysRemaining    *int      `json:"days_remaining,omitempty"`
}

// BriefAutoExtendRow reports clean-report automatic extension evidence.
type BriefAutoExtendRow struct {
	BriefRowState
	ReportID string    `json:"report_id,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

// BriefRulesRow summarizes current policy adherence. It deliberately uses no
// per-rule identifiers: status counts are the whole payload.
type BriefRulesRow struct {
	BriefRowState
	Pass         int `json:"pass"`
	Info         int `json:"info"`
	Watch        int `json:"watch"`
	Act          int `json:"act"`
	Track        int `json:"track"`
	Unknown      int `json:"unknown"`
	NotEvaluated int `json:"not_evaluated"`
}

// BriefProposalsRow reports how many protection proposals were offered versus
// acted on; no proposal keys, symbols, order references, or tokens reach the wire.
type BriefProposalsRow struct {
	BriefRowState
	Day     string `json:"day,omitempty"`
	Offered int    `json:"offered"`
	Acted   int    `json:"acted"`
}

// BriefReadyProposalsRow reports how many protection proposals the daemon
// symbols, contracts, order references, or preview tokens reach the wire.
// Stating that work is staged is not authority to place it — every submit
type BriefReadyProposalsRow struct {
	BriefRowState
	// Actionable is the served count of proposals with no blockers; Blocked
	// is the remainder of Total. Zero is a measured zero only when the
	// embedded row state is OK.
	Actionable int `json:"actionable"`
	Blocked    int `json:"blocked"`
	Total      int `json:"total"`
}

// BriefCapitalEventsRow frames the drawdown latch and adjusted-peak provenance
type BriefCapitalEventsRow struct {
	BriefRowState
	Latched            bool      `json:"latched"`
	LatchedAt          time.Time `json:"latched_at,omitzero"`
	LatchProvisional   bool      `json:"latch_provisional,omitempty"`
	LatchAgeDays       *int      `json:"latch_age_days,omitempty"`
	ConsumedPctAtLatch *float64  `json:"consumed_pct_at_latch,omitempty"`
	AdjustedPeakBase   *float64  `json:"adjusted_peak_base,omitempty"`
	PeakAsOf           time.Time `json:"peak_as_of,omitzero"`
	BaseCurrency       string    `json:"base_currency,omitempty"`
	ReportCoverageTo   time.Time `json:"report_coverage_to,omitzero"`
	ReportCheckedAt    time.Time `json:"report_checked_at,omitzero"`
}

// BriefLastSessionRow is the daemon's close capture of the last completed
// session's account Daily P&L: the reqPnL account frame observed at (or on
// date. Unlike SessionPnL it never moves on off-session marks. Nil
// surfaces must say so rather than substitute a drifted running value.
type BriefLastSessionRow struct {
	BriefRowState
	SessionDate  string    `json:"session_date,omitempty"`
	DailyPnLBase *float64  `json:"daily_pnl_base,omitempty"`
	BaseCurrency string    `json:"base_currency,omitempty"`
	SessionClose time.Time `json:"session_close,omitzero"`
	CapturedAt   time.Time `json:"captured_at,omitzero"`
}

// BriefReviewSection is the post-trade movement since the last regular close.
// the section rolls up its worst child exactly like every other brief section.
type BriefReviewSection struct {
	BriefRowState
	SessionPnL    BriefAccountRow       `json:"session_pnl"`
	LastSession   BriefLastSessionRow   `json:"last_session"`
	Attribution   BriefMoversRow        `json:"attribution"`
	Proposals     BriefProposalsRow     `json:"proposals"`
	Overrides     BriefOverridesRow     `json:"overrides"`
	CapitalEvents BriefCapitalEventsRow `json:"capital_events"`
	Rules         BriefRulesRow         `json:"rules"`
	Reconcile     BriefReconcileRow     `json:"reconcile"`
	AutoExtend    BriefAutoExtendRow    `json:"auto_extend"`
	WorkingOrders BriefCountRow         `json:"working_orders"`
}

// BriefReadySection is the pre-trade movement for today. Its rows regroup the
// existing market, calendar, risk-capacity, and desk-readiness facts.
type BriefReadySection struct {
	BriefRowState
	Regime        BriefRegimeRow         `json:"regime"`
	Breadth       BriefBreadthRow        `json:"breadth"`
	Gamma         BriefGammaRow          `json:"gamma"`
	Stress        BriefStressRow         `json:"stress"`
	Session       BriefSessionRow        `json:"session"`
	MarketEvents  []BriefMarketEventRow  `json:"market_events"`
	Capital       BriefCapitalRow        `json:"capital"`
	Latch         BriefLatchRow          `json:"latch"`
	PremiumAtRisk BriefMoneyCoverageRow  `json:"premium_at_risk"`
	HedgeCost     BriefMoneyCoverageRow  `json:"hedge_cost"`
	Proposals     BriefReadyProposalsRow `json:"proposals"`
	PolicyDrift   BriefPolicyDriftRow    `json:"policy_drift"`
	MonthlyPulse  *BriefMonthlyPulseRow  `json:"monthly_pulse,omitempty"`
}

// Brief narrative run roles. A run carries text plus at most one role, and a
// first-class served number, watch and act may appear only on clauses whose
// roles to their own register and must never re-derive them.
const (
	BriefRunRoleFigure = "figure"
	BriefRunRoleWatch  = "watch"
	BriefRunRoleAct    = "act"
)

// BriefRun is one typed span of composed narrative text. Runs are text, never
// markup. AccountSensitive marks account-derived monetary figures that the SPA
// must hide with the account-value visibility control. Topic, when set, names
// the closed brief-topic slug the run refers to, so a renderer can navigate to
// the surface that owns the row; it never carries symbols or account data.
type BriefRun struct {
	Text             string `json:"text"`
	Role             string `json:"role,omitempty"`
	AccountSensitive bool   `json:"account_sensitive,omitempty"`
	Topic            string `json:"topic,omitempty"`
}

// BriefParagraph is one composed paragraph as an ordered run sequence.
type BriefParagraph struct {
	Runs []BriefRun `json:"runs,omitempty"`
}

// BriefNarrative is the daemon-composed prose reading of the same two
// and Ready only, and a prose revision can never invalidate its identity.
type BriefNarrative struct {
	Lead   []BriefRun       `json:"lead,omitempty"`
	Review []BriefParagraph `json:"review,omitempty"`
	Ready  []BriefParagraph `json:"ready,omitempty"`
	Coda   []BriefRun       `json:"coda,omitempty"`
}

// BriefResult is the complete typed daily brief, composed as two process
type BriefResult struct {
	AsOf             time.Time          `json:"as_of"`
	BriefFingerprint string             `json:"brief_fingerprint"`
	Review           BriefReviewSection `json:"review"`
	Ready            BriefReadySection  `json:"ready"`
	Narrative        *BriefNarrative    `json:"narrative,omitempty"`
}

const (
	// MethodNudgesSnapshot returns the current redacted advisory snapshot.
	MethodNudgesSnapshot = "nudges.snapshot"
)

// Nudge candidate kinds, states, severities, destinations, and the blocking
// drawdown tier mirror the pure risk contract.
const (
	NudgeKindReconcileDue       = risk.NudgeKindReconcileDue
	NudgeKindReconcileException = risk.NudgeKindReconcileException
	NudgeKindShadowWouldBlock   = risk.NudgeKindShadowWouldBlock
	NudgeKindDrawdownLatched    = risk.NudgeKindDrawdownLatched
	NudgeKindPolicyDrift        = risk.NudgeKindPolicyDrift
	NudgeKindConfirmedFlow      = risk.NudgeKindConfirmedFlow
	NudgeKindMonthlyPulse       = risk.NudgeKindMonthlyPulse

	NudgeStateDueSoon  = risk.NudgeStateDueSoon
	NudgeStateOverdue  = risk.NudgeStateOverdue
	NudgeStateOpen     = risk.NudgeStateOpen
	NudgeStateObserved = risk.NudgeStateObserved
	NudgeStateDue      = risk.NudgeStateDue

	NudgeSeverityWatch = risk.NudgeSeverityWatch
	NudgeSeverityAct   = risk.NudgeSeverityAct

	NudgeDestinationMonitor = risk.NudgeDestinationMonitor
	NudgeDestinationAlerts  = risk.NudgeDestinationAlerts
	NudgeDestinationBrief   = risk.NudgeDestinationBrief

	NudgeDrawdownTierBlock = risk.CapitalTierBlock
)

// Nudge input and aggregate health values distinguish ready evidence from
// inactive, suppressed, stale, unavailable, and erroneous inputs.
const (
	NudgeInputStatusOK          = "ok"
	NudgeInputStatusInactive    = "inactive"
	NudgeInputStatusUnapproved  = "unapproved"
	NudgeInputStatusStale       = "stale"
	NudgeInputStatusUnavailable = "unavailable"
	NudgeInputStatusError       = "error"

	NudgeAggregateReady      = "ready"
	NudgeAggregateSuppressed = "suppressed"
	NudgeAggregateDegraded   = "degraded"
)

// Nudge source-health reasons are allowlisted tokens. Raw errors, paths,
// upstream fingerprints, and broker text do not belong on this contract.
const (
	NudgeHealthReasonNone                       = ""
	NudgeHealthReasonPolicyUnapproved           = "policy_unapproved"
	NudgeHealthReasonCadenceUnapproved          = "cadence_unapproved"
	NudgeHealthReasonEvidenceStale              = "evidence_stale"
	NudgeHealthReasonSourceUnavailable          = "source_unavailable"
	NudgeHealthReasonEvaluationError            = "evaluation_error"
	NudgeHealthReasonCoverageUnavailable        = "coverage_unavailable"
	NudgeHealthReasonProcessRemindersNotEnabled = "process_reminders_not_enabled"
	NudgeHealthReasonInvalid                    = "invalid_health"
)

// NudgesSnapshotParams is empty because nudges.snapshot is a
// gateway-independent, side-effect-free read.
type NudgesSnapshotParams struct{}

// MarshalJSON emits the canonical empty object and never JSON null.
func (NudgesSnapshotParams) MarshalJSON() ([]byte, error) {
	return []byte("{}"), nil
}

// UnmarshalJSON accepts only an exact empty object.
func (params *NudgesSnapshotParams) UnmarshalJSON(data []byte) error {
	type wire NudgesSnapshotParams
	var decoded wire
	if err := decodeExactNudgeJSONObject(data, nil, &decoded); err != nil {
		return err
	}
	*params = NudgesSnapshotParams(decoded)
	return nil
}

func decodeExactNudgeJSONObject(data []byte, allowedKeys []string, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("nudge JSON value must be an object")
	}

	allowed := make(map[string]struct{}, len(allowedKeys))
	for _, key := range allowedKeys {
		allowed[key] = struct{}{}
	}
	seen := make(map[string]struct{}, len(allowedKeys))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("nudge JSON object contains a non-string key")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("nudge JSON object contains unknown key %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("nudge JSON object contains duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("nudge JSON object key %q must not be null", key)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("nudge JSON object is not closed")
	}
	for _, key := range allowedKeys {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("nudge JSON object is missing key %q", key)
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing nudge JSON value")
		}
		return err
	}
	return json.Unmarshal(data, destination)
}

// NudgeCandidate is intentionally lockscreen-safe. Its title and body are
// daemon-authored enum templates; Fingerprint is an opaque semantic identity.
type NudgeCandidate struct {
	Fingerprint string    `json:"fingerprint"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	OccurredAt  time.Time `json:"occurred_at,omitzero"`
	DueAt       time.Time `json:"due_at,omitzero"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	Destination string    `json:"destination"`
}

// NudgeInputHealth reports one input's allowlisted status and reason. A zero
// AsOf is invalid and is normalized to error rather than treated as current.
type NudgeInputHealth struct {
	Status string    `json:"status"` // ok | unapproved | stale | unavailable | error
	Reason string    `json:"reason,omitempty"`
	AsOf   time.Time `json:"as_of,omitzero"`
}

// NudgeSourceHealth is separate from app polling/relay health. Fixed fields
// prevent generic notes, raw fingerprints, or unknown source names from
// widening the wire contract.
type NudgeSourceHealth struct {
	Aggregate      string           `json:"aggregate"` // ready | suppressed | degraded
	Policy         NudgeInputHealth `json:"policy"`
	Reconciliation NudgeInputHealth `json:"reconciliation"`
	Capital        NudgeInputHealth `json:"capital"`
	Pins           NudgeInputHealth `json:"pins"`
	Cadence        NudgeInputHealth `json:"cadence"`
	ConfirmedFlow  NudgeInputHealth `json:"confirmed_flow"`
}

type nudgeHealthSource uint8

const (
	nudgeHealthSourcePolicy nudgeHealthSource = iota
	nudgeHealthSourceReconciliation
	nudgeHealthSourceCapital
	nudgeHealthSourcePins
	nudgeHealthSourceCadence
	nudgeHealthSourceConfirmedFlow
)

// NormalizeNudgeSourceHealth is the mandatory wire boundary. It removes raw
// or incoherent status/reason values, preserves missing timestamps as missing,
// and derives Aggregate rather than trusting caller-provided state.
func NormalizeNudgeSourceHealth(health NudgeSourceHealth, candidateCount int) NudgeSourceHealth {
	health.Policy = normalizeNudgeInputHealth(health.Policy, nudgeHealthSourcePolicy)
	health.Reconciliation = normalizeNudgeInputHealth(health.Reconciliation, nudgeHealthSourceReconciliation)
	health.Capital = normalizeNudgeInputHealth(health.Capital, nudgeHealthSourceCapital)
	health.Pins = normalizeNudgeInputHealth(health.Pins, nudgeHealthSourcePins)
	health.Cadence = normalizeNudgeInputHealth(health.Cadence, nudgeHealthSourceCadence)
	health.ConfirmedFlow = normalizeNudgeInputHealth(health.ConfirmedFlow, nudgeHealthSourceConfirmedFlow)
	health.Aggregate = aggregateNormalizedNudgeSourceHealth(health, candidateCount)
	return health
}

func normalizeNudgeInputHealth(health NudgeInputHealth, source nudgeHealthSource) NudgeInputHealth {
	validPair := false
	if !health.AsOf.IsZero() {
		switch health.Status {
		case NudgeInputStatusOK:
			validPair = health.Reason == NudgeHealthReasonNone
		case NudgeInputStatusInactive:
			validPair = (source == nudgeHealthSourcePolicy || source == nudgeHealthSourceCadence || source == nudgeHealthSourceConfirmedFlow) &&
				health.Reason == NudgeHealthReasonProcessRemindersNotEnabled
		case NudgeInputStatusUnapproved:
			validPair = health.Reason == NudgeHealthReasonPolicyUnapproved ||
				health.Reason == NudgeHealthReasonCadenceUnapproved
		case NudgeInputStatusStale:
			validPair = health.Reason == NudgeHealthReasonEvidenceStale
		case NudgeInputStatusUnavailable:
			validPair = health.Reason == NudgeHealthReasonSourceUnavailable ||
				health.Reason == NudgeHealthReasonCoverageUnavailable
		case NudgeInputStatusError:
			validPair = health.Reason == NudgeHealthReasonEvaluationError || health.Reason == NudgeHealthReasonInvalid
		}
	}
	if !validPair {
		health.Status = NudgeInputStatusError
		health.Reason = NudgeHealthReasonInvalid
	}
	return health
}

func aggregateNormalizedNudgeSourceHealth(health NudgeSourceHealth, candidateCount int) string {
	inputs := [...]NudgeInputHealth{
		health.Policy,
		health.Reconciliation,
		health.Capital,
		health.Pins,
		health.Cadence,
		health.ConfirmedFlow,
	}
	allReady := true
	for _, input := range inputs {
		if input.Status != NudgeInputStatusOK &&
			!(input.Status == NudgeInputStatusInactive && input.Reason == NudgeHealthReasonProcessRemindersNotEnabled) {
			allReady = false
			break
		}
	}
	if allReady {
		return NudgeAggregateReady
	}
	if candidateCount <= 0 {
		return NudgeAggregateSuppressed
	}
	return NudgeAggregateDegraded
}

type nudgeSourceHealthWire NudgeSourceHealth

// MarshalJSON prevents standalone source-health values from carrying a false
// ready aggregate. Without result candidate context, partial health is
// conservatively suppressed.
func (health NudgeSourceHealth) MarshalJSON() ([]byte, error) {
	normalized := NormalizeNudgeSourceHealth(health, 0)
	return json.Marshal(nudgeSourceHealthWire(normalized))
}

// NudgesSnapshotResult is the daemon-authored advisory nudge snapshot. An
// empty Candidates slice is reassuring only when SourceHealth is ready.
type NudgesSnapshotResult struct {
	AsOf                  time.Time                   `json:"as_of"`
	Candidates            []NudgeCandidate            `json:"candidates"`
	SourceHealth          NudgeSourceHealth           `json:"source_health"`
	Reconciliation        *ReconAutomationStatus      `json:"reconciliation,omitempty"`
	ConfirmedFlowCoverage *NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
	Context               *NudgeSnapshotContext       `json:"context,omitempty"`
}

// NudgeSnapshotContext is visible snapshot detail, never candidate or push
// copy. Its concrete summaries deliberately admit no arbitrary display text.
type NudgeSnapshotContext struct {
	Shadow   *NudgeShadowSummary   `json:"shadow,omitempty"`
	Drawdown *NudgeDrawdownSummary `json:"drawdown,omitempty"`
}

// NudgeShadowSummary reports the redacted number of shadow findings.
type NudgeShadowSummary struct {
	Count int `json:"count"`
}

// NudgeDrawdownSummary reports the active tier and optional consumption. A nil
// percentage means the value is unavailable, not zero.
type NudgeDrawdownSummary struct {
	Tier        string   `json:"tier"`
	ConsumedPct *float64 `json:"consumed_pct"`
}

// NudgeConfirmedFlowCoverage discloses the redacted observation boundary.
type NudgeConfirmedFlowCoverage struct {
	CoverageFrom time.Time `json:"coverage_from"`
}

// MarshalJSON validates and canonicalizes health and candidates before
// encoding the snapshot.
func (result NudgesSnapshotResult) MarshalJSON() ([]byte, error) {
	normalizedHealth, candidates, err := validateNudgeSnapshot(result)
	if err != nil {
		return nil, err
	}
	wire := struct {
		AsOf                  time.Time                   `json:"as_of"`
		Candidates            []NudgeCandidate            `json:"candidates"`
		SourceHealth          nudgeSourceHealthWire       `json:"source_health"`
		Reconciliation        *ReconAutomationStatus      `json:"reconciliation,omitempty"`
		ConfirmedFlowCoverage *NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
		Context               *NudgeSnapshotContext       `json:"context,omitempty"`
	}{
		AsOf:                  result.AsOf,
		Candidates:            candidates,
		SourceHealth:          nudgeSourceHealthWire(normalizedHealth),
		Reconciliation:        result.Reconciliation,
		ConfirmedFlowCoverage: result.ConfirmedFlowCoverage,
		Context:               result.Context,
	}
	return json.Marshal(wire)
}

// IsCleanEmpty reports whether the snapshot is valid, fully covered, and has
// neither candidates nor contextual findings.
func (result NudgesSnapshotResult) IsCleanEmpty() bool {
	normalized, candidates, err := validateNudgeSnapshot(result)
	if err != nil {
		return false
	}
	return len(candidates) == 0 && result.Context == nil && normalized.Aggregate == NudgeAggregateReady
}

func validateNudgeSnapshot(result NudgesSnapshotResult) (NudgeSourceHealth, []NudgeCandidate, error) {
	normalizedHealth := NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates))
	if result.Reconciliation != nil {
		if err := ValidateReconAutomationStatus(*result.Reconciliation); err != nil {
			return NudgeSourceHealth{}, nil, err
		}
	}
	if err := validateNudgeSnapshotConfirmedFlowCoherence(result.AsOf, result.ConfirmedFlowCoverage, normalizedHealth.ConfirmedFlow); err != nil {
		return NudgeSourceHealth{}, nil, err
	}
	if err := validateNudgeSnapshotSourceHealthTimestamps(result.AsOf, normalizedHealth); err != nil {
		return NudgeSourceHealth{}, nil, err
	}
	candidates := make([]NudgeCandidate, len(result.Candidates))
	for i, candidate := range result.Candidates {
		canonical, err := canonicalizeRPCNudgeCandidate(candidate)
		if err != nil {
			return NudgeSourceHealth{}, nil, fmt.Errorf("invalid nudge candidate at index %d: %w", i, err)
		}
		if err := validateNudgeCandidateSnapshotTime(result.AsOf, canonical); err != nil {
			return NudgeSourceHealth{}, nil, fmt.Errorf("invalid nudge candidate at index %d: %w", i, err)
		}
		candidates[i] = canonical
	}
	if err := validateNudgeSnapshotContext(result.Context, candidates); err != nil {
		return NudgeSourceHealth{}, nil, err
	}
	return normalizedHealth, candidates, nil
}

func validateNudgeCandidateSnapshotTime(asOf time.Time, candidate NudgeCandidate) error {
	if candidate.OccurredAt.After(asOf) {
		return errors.New("nudge candidate occurrence time is after snapshot as_of")
	}
	switch {
	case candidate.Kind == NudgeKindReconcileDue && candidate.State == NudgeStateDueSoon:
		if candidate.DueAt.Before(asOf) {
			return errors.New("reconcile due-soon deadline is before snapshot as_of")
		}
	case candidate.Kind == NudgeKindReconcileDue && candidate.State == NudgeStateOverdue:
		if candidate.DueAt.After(asOf) {
			return errors.New("reconcile overdue deadline is after snapshot as_of")
		}
	case candidate.Kind == NudgeKindMonthlyPulse && candidate.State == NudgeStateOpen:
		if candidate.DueAt.After(asOf) {
			return errors.New("monthly pulse deadline is after snapshot as_of")
		}
	}
	return nil
}

func validateNudgeSnapshotContext(context *NudgeSnapshotContext, candidates []NudgeCandidate) error {
	if context != nil && context.Shadow == nil && context.Drawdown == nil {
		return errors.New("nudge snapshot context is empty")
	}
	shadowCandidates := 0
	drawdownCandidates := 0
	for _, candidate := range candidates {
		switch {
		case candidate.Kind == NudgeKindShadowWouldBlock && candidate.State == NudgeStateObserved:
			shadowCandidates++
		case candidate.Kind == NudgeKindDrawdownLatched && candidate.State == NudgeStateOpen:
			drawdownCandidates++
		}
	}
	if shadowCandidates > 1 {
		return errors.New("nudge snapshot has duplicate shadow context candidates")
	}
	if drawdownCandidates > 1 {
		return errors.New("nudge snapshot has duplicate drawdown context candidates")
	}

	var shadow *NudgeShadowSummary
	var drawdown *NudgeDrawdownSummary
	if context != nil {
		shadow = context.Shadow
		drawdown = context.Drawdown
	}
	if (shadowCandidates == 1) != (shadow != nil) {
		return errors.New("nudge snapshot shadow summary and candidate are incoherent")
	}
	if shadow != nil && shadow.Count < 1 {
		return errors.New("nudge snapshot shadow count must be positive")
	}
	if (drawdownCandidates == 1) != (drawdown != nil) {
		return errors.New("nudge snapshot drawdown summary and candidate are incoherent")
	}
	if drawdown != nil {
		if drawdown.Tier != NudgeDrawdownTierBlock {
			return errors.New("nudge snapshot drawdown tier must be block")
		}
		if drawdown.ConsumedPct != nil && (math.IsNaN(*drawdown.ConsumedPct) || math.IsInf(*drawdown.ConsumedPct, 0) || *drawdown.ConsumedPct < 0) {
			return errors.New("nudge snapshot drawdown consumed_pct must be finite and non-negative")
		}
	}
	return nil
}

func validateNudgeSnapshotSourceHealthTimestamps(asOf time.Time, health NudgeSourceHealth) error {
	inputs := [...]struct {
		name string
		asOf time.Time
	}{
		{name: "policy", asOf: health.Policy.AsOf},
		{name: "reconciliation", asOf: health.Reconciliation.AsOf},
		{name: "capital", asOf: health.Capital.AsOf},
		{name: "pins", asOf: health.Pins.AsOf},
		{name: "cadence", asOf: health.Cadence.AsOf},
		{name: "confirmed_flow", asOf: health.ConfirmedFlow.AsOf},
	}
	for _, input := range inputs {
		if !input.asOf.IsZero() && input.asOf.After(asOf) {
			return fmt.Errorf("nudge snapshot %s source health is after as_of", input.name)
		}
	}
	return nil
}

func validateNudgeSnapshotConfirmedFlowCoherence(
	asOf time.Time,
	coverage *NudgeConfirmedFlowCoverage,
	confirmedFlowHealth NudgeInputHealth,
) error {
	if asOf.IsZero() {
		return errors.New("nudge snapshot is missing as_of")
	}
	if coverage == nil {
		if confirmedFlowHealth.Status == NudgeInputStatusOK {
			return errors.New("nudge snapshot has ready confirmed-flow health without coverage")
		}
		return nil
	}
	if coverage.CoverageFrom.IsZero() {
		return errors.New("nudge snapshot confirmed-flow coverage is missing coverage_from")
	}
	if coverage.CoverageFrom.After(asOf) {
		return errors.New("nudge snapshot confirmed-flow coverage is after as_of")
	}
	if !confirmedFlowHealth.AsOf.IsZero() && coverage.CoverageFrom.After(confirmedFlowHealth.AsOf) {
		return errors.New("nudge snapshot confirmed-flow coverage is newer than source health")
	}
	return nil
}

func canonicalizeRPCNudgeCandidate(candidate NudgeCandidate) (NudgeCandidate, error) {
	canonical, err := risk.CanonicalizeNudgeCandidate(risk.NudgeCandidate{
		Fingerprint: candidate.Fingerprint,
		Kind:        candidate.Kind,
		State:       candidate.State,
		Severity:    candidate.Severity,
		Title:       candidate.Title,
		Body:        candidate.Body,
		OccurredAt:  candidate.OccurredAt,
		DueAt:       candidate.DueAt,
		ExpiresAt:   candidate.ExpiresAt,
		Destination: candidate.Destination,
	})
	if err != nil {
		return NudgeCandidate{}, err
	}
	return NudgeCandidate{
		Fingerprint: canonical.Fingerprint,
		Kind:        canonical.Kind,
		State:       canonical.State,
		Severity:    canonical.Severity,
		Title:       canonical.Title,
		Body:        canonical.Body,
		OccurredAt:  canonical.OccurredAt,
		DueAt:       canonical.DueAt,
		ExpiresAt:   canonical.ExpiresAt,
		Destination: canonical.Destination,
	}, nil
}

// Post-trade reconciliation contract (internal-docs/design/post-trade-truth.md).
// broker writes, submit eligibility, or the order path.

const (
	// MethodReconSnapshot regenerates and returns the reconciliation
	MethodReconSnapshot = "recon.snapshot"
	// MethodReconCheck requests one broker-report check and returns an
	// immediate, typed receipt. It is broker-read-only and never signs off,
	// dismisses an exception, or changes trading controls.
	MethodReconCheck = "recon.check"
	// MethodReconStatus returns only the redacted daily automation state. It
	// deliberately omits report rows, amounts, account data, and identifiers.
	MethodReconStatus = "recon.status"
	// MethodReconBacktest builds the full-window backtest report: every
	// Measurement only — it changes no matching, sign-off, or enforcement.
	MethodReconBacktest = "recon.backtest"
	// MethodReconDismiss records a human resolution for one exception
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
const ReconBaseline = "baseline"

// ReconConfirmed is a normal v3 statement-authoritative flow. It is
// target because declarations are optional after the authority flip.
const ReconConfirmed = "confirmed"

// ReconSnapshotParams tunes one snapshot call.
type ReconSnapshotParams struct {
	// Refresh kicks one background statement fetch (single-flight); the
	Refresh bool `json:"refresh,omitempty"`
}

// ReconException is the shared row shape for an exception or disclosed flow.
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
	PreGenesis    bool   `json:"pre_genesis,omitempty"`
	Note          string `json:"note,omitempty"`
	Dismissed     bool   `json:"dismissed,omitempty"`
	DismissReason string `json:"dismiss_reason,omitempty"`
}

// ReconEquityCheck compares the statement equity series with the runtime
// is computed only from a same-day pair; when SameDay is false,
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
type ReconBacktestFlow struct {
	LineID      string    `json:"line_id"`
	Type        string    `json:"type,omitempty"`
	Description string    `json:"description,omitempty"`
	ValueDate   time.Time `json:"value_date,omitzero"`
	AmountBase  *float64  `json:"amount_base,omitempty"`
	PreGenesis  bool      `json:"pre_genesis,omitempty"`
	// Status is "matched", ReconBaseline, or the recon exception category
	Status    string `json:"status"`
	Dismissed bool   `json:"dismissed,omitempty"`
}

// ReconBacktestCrossing compares the first replayed crossing of one capital
type ReconBacktestCrossing struct {
	Tier                string    `json:"tier"` // warn | block
	ReplayedAt          time.Time `json:"replayed_at,omitzero"`
	ReplayedConsumedPct float64   `json:"replayed_consumed_pct"`
	RuntimeAt           time.Time `json:"runtime_at,omitzero"`
}

// ReconBacktestReplay is the capital-ladder replay over broker statement EOD
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
type ReconAutomationStatus struct {
	Report     ReconFetchStatus      `json:"report"`
	Evaluation ReconEvaluationStatus `json:"evaluation"`
}

// ReconCheckResult is the immediate receipt returned by recon.check. Status
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

// Risk-constitution contract (internal-docs/design/risk-policy.md). policy.snapshot
// is read-only; the four write methods are governance acts, not broker
// of them can touch submit eligibility, blockers, freeze, pins, tokens, or
// any gated broker-write path.

const (
	// MethodRiskPolicySnapshot returns the effective constitution, capital
	MethodRiskPolicySnapshot = "policy.snapshot"
	// MethodRiskPolicyCapitalEvent declares a capital fact: deposit,
	// withdrawal, or reconcile attestation. Human-only.
	MethodRiskPolicyCapitalEvent = "policy.capital_event"
	// MethodRiskPolicyOverride grants a one-shot, expiring, single-control
	MethodRiskPolicyOverride = "policy.override"
	// MethodRiskPolicyResetDrawdown clears a latched drawdown block and
	// re-bases the adjusted peak. Human-only.
	MethodRiskPolicyResetDrawdown = "policy.reset_drawdown"
	// MethodRiskPolicyCorrectPeak lowers a corrupted adjusted peak to an
	// evidence-anchored value without touching the drawdown latch.
	// Corrections may only lower the peak; higher peaks are what the
	// observation path is for. Human-only.
	MethodRiskPolicyCorrectPeak = "policy.correct_peak"
)

// RiskConstitutionFingerprintVersion labels the constitution fingerprint.
// so the two identities can never be conflated in journals.
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
	AmountBase float64 `json:"amount_base,omitempty"`
	// EffectiveAt is when the flow hit the account; zero means now. A
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	Note        string    `json:"note,omitempty"`
	// Report is required for type reconcile since phase 3a: the recon
	Report string `json:"report,omitempty"`
	// Origin is the write-origin claim; the daemon rejects non-human
	Origin string `json:"origin,omitempty"`
}

// OverrideParams grants a one-shot exception against one named control.
type OverrideParams struct {
	// Control is the constitution key being excepted (e.g.
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
type ResetDrawdownParams struct {
	Reason string `json:"reason"`
	Origin string `json:"origin,omitempty"`
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
	Enforcement string `json:"enforcement"`
	// BoundAccount is the broker account this capital document adopted at its
	// first live observation. The drawdown ladder follows this account, not
	// the session pin: a session connected to a different account or mode
	// drawdown computed from one account's equity against another account's
	BoundAccount             string    `json:"bound_account,omitempty"`
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
	// LatchProvisional marks a latch broker statements have not yet decided:
	// coverage reaching the latch day either dissolves it (a confirmed
	// external flow explains the drop) or promotes it to durable.
	LatchProvisional bool `json:"latch_provisional,omitempty"`
	// LatchConsumedPct is the consumed share at the moment the latch engaged.
	LatchConsumedPct      *float64  `json:"latch_consumed_pct,omitempty"`
	LastReconciledAt      time.Time `json:"last_reconciled_at,omitzero"`
	LastReconcileReportID string    `json:"last_reconcile_report_id,omitempty"`
	LastReconcileSource   string    `json:"last_reconcile_source,omitempty"` // human | automatic
	ReconcileStale        bool      `json:"reconcile_stale,omitempty"`
	Reasons               []string  `json:"reasons,omitempty"`
	BaseCurrency          string    `json:"base_currency,omitempty"`
}

// PolicyPinStatus compares one constitution inventory pin with the live
type PolicyPinStatus struct {
	Policy        string `json:"policy"` // rulebook | protection | stress
	PinnedID      string `json:"pinned_id,omitempty"`
	PinnedVersion string `json:"pinned_version,omitempty"`
	LiveID        string `json:"live_id,omitempty"`
	LiveVersion   string `json:"live_version,omitempty"`
	// Status is match | drift | unpinned | unavailable.
	Status string `json:"status"`
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
	Unapproved []string `json:"unapproved,omitempty"`

	Capital   CapitalStateReport       `json:"capital"`
	Limits    []risk.ConstitutionLimit `json:"limits,omitempty"`
	Overrides []OverrideRecord         `json:"overrides,omitempty"`
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

// MethodRulesSnapshot returns the daily trading-rulebook checklist evaluated
// submit eligibility or any gated broker-write path.
const MethodRulesSnapshot = "rules.snapshot"

// RulebookPolicyFingerprintVersion labels the advisory rulebook policy
const RulebookPolicyFingerprintVersion = "rulebook-fp-v3"

// RulesSnapshotParams selects optional evaluation scope. Zero value means the
// full 14-rule checklist over all held names.
type RulesSnapshotParams struct {
	// Symbol narrows per-name offender lists to one underlying; portfolio
	Symbol string `json:"symbol,omitempty"`
}

// EarningsInfo is the per-name earnings context the rules consumed, so
type EarningsInfo struct {
	Symbol string `json:"symbol"`
	// Date is the next earnings date in ET (YYYY-MM-DD), empty when unknown.
	Date string `json:"date,omitempty"`
	// TimeOfDay is "amc", "bmo", or "" when unspecified.
	TimeOfDay string `json:"time_of_day,omitempty"`
	// Estimated marks provider-flagged estimated (unconfirmed) dates.
	Estimated bool `json:"estimated,omitempty"`
	// Source is fetched | override | broker_identity | security_type |
	// Terminal carries the exact-contract evidence when no future issuer earnings
	Source string `json:"source"`
	// SecurityType names the held security type when Source is security_type —
	// the canonical spelling of a type that has no issuer earnings at all. It is
	// the whole authority behind that classification, so consumers re-derive the
	// exemption from this field rather than trusting Source alone.
	SecurityType string `json:"security_type,omitempty"`
	// Status is date or a typed unresolved outcome. Conflicting provider
	// dates never populate Date.
	Status string `json:"status,omitempty"`
	// Reason is a stable aggregate explanation such as single_source or
	// conflicting_sources; it never contains provider free text.
	Reason string `json:"reason,omitempty"`
	// ObservedAt is when the fetched value was last confirmed from the
	// provider; zero for overrides and unknowns.
	ObservedAt time.Time              `json:"observed_at,omitzero"`
	Stale      bool                   `json:"stale,omitempty"`
	Providers  []EarningsProviderInfo `json:"providers,omitempty"`
	Identity   *EarningsIdentityInfo  `json:"identity,omitempty"`
	Terminal   *EarningsTerminalInfo  `json:"terminal,omitempty"`
}

// Earnings statuses are the closed aggregate/provider outcome vocabulary.
const (
	EarningsStatusDate                    = "date"
	EarningsStatusNoDatePublished         = "no_date_published"
	EarningsStatusUnsupportedSecurity     = "unsupported_security"
	EarningsStatusFormatChange            = "format_change"
	EarningsStatusTransportFailure        = "transport_failure"
	EarningsStatusConflictingSources      = "conflicting_sources"
	EarningsStatusNotApplicable           = "not_applicable"
	EarningsStatusTerminalNonReporting    = "terminal_non_reporting"
	EarningsStatusTerminalEvidenceExpired = "terminal_evidence_expired"
)

// EarningsIdentityInfo discloses the independent broker applicability read
// without exposing the held contract ID or raw broker StockType.
type EarningsIdentityInfo struct {
	Outcome              string         `json:"outcome"`
	NotApplicable        bool           `json:"not_applicable,omitempty"`
	AttemptedAt          time.Time      `json:"attempted_at,omitzero"`
	ProofObservedAt      time.Time      `json:"proof_observed_at,omitzero"`
	ProofOutcome         string         `json:"proof_outcome,omitempty"`
	AuthorityRevision    int64          `json:"authority_revision,omitempty"`
	AuthorityFingerprint string         `json:"authority_fingerprint,omitempty"`
	ObservationID        string         `json:"observation_id,omitempty"`
	AuthorityBinding     string         `json:"authority_binding,omitempty"`
	NextAttempt          *time.Time     `json:"next_attempt,omitempty"`
	LastFailure          *SourceFailure `json:"last_failure,omitempty"`
}

// BuildEarningsIdentityAuthorityBinding binds one public earnings projection
// to the exact symbol and opaque proof receipt it describes. The digest exposes
// neither the raw database receipt ID nor broker identity fields; consumers can
// recompute it to reject cross-symbol or cross-proof substitution.
func BuildEarningsIdentityAuthorityBinding(symbol string, identity EarningsIdentityInfo) string {
	if strings.TrimSpace(symbol) == "" || strings.TrimSpace(symbol) != symbol ||
		identity.AuthorityRevision <= 0 || strings.TrimSpace(identity.AuthorityFingerprint) == "" ||
		identity.ProofObservedAt.IsZero() || identity.ProofOutcome != EarningsStatusNotApplicable ||
		strings.TrimSpace(identity.ObservationID) == "" {
		return ""
	}
	payload, err := json.Marshal(struct {
		Kind                 string    `json:"kind"`
		Version              int       `json:"version"`
		Symbol               string    `json:"symbol"`
		AuthorityRevision    int64     `json:"authority_revision"`
		AuthorityFingerprint string    `json:"authority_fingerprint"`
		ProofObservedAt      time.Time `json:"proof_observed_at"`
		ProofOutcome         string    `json:"proof_outcome"`
		ObservationID        string    `json:"observation_id"`
	}{
		Kind: "earnings_identity_authority_binding", Version: 1,
		Symbol: symbol, AuthorityRevision: identity.AuthorityRevision,
		AuthorityFingerprint: identity.AuthorityFingerprint, ProofObservedAt: identity.ProofObservedAt.UTC(),
		ProofOutcome: identity.ProofOutcome, ObservationID: identity.ObservationID,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EarningsTerminalInfo is compiled, reviewed evidence that one exact broker
// must fall back to ordinary provider resolution. RevalidateAfter is a hard
// fail-closed boundary; expired evidence becomes unknown until the catalog is
// reviewed and updated. AuthorityReviewedAt is the monotonic catalog watermark
type EarningsTerminalInfo struct {
	ContractConID        int                         `json:"contract_con_id"`
	Issuer               string                      `json:"issuer"`
	CIK                  string                      `json:"cik,omitempty"`
	Classification       string                      `json:"classification"`
	EffectiveDate        string                      `json:"effective_date"`
	VerifiedAt           time.Time                   `json:"verified_at"`
	RevalidateAfter      time.Time                   `json:"revalidate_after"`
	AuthorityRevision    int64                       `json:"authority_revision"`
	AuthorityReviewedAt  time.Time                   `json:"authority_reviewed_at"`
	AuthorityFingerprint string                      `json:"authority_fingerprint"`
	AuthorityBinding     string                      `json:"authority_binding,omitempty"`
	Evidence             []EarningsEvidenceReference `json:"evidence"`
}

// BuildEarningsTerminalAuthorityBinding binds one public terminal projection
// to the exact symbol and contract authority it describes. The digest excludes
func BuildEarningsTerminalAuthorityBinding(symbol string, terminal EarningsTerminalInfo) string {
	if strings.TrimSpace(symbol) == "" || strings.TrimSpace(symbol) != symbol ||
		terminal.ContractConID <= 0 || terminal.AuthorityRevision <= 0 ||
		strings.TrimSpace(terminal.AuthorityFingerprint) == "" ||
		strings.TrimSpace(terminal.EffectiveDate) == "" ||
		strings.TrimSpace(terminal.Classification) == "" || terminal.VerifiedAt.IsZero() ||
		terminal.AuthorityReviewedAt.IsZero() || terminal.RevalidateAfter.IsZero() {
		return ""
	}
	payload, err := json.Marshal(struct {
		Kind                 string    `json:"kind"`
		Version              int       `json:"version"`
		Symbol               string    `json:"symbol"`
		ContractConID        int       `json:"contract_con_id"`
		AuthorityRevision    int64     `json:"authority_revision"`
		AuthorityFingerprint string    `json:"authority_fingerprint"`
		EffectiveDate        string    `json:"effective_date"`
		VerifiedAt           time.Time `json:"verified_at"`
		AuthorityReviewedAt  time.Time `json:"authority_reviewed_at"`
		RevalidateAfter      time.Time `json:"revalidate_after"`
		Classification       string    `json:"classification"`
	}{
		Kind: "earnings_terminal_authority_binding", Version: 1,
		Symbol: symbol, ContractConID: terminal.ContractConID,
		AuthorityRevision: terminal.AuthorityRevision, AuthorityFingerprint: terminal.AuthorityFingerprint,
		EffectiveDate: terminal.EffectiveDate, VerifiedAt: terminal.VerifiedAt.UTC(),
		AuthorityReviewedAt: terminal.AuthorityReviewedAt.UTC(), RevalidateAfter: terminal.RevalidateAfter.UTC(),
		Classification: terminal.Classification,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EarningsEvidenceReference is one allowlisted primary-source document used
// by the terminal classification. These strings are compiled authority, not
type EarningsEvidenceReference struct {
	Authority string `json:"authority"`
	Document  string `json:"document"`
	URL       string `json:"url"`
}

// EarningsProviderInfo is one provider's latest typed outcome. A transport
// failure may coexist with a retained LastGoodDate, but Date is populated only
// when the latest attempt itself returned a usable date.
type EarningsProviderInfo struct {
	Provider     string         `json:"provider"`
	Status       string         `json:"status"`
	Date         string         `json:"date,omitempty"`
	TimeOfDay    string         `json:"time_of_day,omitempty"`
	Estimated    bool           `json:"estimated,omitempty"`
	ObservedAt   time.Time      `json:"observed_at,omitzero"`
	AttemptedAt  time.Time      `json:"attempted_at,omitzero"`
	NextAttempt  *time.Time     `json:"next_attempt,omitempty"`
	LastGoodDate string         `json:"last_good_date,omitempty"`
	LastFailure  *SourceFailure `json:"last_failure,omitempty"`
}

// RulesResult is the rules.snapshot payload. Rows come from the pure
type RulesResult struct {
	AsOf time.Time `json:"as_of"`
	// Enabled mirrors features.rulebook.enabled; when false Rules is empty
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"` // ok | degraded | disabled
	// Rules holds all rows in rulebook order; Ranked holds indexes into
	// Rules sorted hardest-first so renderers agree on ordering without
	// re-deriving it.
	Rules  []risk.RuleRow `json:"rules"`
	Ranked []int          `json:"ranked,omitempty"`
	// BreachCounts summarizes row counts by status for compact surfaces.
	BreachCounts map[string]int `json:"breach_counts,omitempty"`
	// InputHealth is the result-level gate: when positions or account are
	// pending/stale/absent every portfolio-dependent row is unknown, never
	// pass. Canonical snapshots carry exactly one entry for account,
	// positions, earnings, regime_stage, and tape.
	InputHealth []SourceHealth `json:"input_health,omitempty"`
	Earnings    []EarningsInfo `json:"earnings,omitempty"`
	// Policy provenance, mirroring proposals/Stress.
	PolicyID          string       `json:"policy_id"`
	PolicyVersion     int          `json:"policy_version"`
	PolicyFingerprint *Fingerprint `json:"policy_fingerprint,omitempty"`
	// BaseCurrency scopes every *_base impact figure.
	BaseCurrency string `json:"base_currency,omitempty"`
}
