package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	governanceNudgeStateFile    = "governance-nudges-state.json"
	governanceNudgeStateVersion = 1
)

// nudgeStateFileV1 contains only opaque identities and allowlisted lifecycle
// facts. Broker/account/report/line identities, amounts, symbols, prose,
// paths, and tokens never cross this persistence boundary.
type nudgeStateFileV1 struct {
	Version           int                          `json:"version"`
	Shadow            nudgeShadowEpisodeState      `json:"shadow"`
	ConfirmedCoverage *nudgeConfirmedCoverageState `json:"confirmed_coverage,omitempty"`
	ConfirmedEvents   []nudgeConfirmedEventState   `json:"confirmed_events,omitempty"`
}

type nudgeShadowEpisodeState struct {
	PolicyIdentity string    `json:"policy_identity,omitempty"`
	LatchEpisode   string    `json:"latch_episode,omitempty"`
	OccurredAt     time.Time `json:"occurred_at,omitzero"`
	Count          int       `json:"count,omitempty"`
}

type nudgeConfirmedCoverageState struct {
	CoverageFrom          time.Time `json:"coverage_from"`
	ReportIdentity        string    `json:"report_identity"`
	CoveredRowCount       int       `json:"covered_row_count"`
	CurrentReportIdentity string    `json:"current_report_identity,omitempty"`
	CurrentRowCount       int       `json:"current_row_count,omitempty"`
	CurrentRowsObserved   bool      `json:"current_rows_observed,omitempty"`
	KnownRows             []string  `json:"known_rows,omitempty"`
	CurrentRows           []string  `json:"current_rows,omitempty"`
}

type nudgeConfirmedEventState struct {
	ContentIdentity string    `json:"content_identity"`
	OccurredAt      time.Time `json:"occurred_at"`
	Superseded      bool      `json:"superseded,omitempty"`
}

type nudgeStateStore struct {
	mu       sync.Mutex
	path     string // legacy importer/test helper only
	core     *corestore.Store
	revision int64
	now      func() time.Time
	// writeState is a test seam for atomic-write/rename failures. Production
	// uses writePrivateStateAtomic.
	writeState func(string, []byte) error
	loaded     bool
	loadErr    bool
	fault      bool
	state      nudgeStateFileV1
	committed  nudgeStateFileV1
}

type nudgeConfirmedFlowSnapshot struct {
	PolicyVersion     int
	PolicyIdentity    string
	ReportStatus      string
	ReportIdentity    string
	StatementAsOf     time.Time
	StatementsHealthy bool
	ConfirmedRows     []string
}

func (s *Server) installNudgeStateStore() {
	if s == nil {
		return
	}
	path, err := defaultTradingStatePath(governanceNudgeStateFile)
	if err != nil {
		s.warnf("governance nudges: resolve state path: %v (durable one-shot facts unavailable)", err)
	}
	s.nudges = &nudgeStateStore{path: path, now: s.now}
	if s.riskCapital != nil {
		s.riskCapital.nudges = s.nudges
		s.riskCapital.observeConfirmedFlows = s.observeConfirmedFlows
	}
}

func (st *nudgeStateStore) bindCore(ctx context.Context, core *corestore.Store) error {
	if st == nil || core == nil {
		return fmt.Errorf("governance nudge SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindNudges)
	if err != nil {
		return fmt.Errorf("load governance nudge state from SQLite: %w", err)
	}
	state := nudgeStateFileV1{Version: governanceNudgeStateVersion}
	revision := int64(0)
	if ok {
		if err := json.Unmarshal(doc.JSON, &state); err != nil || state.Version != governanceNudgeStateVersion {
			if err == nil {
				err = fmt.Errorf("unsupported version %d", state.Version)
			}
			return fmt.Errorf("decode governance nudge state from SQLite: %w", err)
		}
		normalizeNudgeState(&state)
		revision = doc.Revision
	} else {
		return fmt.Errorf("governance nudge state is missing from SQLite; initialization was not completed")
	}
	st.mu.Lock()
	st.core, st.revision, st.loaded = core, revision, true
	st.loadErr, st.fault = false, false
	st.state = cloneNudgeState(state)
	st.committed = cloneNudgeState(state)
	st.mu.Unlock()
	return nil
}

func normalizeNudgeState(persisted *nudgeStateFileV1) {
	if persisted == nil || persisted.ConfirmedCoverage == nil {
		return
	}
	if persisted.ConfirmedCoverage.CurrentReportIdentity == "" {
		persisted.ConfirmedCoverage.CurrentReportIdentity = persisted.ConfirmedCoverage.ReportIdentity
	}
	if !persisted.ConfirmedCoverage.CurrentRowsObserved {
		if persisted.ConfirmedCoverage.CurrentRowCount == 0 && persisted.ConfirmedCoverage.CoveredRowCount > 0 {
			persisted.ConfirmedCoverage.CurrentRowCount = persisted.ConfirmedCoverage.CoveredRowCount
		}
		if len(persisted.ConfirmedCoverage.CurrentRows) == 0 {
			persisted.ConfirmedCoverage.CurrentRows = normalizeOpaqueIdentities(persisted.ConfirmedCoverage.KnownRows)
		}
		persisted.ConfirmedCoverage.CurrentRowsObserved = true
	}
}

func (st *nudgeStateStore) loadLocked() {
	if st.loaded {
		return
	}
	st.loaded = true
	st.state = nudgeStateFileV1{Version: governanceNudgeStateVersion}
	st.committed = cloneNudgeState(st.state)
	if strings.TrimSpace(st.path) == "" {
		st.loadErr = true
		return
	}
	raw, err := os.ReadFile(st.path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		st.loadErr = true
		return
	}
	var persisted nudgeStateFileV1
	if json.Unmarshal(raw, &persisted) != nil || persisted.Version != governanceNudgeStateVersion {
		st.loadErr = true
		return
	}
	normalizeNudgeState(&persisted)
	st.state = persisted
	st.committed = cloneNudgeState(persisted)
}

func (st *nudgeStateStore) persistLocked() error {
	if st.loadErr || (st.core == nil && strings.TrimSpace(st.path) == "") {
		st.fault = true
		st.state = cloneNudgeState(st.committed)
		return fmt.Errorf("governance nudge persistence is unavailable")
	}
	st.state.Version = governanceNudgeStateVersion
	raw, err := json.Marshal(st.state)
	if err != nil {
		st.fault = true
		st.state = cloneNudgeState(st.committed)
		return err
	}
	if st.core != nil {
		saved, err := st.core.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: stateKindNudges,
			ExpectedRevision: st.revision, JSON: raw,
		})
		if err != nil {
			st.fault = true
			st.state = cloneNudgeState(st.committed)
			return err
		}
		st.revision = saved.Revision
		st.committed = cloneNudgeState(st.state)
		st.fault = false
		return nil
	}
	writeState := st.writeState
	if writeState == nil {
		writeState = writePrivateStateAtomic
	}
	if err := writeState(st.path, raw); err != nil {
		st.fault = true
		st.state = cloneNudgeState(st.committed)
		return err
	}
	st.committed = cloneNudgeState(st.state)
	st.fault = false
	return nil
}

func (st *nudgeStateStore) healthOK() bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return !st.loadErr && !st.fault && (st.core == nil || st.core.Health().Ready)
}

func (st *nudgeStateStore) recordShadow(policyIdentity, latchEpisode string, riskIncreasing, exempt, wouldBlock bool) error {
	if st == nil {
		return fmt.Errorf("governance nudge persistence is unavailable")
	}
	policyIdentity = strings.TrimSpace(policyIdentity)
	latchEpisode = strings.TrimSpace(latchEpisode)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.loadErr {
		return fmt.Errorf("governance nudge persistence is unavailable")
	}
	before := cloneNudgeState(st.state)
	occurredAt := time.Now().UTC()
	if st.now != nil {
		occurredAt = st.now().UTC()
	}
	prior := 0
	if st.state.Shadow.PolicyIdentity == policyIdentity && st.state.Shadow.LatchEpisode == latchEpisode {
		prior = st.state.Shadow.Count
	}
	evaluated := risk.EvaluateShadowWouldBlock(risk.ShadowWouldBlockInput{
		PolicyFingerprint: policyIdentity,
		LatchEpisode:      latchEpisode,
		RiskIncreasing:    riskIncreasing,
		Exempt:            exempt,
		WouldBlock:        wouldBlock,
		PriorCount:        prior,
		OccurredAt:        occurredAt,
	})
	if evaluated.Count == prior {
		return nil
	}
	if prior == 0 {
		st.state.Shadow = nudgeShadowEpisodeState{
			PolicyIdentity: policyIdentity,
			LatchEpisode:   latchEpisode,
			OccurredAt:     occurredAt.UTC(),
			Count:          evaluated.Count,
		}
	} else {
		st.state.Shadow.Count = evaluated.Count
	}
	if err := st.persistLocked(); err != nil {
		st.state = before
		return err
	}
	return nil
}

func (st *nudgeStateStore) shadowObservation(policyIdentity, latchEpisode string, open bool) (*risk.NudgeCandidate, int) {
	if st == nil || !open {
		return nil, 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	shadow := st.state.Shadow
	if st.loadErr || st.fault || shadow.Count <= 0 || shadow.PolicyIdentity != policyIdentity || shadow.LatchEpisode != latchEpisode {
		return nil, 0
	}
	return risk.EvaluateShadowWouldBlock(risk.ShadowWouldBlockInput{
		PolicyFingerprint: shadow.PolicyIdentity,
		LatchEpisode:      shadow.LatchEpisode,
		RiskIncreasing:    true,
		WouldBlock:        true,
		OccurredAt:        shadow.OccurredAt,
	}).Candidate, shadow.Count
}

// observeConfirmedFlows is called only from the successful retained-statement
// incorporation path. The first v4 observation creates a coverage watermark
// and baselines existing rows without creating a historical notification
// flood. Later content identities become durable one-shot facts.
func (st *nudgeStateStore) observeConfirmedFlows(snapshot nudgeConfirmedFlowSnapshot) error {
	if st == nil || snapshot.PolicyVersion < 4 || strings.TrimSpace(snapshot.ReportIdentity) == "" {
		return nil
	}
	now := time.Now().UTC()
	if st.now != nil {
		now = st.now().UTC()
	}
	rows := normalizeOpaqueIdentities(snapshot.ConfirmedRows)
	current := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		current[row] = struct{}{}
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	before := cloneNudgeState(st.state)
	if st.loadErr {
		return fmt.Errorf("governance nudge persistence is unavailable")
	}
	if st.state.ConfirmedCoverage == nil {
		st.state.ConfirmedCoverage = &nudgeConfirmedCoverageState{
			CoverageFrom:          now,
			ReportIdentity:        snapshot.ReportIdentity,
			CoveredRowCount:       len(rows),
			CurrentReportIdentity: snapshot.ReportIdentity,
			CurrentRowCount:       len(rows),
			CurrentRowsObserved:   true,
			KnownRows:             rows,
			CurrentRows:           rows,
		}
		if err := st.persistLocked(); err != nil {
			st.state = before
			return err
		}
		return nil
	}

	coverage := st.state.ConfirmedCoverage
	coverage.CurrentReportIdentity = snapshot.ReportIdentity
	coverage.CurrentRowCount = len(rows)
	coverage.CurrentRows = rows
	coverage.CurrentRowsObserved = true
	known := make(map[string]struct{}, len(coverage.KnownRows))
	for _, row := range coverage.KnownRows {
		known[row] = struct{}{}
	}
	eventByID := make(map[string]int, len(st.state.ConfirmedEvents))
	for i := range st.state.ConfirmedEvents {
		event := &st.state.ConfirmedEvents[i]
		eventByID[event.ContentIdentity] = i
		_, event.Superseded = current[event.ContentIdentity]
		event.Superseded = !event.Superseded
	}
	for _, row := range rows {
		if idx, exists := eventByID[row]; exists {
			st.state.ConfirmedEvents[idx].Superseded = false
		}
		if _, seen := known[row]; seen {
			continue
		}
		known[row] = struct{}{}
		coverage.KnownRows = append(coverage.KnownRows, row)
		st.state.ConfirmedEvents = append(st.state.ConfirmedEvents, nudgeConfirmedEventState{
			ContentIdentity: row,
			OccurredAt:      now,
		})
	}
	coverage.KnownRows = normalizeOpaqueIdentities(coverage.KnownRows)
	if err := st.persistLocked(); err != nil {
		st.state = before
		return err
	}
	return nil
}

func cloneNudgeState(state nudgeStateFileV1) nudgeStateFileV1 {
	cloned := state
	if state.ConfirmedCoverage != nil {
		coverage := *state.ConfirmedCoverage
		coverage.KnownRows = append([]string(nil), state.ConfirmedCoverage.KnownRows...)
		coverage.CurrentRows = append([]string(nil), state.ConfirmedCoverage.CurrentRows...)
		cloned.ConfirmedCoverage = &coverage
	}
	cloned.ConfirmedEvents = append([]nudgeConfirmedEventState(nil), state.ConfirmedEvents...)
	return cloned
}

func (st *nudgeStateStore) confirmedSnapshotContext(ctx context.Context, currentRows []string) (*rpc.NudgeConfirmedFlowCoverage, []nudgeConfirmedEventState, bool, error) {
	if st == nil {
		return nil, nil, false, nil
	}
	current := make(map[string]struct{}, len(currentRows))
	for _, row := range currentRows {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		current[row] = struct{}{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.loadErr || st.fault || st.state.ConfirmedCoverage == nil {
		return nil, nil, false, nil
	}
	coverageState := st.state.ConfirmedCoverage
	coverage := &rpc.NudgeConfirmedFlowCoverage{
		CoverageFrom: coverageState.CoverageFrom,
	}
	events := make([]nudgeConfirmedEventState, 0, len(st.state.ConfirmedEvents))
	for _, event := range st.state.ConfirmedEvents {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		if event.Superseded {
			continue
		}
		if _, stillCurrent := current[event.ContentIdentity]; !stillCurrent {
			continue
		}
		events = append(events, event)
	}
	return coverage, events, true, nil
}

func normalizeOpaqueIdentities(values []string) []string {
	values = append([]string(nil), values...)
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if value == "" || (len(out) > 0 && out[len(out)-1] == value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func opaqueIdentity(domain string, values ...string) string {
	h := sha256.New()
	h.Write([]byte(strings.TrimSpace(domain)))
	for _, value := range values {
		h.Write([]byte{0})
		h.Write([]byte(strings.TrimSpace(value)))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func nudgePolicyIdentity(c *risk.Constitution) string {
	if c == nil {
		return ""
	}
	return opaqueIdentity("risk-policy", c.FingerprintKey())
}

type nudgeAuthorityState struct {
	policy         *risk.Constitution
	scope          brokerStateScope
	report         rpc.RiskPolicyResult
	policyIdentity string
	policyHealth   rpc.NudgeInputHealth
	loadedAt       time.Time
	pinsReadable   bool
	// eligible is the current, validated base policy authority used by
	// version-independent facts (policy drift, capital latch/shadow, and
	// reconciliation exceptions). Cadence and confirmed-flow features have
	// narrower gates so a missing v4 reminder field cannot suppress an
	// unrelated risk fact.
	eligible              bool
	cadenceEligible       bool
	confirmedFlowEligible bool
	capitalNudge          riskCapitalNudgeSnapshot
}

func (s *Server) governanceMonthlyPulseForAuthority(authority nudgeAuthorityState, constitution *risk.Constitution, _ *rpc.ReconResult, now time.Time) risk.MonthlyPulseEvaluation {
	if constitution == nil || constitution.PolicyVersion < 4 {
		return risk.MonthlyPulseEvaluation{}
	}
	identity := nudgePolicyIdentity(constitution)
	return risk.EvaluateMonthlyPulse(risk.MonthlyPulseInput{
		Now: now, Cadence: constitution.Cadence, PolicyFingerprint: identity,
		PolicyEvidenceReady: authority.cadenceEligible && authority.policyIdentity == identity && policyPinsReady(authority.report.Inventory),
	})
}

// currentNudgeAuthority builds the governance view from the daemon's current
// manager state in one read. A retained last-good constitution is not current
// authority when the file is absent, errored, drifted, stale, or internally
// inconsistent.
func (s *Server) currentNudgeAuthority(now time.Time) nudgeAuthorityState {
	now = now.UTC()
	unavailable := rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusUnavailable, Reason: rpc.NudgeHealthReasonSourceUnavailable, AsOf: now}
	state := nudgeAuthorityState{policyHealth: unavailable}
	if s == nil || s.riskPolicies == nil {
		return state
	}
	state.scope = s.currentBrokerStateScope()

	m := s.riskPolicies
	m.mu.Lock()
	mgr := riskPolicySnapshot{
		policy: m.active, status: m.status, source: m.source, path: m.path,
		message: m.message, loadedAt: m.loadedAt, lastCheckedAt: m.lastCheckedAt,
	}
	lastFingerprint := m.lastFingerprint
	reloadInterval := m.reloadInterval
	m.mu.Unlock()

	state.policy = mgr.policy
	state.loadedAt = mgr.loadedAt.UTC()
	state.report = rpc.RiskPolicyResult{
		AsOf: now, Status: mgr.status, Source: mgr.source, Path: mgr.path, Message: mgr.message,
	}
	if mgr.policy != nil {
		state.report.PolicyID = mgr.policy.PolicyID
		state.report.PolicyVersion = mgr.policy.PolicyVersion
		state.report.Unapproved = mgr.policy.UnapprovedKeys()
		state.report.Inventory = s.riskPolicyInventory(mgr.policy)
		if s.riskCapital != nil {
			state.capitalNudge = s.riskCapital.NudgeSnapshotForScope(mgr.policy, nil, state.scope)
			state.report.Capital = state.capitalNudge.Report
		}
		state.report.PolicyFingerprint = &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: mgr.policy.FingerprintKey()}
		state.policyIdentity = nudgePolicyIdentity(mgr.policy)
	}

	healthAt := mgr.lastCheckedAt.UTC()
	if healthAt.IsZero() || healthAt.After(now) {
		healthAt = now
	}
	setHealth := func(status, reason string) {
		state.policyHealth = rpc.NudgeInputHealth{Status: status, Reason: reason, AsOf: healthAt}
	}
	switch mgr.status {
	case rpc.RiskPolicyStatusAbsent:
		setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonSourceUnavailable)
		return state
	case rpc.RiskPolicyStatusDrift:
		setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonPolicyUnapproved)
		return state
	case rpc.RiskPolicyStatusError:
		setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
		return state
	case rpc.RiskPolicyStatusActive:
	default:
		setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonSourceUnavailable)
		return state
	}
	if mgr.source != "file" || mgr.policy == nil {
		setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonSourceUnavailable)
		return state
	}
	if mgr.loadedAt.IsZero() || mgr.loadedAt.After(now) || mgr.lastCheckedAt.After(now) {
		setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
		return state
	}
	freshFor := max(2*reloadInterval, time.Minute)
	if mgr.lastCheckedAt.IsZero() || now.Sub(mgr.lastCheckedAt) > freshFor {
		setHealth(rpc.NudgeInputStatusStale, rpc.NudgeHealthReasonEvidenceStale)
		return state
	}
	baseUnapproved := false
	for _, key := range state.report.Unapproved {
		if !strings.HasPrefix(key, "cadence.nudges.") && !strings.HasPrefix(key, "cadence.monthly.") {
			baseUnapproved = true
			break
		}
	}
	if baseUnapproved {
		setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonPolicyUnapproved)
		return state
	}
	if err := mgr.policy.Validate(); err != nil || mgr.policy.FingerprintKey() == "" || mgr.policy.FingerprintKey() != lastFingerprint {
		setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
		return state
	}
	state.pinsReadable = policyPinsReadable(state.report.Inventory, false)
	if mgr.policy.PolicyVersion < 3 || mgr.policy.PolicyVersion > 4 {
		setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonPolicyUnapproved)
		return state
	}
	setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
	state.eligible = true
	state.confirmedFlowEligible = mgr.policy.PolicyVersion == 4
	// Cadence keys all default in code (machine timezone, code schedule);
	// authored overrides are validated at load, so a healthy v4 policy is
	// always cadence-eligible.
	state.cadenceEligible = state.confirmedFlowEligible && len(state.report.Unapproved) == 0
	return state
}

// observeConfirmedFlows is the governance-only adapter around successful
// capital incorporation. Capital truth remains installed regardless of this
// advisory check; coverage advances only under current healthy v4 authority
// and fresh, fully healthy broker-backed statement evidence.
func (s *Server) observeConfirmedFlows(snapshot nudgeConfirmedFlowSnapshot) {
	if s == nil || s.nudges == nil {
		return
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	authority := s.currentNudgeAuthority(now)
	if !authority.confirmedFlowEligible || authority.report.PolicyVersion != 4 || snapshot.PolicyVersion != 4 ||
		snapshot.PolicyIdentity != authority.policyIdentity || snapshot.ReportStatus != rpc.ReconStatusActive ||
		!snapshot.StatementsHealthy || strings.TrimSpace(snapshot.ReportIdentity) == "" || snapshot.StatementAsOf.IsZero() ||
		snapshot.StatementAsOf.After(now) || reconReportStale(authority.policy, &rpc.ReconResult{StatementAsOf: snapshot.StatementAsOf}, now) {
		return
	}
	_ = s.nudges.observeConfirmedFlows(snapshot)
}

func confirmedFlowContentIdentity(row rpc.ReconException) string {
	amount := ""
	if row.AmountBase != nil {
		amount = strconv.FormatFloat(*row.AmountBase, 'g', -1, 64)
	}
	return opaqueIdentity("confirmed-flow", row.LineID, row.Category, row.Type,
		row.Description, row.ValueDate.UTC().Format(time.RFC3339Nano), amount)
}

func (s *Server) handleNudgesSnapshot(ctx context.Context, req *rpc.Request) (*rpc.NudgesSnapshotResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Params) > 0 {
		var p rpc.NudgesSnapshotParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	var shadowInput alertShadowNudgeInput
	result, err := s.composeNudgesSnapshotContextWithAuthority(ctx, &shadowInput)
	if err != nil {
		return nil, err
	}
	// The custom RPC marshal is a mandatory safety boundary: it validates
	// timestamps/coverage and replaces every display field with canonical copy.
	wire, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal nudge snapshot: %w", err)
	}
	var canonical rpc.NudgesSnapshotResult
	if err := json.Unmarshal(wire, &canonical); err != nil {
		return nil, fmt.Errorf("decode canonical nudge snapshot: %w", err)
	}
	shadowInput.Snapshot = canonical
	s.observeNudgesAlertShadow(ctx, shadowInput)
	return &canonical, nil
}

// composeNudgesSnapshotContextWithAuthority captures the exact policy,
// nudge-store health, and broker scope used by this composition for the
// source-neutral alert producer. This avoids later independent reads racing a
// policy reload, persistence fault, or account/mode transition after the
// snapshot was built.
func (s *Server) composeNudgesSnapshotContextWithAuthority(ctx context.Context, shadowInput *alertShadowNudgeInput) (rpc.NudgesSnapshotResult, error) {
	now := time.Now().UTC()
	if s != nil && s.now != nil {
		now = s.now().UTC()
	}
	result := rpc.NudgesSnapshotResult{AsOf: now, Candidates: []rpc.NudgeCandidate{}}
	setHealth := func(status, reason string) rpc.NudgeInputHealth {
		return rpc.NudgeInputHealth{Status: status, Reason: reason, AsOf: now}
	}
	unavailable := setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonSourceUnavailable)
	result.SourceHealth = rpc.NudgeSourceHealth{
		Policy: unavailable, Reconciliation: unavailable, Capital: unavailable,
		Pins: unavailable, Cadence: unavailable,
		ConfirmedFlow: setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonCoverageUnavailable),
	}
	authority := s.currentNudgeAuthority(now)
	if shadowInput != nil {
		storeHealth := setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonSourceUnavailable)
		if s != nil && s.nudges != nil {
			if s.nudges.healthOK() {
				storeHealth = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
			} else {
				storeHealth = setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
			}
		}
		scope, _ := newAlertShadowBrokerScope(s.currentBrokerStateScope())
		*shadowInput = alertShadowNudgeInput{
			PolicyFingerprint: rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: authority.policyIdentity},
			StoreHealth:       storeHealth,
			Scope:             scope,
		}
	}
	result.SourceHealth.Policy = authority.policyHealth
	if authority.policyHealth.Status == rpc.NudgeInputStatusInactive {
		inactive := setHealth(rpc.NudgeInputStatusInactive, rpc.NudgeHealthReasonProcessRemindersNotEnabled)
		result.SourceHealth.Cadence = inactive
		result.SourceHealth.ConfirmedFlow = inactive
		if authority.pinsReadable {
			result.SourceHealth.Pins = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
		}
		capital := authority.report.Capital
		if s.riskCapital != nil {
			switch {
			case capital.Tier == risk.CapitalTierUnapproved:
				result.SourceHealth.Capital = setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonPolicyUnapproved)
			case capital.EquityAsOf.IsZero():
				result.SourceHealth.Capital = unavailable
			case capital.EquityStale:
				result.SourceHealth.Capital = setHealth(rpc.NudgeInputStatusStale, rpc.NudgeHealthReasonEvidenceStale)
			default:
				result.SourceHealth.Capital = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
			}
			report, err := s.buildReconReportContext(ctx)
			if err != nil {
				return rpc.NudgesSnapshotResult{}, err
			}
			result.Reconciliation = &report.Automation
			switch report.Status {
			case rpc.ReconStatusActive:
				if reconReportStale(authority.policy, report, now) {
					result.SourceHealth.Reconciliation = setHealth(rpc.NudgeInputStatusStale, rpc.NudgeHealthReasonEvidenceStale)
				} else {
					result.SourceHealth.Reconciliation = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
				}
			case rpc.ReconStatusUnapproved:
				result.SourceHealth.Reconciliation = setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonPolicyUnapproved)
			case rpc.ReconStatusUnavailable:
				result.SourceHealth.Reconciliation = unavailable
			default:
				result.SourceHealth.Reconciliation = setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
			}
		}
		return result, nil
	}
	if authority.policyHealth.Status != rpc.NudgeInputStatusOK {
		return result, nil
	}
	policy := authority.policy
	inventory := authority.report.Inventory
	var mismatches []risk.NudgePinMismatch
	for _, pin := range inventory {
		switch pin.Status {
		case "match":
		case "drift":
			mismatches = append(mismatches, risk.NudgePinMismatch{
				Policy: pin.Policy, PinnedID: pin.PinnedID, PinnedVersion: pin.PinnedVersion,
				LiveID: pin.LiveID, LiveVersion: pin.LiveVersion,
			})
		}
	}
	if authority.pinsReadable {
		result.SourceHealth.Pins = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
	} else {
		result.SourceHealth.Pins = setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonSourceUnavailable)
	}
	if authority.pinsReadable {
		if candidate := risk.EvaluatePolicyDrift(mismatches, stableNudgeTime(authority.loadedAt, now)); candidate != nil {
			result.Candidates = append(result.Candidates, rpcNudgeCandidate(candidate))
		}
	}

	policyIdentity := authority.policyIdentity
	switch {
	case policy.PolicyVersion < 4:
		result.SourceHealth.Cadence = setHealth(rpc.NudgeInputStatusInactive, rpc.NudgeHealthReasonProcessRemindersNotEnabled)
	case !authority.cadenceEligible:
		result.SourceHealth.Cadence = setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonCadenceUnapproved)
	default:
		result.SourceHealth.Cadence = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
	}

	capital := authority.report.Capital
	if s.riskCapital != nil {
		switch {
		case capital.Tier == risk.CapitalTierUnapproved:
			result.SourceHealth.Capital = setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonPolicyUnapproved)
		case capital.EquityAsOf.IsZero():
			result.SourceHealth.Capital = unavailable
		case capital.EquityStale:
			result.SourceHealth.Capital = setHealth(rpc.NudgeInputStatusStale, rpc.NudgeHealthReasonEvidenceStale)
		default:
			result.SourceHealth.Capital = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
		}
		latchOpen, latchEpisode, latchedAt := authority.capitalNudge.LatchOpen, authority.capitalNudge.Episode, authority.capitalNudge.OccurredAt
		if candidate := risk.EvaluateDrawdownLatched(latchEpisode, latchOpen, latchedAt); candidate != nil {
			result.Candidates = append(result.Candidates, rpcNudgeCandidate(candidate))
			if result.Context == nil {
				result.Context = &rpc.NudgeSnapshotContext{}
			}
			result.Context.Drawdown = &rpc.NudgeDrawdownSummary{Tier: rpc.NudgeDrawdownTierBlock, ConsumedPct: capital.ConsumedPct}
		}
		if s.nudges != nil {
			if candidate, count := s.nudges.shadowObservation(policyIdentity, latchEpisode, latchOpen); candidate != nil {
				result.Candidates = append(result.Candidates, rpcNudgeCandidate(candidate))
				if result.Context == nil {
					result.Context = &rpc.NudgeSnapshotContext{}
				}
				result.Context.Shadow = &rpc.NudgeShadowSummary{Count: count}
			}
		}
		clock := s.riskCapital.UnreconciledClockForScope(policy, now, authority.scope)
		if authority.cadenceEligible && clock.Approved && !clock.Deadline.IsZero() {
			if candidate := risk.EvaluateReconcileDue(risk.ReconcileDueInput{
				Now: now, Deadline: clock.Deadline,
				WarningDays: new(policy.Cadence.ResolvedReconcileWarningDays()),
			}); candidate != nil {
				result.Candidates = append(result.Candidates, rpcNudgeCandidate(candidate))
			}
		}
	}

	var report *rpc.ReconResult
	if s.riskCapital != nil {
		var err error
		report, err = s.buildReconReportContext(ctx)
		if err != nil {
			return rpc.NudgesSnapshotResult{}, err
		}
	}
	currentConfirmed := []string(nil)
	if report != nil {
		result.Reconciliation = &report.Automation
		for _, row := range report.Confirmed {
			currentConfirmed = append(currentConfirmed, confirmedFlowContentIdentity(row))
		}
		switch report.Status {
		case rpc.ReconStatusActive:
			if reconReportStale(policy, report, now) {
				result.SourceHealth.Reconciliation = setHealth(rpc.NudgeInputStatusStale, rpc.NudgeHealthReasonEvidenceStale)
			} else {
				result.SourceHealth.Reconciliation = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
			}
		case rpc.ReconStatusUnapproved:
			result.SourceHealth.Reconciliation = setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonPolicyUnapproved)
		case rpc.ReconStatusUnavailable:
			result.SourceHealth.Reconciliation = unavailable
		default:
			result.SourceHealth.Reconciliation = setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
		}
		unresolved, occurredAt := reconcileNudgeIdentities(report)
		if candidate := risk.EvaluateReconcileException(unresolved, occurredAt); candidate != nil {
			result.Candidates = append(result.Candidates, rpcNudgeCandidate(candidate))
		}
	}

	if !authority.confirmedFlowEligible {
		result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusInactive, rpc.NudgeHealthReasonProcessRemindersNotEnabled)
	} else if s.nudges != nil && s.nudges.healthOK() {
		coverage, _, established, err := s.nudges.confirmedSnapshotContext(ctx, currentConfirmed)
		if err != nil {
			return rpc.NudgesSnapshotResult{}, err
		}
		if established {
			result.ConfirmedFlowCoverage = coverage
			switch {
			case report == nil || report.Status == rpc.ReconStatusUnavailable:
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonSourceUnavailable)
			case report.Status != rpc.ReconStatusActive:
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
			case reconReportStale(policy, report, now):
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusStale, rpc.NudgeHealthReasonEvidenceStale)
			default:
				// Statement-confirmed cash movements emit NO candidate: the
				// statement is the confirmation, the capital state has already
				// credited the flow, and the journal remembers it permanently.
				// The daily brief's capital-events row carries the awareness.
				// Operator decision 2026-08-11: nothing the authority already
				// proved may stand as a pending confirmation, however briefly.
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
			}
		}
	} else if s.nudges != nil {
		result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
	}

	monthly := s.governanceMonthlyPulseForAuthority(authority, policy, report, now)
	if monthly.Candidate != nil {
		result.Candidates = append(result.Candidates, rpcNudgeCandidate(monthly.Candidate))
	}

	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].Kind != result.Candidates[j].Kind {
			return result.Candidates[i].Kind < result.Candidates[j].Kind
		}
		return result.Candidates[i].Fingerprint < result.Candidates[j].Fingerprint
	})
	if err := ctx.Err(); err != nil {
		return rpc.NudgesSnapshotResult{}, err
	}
	return result, nil
}

func stableNudgeTime(preferred, fallback time.Time) time.Time {
	if preferred.IsZero() || preferred.After(fallback) {
		return fallback
	}
	return preferred
}

func reconReportStale(policy *risk.Constitution, report *rpc.ReconResult, now time.Time) bool {
	rc := reconPolicyOf(policy)
	return rc == nil || report == nil || report.StatementAsOf.IsZero() || report.StatementAsOf.After(now) ||
		now.Sub(report.StatementAsOf) > time.Duration(*rc.MaxReportAgeDays)*24*time.Hour
}

func reconcileNudgeIdentities(report *rpc.ReconResult) ([]risk.ReconcileExceptionIdentity, time.Time) {
	if report == nil {
		return nil, time.Time{}
	}
	rows := make([]risk.ReconcileExceptionIdentity, 0, report.Unresolved)
	var occurredAt time.Time
	for _, row := range report.Exceptions {
		if row.Dismissed {
			continue
		}
		material := []string{row.Category, row.ValueDate.UTC().Format(time.RFC3339Nano), row.EventAt.UTC().Format(time.RFC3339Nano)}
		if row.AmountBase != nil {
			material = append(material, strconv.FormatFloat(*row.AmountBase, 'g', -1, 64))
		}
		if row.EventAmountBase != nil {
			material = append(material, strconv.FormatFloat(*row.EventAmountBase, 'g', -1, 64))
		}
		rows = append(rows, risk.ReconcileExceptionIdentity{Kind: row.Category, Identity: row.LineID, Material: material})
		at := row.ValueDate
		if at.IsZero() {
			at = row.EventAt
		}
		if !at.IsZero() && (occurredAt.IsZero() || at.Before(occurredAt)) {
			occurredAt = at
		}
	}
	if occurredAt.IsZero() {
		occurredAt = stableNudgeTime(report.CoverageFrom, report.AsOf)
	}
	return rows, occurredAt
}

func rpcNudgeCandidate(candidate *risk.NudgeCandidate) rpc.NudgeCandidate {
	if candidate == nil {
		return rpc.NudgeCandidate{}
	}
	return rpc.NudgeCandidate{
		Fingerprint: candidate.Fingerprint, Kind: candidate.Kind, State: candidate.State,
		Severity: candidate.Severity, Title: candidate.Title, Body: candidate.Body,
		OccurredAt: candidate.OccurredAt, DueAt: candidate.DueAt,
		ExpiresAt: candidate.ExpiresAt, Destination: candidate.Destination,
	}
}
