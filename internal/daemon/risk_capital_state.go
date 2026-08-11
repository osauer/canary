package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// Runtime capital state for the risk constitution (internal-docs/design/risk-policy.md):
// Authority split: risk-policy.toml owns the numbers, internal/risk owns the
// broker evidence. A versioned CAS document owns what those sources cannot
// overrides. Legacy files below are importer/test
// oracles only after cutover.
// Nothing in this file may influence submit eligibility, blockers, freeze,
// pins, or tokens: v1 is advisory/shadow end to end.

const (
	riskCapitalStateFile     = "risk-capital-state.json"
	capitalEventsJournalFile = "capital-events.jsonl"
	riskPolicyJournalFile    = "risk-policy-journal.jsonl"
	riskCapitalStateVer      = 1
	riskCapitalSQLiteDocVer  = 2
	// riskCapitalDailySampleKeep bounds the per-day equity sample cache
	riskCapitalDailySampleKeep = 45 * 24 * time.Hour
	// riskCapitalPersistEvery throttles equity-cache persistence; latch,
	// peak, and event writes always persist immediately.
	riskCapitalPersistEvery = time.Minute
	riskCapitalAutoOrigin   = "daemon-auto"
	riskCapitalScopeVersion = "risk-capital-scope-v1"
)

var errRiskCapitalScopeUnresolved = errors.New("risk capital state requires one concrete account and mode")
var errLegacyRiskCapitalScopeUnresolved = errors.New("legacy risk capital safety state has no account binding; refusing to assign it to an account")

type riskCapitalStateFileV1 struct {
	Version   int       `json:"version"`
	GenesisAt time.Time `json:"genesis_at,omitzero"`
	Seeded    bool      `json:"seeded"`
	// AccountID/AccountMode bind this capital document to one broker identity.
	// SQLite gives every account/mode its own document; the legacy file helper
	// A non-live observation can never write a live peak
	// state dir ratcheted the live peak with the paper account's equity).
	AccountID        string             `json:"account_id,omitempty"`
	AccountMode      string             `json:"account_mode,omitempty"`
	AdjustedPeakBase float64            `json:"adjusted_peak_base"`
	PeakAsOf         time.Time          `json:"peak_as_of,omitzero"`
	LastEquityBase   float64            `json:"last_equity_base"`
	LastEquityAsOf   time.Time          `json:"last_equity_as_of,omitzero"`
	DailyEquity      map[string]float64 `json:"daily_equity,omitempty"`
	LastTier         string             `json:"last_tier,omitempty"`
	BlockLatched     bool               `json:"block_latched"`
	LatchedAt        time.Time          `json:"latched_at,omitzero"`
	LatchEpisodeSeq  uint64             `json:"latch_episode_seq,omitempty"`
	LatchConsumedPct float64            `json:"latch_consumed_pct,omitempty"`
	// LatchProvisional marks a latch the statement window covering its day
	// has not yet decided; absent on pre-two-stage latches, which therefore
	// stay durable. LatchEquityBase freezes the engagement equity so the
	// statement replay can never dissolve a latch on mark recovery.
	LatchProvisional                  bool                 `json:"latch_provisional,omitempty"`
	LatchEquityBase                   float64              `json:"latch_equity_base,omitempty"`
	Overrides                         []rpc.OverrideRecord `json:"overrides,omitempty"`
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
	scope                  brokerStateScope
	scopeKey               string
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
	legacyHasUnscopedSafety  bool
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
	events, err := loadCoreEventsForScope(ctx, core, daemonStateScope, coreEventCapital)
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
	legacyScope := riskCapitalDocumentScope(persisted)
	if brokerScopeConcrete(legacyScope) {
		if err := migrateLegacyRiskCapitalScope(ctx, core, legacyScope, persisted); err != nil {
			return err
		}
	}
	st.mu.Lock()
	st.core, st.revision, st.loaded = core, revision, true
	st.scope, st.scopeKey = brokerStateScope{}, daemonStateScope
	st.legacyHasUnscopedSafety = !brokerScopeConcrete(legacyScope) && riskCapitalAuthorityHasSafetyContinuity(persisted, capitalEvents)
	st.capitalEvents = append([]capitalEventV1(nil), capitalEvents...)
	st.installSQLiteDocumentLocked(persisted)
	st.committed = cloneRiskCapitalDocument(persisted)
	st.committedCapitalEvents = append([]capitalEventV1(nil), capitalEvents...)
	st.mu.Unlock()
	return nil
}

func riskCapitalDocumentScope(doc riskCapitalSQLiteDocument) brokerStateScope {
	mode := strings.ToLower(strings.TrimSpace(doc.State.AccountMode))
	if mode == "" && brokerScopeAccountConcrete(doc.State.AccountID) {
		// Account binding predates the mode field. Observe has always accepted
		// live state only, so this legacy value has one safe interpretation.
		mode = rpc.AccountModeLive
	}
	return brokerStateScope{Account: strings.ToUpper(strings.TrimSpace(doc.State.AccountID)), Mode: mode}
}

func riskCapitalScopeKey(scope brokerStateScope) (string, error) {
	if !brokerScopeConcrete(scope) {
		return "", errRiskCapitalScopeUnresolved
	}
	return riskCapitalScopeVersion + ":" + strings.TrimPrefix(opaqueIdentity(riskCapitalScopeVersion,
		strings.ToUpper(strings.TrimSpace(scope.Account)), strings.ToLower(strings.TrimSpace(scope.Mode))), "sha256:"), nil
}

func riskCapitalAuthorityHasSafetyContinuity(doc riskCapitalSQLiteDocument, events []capitalEventV1) bool {
	s := doc.State
	return len(events) > 0 || s.Seeded || s.BlockLatched || s.AdjustedPeakBase != 0 || s.LastEquityBase != 0 ||
		!s.GenesisAt.IsZero() || !s.PeakAsOf.IsZero() || !s.LastEquityAsOf.IsZero() || len(s.DailyEquity) > 0 ||
		len(s.Overrides) > 0 || s.StatementAuthorityActive || s.StatementFlowsBase != 0 ||
		!s.StatementCoverageTo.IsZero() || len(s.IncorporatedStatementLineIDs) > 0 || len(s.AppliedStatementPeakCorrectionIDs) > 0
}

// migrateLegacyRiskCapitalScope copies the singleton document and its events
// into the one account/mode scope named inside that document. The singleton
// remains immutable compatibility evidence; all later writes use the scoped
// authority. Existing scoped state always wins, making restart idempotent.
func migrateLegacyRiskCapitalScope(ctx context.Context, core *corestore.Store, scope brokerStateScope, legacy riskCapitalSQLiteDocument) error {
	scopeKey, err := riskCapitalScopeKey(scope)
	if err != nil {
		return err
	}
	if _, ok, err := core.GetStateDocument(ctx, scopeKey, stateKindRiskCapital); err != nil {
		return fmt.Errorf("inspect scoped risk capital state: %w", err)
	} else if ok {
		return nil
	}
	legacy.State.AccountID = strings.ToUpper(strings.TrimSpace(scope.Account))
	legacy.State.AccountMode = strings.ToLower(strings.TrimSpace(scope.Mode))
	raw, err := json.Marshal(legacy)
	if err != nil {
		return fmt.Errorf("encode scoped risk capital state: %w", err)
	}
	inputs, err := legacyRiskCapitalEventInputs(ctx, core, scopeKey)
	if err != nil {
		return err
	}
	update := corestore.StateDocumentCAS{ScopeKey: scopeKey, Kind: stateKindRiskCapital, JSON: raw}
	if len(inputs) == 0 {
		_, err = core.CompareAndSwapStateDocument(ctx, update)
	} else {
		_, _, err = core.CompareAndSwapStateDocumentWithEvents(ctx, update, inputs)
	}
	if err != nil {
		return fmt.Errorf("migrate risk capital state to account scope: %w", err)
	}
	return nil
}

func legacyRiskCapitalEventInputs(ctx context.Context, core *corestore.Store, scopeKey string) ([]corestore.EventInput, error) {
	type sequencedInput struct {
		seq   int64
		input corestore.EventInput
	}
	var all []sequencedInput
	for _, eventType := range []string{coreEventCapital, coreEventRiskPolicy} {
		events, err := loadCoreEventsForScope(ctx, core, daemonStateScope, eventType)
		if err != nil {
			return nil, fmt.Errorf("load legacy %s events: %w", eventType, err)
		}
		for _, event := range events {
			input := corestore.EventInput{
				ScopeKey: scopeKey, EventKey: event.EventKey, Type: event.Type,
				Action: "scope_migration", Origin: coreEventOriginDaemon,
				OccurredAt: event.OccurredAt, PayloadJSON: append([]byte(nil), event.PayloadJSON...),
			}
			switch event.Type {
			case coreEventCapital:
				var capital capitalEventV1
				if err := json.Unmarshal(event.PayloadJSON, &capital); err != nil || capital.Version != 1 {
					return nil, fmt.Errorf("decode legacy capital event %d", event.EventSeq)
				}
				input.Projection.CapitalEvent = &corestore.CapitalEventProjection{
					Kind: capital.Type, AmountBaseText: strconv.FormatFloat(capital.AmountBase, 'g', -1, 64),
					EffectiveAt: capital.EffectiveAt.UTC().Format(time.RFC3339Nano), ReportID: capital.ReportID,
				}
			case coreEventRiskPolicy:
				var header struct {
					Kind              string `json:"kind"`
					PolicyID          string `json:"policy_id"`
					PolicyFingerprint string `json:"policy_fingerprint"`
					PolicyVersion     *int64 `json:"policy_version"`
				}
				if err := json.Unmarshal(event.PayloadJSON, &header); err != nil || strings.TrimSpace(header.Kind) == "" {
					return nil, fmt.Errorf("decode legacy risk policy event %d", event.EventSeq)
				}
				input.Projection.RiskPolicyEvent = &corestore.RiskPolicyEventProjection{
					Kind: header.Kind, PolicyID: header.PolicyID, PolicyVersion: header.PolicyVersion,
					PolicyFingerprint: header.PolicyFingerprint,
				}
			}
			all = append(all, sequencedInput{seq: event.EventSeq, input: input})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	out := make([]corestore.EventInput, 0, len(all))
	for _, item := range all {
		out = append(out, item.input)
	}
	return out, nil
}

func loadCoreEventsForScope(ctx context.Context, core *corestore.Store, scopeKey, eventType string) ([]corestore.EventRecord, error) {
	var out []corestore.EventRecord
	var after int64
	for {
		page, err := core.LoadEvents(ctx, corestore.EventQuery{ScopeKey: scopeKey, Type: eventType, AfterEventSeq: after, Limit: 10000})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < 10000 {
			return out, nil
		}
		after = page[len(page)-1].EventSeq
	}
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
	if st.state.BlockLatched && st.state.LatchEpisodeSeq == 0 {
		st.state.LatchEpisodeSeq = 1
	}
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

// selectScopeLocked installs the current document for one explicit account
// writes from the old account are committed before the new account can load.
func (st *riskCapitalStore) selectScopeLocked(scope brokerStateScope) error {
	if st.core == nil {
		return nil
	}
	scopeKey, err := riskCapitalScopeKey(scope)
	if err != nil {
		return err
	}
	if st.scopeKey == scopeKey && sameBrokerScope(st.scope, scope) {
		return nil
	}
	if st.legacyHasUnscopedSafety {
		return errLegacyRiskCapitalScopeUnresolved
	}
	if st.scopeKey != "" && st.scopeKey != daemonStateScope {
		if err := st.persistLocked(true); err != nil {
			return fmt.Errorf("commit previous risk capital scope: %w", err)
		}
	}
	doc := riskCapitalSQLiteDocument{
		Version: riskCapitalSQLiteDocVer,
		State: riskCapitalStateFileV1{
			Version: riskCapitalStateVer, AccountID: strings.ToUpper(strings.TrimSpace(scope.Account)),
			AccountMode: strings.ToLower(strings.TrimSpace(scope.Mode)),
		},
	}
	revision := int64(0)
	persisted, ok, err := st.core.GetStateDocument(context.Background(), scopeKey, stateKindRiskCapital)
	if err != nil {
		return fmt.Errorf("load scoped risk capital state: %w", err)
	}
	if ok {
		if err := json.Unmarshal(persisted.JSON, &doc); err != nil || doc.Version != riskCapitalSQLiteDocVer || doc.State.Version != riskCapitalStateVer {
			if err == nil {
				err = fmt.Errorf("unsupported capital document version %d/state version %d", doc.Version, doc.State.Version)
			}
			return fmt.Errorf("decode scoped risk capital state: %w", err)
		}
		docScope := riskCapitalDocumentScope(doc)
		if !sameBrokerScope(docScope, scope) {
			return fmt.Errorf("scoped risk capital document identity mismatch")
		}
		revision = persisted.Revision
	}
	events, err := loadCoreEventsForScope(context.Background(), st.core, scopeKey, coreEventCapital)
	if err != nil {
		return fmt.Errorf("load scoped capital events: %w", err)
	}
	capitalEvents := make([]capitalEventV1, 0, len(events))
	for _, event := range events {
		var capital capitalEventV1
		if err := json.Unmarshal(event.PayloadJSON, &capital); err != nil || capital.Version != 1 {
			return fmt.Errorf("decode scoped capital event %d", event.EventSeq)
		}
		capitalEvents = append(capitalEvents, capital)
	}
	st.scope = brokerStateScope{Account: strings.ToUpper(strings.TrimSpace(scope.Account)), Mode: strings.ToLower(strings.TrimSpace(scope.Mode))}
	st.scopeKey = scopeKey
	st.revision = revision
	st.capitalEvents = append([]capitalEventV1(nil), capitalEvents...)
	st.pendingEvents = nil
	st.installSQLiteDocumentLocked(doc)
	st.committed = cloneRiskCapitalDocument(doc)
	st.committedCapitalEvents = append([]capitalEventV1(nil), capitalEvents...)
	return nil
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
			ScopeKey: st.scopeKey,
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
				ScopeKey: st.scopeKey,
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
			if st.scopeKey == "" || st.scopeKey == daemonStateScope {
				err = errRiskCapitalScopeUnresolved
			} else {
				update := corestore.StateDocumentCAS{ScopeKey: st.scopeKey, Kind: stateKindRiskCapital, ExpectedRevision: st.revision, JSON: raw}
				if len(st.pendingEvents) > 0 {
					saved, _, err = st.core.CompareAndSwapStateDocumentWithEvents(context.Background(), update, st.pendingEvents)
				} else {
					saved, err = st.core.CompareAndSwapStateDocument(context.Background(), update)
				}
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
		LatchProvisional:          st.state.LatchProvisional,
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
// Called from the account-summary success path — observation cadence is
// the caller's connected broker identity: SQLite selects that account's own
// document; an unresolved or non-live scope is refused. The legacy file helper
// also refuses a different account after first adoption.
func (st *riskCapitalStore) Observe(equityBase float64, asOf time.Time, c *risk.Constitution, scope brokerStateScope) bool {
	if st == nil || equityBase <= 0 || asOf.IsZero() {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()

	now := st.now()
	if st.core != nil {
		// Preserve the existing live-only observation guard before selecting a
		// document. A paper observation cannot even create a live-like ladder.
		if !brokerScopeConcrete(scope) || scope.Mode != rpc.AccountModeLive {
			return false
		}
		if err := st.selectScopeLocked(scope); err != nil {
			return false
		}
	}
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
		// glitch) can poison, and an unexplained jump must be diagnosable
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
		// Engagement is provisional: the breach journals and alerts now,
		// and the statement window covering the latch day later dissolves
		// it (an external flow explains the drop) or promotes it to durable.
		st.state.LatchProvisional = true
		st.state.LatchEquityBase = equityBase
		force = true
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "drawdown_block_latched",
			"consumed_pct": *v.ConsumedPct, "enforcement": constitutionEnforcement(c),
			"provisional":        true,
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
// stays ResetDrawdown's job. Corrections may only lower the peak — higher
func (st *riskCapitalStore) CorrectPeak(peakBase float64, peakAsOf time.Time, source, reason string, c *risk.Constitution) (float64, error) {
	return st.CorrectPeakForScope(peakBase, peakAsOf, source, reason, c, brokerStateScope{})
}

func (st *riskCapitalStore) CorrectPeakForScope(peakBase float64, peakAsOf time.Time, source, reason string, c *risk.Constitution, scope brokerStateScope) (float64, error) {
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
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return 0, err
		}
	}
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
// money that was never earned (never-inflate discipline; the symmetric
func (st *riskCapitalStore) ApplyCapitalEvent(p rpc.CapitalEventParams, origin string, refs ...*capitalReconRef) (capitalEventV1, error) {
	return st.ApplyCapitalEventForPolicy(p, origin, nil, refs...)
}

func (st *riskCapitalStore) ApplyCapitalEventForPolicy(p rpc.CapitalEventParams, origin string, c *risk.Constitution, refs ...*capitalReconRef) (capitalEventV1, error) {
	return st.ApplyCapitalEventForPolicyScope(p, origin, c, brokerStateScope{}, refs...)
}

func (st *riskCapitalStore) ApplyCapitalEventForPolicyScope(p rpc.CapitalEventParams, origin string, c *risk.Constitution, scope brokerStateScope, refs ...*capitalReconRef) (capitalEventV1, error) {
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
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return capitalEventV1{}, err
		}
	}

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
	return st.ApplyAutomaticReconcileForScope(reportID, coverageTo, brokerStateScope{})
}

func (st *riskCapitalStore) ApplyAutomaticReconcileForScope(reportID string, coverageTo time.Time, scope brokerStateScope) (capitalEventV1, bool, error) {
	reportID = strings.TrimSpace(reportID)
	if reportID == "" {
		return capitalEventV1{}, false, fmt.Errorf("automatic reconcile requires a report id")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return capitalEventV1{}, false, err
		}
	}
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
	Scope      brokerStateScope
	FlowsBase  float64
	CoverageTo time.Time
	Flows      []reconFlow
	// EquityDayTotals maps calendar days ("2006-01-02") to the broker
	// statement's official end-of-day equity in base currency, already
	// filtered to this reconciliation scope. The legacy-latch replay uses
	// the latch day's row as its engagement-equity reconstruction.
	EquityDayTotals     map[string]float64
	NudgeConfirmedFlows nudgeConfirmedFlowSnapshot
}

// IncorporateStatementSnapshotForScope installs one fully healthy
// reconstruction: statement-authoritative flows and coverage, the one-time R4
// peak corrections keyed by broker value dates, and — once coverage reaches
// the latch day — the provisional-latch decision.
func (st *riskCapitalStore) IncorporateStatementSnapshotForScope(snap statementCapitalSnapshot, c *risk.Constitution) error {
	st.mu.Lock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(snap.Scope); err != nil {
			st.mu.Unlock()
			return err
		}
	}
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
	st.resolveProvisionalLatchLocked(snap, c)
	st.resolveLegacyLatchLocked(snap, c)
	persistErr := st.persistLocked(true)
	persisted := persistErr == nil
	st.mu.Unlock()
	// The nudge store has its own lock and persistence boundary. Observe only
	// the other, and never let advisory nudge persistence alter capital truth.
	if persisted && st.observeConfirmedFlows != nil {
		st.observeConfirmedFlows(snap.NudgeConfirmedFlows)
	}
	if !persisted {
		return persistErr
	}
	return nil
}

// resolveProvisionalLatchLocked decides a provisional latch once statement
// coverage includes the latch day. The engagement equity is replayed against
// statement-true flows value-dated through that day: a drop those flows
// explain dissolves the latch automatically; anything else — trading loss,
// missing policy numbers, incomplete engagement evidence — promotes it to
// durable, which only a human reset clears. The equity term stays frozen at
// engagement, so mark recovery can never dissolve a latch (decision 5), and
// every ambiguity promotes: a wrong promotion returns to the human, a wrong
// dissolution would release the brake.
func (st *riskCapitalStore) resolveProvisionalLatchLocked(snap statementCapitalSnapshot, c *risk.Constitution) {
	if !st.state.BlockLatched || !st.state.LatchProvisional || st.state.LatchedAt.IsZero() {
		return
	}
	if snap.CoverageTo.IsZero() || utcDateBefore(snap.CoverageTo, st.state.LatchedAt) {
		return // the statement window cannot see the latch day yet
	}
	now := st.now().UTC()
	entry := map[string]any{
		"version": 1, "at": now, "latched_at": st.state.LatchedAt,
		"latch_consumed_pct": st.state.LatchConsumedPct, "coverage_to": snap.CoverageTo,
		"policy_fingerprint": constitutionFingerprint(c),
	}
	dissolve := false
	if c != nil && c.Capital.DeclaredRiskCapital != nil && c.Drawdown.BlockConsumedPct != nil && st.state.LatchEquityBase > 0 {
		flows := 0.0
		lineIDs := []string{}
		for _, flow := range snap.Flows {
			if !utcDateAfter(flow.valueDate, st.state.LatchedAt) {
				flows += flow.amountBase
				lineIDs = append(lineIDs, flow.id)
			}
		}
		dd := max(st.state.AdjustedPeakBase-(st.state.LatchEquityBase-flows), 0)
		pct := dd / *c.Capital.DeclaredRiskCapital * 100
		entry["statement_flows_to_latch_base"] = flows
		entry["statement_consumed_pct"] = pct
		// The decision's evidence: which broker statement lines were replayed.
		entry["statement_line_ids"] = lineIDs
		dissolve = pct < *c.Drawdown.BlockConsumedPct
	} else {
		entry["reason"] = "policy numbers or engagement evidence incomplete; promoted for human review"
	}
	if dissolve {
		entry["kind"] = "drawdown_latch_dissolved"
		st.state.BlockLatched = false
		st.state.LatchedAt = time.Time{}
		st.state.LatchProvisional = false
		st.state.LatchConsumedPct = 0
		st.state.LatchEquityBase = 0
	} else {
		entry["kind"] = "drawdown_latch_promoted"
		st.state.LatchProvisional = false
	}
	st.appendRiskPolicyJournal(entry)
}

// resolveLegacyLatchLocked decides a latch engaged before the two-stage
// feature existed. Such a latch carries no provisional marker and no frozen
// engagement equity, so it reads as durable — but durable is meant to be
// the OUTCOME of a statement replay that failed to explain the drop, and a
// pre-feature latch never received that replay. The broker statement's own
// end-of-day equity for the latch day reconstructs the engagement equity
// (it already reflects that day's flows, exactly as the frozen value would
// have), and the replay is otherwise identical to the provisional path:
// flows through the latch day that explain the drop below the block line
// dissolve the latch with the full evidence journaled. Anything less than
// complete evidence — no statement equity row for the exact latch day,
// missing policy numbers, coverage short of the latch day — leaves the
// latch untouched, where the human reset path still applies. A prior-day
// equity row is deliberately NOT a substitute: it predates the drop, so it
// would understate the replayed drawdown and dissolve unsafely. Operator
// decision 2026-08-11: a statement-proven withdrawal must release the
// brake without a human attestation.
func (st *riskCapitalStore) resolveLegacyLatchLocked(snap statementCapitalSnapshot, c *risk.Constitution) {
	if !st.state.BlockLatched || st.state.LatchProvisional || st.state.LatchedAt.IsZero() || st.state.LatchEquityBase > 0 {
		return
	}
	if snap.CoverageTo.IsZero() || utcDateBefore(snap.CoverageTo, st.state.LatchedAt) {
		return
	}
	if c == nil || c.Capital.DeclaredRiskCapital == nil || c.Drawdown.BlockConsumedPct == nil {
		return
	}
	day := st.state.LatchedAt.UTC().Format("2006-01-02")
	equity, ok := snap.EquityDayTotals[day]
	if !ok || equity <= 0 {
		return
	}
	flows := 0.0
	lineIDs := []string{}
	for _, flow := range snap.Flows {
		if !utcDateAfter(flow.valueDate, st.state.LatchedAt) {
			flows += flow.amountBase
			lineIDs = append(lineIDs, flow.id)
		}
	}
	dd := max(st.state.AdjustedPeakBase-(equity-flows), 0)
	pct := dd / *c.Capital.DeclaredRiskCapital * 100
	if pct >= *c.Drawdown.BlockConsumedPct {
		return // not explained: the latch stays durable, human path unchanged
	}
	st.appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": st.now().UTC(), "kind": "drawdown_latch_backfill_dissolved",
		"latched_at": st.state.LatchedAt, "latch_consumed_pct": st.state.LatchConsumedPct,
		"coverage_to": snap.CoverageTo, "statement_equity_day": day,
		"statement_equity_base":         equity,
		"statement_flows_to_latch_base": flows, "statement_consumed_pct": pct,
		"statement_line_ids": lineIDs,
		"policy_fingerprint": constitutionFingerprint(c),
	})
	st.state.BlockLatched = false
	st.state.LatchedAt = time.Time{}
	st.state.LatchConsumedPct = 0
	st.state.LatchEquityBase = 0
}

func (st *riskCapitalStore) ActivateStatementAuthorityWithoutStatements() {
	_ = st.ActivateStatementAuthorityWithoutStatementsForScope(brokerStateScope{})
}

func (st *riskCapitalStore) ActivateStatementAuthorityWithoutStatementsForScope(scope brokerStateScope) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return err
		}
	}
	if st.state.StatementAuthorityActive {
		return nil
	}
	st.state.StatementAuthorityActive = true
	return st.persistLocked(true)
}

// GrantOverride records a one-shot, expiring exception against one named
func (st *riskCapitalStore) GrantOverride(p rpc.OverrideParams, c *risk.Constitution) (rpc.OverrideRecord, error) {
	return st.GrantOverrideForScope(p, c, brokerStateScope{})
}

func (st *riskCapitalStore) GrantOverrideForScope(p rpc.OverrideParams, c *risk.Constitution, scope brokerStateScope) (rpc.OverrideRecord, error) {
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
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return rpc.OverrideRecord{}, err
		}
	}
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
	return st.ResetDrawdownForScope(reason, c, brokerStateScope{})
}

func (st *riskCapitalStore) ResetDrawdownForScope(reason string, c *risk.Constitution, scope brokerStateScope) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("drawdown reset requires a reason")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return err
		}
	}
	now := st.now().UTC()
	wasLatched := st.state.BlockLatched
	wasProvisional := st.state.LatchProvisional
	st.state.BlockLatched = false
	st.state.LatchedAt = time.Time{}
	st.state.LatchConsumedPct = 0
	st.state.LatchProvisional = false
	st.state.LatchEquityBase = 0
	if st.state.LastEquityBase > 0 {
		flows, _, _ := st.effectiveFlowsLocked(c)
		st.state.AdjustedPeakBase = st.state.LastEquityBase - flows
		st.state.PeakAsOf = st.state.LastEquityAsOf
		st.state.Seeded = true
	}
	st.appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": now, "kind": "drawdown_reset", "reason": reason,
		"was_latched": wasLatched, "was_provisional": wasProvisional,
		"policy_fingerprint": constitutionFingerprint(c),
	})
	return st.persistLocked(true)
}

// Report evaluates the current state under the active constitution for the
// caller's connected broker identity, the same one Observe takes: a session
// whose account or mode differs from the document's adopted binding is served
// Tier unknown with the binding disclosed, never a drawdown computed from one
// account's equity against another account's peak (audit A1).
func (st *riskCapitalStore) Report(c *risk.Constitution, obs *risk.CapitalObservation, scope brokerStateScope) rpc.CapitalStateReport {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if !brokerScopeConcrete(scope) {
			return st.unavailableScopeReportLocked(c, "capital state requires one selected broker account")
		}
		if err := st.selectScopeLocked(scope); err != nil {
			return st.unavailableScopeReportLocked(c, riskCapitalPublicScopeReason(err))
		}
	}
	return st.reportLocked(c, obs, scope)
}

func riskCapitalPublicScopeReason(err error) string {
	switch {
	case errors.Is(err, errRiskCapitalScopeUnresolved):
		return "capital state requires one selected broker account"
	case errors.Is(err, errLegacyRiskCapitalScopeUnresolved):
		return "older capital safety state has no account identity; account-scoped capital state is unavailable until it is recovered"
	default:
		return "capital state is unavailable for the selected account"
	}
}

func (st *riskCapitalStore) unavailableScopeReportLocked(c *risk.Constitution, reason string) rpc.CapitalStateReport {
	rep := rpc.CapitalStateReport{
		Tier: risk.CapitalTierUnknown, Enforcement: constitutionEnforcement(c),
		BoundAccount: st.state.AccountID, BlockLatched: st.state.BlockLatched,
		LatchedAt: st.state.LatchedAt, LatchProvisional: st.state.LatchProvisional,
		LatchConsumedPct: latchConsumedPct(st.state),
		Reasons:          []string{reason},
	}
	if c != nil {
		rep.BaseCurrency = c.Capital.BaseCurrency
	}
	return rep
}

type riskCapitalNudgeSnapshot struct {
	Report     rpc.CapitalStateReport
	LatchOpen  bool
	Episode    string
	OccurredAt time.Time
}

// NudgeSnapshot captures the policy-derived capital report and latch episode
func (st *riskCapitalStore) NudgeSnapshot(c *risk.Constitution, obs *risk.CapitalObservation) riskCapitalNudgeSnapshot {
	return st.NudgeSnapshotForScope(c, obs, brokerStateScope{})
}

func (st *riskCapitalStore) NudgeSnapshotForScope(c *risk.Constitution, obs *risk.CapitalObservation, scope brokerStateScope) riskCapitalNudgeSnapshot {
	if st == nil {
		return riskCapitalNudgeSnapshot{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return riskCapitalNudgeSnapshot{Report: st.unavailableScopeReportLocked(c, riskCapitalPublicScopeReason(err))}
		}
	}
	if st.nudgeCaptureHook != nil {
		st.nudgeCaptureHook()
	}
	// The nudge path passes no fresh observation, so the report pairs the
	// document's own persisted equity with its own peak — self-consistent for
	// the bound account regardless of the current session. An empty scope is
	// therefore correct here, not an oversight.
	report := st.reportLocked(c, obs, brokerStateScope{})
	open, episode, occurredAt := st.nudgeLatchLocked()
	return riskCapitalNudgeSnapshot{Report: report, LatchOpen: open, Episode: episode, OccurredAt: occurredAt}
}

func (st *riskCapitalStore) reportLocked(c *risk.Constitution, obs *risk.CapitalObservation, scope brokerStateScope) rpc.CapitalStateReport {
	now := st.now()

	if reason := st.reportScopeMismatchLocked(scope); reason != "" {
		// No drawdown across accounts: computing this document's peak against
		// — a latched block must not vanish from view behind a repin — but
		rep := rpc.CapitalStateReport{
			Tier:             risk.CapitalTierUnknown,
			Enforcement:      constitutionEnforcement(c),
			BoundAccount:     st.state.AccountID,
			BlockLatched:     st.state.BlockLatched,
			LatchedAt:        st.state.LatchedAt,
			LatchProvisional: st.state.LatchProvisional,
			LatchConsumedPct: latchConsumedPct(st.state),
			Reasons:          []string{reason},
		}
		if c != nil {
			rep.BaseCurrency = c.Capital.BaseCurrency
		}
		return rep
	}
	// A fresh observation with no resolvable session identity cannot be
	// attributed to the bound account; the persisted last equity below is
	// self-consistent with the document and serves instead, staleness honest.
	if obs != nil && st.state.AccountID != "" && !brokerScopeConcrete(scope) {
		obs = nil
	}

	if obs == nil && st.state.LastEquityBase > 0 {
		obs = &risk.CapitalObservation{EquityBase: st.state.LastEquityBase, AsOf: st.state.LastEquityAsOf}
	}
	_, statementFlows, flowSource := st.effectiveFlowsLocked(c)
	v := risk.EvaluateCapital(c, st.runtimeLocked(c, now), obs, now)

	rep := rpc.CapitalStateReport{
		Tier:                     v.Tier,
		Enforcement:              constitutionEnforcement(c),
		BoundAccount:             st.state.AccountID,
		EquityStale:              v.EquityStale,
		EffectiveRiskCapitalBase: v.EffectiveRiskCapitalBase,
		DrawdownBase:             v.DrawdownBase,
		ConsumedPct:              v.ConsumedPct,
		BlockLatched:             st.state.BlockLatched,
		LatchedAt:                st.state.LatchedAt,
		LatchProvisional:         st.state.LatchProvisional,
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
		flows := declared
		rep.AdjustedPeakBase = &peak
		rep.PeakAsOf = st.state.PeakAsOf
		rep.CumExternalFlowsBase = &flows
	}
	return rep
}

// reportScopeMismatchLocked names why the calling session may not read this
// pairs the bound account's persisted equity with its own peak, which is
// different account's fresh equity to this document's peak (audit A1).
func (st *riskCapitalStore) reportScopeMismatchLocked(scope brokerStateScope) string {
	if st.state.AccountID == "" || !brokerScopeConcrete(scope) {
		return ""
	}
	if !strings.EqualFold(st.state.AccountID, scope.Account) {
		return "the capital ladder is bound to another account; its drawdown is not computed against this session's equity"
	}
	if st.state.AccountMode != "" && !strings.EqualFold(st.state.AccountMode, scope.Mode) {
		return "the capital ladder is bound to another account mode; its drawdown is not computed against this session's equity"
	}
	return ""
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
func (st *riskCapitalStore) ActiveOverrides() []rpc.OverrideRecord {
	return st.ActiveOverridesForScope(brokerStateScope{})
}

func (st *riskCapitalStore) ActiveOverridesForScope(scope brokerStateScope) []rpc.OverrideRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return nil
		}
	}
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

// OverridesSnapshot returns the persisted override rows without expiring or
// journaling them. Read-only compositions use this instead of ActiveOverrides,
// whose expiry maintenance is intentionally write-bearing.
func (st *riskCapitalStore) OverridesSnapshot() []rpc.OverrideRecord {
	return st.OverridesSnapshotForScope(brokerStateScope{})
}

func (st *riskCapitalStore) OverridesSnapshotForScope(scope brokerStateScope) []rpc.OverrideRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return nil
		}
	}
	out := make([]rpc.OverrideRecord, len(st.state.Overrides))
	copy(out, st.state.Overrides)
	return out
}

// UnreconciledClock returns the evaluator's exact deadline projection without
func (st *riskCapitalStore) UnreconciledClock(c *risk.Constitution, now time.Time) risk.UnreconciledClock {
	return st.UnreconciledClockForScope(c, now, brokerStateScope{})
}

func (st *riskCapitalStore) UnreconciledClockForScope(c *risk.Constitution, now time.Time, scope brokerStateScope) risk.UnreconciledClock {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return risk.UnreconciledClock{}
		}
	}
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
	return st.NudgeLatchForScope(brokerStateScope{})
}

func (st *riskCapitalStore) NudgeLatchForScope(scope brokerStateScope) (open bool, episode string, occurredAt time.Time) {
	if st == nil {
		return false, "", time.Time{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return false, "", time.Time{}
		}
	}
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
	return st.LastEquityForScope(brokerStateScope{})
}

func (st *riskCapitalStore) LastEquityForScope(scope brokerStateScope) (float64, time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return 0, time.Time{}
		}
	}
	return st.state.LastEquityBase, st.state.LastEquityAsOf
}

// DailySample returns the runtime equity sample for one UTC day key.
func (st *riskCapitalStore) DailySample(day string) (float64, bool) {
	return st.DailySampleForScope(day, brokerStateScope{})
}

func (st *riskCapitalStore) DailySampleForScope(day string, scope brokerStateScope) (float64, bool) {
	if st == nil {
		return 0, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return 0, false
		}
	}
	equity, ok := st.state.DailyEquity[day]
	return equity, ok
}

// capitalReplayContext is the read-only snapshot the backtest replay
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
	ctx, _ := st.ReplayContextForScope(brokerStateScope{})
	return ctx
}

func (st *riskCapitalStore) ReplayContextForScope(scope brokerStateScope) (capitalReplayContext, error) {
	if st == nil {
		return capitalReplayContext{}, fmt.Errorf("risk capital store is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return capitalReplayContext{}, err
		}
	}
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
	return ctx, nil
}

func (st *riskCapitalStore) CapitalFlowEventsContext(ctx context.Context, checkpoint func(string) error) ([]capitalEventV1, error) {
	return st.CapitalFlowEventsContextForScope(ctx, checkpoint, brokerStateScope{})
}

func (st *riskCapitalStore) CapitalFlowEventsContextForScope(ctx context.Context, checkpoint func(string) error, scope brokerStateScope) ([]capitalEventV1, error) {
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
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return nil, err
		}
	}
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
	return st.GovernanceEventPayloadsForScope(ctx, brokerStateScope{})
}

func (st *riskCapitalStore) GovernanceEventPayloadsForScope(ctx context.Context, scope brokerStateScope) ([][]byte, error) {
	if st == nil || st.core == nil {
		return nil, fmt.Errorf("risk governance SQLite authority is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if err := st.selectScopeLocked(scope); err != nil {
		return nil, err
	}
	events, err := loadCoreEventsForScope(ctx, st.core, st.scopeKey, coreEventRiskPolicy)
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
	return st.LastAutoExtendForScope(brokerStateScope{})
}

func (st *riskCapitalStore) LastAutoExtendForScope(scope brokerStateScope) (string, time.Time) {
	if st == nil {
		return "", time.Time{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return "", time.Time{}
		}
	}
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
	return st.RecordGovernanceEventForScope(entry, brokerStateScope{})
}

func (st *riskCapitalStore) RecordGovernanceEventForScope(entry map[string]any, scope brokerStateScope) error {
	if st == nil {
		return fmt.Errorf("risk capital persistence is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return err
		}
	}
	st.appendRiskPolicyJournal(entry)
	return st.persistLocked(true)
}

// RecordDeskGovernanceEvent keeps accountless policy-manager lifecycle facts
// in the daemon scope. Account-derived tiers, dismissals, overrides, and
func (st *riskCapitalStore) RecordDeskGovernanceEvent(entry map[string]any) error {
	if st == nil {
		return fmt.Errorf("risk capital persistence is unavailable")
	}
	if st.core == nil {
		appendRiskPolicyJournal(entry)
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	at := st.now().UTC()
	if value, ok := entry["at"].(time.Time); ok && !value.IsZero() {
		at = value.UTC()
	}
	kind := strings.TrimSpace(fmt.Sprint(entry["kind"]))
	if kind == "" {
		kind = "governance_event"
	}
	key, err := coreStoreEventKey(context.Background(), st.core, coreEventRiskPolicy, at, raw, 0)
	if err != nil {
		return err
	}
	projection := corestore.RiskPolicyEventProjection{
		Kind: kind, PolicyID: strings.TrimSpace(fmt.Sprint(entry["policy_id"])),
		PolicyFingerprint: strings.TrimSpace(fmt.Sprint(entry["policy_fingerprint"])),
	}
	if version, ok := integerAny(entry["policy_version"]); ok {
		projection.PolicyVersion = &version
	}
	_, err = st.core.AppendEvents(context.Background(), []corestore.EventInput{{
		ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRiskPolicy,
		Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
		OccurredAt: at, PayloadJSON: raw,
		Projection: corestore.EventProjection{RiskPolicyEvent: &projection},
	}})
	return err
}

// journalRiskPolicyTransition records manager status transitions
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
		_ = s.riskCapital.RecordDeskGovernanceEvent(entry)
	} else {
		// Legacy unit/import helper only. A started daemon always binds the
		// risk capital store before policy reload can emit transitions.
		appendRiskPolicyJournal(entry)
	}
}
