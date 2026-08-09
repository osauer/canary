package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// NudgeKindReconcileDue and the related constants define the closed nudge
const (
	NudgeKindReconcileDue       = "reconcile_due"
	NudgeKindReconcileException = "reconcile_exception"
	NudgeKindShadowWouldBlock   = "shadow_would_block"
	NudgeKindDrawdownLatched    = "drawdown_latched"
	NudgeKindPolicyDrift        = "policy_drift"
	NudgeKindConfirmedFlow      = "confirmed_flow"
	NudgeKindMonthlyPulse       = "monthly_pulse"

	NudgeStateDueSoon  = "due_soon"
	NudgeStateOverdue  = "overdue"
	NudgeStateOpen     = "open"
	NudgeStateObserved = "observed"
	NudgeStateDue      = "due"

	NudgeSeverityWatch = "watch"
	NudgeSeverityAct   = "act"

	NudgeDestinationMonitor = "monitor"
	NudgeDestinationAlerts  = "alerts"
	// NudgeDestinationBrief lands a process/governance nudge on the Brief tab,
	NudgeDestinationBrief = "brief"

	MonthlyPulseStatusNotDue    = "not_due"
	MonthlyPulseStatusCompleted = "completed"
	MonthlyPulseStatusBlocked   = "blocked"
)

// NudgeCandidate is the pure semantic result consumed by daemon adapters. It
// symbol, account, or order fields. CanonicalizeNudgeCandidate replaces Title,
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

// ReconcileDueInput supplies the current instant, reconciliation deadline, and
type ReconcileDueInput struct {
	Now         time.Time
	Deadline    time.Time
	WarningDays *int
}

// EvaluateReconcileDue uses an exact rolling duration. Equality at the
// deadline remains due-soon; overdue begins only when Now.After(Deadline).
func EvaluateReconcileDue(input ReconcileDueInput) *NudgeCandidate {
	if input.Now.IsZero() || input.Deadline.IsZero() || input.WarningDays == nil || *input.WarningDays <= 0 {
		return nil
	}
	warningHorizon := reconcileWarningHorizon(*input.WarningDays)
	state := ""
	if input.Now.After(input.Deadline) {
		state = NudgeStateOverdue
	} else if input.Deadline.Sub(input.Now) <= warningHorizon {
		state = NudgeStateDueSoon
	}
	if state == "" {
		return nil
	}
	occurredAt := input.Deadline.Add(-warningHorizon)
	if state == NudgeStateOverdue {
		occurredAt = input.Deadline
	}
	return newNudgeCandidate(NudgeKindReconcileDue, state, occurredAt, input.Deadline, time.Time{}, struct {
		DeadlineUTC string `json:"deadline_utc"`
		WarningDays int    `json:"warning_days"`
		State       string `json:"state"`
	}{input.Deadline.UTC().Format(time.RFC3339Nano), *input.WarningDays, state})
}

func reconcileWarningHorizon(days int) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	if days > int(maxDuration/(24*time.Hour)) {
		return maxDuration
	}
	return time.Duration(days) * 24 * time.Hour
}

// ReconcileExceptionIdentity contains only the identity/material fields the
type ReconcileExceptionIdentity struct {
	Kind     string
	Identity string
	Material []string
}

// EvaluateReconcileException returns one order-independent candidate for the
func EvaluateReconcileException(unresolved []ReconcileExceptionIdentity, occurredAt time.Time) *NudgeCandidate {
	type normalizedException struct {
		Kind     string   `json:"kind"`
		Identity string   `json:"identity"`
		Material []string `json:"material"`
	}
	rows := make([]normalizedException, 0, len(unresolved))
	for _, unresolved := range unresolved {
		identity := strings.TrimSpace(unresolved.Identity)
		if identity == "" {
			continue
		}
		material := normalizeStrings(unresolved.Material, false)
		rows = append(rows, normalizedException{
			Kind: strings.ToLower(strings.TrimSpace(unresolved.Kind)), Identity: identity, Material: material,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		left, _ := json.Marshal(rows[i])
		right, _ := json.Marshal(rows[j])
		return string(left) < string(right)
	})
	rows = dedupeComparableJSON(rows)
	return newNudgeCandidate(NudgeKindReconcileException, NudgeStateOpen, occurredAt, time.Time{}, time.Time{}, rows)
}

// ShadowWouldBlockInput describes one risk-increasing preview against a stable
type ShadowWouldBlockInput struct {
	PolicyFingerprint string
	LatchEpisode      string
	RiskIncreasing    bool
	Exempt            bool
	WouldBlock        bool
	PriorCount        int
	OccurredAt        time.Time
}

// ShadowWouldBlockEvaluation carries the updated episode count and, only for
type ShadowWouldBlockEvaluation struct {
	Candidate *NudgeCandidate
	Count     int
}

// EvaluateShadowWouldBlock emits one candidate for the first qualifying
func EvaluateShadowWouldBlock(input ShadowWouldBlockInput) ShadowWouldBlockEvaluation {
	count := max(input.PriorCount, 0)
	if !input.RiskIncreasing || input.Exempt || !input.WouldBlock || strings.TrimSpace(input.PolicyFingerprint) == "" || strings.TrimSpace(input.LatchEpisode) == "" {
		return ShadowWouldBlockEvaluation{Count: count}
	}
	count++
	result := ShadowWouldBlockEvaluation{Count: count}
	if count == 1 {
		result.Candidate = newNudgeCandidate(NudgeKindShadowWouldBlock, NudgeStateObserved, input.OccurredAt, time.Time{}, time.Time{}, struct {
			PolicyFingerprint string `json:"policy_fingerprint"`
			LatchEpisode      string `json:"latch_episode"`
		}{strings.TrimSpace(input.PolicyFingerprint), strings.TrimSpace(input.LatchEpisode)})
	}
	return result
}

// EvaluateDrawdownLatched returns an open drawdown candidate for a non-empty
func EvaluateDrawdownLatched(latchEpisode string, open bool, occurredAt time.Time) *NudgeCandidate {
	episode := strings.TrimSpace(latchEpisode)
	if !open || episode == "" {
		return nil
	}
	return newNudgeCandidate(NudgeKindDrawdownLatched, NudgeStateOpen, occurredAt, time.Time{}, time.Time{}, episode)
}

// NudgePinMismatch compares one constitution pin with the corresponding live
type NudgePinMismatch struct {
	Policy        string
	PinnedID      string
	PinnedVersion string
	LiveID        string
	LiveVersion   string
}

// EvaluatePolicyDrift returns one order-independent candidate for valid policy
func EvaluatePolicyDrift(mismatches []NudgePinMismatch, occurredAt time.Time) *NudgeCandidate {
	type normalizedMismatch struct {
		Policy        string `json:"policy"`
		PinnedID      string `json:"pinned_id"`
		PinnedVersion string `json:"pinned_version"`
		LiveID        string `json:"live_id"`
		LiveVersion   string `json:"live_version"`
	}
	rows := make([]normalizedMismatch, 0, len(mismatches))
	for _, mismatch := range mismatches {
		row := normalizedMismatch{
			Policy:   strings.ToLower(strings.TrimSpace(mismatch.Policy)),
			PinnedID: strings.TrimSpace(mismatch.PinnedID), PinnedVersion: strings.TrimSpace(mismatch.PinnedVersion),
			LiveID: strings.TrimSpace(mismatch.LiveID), LiveVersion: strings.TrimSpace(mismatch.LiveVersion),
		}
		if row.Policy == "" || row.PinnedID == "" || row.PinnedVersion == "" || row.LiveID == "" || row.LiveVersion == "" {
			continue
		}
		if row.PinnedID == row.LiveID && row.PinnedVersion == row.LiveVersion {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		left, _ := json.Marshal(rows[i])
		right, _ := json.Marshal(rows[j])
		return string(left) < string(right)
	})
	rows = dedupeComparableJSON(rows)
	return newNudgeCandidate(NudgeKindPolicyDrift, NudgeStateOpen, occurredAt, time.Time{}, time.Time{}, rows)
}

// EvaluateConfirmedFlow returns a candidate keyed by a normalized, non-empty
func EvaluateConfirmedFlow(statementRowIdentity string, occurredAt time.Time) *NudgeCandidate {
	identity := strings.TrimSpace(statementRowIdentity)
	if identity == "" {
		return nil
	}
	return newNudgeCandidate(NudgeKindConfirmedFlow, NudgeStateObserved, occurredAt, time.Time{}, time.Time{}, identity)
}

// MonthlyPulseInput supplies the current instant, approved cadence, policy
type MonthlyPulseInput struct {
	Now               time.Time
	Cadence           ConstitutionCadence
	PolicyFingerprint string
	// PolicyEvidenceReady means current readable policy pins all match. Before
	PolicyEvidenceReady bool
}

// MonthlyPulseEvaluation reports the local policy month, its resolved due
type MonthlyPulseEvaluation struct {
	Status    string
	Month     string
	DueAt     time.Time
	Candidate *NudgeCandidate
}

// EvaluateMonthlyPulse derives the monthly cadence state. A routine pulse
// both present; only missing or invalid evidence returns to the operator.
func EvaluateMonthlyPulse(input MonthlyPulseInput) MonthlyPulseEvaluation {
	zone, day, hour, minute, ok := monthlySchedule(input.Cadence)
	policyFingerprint := strings.TrimSpace(input.PolicyFingerprint)
	if !ok || input.Now.IsZero() || policyFingerprint == "" {
		return MonthlyPulseEvaluation{Status: MonthlyPulseStatusBlocked}
	}
	localNow := input.Now.In(zone)
	month := localNow.Format("2006-01")
	dueAt, unique := resolveUniqueLocalInstant(zone, localNow.Year(), localNow.Month(),
		nthWorkingDayOfMonth(localNow.Year(), localNow.Month(), day), hour, minute)
	if !unique {
		return MonthlyPulseEvaluation{Status: MonthlyPulseStatusBlocked, Month: month}
	}
	result := MonthlyPulseEvaluation{Month: month, DueAt: dueAt}
	if input.Now.Before(dueAt) {
		result.Status = MonthlyPulseStatusNotDue
		return result
	}
	if !input.PolicyEvidenceReady {
		result.Status = MonthlyPulseStatusBlocked
		result.Candidate = newNudgeCandidate(NudgeKindMonthlyPulse, NudgeStateOpen, dueAt, dueAt, time.Time{}, struct {
			Month             string `json:"month"`
			PolicyFingerprint string `json:"policy_fingerprint"`
		}{month, policyFingerprint})
		return result
	}
	result.Status = MonthlyPulseStatusCompleted
	return result
}

// monthlySchedule returns the pulse clock, the working-day index (not a
// value defaults in code; an authored override that is invalid fails the
func monthlySchedule(cadence ConstitutionCadence) (*time.Location, int, int, int, bool) {
	if cadence.Monthly != nil && cadence.Monthly.Class != nil && *cadence.Monthly.Class != EnforcementAdvisory {
		return nil, 0, 0, 0, false
	}
	day := cadence.ResolvedMonthlyWorkingDay()
	if day < 1 || day > 20 {
		return nil, 0, 0, 0, false
	}
	zone, err := cadence.NudgeLocation()
	if err != nil {
		return nil, 0, 0, 0, false
	}
	atLocal := cadence.ResolvedMonthlyNudgeAtLocal()
	parsed, err := time.Parse("15:04", atLocal)
	if err != nil || parsed.Format("15:04") != atLocal {
		return nil, 0, 0, 0, false
	}
	return zone, day, parsed.Hour(), parsed.Minute(), true
}

// nthWorkingDayOfMonth maps a working-day index to its calendar day. Working
// resolveUniqueLocalInstant then fails closed on.
func nthWorkingDayOfMonth(year int, month time.Month, n int) int {
	count := 0
	for day := 1; day <= 31; day++ {
		t := time.Date(year, month, day, 12, 0, 0, 0, time.UTC)
		if t.Month() != month {
			break
		}
		if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday {
			count++
			if count == n {
				return day
			}
		}
	}
	return 0
}

// resolveUniqueLocalInstant maps a local wall clock to exactly one instant.
// fall-back folds have multiple matches; both fail closed.
func resolveUniqueLocalInstant(location *time.Location, year int, month time.Month, day, hour, minute int) (time.Time, bool) {
	wall := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	offsets := make(map[int]struct{})
	for delta := -48 * time.Hour; delta <= 48*time.Hour; delta += 30 * time.Minute {
		_, offset := wall.Add(delta).In(location).Zone()
		offsets[offset] = struct{}{}
	}
	matches := make([]time.Time, 0, len(offsets))
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(location)
		if local.Year() != year || local.Month() != month || local.Day() != day || local.Hour() != hour || local.Minute() != minute || local.Second() != 0 || local.Nanosecond() != 0 {
			continue
		}
		duplicate := false
		for _, match := range matches {
			if match.Equal(candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return time.Time{}, false
	}
	return matches[0], true
}

func newNudgeCandidate(kind, state string, occurredAt, dueAt, expiresAt time.Time, identity any) *NudgeCandidate {
	title, body, severity := candidateTemplate(kind, state)
	if title == "" {
		return nil
	}
	// Process-kind nudges land on the Brief tab (the process artifact); only
	destination := NudgeDestinationBrief
	if severity == NudgeSeverityAct {
		destination = NudgeDestinationAlerts
	}
	return &NudgeCandidate{
		Fingerprint: semanticNudgeFingerprint(kind, identity), Kind: kind, State: state,
		Severity: severity, Title: title, Body: body, OccurredAt: occurredAt,
		DueAt: dueAt, ExpiresAt: expiresAt, Destination: destination,
	}
}

func candidateTemplate(kind, state string) (title, body, severity string) {
	switch kind {
	case NudgeKindReconcileDue:
		if state == NudgeStateDueSoon {
			return "Daily broker check due soon", "Open Process checks before the due date.", NudgeSeverityWatch
		}
		if state == NudgeStateOverdue {
			return "Daily broker check overdue", "Open Process checks and resolve what is waiting.", NudgeSeverityAct
		}
	case NudgeKindReconcileException:
		if state == NudgeStateOpen {
			return "Broker report needs your review", "One or more broker entries could not be cleared automatically.", NudgeSeverityAct
		}
	case NudgeKindShadowWouldBlock:
		if state == NudgeStateObserved {
			return "A planned trade would have been blocked", "Review the risk warning before placing a similar trade.", NudgeSeverityAct
		}
	case NudgeKindDrawdownLatched:
		if state == NudgeStateOpen {
			return "The drawdown warning remains active", "This is a warning only; Canary has not blocked trading. Review the risk screen before increasing risk.", NudgeSeverityAct
		}
	case NudgeKindPolicyDrift:
		if state == NudgeStateOpen {
			return "Risk settings changed", "Review the changed settings before relying on reminders.", NudgeSeverityAct
		}
	case NudgeKindConfirmedFlow:
		if state == NudgeStateObserved {
			return "New cash movement found", "A broker statement contains a new deposit or withdrawal.", NudgeSeverityWatch
		}
	case NudgeKindMonthlyPulse:
		if state == NudgeStateOpen {
			return "Monthly process evidence needs review", "Current policy evidence could not complete the monthly process automatically.", NudgeSeverityAct
		}
	}
	return "", "", ""
}

// CanonicalizeNudgeCandidate validates the narrow candidate contract and
func CanonicalizeNudgeCandidate(candidate NudgeCandidate) (NudgeCandidate, error) {
	title, body, severity := candidateTemplate(candidate.Kind, candidate.State)
	if title == "" {
		return NudgeCandidate{}, errors.New("invalid nudge candidate kind or state")
	}
	if !validNudgeFingerprint(candidate.Fingerprint) {
		return NudgeCandidate{}, errors.New("invalid nudge candidate fingerprint")
	}
	if candidate.OccurredAt.IsZero() {
		return NudgeCandidate{}, errors.New("invalid nudge candidate occurrence time")
	}
	if !candidate.ExpiresAt.IsZero() {
		return NudgeCandidate{}, errors.New("invalid nudge candidate expiry time")
	}

	switch {
	case candidate.Kind == NudgeKindReconcileDue && candidate.State == NudgeStateDueSoon:
		if candidate.DueAt.IsZero() || candidate.OccurredAt.After(candidate.DueAt) {
			return NudgeCandidate{}, errors.New("invalid reconcile due-soon timestamps")
		}
	case candidate.Kind == NudgeKindReconcileDue && candidate.State == NudgeStateOverdue:
		if candidate.DueAt.IsZero() || !candidate.OccurredAt.Equal(candidate.DueAt) {
			return NudgeCandidate{}, errors.New("invalid reconcile overdue timestamps")
		}
	case candidate.Kind == NudgeKindMonthlyPulse && candidate.State == NudgeStateOpen:
		if candidate.DueAt.IsZero() || !candidate.OccurredAt.Equal(candidate.DueAt) {
			return NudgeCandidate{}, errors.New("invalid monthly pulse timestamps")
		}
	default:
		if !candidate.DueAt.IsZero() {
			return NudgeCandidate{}, errors.New("invalid nudge candidate due time")
		}
	}

	candidate.Title = title
	candidate.Body = body
	candidate.Severity = severity
	candidate.Destination = NudgeDestinationBrief
	if severity == NudgeSeverityAct {
		candidate.Destination = NudgeDestinationAlerts
	}
	return candidate, nil
}

func validNudgeFingerprint(fingerprint string) bool {
	const prefix = "sha256:"
	if len(fingerprint) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(fingerprint, prefix) {
		return false
	}
	for i := len(prefix); i < len(fingerprint); i++ {
		if (fingerprint[i] < '0' || fingerprint[i] > '9') && (fingerprint[i] < 'a' || fingerprint[i] > 'f') {
			return false
		}
	}
	return true
}

func semanticNudgeFingerprint(kind string, identity any) string {
	raw, _ := json.Marshal(struct {
		Kind     string `json:"kind"`
		Identity any    `json:"identity"`
	}{kind, identity})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizeStrings(values []string, lower bool) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lower {
			value = strings.ToLower(value)
		}
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	sort.Strings(normalized)
	if len(normalized) < 2 {
		return normalized
	}
	out := normalized[:1]
	for _, value := range normalized[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func dedupeComparableJSON[T any](values []T) []T {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	previous, _ := json.Marshal(values[0])
	for _, value := range values[1:] {
		current, _ := json.Marshal(value)
		if string(current) != string(previous) {
			out = append(out, value)
			previous = current
		}
	}
	return out
}
