package rpc

import (
	"slices"
	"strings"
	"time"
)

// Lifecycle stages, timing classes, and fingerprint stability are the stable
// vocabulary used by regime and Stress monitor projections.
const (
	LifecycleQuiet           = "quiet"
	LifecycleEarlyWarning    = "early_warning"
	LifecycleConfirmedStress = "confirmed_stress"
	LifecyclePanic           = "panic"
	LifecycleStabilization   = "stabilization"
	LifecycleOpportunity     = "opportunity"
	LifecycleDataQuality     = "data_quality"

	LifecycleTimingForwardWarning = "forward_warning"
	LifecycleTimingContemporary   = "contemporaneous"
	LifecycleTimingRecovery       = "recovery"
	LifecycleTimingDataQuality    = "data_quality"

	FingerprintStabilitySemanticBuckets = "semantic_buckets_only"
)

// Source-health statuses distinguish complete observations from partial,
// stale, unknown, and degraded evidence.
const (
	SourceStatusOK       = "ok"
	SourceStatusPartial  = "partial"
	SourceStatusStale    = "stale"
	SourceStatusUnknown  = "unknown"
	SourceStatusDegraded = "degraded"
)

// Regime tones are presentation classifications derived from lifecycle state.
const (
	RegimeToneNormal      = "normal"
	RegimeToneWatch       = "watch"
	RegimeToneStress      = "stress"
	RegimeToneRiskOff     = "risk_off"
	RegimeToneDataQuality = "data_quality"
)

// LifecycleState is the stable monitor/orchestration surface for regime and
// Stress payloads. Stage is intentionally a small state machine, while
// Evidence preserves weak/unconfirmed inputs without letting them dominate the
// trigger.
type LifecycleState struct {
	Stage       string              `json:"stage,omitempty"`
	Scope       string              `json:"scope,omitempty"`
	Severity    string              `json:"severity,omitempty"`
	Readiness   string              `json:"readiness,omitempty"`
	Timing      string              `json:"timing,omitempty"`
	Confidence  string              `json:"confidence,omitempty"`
	Evidence    []LifecycleEvidence `json:"evidence,omitempty"`
	ConfirmedBy []string            `json:"confirmed_by,omitempty"`
	Unconfirmed []string            `json:"unconfirmed,omitempty"`
	Suppressed  []string            `json:"suppressed,omitempty"`
	RejectedBy  []string            `json:"rejected_by,omitempty"`
	// Governors discloses every policy downgrade applied after stage
	// selection (provenance gate, evidence-keyed quality cap). Nothing is
	// silently weakened: when severity reads lower than the stage suggests,
	// this is where the reason lives.
	Governors    []GovernorAction `json:"governors,omitempty"`
	Fingerprint  Fingerprint      `json:"fingerprint,omitzero"`
	NotExecution string           `json:"not_execution,omitempty"`
}

// GovernorAction is one disclosed policy downgrade. Reasons are stable
// tokens: "pending_backtest_no_tape_cosign" (heuristic threshold sets
// confirmed the stage but no fresh tape co-signature was present) and
// "confirming_cluster_quality" (a confirming cluster's data quality is
// stale/partial/degraded).
type GovernorAction struct {
	Action   string   `json:"action"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Clusters []string `json:"clusters,omitempty"`
}

// LifecycleEvidence is one classified input to a lifecycle decision. Confirmed
// distinguishes qualifying evidence from context retained for explanation.
type LifecycleEvidence struct {
	Source    string `json:"source,omitempty"`
	Signal    string `json:"signal,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Timing    string `json:"timing,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Confirmed bool   `json:"confirmed,omitempty"`
}

// SourceHealth is the source-level freshness and confidence contract for
// scheduled monitors. FingerprintStability says why downstream dedupe should
// key off Fingerprint rather than wall-clock AsOf churn.
type SourceHealth struct {
	Source               string       `json:"source"`
	Status               string       `json:"status"`
	AsOf                 time.Time    `json:"as_of,omitzero"`
	AgeSeconds           int64        `json:"age_seconds,omitempty"`
	MaxAgeSeconds        int64        `json:"max_age_seconds,omitempty"`
	Confidence           string       `json:"confidence,omitempty"`
	Fingerprint          *Fingerprint `json:"fingerprint,omitempty"`
	FingerprintStability string       `json:"fingerprint_stability,omitempty"`
	// RefreshState separates source scheduling from evidence quality. A
	// source can be not_due with no current negative, or stale because a due
	// fetch failed and is waiting for its bounded retry.
	RefreshState string     `json:"refresh_state,omitempty"`
	NextAttempt  *time.Time `json:"next_attempt,omitempty"`
	// LastFailure is a redacted, allowlisted machine-readable failure. Raw
	// transport, provider, and parser text must never cross the RPC boundary.
	LastFailure *SourceFailure `json:"last_failure,omitempty"`
	Notes       []string       `json:"notes,omitempty"`
}

// SourceFailure identifies where and how the most recent source attempt
// failed without carrying untrusted upstream text. FailedAt and Retryable are
// scheduling context; Code and Stage are stable semantic values suitable for
// persistence, rendering, and fingerprints.
type SourceFailure struct {
	Code      string    `json:"code"`
	Stage     string    `json:"stage"`
	FailedAt  time.Time `json:"failed_at"`
	Retryable bool      `json:"retryable"`
}

// Source-failure codes and stages are the allowlisted cross-surface vocabulary
// for redacted transport, parser, provider, and authority failures.
const (
	SourceFailureTimeout                = "timeout"
	SourceFailureDNSFailed              = "dns_failed"
	SourceFailureConnectionRefused      = "connection_refused"
	SourceFailureTransportFailed        = "transport_failed"
	SourceFailureProtocolRejected       = "protocol_rejected"
	SourceFailureAuthenticationRejected = "authentication_rejected"
	SourceFailureInvalidPayload         = "invalid_payload"
	SourceFailureAuthorityWriteFailed   = "authority_write_failed"
	SourceFailureNotEntitled            = "not_entitled"
	SourceFailureGatewayUnavailable     = "gateway_unavailable"
	SourceFailureNoData                 = "no_data"
	SourceFailurePacing                 = "pacing"
	SourceFailureContractUnavailable    = "contract_unavailable"

	SourceFailureStageFTPControlConnect    = "ftp_control_connect"
	SourceFailureStageFTPGreeting          = "ftp_greeting"
	SourceFailureStageFTPAuthenticate      = "ftp_authenticate"
	SourceFailureStageFTPPassiveNegotiate  = "ftp_passive_negotiate"
	SourceFailureStageFTPPassiveConnect    = "ftp_passive_connect"
	SourceFailureStageFTPRetrieve          = "ftp_retrieve"
	SourceFailureStageBorrowParse          = "borrow_parse"
	SourceFailureStageNasdaqRequest        = "nasdaq_request"
	SourceFailureStageNasdaqDecode         = "nasdaq_decode"
	SourceFailureStageNasdaqSchema         = "nasdaq_schema"
	SourceFailureStageWSHContractResolve   = "wsh_contract_resolve"
	SourceFailureStageWSHMetadata          = "wsh_metadata"
	SourceFailureStageWSHEvent             = "wsh_event"
	SourceFailureStageWSHDecode            = "wsh_decode"
	SourceFailureStageHistoricalFeeRequest = "historical_fee_request"
	SourceFailureStageHistoricalFeeDecode  = "historical_fee_decode"
	SourceFailureStageAuthorityPersist     = "authority_persist"
)

// ValidSourceFailure enforces the shared allowlist. Persistence and adapters
// call this before accepting a failure so upstream prose cannot masquerade as
// a typed code or stage.
func ValidSourceFailure(f *SourceFailure) bool {
	if f == nil {
		return true
	}
	validCode := false
	switch f.Code {
	case SourceFailureTimeout, SourceFailureDNSFailed, SourceFailureConnectionRefused,
		SourceFailureTransportFailed, SourceFailureProtocolRejected,
		SourceFailureAuthenticationRejected, SourceFailureInvalidPayload,
		SourceFailureAuthorityWriteFailed, SourceFailureNotEntitled,
		SourceFailureGatewayUnavailable, SourceFailureNoData,
		SourceFailurePacing, SourceFailureContractUnavailable:
		validCode = true
	}
	if !validCode {
		return false
	}
	switch f.Stage {
	case SourceFailureStageFTPControlConnect, SourceFailureStageFTPGreeting,
		SourceFailureStageFTPAuthenticate, SourceFailureStageFTPPassiveNegotiate,
		SourceFailureStageFTPPassiveConnect, SourceFailureStageFTPRetrieve,
		SourceFailureStageBorrowParse, SourceFailureStageNasdaqRequest,
		SourceFailureStageNasdaqDecode, SourceFailureStageNasdaqSchema,
		SourceFailureStageWSHContractResolve, SourceFailureStageWSHMetadata,
		SourceFailureStageWSHEvent, SourceFailureStageWSHDecode,
		SourceFailureStageHistoricalFeeRequest, SourceFailureStageHistoricalFeeDecode,
		SourceFailureStageAuthorityPersist:
		return true
	default:
		return false
	}
}

// Source-refresh states describe scheduler progress separately from the
// observation's evidence quality.
const (
	SourceRefreshCurrent            = "current"
	SourceRefreshNotDue             = "not_due"
	SourceRefreshPending            = "pending"
	SourceRefreshFetchFailed        = "fetch_failed"
	SourceRefreshFetchFailedBackoff = "fetch_failed_backoff"
)

// BuildRegimeLifecycle classifies broad-market-only regime evidence into the
// stress lifecycle used by downstream orchestration. It does not look at
// account, position, margin, or execution state.
//
// Confirmation policy (internal-docs/design/regime-calibration.md): only ELIGIBLE
// reds — deep, persistent, cadence-fresh evidence per the shared gates —
// count toward confirmed_stress/panic and confirmed_by. Provisional reds
// stay visible and land in unconfirmed. They drive early_warning only while
// the required input set is usable; broken or overdue evidence produces an
// explicit data_quality state instead of masquerading as a market warning.
// Independently current confirmed stress/panic is preserved even when an
// unrelated source is impaired. A severity governor then applies the provenance gate (heuristic
// pending_backtest evidence without a tape co-signature is capped one rung
// down) and the evidence-keyed quality cap, disclosing every downgrade in
// Governors.
func BuildRegimeLifecycle(r *RegimeSnapshotResult) LifecycleState {
	if r == nil {
		return LifecycleState{Stage: LifecycleDataQuality, Scope: "market", Severity: "watch", Readiness: "blocked", Timing: LifecycleTimingDataQuality, Confidence: "low"}
	}
	cb := BuildRegimeClusterBands(r)
	tally := tallyRegimeClusters(cb)
	confirmedNames := regimeEligibleRedNames(cb)
	yellowNames := regimeClusterNamesWithBand(cb.Confirmed, "yellow")
	unconfirmed := regimeProvisionalRedNames(cb)
	evidence := regimeLifecycleEvidence(cb, *r)
	confidence := r.Summary.Confidence
	if strings.TrimSpace(confidence) == "" {
		confidence = regimeLifecycleConfidence(tally)
	}
	state := LifecycleState{
		Scope:        "market",
		Severity:     "observe",
		Readiness:    "ready",
		Timing:       "",
		Confidence:   confidence,
		Evidence:     evidence,
		Unconfirmed:  unconfirmed,
		NotExecution: "Regime read only; no orders are placed by Canary.",
	}
	switch {
	case tally.ranked < RegimeVerdictFloor:
		state.Stage = LifecycleDataQuality
		state.Severity = "watch"
		state.Readiness = "blocked"
		state.Timing = LifecycleTimingDataQuality
	case regimeLifecyclePanic(*r, tally):
		state.Stage = LifecyclePanic
		state.Severity = "urgent"
		state.Timing = LifecycleTimingContemporary
	case regimeLifecycleConfirmedStress(*r, tally):
		state.Stage = LifecycleConfirmedStress
		state.Severity = "act"
		state.Timing = LifecycleTimingContemporary
	case regimeLifecycleEarlyWarning(*r, tally, unconfirmed):
		state.Stage = LifecycleEarlyWarning
		state.Severity = "watch"
		state.Timing = LifecycleTimingForwardWarning
	case regimeLifecycleOpportunity(*r, tally):
		state.Stage = LifecycleOpportunity
		state.Severity = "watch"
		state.Timing = LifecycleTimingRecovery
	case regimeLifecycleStabilization(*r, tally):
		state.Stage = LifecycleStabilization
		state.Severity = "observe"
		state.Timing = LifecycleTimingRecovery
	default:
		state.Stage = LifecycleQuiet
		state.Timing = "current"
	}
	if len(unconfirmed) > 0 && (state.Stage == LifecycleQuiet || state.Stage == LifecycleOpportunity || state.Stage == LifecycleStabilization) {
		state.Suppressed = append(state.Suppressed, unconfirmed...)
	}
	if len(yellowNames) > 0 && state.Stage == LifecycleQuiet {
		state.RejectedBy = append(state.RejectedBy, yellowNames...)
	}
	if state.Stage == LifecycleConfirmedStress || state.Stage == LifecyclePanic {
		state.ConfirmedBy = confirmedNames
	}
	inputs := regimeLifecycleInputVerdict(*r)
	if inputs.any() {
		blanks := inputs.global ||
			len(inputs.defects)+len(inputs.impaired) >= RegimeCurrencyBlankFloor ||
			!regimeLifecycleStageEvidenceCurrent(state, *r, cb, inputs)
		if blanks {
			state.Stage = LifecycleDataQuality
			state.Severity = "watch"
			state.Readiness = "blocked"
			state.Timing = LifecycleTimingDataQuality
			state.Confidence = "low"
			state.ConfirmedBy = nil
			state.Suppressed = append(state.Suppressed, unconfirmed...)
		} else if state.Readiness == "ready" {
			state.Readiness = "degraded"
			state.Confidence = capLifecycleConfidence(state.Confidence)
			state.Governors = append(state.Governors, GovernorAction{
				Action:   "readiness_degraded",
				From:     "ready",
				To:       "degraded",
				Reason:   "input_currency",
				Clusters: append(append([]string(nil), inputs.defects...), inputs.impaired...),
			})
		}
	}
	applyRegimeSeverityGovernor(&state, r, tally)
	state.Fingerprint = lifecycleFingerprint(state)
	return state
}

// regimeLifecycleHasIndependentCurrentStress permits a stress/panic stage to
// survive an unrelated broken input only when the stage has its own current
// confirmation. A tape panic is self-confirming; cluster-driven confirmation
// survives only when none of its confirming clusters is impaired.
func regimeLifecycleHasIndependentCurrentStress(state LifecycleState, r RegimeSnapshotResult, cb RegimeClusterBands) bool {
	if state.Stage != LifecycleConfirmedStress && state.Stage != LifecyclePanic {
		return false
	}
	cleanRed, cleanAtLeastYellow := 0, 0
	for i, name := range RegimeClusterNames {
		if i >= len(cb.Confirmed) || !regimeLifecycleClusterCurrent(r, name) {
			continue
		}
		switch cb.Confirmed[i] {
		case "red":
			cleanAtLeastYellow++
			if i < len(cb.Eligible) && cb.Eligible[i] {
				cleanRed++
			}
		case "yellow":
			cleanAtLeastYellow++
		}
	}
	spyCurrent := regimeLifecycleSPYTapeCurrent(r)
	vixCurrent := regimeLifecycleVIXTapeCurrent(r)
	panicCurrent := cleanRed >= 3 ||
		(spyCurrent && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0) && cleanRed >= 1) ||
		(spyCurrent && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -7.0))
	if state.Stage == LifecyclePanic {
		return panicCurrent
	}
	return cleanRed >= 2 ||
		(spyCurrent && cleanRed >= 1 && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -2.5)) ||
		(spyCurrent && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0) && cleanAtLeastYellow >= 2) ||
		(vixCurrent && cleanRed >= 1 && pctAtLeast(r.VIXTermStructure.VIXChangePct, 20.0))
}

// regimeClusterTally is the lifecycle's working view of the combined cluster
// bands. red/yellow/green count CONFIRMED bands; eligibleRed and
// provisionalRed split the raw reds by confirmation eligibility.
type regimeClusterTally struct {
	ranked         int
	green          int
	yellow         int
	red            int
	unranked       int
	eligibleRed    int
	provisionalRed int
}

func tallyRegimeClusters(cb RegimeClusterBands) regimeClusterTally {
	t := regimeClusterTally{
		eligibleRed:    cb.EligibleRedCount(),
		provisionalRed: cb.ProvisionalRedCount(),
	}
	for _, band := range cb.Confirmed {
		switch band {
		case "green":
			t.green++
			t.ranked++
		case "yellow":
			t.yellow++
			t.ranked++
		case "red":
			t.red++
			t.ranked++
		default:
			t.unranked++
		}
	}
	return t
}

// atLeastYellow counts confirmed clusters at yellow or worse. Lower-bound
// corroboration counts must use it rather than yellow alone: a cluster that
// deteriorates from yellow to red would otherwise leave the yellow bucket and
// quiet the stage.
func (t regimeClusterTally) atLeastYellow() int {
	return t.yellow + t.red
}

// applyRegimeSeverityGovernor applies, in order, the provenance gate and the
// evidence-keyed quality cap. Pure-tape panic (SPY ≤ −4% / −7%) is exempt
// from both — the tape is its own co-signature and its own evidence quality.
func applyRegimeSeverityGovernor(state *LifecycleState, r *RegimeSnapshotResult, tally regimeClusterTally) {
	if state.Stage != LifecycleConfirmedStress && state.Stage != LifecyclePanic {
		return
	}
	// The exemption requires a confirmable tape: a frozen closed-date print
	// is neither its own co-signature nor its own evidence quality, so a
	// cluster-driven panic on a closed date stays governable.
	tapePanic := state.Stage == LifecyclePanic && regimeLifecycleSPYTapeCurrent(*r) && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0)
	pending := regimePendingBacktestClusters(r, state.ConfirmedBy)
	if len(pending) > 0 && !tapePanic && !regimeTapeCosign(r) {
		from := state.Severity
		to := from
		switch state.Stage {
		case LifecycleConfirmedStress:
			to = "watch"
		case LifecyclePanic:
			to = "act"
		}
		if to != from {
			state.Severity = to
			state.Governors = append(state.Governors, GovernorAction{
				Action:   "severity_capped",
				From:     from,
				To:       to,
				Reason:   "pending_backtest_no_tape_cosign",
				Clusters: pending,
			})
		}
	}
	impaired := regimeImpairedConfirmingClusters(r, state.ConfirmedBy)
	if len(impaired) > 0 && !tapePanic && severityRank(state.Severity) > severityRank("watch") {
		from := state.Severity
		state.Severity = "watch"
		state.Governors = append(state.Governors, GovernorAction{
			Action:   "severity_capped",
			From:     from,
			To:       "watch",
			Reason:   "confirming_cluster_quality",
			Clusters: impaired,
		})
	}
}

func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "urgent":
		return 3
	case "act":
		return 2
	case "watch":
		return 1
	default:
		return 0
	}
}

// regimeTapeCosign reports whether the tape itself corroborates stress in
// this snapshot: SPY down 1.5%+, VIX up 10%+, or a fresh same-session
// VIX-term inversion. While threshold sets are pending_backtest, "act"
// requires one of these as the second witness. The SPY/VIX change arms
// (arms 1-2) require a confirmable tape — a frozen closed-date print cannot
// co-sign. Arm 3 keeps its own status gate unchanged.
func regimeTapeCosign(r *RegimeSnapshotResult) bool {
	if regimeLifecycleSPYTapeCurrent(*r) && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -1.5) {
		return true
	}
	if regimeLifecycleVIXTapeCurrent(*r) && pctAtLeast(r.VIXTermStructure.VIXChangePct, 10.0) {
		return true
	}
	return regimeLifecycleVIXTermCurrent(*r) && *r.VIXTermStructure.Ratio >= 1.0
}

// regimeTapeConfirmable reports whether direct SPY/VIX day-change prints may
// enter or hold tape-driven lifecycle terms for this snapshot. On an official
// closed date (weekend/holiday per TapeSessionFor) the day-change anchors are
// frozen last-session values, so tape terms evaluate as not firing; the
// cluster-driven terms are untouched. Empty state — outside embedded calendar
// coverage, or a builder that never stamped the session — fails open to full
// effect, so historical replay and older corpora behave exactly as before.
// Weekday pre/post/overnight prints are live (VIX prints overnight on
// weekdays) and keep full effect by design.
func regimeTapeConfirmable(r RegimeSnapshotResult) bool {
	return r.TapeSessionState != TapeSessionClosedDate
}

// regimePendingBacktestClusters returns the confirming clusters whose red
// rows classify on threshold sets still flagged pending_backtest. The flag
// finally becomes load-bearing here: per-set promotion (version-label bump
// via the backtest plan) relaxes the gate without policy edits.
func regimePendingBacktestClusters(r *RegimeSnapshotResult, confirmedBy []string) []string {
	var out []string
	for _, name := range confirmedBy {
		for _, meta := range regimeClusterRowMetas(r, name) {
			if meta.Band == "red" && meta.Thresholds != nil && meta.Thresholds.PendingBacktest {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// regimeImpairedConfirmingClusters returns confirming clusters whose source
// health or typed data-quality row is non-current — the evidence-keyed
// readiness cap and the independent-confirmation gate.
// Deliberately scoped to the CONFIRMING clusters: one dead unrelated feed
// must not mute a fresh multi-cluster confirmation.
func regimeImpairedConfirmingClusters(r *RegimeSnapshotResult, confirmedBy []string) []string {
	var out []string
	for _, name := range confirmedBy {
		if !regimeLifecycleClusterCurrent(*r, name) {
			out = append(out, name)
		}
	}
	return out
}

// regimeClusterRowMetas maps a cluster wire name to its row metadata.
func regimeClusterRowMetas(r *RegimeSnapshotResult, cluster string) []RegimeIndicatorMeta {
	switch cluster {
	case "vol":
		return []RegimeIndicatorMeta{r.VIXTermStructure.RegimeIndicatorMeta, r.VolOfVol.RegimeIndicatorMeta}
	case "credit":
		return []RegimeIndicatorMeta{r.HYGSPYDivergence.RegimeIndicatorMeta, r.CreditSpreads.RegimeIndicatorMeta}
	case "funding":
		return []RegimeIndicatorMeta{r.FundingStress.RegimeIndicatorMeta}
	case "fx":
		return []RegimeIndicatorMeta{r.USDJPY.RegimeIndicatorMeta}
	case "gamma":
		return []RegimeIndicatorMeta{r.GammaZero.RegimeIndicatorMeta}
	case "breadth":
		return []RegimeIndicatorMeta{r.Breadth.RegimeIndicatorMeta}
	default:
		return nil
	}
}

func regimeEligibleRedNames(cb RegimeClusterBands) []string {
	var out []string
	for i, band := range cb.Confirmed {
		if band == "red" && i < len(cb.Eligible) && cb.Eligible[i] && i < len(RegimeClusterNames) {
			out = append(out, RegimeClusterNames[i])
		}
	}
	return out
}

func regimeProvisionalRedNames(cb RegimeClusterBands) []string {
	var out []string
	for i, band := range cb.Raw {
		if band != "red" || i >= len(RegimeClusterNames) {
			continue
		}
		if i < len(cb.Confirmed) && cb.Confirmed[i] == "red" && i < len(cb.Eligible) && cb.Eligible[i] {
			continue
		}
		out = append(out, RegimeClusterNames[i])
	}
	return out
}

func regimeClusterNamesWithBand(bands []string, want string) []string {
	var out []string
	for i, band := range bands {
		if band == want && i < len(RegimeClusterNames) {
			out = append(out, RegimeClusterNames[i])
		}
	}
	return out
}

// BuildRegimePosture derives a stable display posture from the daemon-authored
// snapshot. A nil snapshot produces a blocked data-quality posture.
func BuildRegimePosture(r *RegimeSnapshotResult) RegimePosture {
	if r == nil {
		return RegimePosture{
			Label:      "No usable signal yet",
			Tone:       RegimeToneDataQuality,
			Stage:      LifecycleDataQuality,
			Severity:   "watch",
			Readiness:  "blocked",
			Confidence: "low",
		}
	}
	label := RegimeHeadline(r.Composite, r.Lifecycle.Stage)
	confidence := strings.TrimSpace(r.Lifecycle.Confidence)
	if confidence == "" {
		confidence = strings.TrimSpace(r.Summary.Confidence)
	}
	tone := regimePostureTone(r.Composite, r.Lifecycle)
	return RegimePosture{
		Label:      label,
		Tone:       tone,
		Stage:      strings.TrimSpace(r.Lifecycle.Stage),
		Severity:   regimePostureSeverity(r.Lifecycle, tone),
		Readiness:  strings.TrimSpace(r.Lifecycle.Readiness),
		Confidence: confidence,
		Evidence:   strings.TrimSpace(r.Summary.Evidence),
	}
}

// regimePostureTone maps the unified headline + lifecycle state to the display
// tone. Stage keeps the condition label honest, while severity owns urgency:
// governed confirmed stress with severity watch remains an amber watch so red
// stays available for act-grade stress and true risk-off conditions.
func regimePostureTone(c RegimeComposite, lifecycle LifecycleState) string {
	label := RegimeHeadline(c, lifecycle.Stage)
	if label == "Full risk-off conditions" {
		return RegimeToneRiskOff
	}
	if c.ClusterRankedCount < RegimeVerdictFloor {
		return RegimeToneDataQuality
	}
	switch lifecycle.Stage {
	case LifecycleDataQuality:
		return RegimeToneDataQuality
	case LifecycleConfirmedStress:
		if severityRank(lifecycle.Severity) <= severityRank("watch") {
			return RegimeToneWatch
		}
		return RegimeToneStress
	case LifecyclePanic:
		if severityRank(lifecycle.Severity) <= severityRank("watch") {
			return RegimeToneWatch
		}
		return RegimeToneStress
	}
	if regimeLifecycleReadinessDegraded(lifecycle.Readiness) {
		return RegimeToneDataQuality
	}
	switch lifecycle.Stage {
	case LifecycleEarlyWarning, LifecycleStabilization:
		return RegimeToneWatch
	}
	if c.ClusterYellowCount > 0 {
		return RegimeToneWatch
	}
	if lifecycle.Stage == LifecycleOpportunity {
		return RegimeToneNormal
	}
	switch label {
	case "Broad stress regime", "Confirmed stress regime":
		return RegimeToneStress
	case "Stress signal present", "Elevated stress watch":
		return RegimeToneWatch
	case "No usable signal yet", "Insufficient signal — too few inputs ready":
		return RegimeToneDataQuality
	default:
		return RegimeToneNormal
	}
}

func regimePostureSeverity(lifecycle LifecycleState, tone string) string {
	severity := strings.TrimSpace(lifecycle.Severity)
	if severity == "" {
		severity = "observe"
	}
	if severity == "observe" {
		switch tone {
		case RegimeToneWatch, RegimeToneDataQuality:
			return "watch"
		}
	}
	return severity
}

func regimeLifecycleReadinessDegraded(readiness string) bool {
	switch strings.ToLower(strings.TrimSpace(readiness)) {
	case "blocked", "degraded", "failed", "partial", "warming":
		return true
	default:
		return false
	}
}

// BuildRegimeSourceHealth derives per-cluster freshness and confidence. It
// returns nil for a nil snapshot; nil therefore means unavailable, not healthy.
func BuildRegimeSourceHealth(r *RegimeSnapshotResult, now time.Time) []SourceHealth {
	if r == nil {
		return nil
	}
	if now.IsZero() {
		now = r.AsOf
	}
	bands := BuildRegimeClusterBands(r).Confirmed
	volPartial := regimeSourceMissingRequiredFields(r.VIXTermStructure.Status, r.VIXTermStructure.Band, r.VIXTermStructure.FieldsMissing)
	creditPartial := regimeSourceMissingRequiredFields(r.HYGSPYDivergence.Status, r.HYGSPYDivergence.Band, r.HYGSPYDivergence.FieldsMissing) ||
		regimeSourceMissingRequiredFields(r.CreditSpreads.Status, r.CreditSpreads.Band, r.CreditSpreads.FieldsMissing)
	rows := []struct {
		name          string
		band          string
		statuses      []string
		asOf          []RegimeAsOfSummary
		qualityStatus string
		partial       bool
	}{
		{"vol", bands[0], []string{r.VIXTermStructure.Status, r.VolOfVol.Status}, []RegimeAsOfSummary{metaAsOf(r.VIXTermStructure.RegimeIndicatorMeta), metaAsOf(r.VolOfVol.RegimeIndicatorMeta)}, "", volPartial},
		{"credit", bands[1], []string{r.HYGSPYDivergence.Status, r.CreditSpreads.Status}, []RegimeAsOfSummary{metaAsOf(r.HYGSPYDivergence.RegimeIndicatorMeta), metaAsOf(r.CreditSpreads.RegimeIndicatorMeta)}, "", creditPartial},
		{"funding", bands[2], []string{r.FundingStress.Status}, []RegimeAsOfSummary{metaAsOf(r.FundingStress.RegimeIndicatorMeta)}, "", regimeSourceMissingRequiredFields(r.FundingStress.Status, r.FundingStress.Band, r.FundingStress.FieldsMissing)},
		{"fx", bands[3], []string{r.USDJPY.Status}, []RegimeAsOfSummary{metaAsOf(r.USDJPY.RegimeIndicatorMeta)}, "", regimeSourceMissingRequiredFields(r.USDJPY.Status, r.USDJPY.Band, r.USDJPY.FieldsMissing)},
		{"gamma", bands[4], []string{r.GammaZero.Status}, []RegimeAsOfSummary{metaAsOf(r.GammaZero.RegimeIndicatorMeta)}, regimeSourceQualityStatus(r.DataQuality, "gamma"), regimeSourceMissingRequiredFields(r.GammaZero.Status, r.GammaZero.Band, r.GammaZero.FieldsMissing)},
		{"breadth", bands[5], []string{r.Breadth.Status}, []RegimeAsOfSummary{metaAsOf(r.Breadth.RegimeIndicatorMeta)}, regimeSourceQualityStatus(r.DataQuality, "breadth"), regimeSourceMissingRequiredFields(r.Breadth.Status, r.Breadth.Band, r.Breadth.FieldsMissing)},
	}
	out := make([]SourceHealth, 0, len(rows))
	for _, row := range rows {
		asOf := weakestRegimeAsOf(row.asOf)
		status := regimeSourceStatus(row.statuses, row.band, row.qualityStatus, row.partial)
		refreshState := ""
		if class, scheduled := RegimeClusterScheduledContext(*r, row.name); scheduled {
			// Keep the raw row stale for evidence honesty, but normalize the
			// aggregate source state: no newer observation is being served
			// because the window is closed (not_due) or its refresh is in
			// flight inside a bounded window (pending).
			status = SourceStatusOK
			refreshState = SourceRefreshNotDue
			if class == RegimeFreshnessPending {
				refreshState = SourceRefreshPending
			}
		}
		out = append(out, SourceHealth{
			Source:               row.name,
			Status:               status,
			AsOf:                 asOf,
			AgeSeconds:           sourceAgeSeconds(now, asOf),
			MaxAgeSeconds:        RegimeSourceMaxAgeSeconds(row.name),
			Confidence:           regimeSourceConfidence(status),
			FingerprintStability: FingerprintStabilitySemanticBuckets,
			RefreshState:         refreshState,
		})
	}
	return out
}

// RegimeSourceMaxAgeSeconds is the served per-cluster staleness policy for
// the cluster's weakest-leg as_of: older than this is unambiguously overdue.
// Wall-clock documentation values sized to each cluster's slowest native
// cadence (VVIX daily close over a weekend, FRED publication lag, a Friday
// gamma compute read on Monday pre-open); the binding eligibility gate uses
// trading-date logic daemon-side. Served so renderers derive their stale
// badges from the wire instead of hardcoding twins.
func RegimeSourceMaxAgeSeconds(source string) int64 {
	const day = int64(24 * 60 * 60)
	switch source {
	case "vol":
		return 4 * day
	case "credit":
		return 7 * day
	case "funding":
		return 7 * day
	case "fx":
		return 4 * day
	case "gamma":
		return 3 * day
	case "breadth":
		return 4 * day
	default:
		return 0
	}
}

// BuildLifecycleFingerprint returns a semantic identity that ignores
// continuous values and wall-clock churn while retaining classified changes.
func BuildLifecycleFingerprint(state LifecycleState) Fingerprint {
	return lifecycleFingerprint(state)
}

func lifecycleFingerprint(state LifecycleState) Fingerprint {
	// Governors enter the projection as action:reason tokens only — never
	// ages, depths, or other continuous values, which would churn the
	// fingerprint (and downstream alert dedupe) without a semantic change.
	governors := make([]string, 0, len(state.Governors))
	for _, g := range state.Governors {
		governors = append(governors, strings.ToLower(strings.TrimSpace(g.Action))+":"+strings.ToLower(strings.TrimSpace(g.Reason)))
	}
	projection := struct {
		Scope       string              `json:"scope,omitempty"`
		Stage       string              `json:"stage,omitempty"`
		Severity    string              `json:"severity,omitempty"`
		Readiness   string              `json:"readiness,omitempty"`
		Timing      string              `json:"timing,omitempty"`
		Confidence  string              `json:"confidence,omitempty"`
		Evidence    []LifecycleEvidence `json:"evidence,omitempty"`
		ConfirmedBy []string            `json:"confirmed_by,omitempty"`
		Unconfirmed []string            `json:"unconfirmed,omitempty"`
		Suppressed  []string            `json:"suppressed,omitempty"`
		RejectedBy  []string            `json:"rejected_by,omitempty"`
		Governors   []string            `json:"governors,omitempty"`
	}{
		Scope:       strings.ToLower(strings.TrimSpace(state.Scope)),
		Stage:       strings.ToLower(strings.TrimSpace(state.Stage)),
		Severity:    strings.ToLower(strings.TrimSpace(state.Severity)),
		Readiness:   strings.ToLower(strings.TrimSpace(state.Readiness)),
		Timing:      strings.ToLower(strings.TrimSpace(state.Timing)),
		Confidence:  strings.ToLower(strings.TrimSpace(state.Confidence)),
		Evidence:    state.Evidence,
		ConfirmedBy: cleanSorted(state.ConfirmedBy),
		Unconfirmed: cleanSorted(state.Unconfirmed),
		Suppressed:  cleanSorted(state.Suppressed),
		RejectedBy:  cleanSorted(state.RejectedBy),
		Governors:   cleanSorted(governors),
	}
	// lifecycle-fp-v2: eligibility-aware evidence (Confirmed now means
	// eligible-confirmed) + governors. The version bump re-fires active
	// alerts once on upgrade — accepted and changelog-noted.
	return semanticFingerprint("lifecycle-fp-v2", projection)
}

// regimeLifecyclePanic: three deep/fresh/persistent independent reds, or
// tape-grade crashes. Eligible reds only — provisional evidence never
// reaches the panic tally. Tape arms require a confirmable session: a frozen
// closed-date print cannot enter (or, on re-evaluation, hold) panic.
func regimeLifecyclePanic(r RegimeSnapshotResult, t regimeClusterTally) bool {
	if t.eligibleRed >= 3 {
		return true
	}
	return regimeTapeConfirmable(r) &&
		((pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0) && t.eligibleRed >= 1) ||
			pctAtMost(r.HYGSPYDivergence.SPYChangePct, -7.0))
}

func regimeLifecycleConfirmedStress(r RegimeSnapshotResult, t regimeClusterTally) bool {
	if t.eligibleRed >= 2 {
		return true
	}
	return regimeTapeConfirmable(r) &&
		((t.eligibleRed >= 1 && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -2.5)) ||
			(pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0) && t.atLeastYellow() >= 2) ||
			(t.eligibleRed >= 1 && pctAtLeast(r.VIXTermStructure.VIXChangePct, 20.0)))
}

// regimeLifecycleEarlyWarning is the home of provisional reds: any raw red
// (eligible or not) warns immediately, even though only eligible reds may
// confirm. The direct tape triggers require a confirmable session; cluster
// evidence warns on any date.
func regimeLifecycleEarlyWarning(r RegimeSnapshotResult, t regimeClusterTally, unconfirmed []string) bool {
	return t.eligibleRed+t.provisionalRed >= 1 ||
		t.red >= 1 ||
		t.atLeastYellow() >= 3 ||
		len(unconfirmed) > 0 ||
		(regimeTapeConfirmable(r) &&
			(pctAtMost(r.HYGSPYDivergence.SPYChangePct, -1.5) ||
				pctAtLeast(r.VIXTermStructure.VIXChangePct, 10.0)))
}

// Opportunity and stabilization are tape-driven recovery stages: a frozen
// closed-date rally print is last session's news, so neither stage is
// enterable on a closed date and the machine reads quiet instead.
func regimeLifecycleOpportunity(r RegimeSnapshotResult, t regimeClusterTally) bool {
	return t.red == 0 &&
		t.yellow <= 1 &&
		t.ranked >= 3 &&
		regimeTapeConfirmable(r) &&
		pctAtLeast(r.HYGSPYDivergence.SPYChangePct, 1.5) &&
		pctAtMost(r.VIXTermStructure.VIXChangePct, -10.0)
}

func regimeLifecycleStabilization(r RegimeSnapshotResult, t regimeClusterTally) bool {
	return t.red == 0 &&
		t.yellow > 0 &&
		t.yellow <= 2 &&
		regimeTapeConfirmable(r) &&
		(pctAtLeast(r.HYGSPYDivergence.SPYChangePct, 1.0) || pctAtMost(r.VIXTermStructure.VIXChangePct, -10.0))
}

func regimeLifecycleConfidence(t regimeClusterTally) string {
	switch {
	case t.ranked < RegimeVerdictFloor:
		return "low"
	case t.unranked > 0:
		return "medium"
	default:
		return "high"
	}
}

// RegimeCurrencyBlankFloor is how many clusters may be defective or impaired
// before the whole market state blanks to data_quality instead of standing with
// degraded readiness. One dead or stale feed degrades and is named; two
// independent ones mean the read no longer describes the market. Operator
// decision, 2026-07-31: the tolerance was set for defects, and applies to the
// impaired grade too because two clusters that cannot supply current evidence
// leave the same hole whichever grade got them there.
const RegimeCurrencyBlankFloor = 2

// regimeLifecycleInputVerdict classifies every input defect once, by cluster,
// so the lifecycle can ask whether the defect touches what it is claiming
// rather than blanking on any defect anywhere.
//
// Defects are fatal-grade: an overdue or untyped unit, or a typed data-quality
// row that is not an exempt scheduled state. Impairments are degrade-grade: a
// stale unit, or a cluster whose evidence is stale but present. A defect that
// cannot be attributed to one of the six clusters — authority health, a
// surface-wide degradation — is global and always blanks: it describes the
// publication itself, not one input.
type regimeLifecycleInputs struct {
	defects  []string
	impaired []string
	global   bool
}

func (in regimeLifecycleInputs) clusterDefective(name string) bool {
	return sourceInDataQualityClusters(in.defects, name)
}

func (in regimeLifecycleInputs) any() bool {
	return in.global || len(in.defects) > 0 || len(in.impaired) > 0
}

func regimeLifecycleInputVerdict(r RegimeSnapshotResult) regimeLifecycleInputs {
	defects := map[string]bool{}
	impaired := map[string]bool{}
	global := false
	mark := func(into map[string]bool, name string) {
		name = strings.ToLower(strings.TrimSpace(name))
		if regimeClusterKnown(name) {
			into[name] = true
			return
		}
		// An unattributed defect is not one input's problem.
		global = true
	}
	for _, item := range r.DataQuality {
		attributed := len(item.PartialClusters) + len(item.DegradedClusters) + len(item.StaleClusters)
		for _, name := range item.PartialClusters {
			mark(defects, name)
		}
		for _, name := range item.DegradedClusters {
			// A cluster degraded only by its own publication cadence, while its
			// currency is a scheduled state, is not an input defect: it is the
			// schedule. Anything else — coverage, model, OI, entitlement — still
			// fails closed.
			if regimeClusterCadenceOnlyDegraded(r, name) {
				continue
			}
			mark(defects, name)
		}
		for _, name := range item.StaleClusters {
			if _, scheduled := RegimeClusterScheduledContext(r, name); scheduled {
				continue
			}
			mark(impaired, name)
		}
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "degraded", "partial", RegimeStatusStale:
			if attributed == 0 {
				global = true
			}
		}
	}
	for _, name := range RegimeClusterNames {
		switch RegimeCurrencyGrade(RegimeClusterCurrency(r, name)) {
		case RegimeCurrencyGradeFatal:
			defects[name] = true
		case RegimeCurrencyGradeDegrade:
			impaired[name] = true
		}
		switch grade := regimeSourceHealthGrade(r, name); grade {
		case RegimeCurrencyGradeFatal:
			defects[name] = true
		case RegimeCurrencyGradeDegrade:
			impaired[name] = true
		}
	}
	out := regimeLifecycleInputs{global: global}
	for _, name := range RegimeClusterNames {
		switch {
		case defects[name]:
			out.defects = append(out.defects, name)
		case impaired[name]:
			out.impaired = append(out.impaired, name)
		}
	}
	return out
}

// regimeSourceHealthGrade grades the cluster's aggregate source contract, which
// the row currency does not cover: a missing health row is untyped evidence and
// fails closed, an age past the served policy is overdue however the rows read,
// and a failed or unknown refresh state is a gap.
func regimeSourceHealthGrade(r RegimeSnapshotResult, name string) string {
	health, ok := regimeLifecycleSourceHealth(r, name)
	if !ok {
		return RegimeCurrencyGradeFatal
	}
	if health.MaxAgeSeconds > 0 && health.AgeSeconds >= health.MaxAgeSeconds {
		return RegimeCurrencyGradeFatal
	}
	scheduled, isScheduled := RegimeClusterScheduledContext(r, name)
	switch strings.ToLower(strings.TrimSpace(health.Status)) {
	case SourceStatusOK:
	case SourceStatusStale:
		// A scheduled cluster serves its last publication on purpose; the
		// aggregate reading stale is the schedule, not a gap.
		if !isScheduled {
			return RegimeCurrencyGradeDegrade
		}
	default:
		return RegimeCurrencyGradeFatal
	}
	refresh := strings.ToLower(strings.TrimSpace(health.RefreshState))
	if isScheduled {
		// The typed contract must agree with itself: a cluster whose currency
		// is a scheduled state and whose scheduler says otherwise is not
		// evidence anyone can reason about.
		if (scheduled == RegimeFreshnessNotDue && refresh == SourceRefreshNotDue) ||
			(scheduled == RegimeFreshnessPending && refresh == SourceRefreshPending) {
			return RegimeCurrencyGradeNone
		}
		return RegimeCurrencyGradeFatal
	}
	switch refresh {
	case "", SourceRefreshCurrent:
		return RegimeCurrencyGradeNone
	case SourceRefreshFetchFailed, SourceRefreshFetchFailedBackoff:
		return RegimeCurrencyGradeDegrade
	default:
		return RegimeCurrencyGradeFatal
	}
}

func regimeClusterKnown(name string) bool {
	return slices.Contains(RegimeClusterNames, name)
}

// regimeClusterCadenceOnlyDegraded reports a degraded cluster whose only
// blocker is its own publication cadence while its currency is a scheduled
// state. Gamma at the options open is the case this exists for: a
// current-session compute is in flight, so the served prior-session result is
// rankability-blocked on session mismatch alone.
func regimeClusterCadenceOnlyDegraded(r RegimeSnapshotResult, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if _, scheduled := RegimeClusterScheduledContext(r, name); !scheduled {
		return false
	}
	if name != "gamma" {
		return false
	}
	return gammaBlockedOnSessionCadenceOnly(r.GammaZero.Envelope.Result)
}

// regimeLifecycleStageEvidenceCurrent reports whether the stage stands on its
// own current evidence. This generalizes the confirmed-stress escape hatch to
// every stage: a defect elsewhere degrades the read, a defect in what the stage
// is claiming blanks it.
func regimeLifecycleStageEvidenceCurrent(state LifecycleState, r RegimeSnapshotResult, cb RegimeClusterBands, inputs regimeLifecycleInputs) bool {
	switch state.Stage {
	case LifecycleConfirmedStress, LifecyclePanic:
		return regimeLifecycleHasIndependentCurrentStress(state, r, cb)
	case LifecycleEarlyWarning:
		// A warning needs at least one non-defective cluster carrying the
		// visible evidence, or a current tape arm. An overdue red is not a
		// market warning — it is missing evidence, and stays data_quality.
		for i, name := range RegimeClusterNames {
			if i >= len(cb.Raw) || (cb.Raw[i] != "red" && cb.Raw[i] != "yellow") {
				continue
			}
			if !inputs.clusterDefective(name) {
				return true
			}
		}
		return (regimeLifecycleSPYTapeCurrent(r) && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -1.5)) ||
			(regimeLifecycleVIXTapeCurrent(r) && pctAtLeast(r.VIXTermStructure.VIXChangePct, 10.0))
	default:
		// quiet, opportunity, and stabilization claim an absence. There is no
		// single witness to check; the tolerated-defect count is what sizes
		// how much darkness an absence claim survives.
		return true
	}
}

type regimeLifecycleRequiredRow struct {
	status    string
	freshness *RegimeFreshness
}

func regimeLifecycleClusterRows(r RegimeSnapshotResult, name string) []regimeLifecycleRequiredRow {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "vol":
		return []regimeLifecycleRequiredRow{
			{r.VIXTermStructure.Status, r.VIXTermStructure.Freshness},
			{r.VolOfVol.Status, r.VolOfVol.Freshness},
		}
	case "credit":
		return []regimeLifecycleRequiredRow{
			{r.HYGSPYDivergence.Status, r.HYGSPYDivergence.Freshness},
			{r.CreditSpreads.Status, r.CreditSpreads.Freshness},
		}
	case "funding":
		return []regimeLifecycleRequiredRow{{r.FundingStress.Status, r.FundingStress.Freshness}}
	case "fx":
		return []regimeLifecycleRequiredRow{{r.USDJPY.Status, r.USDJPY.Freshness}}
	case "gamma":
		return []regimeLifecycleRequiredRow{{r.GammaZero.Status, r.GammaZero.Freshness}}
	case "breadth":
		return []regimeLifecycleRequiredRow{{r.Breadth.Status, r.Breadth.Freshness}}
	default:
		return nil
	}
}

// RegimeClusterScheduledContext reports a cluster whose non-fresh currency is
// a scheduled publication state rather than a defect, and which state it is:
//
//   - not_due — the source's publication window is closed, so no newer
//     observation can exist yet.
//   - pending — the current period's refresh is in flight, evidenced by a
//     typed marker, inside a bounded window anchored to the period start.
//
// It is the single exact-cluster exemption used by source health, data-quality
// projection, warnings, and lifecycle readiness. Every caller must consult the
// pair through this function rather than one class alone: the two states carry
// identical authority (visible context, never confirmation) and differ only in
// why no newer observation is being served. Malformed, partial, stale, and
// overdue clusters still fail closed, and the currency roll-up has already
// applied the served max-age bound, so a dead subscription cannot hold a
// scheduled state open indefinitely.
func RegimeClusterScheduledContext(r RegimeSnapshotResult, name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	class := RegimeClusterCurrency(r, name)
	switch class {
	case RegimeFreshnessNotDue, RegimeFreshnessPending:
	default:
		return "", false
	}
	rows := regimeLifecycleClusterRows(r, name)
	ok := false
	switch name {
	case "vol":
		ok = len(rows) == 2 &&
			regimeLifecycleRowCurrent(rows[1]) &&
			!regimeSourceMissingRequiredFields(r.VIXTermStructure.Status, r.VIXTermStructure.Band, r.VIXTermStructure.FieldsMissing)
	case "fx":
		ok = len(rows) == 1 &&
			r.USDJPY.Last != nil &&
			!regimeSourceMissingRequiredFields(r.USDJPY.Status, r.USDJPY.Band, r.USDJPY.FieldsMissing)
	case "gamma":
		ok = len(rows) == 1 &&
			r.GammaZero.Envelope.Result != nil &&
			!regimeSourceMissingRequiredFields(r.GammaZero.Status, r.GammaZero.Band, r.GammaZero.FieldsMissing) &&
			(class == RegimeFreshnessNotDue || r.GammaZero.Envelope.Refreshing)
	case "breadth":
		ok = len(rows) == 1 &&
			r.Breadth.Envelope.State == BreadthStateReady &&
			r.Breadth.Envelope.SessionKey != "" &&
			!regimeSourceMissingRequiredFields(r.Breadth.Status, r.Breadth.Band, r.Breadth.FieldsMissing) &&
			(class == RegimeFreshnessNotDue || r.Breadth.Envelope.Refreshing)
	}
	if !ok {
		return "", false
	}
	return class, true
}

// RegimeClusterExpectedNotDue reports the closed-window half of the scheduled
// pair. Prefer RegimeClusterScheduledContext unless the caller genuinely means
// "the window is closed" rather than "no newer observation is being served".
func RegimeClusterExpectedNotDue(r RegimeSnapshotResult, name string) bool {
	class, ok := RegimeClusterScheduledContext(r, name)
	return ok && class == RegimeFreshnessNotDue
}

func regimeLifecycleRowCurrent(row regimeLifecycleRequiredRow) bool {
	return strings.EqualFold(strings.TrimSpace(row.status), RegimeStatusOK) &&
		row.freshness != nil && row.freshness.Class == RegimeFreshnessFresh
}

func regimeLifecycleSourceHealth(r RegimeSnapshotResult, name string) (SourceHealth, bool) {
	for _, source := range r.SourceHealth {
		if strings.EqualFold(strings.TrimSpace(source.Source), strings.TrimSpace(name)) {
			return source, true
		}
	}
	return SourceHealth{}, false
}

func regimeLifecycleClusterInDataQuality(r RegimeSnapshotResult, name string) bool {
	for _, item := range r.DataQuality {
		if sourceInDataQualityClusters(item.PartialClusters, name) ||
			sourceInDataQualityClusters(item.DegradedClusters, name) ||
			sourceInDataQualityClusters(item.StaleClusters, name) {
			return true
		}
	}
	return false
}

// regimeLifecycleClusterCurrent is the strict, cluster-scoped currency test:
// every row fresh, no typed data-quality row against the cluster, and source
// health agreeing. Used where a widened test would widen authority — the
// clean-red tally, the SPY tape arms, and the severity cap on an impaired
// confirming cluster.
func regimeLifecycleClusterCurrent(r RegimeSnapshotResult, name string) bool {
	if !RegimeCurrencyMayConfirm(RegimeClusterCurrency(r, name)) {
		return false
	}
	if regimeLifecycleClusterInDataQuality(r, name) {
		return false
	}
	health, ok := regimeLifecycleSourceHealth(r, name)
	if !ok || !strings.EqualFold(strings.TrimSpace(health.Status), SourceStatusOK) {
		return false
	}
	if health.MaxAgeSeconds > 0 && health.AgeSeconds >= health.MaxAgeSeconds {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(health.RefreshState)) {
	case "", SourceRefreshCurrent:
		return true
	default:
		return false
	}
}

func regimeLifecycleSPYTapeCurrent(r RegimeSnapshotResult) bool {
	return regimeTapeConfirmable(r) && regimeLifecycleClusterCurrent(r, "credit") &&
		r.HYGSPYDivergence.SPYChangePct != nil
}

// regimeLifecycleVIXTapeCurrent gates the arms that read only the VIX
// day-change print. Scoped to that leg, not to the vol cluster: VIX3M is a thin
// index that misses polls and is not due pre-open, and neither event says
// anything about a live VIX print. The ratio arm keeps cluster scope below.
func regimeLifecycleVIXTapeCurrent(r RegimeSnapshotResult) bool {
	return regimeTapeConfirmable(r) &&
		RegimeCurrencyMayConfirm(RegimeVIXTapeCurrency(r)) &&
		r.VIXTermStructure.VIXChangePct != nil
}

// regimeLifecycleVIXTermCurrent gates the term-inversion co-sign, which reads
// the ratio and therefore depends on both legs. Deliberately cluster-scoped:
// narrowing it to the VIX-term row alone would widen a severity co-signature
// with no observed defect asking for it.
func regimeLifecycleVIXTermCurrent(r RegimeSnapshotResult) bool {
	return regimeTapeConfirmable(r) && regimeLifecycleClusterCurrent(r, "vol") &&
		r.VIXTermStructure.Ratio != nil
}

func capLifecycleConfidence(confidence string) string {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "", "high":
		return "medium"
	default:
		return confidence
	}
}

func rankableLifecycleGammaBand(g RegimeGammaZero) string {
	if g.Envelope.Result == nil || g.Envelope.Result.Quality == nil ||
		g.Envelope.Result.Quality.Rankability != GammaRankabilityRankable {
		return ""
	}
	return g.Band
}

func regimeLifecycleEvidence(cb RegimeClusterBands, r RegimeSnapshotResult) []LifecycleEvidence {
	out := make([]LifecycleEvidence, 0, len(RegimeClusterNames)+2)
	for i, name := range RegimeClusterNames {
		if i >= len(cb.Raw) || cb.Raw[i] == "" {
			continue
		}
		band := cb.Raw[i]
		// Timing honesty: only an ELIGIBLE confirmed red is contemporaneous
		// act-grade evidence. Provisional measurements stay visible but are
		// never confirmation; if one is overdue, the required-input gate
		// makes the top-level state data_quality rather than a market warning.
		confirmedSignal := band == "red" &&
			i < len(cb.Confirmed) && cb.Confirmed[i] == "red" &&
			i < len(cb.Eligible) && cb.Eligible[i]
		timing := LifecycleTimingForwardWarning
		severity := "watch"
		if confirmedSignal {
			timing = LifecycleTimingContemporary
			severity = "act"
		}
		out = append(out, LifecycleEvidence{
			Source:    name,
			Signal:    "cluster",
			Bucket:    band,
			Timing:    timing,
			Severity:  severity,
			Confirmed: confirmedSignal,
		})
	}
	confirmable := regimeTapeConfirmable(r)
	if r.HYGSPYDivergence.SPYChangePct != nil {
		out = append(out, tapeLifecycleEvidence("spy", *r.HYGSPYDivergence.SPYChangePct, -1.5, -2.5, confirmable))
	}
	if r.VIXTermStructure.VIXChangePct != nil {
		out = append(out, tapeLifecycleEvidence("vix", *r.VIXTermStructure.VIXChangePct, 10, 20, confirmable))
	}
	return out
}

func tapeLifecycleEvidence(source string, observed, watch, act float64, confirmable bool) LifecycleEvidence {
	ev := LifecycleEvidence{Source: source, Signal: "tape", Timing: "current"}
	switch source {
	case "spy":
		switch {
		case observed <= act:
			ev.Bucket = "red"
			ev.Timing = LifecycleTimingContemporary
			ev.Severity = "act"
			ev.Confirmed = true
		case observed <= watch:
			ev.Bucket = "yellow"
			ev.Timing = LifecycleTimingForwardWarning
			ev.Severity = "watch"
		default:
			ev.Bucket = "green"
			ev.Severity = "observe"
		}
	default:
		switch {
		case observed >= act:
			ev.Bucket = "red"
			ev.Timing = LifecycleTimingContemporary
			ev.Severity = "act"
			ev.Confirmed = true
		case observed >= watch:
			ev.Bucket = "yellow"
			ev.Timing = LifecycleTimingForwardWarning
			ev.Severity = "watch"
		default:
			ev.Bucket = "green"
			ev.Severity = "observe"
		}
	}
	// Closed-date demotion mirrors the Stress tape row: the bucket keeps the
	// frozen print's factual magnitude, but a print that cannot confirm is
	// never contemporaneous act-grade evidence — it reads as a forward
	// warning to re-check at the next open, severity observe, unconfirmed.
	if !confirmable && ev.Bucket != "green" {
		ev.Timing = LifecycleTimingForwardWarning
		ev.Severity = "observe"
		ev.Confirmed = false
	}
	return ev
}

func strongestLifecycleBand(bands ...string) string {
	seenYellow, seenGreen := false, false
	for _, band := range bands {
		switch strings.ToLower(strings.TrimSpace(band)) {
		case "red":
			return "red"
		case "yellow":
			seenYellow = true
		case "green":
			seenGreen = true
		}
	}
	if seenYellow {
		return "yellow"
	}
	if seenGreen {
		return "green"
	}
	return ""
}

// isolatedLifecycleEquityVolConfirmed corroborates an isolated equity-vol
// red inside cluster-band combination (BuildRegimeClusterBands). The SPY/VIX
// change terms here deliberately stay session-blind: they only preserve a red that live-session
// banding already produced, and with the stage tape arms closed-date-gated
// the worst residual is cluster-grade early warning. Revisit with journal
// evidence if weekend audits show frozen corroboration mattering.
func isolatedLifecycleEquityVolConfirmed(r RegimeSnapshotResult) bool {
	if r.VIXTermStructure.Band == "red" {
		return true
	}
	if r.VolOfVol.Last != nil && *r.VolOfVol.Last >= 120 {
		return true
	}
	if r.VIXTermStructure.VIXChangePct != nil && *r.VIXTermStructure.VIXChangePct >= 20 {
		return true
	}
	return r.HYGSPYDivergence.SPYChangePct != nil && *r.HYGSPYDivergence.SPYChangePct <= -1
}

func pctAtMost(v *float64, threshold float64) bool {
	return v != nil && *v <= threshold
}

func pctAtLeast(v *float64, threshold float64) bool {
	return v != nil && *v >= threshold
}

func metaAsOf(meta RegimeIndicatorMeta) RegimeAsOfSummary {
	if meta.AsOf == nil {
		return RegimeAsOfSummary{}
	}
	return *meta.AsOf
}

func latestRegimeAsOf(values []RegimeAsOfSummary) time.Time {
	var latest time.Time
	for _, v := range values {
		if !v.Time.IsZero() && v.Time.After(latest) {
			latest = v.Time
		}
	}
	return latest
}

func weakestRegimeAsOf(values []RegimeAsOfSummary) time.Time {
	var oldest time.Time
	for _, v := range values {
		if v.Time.IsZero() {
			continue
		}
		if oldest.IsZero() || v.Time.Before(oldest) {
			oldest = v.Time
		}
	}
	if oldest.IsZero() {
		return latestRegimeAsOf(values)
	}
	return oldest
}

func sourceAgeSeconds(now, asOf time.Time) int64 {
	if now.IsZero() || asOf.IsZero() {
		return 0
	}
	age := now.Sub(asOf)
	if age < 0 {
		return 0
	}
	return int64(age.Seconds())
}

func regimeSourceStatus(statuses []string, band string, qualityStatus string, partial bool) string {
	switch qualityStatus {
	case "degraded":
		return "degraded"
	case "partial":
		return "partial"
	case RegimeStatusStale:
		return RegimeStatusStale
	}
	if partial {
		return "partial"
	}
	sawBad, sawStale, sawComputing, sawError, sawUnavailable := false, false, false, false, false
	for _, status := range statuses {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case RegimeStatusStale:
			sawStale = true
		case RegimeStatusComputing:
			sawBad = true
			sawComputing = true
		case RegimeStatusError:
			sawBad = true
			sawError = true
		case RegimeStatusUnavailable:
			sawBad = true
			sawUnavailable = true
		case "", RegimeStatusOK:
		default:
			sawBad = true
		}
	}
	if band != "" && sawBad {
		return "partial"
	}
	if sawError {
		return RegimeStatusError
	}
	if sawUnavailable {
		return RegimeStatusUnavailable
	}
	if sawComputing {
		return RegimeStatusComputing
	}
	if sawStale {
		return RegimeStatusStale
	}
	return RegimeStatusOK
}

func regimeSourceMissingRequiredFields(status string, band string, fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case RegimeStatusOK, RegimeStatusStale:
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(band)) {
	case "", "unranked":
		return true
	default:
		return false
	}
}

func regimeSourceConfidence(status string) string {
	switch status {
	case RegimeStatusOK:
		return "high"
	case RegimeStatusStale, "partial", "degraded":
		return "medium"
	default:
		return "low"
	}
}

func regimeSourceQualityStatus(items []DataQualityHealth, source string) string {
	out := ""
	for _, item := range items {
		status := strings.ToLower(strings.TrimSpace(item.Status))
		if sourceInDataQualityClusters(item.DegradedClusters, source) || sourceInDataQualityClusters(item.PartialClusters, source) || sourceInDataQualityClusters(item.StaleClusters, source) {
			switch status {
			case "degraded":
				return "degraded"
			case "partial":
				if out != "degraded" {
					out = "partial"
				}
			case RegimeStatusStale:
				if out == "" {
					out = RegimeStatusStale
				}
			}
		}
	}
	return out
}

func sourceInDataQualityClusters(clusters []string, source string) bool {
	for _, cluster := range clusters {
		if strings.EqualFold(cluster, source) {
			return true
		}
	}
	return false
}
