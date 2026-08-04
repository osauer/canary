package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
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
	Version            int                          `json:"version"`
	Shadow             nudgeShadowEpisodeState      `json:"shadow"`
	ConfirmedCoverage  *nudgeConfirmedCoverageState `json:"confirmed_coverage,omitempty"`
	ConfirmedEvents    []nudgeConfirmedEventState   `json:"confirmed_events,omitempty"`
	MonthlyCompletions []nudgeMonthlyCompletion     `json:"monthly_completions,omitempty"`
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
	PreCutoverUnreviewed  bool      `json:"pre_cutover_unreviewed"`
	ReviewedAt            time.Time `json:"reviewed_at,omitzero"`
	ReviewPolicyIdentity  string    `json:"review_policy_identity,omitempty"`
	ReviewPolicyVersion   int       `json:"review_policy_version,omitempty"`
	ReviewReportIdentity  string    `json:"review_report_identity,omitempty"`
	ReviewedRowCount      int       `json:"reviewed_row_count,omitempty"`
	KnownRows             []string  `json:"known_rows,omitempty"`
	CurrentRows           []string  `json:"current_rows,omitempty"`
	ReviewedRows          []string  `json:"reviewed_rows,omitempty"`
	ReviewStatementAsOf   time.Time `json:"review_statement_as_of,omitzero"`
	ReviewAuthority       string    `json:"review_authority,omitempty"`
	ReviewGovernance      string    `json:"review_governance,omitempty"`
}

type nudgeConfirmedEventState struct {
	ContentIdentity string    `json:"content_identity"`
	OccurredAt      time.Time `json:"occurred_at"`
	Superseded      bool      `json:"superseded,omitempty"`
}

type nudgeMonthlyCompletion struct {
	Month             string    `json:"month"`
	PolicyIdentity    string    `json:"policy_identity"`
	BriefIdentity     string    `json:"brief_identity"`
	CompletedAt       time.Time `json:"completed_at"`
	Evidence          string    `json:"evidence"`
	AuthorityIdentity string    `json:"authority_identity,omitempty"`
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

type nudgeCutoverReviewEvidence struct {
	ReviewedAt         time.Time
	PolicyIdentity     string
	PolicyVersion      int
	ReportIdentity     string
	ConfirmedRows      int
	ReviewedRows       []string
	StatementAsOf      time.Time
	AuthorityIdentity  string
	GovernanceIdentity string
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
		return fmt.Errorf("governance nudge state is missing from SQLite; cutover bootstrap was not completed")
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

// writeReady permits an authorized mutation to retry after a transient atomic
// write failure. Reads remain faulted through healthOK until a successful
// persist clears fault. A corrupt/unreadable load and an unresolved path are
// never recoverable through this path.
func (st *nudgeStateStore) writeReady() bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return !st.loadErr && (st.core != nil || strings.TrimSpace(st.path) != "")
}

func (st *nudgeStateStore) transientWriteFault() bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return !st.loadErr && st.fault && (st.core != nil || strings.TrimSpace(st.path) != "")
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

func (st *nudgeStateStore) shadowCandidate(policyIdentity, latchEpisode string, open bool) *risk.NudgeCandidate {
	candidate, _ := st.shadowObservation(policyIdentity, latchEpisode, open)
	return candidate
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
			PreCutoverUnreviewed:  true,
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
		if coverage.PreCutoverUnreviewed {
			continue
		}
		if slices.Contains(coverage.ReviewedRows, row) {
			continue
		}
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
		coverage.ReviewedRows = append([]string(nil), state.ConfirmedCoverage.ReviewedRows...)
		cloned.ConfirmedCoverage = &coverage
	}
	cloned.ConfirmedEvents = append([]nudgeConfirmedEventState(nil), state.ConfirmedEvents...)
	cloned.MonthlyCompletions = append([]nudgeMonthlyCompletion(nil), state.MonthlyCompletions...)
	return cloned
}

func (st *nudgeStateStore) confirmedSnapshot(currentRows []string) (*rpc.NudgeConfirmedFlowCoverage, []nudgeConfirmedEventState, bool) {
	coverage, events, ok, _ := st.confirmedSnapshotContext(context.Background(), currentRows)
	return coverage, events, ok
}

type confirmedFlowCurrentAuthority struct {
	GovernanceIdentity string
	ReportIdentity     string
	StatementAsOf      time.Time
}

func (st *nudgeStateStore) confirmedSnapshotContext(ctx context.Context, currentRows []string, currentAuthority ...confirmedFlowCurrentAuthority) (*rpc.NudgeConfirmedFlowCoverage, []nudgeConfirmedEventState, bool, error) {
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
	var authority *confirmedFlowCurrentAuthority
	if len(currentAuthority) > 0 {
		authority = &currentAuthority[0]
	}
	effectiveUnreviewed := !cutoverReviewCurrentLocked(coverageState, st.state.ConfirmedEvents, current, authority)
	coverage := &rpc.NudgeConfirmedFlowCoverage{
		CoverageFrom:              coverageState.CoverageFrom,
		PreCutoverFlowsUnreviewed: effectiveUnreviewed,
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

// cutoverReviewCurrentLocked is the single projection used both by snapshots
// and by the write path deciding whether old evidence is still an idempotency
// boundary or may be replaced by a fresh paired-device review.
func cutoverReviewCurrentLocked(coverage *nudgeConfirmedCoverageState, events []nudgeConfirmedEventState, current map[string]struct{}, authority *confirmedFlowCurrentAuthority) bool {
	if coverage == nil || coverage.PreCutoverUnreviewed {
		return false
	}
	if authority != nil {
		currentGovernance := strings.TrimSpace(authority.GovernanceIdentity)
		currentReviewAuthority := cutoverAuthorityIdentity(currentGovernance, coverage.ReviewReportIdentity,
			coverage.ReviewStatementAsOf, coverage.ReviewedRows)
		if currentGovernance == "" || coverage.ReviewGovernance != currentGovernance ||
			coverage.ReviewAuthority == "" || coverage.ReviewAuthority != currentReviewAuthority {
			return false
		}
	}
	known := make(map[string]struct{}, len(coverage.KnownRows))
	for _, row := range coverage.KnownRows {
		known[row] = struct{}{}
	}
	for row := range current {
		if _, ok := known[row]; !ok {
			return false
		}
	}
	if authority == nil || (authority.ReportIdentity == coverage.ReviewReportIdentity && authority.StatementAsOf.Equal(coverage.ReviewStatementAsOf)) {
		return true
	}
	eventIDs := make(map[string]struct{}, len(events))
	for _, event := range events {
		eventIDs[event.ContentIdentity] = struct{}{}
	}
	delta := 0
	for row := range current {
		if slices.Contains(coverage.ReviewedRows, row) {
			continue
		}
		delta++
		if _, observed := eventIDs[row]; !observed {
			return false
		}
	}
	return delta > 0
}

func (st *nudgeStateStore) reviewConfirmedCutover(evidence nudgeCutoverReviewEvidence) (nudgeConfirmedCoverageState, bool, error) {
	return st.reviewConfirmedCutoverContext(context.Background(), evidence)
}

func (st *nudgeStateStore) reviewConfirmedCutoverContext(ctx context.Context, evidence nudgeCutoverReviewEvidence) (nudgeConfirmedCoverageState, bool, error) {
	if st == nil {
		return nudgeConfirmedCoverageState{}, false, fmt.Errorf("governance nudge persistence is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nudgeConfirmedCoverageState{}, false, err
	}
	st.loadLocked()
	if st.loadErr {
		return nudgeConfirmedCoverageState{}, false, fmt.Errorf("governance nudge persistence is unavailable")
	}
	if st.state.ConfirmedCoverage == nil {
		return nudgeConfirmedCoverageState{}, false, fmt.Errorf("confirmed-flow cutover coverage is unavailable")
	}
	now := time.Now().UTC()
	if st.now != nil {
		now = st.now().UTC()
	}
	coverage := st.state.ConfirmedCoverage
	currentReport := coverage.CurrentReportIdentity
	if currentReport == "" {
		currentReport = coverage.ReportIdentity
	}
	reviewedRows := normalizeOpaqueIdentities(evidence.ReviewedRows)
	governanceIdentity := strings.TrimSpace(evidence.GovernanceIdentity)
	authorityIdentity := strings.TrimSpace(evidence.AuthorityIdentity)
	if authorityIdentity == "" {
		authorityIdentity = cutoverAuthorityIdentity(governanceIdentity, evidence.ReportIdentity, evidence.StatementAsOf, reviewedRows)
	}
	if evidence.ReviewedAt.IsZero() || evidence.ReviewedAt.Before(coverage.CoverageFrom) || evidence.ReviewedAt.After(now) ||
		strings.TrimSpace(evidence.PolicyIdentity) == "" || evidence.PolicyVersion != 4 ||
		strings.TrimSpace(evidence.ReportIdentity) == "" || authorityIdentity == "" || evidence.ConfirmedRows < 0 || evidence.ConfirmedRows != len(reviewedRows) {
		return nudgeConfirmedCoverageState{}, false, fmt.Errorf("confirmed-flow cutover review evidence is invalid")
	}
	if evidence.ReportIdentity != currentReport || evidence.ConfirmedRows != coverage.CurrentRowCount || !slices.Equal(reviewedRows, normalizeOpaqueIdentities(coverage.CurrentRows)) {
		return nudgeConfirmedCoverageState{}, false, fmt.Errorf("confirmed-flow cutover review conflicts with current coverage")
	}
	if !coverage.PreCutoverUnreviewed {
		current := make(map[string]struct{}, len(reviewedRows))
		for _, row := range reviewedRows {
			current[row] = struct{}{}
		}
		currentAuthority := &confirmedFlowCurrentAuthority{
			GovernanceIdentity: governanceIdentity,
			ReportIdentity:     evidence.ReportIdentity,
			StatementAsOf:      evidence.StatementAsOf,
		}
		if cutoverReviewCurrentLocked(coverage, st.state.ConfirmedEvents, current, currentAuthority) {
			if coverage.ReviewPolicyIdentity != evidence.PolicyIdentity || coverage.ReviewPolicyVersion != evidence.PolicyVersion ||
				coverage.ReviewReportIdentity != evidence.ReportIdentity || coverage.ReviewedRowCount != evidence.ConfirmedRows ||
				!slices.Equal(normalizeOpaqueIdentities(coverage.ReviewedRows), reviewedRows) ||
				coverage.ReviewAuthority != authorityIdentity || coverage.ReviewGovernance != governanceIdentity {
				return nudgeConfirmedCoverageState{}, false, fmt.Errorf("confirmed-flow cutover review conflicts with pinned evidence")
			}
			return *coverage, true, nil
		}
		// The old exact evidence is inert under the same projection used by
		// snapshots. Only this fresh validated foreground action may replace it.
	}
	before := cloneNudgeState(st.state)
	coverage.PreCutoverUnreviewed = false
	coverage.ReviewedAt = evidence.ReviewedAt.UTC()
	coverage.ReviewPolicyIdentity = evidence.PolicyIdentity
	coverage.ReviewPolicyVersion = evidence.PolicyVersion
	coverage.ReviewReportIdentity = evidence.ReportIdentity
	coverage.ReviewedRowCount = evidence.ConfirmedRows
	coverage.ReviewedRows = reviewedRows
	coverage.ReviewStatementAsOf = evidence.StatementAsOf.UTC()
	coverage.ReviewAuthority = authorityIdentity
	coverage.ReviewGovernance = governanceIdentity
	filtered := st.state.ConfirmedEvents[:0]
	for _, event := range st.state.ConfirmedEvents {
		if err := ctx.Err(); err != nil {
			st.state = before
			return nudgeConfirmedCoverageState{}, false, err
		}
		if !slices.Contains(reviewedRows, event.ContentIdentity) {
			filtered = append(filtered, event)
		}
	}
	st.state.ConfirmedEvents = filtered
	if err := ctx.Err(); err != nil {
		st.state = before
		return nudgeConfirmedCoverageState{}, false, err
	}
	if err := st.persistLocked(); err != nil {
		st.state = before
		return nudgeConfirmedCoverageState{}, false, err
	}
	return *coverage, false, nil
}

func (st *nudgeStateStore) monthlyCompletion(month, policyIdentity string) *risk.MonthlyPulseCompletion {
	return st.monthlyCompletionForUse(month, policyIdentity, false)
}

func (st *nudgeStateStore) monthlyCompletionForWrite(month, policyIdentity string) *risk.MonthlyPulseCompletion {
	return st.monthlyCompletionForUse(month, policyIdentity, true)
}

func (st *nudgeStateStore) monthlyCompletionForUse(month, policyIdentity string, allowTransientFault bool) *risk.MonthlyPulseCompletion {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.loadErr || (st.fault && !allowTransientFault) {
		return nil
	}
	for _, rec := range slices.Backward(st.state.MonthlyCompletions) {
		if rec.Month == month && rec.PolicyIdentity == policyIdentity {
			return &risk.MonthlyPulseCompletion{
				Month: month, PolicyFingerprint: policyIdentity,
				CompletedAt: rec.CompletedAt, Evidence: rec.Evidence,
			}
		}
	}
	return nil
}

func (st *nudgeStateStore) monthlyCompletionRecord(month, policyIdentity string) (nudgeMonthlyCompletion, bool) {
	if st == nil {
		return nudgeMonthlyCompletion{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.loadErr || st.fault {
		return nudgeMonthlyCompletion{}, false
	}
	for _, rec := range slices.Backward(st.state.MonthlyCompletions) {
		if rec.Month == month && rec.PolicyIdentity == policyIdentity {
			return rec, true
		}
	}
	return nudgeMonthlyCompletion{}, false
}

func (st *nudgeStateStore) recordMonthlyCompletion(month, policyIdentity, briefFingerprint, authorityIdentity string, at time.Time) (nudgeMonthlyCompletion, bool, error) {
	if st == nil {
		return nudgeMonthlyCompletion{}, false, fmt.Errorf("governance nudge persistence is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.loadErr {
		return nudgeMonthlyCompletion{}, false, fmt.Errorf("governance nudge persistence is unavailable")
	}
	for _, rec := range st.state.MonthlyCompletions {
		if rec.Month == month && rec.PolicyIdentity == policyIdentity {
			if rec.BriefIdentity != briefFingerprint {
				return nudgeMonthlyCompletion{}, false, fmt.Errorf("monthly brief completion conflicts with the pinned rendered brief")
			}
			return rec, true, nil
		}
	}
	rec := nudgeMonthlyCompletion{
		Month: month, PolicyIdentity: policyIdentity,
		BriefIdentity: briefFingerprint, AuthorityIdentity: authorityIdentity,
		CompletedAt: at.UTC(), Evidence: rpc.BriefAckEvidenceRender,
	}
	st.state.MonthlyCompletions = append(st.state.MonthlyCompletions, rec)
	if err := st.persistLocked(); err != nil {
		return nudgeMonthlyCompletion{}, false, err
	}
	return rec, false, nil
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

func (s *Server) governanceMonthlyPulse(constitution *risk.Constitution, report *rpc.ReconResult, now time.Time) (risk.MonthlyPulseEvaluation, *risk.MonthlyPulseCompletion) {
	return s.governanceMonthlyPulseForAuthority(s.currentNudgeAuthority(now), constitution, report, now)
}

func (s *Server) governanceMonthlyPulseForAuthority(authority nudgeAuthorityState, constitution *risk.Constitution, report *rpc.ReconResult, now time.Time) (risk.MonthlyPulseEvaluation, *risk.MonthlyPulseCompletion) {
	return s.governanceMonthlyPulseForAuthorityUse(authority, constitution, report, now, false)
}

func (s *Server) governanceMonthlyPulseForWrite(authority nudgeAuthorityState, constitution *risk.Constitution, report *rpc.ReconResult, now time.Time) (risk.MonthlyPulseEvaluation, *risk.MonthlyPulseCompletion) {
	return s.governanceMonthlyPulseForAuthorityUse(authority, constitution, report, now, true)
}

// governanceMonthlyPulseForRenderRecovery never projects completion from a
// faulted store. It only keeps a currently due month conservatively due so a
// fresh exact-authority render receipt can reach the authorized retry write.
func (s *Server) governanceMonthlyPulseForRenderRecovery(authority nudgeAuthorityState, constitution *risk.Constitution, now time.Time) risk.MonthlyPulseEvaluation {
	if constitution == nil || constitution.PolicyVersion < 4 || s == nil || s.nudges == nil ||
		!authority.cadenceEligible || authority.policyIdentity != nudgePolicyIdentity(constitution) || !s.nudges.transientWriteFault() {
		return risk.MonthlyPulseEvaluation{}
	}
	return risk.EvaluateMonthlyPulse(risk.MonthlyPulseInput{
		Now: now, Cadence: constitution.Cadence, PolicyFingerprint: authority.policyIdentity,
		PolicyEvidenceReady: policyPinsReady(authority.report.Inventory),
	})
}

func (s *Server) governanceMonthlyPulseForAuthorityUse(authority nudgeAuthorityState, constitution *risk.Constitution, report *rpc.ReconResult, now time.Time, allowTransientFault bool) (risk.MonthlyPulseEvaluation, *risk.MonthlyPulseCompletion) {
	if constitution == nil || constitution.PolicyVersion < 4 {
		return risk.MonthlyPulseEvaluation{}, nil
	}
	identity := nudgePolicyIdentity(constitution)
	month := nudgeMonth(constitution.Cadence, now)
	storeReady := s != nil && s.nudges != nil && s.nudges.healthOK()
	if allowTransientFault {
		storeReady = s != nil && s.nudges != nil && s.nudges.writeReady()
	}
	if !authority.cadenceEligible || authority.policyIdentity != identity || !storeReady {
		return risk.MonthlyPulseEvaluation{Status: risk.MonthlyPulseStatusBlocked, Month: month}, nil
	}
	completion := s.nudges.monthlyCompletion(month, identity)
	if allowTransientFault {
		completion = s.nudges.monthlyCompletionForWrite(month, identity)
	}
	evaluation := risk.EvaluateMonthlyPulse(risk.MonthlyPulseInput{
		Now: now, Cadence: constitution.Cadence, PolicyFingerprint: identity,
		PolicyEvidenceReady: completion != nil || policyPinsReady(authority.report.Inventory), Completion: completion,
	})
	return evaluation, completion
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

func (s *Server) handleNudgesCutoverReview(ctx context.Context, req *rpc.Request) (*rpc.NudgesCutoverReviewResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var params rpc.NudgesCutoverReviewParams
	if err := decodeParams(req.Params, &params); err != nil {
		return nil, err
	}
	// The fixed paired origin is assigned by the authenticated app route. On
	// the raw local RPC socket this label is advisory and forgeable; it is not
	// broker-write, freeze/limit, or policy-change authority and does not claim
	// device-cryptographic proof or that a person was observed reviewing.
	if params.Origin != rpc.NudgeCutoverReviewOriginPairedDevice ||
		params.Evidence != rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender {
		return nil, errBadRequest("confirmed-flow cutover review requires an authenticated paired-device foreground render")
	}
	if s == nil || s.nudges == nil {
		return nil, errBadRequest("confirmed-flow cutover review state is unavailable")
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	authority, reportIdentity, rows, token, err := s.validateCutoverReview(ctx, now)
	if err != nil {
		return nil, err
	}
	s.nudgeWriteMu.Lock()
	defer s.nudgeWriteMu.Unlock()
	if s.nudgeBeforeCommit != nil {
		s.nudgeBeforeCommit("cutover")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finalAuthority, finalReportIdentity, finalRows, finalToken, err := s.validateCutoverReview(ctx, now)
	if err != nil {
		return nil, err
	}
	if token != finalToken || authority.policyIdentity != finalAuthority.policyIdentity || reportIdentity != finalReportIdentity || !slices.Equal(rows, finalRows) {
		return nil, errBadRequest("confirmed-flow cutover review conflicts with current authority")
	}
	if s.nudgeAfterValidation != nil {
		s.nudgeAfterValidation("cutover")
	}
	governanceIdentity := nudgeAuthorityToken(finalAuthority)
	receipt, already, err := s.nudges.reviewConfirmedCutoverContext(ctx, nudgeCutoverReviewEvidence{
		ReviewedAt: now, PolicyIdentity: finalAuthority.policyIdentity, PolicyVersion: finalAuthority.report.PolicyVersion,
		ReportIdentity: finalReportIdentity, ConfirmedRows: len(finalRows), ReviewedRows: finalRows,
		StatementAsOf: finalToken.statementAsOf, AuthorityIdentity: finalToken.identity, GovernanceIdentity: governanceIdentity,
	})
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	if s.nudgeAfterPersist != nil {
		s.nudgeAfterPersist("cutover")
	}
	return &rpc.NudgesCutoverReviewResult{
		OK: true, AlreadyReviewed: already, ReviewedAt: receipt.ReviewedAt,
		CoverageFrom: receipt.CoverageFrom,
		Evidence:     rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}, nil
}

type cutoverValidationToken struct {
	identity      string
	statementAsOf time.Time
}

func (s *Server) validateCutoverReview(ctx context.Context, now time.Time) (nudgeAuthorityState, string, []string, cutoverValidationToken, error) {
	if err := ctx.Err(); err != nil {
		return nudgeAuthorityState{}, "", nil, cutoverValidationToken{}, err
	}
	authority := s.currentNudgeAuthority(now)
	if !authority.confirmedFlowEligible || !policyPinsReady(authority.report.Inventory) {
		return nudgeAuthorityState{}, "", nil, cutoverValidationToken{}, errBadRequest("confirmed-flow cutover review requires current active v4 base-policy authority with matching sibling policy pins")
	}
	report, err := s.buildReconReportContext(ctx)
	if err != nil {
		return nudgeAuthorityState{}, "", nil, cutoverValidationToken{}, err
	}
	if !currentBrokerBackedReconReport(authority.policy, report, now) {
		return nudgeAuthorityState{}, "", nil, cutoverValidationToken{}, errBadRequest("confirmed-flow cutover review requires a current active broker-backed reconciliation report")
	}
	rows := make([]string, 0, len(report.Confirmed))
	for _, row := range report.Confirmed {
		if err := ctx.Err(); err != nil {
			return nudgeAuthorityState{}, "", nil, cutoverValidationToken{}, err
		}
		rows = append(rows, confirmedFlowContentIdentity(row))
	}
	rows = normalizeOpaqueIdentities(rows)
	reportIdentity := opaqueIdentity("recon-report", report.ReportID)
	token := cutoverValidationToken{
		identity:      cutoverAuthorityIdentity(nudgeAuthorityToken(authority), reportIdentity, report.StatementAsOf, rows),
		statementAsOf: report.StatementAsOf.UTC(),
	}
	return authority, reportIdentity, rows, token, nil
}

func cutoverAuthorityIdentity(governanceIdentity, reportIdentity string, statementAsOf time.Time, rows []string) string {
	return opaqueIdentity("cutover-authority", governanceIdentity, reportIdentity,
		statementAsOf.UTC().Format(time.RFC3339Nano), strings.Join(normalizeOpaqueIdentities(rows), ","))
}

func nudgeAuthorityToken(authority nudgeAuthorityState) string {
	parts := []string{authority.policyIdentity, strconv.Itoa(authority.report.PolicyVersion), authority.report.Status, authority.report.Source,
		strconv.FormatBool(authority.eligible), strconv.FormatBool(authority.cadenceEligible), strconv.FormatBool(authority.confirmedFlowEligible),
		authority.policyHealth.Status, authority.policyHealth.Reason}
	for _, pin := range authority.report.Inventory {
		parts = append(parts, pin.Policy, pin.Status, pin.PinnedID, pin.PinnedVersion, pin.LiveID, pin.LiveVersion)
	}
	return opaqueIdentity("nudge-authority", parts...)
}

func monthlyAuthorityIdentity(authority nudgeAuthorityState, month string, report *rpc.ReconResult, now time.Time) string {
	parts := []string{nudgeAuthorityToken(authority), month}
	if report == nil || (report.Status == rpc.ReconStatusUnavailable && strings.TrimSpace(report.ReportID) == "") {
		parts = append(parts, "report_unavailable")
		return opaqueIdentity("monthly-authority", parts...)
	}
	parts = append(parts,
		opaqueIdentity("recon-report", report.ReportID), report.Status,
		report.StatementAsOf.UTC().Format(time.RFC3339Nano),
		strconv.FormatBool(currentBrokerBackedReconReport(authority.policy, report, now)),
	)
	for _, health := range report.InputHealth {
		parts = append(parts, health.Source, health.Status, health.AsOf.UTC().Format(time.RFC3339Nano))
	}
	return opaqueIdentity("monthly-authority", parts...)
}

func currentBrokerBackedReconReport(policy *risk.Constitution, report *rpc.ReconResult, now time.Time) bool {
	if report == nil || report.Status != rpc.ReconStatusActive || strings.TrimSpace(report.ReportID) == "" || reconReportStale(policy, report, now) {
		return false
	}
	for _, health := range report.InputHealth {
		if health.Source == "statements" {
			return health.Status == "ok" && !health.AsOf.After(now)
		}
	}
	return false
}

func (s *Server) composeNudgesSnapshot() rpc.NudgesSnapshotResult {
	result, _ := s.composeNudgesSnapshotContext(context.Background())
	return result
}

func (s *Server) composeNudgesSnapshotContext(ctx context.Context) (rpc.NudgesSnapshotResult, error) {
	return s.composeNudgesSnapshotContextWithAuthority(ctx, nil)
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
		currentAuthority := confirmedFlowCurrentAuthority{GovernanceIdentity: nudgeAuthorityToken(authority)}
		if report != nil {
			currentAuthority.ReportIdentity = opaqueIdentity("recon-report", report.ReportID)
			currentAuthority.StatementAsOf = report.StatementAsOf.UTC()
		}
		coverage, events, established, err := s.nudges.confirmedSnapshotContext(ctx, currentConfirmed, currentAuthority)
		if err != nil {
			return rpc.NudgesSnapshotResult{}, err
		}
		if established {
			result.ConfirmedFlowCoverage = coverage
			switch {
			case coverage.PreCutoverFlowsUnreviewed:
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusUnapproved, rpc.NudgeHealthReasonCutoverReviewRequired)
			case report == nil || report.Status == rpc.ReconStatusUnavailable:
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusUnavailable, rpc.NudgeHealthReasonSourceUnavailable)
			case report.Status != rpc.ReconStatusActive:
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
			case reconReportStale(policy, report, now):
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusStale, rpc.NudgeHealthReasonEvidenceStale)
			default:
				result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonNone)
				for _, event := range events {
					if candidate := risk.EvaluateConfirmedFlow(event.ContentIdentity, event.OccurredAt); candidate != nil {
						result.Candidates = append(result.Candidates, rpcNudgeCandidate(candidate))
					}
				}
			}
		}
	} else if s.nudges != nil {
		result.SourceHealth.ConfirmedFlow = setHealth(rpc.NudgeInputStatusError, rpc.NudgeHealthReasonEvaluationError)
	}

	monthly, _ := s.governanceMonthlyPulseForAuthority(authority, policy, report, now)
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

func nudgeMonth(cadence risk.ConstitutionCadence, now time.Time) string {
	location, err := cadence.NudgeLocation()
	if err != nil {
		return ""
	}
	return now.In(location).Format("2006-01")
}

func nudgeLocalDay(cadence risk.ConstitutionCadence, now time.Time) string {
	location, err := cadence.NudgeLocation()
	if err != nil {
		return ""
	}
	return now.In(location).Format(time.DateOnly)
}
