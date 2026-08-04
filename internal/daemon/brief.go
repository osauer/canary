package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

const (
	briefStateFile            = "brief-state.json"
	briefStateVersion         = 1
	briefFingerprintMax       = 256
	briefMoverLimit           = 3
	monthlyRenderReceiptLimit = 64
	monthlyRenderReceiptTTL   = 15 * time.Minute
)

type monthlyRenderReceipt struct {
	Month             string
	AuthorityIdentity string
	IssuedAt          time.Time
	ExpiresAt         time.Time
}

type briefRuleState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type briefStampState struct {
	Fingerprint         string           `json:"fingerprint"`
	At                  time.Time        `json:"at"`
	RulebookFingerprint string           `json:"rulebook_fingerprint,omitempty"`
	Rules               []briefRuleState `json:"rules,omitempty"`
}

type briefStateFileV1 struct {
	Version int                        `json:"version"`
	Stamps  map[string]briefStampState `json:"stamps,omitempty"`
}

type briefStateStore struct {
	mu       sync.Mutex
	path     string // legacy importer/test helper only
	core     *corestore.Store
	revision int64
	loaded   bool
	state    briefStateFileV1
}

func (st *briefStateStore) bindCore(ctx context.Context, core *corestore.Store) error {
	if st == nil || core == nil {
		return fmt.Errorf("brief state SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindBrief)
	if err != nil {
		return fmt.Errorf("load brief state from SQLite: %w", err)
	}
	state := briefStateFileV1{Version: briefStateVersion, Stamps: map[string]briefStampState{}}
	revision := int64(0)
	if ok {
		if err := json.Unmarshal(doc.JSON, &state); err != nil || state.Version != briefStateVersion {
			if err == nil {
				err = fmt.Errorf("unsupported version %d", state.Version)
			}
			return fmt.Errorf("decode brief state from SQLite: %w", err)
		}
		if state.Stamps == nil {
			state.Stamps = map[string]briefStampState{}
		}
		revision = doc.Revision
	} else {
		return fmt.Errorf("brief state is missing from SQLite; cutover bootstrap was not completed")
	}
	st.mu.Lock()
	st.core, st.revision, st.loaded, st.state = core, revision, true, state
	st.mu.Unlock()
	return nil
}

func (s *Server) installBriefStateStore() {
	if s == nil {
		return
	}
	path, err := defaultTradingStatePath(briefStateFile)
	if err != nil {
		s.warnf("brief state: resolve state path: %v (stamp baselines will not survive restart)", err)
	}
	s.briefState = &briefStateStore{path: path}
}

func (st *briefStateStore) loadLocked() {
	if st.loaded {
		return
	}
	st.loaded = true
	st.state = briefStateFileV1{Version: briefStateVersion, Stamps: map[string]briefStampState{}}
	if st.path == "" {
		return
	}
	data, err := osReadFile(st.path)
	if err != nil {
		return
	}
	var persisted briefStateFileV1
	if json.Unmarshal(data, &persisted) == nil && persisted.Version == briefStateVersion {
		st.state = persisted
		if st.state.Stamps == nil {
			st.state.Stamps = map[string]briefStampState{}
		}
	}
}

// osReadFile is a package seam kept variable-free in production; tests use
// ordinary XDG paths and do not need to replace it.
func osReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (st *briefStateStore) latestBaseline() (briefStampState, bool) {
	if st == nil {
		return briefStampState{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	var latest briefStampState
	for _, stamp := range st.state.Stamps {
		if stamp.At.After(latest.At) {
			latest = stamp
		}
	}
	return latest, !latest.At.IsZero()
}

func (st *briefStateStore) stamp(kind, fingerprint string, at time.Time, rules *rpc.RulesResult) error {
	if st == nil || (st.core == nil && st.path == "") {
		return fmt.Errorf("brief state persistence is unavailable")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	stamp := briefStampState{Fingerprint: fingerprint, At: at.UTC()}
	if rules != nil {
		if rules.PolicyFingerprint != nil {
			stamp.RulebookFingerprint = rules.PolicyFingerprint.Key
		}
		stamp.Rules = make([]briefRuleState, 0, len(rules.Rules))
		for _, row := range rules.Rules {
			stamp.Rules = append(stamp.Rules, briefRuleState{ID: row.ID, Status: row.Status})
		}
	}
	next := st.state
	next.Stamps = maps.Clone(st.state.Stamps)
	next.Stamps[kind] = stamp
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if st.core != nil {
		saved, err := st.core.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: stateKindBrief,
			ExpectedRevision: st.revision, JSON: data,
		})
		if err != nil {
			return err
		}
		st.revision = saved.Revision
		st.state = next
		return nil
	}
	if err := writePrivateStateAtomic(st.path, data); err != nil {
		return err
	}
	st.state = next
	return nil
}

func (st *briefStateStore) stampedToday(kind string, now time.Time) (briefStampState, bool) {
	if st == nil {
		return briefStampState{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	stamp, ok := st.state.Stamps[kind]
	return stamp, ok && sameLocalDay(stamp.At, now)
}

func (s *Server) handleBriefSnapshot(ctx context.Context, req *rpc.Request) (*rpc.BriefResult, error) {
	if len(req.Params) > 0 {
		var p rpc.BriefSnapshotParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	res, _ := s.composeBrief(ctx)
	return res, nil
}

func (s *Server) handleBriefAck(ctx context.Context, req *rpc.Request) (*rpc.BriefAckResult, error) {
	var p rpc.BriefAckParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(p.Kind))
	if kind == rpc.BriefKindMonthly {
		return s.handleMonthlyBriefAck(ctx, p)
	}
	// Origin is checked before any composition or store access that could
	// lead to a write. Refused agent/empty requests journal nothing.
	if err := requireHumanRiskPolicyOrigin(p.Origin); err != nil {
		return nil, err
	}
	if kind != rpc.BriefKindMorning && kind != rpc.BriefKindEOD {
		return nil, errBadRequest("brief kind must be morning or eod")
	}
	fingerprint := strings.TrimSpace(p.BriefFingerprint)
	if fingerprint == "" || len(fingerprint) > briefFingerprintMax {
		return nil, errBadRequest(fmt.Sprintf("brief fingerprint must be non-empty and at most %d bytes", briefFingerprintMax))
	}
	now := s.briefNow()
	day := localDay(now)
	scope := s.currentBrokerStateScope()
	if rec, ok := artefactCompletedInPeriod(s.riskCapital.ArtefactsForScope(scope), kind, now); ok {
		return &rpc.BriefAckResult{OK: true, Kind: kind, Day: day, At: rec.CompletedAt,
			AlreadyStamped: true, BriefFingerprint: rec.BriefFingerprint,
			Message: fmt.Sprintf("%s artefact already complete for %s", kind, day)}, nil
	}
	if stamp, ok := s.briefState.stampedToday(kind, now); ok {
		return &rpc.BriefAckResult{OK: true, Kind: kind, Day: day, At: stamp.At,
			AlreadyStamped: true, BriefFingerprint: stamp.Fingerprint,
			Message: fmt.Sprintf("%s brief already stamped for %s", kind, day)}, nil
	}

	// Re-evaluate only the rule set at stamp time for the durable delta
	// baseline. The already-rendered fingerprint is the attested content;
	// there is no second full brief fan-out on the write path.
	rules := s.evaluateRulesMode(ctx, false, false)
	mgr := s.riskPolicies.snapshot()
	rec, err := s.riskCapital.RecordArtefactForScope(rpc.ArtefactParams{
		Artefact: kind, Origin: normalizedWriteOrigin(p.Origin), BriefFingerprint: fingerprint,
	}, mgr.policy, scope)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	if err := s.briefState.stamp(kind, fingerprint, rec.CompletedAt, rules); err != nil {
		return nil, fmt.Errorf("persist brief stamp baseline: %w", err)
	}
	return &rpc.BriefAckResult{OK: true, Kind: kind, Day: day, At: rec.CompletedAt,
		BriefFingerprint: fingerprint, Message: fmt.Sprintf("stamped %s artefact for %s", kind, day)}, nil
}

func (s *Server) handleMonthlyBriefAck(ctx context.Context, p rpc.BriefAckParams) (*rpc.BriefAckResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Monthly completion is narrower than the legacy human-origin stamp: only
	// the authenticated paired foreground route may supply this origin.
	if strings.TrimSpace(p.Origin) != rpc.OrderOriginPairedDevice {
		return nil, errBadRequest("monthly brief completion requires human-paired-device foreground-render evidence; CLI and agent origins are refused")
	}
	fingerprint := strings.TrimSpace(p.BriefFingerprint)
	if fingerprint == "" || len(fingerprint) > briefFingerprintMax {
		return nil, errBadRequest(fmt.Sprintf("brief fingerprint must be non-empty and at most %d bytes", briefFingerprintMax))
	}
	if strings.TrimSpace(p.Evidence) != rpc.BriefAckEvidenceRender {
		return nil, errBadRequest("monthly brief completion requires render evidence")
	}
	if s == nil || s.riskPolicies == nil || s.nudges == nil {
		return nil, errBadRequest("monthly brief completion state is unavailable")
	}
	now := s.briefNow().UTC()
	authority := s.currentNudgeAuthority(now)
	policy := authority.policy
	if !authority.cadenceEligible {
		return nil, errBadRequest("monthly brief completion is unavailable until current active fully approved v4 authority is present")
	}
	report, err := s.buildReconReportContext(ctx)
	if err != nil {
		return nil, err
	}
	day := nudgeLocalDay(policy.Cadence, now)
	evaluation, completion := s.governanceMonthlyPulseForWrite(authority, policy, report, now)
	if strings.TrimSpace(p.Month) != evaluation.Month || evaluation.Month == "" {
		return nil, errBadRequest("monthly brief completion month does not match the current policy month")
	}
	if evaluation.Status == risk.MonthlyPulseStatusCompleted && completion != nil {
		rec, ok := s.nudges.monthlyCompletionRecord(evaluation.Month, authority.policyIdentity)
		if !ok || rec.BriefIdentity != fingerprint {
			return nil, errBadRequest("monthly brief completion conflicts with the pinned rendered brief")
		}
		return &rpc.BriefAckResult{
			OK: true, Kind: rpc.BriefKindMonthly, Day: day, At: completion.CompletedAt,
			AlreadyStamped: true, BriefFingerprint: rec.BriefIdentity, Month: evaluation.Month,
			Evidence: rpc.BriefAckEvidenceRender, Message: "monthly foreground render already recorded",
		}, nil
	}
	if !policyPinsReady(authority.report.Inventory) {
		return nil, errBadRequest("monthly brief completion is unavailable until readable matching policy pins are present")
	}
	if evaluation.Status != risk.MonthlyPulseStatusDue {
		return nil, errBadRequest("monthly brief is not currently due with readable matching policy pins")
	}
	authorityIdentity := monthlyAuthorityIdentity(authority, evaluation.Month, report, now)
	receipt, ok := s.monthlyRenderReceipt(fingerprint, evaluation.Month, authorityIdentity, now)
	if !ok {
		return nil, errBadRequest("monthly brief fingerprint has no current daemon render receipt; render the monthly brief again")
	}
	if s.monthlyAckBeforeWriteLock != nil {
		s.monthlyAckBeforeWriteLock()
	}
	s.nudgeWriteMu.Lock()
	defer s.nudgeWriteMu.Unlock()
	if s.nudgeBeforeCommit != nil {
		s.nudgeBeforeCommit("monthly")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finalAuthority := s.currentNudgeAuthority(now)
	if !finalAuthority.cadenceEligible || !policyPinsReady(finalAuthority.report.Inventory) {
		return nil, errBadRequest("monthly brief completion authority changed before persistence")
	}
	finalReport, err := s.buildReconReportContext(ctx)
	if err != nil {
		return nil, err
	}
	finalEvaluation, finalCompletion := s.governanceMonthlyPulseForWrite(finalAuthority, finalAuthority.policy, finalReport, now)
	finalAuthorityIdentity := monthlyAuthorityIdentity(finalAuthority, finalEvaluation.Month, finalReport, now)
	if receipt.AuthorityIdentity != finalAuthorityIdentity {
		return nil, errBadRequest("monthly brief completion conflicts with current authority")
	}
	if finalCompletion != nil {
		if finalEvaluation.Month != evaluation.Month || finalAuthority.policyIdentity != authority.policyIdentity {
			return nil, errBadRequest("monthly brief completion authority changed before persistence")
		}
		rec, ok := s.nudges.monthlyCompletionRecord(finalEvaluation.Month, finalAuthority.policyIdentity)
		if !ok || rec.BriefIdentity != fingerprint || !rec.CompletedAt.Equal(finalCompletion.CompletedAt) {
			return nil, errBadRequest("monthly brief completion conflicts with the pinned rendered brief")
		}
		s.consumeMonthlyRenderReceipt(fingerprint, now)
		return &rpc.BriefAckResult{
			OK: true, Kind: rpc.BriefKindMonthly, Day: day, At: rec.CompletedAt,
			AlreadyStamped: true, BriefFingerprint: rec.BriefIdentity, Month: rec.Month,
			Evidence: rec.Evidence, Message: "monthly foreground render already recorded",
		}, nil
	}
	if finalEvaluation.Status != risk.MonthlyPulseStatusDue || finalEvaluation.Month != evaluation.Month {
		return nil, errBadRequest("monthly brief completion authority changed before persistence")
	}
	if s.nudgeAfterValidation != nil {
		s.nudgeAfterValidation("monthly")
	}
	// The test seam represents the last possible authority race before the
	// filesystem write. Revalidate again so receipt acceptance is still bound
	// to the exact current policy/pin/report generation at commit time.
	commitAuthority := s.currentNudgeAuthority(now)
	if !commitAuthority.cadenceEligible || !policyPinsReady(commitAuthority.report.Inventory) {
		return nil, errBadRequest("monthly brief completion authority changed before persistence")
	}
	commitReport, err := s.buildReconReportContext(ctx)
	if err != nil {
		return nil, err
	}
	commitEvaluation, commitCompletion := s.governanceMonthlyPulseForWrite(commitAuthority, commitAuthority.policy, commitReport, now)
	commitAuthorityIdentity := monthlyAuthorityIdentity(commitAuthority, commitEvaluation.Month, commitReport, now)
	if commitCompletion != nil || commitEvaluation.Status != risk.MonthlyPulseStatusDue ||
		commitEvaluation.Month != finalEvaluation.Month || commitAuthorityIdentity != finalAuthorityIdentity ||
		receipt.AuthorityIdentity != commitAuthorityIdentity {
		return nil, errBadRequest("monthly brief completion authority changed before persistence")
	}
	rec, already, err := s.nudges.recordMonthlyCompletion(commitEvaluation.Month, commitAuthority.policyIdentity, fingerprint, commitAuthorityIdentity, now)
	if err != nil {
		return nil, fmt.Errorf("persist monthly brief completion: %w", err)
	}
	if s.nudgeAfterPersist != nil {
		s.nudgeAfterPersist("monthly")
	}
	s.consumeMonthlyRenderReceipt(fingerprint, now)
	return &rpc.BriefAckResult{
		OK: true, Kind: rpc.BriefKindMonthly, Day: day, At: rec.CompletedAt,
		AlreadyStamped: already, BriefFingerprint: rec.BriefIdentity, Month: rec.Month,
		Evidence: rec.Evidence, Message: "monthly paired-device foreground render recorded",
	}, nil
}

func (s *Server) issueMonthlyRenderReceipt(fingerprint, month, authorityIdentity string, now time.Time) {
	s.issueMonthlyRenderReceiptContext(context.Background(), fingerprint, month, authorityIdentity, now)
}

func (s *Server) issueMonthlyRenderReceiptContext(ctx context.Context, fingerprint, month, authorityIdentity string, now time.Time) bool {
	if s == nil || fingerprint == "" || month == "" || authorityIdentity == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	s.monthlyRenderMu.Lock()
	defer s.monthlyRenderMu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	wasNil := s.monthlyRenderReceipts == nil
	before := make(map[string]monthlyRenderReceipt, len(s.monthlyRenderReceipts))
	maps.Copy(before, s.monthlyRenderReceipts)
	if s.monthlyRenderBeforePersist != nil {
		s.monthlyRenderBeforePersist()
	}
	if ctx.Err() != nil {
		return false
	}
	if s.monthlyRenderReceipts == nil {
		s.monthlyRenderReceipts = make(map[string]monthlyRenderReceipt)
	}
	s.pruneMonthlyRenderReceiptsLocked(now)
	for len(s.monthlyRenderReceipts) >= monthlyRenderReceiptLimit {
		oldestKey := ""
		var oldest time.Time
		for key, receipt := range s.monthlyRenderReceipts {
			if oldestKey == "" || receipt.IssuedAt.Before(oldest) {
				oldestKey, oldest = key, receipt.IssuedAt
			}
		}
		delete(s.monthlyRenderReceipts, oldestKey)
	}
	s.monthlyRenderReceipts[fingerprint] = monthlyRenderReceipt{
		Month: month, AuthorityIdentity: authorityIdentity,
		IssuedAt: now.UTC(), ExpiresAt: now.UTC().Add(monthlyRenderReceiptTTL),
	}
	if ctx.Err() != nil {
		if wasNil {
			s.monthlyRenderReceipts = nil
		} else {
			s.monthlyRenderReceipts = before
		}
		return false
	}
	return true
}

func (s *Server) monthlyRenderReceipt(fingerprint, month, authorityIdentity string, now time.Time) (monthlyRenderReceipt, bool) {
	if s == nil {
		return monthlyRenderReceipt{}, false
	}
	s.monthlyRenderMu.Lock()
	defer s.monthlyRenderMu.Unlock()
	s.pruneMonthlyRenderReceiptsLocked(now)
	receipt, ok := s.monthlyRenderReceipts[fingerprint]
	return receipt, ok && receipt.Month == month && receipt.AuthorityIdentity == authorityIdentity
}

func (s *Server) consumeMonthlyRenderReceipt(fingerprint string, now time.Time) {
	s.monthlyRenderMu.Lock()
	defer s.monthlyRenderMu.Unlock()
	s.pruneMonthlyRenderReceiptsLocked(now)
	delete(s.monthlyRenderReceipts, fingerprint)
}

func (s *Server) pruneMonthlyRenderReceiptsLocked(now time.Time) {
	for fingerprint, receipt := range s.monthlyRenderReceipts {
		if !now.Before(receipt.ExpiresAt) {
			delete(s.monthlyRenderReceipts, fingerprint)
		}
	}
}

func (s *Server) composeBrief(ctx context.Context) (*rpc.BriefResult, *rpc.RulesResult) {
	acct, acctErr := s.buildAccountSummary(ctx, false)
	pos, posErr := s.handlePositionsList(ctx, &rpc.Request{})
	pos = s.analysisPositions(pos, s.briefNow())
	regime, regimeErr := s.briefRegimeSnapshotContext(ctx)
	breadth, breadthErr := s.buildBreadthSPX(&rpc.Request{}, false)
	gamma := s.briefGammaSnapshot()

	var marketEvents *rpc.MarketEventsResult
	var marketEventsErr error
	if pos != nil {
		symbols := marketEventSymbolsFromPositions(pos)
		marketEvents, marketEventsErr = s.handleMarketEventsSnapshot(ctx, &rpc.Request{Params: briefJSON(rpc.MarketEventsParams{Symbols: symbols})})
	} else {
		marketEventsErr = posErr
	}

	rules := s.evaluateRulesMode(ctx, false, false)
	// The brief boundary is captured after its input reads. In particular,
	// Stress source snapshots are stamped while those reads are in flight; a
	// boundary captured before them makes healthy evidence look future-dated
	// and causes the alert producer to fail closed with source_time_invalid.
	now := s.briefNow()
	res := &rpc.BriefResult{AsOf: now}
	cal, calErr := s.handleMarketCalendar(&rpc.Request{Params: briefJSON(rpc.MarketCalendarParams{Market: "us", At: now, Days: 1})})
	renderAuthority := s.currentNudgeAuthority(now)
	policy := s.briefPolicyResultForAuthority(acct, acctErr, renderAuthority, now)
	constitution := renderAuthority.policy
	recon := s.buildReconReport()

	// A closed official session downgrades expected coldness (paused event
	// sources, an idle gamma cache) from degraded to disclosed-normal; an
	// unavailable calendar conservatively counts as open so real gaps keep
	// their full weight.
	sessionOpen := calErr != nil || cal == nil || cal.Session.IsOpen

	market, can := composeBriefMarket(now, acct, pos, regime, breadth, gamma, marketEvents,
		acctErr, posErr, regimeErr, breadthErr, marketEventsErr, sessionOpen)
	// Brief-hook stress evidence: the same computed result the brief row
	// rendered, journaled with dedupe (internal-docs/design/history-index.md).
	s.journalStressDecision(&can)
	calendar := composeBriefCalendar(cal, marketEvents, rules, calErr, marketEventsErr, sessionOpen, briefBorrowFeeRelevant(pos, posErr))
	portfolio := s.composeBriefPortfolio(acct, pos, acctErr, posErr, sessionOpen)
	riskLimits := composeBriefRisk(policy, now)
	process := s.composeBriefProcessForAuthority(policy, constitution, recon, rules, renderAuthority, now)

	// The five domain sections above are composition intermediates: the two
	// rendered movements regroup their rows without changing any row's
	// severity/status semantics or the worst-child rollup behavior.
	res.Review = s.composeBriefReview(portfolio, riskLimits, process, now)
	res.Ready = composeBriefReady(market, calendar, riskLimits, portfolio, process, s.briefReadyProposals())
	res.StampTarget, res.StampTargetReason = s.briefStampTarget(policy, constitution, now)
	res.BriefFingerprint = briefContentFingerprint(res)
	// The narrative is a deterministic projection of the two movements above
	// (internal/daemon/brief_narrative.go): it states served facts and their
	// served statuses and adds none of its own. It is composed after the
	// fingerprint and stays outside it — the identity hashes Review and Ready
	// only, so revised prose can never invalidate a stamped brief.
	res.Narrative = composeBriefNarrative(res)
	// V4 monthly render evidence is bound to the current constitution even
	// when a policy-only revision happens not to alter a visible brief row.
	// V1-v3 retain their existing daily-stamp fingerprint byte behavior.
	if constitution != nil && constitution.PolicyVersion >= 4 {
		res.BriefFingerprint = opaqueIdentity("v4-brief", res.BriefFingerprint, renderAuthority.policyIdentity)
		if s.monthlyRenderBeforeIssue != nil {
			s.monthlyRenderBeforeIssue()
		}
		currentAuthority := s.currentNudgeAuthority(now)
		if nudgeAuthorityToken(renderAuthority) != nudgeAuthorityToken(currentAuthority) {
			return res, rules
		}
		monthly, _ := s.governanceMonthlyPulseForAuthority(renderAuthority, constitution, recon, now)
		if monthly.Status == risk.MonthlyPulseStatusBlocked {
			if recovery := s.governanceMonthlyPulseForRenderRecovery(renderAuthority, constitution, now); recovery.Status != "" {
				monthly = recovery
			}
		}
		if monthly.Status == risk.MonthlyPulseStatusDue {
			s.issueMonthlyRenderReceiptContext(ctx, res.BriefFingerprint, monthly.Month,
				monthlyAuthorityIdentity(renderAuthority, monthly.Month, recon, now), now)
		}
	}
	return res, rules
}

func (s *Server) briefNow() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func briefJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) briefGammaSnapshot() *rpc.GammaZeroSPXResult {
	if s == nil || s.zeroGamma == nil {
		return nil
	}
	env := s.zeroGamma.snapshotForScope(rpc.GammaZeroScopeCombined, nil, s.briefNow)
	return &env
}

func (s *Server) briefRegimeSnapshotContext(ctx context.Context) (*rpc.RegimeSnapshotResult, error) {
	if s == nil {
		return nil, fmt.Errorf("regime snapshot unavailable")
	}
	return s.currentDecisionReadyRegimeSnapshot(ctx)
}

func (s *Server) briefPolicyResult(acct *rpc.AccountResult, acctErr error, now time.Time) *rpc.RiskPolicyResult {
	return s.briefPolicyResultForAuthority(acct, acctErr, s.currentNudgeAuthority(now), now)
}

func (s *Server) briefPolicyResultForAuthority(acct *rpc.AccountResult, acctErr error, authority nudgeAuthorityState, now time.Time) *rpc.RiskPolicyResult {
	value := authority.report
	res := &value
	res.AsOf = now
	res.Unapproved = append([]string(nil), authority.report.Unapproved...)
	res.Inventory = append([]rpc.PolicyPinStatus(nil), authority.report.Inventory...)
	if authority.policy == nil {
		res.Unapproved = (&risk.Constitution{}).UnapprovedKeys()
	}
	var obs *risk.CapitalObservation
	if acctErr == nil && acct != nil && acct.NetLiquidation > 0 &&
		brokerScopeConcrete(authority.scope) && strings.EqualFold(strings.TrimSpace(acct.AccountID), strings.TrimSpace(authority.scope.Account)) &&
		(authority.policy == nil || authority.policy.Capital.BaseCurrency == "" || acct.BaseCurrency == "" || strings.EqualFold(authority.policy.Capital.BaseCurrency, acct.BaseCurrency)) {
		obs = &risk.CapitalObservation{EquityBase: acct.NetLiquidation, AsOf: acct.AsOf}
	}
	scope := authority.scope
	res.Capital = s.riskCapital.Report(authority.policy, obs, scope)
	res.Limits = risk.ConstitutionLimits(authority.policy)
	res.Overrides = s.riskCapital.OverridesSnapshotForScope(scope)
	res.Cadence = s.riskCapital.ArtefactsForScope(scope)
	return res
}

// composeBriefReview assembles the post-trade Review movement from the existing
// portfolio, risk, and process composition intermediates plus the read-only
// proposals-offered-vs-acted derivation. Row severities and the worst-child
// rollup are unchanged from the domain composers.
func (s *Server) composeBriefReview(portfolio rpc.BriefPortfolioSection, riskLimits rpc.BriefRiskSection, process rpc.BriefProcessSection, now time.Time) rpc.BriefReviewSection {
	out := rpc.BriefReviewSection{
		SessionPnL:    portfolio.Account,
		LastSession:   s.composeBriefLastSession(now),
		Attribution:   portfolio.Movers,
		RulesDelta:    process.RulesDelta,
		Proposals:     s.briefProposals(now),
		Overrides:     riskLimits.Overrides,
		CapitalEvents: briefCapitalEvents(riskLimits.Capital, riskLimits.Latch),
		Reconcile:     process.Reconcile,
		AutoExtend:    process.AutoExtend,
		OneTap:        process.OneTap,
		WorkingOrders: portfolio.WorkingOrders,
	}
	out.BriefRowState = briefSectionState("review",
		out.SessionPnL.BriefRowState, out.LastSession.BriefRowState, out.Attribution.BriefRowState, out.RulesDelta.BriefRowState,
		out.Proposals.BriefRowState, out.Overrides.BriefRowState, out.CapitalEvents.BriefRowState,
		out.Reconcile.BriefRowState, out.AutoExtend.BriefRowState, out.OneTap.BriefRowState,
		out.WorkingOrders.BriefRowState)
	return out
}

// composeBriefLastSession serves the daemon's close capture as the last
// completed session's Daily P&L. It never substitutes: a retained capture for
// any other session date, a non-concrete broker scope, or an unresolvable
// calendar all read as not captured, because everything the broker serves off
// the close is a running value at off-session marks.
func (s *Server) composeBriefLastSession(now time.Time) rpc.BriefLastSessionRow {
	row := rpc.BriefLastSessionRow{}
	date, ok := lastCompletedUSEquitySessionDate(now)
	if !ok {
		row.BriefRowState = briefUnavailable("the served calendar cannot resolve the last completed session")
		return row
	}
	row.SessionDate = date
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		row.BriefRowState = briefUnavailable("close captures bind to one concrete account and mode; the current broker scope names neither")
		return row
	}
	capture, ok := s.dailyPnLCloseCaptures.captureFor(dailyPnLScopeSource(scope))
	if !ok || capture.SessionKey != date {
		row.BriefRowState = briefUnavailable(fmt.Sprintf("not captured for %s — the daemon records this figure only while running and connected at the official close", date))
		return row
	}
	row.BriefRowState = briefOK("account Daily P&L pinned at the official close; unlike the live row it does not move on off-session marks")
	row.DailyPnLBase = new(capture.DailyPnL)
	row.BaseCurrency = capture.BaseCurrency
	row.SessionClose = capture.SessionClose
	row.CapturedAt = capture.CapturedAt
	return row
}

// composeBriefReady assembles the pre-trade Ready movement from the existing
// market, calendar, risk, portfolio, and process intermediates plus the
// read-only actionable-proposal projection.
func composeBriefReady(market rpc.BriefMarketSection, calendar rpc.BriefCalendarSection,
	riskLimits rpc.BriefRiskSection, portfolio rpc.BriefPortfolioSection, process rpc.BriefProcessSection,
	proposals rpc.BriefReadyProposalsRow) rpc.BriefReadySection {
	out := rpc.BriefReadySection{
		Regime:        market.Regime,
		Breadth:       market.Breadth,
		Gamma:         market.Gamma,
		Stress:        market.Stress,
		Session:       calendar.Session,
		MarketEvents:  calendar.MarketEvents,
		Capital:       riskLimits.Capital,
		Latch:         riskLimits.Latch,
		PremiumAtRisk: portfolio.PremiumAtRisk,
		HedgeCost:     portfolio.HedgeCost,
		Proposals:     proposals,
		PolicyDrift:   riskLimits.PolicyDrift,
		Artefacts:     process.Artefacts,
		MonthlyPulse:  process.MonthlyPulse,
	}
	out.BriefRowState = briefReadySectionState(out)
	return out
}

func briefReadySectionState(ready rpc.BriefReadySection) rpc.BriefRowState {
	rows := []rpc.BriefRowState{
		ready.Regime.BriefRowState, ready.Breadth.BriefRowState, ready.Gamma.BriefRowState,
		ready.Stress.BriefRowState, ready.Session.BriefRowState,
	}
	for _, ev := range ready.MarketEvents {
		rows = append(rows, ev.BriefRowState)
	}
	rows = append(rows,
		ready.Capital.BriefRowState, ready.Latch.BriefRowState,
		ready.PremiumAtRisk.BriefRowState, ready.HedgeCost.BriefRowState,
		ready.Proposals.BriefRowState, ready.PolicyDrift.BriefRowState, ready.Artefacts.BriefRowState)
	if ready.MonthlyPulse != nil {
		rows = append(rows, briefMonthlyPulseRollupState(ready.MonthlyPulse.Status))
	}
	return briefSectionState("ready", rows...)
}

// briefCapitalEvents frames the current latch and adjusted-peak provenance as
// the Review movement's capital-events row. It regroups existing facts only —
// no new journal read — and inherits the latch/capital condition so an engaged
// latch or an absent constitution never reads as a clean "no events" line.
func briefCapitalEvents(capital rpc.BriefCapitalRow, latch rpc.BriefLatchRow) rpc.BriefCapitalEventsRow {
	row := rpc.BriefCapitalEventsRow{
		BriefRowState:      briefOK("no capital events this session; adjusted-peak provenance shown"),
		Latched:            latch.Latched,
		LatchedAt:          latch.At,
		LatchAgeDays:       latch.AgeDays,
		ConsumedPctAtLatch: latch.ConsumedPctAtLatch,
		AdjustedPeakBase:   capital.AdjustedPeakBase,
		PeakAsOf:           capital.PeakAsOf,
		BaseCurrency:       capital.BaseCurrency,
	}
	switch {
	case capital.Status == rpc.BriefStatusUnavailable:
		row.BriefRowState = briefUnavailable("risk constitution absent; capital events cannot be evaluated")
	case latch.Latched:
		row.BriefRowState = briefAttention("drawdown latch engaged this episode and remains open until a human reset")
	}
	return row
}

// briefProposals derives protection-proposal offered-vs-acted counts read-only
// from the trade-proposal-outcomes journal. It is the one new derivation this
// restructure adds; only counts and the covered day reach the wire.
func (s *Server) briefProposals(_ time.Time) rpc.BriefProposalsRow {
	if s == nil || s.proposalOutcomes == nil {
		return rpc.BriefProposalsRow{BriefRowState: briefUnavailable("proposal outcome journal is unavailable")}
	}
	offered, acted, day, ok, err := s.proposalOutcomes.SessionSummary()
	if err != nil {
		return rpc.BriefProposalsRow{BriefRowState: briefUnavailable("proposal outcome journal could not be read")}
	}
	if !ok {
		return rpc.BriefProposalsRow{BriefRowState: briefOK("no protection proposals recorded yet")}
	}
	return rpc.BriefProposalsRow{
		BriefRowState: briefOK(fmt.Sprintf("%d offered, %d acted in the last recorded session (%s)", offered, acted, day)),
		Day:           day, Offered: offered, Acted: acted,
	}
}

// briefReadyProposals projects the CURRENT protection-proposal snapshot into
// the Ready movement: how much work is staged for the session ahead. It is a
// pure read of the engine's last-installed snapshot (show=false, so it emits
// no shown events and advances no clock) and it carries counts only. Staged
// work is a decision the desk owes, so a non-zero actionable count is an
// attention row — never an authorization: submission keeps every gate it has.
func (s *Server) briefReadyProposals() rpc.BriefReadyProposalsRow {
	if s == nil || s.tradeProposals == nil {
		return rpc.BriefReadyProposalsRow{BriefRowState: briefUnavailable("protection proposal snapshot is unavailable")}
	}
	snap := s.tradeProposals.Snapshot(false)
	total, actionable := snap.Counts.Total, snap.Counts.Actionable
	blocked := max(total-actionable, 0)
	row := rpc.BriefReadyProposalsRow{Actionable: actionable, Blocked: blocked, Total: total}
	switch {
	case actionable > 0:
		row.BriefRowState = briefAttention(fmt.Sprintf("%d protection proposal(s) ready to act, %d blocked", actionable, blocked))
	case blocked > 0:
		row.BriefRowState = briefOK(fmt.Sprintf("no protection proposal is ready to act; %d blocked", blocked))
	default:
		row.BriefRowState = briefOK("no protection proposals are staged")
	}
	return row
}

// composeBriefMarket stays pure: it also returns the computed stress
// result so composeBrief (the method) can journal it as stress evidence
// without this function taking on daemon state.
func composeBriefMarket(now time.Time, acct *rpc.AccountResult, pos *rpc.PositionsResult,
	regime *rpc.RegimeSnapshotResult, breadth *rpc.BreadthSPXResult, gamma *rpc.GammaZeroSPXResult,
	events *rpc.MarketEventsResult, acctErr, posErr, regimeErr, breadthErr, eventsErr error, sessionOpen bool) (rpc.BriefMarketSection, rpc.StressResult) {
	out := rpc.BriefMarketSection{}
	if regimeErr != nil || regime == nil {
		out.Regime.BriefRowState = briefUnavailable("regime snapshot unavailable: " + errText(regimeErr))
	} else {
		out.Regime.BriefRowState = briefOK("daemon regime lifecycle and composite verdict")
		out.Regime.Stage = regime.Posture.Stage
		if out.Regime.Stage == "" {
			out.Regime.Stage = regime.Lifecycle.Stage
		}
		out.Regime.Verdict = regime.Composite.Verdict
		if len(regime.WarningDetails) > 0 || out.Regime.Stage == "" || out.Regime.Verdict == "" {
			out.Regime.BriefRowState = briefDegraded("regime returned partial or unclassified evidence")
		}
		if health := regime.AuthorityHealth; health != nil {
			switch health.Status {
			case rpc.RegimeAuthorityUnavailable:
				out.Regime.BriefRowState = briefUnavailable("daemon Regime last-good authority is unavailable")
			case rpc.RegimeAuthorityStale:
				out.Regime.BriefRowState = briefDegraded("daemon Regime verdict is retained stale last-good context")
			case rpc.RegimeAuthorityFresh:
				if health.FailureCode != rpc.RegimeAuthorityFailureNone {
					out.Regime.BriefRowState = briefDegraded("daemon Regime last-good is fresh but its latest authority operation failed")
				}
			default:
				out.Regime.BriefRowState = briefUnavailable("daemon Regime authority health is invalid")
			}
		}
	}
	if breadthErr != nil || breadth == nil {
		out.Breadth.BriefRowState = briefUnavailable("breadth snapshot unavailable: " + errText(breadthErr))
	} else if breadth.State != rpc.BreadthStateReady {
		out.Breadth.BriefRowState = briefDegraded("breadth source is " + string(breadth.State))
	} else {
		// Name the session on every row. A reading is one trading day's close
		// and the engine keeps serving the last converged one rather than
		// publishing below its coverage threshold, so an undated row shows a
		// stalled lane's numbers as if they were today's.
		detail := "S&P 500 constituent breadth · " + breadth.SessionKey + " session"
		switch {
		case !breadth.Stale:
			out.Breadth.BriefRowState = briefOK(detail)
		case spx.PublicationPending(breadth.SessionKey, breadth.Refreshing, now):
			// The ordinary post-close window: the newer session's fan-out is
			// running and still inside its bounded deadline. Degrading here
			// would light the row for ~90 min after every close.
			out.Breadth.BriefRowState = briefOK(detail + "; the newer session is still computing")
		default:
			out.Breadth.BriefRowState = briefDegraded(detail + "; a newer session is overdue")
		}
		out.Breadth.PctAbove50DMA = new(breadth.PctAbove50DMA)
		out.Breadth.PctAbove200DMA = new(breadth.PctAbove200DMA)
		out.Breadth.NetNewHighsPct = new(breadth.NetNewHighsPct)
	}
	if breadth != nil {
		out.Breadth.AsOf, out.Breadth.DataType = breadth.AsOf, breadth.DataType
	}
	out.Gamma = composeBriefGamma(gamma, sessionOpen, now)
	stressInput := rpc.StressInput{Now: now}
	if acct != nil {
		stressInput.Account = *acct
	}
	if pos != nil {
		stressInput.Positions = *pos
	}
	if regime != nil {
		stressInput.Regime = *regime
	}
	if events != nil {
		stressInput.MarketEvents = *events
	}
	can := stress.ComputeStress(stressInput)
	out.Stress = rpc.BriefStressRow{
		BriefRowState: briefOK("pure stress composition over daemon snapshots"),
		Action:        can.Action, Severity: string(can.Severity), Summary: can.Summary,
	}
	if acctErr != nil || posErr != nil || regimeErr != nil || eventsErr != nil || can.InputHealth != "ok" {
		out.Stress.BriefRowState = briefDegraded("stress inputs are partial; unavailable sources remain explicit")
	}
	out.BriefRowState = briefSectionState("market", out.Regime.BriefRowState, out.Breadth.BriefRowState, out.Gamma.BriefRowState, out.Stress.BriefRowState)
	return out, can
}

func composeBriefGamma(env *rpc.GammaZeroSPXResult, sessionOpen bool, now time.Time) rpc.BriefGammaRow {
	row := rpc.BriefGammaRow{BriefRowState: briefUnavailable("dealer gamma cache is unavailable")}
	if env == nil {
		return row
	}
	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		cadence := gammaOperationalCadence(env, now)
		row.BriefRowState = briefDegraded("dealer gamma source is " + env.Status + " (" + cadence + ")")
		return row
	}
	computed := env.Result
	if spx := computed.PerIndex["SPX"]; spx != nil {
		computed = spx
	}
	row.BriefRowState = briefOK("SPX dealer zero-gamma versus spot")
	if computed.SpotUnderlying > 0 {
		row.Spot = new(computed.SpotUnderlying)
	}
	row.ZeroGamma, row.GapPct, row.GammaSign, row.AsOf = computed.ZeroGamma, computed.GapPct, computed.GammaSign, computed.AsOf
	cadence := gammaOperationalCadence(env, now)
	if !sessionOpen && cadence == rpc.DataCadenceNotDue {
		row.BriefRowState = briefOK("dealer gamma is last-completed-session context; no newer regular-session compute is due")
	} else if cadence == rpc.DataCadenceMissedSession || cadence == rpc.DataCadenceUnknown {
		row.BriefRowState = briefDegraded("dealer gamma process health is " + cadence)
	}
	if row.Spot == nil || (row.ZeroGamma == nil && row.GammaSign == "") {
		row.BriefRowState = briefDegraded("gamma result lacks a complete spot/zero-crossing classification")
	}
	if computed.Quality != nil && computed.Quality.Rankability != rpc.GammaRankabilityRankable &&
		!(cadence == rpc.DataCadenceNotDue && !sessionOpen && gammaRankabilityCadenceOnly(computed.Quality)) {
		row.BriefRowState = briefDegraded("gamma is context-only: " + computed.Quality.RankabilityReason)
	}
	return row
}

func gammaRankabilityCadenceOnly(quality *rpc.GammaSignalQuality) bool {
	if quality == nil {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(quality.RankabilityReason))
	return strings.Contains(reason, "market is closed") || strings.Contains(reason, "prior session")
}

func composeBriefCalendar(cal *rpc.MarketCalendarResult, events *rpc.MarketEventsResult, rules *rpc.RulesResult, calErr, eventsErr error, sessionOpen bool, borrowRelevant ...*bool) rpc.BriefCalendarSection {
	out := rpc.BriefCalendarSection{}
	if calErr != nil || cal == nil {
		out.Session.BriefRowState = briefUnavailable("market calendar unavailable: " + errText(calErr))
	} else {
		s := cal.Session
		out.Session = rpc.BriefSessionRow{BriefRowState: briefOK(nonEmptyString(s.Reason, "official session calendar")),
			Market: s.Market, State: s.State, IsOpen: s.IsOpen, Open: s.Open, Close: s.Close}
		if s.NextOpen != nil {
			out.Session.NextOpen = *s.NextOpen
		}
	}
	out.MarketEvents = briefMarketEventRows(events, rules, eventsErr, sessionOpen, borrowRelevant...)
	states := []rpc.BriefRowState{out.Session.BriefRowState}
	for _, row := range out.MarketEvents {
		states = append(states, row.BriefRowState)
	}
	out.BriefRowState = briefSectionState("calendar", states...)
	return out
}

func briefMarketEventRows(events *rpc.MarketEventsResult, rules *rpc.RulesResult, sourceErr error, sessionOpen bool, borrowRelevant ...*bool) []rpc.BriefMarketEventRow {
	kinds := []string{"earnings", "halt", "ssr", "borrow"}
	sets := map[string]map[string]struct{}{}
	for _, kind := range kinds {
		sets[kind] = map[string]struct{}{}
	}
	if rules != nil {
		for _, e := range rules.Earnings {
			// A terminal non-reporting issuer has no earnings to await; the
			// applicability summary below already discloses it as excluded.
			if e.Status == rpc.EarningsStatusTerminalNonReporting {
				continue
			}
			if strings.TrimSpace(e.Symbol) != "" {
				sets["earnings"][strings.ToUpper(e.Symbol)] = struct{}{}
			}
		}
	}
	if events != nil {
		for _, flag := range events.Flags {
			id := strings.ToLower(flag.ID + " " + flag.Label)
			kind := ""
			switch {
			case strings.Contains(id, "halt") || strings.Contains(id, "luld"):
				kind = "halt"
			case strings.Contains(id, "reg_sho") || strings.Contains(id, "ssr"):
				kind = "ssr"
			case strings.Contains(id, "borrow"):
				kind = "borrow"
			}
			if kind != "" && flag.Status != rpc.MarketEventStatusInactive {
				sets[kind][strings.ToUpper(flag.Symbol)] = struct{}{}
			}
		}
	}
	rows := make([]rpc.BriefMarketEventRow, 0, len(kinds))
	hardErr := sourceErr != nil || events == nil
	for _, kind := range kinds {
		syms := mapKeysSorted(sets[kind])
		flagged := fmt.Sprintf("%d held %s flagged", len(syms), pluralNoun(len(syms), "symbol"))
		if kind == "earnings" {
			flagged = fmt.Sprintf("%d held %s with earnings context", len(syms), pluralNoun(len(syms), "symbol"))
		}
		state := briefOK(flagged)
		if kind == "borrow" && len(borrowRelevant) > 0 && borrowRelevant[0] != nil && !*borrowRelevant[0] {
			state = briefOK(flagged + "; borrow-fee coverage is not required because there is no short-stock exposure")
			rows = append(rows, rpc.BriefMarketEventRow{BriefRowState: state, Kind: kind, Count: len(syms), Symbols: syms})
			continue
		}
		worst, refreshState, lastChecked := briefEventKindHealth(events, kind)
		switch {
		case hardErr || (kind == "earnings" && rules == nil):
			state = briefDegraded(fmt.Sprintf("%d known; one or more event sources are degraded", len(syms)))
		case worst == "" || worst == "ok":
			// healthy source: flagged copy stands as-is
		case !sessionOpen && (worst == rpc.SourceStatusStale || worst == rpc.SourceStatusUnknown) &&
			(kind != "borrow" || (refreshState == rpc.SourceRefreshNotDue && worst == rpc.SourceStatusUnknown)):
			// Only stale/unknown are quiet-eligible while closed: no fresh
			// update is expected, and the copy claims only what the code
			// verified — counts come from the last good data, not a fresh
			// check, so a zero is never asserted as current fact.
			inLast := fmt.Sprintf("%d held %s flagged in the last good data", len(syms), pluralNoun(len(syms), "symbol"))
			if len(syms) == 0 {
				inLast = "no flags in the last good data"
			}
			state = briefOK(inLast + "; no fresh update expected while the market is closed (source health " + worst + briefLastChecked(lastChecked) + ")")
		default:
			// Everything else — degraded, partial, any status outside the
			// known vocabulary, or any non-ok state during an open session —
			// keeps its weight: a source that misbehaved is not idle.
			state = briefDegraded(flagged + "; source health is " + worst + briefLastChecked(lastChecked))
		}
		if kind == "earnings" && len(syms) > 0 {
			unresolved := briefUnresolvedEarnings(rules)
			if len(unresolved) > 0 {
				state = briefDegraded(fmt.Sprintf("%d held earnings context unresolved (%s)", len(unresolved), strings.Join(unresolved, ", ")))
			}
			if unknown := briefUnknownEarningsRules(rules); len(unknown) > 0 {
				verb := "report"
				if len(unknown) == 1 {
					verb = "reports"
				}
				if len(unresolved) > 0 {
					state = briefAttention(fmt.Sprintf("%d held earnings context unresolved (%s) while the %s %s %s unknown; the rulebook cannot confirm the held-name earnings controls",
						len(unresolved), strings.Join(unresolved, ", "), strings.Join(unknown, " and "), pluralNoun(len(unknown), "rule"), verb))
				} else {
					state = briefAttention(fmt.Sprintf("%d held earnings upcoming while the %s %s %s unknown; the rulebook cannot confirm the held-name earnings controls",
						len(syms), strings.Join(unknown, " and "), pluralNoun(len(unknown), "rule"), verb))
				}
			}
			if applicability := briefEarningsApplicabilitySummary(rules); applicability != "" {
				state.Detail += "; " + applicability
			}
			if evidenceInfo := briefEarningsEvidenceInfo(rules); evidenceInfo != "" {
				state.Detail += "; " + evidenceInfo
			}
			if notice := briefWSHEntitlementNotice(rules); notice != "" {
				state.Detail += "; " + notice
			}
		}
		rows = append(rows, rpc.BriefMarketEventRow{BriefRowState: state, Kind: kind, Count: len(syms), Symbols: syms})
	}
	return rows
}

func briefEarningsApplicabilitySummary(rules *rpc.RulesResult) string {
	if rules == nil {
		return ""
	}
	broker, terminal := 0, 0
	for _, info := range rules.Earnings {
		switch {
		case info.Source == "broker_identity" && info.Status == rpc.EarningsStatusNotApplicable:
			broker++
		case info.Source == "verified_terminal" && info.Status == rpc.EarningsStatusTerminalNonReporting:
			terminal++
		}
	}
	parts := make([]string, 0, 2)
	if broker > 0 {
		parts = append(parts, fmt.Sprintf("%d broker-proven nonissuer", broker))
	}
	if terminal > 0 {
		parts = append(parts, fmt.Sprintf("%d terminal/non-reporting", terminal))
	}
	if len(parts) == 0 {
		return ""
	}
	return "issuer earnings not applicable: " + strings.Join(parts, ", ")
}

func briefEarningsEvidenceInfo(rules *rpc.RulesResult) string {
	if rules == nil {
		return ""
	}
	for _, health := range rules.InputHealth {
		if health.Source == "earnings" && health.Status == rpc.SourceStatusOK && len(health.Notes) > 0 {
			return "earnings evidence informational issue: " + strings.Join(health.Notes, "; ")
		}
	}
	return ""
}

func briefWSHEntitlementNotice(rules *rpc.RulesResult) string {
	if rules == nil {
		return ""
	}
	for _, info := range rules.Earnings {
		for _, provider := range info.Providers {
			failure := provider.LastFailure
			if provider.Provider != "ibkr_wsh" || failure == nil ||
				failure.Code != rpc.SourceFailureNotEntitled || failure.Retryable ||
				(failure.Stage != rpc.SourceFailureStageWSHMetadata && failure.Stage != rpc.SourceFailureStageWSHEvent) {
				continue
			}
			return "optional Wall Street Horizon earnings feed unavailable because this account lacks the WSH research subscription; Nasdaq remains active; names without a usable date stay unknown, never pass"
		}
	}
	return ""
}

// briefBorrowFeeRelevant returns nil when positions are unavailable, false for
// a known all-long book, and true only for actual short-stock exposure. Option
// positions do not make the stock-borrow fee source decision-relevant.
func briefBorrowFeeRelevant(pos *rpc.PositionsResult, posErr error) *bool {
	if posErr != nil || pos == nil {
		return nil
	}
	relevant := false
	for _, stock := range pos.Stocks {
		if briefPositionIsStock(stock) && stock.Quantity < 0 {
			relevant = true
			break
		}
	}
	if !relevant {
		for _, group := range pos.ByUnderlying {
			if group.Stock != nil && briefPositionIsStock(*group.Stock) && group.Stock.Quantity < 0 {
				relevant = true
				break
			}
		}
	}
	return &relevant
}

func briefPositionIsStock(position rpc.PositionView) bool {
	secType := strings.ToUpper(strings.TrimSpace(position.SecType))
	// Empty is a legacy stock projection. Explicit non-stock security types
	// cannot make the stock-borrow fee source decision-relevant.
	return secType == "" || secType == rpc.SecTypeStock || secType == "STK" || secType == "ETF"
}

// briefEventKindHealth maps one brief event kind to its own source-health rows
// so an unrelated source (for example an unreachable borrow-fee feed) cannot
// degrade every event row. It returns the worst matching status and the newest
// matching observation time; empty means no matching source reported.
func briefEventKindHealth(events *rpc.MarketEventsResult, kind string) (string, string, time.Time) {
	if events == nil {
		return "", "", time.Time{}
	}
	match := func(source string) bool {
		source = strings.ToLower(source)
		switch kind {
		case "halt":
			return strings.Contains(source, "halt") || strings.Contains(source, "luld")
		case "ssr":
			return strings.Contains(source, "reg_sho") || strings.Contains(source, "ssr")
		case "borrow":
			return strings.Contains(source, "borrow")
		default:
			return false
		}
	}
	rank := map[string]int{rpc.SourceStatusOK: 0, rpc.SourceStatusStale: 1, rpc.SourceStatusUnknown: 2, rpc.SourceStatusPartial: 3, rpc.SourceStatusDegraded: 4}
	worst, worstRefresh, worstRank := "", "", -1
	var lastChecked time.Time
	for _, row := range events.SourceHealth {
		if !match(row.Source) {
			continue
		}
		if r, known := rank[row.Status]; known && r > worstRank {
			worst, worstRefresh, worstRank = row.Status, row.RefreshState, r
		} else if !known && row.Status != "" && worstRank < len(rank) {
			worst, worstRefresh, worstRank = row.Status, row.RefreshState, len(rank)
		}
		if row.AsOf.After(lastChecked) {
			lastChecked = row.AsOf
		}
	}
	return worst, worstRefresh, lastChecked
}

func briefLastChecked(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return "; last checked " + at.In(time.Local).Format("2006-01-02 15:04")
}

func pluralNoun(count int, noun string) string {
	if count == 1 {
		return noun
	}
	return noun + "s"
}

// briefUnknownEarningsRules cross-links the earnings event row to the rules
// that govern earnings behavior. This is disclosure only — it names which
// governing rules cannot currently be evaluated; it gates nothing.
func briefUnknownEarningsRules(rules *rpc.RulesResult) []string {
	if rules == nil {
		return nil
	}
	governing := map[string]bool{"catalyst_coverage": true, "earnings_size_freeze": true, "overwrite_earnings": true}
	var unknown []string
	for _, row := range rules.Rules {
		if governing[row.ID] && row.Status == risk.RuleStatusUnknown {
			unknown = append(unknown, strings.ReplaceAll(row.ID, "_", " "))
		}
	}
	slices.Sort(unknown)
	return unknown
}

func briefUnresolvedEarnings(rules *rpc.RulesResult) []string {
	if rules == nil {
		return nil
	}
	var unresolved []string
	for _, earnings := range rules.Earnings {
		if earnings.Status == rpc.EarningsStatusNotApplicable || earnings.Status == rpc.EarningsStatusTerminalNonReporting {
			continue
		}
		if earnings.Date != "" && earnings.Source != "unknown" && (earnings.Status == "" || earnings.Status == rpc.EarningsStatusDate) {
			continue
		}
		reason := nonEmptyString(earnings.Reason, nonEmptyString(earnings.Status, "not_observed"))
		unresolved = append(unresolved, strings.ToUpper(earnings.Symbol)+" ("+strings.ReplaceAll(reason, "_", " ")+")")
	}
	slices.Sort(unresolved)
	return unresolved
}

func (s *Server) composeBriefPortfolio(acct *rpc.AccountResult, pos *rpc.PositionsResult, acctErr, posErr error, sessionOpen bool) rpc.BriefPortfolioSection {
	out := rpc.BriefPortfolioSection{}
	if acctErr != nil || acct == nil {
		out.Account.BriefRowState = briefUnavailable("account summary unavailable: " + errText(acctErr))
	} else {
		detail := "account summary in base currency"
		if !sessionOpen {
			detail = "account summary in base currency; market closed — daily P&L is the broker's running since-close value at off-session marks, not a completed-session result"
		}
		out.Account = rpc.BriefAccountRow{BriefRowState: briefOK(detail),
			DailyPnLBase: acct.DailyPnL, BaseCurrency: acct.BaseCurrency, AsOf: acct.AsOf}
		if acct.NetLiquidation > 0 {
			out.Account.EquityBase = new(acct.NetLiquidation)
		}
		if out.Account.EquityBase == nil || acct.DailyPnL == nil {
			out.Account.BriefRowState = briefDegraded("daily P&L is unavailable; equity remains present")
			if out.Account.EquityBase == nil {
				out.Account.Detail = "account equity is unavailable; zero was not substituted"
			}
		}
	}
	if posErr != nil || pos == nil {
		out.Movers.BriefRowState = briefUnavailable("positions unavailable: " + errText(posErr))
		out.PremiumAtRisk.BriefRowState = briefUnavailable("positions unavailable")
		out.HedgeCost.BriefRowState = briefUnavailable("positions unavailable")
	} else {
		out.Movers = briefMovers(pos, sessionOpen)
		out.PremiumAtRisk = briefPremiumAtRisk(pos, out.Account.BaseCurrency)
		out.HedgeCost = briefHedgeCost(pos, out.Account.BaseCurrency)
		// The premium-at-risk headline includes every long option leg. When a
		// hedge-candidate leg cannot be classified, the protective share of
		// that premium is unknown, so the row's confidence must say so even
		// though the amount itself is complete.
		if out.HedgeCost.ExcludedLegs > 0 && out.PremiumAtRisk.Status == rpc.BriefStatusOK {
			out.PremiumAtRisk.BriefRowState = briefDegraded(fmt.Sprintf(
				"long-option market value in base currency; %d hedge-candidate %s cannot be classified, so the protective share of this premium is unknown",
				out.HedgeCost.ExcludedLegs, pluralNoun(out.HedgeCost.ExcludedLegs, "leg")))
		}
	}
	out.WorkingOrders = s.briefWorkingOrders()
	out.BriefRowState = briefSectionState("portfolio", out.Account.BriefRowState, out.Movers.BriefRowState,
		out.PremiumAtRisk.BriefRowState, out.HedgeCost.BriefRowState, out.WorkingOrders.BriefRowState)
	return out
}

// briefMovers aggregates daily P&L by underlying — the same basis as the
// Underlyings panel — so the two surfaces reconcile. The residual beyond the
// top rows is disclosed so the row's implied total matches the account-level
// daily attribution instead of silently truncating.
func briefMovers(pos *rpc.PositionsResult, sessionOpen bool) rpc.BriefMoversRow {
	detail := "daily P&L by underlying, largest absolute first; position-level sums can differ from the account row by fees and FX"
	if !sessionOpen {
		detail += " (market closed — broker running values at off-session marks)"
	}
	row := rpc.BriefMoversRow{BriefRowState: briefOK(detail)}
	for _, group := range pos.ByUnderlying {
		if group.GroupDailyPnLBase != nil {
			row.Rows = append(row.Rows, rpc.BriefMover{Symbol: strings.ToUpper(group.Underlying), DailyPnLBase: *group.GroupDailyPnLBase})
		}
	}
	sort.SliceStable(row.Rows, func(i, j int) bool {
		return math.Abs(row.Rows[i].DailyPnLBase) > math.Abs(row.Rows[j].DailyPnLBase)
	})
	if len(row.Rows) > briefMoverLimit {
		var rest float64
		for _, mover := range row.Rows[briefMoverLimit:] {
			rest += mover.DailyPnLBase
		}
		row.OtherPnLBase = &rest
		row.OtherCount = len(row.Rows) - briefMoverLimit
		row.Rows = row.Rows[:briefMoverLimit]
	}
	if len(row.Rows) == 0 {
		row.BriefRowState = briefDegraded("no per-underlying daily P&L values are available")
	}
	return row
}

func briefPremiumAtRisk(pos *rpc.PositionsResult, base string) rpc.BriefMoneyCoverageRow {
	row := rpc.BriefMoneyCoverageRow{BriefRowState: briefOK("long-option market value in base currency"), BaseCurrency: base}
	var sum float64
	for _, p := range pos.Options {
		if p.Quantity <= 0 {
			continue
		}
		if p.MarketValueBase == nil {
			row.ExcludedLegs++
			continue
		}
		sum += *p.MarketValueBase
		row.IncludedLegs++
	}
	if row.IncludedLegs > 0 {
		row.AmountBase = new(sum)
	}
	if row.ExcludedLegs > 0 {
		row.BriefRowState = briefDegraded(fmt.Sprintf("%d long option %s excluded because base market value is unavailable", row.ExcludedLegs, pluralNoun(row.ExcludedLegs, "leg")))
	} else if row.IncludedLegs == 0 {
		row.BriefRowState = briefOK("no long option positions")
		zero := 0.0
		row.AmountBase = &zero
	}
	return row
}

func briefHedgeCost(pos *rpc.PositionsResult, base string) rpc.BriefMoneyCoverageRow {
	row := rpc.BriefMoneyCoverageRow{BriefRowState: briefOK("daily theta of rulebook-classified hedge legs"), BaseCurrency: base}
	pol := risk.DefaultRulebookPolicy()
	var sum float64
	for _, p := range pos.Options {
		candidate := p.Quantity > 0 && strings.EqualFold(p.Right, "P") && pol.IsHedgeSymbol(p.Symbol)
		if !candidate {
			continue
		}
		leg := risk.LegInput{Right: p.Right, Quantity: p.Quantity, Delta: p.Delta, Underlying: p.Underlying, HedgeListed: true}
		if !risk.RulebookHedgeLeg(leg) || p.Theta == nil {
			row.ExcludedLegs++
			continue
		}
		value := *p.Theta * p.Quantity * float64(max(p.Multiplier, 1))
		if rate, ok := positionBaseRate(p, base); ok {
			value *= rate
		} else {
			row.ExcludedLegs++
			continue
		}
		sum += value
		row.IncludedLegs++
	}
	if row.IncludedLegs > 0 {
		row.AmountBase = new(sum)
	}
	if row.ExcludedLegs > 0 {
		row.BriefRowState = briefDegraded(fmt.Sprintf("%d candidate hedge %s excluded because classification Greeks/theta/FX are unavailable", row.ExcludedLegs, pluralNoun(row.ExcludedLegs, "leg")))
	} else if row.IncludedLegs == 0 {
		row.BriefRowState = briefOK("no rulebook-classified hedge legs")
		zero := 0.0
		row.AmountBase = &zero
	}
	return row
}

func (s *Server) briefWorkingOrders() rpc.BriefCountRow {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return rpc.BriefCountRow{BriefRowState: briefUnavailable("open-orders journal unavailable: " + err.Error())}
	}
	scope := s.currentBrokerStateScope()
	count := 0
	for _, view := range views {
		if view.Open && orderViewMatchesBrokerScope(view, scope) {
			count++
		}
	}
	return rpc.BriefCountRow{BriefRowState: briefOK("daemon open-orders journal view"), Count: &count}
}

func composeBriefRisk(policy *rpc.RiskPolicyResult, now time.Time) rpc.BriefRiskSection {
	out := rpc.BriefRiskSection{}
	if policy == nil || policy.Status == rpc.RiskPolicyStatusAbsent {
		state := briefUnavailable("risk constitution absent; capital controls are unapproved")
		out.Capital.BriefRowState, out.Latch.BriefRowState, out.Overrides.BriefRowState, out.PolicyDrift.BriefRowState = state, state, state, state
		out.BriefRowState = state
		return out
	}
	c := policy.Capital
	out.Capital = rpc.BriefCapitalRow{BriefRowState: briefOK("constitution capital state"), Tier: c.Tier,
		Enforcement: c.Enforcement, ConsumedPct: c.ConsumedPct, DrawdownBase: c.DrawdownBase,
		AdjustedPeakBase: c.AdjustedPeakBase, PeakAsOf: c.PeakAsOf, BaseCurrency: c.BaseCurrency}
	// The capital status derives from the values it shows: a breached tier or
	// a fully consumed budget can never render ok, whatever produced it. In
	// shadow enforcement the copy says so plainly rather than implying a
	// block that does not exist yet.
	blockDetail := "drawdown block tier is breached; risk-increasing orders are the enforcement target"
	if strings.EqualFold(c.Enforcement, "shadow") {
		blockDetail = "drawdown block tier is breached; shadow enforcement journals what would block — nothing is blocked yet, and reductions and closes stay available"
	}
	switch {
	case c.Tier == risk.CapitalTierBlock || c.BlockLatched || (c.ConsumedPct != nil && *c.ConsumedPct >= 100):
		out.Capital.BriefRowState = briefAttention(blockDetail)
	case c.Tier == risk.CapitalTierWarn:
		out.Capital.BriefRowState = briefAttention("advisory drawdown tier is breached; consumed risk capital needs eyes")
	case len(policy.Unapproved) > 0 || c.Tier == risk.CapitalTierUnapproved || c.ConsumedPct == nil:
		out.Capital.BriefRowState = briefDegraded("one or more capital inputs or policy decisions are unapproved")
	case c.Tier == risk.CapitalTierUnknown:
		out.Capital.BriefRowState = briefDegraded("capital state cannot be evaluated from current inputs")
	}
	out.Latch = rpc.BriefLatchRow{BriefRowState: briefOK("drawdown latch is not engaged"), Latched: c.BlockLatched, At: c.LatchedAt,
		ConsumedPctAtLatch: c.LatchConsumedPct}
	if c.BlockLatched {
		age := max(int(now.Sub(c.LatchedAt).Hours()/24), 0)
		out.Latch.AgeDays = &age
		// An engaged latch is an active risk state, not a healthy steady
		// state; it holds attention until the journaled human reset.
		out.Latch.BriefRowState = briefAttention("drawdown latch is engaged and remains so until a human reset")
	}
	out.Overrides.BriefRowState = briefOK("no active overrides")
	for _, o := range policy.Overrides {
		if o.Active && !now.After(o.ExpiresAt) {
			out.Overrides.Rows = append(out.Overrides.Rows, rpc.BriefOverride{Control: o.Control, ExpiresAt: o.ExpiresAt})
		}
	}
	if len(out.Overrides.Rows) > 0 {
		verb := "widen"
		if len(out.Overrides.Rows) == 1 {
			verb = "widens"
		}
		out.Overrides.BriefRowState = briefAttention(fmt.Sprintf("%d active %s temporarily %s policy controls",
			len(out.Overrides.Rows), pluralNoun(len(out.Overrides.Rows), "override"), verb))
	}
	out.PolicyDrift.BriefRowState = briefOK("all approval pins match")
	for _, pin := range policy.Inventory {
		if pin.Status != "match" {
			out.PolicyDrift.Rows = append(out.PolicyDrift.Rows, pin)
		}
	}
	if len(out.PolicyDrift.Rows) > 0 {
		out.PolicyDrift.BriefRowState = briefDegraded(fmt.Sprintf("%d sibling-policy approval pin(s) do not match", len(out.PolicyDrift.Rows)))
	}
	out.BriefRowState = briefSectionState("risk and limits", out.Capital.BriefRowState, out.Latch.BriefRowState,
		out.Overrides.BriefRowState, out.PolicyDrift.BriefRowState)
	return out
}

func (s *Server) composeBriefProcess(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, recon *rpc.ReconResult, rules *rpc.RulesResult, now time.Time) rpc.BriefProcessSection {
	return s.composeBriefProcessForAuthority(policy, constitution, recon, rules, s.currentNudgeAuthority(now), now)
}

func (s *Server) composeBriefProcessForAuthority(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, recon *rpc.ReconResult, rules *rpc.RulesResult, authority nudgeAuthorityState, now time.Time) rpc.BriefProcessSection {
	out := rpc.BriefProcessSection{}
	if policy == nil {
		out.Reconcile.BriefRowState = briefUnavailable("risk policy unavailable")
	} else {
		capital := policy.Capital
		out.Reconcile = rpc.BriefReconcileRow{BriefRowState: briefOK("reconcile evidence and shared constitution clock"),
			LastReconciledAt: capital.LastReconciledAt, Source: capital.LastReconcileSource}
		clock := s.riskCapital.UnreconciledClockForScope(constitution, now, authority.scope)
		if !clock.Approved {
			out.Reconcile.BriefRowState = briefDegraded("capital.max_unreconciled_days is unapproved")
		} else if capital.LastReconciledAt.IsZero() {
			out.Reconcile.BriefRowState = briefDegraded("no reconcile evidence has been recorded")
		} else {
			out.Reconcile.Deadline, out.Reconcile.DaysRemaining = clock.Deadline, clock.DaysRemaining
			if clock.Stale {
				out.Reconcile.BriefRowState = briefDegraded("reconcile evidence is past its declared horizon")
			}
		}
	}
	if recon == nil {
		out.AutoExtend.BriefRowState = briefUnavailable("reconciliation report unavailable")
		out.OneTap.BriefRowState = briefUnavailable("reconciliation report unavailable")
	} else {
		out.AutoExtend = rpc.BriefAutoExtendRow{BriefRowState: briefOK("no automatic extension recorded"),
			ReportID: recon.LastAutoExtendReportID, At: recon.LastAutoExtendedAt}
		if recon.LastAutoExtendReportID != "" {
			out.AutoExtend.Detail = "latest clean-report automatic extension"
		}
		_, blockers := s.reconcileReportAssessmentForScope(recon.ReportID, authority.scope)
		out.OneTap = rpc.BriefOneTapRow{BriefRowState: briefOK("current report is signable"), ReportID: recon.ReportID,
			Signable: len(blockers) == 0, Blockers: blockers}
		if len(blockers) > 0 {
			out.OneTap.BriefRowState = briefDegraded("current report is not signable")
		}
	}
	out.RulesDelta = s.briefRulesDelta(rules)
	out.Artefacts = composeBriefArtefacts(policy, constitution, now)
	if constitution != nil && constitution.PolicyVersion >= 4 {
		evaluation, completion := s.governanceMonthlyPulseForAuthority(authority, constitution, recon, now)
		if evaluation.Status == risk.MonthlyPulseStatusBlocked {
			if recovery := s.governanceMonthlyPulseForRenderRecovery(authority, constitution, now); recovery.Status != "" {
				evaluation = recovery
			}
		}
		out.MonthlyPulse = &rpc.BriefMonthlyPulseRow{
			Status: evaluation.Status, Month: evaluation.Month, DueAt: evaluation.DueAt,
		}
		if completion != nil && evaluation.Status == risk.MonthlyPulseStatusCompleted {
			out.MonthlyPulse.CompletedAt = completion.CompletedAt
		}
	}
	out.BriefRowState = briefProcessSectionState(out)
	return out
}

func (s *Server) briefMonthlyPulse(constitution *risk.Constitution, _ *rpc.RiskPolicyResult, report *rpc.ReconResult, now time.Time) (risk.MonthlyPulseEvaluation, *risk.MonthlyPulseCompletion) {
	return s.governanceMonthlyPulse(constitution, report, now)
}

func briefProcessSectionState(process rpc.BriefProcessSection) rpc.BriefRowState {
	rows := []rpc.BriefRowState{
		process.Reconcile.BriefRowState, process.AutoExtend.BriefRowState,
		process.OneTap.BriefRowState, process.RulesDelta.BriefRowState,
		process.Artefacts.BriefRowState,
	}
	if process.MonthlyPulse != nil {
		rows = append(rows, briefMonthlyPulseRollupState(process.MonthlyPulse.Status))
	}
	return briefSectionState("process", rows...)
}

// briefMonthlyPulseRollupState maps the monthly-pulse status vocabulary onto a
// section-rollup row state. Shared so the Ready movement and the legacy process
// rollup treat a due/blocked pulse identically.
func briefMonthlyPulseRollupState(status string) rpc.BriefRowState {
	switch status {
	case rpc.BriefMonthlyPulseNotDue, rpc.BriefMonthlyPulseCompleted:
		return briefOK("monthly pulse is current")
	case rpc.BriefMonthlyPulseDue:
		return briefDegraded("monthly pulse is due")
	default:
		return briefDegraded("monthly pulse is blocked by policy evidence")
	}
}

func policyPinsReady(inventory []rpc.PolicyPinStatus) bool {
	return policyPinsReadable(inventory, true)
}

func policyPinsReadable(inventory []rpc.PolicyPinStatus, requireMatch bool) bool {
	if len(inventory) != 3 {
		return false
	}
	want := map[string]bool{"rulebook": false, "protection": false, "stress": false}
	for _, pin := range inventory {
		statusReadable := pin.Status == "match" || (!requireMatch && pin.Status == "drift")
		if _, known := want[pin.Policy]; !known || want[pin.Policy] || !statusReadable || pin.PinnedID == "" || pin.PinnedVersion == "" || pin.LiveID == "" || pin.LiveVersion == "" {
			return false
		}
		want[pin.Policy] = true
	}
	return true
}

func (s *Server) briefRulesDelta(current *rpc.RulesResult) rpc.BriefRulesDeltaRow {
	row := rpc.BriefRulesDeltaRow{BriefRowState: briefOK("no rule status changes since the last stamped brief")}
	if current == nil {
		row.BriefRowState = briefUnavailable("rulebook snapshot unavailable")
		return row
	}
	if current.PolicyFingerprint != nil {
		row.CurrentFingerprint = current.PolicyFingerprint.Key
	}
	baseline, ok := s.briefState.latestBaseline()
	if !ok {
		row.BriefRowState = briefDegraded("no delta baseline yet")
		return row
	}
	row.BaselineAt, row.BaselineFingerprint = baseline.At, baseline.RulebookFingerprint
	row.RulebookFingerprintChanged = baseline.RulebookFingerprint != row.CurrentFingerprint
	base := make(map[string]string, len(baseline.Rules))
	for _, r := range baseline.Rules {
		base[r.ID] = r.Status
	}
	seen := map[string]bool{}
	for _, r := range current.Rules {
		seen[r.ID] = true
		old, exists := base[r.ID]
		switch {
		case !exists:
			row.Added = append(row.Added, r.ID)
		case old != r.Status:
			row.Transitions = append(row.Transitions, rpc.BriefRuleTransition{RuleID: r.ID, From: old, To: r.Status})
		}
	}
	for id := range base {
		if !seen[id] {
			row.Removed = append(row.Removed, id)
		}
	}
	slices.Sort(row.Added)
	slices.Sort(row.Removed)
	actTransitions := 0
	for _, t := range row.Transitions {
		if t.To == risk.RuleStatusAct {
			actTransitions++
		}
	}
	switch {
	case actTransitions > 0:
		// A transition into act is a risk deterioration, not a bookkeeping
		// delta; it must not hide under data-quality vocabulary.
		row.BriefRowState = briefAttention(fmt.Sprintf("rulebook changed since the last stamped brief; %d %s worsened to act",
			actTransitions, pluralNoun(actTransitions, "rule")))
	case row.RulebookFingerprintChanged || len(row.Transitions)+len(row.Added)+len(row.Removed) > 0:
		row.BriefRowState = briefDegraded("rulebook changed since the last stamped brief")
	}
	return row
}

func composeBriefArtefacts(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, now time.Time) rpc.BriefArtefactsRow {
	row := rpc.BriefArtefactsRow{BriefRowState: briefOK("declared cadence completion; no overdue judgment")}
	classes := map[string]string{}
	if constitution != nil {
		classes[rpc.BriefKindMorning] = constitution.Cadence.Morning.Class
		classes[rpc.BriefKindEOD] = constitution.Cadence.EOD.Class
		classes["weekly"] = constitution.Cadence.Weekly.Class
	}
	for _, kind := range []string{rpc.BriefKindMorning, rpc.BriefKindEOD, "weekly"} {
		cadence := "daily"
		if kind == "weekly" {
			cadence = "weekly"
		}
		item := rpc.BriefArtefact{BriefRowState: briefUnavailable("artefact cadence is not declared"), Kind: kind, Cadence: cadence, Declared: classes[kind] != ""}
		if item.Declared {
			item.BriefRowState = briefOK("declared; completion state only, with no overdue judgment")
		}
		if rec, ok := artefactCompletedInPeriod(policyCadence(policy), kind, now); ok {
			item.Completed, item.CompletedAt = true, rec.CompletedAt
			item.Declared = rec.Class != ""
			item.BriefRowState = briefOK("completed in the current cadence period")
		}
		row.Rows = append(row.Rows, item)
	}
	declared := 0
	for _, item := range row.Rows {
		if item.Declared {
			declared++
		}
	}
	if declared == 0 {
		row.BriefRowState = briefUnavailable("cadence artefacts are unapproved or undeclared")
	} else if declared < len(row.Rows) {
		row.BriefRowState = briefDegraded("one or more cadence artefacts are unapproved or undeclared")
	}
	return row
}

func policyCadence(policy *rpc.RiskPolicyResult) []rpc.ArtefactRecord {
	if policy == nil {
		return nil
	}
	return policy.Cadence
}

func briefStampTarget(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, now time.Time) (string, string) {
	if policy == nil || constitution == nil {
		return "", "daily artefact cadence is unapproved"
	}
	declared := map[string]bool{
		rpc.BriefKindMorning: constitution.Cadence.Morning.Class != "",
		rpc.BriefKindEOD:     constitution.Cadence.EOD.Class != "",
	}
	for _, kind := range []string{rpc.BriefKindMorning, rpc.BriefKindEOD} {
		if !declared[kind] {
			return "", kind + " artefact cadence is unapproved"
		}
		if _, done := artefactCompletedInPeriod(policy.Cadence, kind, now); !done {
			return kind, ""
		}
	}
	return "", "both daily artefacts complete"
}

func (s *Server) briefStampTarget(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, now time.Time) (string, string) {
	if constitution != nil && constitution.PolicyVersion >= 4 {
		monthly, _ := s.briefMonthlyPulse(constitution, policy, s.buildReconReport(), now)
		switch monthly.Status {
		case risk.MonthlyPulseStatusDue:
			return rpc.BriefKindMonthly, ""
		case risk.MonthlyPulseStatusBlocked:
			return "", "monthly pulse is blocked by cadence or sibling-pin evidence"
		}
	}
	return briefStampTarget(policy, constitution, now)
}

func artefactCompletedInPeriod(records []rpc.ArtefactRecord, kind string, now time.Time) (rpc.ArtefactRecord, bool) {
	for _, rec := range records {
		if rec.Artefact != kind || rec.CompletedAt.IsZero() {
			continue
		}
		if kind == "weekly" {
			y1, w1 := rec.CompletedAt.In(time.Local).ISOWeek()
			y2, w2 := now.In(time.Local).ISOWeek()
			if y1 == y2 && w1 == w2 {
				return rec, true
			}
		} else if sameLocalDay(rec.CompletedAt, now) {
			return rec, true
		}
	}
	return rpc.ArtefactRecord{}, false
}

func sameLocalDay(a, b time.Time) bool { return localDay(a) == localDay(b) }
func localDay(t time.Time) string      { return t.In(time.Local).Format(time.DateOnly) }

func briefContentFingerprint(res *rpc.BriefResult) string {
	projection := struct {
		Review rpc.BriefReviewSection
		Ready  rpc.BriefReadySection
	}{res.Review, res.Ready}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func briefOK(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusOK, Detail: nonEmptyString(detail, "available")}
}
func briefAttention(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusAttention, Detail: nonEmptyString(detail, "needs attention")}
}
func briefDegraded(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusDegraded, Detail: nonEmptyString(detail, "degraded")}
}
func briefUnavailable(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusUnavailable, Detail: nonEmptyString(detail, "unavailable")}
}

// briefSectionState rolls a section up to its worst child — attention
// outranks data problems — and states completeness separately in the detail
// so an all-green header can never sit above a row that needs eyes.
func briefSectionState(name string, rows ...rpc.BriefRowState) rpc.BriefRowState {
	ok, attention, unavailable := 0, 0, 0
	for _, row := range rows {
		switch row.Status {
		case rpc.BriefStatusOK:
			ok++
		case rpc.BriefStatusAttention:
			attention++
		case rpc.BriefStatusUnavailable:
			unavailable++
		}
	}
	degraded := len(rows) - ok - attention - unavailable
	if len(rows) > 0 && unavailable == len(rows) {
		return briefUnavailable(name + " section unavailable")
	}
	if attention > 0 {
		verb := "need"
		if attention == 1 {
			verb = "needs"
		}
		detail := fmt.Sprintf("%s: %d of %d %s %s attention", name, attention, len(rows), pluralNoun(len(rows), "row"), verb)
		if degraded+unavailable > 0 {
			detail += fmt.Sprintf("; %d degraded or unavailable", degraded+unavailable)
		}
		return briefAttention(detail)
	}
	if ok != len(rows) {
		return briefDegraded(fmt.Sprintf("%s: %d of %d %s degraded or unavailable", name, degraded+unavailable, len(rows), pluralNoun(len(rows), "row")))
	}
	return briefOK(name + " section complete")
}

func mapKeysSorted(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		if key != "" {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}
