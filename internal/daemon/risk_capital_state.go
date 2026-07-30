package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Runtime capital state for the risk constitution (internal-docs/design/risk-policy.md):
// the cash-flow-adjusted equity peak, the drawdown latch, declared capital
// events, statement-authoritative v3 facts, one-shot overrides, and cadence
// artefact completions.
//
// Authority split: risk-policy.toml owns the numbers, internal/risk owns the
// evaluation, this store owns observed/derived runtime facts. Aggregates that
// can be re-derived from SQLite capital events (declared flows, reconcile
// evidence) are replayed on every bind; statement facts come from retained
// broker evidence. A versioned CAS document owns what those sources cannot
// rebuild: peak, latch, applied statement corrections, daily equity samples,
// overrides, and artefact completions. Legacy files below are importer/test
// oracles only after cutover.
//
// Nothing in this file may influence submit eligibility, blockers, freeze,
// pins, or tokens: v1 is advisory/shadow end to end.

const (
	riskCapitalStateFile     = "risk-capital-state.json"
	capitalEventsJournalFile = "capital-events.jsonl"
	riskPolicyJournalFile    = "risk-policy-journal.jsonl"
	riskCapitalStateVer      = 1
	riskCapitalSQLiteDocVer  = 2
	// riskCapitalDailySampleKeep bounds the per-day equity sample cache
	// feeding the same-day recon equity check and the backtest replay.
	riskCapitalDailySampleKeep = 45 * 24 * time.Hour
	// riskCapitalPersistEvery throttles equity-cache persistence; latch,
	// peak, and event writes always persist immediately.
	riskCapitalPersistEvery = time.Minute
	riskCapitalAutoOrigin   = "daemon-auto"
)

type riskCapitalStateFileV1 struct {
	Version   int       `json:"version"`
	GenesisAt time.Time `json:"genesis_at,omitzero"`
	Seeded    bool      `json:"seeded"`
	// AccountID/AccountMode bind this capital state to one broker identity,
	// adopted from the first accepted live-scope observation. A session on a
	// different account or a non-live mode can never write the peak again
	// (2026-07-19 incident: a paper-pinned rehearsal daemon sharing this
	// state dir ratcheted the live peak with the paper account's equity).
	AccountID                         string               `json:"account_id,omitempty"`
	AccountMode                       string               `json:"account_mode,omitempty"`
	AdjustedPeakBase                  float64              `json:"adjusted_peak_base"`
	PeakAsOf                          time.Time            `json:"peak_as_of,omitzero"`
	LastEquityBase                    float64              `json:"last_equity_base"`
	LastEquityAsOf                    time.Time            `json:"last_equity_as_of,omitzero"`
	DailyEquity                       map[string]float64   `json:"daily_equity,omitempty"`
	LastTier                          string               `json:"last_tier,omitempty"`
	BlockLatched                      bool                 `json:"block_latched"`
	LatchedAt                         time.Time            `json:"latched_at,omitzero"`
	LatchEpisodeSeq                   uint64               `json:"latch_episode_seq,omitempty"`
	LatchConsumedPct                  float64              `json:"latch_consumed_pct,omitempty"`
	Overrides                         []rpc.OverrideRecord `json:"overrides,omitempty"`
	Artefacts                         []rpc.ArtefactRecord `json:"artefacts,omitempty"`
	StatementFlowsBase                float64              `json:"statement_flows_base,omitempty"`
	StatementCoverageTo               time.Time            `json:"statement_coverage_to,omitzero"`
	StatementAuthorityActive          bool                 `json:"statement_authority_active,omitempty"`
	IncorporatedStatementLineIDs      []string             `json:"incorporated_statement_line_ids,omitempty"`
	AppliedStatementPeakCorrectionIDs []string             `json:"applied_statement_peak_correction_ids,omitempty"`
}

type capitalEventV1 struct {
	Version     int       `json:"version"`
	At          time.Time `json:"at"`
	Type        string    `json:"type"` // deposit | withdrawal | reconcile
	AmountBase  float64   `json:"amount_base,omitempty"`
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	Note        string    `json:"note,omitempty"`
	Origin      string    `json:"origin,omitempty"`
	// ReportID and CoverageTo record which recon report a reconcile signed
	// off: an audit fact previously preserved only in the human message.
	ReportID   string    `json:"report_id,omitempty"`
	CoverageTo time.Time `json:"coverage_to,omitzero"`
}

type capitalReconRef struct {
	ReportID   string
	CoverageTo time.Time
}

type capitalEventReplay struct {
	declaredFlowsBase      float64
	declaredEvents         []capitalEventV1
	lastReconciledAt       time.Time
	lastReconcileReportID  string
	lastReconcileSource    string
	lastAutoExtendedAt     time.Time
	lastAutoExtendReportID string
	reconciledReportIDs    map[string]struct{}
}

// riskCapitalSQLiteDocument is the complete authoritative capital state.
// Capital declarations and governance evidence live with the current facts
// so a state transition and the evidence explaining it commit under one CAS.
// The old JSONL files are read only by the explicit cutover importer.
type riskCapitalSQLiteDocument struct {
	Version     int                    `json:"version"`
	State       riskCapitalStateFileV1 `json:"state"`
	OverrideSeq int                    `json:"override_seq,omitempty"`
}

type riskCapitalStore struct {
	mu                     sync.Mutex
	now                    func() time.Time
	core                   *corestore.Store
	revision               int64
	committed              riskCapitalSQLiteDocument
	committedCapitalEvents []capitalEventV1
	pendingEvents          []corestore.EventInput
	capitalEvents          []capitalEventV1
	nudges                 *nudgeStateStore
	observeConfirmedFlows  func(nudgeConfirmedFlowSnapshot)
	// nudgeCaptureHook is a test-only barrier invoked while mu is held before
	// the atomic capital-report/latch capture.
	nudgeCaptureHook func()
	loaded           bool
	state            riskCapitalStateFileV1
	// Re-derived from capital-events.jsonl on load, maintained incrementally
	// afterwards; never trusted from the state file.
	cumFlowsBase           float64
	declaredEvents         []capitalEventV1
	lastReconciledAt       time.Time
	lastReconcileReportID  string
	lastReconcileSource    string
	lastAutoExtendedAt     time.Time
	lastAutoExtendReportID string
	reconciledReportIDs    map[string]struct{}
	lastPersistAt          time.Time
	overrideSeq            int
	// scopeRejectionsJournaled throttles equity_observation_rejected journal
	// rows to one per (reason, account) per process.
	scopeRejectionsJournaled map[string]bool
}

func (s *Server) installRiskCapitalStore() {
	if s == nil {
		return
	}
	s.riskCapital = &riskCapitalStore{now: s.now}
}

func (st *riskCapitalStore) bindCore(ctx context.Context, core *corestore.Store) error {
	if st == nil || core == nil {
		return fmt.Errorf("risk capital SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindRiskCapital)
	if err != nil {
		return fmt.Errorf("load risk capital state from SQLite: %w", err)
	}
	persisted := riskCapitalSQLiteDocument{
		Version: riskCapitalSQLiteDocVer,
		State:   riskCapitalStateFileV1{Version: riskCapitalStateVer},
	}
	revision := int64(0)
	if ok {
		if err := json.Unmarshal(doc.JSON, &persisted); err != nil || persisted.Version != riskCapitalSQLiteDocVer || persisted.State.Version != riskCapitalStateVer {
			if err == nil {
				err = fmt.Errorf("unsupported capital document version %d/state version %d", persisted.Version, persisted.State.Version)
			}
			return fmt.Errorf("decode risk capital state from SQLite: %w", err)
		}
		revision = doc.Revision
	} else {
		return fmt.Errorf("risk capital state is missing from SQLite; cutover bootstrap was not completed")
	}
	events, err := loadAllCoreEvents(ctx, core, coreEventCapital)
	if err != nil {
		return fmt.Errorf("load capital events from SQLite: %w", err)
	}
	capitalEvents := make([]capitalEventV1, 0, len(events))
	for _, event := range events {
		var capital capitalEventV1
		if err := json.Unmarshal(event.PayloadJSON, &capital); err != nil || capital.Version != 1 {
			return fmt.Errorf("decode capital event %d from SQLite", event.EventSeq)
		}
		capitalEvents = append(capitalEvents, capital)
	}
	st.mu.Lock()
	st.core, st.revision, st.loaded = core, revision, true
	st.capitalEvents = append([]capitalEventV1(nil), capitalEvents...)
	st.installSQLiteDocumentLocked(persisted)
	st.committed = cloneRiskCapitalDocument(persisted)
	st.committedCapitalEvents = append([]capitalEventV1(nil), capitalEvents...)
	st.mu.Unlock()
	return nil
}

func cloneRiskCapitalDocument(in riskCapitalSQLiteDocument) riskCapitalSQLiteDocument {
	raw, err := json.Marshal(in)
	if err != nil {
		return riskCapitalSQLiteDocument{Version: riskCapitalSQLiteDocVer, State: riskCapitalStateFileV1{Version: riskCapitalStateVer}}
	}
	var out riskCapitalSQLiteDocument
	if json.Unmarshal(raw, &out) != nil {
		return riskCapitalSQLiteDocument{Version: riskCapitalSQLiteDocVer, State: riskCapitalStateFileV1{Version: riskCapitalStateVer}}
	}
	return out
}

func (st *riskCapitalStore) sqliteDocumentLocked() riskCapitalSQLiteDocument {
	return riskCapitalSQLiteDocument{
		Version: riskCapitalSQLiteDocVer, State: st.state,
		OverrideSeq: st.overrideSeq,
	}
}

func (st *riskCapitalStore) installSQLiteDocumentLocked(doc riskCapitalSQLiteDocument) {
	st.state = doc.State
	st.state.Version = riskCapitalStateVer
	st.overrideSeq = doc.OverrideSeq
	replayed := replayCapitalEventSlice(st.capitalEvents)
	st.cumFlowsBase = replayed.declaredFlowsBase
	st.declaredEvents = replayed.declaredEvents
	st.lastReconciledAt = replayed.lastReconciledAt
	st.lastReconcileReportID = replayed.lastReconcileReportID
	st.lastReconcileSource = replayed.lastReconcileSource
	st.lastAutoExtendedAt = replayed.lastAutoExtendedAt
	st.lastAutoExtendReportID = replayed.lastAutoExtendReportID
	st.reconciledReportIDs = replayed.reconciledReportIDs
}

// appendCapitalEvent journals one declared capital event.
func (st *riskCapitalStore) appendCapitalEvent(ev capitalEventV1) error {
	if st != nil && st.core != nil {
		raw, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		st.capitalEvents = append(st.capitalEvents, ev)
		st.pendingEvents = append(st.pendingEvents, corestore.EventInput{
			ScopeKey: daemonStateScope,
			EventKey: coreEventKey(coreEventCapital, ev.At, raw, len(st.capitalEvents)),
			Type:     coreEventCapital, Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
			OccurredAt: ev.At, PayloadJSON: raw,
			Projection: corestore.EventProjection{CapitalEvent: &corestore.CapitalEventProjection{
				Kind: ev.Type, AmountBaseText: strconv.FormatFloat(ev.AmountBase, 'g', -1, 64),
				EffectiveAt: ev.EffectiveAt.UTC().Format(time.RFC3339Nano), ReportID: ev.ReportID,
			}},
		})
		return nil
	}
	return appendCapitalEvent(ev)
}

// appendRiskPolicyJournal journals one governance event.
func (st *riskCapitalStore) appendRiskPolicyJournal(entry map[string]any) {
	if st != nil && st.core != nil {
		raw, err := json.Marshal(entry)
		if err == nil {
			at := st.now().UTC()
			if value, ok := entry["at"].(time.Time); ok && !value.IsZero() {
				at = value.UTC()
			}
			kind := strings.TrimSpace(fmt.Sprint(entry["kind"]))
			if kind == "" {
				kind = "governance_event"
			}
			projection := corestore.RiskPolicyEventProjection{
				Kind: kind, PolicyID: strings.TrimSpace(fmt.Sprint(entry["policy_id"])),
				PolicyFingerprint: strings.TrimSpace(fmt.Sprint(entry["policy_fingerprint"])),
			}
			if version, ok := integerAny(entry["policy_version"]); ok {
				projection.PolicyVersion = &version
			}
			st.pendingEvents = append(st.pendingEvents, corestore.EventInput{
				ScopeKey: daemonStateScope,
				EventKey: coreEventKey(coreEventRiskPolicy, at, raw, int(st.revision)+len(st.pendingEvents)+1),
				Type:     coreEventRiskPolicy, Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
				OccurredAt: at, PayloadJSON: raw,
				Projection: corestore.EventProjection{RiskPolicyEvent: &projection},
			})
		}
		return
	}
	appendRiskPolicyJournal(entry)
}

func integerAny(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), v == float64(int64(v))
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func (st *riskCapitalStore) loadLocked() {
	if st.loaded {
		return
	}
	st.loaded = true
	if path, err := defaultTradingStatePath(riskCapitalStateFile); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			var f riskCapitalStateFileV1
			if json.Unmarshal(data, &f) == nil && f.Version == riskCapitalStateVer {
				st.state = f
			}
		}
	}
	st.state.Version = riskCapitalStateVer
	if st.state.BlockLatched && st.state.LatchEpisodeSeq == 0 {
		// Backward-compatible replay for a latch created before the opaque
		// episode counter existed. Snapshot reads do not persist this upgrade.
		st.state.LatchEpisodeSeq = 1
	}
	// Journal replay owns flows and reconciliation recency.
	replayed := replayCapitalEvents()
	st.cumFlowsBase = replayed.declaredFlowsBase
	st.declaredEvents = replayed.declaredEvents
	st.lastReconciledAt = replayed.lastReconciledAt
	st.lastReconcileReportID = replayed.lastReconcileReportID
	st.lastReconcileSource = replayed.lastReconcileSource
	st.lastAutoExtendedAt = replayed.lastAutoExtendedAt
	st.lastAutoExtendReportID = replayed.lastAutoExtendReportID
	st.reconciledReportIDs = replayed.reconciledReportIDs
}

func replayCapitalEvents() capitalEventReplay {
	path, err := defaultTradingStatePath(capitalEventsJournalFile)
	if err != nil {
		return capitalEventReplay{reconciledReportIDs: make(map[string]struct{})}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return capitalEventReplay{reconciledReportIDs: make(map[string]struct{})}
	}
	var events []capitalEventV1
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev capitalEventV1
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Version != 1 {
			continue
		}
		events = append(events, ev)
	}
	return replayCapitalEventSlice(events)
}

func replayCapitalEventSlice(events []capitalEventV1) capitalEventReplay {
	out := capitalEventReplay{reconciledReportIDs: make(map[string]struct{})}
	for _, ev := range events {
		switch ev.Type {
		case "deposit":
			out.declaredFlowsBase += ev.AmountBase
			out.declaredEvents = append(out.declaredEvents, ev)
		case "withdrawal":
			out.declaredFlowsBase -= ev.AmountBase
			out.declaredEvents = append(out.declaredEvents, ev)
		case "reconcile":
			if ev.ReportID != "" {
				out.reconciledReportIDs[ev.ReportID] = struct{}{}
			}
			if ev.At.After(out.lastReconciledAt) {
				out.lastReconciledAt = ev.At
				out.lastReconcileReportID = ev.ReportID
				out.lastReconcileSource = rpc.ReconcileSourceHuman
				if ev.Origin == riskCapitalAutoOrigin {
					out.lastReconcileSource = rpc.ReconcileSourceAutomatic
				}
			}
			if ev.Origin == riskCapitalAutoOrigin && ev.At.After(out.lastAutoExtendedAt) {
				out.lastAutoExtendedAt = ev.At
				out.lastAutoExtendReportID = ev.ReportID
			}
		}
	}
	return out
}

func (st *riskCapitalStore) persistLocked(force bool) error {
	if st.core != nil {
		doc := st.sqliteDocumentLocked()
		raw, err := json.Marshal(doc)
		if err == nil {
			var saved corestore.StateDocument
			update := corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindRiskCapital, ExpectedRevision: st.revision, JSON: raw}
			if len(st.pendingEvents) > 0 {
				saved, _, err = st.core.CompareAndSwapStateDocumentWithEvents(context.Background(), update, st.pendingEvents)
			} else {
				saved, err = st.core.CompareAndSwapStateDocument(context.Background(), update)
			}
			if err == nil {
				st.revision = saved.Revision
				st.committed = cloneRiskCapitalDocument(doc)
				st.committedCapitalEvents = append([]capitalEventV1(nil), st.capitalEvents...)
				st.pendingEvents = nil
				st.lastPersistAt = st.now()
				return nil
			}
		}
		// The mutex keeps the uncommitted state private; restore the last
		// committed generation before any caller can observe it.
		st.installSQLiteDocumentLocked(cloneRiskCapitalDocument(st.committed))
		st.capitalEvents = append([]capitalEventV1(nil), st.committedCapitalEvents...)
		replayed := replayCapitalEventSlice(st.capitalEvents)
		st.cumFlowsBase = replayed.declaredFlowsBase
		st.declaredEvents = replayed.declaredEvents
		st.lastReconciledAt = replayed.lastReconciledAt
		st.lastReconcileReportID = replayed.lastReconcileReportID
		st.lastReconcileSource = replayed.lastReconcileSource
		st.lastAutoExtendedAt = replayed.lastAutoExtendedAt
		st.lastAutoExtendReportID = replayed.lastAutoExtendReportID
		st.reconciledReportIDs = replayed.reconciledReportIDs
		st.pendingEvents = nil
		if err == nil {
			err = fmt.Errorf("encode risk capital SQLite document")
		}
		return fmt.Errorf("persist risk capital state in SQLite: %w", err)
	}
	now := st.now()
	if !force && now.Sub(st.lastPersistAt) < riskCapitalPersistEvery {
		return nil
	}
	st.lastPersistAt = now
	if path, err := defaultTradingStatePath(riskCapitalStateFile); err == nil {
		if data, err := json.Marshal(st.state); err == nil {
			_ = writePrivateStateAtomic(path, data) // best-effort, never fails the hot path
		}
	}
	return nil
}

// runtimeLocked builds the evaluator's view of the state.
func (st *riskCapitalStore) runtimeLocked(c *risk.Constitution, now time.Time) risk.CapitalRuntime {
	flows, _, _ := st.effectiveFlowsLocked(c)
	var overrideUntil time.Time
	for _, o := range st.state.Overrides {
		if o.Active && o.Control == "capital.max_unreconciled_days" && !now.After(o.ExpiresAt) && o.ExpiresAt.After(overrideUntil) {
			overrideUntil = o.ExpiresAt
		}
	}
	return risk.CapitalRuntime{
		AdjustedPeakBase:          st.state.AdjustedPeakBase,
		PeakAsOf:                  st.state.PeakAsOf,
		CumExternalFlowsBase:      flows,
		Seeded:                    st.state.Seeded,
		BlockLatched:              st.state.BlockLatched,
		LastReconciledAt:          st.lastReconciledAt,
		UnreconciledOverrideUntil: overrideUntil,
	}
}

func (st *riskCapitalStore) effectiveFlowsLocked(c *risk.Constitution) (effective, statement float64, source string) {
	if c == nil || c.PolicyVersion < 3 {
		return st.cumFlowsBase, 0, rpc.CapitalFlowSourceDeclared
	}
	statement = st.state.StatementFlowsBase
	for _, ev := range st.declaredEvents {
		effectiveAt := ev.EffectiveAt
		if effectiveAt.IsZero() {
			effectiveAt = ev.At
		}
		if st.state.Seeded && !st.state.GenesisAt.IsZero() && utcDateBefore(effectiveAt, st.state.GenesisAt) {
			continue
		}
		if !st.state.StatementCoverageTo.IsZero() && !utcDateAfter(effectiveAt, st.state.StatementCoverageTo) {
			continue
		}
		if ev.Type == "deposit" {
			statement += ev.AmountBase
		} else if ev.Type == "withdrawal" {
			statement -= ev.AmountBase
		}
	}
	return statement, statement, rpc.CapitalFlowSourceStatement
}

// Observe folds one equity reading into the state: seeds or raises the
// cash-flow-adjusted peak, evaluates the tier under the active constitution,
// engages the latch on a block breach, and journals tier transitions.
// Called from the account-summary success path — observation cadence is
// usage cadence; there is deliberately no new scheduler in v1. The scope is
// the caller's connected broker identity: an observation from an unresolved
// scope, a non-live mode, or a different account is refused and journaled,
// never folded into the peak.
func (st *riskCapitalStore) Observe(equityBase float64, asOf time.Time, c *risk.Constitution, scope brokerStateScope) bool {
	if st == nil || equityBase <= 0 || asOf.IsZero() {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()

	now := st.now()
	if reason := st.observationScopeRejectionLocked(scope); reason != "" {
		st.journalScopeRejectionLocked(scope, equityBase, asOf, reason, c, now)
		return false
	}
	force := false
	if st.state.GenesisAt.IsZero() {
		st.state.GenesisAt = now.UTC()
		force = true
	}
	if st.state.AccountID == "" {
		// The binding must hit disk immediately: until it persists, a
		// mis-pinned daemon sharing this state dir could adopt instead.
		st.state.AccountID = scope.Account
		st.state.AccountMode = scope.Mode
		force = true
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "capital_state_scoped",
			"account": scope.Account, "account_mode": scope.Mode,
			"policy_fingerprint": constitutionFingerprint(c),
		})
	}
	st.state.LastEquityBase = equityBase
	st.state.LastEquityAsOf = asOf
	if st.state.DailyEquity == nil {
		st.state.DailyEquity = make(map[string]float64)
	}
	dayKey := asOf.UTC().Format("2006-01-02")
	_, alreadyObservedToday := st.state.DailyEquity[dayKey]
	st.state.DailyEquity[dayKey] = equityBase
	cutoff := now.UTC().Add(-riskCapitalDailySampleKeep)
	cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	for day := range st.state.DailyEquity {
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil || parsed.Before(cutoff) {
			delete(st.state.DailyEquity, day)
		}
	}

	flows, _, _ := st.effectiveFlowsLocked(c)
	adjusted := equityBase - flows
	if !st.state.Seeded || adjusted > st.state.AdjustedPeakBase {
		// Every peak ratchet is journaled: the peak is monotonic runtime
		// state that a single bad equity observation (e.g. a reconnect-window
		// glitch) can poison, and an unexplained jump must be diagnosable
		// after the fact from the journal alone.
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "adjusted_peak_advanced",
			"from_base": st.state.AdjustedPeakBase, "to_base": adjusted,
			"seed": !st.state.Seeded, "equity_base": equityBase, "equity_as_of": asOf.UTC(),
			"policy_fingerprint": constitutionFingerprint(c),
		})
		st.state.Seeded = true
		st.state.AdjustedPeakBase = adjusted
		st.state.PeakAsOf = asOf
		force = true
	}

	obs := risk.CapitalObservation{EquityBase: equityBase, AsOf: asOf}
	v := risk.EvaluateCapital(c, st.runtimeLocked(c, now), &obs, now)
	if v.Tier == risk.CapitalTierBlock && !st.state.BlockLatched && v.ConsumedPct != nil {
		st.state.LatchEpisodeSeq++
		st.state.BlockLatched = true
		st.state.LatchedAt = now.UTC()
		st.state.LatchConsumedPct = *v.ConsumedPct
		force = true
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "drawdown_block_latched",
			"consumed_pct": *v.ConsumedPct, "enforcement": constitutionEnforcement(c),
			"policy_fingerprint": constitutionFingerprint(c),
		})
	}
	if prev := st.state.LastTier; prev != v.Tier {
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "capital_tier", "from": prev, "to": v.Tier,
			"policy_fingerprint": constitutionFingerprint(c),
		})
		st.state.LastTier = v.Tier
		force = true
	}
	st.persistLocked(force)
	return !alreadyObservedToday
}

// observationScopeRejectionLocked names why an equity observation may not
// touch this capital state, or returns "" when it may. Fail closed: an
// unidentified session is treated exactly like a wrong one.
func (st *riskCapitalStore) observationScopeRejectionLocked(scope brokerStateScope) string {
	if !brokerScopeConcrete(scope) {
		return "scope_unresolved"
	}
	if scope.Mode != rpc.AccountModeLive {
		return "non_live_mode"
	}
	if st.state.AccountID != "" && !strings.EqualFold(st.state.AccountID, scope.Account) {
		return "account_mismatch"
	}
	return ""
}

// journalScopeRejectionLocked records a refused observation once per
// (reason, account) per process — loud enough to diagnose, quiet enough that
// a mis-pinned daemon polling every few seconds cannot flood the journal.
func (st *riskCapitalStore) journalScopeRejectionLocked(scope brokerStateScope, equityBase float64, asOf time.Time, reason string, c *risk.Constitution, now time.Time) {
	key := reason + "\x00" + strings.ToUpper(scope.Account)
	if st.scopeRejectionsJournaled == nil {
		st.scopeRejectionsJournaled = make(map[string]bool)
	}
	if st.scopeRejectionsJournaled[key] {
		return
	}
	st.scopeRejectionsJournaled[key] = true
	st.appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": now.UTC(), "kind": "equity_observation_rejected",
		"reason": reason, "observed_account": scope.Account, "observed_mode": scope.Mode,
		"bound_account": st.state.AccountID, "equity_base": equityBase, "equity_as_of": asOf.UTC(),
		"policy_fingerprint": constitutionFingerprint(c),
	})
	_ = st.persistLocked(true)
}

// CorrectPeak lowers a corrupted adjusted peak to an evidence-anchored value.
// Human-only (caller-verified); reason mandatory. It deliberately never
// touches the latch: the latch records a real engagement, and clearing it
// stays ResetDrawdown's job. Corrections may only lower the peak — higher
// peaks come exclusively from scoped observations.
func (st *riskCapitalStore) CorrectPeak(peakBase float64, peakAsOf time.Time, source, reason string, c *risk.Constitution) (float64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return 0, fmt.Errorf("peak correction requires a reason")
	}
	if peakBase <= 0 {
		return 0, fmt.Errorf("peak correction requires a positive peak value")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if !st.state.Seeded {
		return 0, fmt.Errorf("capital state is not seeded; there is no peak to correct")
	}
	from := st.state.AdjustedPeakBase
	if peakBase >= from {
		return 0, fmt.Errorf("peak correction must lower the peak (current %.2f, requested %.2f); higher peaks come from observations", from, peakBase)
	}
	now := st.now().UTC()
	st.state.AdjustedPeakBase = peakBase
	if !peakAsOf.IsZero() {
		st.state.PeakAsOf = peakAsOf
	} else {
		st.state.PeakAsOf = now
	}
	st.appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": now, "kind": "adjusted_peak_corrected",
		"from_base": from, "to_base": peakBase, "source": source, "reason": reason,
		"peak_as_of": st.state.PeakAsOf, "latch_untouched": st.state.BlockLatched,
		"policy_fingerprint": constitutionFingerprint(c),
	})
	if err := st.persistLocked(true); err != nil {
		return 0, err
	}
	return from, nil
}

// ApplyCapitalEvent journals a declared capital fact and folds it into the
// derived aggregates. A deposit whose effective time precedes the recorded
// peak corrects the peak downward: the peak was observed with the deposit
// already in equity, and an inflated peak would overstate drawdown against
// money that was never earned (never-inflate discipline; the symmetric
// withdrawal case cannot inflate the peak, so no correction is needed).
func (st *riskCapitalStore) ApplyCapitalEvent(p rpc.CapitalEventParams, origin string, refs ...*capitalReconRef) (capitalEventV1, error) {
	return st.ApplyCapitalEventForPolicy(p, origin, nil, refs...)
}

func (st *riskCapitalStore) ApplyCapitalEventForPolicy(p rpc.CapitalEventParams, origin string, c *risk.Constitution, refs ...*capitalReconRef) (capitalEventV1, error) {
	typ := strings.ToLower(strings.TrimSpace(p.Type))
	switch typ {
	case "deposit", "withdrawal":
		if p.AmountBase <= 0 {
			return capitalEventV1{}, fmt.Errorf("capital event amount_base must be positive")
		}
	case "reconcile":
	default:
		return capitalEventV1{}, fmt.Errorf("capital event type %q is invalid; use deposit, withdrawal, or reconcile", p.Type)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()

	now := st.now().UTC()
	ev := capitalEventV1{
		Version: 1, At: now, Type: typ, AmountBase: p.AmountBase,
		EffectiveAt: p.EffectiveAt, Note: strings.TrimSpace(p.Note), Origin: origin,
	}
	if ev.EffectiveAt.IsZero() {
		ev.EffectiveAt = now
	}
	if ev.Type == "reconcile" {
		ev.AmountBase = 0
		if len(refs) > 0 && refs[0] != nil {
			ev.ReportID = strings.TrimSpace(refs[0].ReportID)
			ev.CoverageTo = refs[0].CoverageTo
		}
	}
	if err := st.appendCapitalEvent(ev); err != nil {
		return capitalEventV1{}, err
	}
	switch ev.Type {
	case "deposit":
		st.cumFlowsBase += ev.AmountBase
		st.declaredEvents = append(st.declaredEvents, ev)
		if (c == nil || c.PolicyVersion < 3) && st.state.Seeded && !st.state.PeakAsOf.IsZero() && !st.state.PeakAsOf.Before(ev.EffectiveAt) {
			st.state.AdjustedPeakBase -= ev.AmountBase
		}
	case "withdrawal":
		st.cumFlowsBase -= ev.AmountBase
		st.declaredEvents = append(st.declaredEvents, ev)
	case "reconcile":
		if ev.ReportID != "" {
			if st.reconciledReportIDs == nil {
				st.reconciledReportIDs = make(map[string]struct{})
			}
			st.reconciledReportIDs[ev.ReportID] = struct{}{}
		}
		if ev.At.After(st.lastReconciledAt) {
			st.lastReconciledAt = ev.At
			st.lastReconcileReportID = ev.ReportID
			st.lastReconcileSource = rpc.ReconcileSourceHuman
			if ev.Origin == riskCapitalAutoOrigin {
				st.lastReconcileSource = rpc.ReconcileSourceAutomatic
			}
		}
		if ev.Origin == riskCapitalAutoOrigin && ev.At.After(st.lastAutoExtendedAt) {
			st.lastAutoExtendedAt = ev.At
			st.lastAutoExtendReportID = ev.ReportID
		}
	}
	if err := st.persistLocked(true); err != nil {
		return capitalEventV1{}, err
	}
	return ev, nil
}

// ApplyAutomaticReconcile appends daemon-owned evidence while holding the
// same serialization lock as human capital events. The report id is checked
// and recorded atomically, so concurrent startup/fetch evaluations cannot
// append the same pinned report twice.
func (st *riskCapitalStore) ApplyAutomaticReconcile(reportID string, coverageTo time.Time) (capitalEventV1, bool, error) {
	reportID = strings.TrimSpace(reportID)
	if reportID == "" {
		return capitalEventV1{}, false, fmt.Errorf("automatic reconcile requires a report id")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if _, exists := st.reconciledReportIDs[reportID]; exists {
		return capitalEventV1{}, false, nil
	}
	now := st.now().UTC()
	ev := capitalEventV1{
		Version: 1, At: now, Type: "reconcile", Origin: riskCapitalAutoOrigin,
		ReportID: reportID, CoverageTo: coverageTo,
	}
	if err := st.appendCapitalEvent(ev); err != nil {
		return capitalEventV1{}, false, err
	}
	if st.reconciledReportIDs == nil {
		st.reconciledReportIDs = make(map[string]struct{})
	}
	st.reconciledReportIDs[reportID] = struct{}{}
	st.lastReconciledAt = ev.At
	st.lastReconcileReportID = reportID
	st.lastReconcileSource = rpc.ReconcileSourceAutomatic
	st.lastAutoExtendedAt = ev.At
	st.lastAutoExtendReportID = reportID
	if err := st.persistLocked(true); err != nil {
		return capitalEventV1{}, false, err
	}
	return ev, true, nil
}

type statementCapitalSnapshot struct {
	FlowsBase           float64
	CoverageTo          time.Time
	Flows               []reconFlow
	NudgeConfirmedFlows nudgeConfirmedFlowSnapshot
}

// IncorporateStatementSnapshot installs one fully healthy reconstruction.
// The first v3 incorporation is an activation baseline: existing lines are
// marked seen without changing peak/latch state. Later new deposits use their
// broker value dates for the one-time R4 peak correction.
func (st *riskCapitalStore) IncorporateStatementSnapshot(snap statementCapitalSnapshot) {
	st.mu.Lock()
	st.loadLocked()
	incorporated := make(map[string]struct{}, len(st.state.IncorporatedStatementLineIDs))
	for _, id := range st.state.IncorporatedStatementLineIDs {
		incorporated[id] = struct{}{}
	}
	applied := make(map[string]struct{}, len(st.state.AppliedStatementPeakCorrectionIDs))
	for _, id := range st.state.AppliedStatementPeakCorrectionIDs {
		applied[id] = struct{}{}
	}
	activation := !st.state.StatementAuthorityActive
	for _, flow := range snap.Flows {
		if _, seen := incorporated[flow.id]; seen {
			continue
		}
		incorporated[flow.id] = struct{}{}
		st.state.IncorporatedStatementLineIDs = append(st.state.IncorporatedStatementLineIDs, flow.id)
		if activation || flow.amountBase <= 0 || !st.state.Seeded || st.state.PeakAsOf.IsZero() || utcDateAfter(flow.valueDate, st.state.PeakAsOf) {
			continue
		}
		if _, corrected := applied[flow.id]; corrected {
			continue
		}
		st.state.AdjustedPeakBase -= flow.amountBase
		applied[flow.id] = struct{}{}
		st.state.AppliedStatementPeakCorrectionIDs = append(st.state.AppliedStatementPeakCorrectionIDs, flow.id)
	}
	st.state.StatementAuthorityActive = true
	st.state.StatementFlowsBase = snap.FlowsBase
	st.state.StatementCoverageTo = snap.CoverageTo
	persisted := st.persistLocked(true) == nil
	st.mu.Unlock()
	// The nudge store has its own lock and persistence boundary. Observe only
	// after capital state is installed so neither store is held while writing
	// the other, and never let advisory nudge persistence alter capital truth.
	if persisted && st.observeConfirmedFlows != nil {
		st.observeConfirmedFlows(snap.NudgeConfirmedFlows)
	}
}

func (st *riskCapitalStore) ActivateStatementAuthorityWithoutStatements() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.state.StatementAuthorityActive {
		return
	}
	st.state.StatementAuthorityActive = true
	_ = st.persistLocked(true)
}

// GrantOverride records a one-shot, expiring exception against one named
// control. The caller has already verified a human origin; this layer
// enforces the policy's lifetime cap and journals the grant.
func (st *riskCapitalStore) GrantOverride(p rpc.OverrideParams, c *risk.Constitution) (rpc.OverrideRecord, error) {
	control := strings.TrimSpace(p.Control)
	reason := strings.TrimSpace(p.Reason)
	if control == "" || reason == "" {
		return rpc.OverrideRecord{}, fmt.Errorf("override needs both control and reason")
	}
	if p.Hours <= 0 {
		return rpc.OverrideRecord{}, fmt.Errorf("override hours must be positive")
	}
	if c == nil || c.Override.MaxDurationHours == nil {
		return rpc.OverrideRecord{}, fmt.Errorf("override.max_duration_hours is unapproved; overrides are unavailable until the policy declares the cap")
	}
	if p.Hours > *c.Override.MaxDurationHours {
		return rpc.OverrideRecord{}, fmt.Errorf("override hours %d exceed override.max_duration_hours %d", p.Hours, *c.Override.MaxDurationHours)
	}
	known := false
	for _, l := range risk.ConstitutionLimits(c) {
		if l.Key == control {
			known = true
			break
		}
	}
	if !known {
		return rpc.OverrideRecord{}, fmt.Errorf("override control %q is not a constitution key; safety invariants have no keys and cannot be overridden", control)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now().UTC()
	st.overrideSeq++
	rec := rpc.OverrideRecord{
		ID:                fmt.Sprintf("ov-%s-%d", now.Format("20060102-150405"), st.overrideSeq),
		Control:           control,
		Reason:            reason,
		GrantedAt:         now,
		ExpiresAt:         now.Add(time.Duration(p.Hours) * time.Hour),
		PolicyFingerprint: constitutionFingerprint(c),
		Active:            true,
	}
	st.state.Overrides = append(st.state.Overrides, rec)
	st.appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": now, "kind": "override_granted", "id": rec.ID,
		"control": rec.Control, "reason": rec.Reason, "expires_at": rec.ExpiresAt,
		"policy_fingerprint": rec.PolicyFingerprint,
	})
	if err := st.persistLocked(true); err != nil {
		return rpc.OverrideRecord{}, err
	}
	return rec, nil
}

// ResetDrawdown clears the latch and re-bases the adjusted peak to the
// current equity reading. Human-only (caller-verified); reason mandatory.
func (st *riskCapitalStore) ResetDrawdown(reason string, c *risk.Constitution) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("drawdown reset requires a reason")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now().UTC()
	wasLatched := st.state.BlockLatched
	st.state.BlockLatched = false
	st.state.LatchedAt = time.Time{}
	st.state.LatchConsumedPct = 0
	if st.state.LastEquityBase > 0 {
		flows, _, _ := st.effectiveFlowsLocked(c)
		st.state.AdjustedPeakBase = st.state.LastEquityBase - flows
		st.state.PeakAsOf = st.state.LastEquityAsOf
		st.state.Seeded = true
	}
	st.appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": now, "kind": "drawdown_reset", "reason": reason,
		"was_latched": wasLatched, "policy_fingerprint": constitutionFingerprint(c),
	})
	return st.persistLocked(true)
}

// RecordArtefact journals completion of one declared cadence artefact.
func (st *riskCapitalStore) RecordArtefact(p rpc.ArtefactParams, c *risk.Constitution) (rpc.ArtefactRecord, error) {
	name := strings.ToLower(strings.TrimSpace(p.Artefact))
	var class string
	switch name {
	case "morning":
		class = artefactDeclaredClass(c, func(cc *risk.Constitution) string { return cc.Cadence.Morning.Class })
	case "eod":
		class = artefactDeclaredClass(c, func(cc *risk.Constitution) string { return cc.Cadence.EOD.Class })
	case "weekly":
		class = artefactDeclaredClass(c, func(cc *risk.Constitution) string { return cc.Cadence.Weekly.Class })
	default:
		return rpc.ArtefactRecord{}, fmt.Errorf("artefact %q is invalid; use morning, eod, or weekly", p.Artefact)
	}
	if class == "" {
		return rpc.ArtefactRecord{}, fmt.Errorf("artefact %q is not declared in the risk policy cadence section", name)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now().UTC()
	rec := rpc.ArtefactRecord{
		Artefact: name, Class: class, CompletedAt: now, Note: strings.TrimSpace(p.Note),
		Origin: strings.TrimSpace(p.Origin), BriefFingerprint: strings.TrimSpace(p.BriefFingerprint),
	}
	kept := st.state.Artefacts[:0:0]
	for _, a := range st.state.Artefacts {
		if a.Artefact != name {
			kept = append(kept, a)
		}
	}
	st.state.Artefacts = append(kept, rec)
	journal := map[string]any{
		"version": 1, "at": now, "kind": "artefact_completed", "artefact": name,
		"note": rec.Note, "policy_fingerprint": constitutionFingerprint(c),
	}
	if rec.Origin != "" {
		journal["origin"] = rec.Origin
	}
	if rec.BriefFingerprint != "" {
		journal["brief_fingerprint"] = rec.BriefFingerprint
	}
	st.appendRiskPolicyJournal(journal)
	if err := st.persistLocked(true); err != nil {
		return rpc.ArtefactRecord{}, err
	}
	return rec, nil
}

// Report evaluates the current state under the active constitution for the
// snapshot surface. obs may be nil (no fresh reading): the persisted last
// equity serves with its own timestamp so staleness is honest.
func (st *riskCapitalStore) Report(c *risk.Constitution, obs *risk.CapitalObservation) rpc.CapitalStateReport {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.reportLocked(c, obs)
}

type riskCapitalNudgeSnapshot struct {
	Report     rpc.CapitalStateReport
	LatchOpen  bool
	Episode    string
	OccurredAt time.Time
}

// NudgeSnapshot captures the policy-derived capital report and latch episode
// from one state generation. Snapshot composition cannot pair an old capital
// magnitude with a reset or rearmed latch.
func (st *riskCapitalStore) NudgeSnapshot(c *risk.Constitution, obs *risk.CapitalObservation) riskCapitalNudgeSnapshot {
	if st == nil {
		return riskCapitalNudgeSnapshot{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.nudgeCaptureHook != nil {
		st.nudgeCaptureHook()
	}
	report := st.reportLocked(c, obs)
	open, episode, occurredAt := st.nudgeLatchLocked()
	return riskCapitalNudgeSnapshot{Report: report, LatchOpen: open, Episode: episode, OccurredAt: occurredAt}
}

func (st *riskCapitalStore) reportLocked(c *risk.Constitution, obs *risk.CapitalObservation) rpc.CapitalStateReport {
	now := st.now()

	if obs == nil && st.state.LastEquityBase > 0 {
		obs = &risk.CapitalObservation{EquityBase: st.state.LastEquityBase, AsOf: st.state.LastEquityAsOf}
	}
	_, statementFlows, flowSource := st.effectiveFlowsLocked(c)
	v := risk.EvaluateCapital(c, st.runtimeLocked(c, now), obs, now)

	rep := rpc.CapitalStateReport{
		Tier:                     v.Tier,
		Enforcement:              constitutionEnforcement(c),
		EquityStale:              v.EquityStale,
		EffectiveRiskCapitalBase: v.EffectiveRiskCapitalBase,
		DrawdownBase:             v.DrawdownBase,
		ConsumedPct:              v.ConsumedPct,
		BlockLatched:             st.state.BlockLatched,
		LatchedAt:                st.state.LatchedAt,
		LatchConsumedPct:         latchConsumedPct(st.state),
		LastReconciledAt:         st.lastReconciledAt,
		LastReconcileReportID:    st.lastReconcileReportID,
		LastReconcileSource:      st.lastReconcileSource,
		ReconcileStale:           v.ReconcileStale,
		Reasons:                  v.Reasons,
	}
	declared := st.cumFlowsBase
	rep.DeclaredCumFlowsBase = &declared
	rep.FlowSource = flowSource
	if c != nil && c.PolicyVersion >= 3 {
		rep.StatementCumFlowsBase = &statementFlows
	}
	if c != nil {
		rep.BaseCurrency = c.Capital.BaseCurrency
	}
	if obs != nil {
		rep.EquityBase = &obs.EquityBase
		rep.EquityAsOf = obs.AsOf
	}
	if st.state.Seeded {
		peak := st.state.AdjustedPeakBase
		// Preserve the existing wire field's declared-ledger meaning. The
		// evaluator receives effectiveFlows; the additive dual-compute fields
		// disclose both inputs and FlowSource identifies the selected one.
		flows := declared
		rep.AdjustedPeakBase = &peak
		rep.PeakAsOf = st.state.PeakAsOf
		rep.CumExternalFlowsBase = &flows
	}
	return rep
}

// latchConsumedPct discloses the consumed share recorded at latch engagement,
// so a later data glitch inflating the live consumed percentage cannot
// retroactively misrepresent why the latch fired.
func latchConsumedPct(state riskCapitalStateFileV1) *float64 {
	if !state.BlockLatched || state.LatchConsumedPct == 0 {
		return nil
	}
	pct := state.LatchConsumedPct
	return &pct
}

// ActiveOverrides prunes expired overrides and returns the full record list
// (active first ordering is the caller's concern; expiry is journaled once).
func (st *riskCapitalStore) ActiveOverrides() []rpc.OverrideRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now()
	changed := false
	for i := range st.state.Overrides {
		o := &st.state.Overrides[i]
		if o.Active && now.After(o.ExpiresAt) {
			o.Active = false
			changed = true
			st.appendRiskPolicyJournal(map[string]any{
				"version": 1, "at": now.UTC(), "kind": "override_expired", "id": o.ID, "control": o.Control,
			})
		}
	}
	if changed {
		st.persistLocked(true)
	}
	out := make([]rpc.OverrideRecord, len(st.state.Overrides))
	copy(out, st.state.Overrides)
	return out
}

// Artefacts returns the latest completion per declared artefact.
func (st *riskCapitalStore) Artefacts() []rpc.ArtefactRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	out := make([]rpc.ArtefactRecord, len(st.state.Artefacts))
	copy(out, st.state.Artefacts)
	return out
}

// OverridesSnapshot returns the persisted override rows without expiring or
// journaling them. Read-only compositions use this instead of ActiveOverrides,
// whose expiry maintenance is intentionally write-bearing.
func (st *riskCapitalStore) OverridesSnapshot() []rpc.OverrideRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	out := make([]rpc.OverrideRecord, len(st.state.Overrides))
	copy(out, st.state.Overrides)
	return out
}

// UnreconciledClock returns the evaluator's exact deadline projection without
// mutating runtime state.
func (st *riskCapitalStore) UnreconciledClock(c *risk.Constitution, now time.Time) risk.UnreconciledClock {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	var maxDays *int
	if c != nil {
		maxDays = c.Capital.MaxUnreconciledDays
	}
	rt := st.runtimeLocked(c, now)
	return risk.EvaluateUnreconciledClock(maxDays, rt.LastReconciledAt, rt.UnreconciledOverrideUntil, now)
}

// NudgeLatch returns only an opaque episode identity plus the authoritative
// open/occurred facts. It never exposes capital values or changes latch state.
func (st *riskCapitalStore) NudgeLatch() (open bool, episode string, occurredAt time.Time) {
	if st == nil {
		return false, "", time.Time{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.nudgeLatchLocked()
}

func (st *riskCapitalStore) nudgeLatchLocked() (open bool, episode string, occurredAt time.Time) {
	if !st.state.BlockLatched || st.state.LatchedAt.IsZero() {
		return false, "", time.Time{}
	}
	sequence := st.state.LatchEpisodeSeq
	if sequence == 0 {
		sequence = 1
	}
	return true, opaqueIdentity("drawdown-latch", st.state.LatchedAt.UTC().Format(time.RFC3339Nano), fmt.Sprintf("%d", sequence)), st.state.LatchedAt
}

// LastEquity returns the persisted last equity observation for the recon
// equity-divergence check; zero when never observed.
func (st *riskCapitalStore) LastEquity() (float64, time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.state.LastEquityBase, st.state.LastEquityAsOf
}

// DailySample returns the runtime equity sample for one UTC day key.
func (st *riskCapitalStore) DailySample(day string) (float64, bool) {
	if st == nil {
		return 0, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	equity, ok := st.state.DailyEquity[day]
	return equity, ok
}

// capitalReplayContext is the read-only snapshot the backtest replay
// consumes.
type capitalReplayContext struct {
	GenesisAt        time.Time
	Seeded           bool
	AdjustedPeakBase float64
	PeakAsOf         time.Time
	LatchedAt        time.Time
	CumFlowsBase     float64
	DailyEquity      map[string]float64 // copy
}

// ReplayContext returns an isolated copy of the runtime facts used by the
// capital-ladder backtest.
func (st *riskCapitalStore) ReplayContext() capitalReplayContext {
	if st == nil {
		return capitalReplayContext{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	ctx := capitalReplayContext{
		GenesisAt:        st.state.GenesisAt,
		Seeded:           st.state.Seeded,
		AdjustedPeakBase: st.state.AdjustedPeakBase,
		PeakAsOf:         st.state.PeakAsOf,
		LatchedAt:        st.state.LatchedAt,
		CumFlowsBase:     st.cumFlowsBase,
	}
	if len(st.state.DailyEquity) > 0 {
		ctx.DailyEquity = make(map[string]float64, len(st.state.DailyEquity))
		maps.Copy(ctx.DailyEquity, st.state.DailyEquity)
	}
	return ctx
}

func (st *riskCapitalStore) CapitalFlowEventsContext(ctx context.Context, checkpoint func(string) error) ([]capitalEventV1, error) {
	if checkpoint != nil {
		if err := checkpoint("capital_events_start"); err != nil {
			return nil, err
		}
	} else if err := ctx.Err(); err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("risk capital store is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	out := make([]capitalEventV1, 0, len(st.capitalEvents))
	if st.core == nil {
		// Explicit legacy unit/import helper; a started daemon is core-bound.
		out = append(out, replayCapitalEvents().declaredEvents...)
		return out, nil
	}
	for _, event := range st.capitalEvents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Type == "deposit" || event.Type == "withdrawal" {
			out = append(out, event)
		}
	}
	return out, nil
}

func (st *riskCapitalStore) GovernanceEventPayloads(ctx context.Context) ([][]byte, error) {
	if st == nil || st.core == nil {
		return nil, fmt.Errorf("risk governance SQLite authority is unavailable")
	}
	events, err := loadAllCoreEvents(ctx, st.core, coreEventRiskPolicy)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(events))
	for _, event := range events {
		out = append(out, append([]byte(nil), event.PayloadJSON...))
	}
	return out, nil
}

func (st *riskCapitalStore) LastAutoExtend() (string, time.Time) {
	if st == nil {
		return "", time.Time{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.lastAutoExtendReportID, st.lastAutoExtendedAt
}

func (st *riskCapitalStore) EnsureLoaded() {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
}

func artefactDeclaredClass(c *risk.Constitution, pick func(*risk.Constitution) string) string {
	if c == nil {
		return ""
	}
	return pick(c)
}

func constitutionFingerprint(c *risk.Constitution) string {
	if c == nil {
		return ""
	}
	return c.FingerprintKey()
}

func constitutionEnforcement(c *risk.Constitution) string {
	if c == nil {
		return risk.EnforcementShadow
	}
	return c.EffectiveBlockEnforcement()
}

func appendCapitalEvent(ev capitalEventV1) error {
	path, err := defaultTradingStatePath(capitalEventsJournalFile)
	if err != nil {
		return err
	}
	if err := ensurePrivateStateDir(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(ev)
}

// appendRiskPolicyJournal is the legacy file-backed test/import seam. Runtime
// governance events use the riskCapitalStore method and daemon.db. The payload
// always carries the policy fingerprint so replay can prove which exact policy
// produced a transition. Best-effort: journaling never fails the caller.
func appendRiskPolicyJournal(entry map[string]any) {
	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		return
	}
	if err := ensurePrivateStateDir(path); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(entry)
}

func (st *riskCapitalStore) RecordGovernanceEvent(entry map[string]any) error {
	if st == nil {
		return fmt.Errorf("risk capital persistence is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	st.appendRiskPolicyJournal(entry)
	return st.persistLocked(true)
}

// journalRiskPolicyTransition records manager status transitions
// (active/absent/drift/error) with full policy identity.
func (s *Server) journalRiskPolicyTransition(prev, next string, c *risk.Constitution) {
	entry := map[string]any{
		"version": 1, "at": time.Now().UTC(), "kind": "policy_status", "from": prev, "to": next,
	}
	if c != nil {
		entry["policy_id"] = c.PolicyID
		entry["policy_version"] = c.PolicyVersion
		entry["fingerprint_version"] = rpc.RiskConstitutionFingerprintVersion
		entry["policy_fingerprint"] = c.FingerprintKey()
	}
	if s.riskCapital != nil {
		_ = s.riskCapital.RecordGovernanceEvent(entry)
	} else {
		// Legacy unit/import helper only. A started daemon always binds the
		// risk capital store before policy reload can emit transitions.
		appendRiskPolicyJournal(entry)
	}
}
