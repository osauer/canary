package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Risk-constitution RPC handlers (internal-docs/design/risk-policy.md). The snapshot
// is read-only and must work without gateway connectivity (persisted last
// equity serves, disclosed as stale). The write handlers are governance
// acts: human-origin-only, journaled, and none of them can reach submit
// eligibility, blockers, freeze, pins, or tokens.

func (s *Server) handleRiskPolicySnapshot(ctx context.Context, _ *rpc.Request) (*rpc.RiskPolicyResult, error) {
	now := time.Now()
	mgr := s.riskPolicies.snapshot()
	res := &rpc.RiskPolicyResult{
		AsOf:    now,
		Status:  mgr.status,
		Source:  mgr.source,
		Path:    mgr.path,
		Message: mgr.message,
	}
	if c := mgr.policy; c != nil {
		res.PolicyID = c.PolicyID
		res.PolicyVersion = c.PolicyVersion
		res.PolicyFingerprint = &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: c.FingerprintKey()}
		res.Unapproved = c.UnapprovedKeys()
	} else {
		res.Unapproved = (&risk.Constitution{}).UnapprovedKeys()
	}

	var health []rpc.SourceHealth
	policyHealth := rpc.SourceHealth{Source: "risk_policy", Status: "ok", AsOf: mgr.loadedAt}
	if mgr.status != rpc.RiskPolicyStatusActive {
		policyHealth.Status = mgr.status
		policyHealth.Notes = []string{mgr.message}
	}
	health = append(health, policyHealth)

	// Best-effort fresh equity observation; degrade to the persisted last
	// reading (Report handles nil obs) rather than failing the snapshot.
	var obs *risk.CapitalObservation
	acct, acctErr := s.handleAccountSummary(ctx)
	switch {
	case acctErr != nil || acct == nil:
		health = append(health, rpc.SourceHealth{Source: "account", Status: "unavailable",
			Notes: []string{errText(acctErr), "serving the persisted last equity observation, if any"}})
	case mgr.policy != nil && mgr.policy.Capital.BaseCurrency != "" && acct.BaseCurrency != "" &&
		!strings.EqualFold(mgr.policy.Capital.BaseCurrency, acct.BaseCurrency):
		health = append(health, rpc.SourceHealth{Source: "account", Status: "mismatch",
			Notes: []string{fmt.Sprintf("account base currency %s does not match capital.base_currency %s; the observation is unusable for capital math", acct.BaseCurrency, mgr.policy.Capital.BaseCurrency)}})
	default:
		obs = &risk.CapitalObservation{EquityBase: acct.NetLiquidation, AsOf: acct.AsOf}
		health = append(health, rpc.SourceHealth{Source: "account", Status: "ok", AsOf: acct.AsOf})
	}

	res.Capital = s.riskCapital.Report(mgr.policy, obs)
	res.Limits = risk.ConstitutionLimits(mgr.policy)
	res.Overrides = s.riskCapital.ActiveOverrides()
	res.Cadence = s.riskCapital.Artefacts()
	res.Inventory = s.riskPolicyInventory(mgr.policy)
	res.InputHealth = health
	return res, nil
}

// riskPolicyInventory compares the constitution's sibling-policy pins with
// live identities. Pins are identity references only; the siblings stay
// authoritative for their own thresholds.
func (s *Server) riskPolicyInventory(c *risk.Constitution) []rpc.PolicyPinStatus {
	rb := risk.DefaultRulebookPolicy()
	stress := risk.DefaultPolicy()
	rows := []rpc.PolicyPinStatus{
		pinStatus("rulebook", pinOf(c, func(cc *risk.Constitution) *risk.ConstitutionPolicyPin { return cc.Inventory.Rulebook }), rb.ID, strconv.Itoa(rb.Version)),
		pinStatus("stress", pinOf(c, func(cc *risk.Constitution) *risk.ConstitutionPolicyPin { return cc.Inventory.Stress }), stress.PolicyProfile(), stress.PolicyVersion()),
	}
	if s.protectionPolicies != nil {
		st := s.protectionPolicies.Status()
		rows = append(rows, pinStatus("protection", pinOf(c, func(cc *risk.Constitution) *risk.ConstitutionPolicyPin { return cc.Inventory.Protection }), st.PolicyID, strconv.Itoa(st.PolicyVersion)))
	} else {
		rows = append(rows, rpc.PolicyPinStatus{Policy: "protection", Status: "unavailable"})
	}
	return rows
}

func pinOf(c *risk.Constitution, pick func(*risk.Constitution) *risk.ConstitutionPolicyPin) *risk.ConstitutionPolicyPin {
	if c == nil {
		return nil
	}
	return pick(c)
}

func pinStatus(name string, pin *risk.ConstitutionPolicyPin, liveID, liveVersion string) rpc.PolicyPinStatus {
	row := rpc.PolicyPinStatus{Policy: name, LiveID: liveID, LiveVersion: liveVersion, Status: "unpinned"}
	if pin != nil {
		row.PinnedID = pin.ID
		row.PinnedVersion = pin.Version
		if strings.EqualFold(pin.ID, liveID) && pin.Version == liveVersion {
			row.Status = "match"
		} else {
			row.Status = "drift"
		}
	}
	return row
}

// requireHumanRiskPolicyOrigin gates every risk-policy write. These are
// governance acts, not broker writes: they are human-only regardless of
// trading mode (interview decision 6), and no environment claim can force a
// human classification (cli.DetectWriteOrigin can only restrict).
func requireHumanRiskPolicyOrigin(origin string) error {
	if !originIsHuman(origin) {
		return errBadRequest("risk-policy writes are human-only; run this from an interactive terminal (agent-origin requests are recorded and refused)")
	}
	return nil
}

func (s *Server) handleRiskPolicyCapitalEvent(_ context.Context, req *rpc.Request) (*rpc.RiskPolicyWriteResult, error) {
	var p rpc.CapitalEventParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := requireHumanRiskPolicyOrigin(p.Origin); err != nil {
		return nil, err
	}
	s.reconMu.Lock()
	defer s.reconMu.Unlock()
	var recon *capitalReconRef
	if strings.EqualFold(strings.TrimSpace(p.Type), "reconcile") {
		if strings.TrimSpace(p.Report) == "" {
			// One-tap default: resolve the current report daemon-side, then
			// pass its id through the exact same gate as an explicit id.
			p.Report = s.buildReconReport().ReportID
		}
		rep, err := s.reconcileReportGate(p.Report)
		if err != nil {
			return nil, err
		}
		recon = &capitalReconRef{ReportID: rep.ReportID, CoverageTo: rep.CoverageTo}
	}
	pol := s.riskPolicies.snapshot().policy
	ev, err := s.riskCapital.ApplyCapitalEventForPolicy(p, normalizedWriteOrigin(p.Origin), pol, recon)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	msg := fmt.Sprintf("recorded %s capital event", ev.Type)
	if ev.Type == "reconcile" {
		msg = fmt.Sprintf("recorded reconcile sign-off against report %s; the unreconciled clock restarts now", strings.TrimSpace(p.Report))
	}
	return &rpc.RiskPolicyWriteResult{OK: true, At: ev.At, Message: msg}, nil
}

// reconcileReportGate enforces the phase-3a contract: a reconcile is a
// sign-off against a specific, fresh, fully resolved recon report — never
// a bare attestation (internal-docs/design/post-trade-truth.md; operator decision
// 2026-07-13, no shadow period). The sanctioned escape during statement
// outages is a one-shot override on capital.max_unreconciled_days, not a
// soft mode here.
func (s *Server) reconcileReportGate(reportID string) (*rpc.ReconResult, error) {
	rep, blockers := s.reconcileReportAssessment(reportID)
	if len(blockers) > 0 {
		return nil, errBadRequest(blockers[0])
	}
	return rep, nil
}

// reconcileReportAssessment is the single source of truth for report
// signability. The write gate returns its first blocker verbatim; the daily
// brief exposes the full ordered list. No caller may infer signability from a
// second set of conditions.
func (s *Server) reconcileReportAssessment(reportID string) (*rpc.ReconResult, []string) {
	reportID = strings.TrimSpace(reportID)
	if reportID == "" {
		return nil, []string{"current reconcile report is unavailable to sign off; review `ibkr recon` for its blocking status"}
	}
	rep := s.buildReconReport()
	switch rep.Status {
	case rpc.ReconStatusActive, rpc.ReconStatusDegraded:
	case rpc.ReconStatusUnapproved:
		return rep, []string{"recon.* policy keys are unapproved; reconcile is unavailable until they exist in the risk policy"}
	default:
		return rep, []string{"no recon report can be built: " + nonEmptyString(rep.Message, rep.Status)}
	}
	if rep.ReportID != reportID {
		return rep, []string{fmt.Sprintf("report %s is superseded; review the current report %s (`ibkr recon`) and sign that off", reportID, rep.ReportID)}
	}
	pol := s.riskPolicies.snapshot().policy
	rc := reconPolicyOf(pol)
	if rc == nil {
		return rep, []string{"recon.* policy keys are unapproved; reconcile is unavailable until they exist in the risk policy"}
	}
	var blockers []string
	if age := time.Since(rep.StatementAsOf); age > time.Duration(*rc.MaxReportAgeDays)*24*time.Hour {
		blockers = append(blockers, fmt.Sprintf("newest ingested statement is %.0f days old, past recon.max_report_age_days=%d; fetch fresher statements first (or, during an outage, grant a one-shot override on capital.max_unreconciled_days)", age.Hours()/24, *rc.MaxReportAgeDays))
	}
	if rep.Unresolved > 0 {
		blockers = append(blockers, fmt.Sprintf("report %s carries %d unresolved exception(s); declare the missing events or dismiss each with a reason, then reconcile", reportID, rep.Unresolved))
	}
	return rep, blockers
}

func (s *Server) handleReconSnapshot(ctx context.Context, req *rpc.Request) (*rpc.ReconResult, error) {
	var p rpc.ReconSnapshotParams
	if len(req.Params) > 0 {
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	if p.Refresh {
		s.kickFlexFetch(ctx)
	}
	return s.buildReconReport(), nil
}

func (s *Server) currentReconAutomationStatus(ctx context.Context) (rpc.ReconAutomationStatus, error) {
	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	status := s.reconAutomationStatus(nil, now)
	if status.Report.State != rpc.ReconReportStateCurrent {
		return status, nil
	}
	report, err := s.buildReconReportContext(ctx)
	if err != nil {
		return rpc.ReconAutomationStatus{}, err
	}
	return report.Automation, nil
}

func (s *Server) handleReconStatus(ctx context.Context, req *rpc.Request) (*rpc.ReconStatusResult, error) {
	if len(req.Params) == 0 {
		return nil, errBadRequest("recon status params must be an empty JSON object")
	}
	var params rpc.ReconStatusParams
	if err := decodeParams(req.Params, &params); err != nil {
		return nil, err
	}
	status, err := s.currentReconAutomationStatus(ctx)
	if err != nil {
		return nil, err
	}
	return &rpc.ReconStatusResult{Status: status}, nil
}

func (s *Server) handleReconCheck(ctx context.Context, req *rpc.Request) (*rpc.ReconCheckResult, error) {
	if len(req.Params) == 0 {
		return nil, errBadRequest("recon check params must be an empty JSON object")
	}
	var params rpc.ReconCheckParams
	if err := decodeParams(req.Params, &params); err != nil {
		return nil, err
	}
	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	before := s.flexFetchStatusAt(now)
	outcome := ""
	switch {
	case before.State == rpc.ReconReportStateChecking:
		outcome = rpc.ReconCheckOutcomeAlreadyChecking
	case before.State == rpc.ReconReportStateUnavailable:
		outcome = rpc.ReconCheckOutcomeActionRequired
	case before.State == rpc.ReconReportStateActionRequired && !before.CanCheckNow:
		outcome = rpc.ReconCheckOutcomeActionRequired
	case !before.CanCheckNow:
		outcome = rpc.ReconCheckOutcomeCooldown
	case s.startFlexFetch(ctx, true):
		outcome = rpc.ReconCheckOutcomeStarted
	default:
		after := s.flexFetchStatusAt(now)
		switch {
		case after.State == rpc.ReconReportStateChecking:
			outcome = rpc.ReconCheckOutcomeAlreadyChecking
		case !after.CanCheckNow:
			outcome = rpc.ReconCheckOutcomeCooldown
		default:
			outcome = rpc.ReconCheckOutcomeActionRequired
		}
	}
	status, err := s.currentReconAutomationStatus(ctx)
	if err != nil {
		return nil, err
	}
	return &rpc.ReconCheckResult{Outcome: outcome, Status: status}, nil
}

func (s *Server) handleReconBacktest(ctx context.Context, req *rpc.Request) (*rpc.ReconBacktestResult, error) {
	var p rpc.ReconSnapshotParams
	if len(req.Params) > 0 {
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	if p.Refresh {
		s.kickFlexFetch(ctx)
	}
	return s.buildReconBacktest(), nil
}

func (s *Server) handleReconDismiss(_ context.Context, req *rpc.Request) (*rpc.RiskPolicyWriteResult, error) {
	var p rpc.ReconDismissParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := requireHumanRiskPolicyOrigin(p.Origin); err != nil {
		return nil, err
	}
	s.reconMu.Lock()
	defer s.reconMu.Unlock()
	lineID, reason := strings.TrimSpace(p.LineID), strings.TrimSpace(p.Reason)
	if lineID == "" || reason == "" {
		return nil, errBadRequest("recon dismiss needs both --line and --reason")
	}
	rep := s.buildReconReport()
	found := false
	for _, ex := range rep.Exceptions {
		if ex.LineID == lineID {
			if ex.Dismissed {
				return nil, errBadRequest("line " + lineID + " is already dismissed")
			}
			found = true
			break
		}
	}
	if !found {
		return nil, errBadRequest("line " + lineID + " is not an exception on the current report; run `ibkr recon` for the live list")
	}
	now := time.Now().UTC()
	if err := s.riskCapital.RecordGovernanceEvent(map[string]any{
		"version": 1, "at": now, "kind": "recon_dismiss", "line_id": lineID, "reason": reason,
		"report": rep.ReportID, "policy_fingerprint": constitutionFingerprint(s.riskPolicies.snapshot().policy),
	}); err != nil {
		return nil, fmt.Errorf("persist reconciliation dismissal: %w", err)
	}
	s.kickHistoryIndex()
	return &rpc.RiskPolicyWriteResult{OK: true, At: now,
		Message: "exception dismissed and journaled; the report id changes to reflect it — rerun `ibkr recon` before reconciling"}, nil
}

func (s *Server) handleRiskPolicyOverride(_ context.Context, req *rpc.Request) (*rpc.RiskPolicyWriteResult, error) {
	var p rpc.OverrideParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := requireHumanRiskPolicyOrigin(p.Origin); err != nil {
		return nil, err
	}
	mgr := s.riskPolicies.snapshot()
	rec, err := s.riskCapital.GrantOverride(p, mgr.policy)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	return &rpc.RiskPolicyWriteResult{
		OK: true, At: rec.GrantedAt, Override: &rec,
		Message: "override granted and journaled; it expires on its own — a durable change needs a policy revision (version bump)",
	}, nil
}

func (s *Server) handleRiskPolicyResetDrawdown(_ context.Context, req *rpc.Request) (*rpc.RiskPolicyWriteResult, error) {
	var p rpc.ResetDrawdownParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := requireHumanRiskPolicyOrigin(p.Origin); err != nil {
		return nil, err
	}
	mgr := s.riskPolicies.snapshot()
	if err := s.riskCapital.ResetDrawdown(p.Reason, mgr.policy); err != nil {
		return nil, errBadRequest(err.Error())
	}
	return &rpc.RiskPolicyWriteResult{
		OK: true, At: time.Now().UTC(),
		Message: "drawdown latch cleared and peak re-based to current equity; if the reset should also reduce declared risk capital, publish a policy revision",
	}, nil
}

func (s *Server) handleRiskPolicyCorrectPeak(_ context.Context, req *rpc.Request) (*rpc.RiskPolicyWriteResult, error) {
	var p rpc.CorrectPeakParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := requireHumanRiskPolicyOrigin(p.Origin); err != nil {
		return nil, err
	}
	if p.FromStatements == (p.PeakBase != 0) {
		return nil, errBadRequest("choose exactly one anchor: from_statements or an explicit peak value")
	}
	mgr := s.riskPolicies.snapshot()
	peak, asOf, source := p.PeakBase, time.Time{}, "manual"
	if p.FromStatements {
		bt := s.buildReconBacktest()
		if bt == nil || bt.Replay == nil || bt.Replay.ReplayedPeakBase <= 0 {
			msg := "statement replay peak is unavailable"
			if bt != nil && bt.Message != "" {
				msg += ": " + bt.Message
			}
			return nil, errBadRequest(msg)
		}
		peak, asOf, source = bt.Replay.ReplayedPeakBase, bt.Replay.ReplayedPeakAt, "statement_replay"
	}
	from, err := s.riskCapital.CorrectPeak(peak, asOf, source, p.Reason, mgr.policy)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	return &rpc.RiskPolicyWriteResult{
		OK: true, At: time.Now().UTC(),
		Message: fmt.Sprintf("adjusted peak corrected %.2f → %.2f (%s) and journaled; the drawdown latch is untouched — clearing it stays a separate reset-drawdown decision", from, peak, source),
	}, nil
}

func (s *Server) handleRiskPolicyArtefact(_ context.Context, req *rpc.Request) (*rpc.RiskPolicyWriteResult, error) {
	var p rpc.ArtefactParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := requireHumanRiskPolicyOrigin(p.Origin); err != nil {
		return nil, err
	}
	mgr := s.riskPolicies.snapshot()
	rec, err := s.riskCapital.RecordArtefact(p, mgr.policy)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	return &rpc.RiskPolicyWriteResult{OK: true, At: rec.CompletedAt,
		Message: fmt.Sprintf("recorded %s artefact completion", rec.Artefact)}, nil
}

// riskPolicyPreviewWarnings maps a warn/block capital tier to an advisory
// DataWarning on a risk-increasing order preview. Reduce/close intents and
// policy-classified hedge entries (long put on the rulebook hedge index
// list) never warn — the exemptions of interview decision 4. Cheap by
// construction: in-memory policy + persisted equity, never an account
// fetch. submit_eligible is never affected.
func (s *Server) riskPolicyPreviewWarnings(draft rpc.OrderDraft, position rpc.OrderPositionImpact) []rpc.DataWarning {
	if s.riskPolicies == nil || s.riskCapital == nil {
		return nil
	}
	switch position.Effect {
	case "close", "reduce":
		return nil
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	authority := s.currentNudgeAuthority(now)
	if authority.policy == nil {
		return nil // unapproved constitution: policy show owns that disclosure, not preview noise
	}
	if strings.EqualFold(draft.Action, "BUY") && strings.EqualFold(draft.Contract.SecType, "OPT") &&
		strings.EqualFold(draft.Contract.Right, "P") && risk.DefaultRulebookPolicy().IsHedgeSymbol(draft.Contract.Symbol) {
		return nil // hedge entry stays available under a drawdown breach
	}
	v := authority.capitalNudge.Report
	var severity, tier string
	switch v.Tier {
	case risk.CapitalTierWarn:
		severity, tier = "watch", "warning"
	case risk.CapitalTierBlock:
		severity, tier = "act", "block"
		// Advisory occurrence bookkeeping is attached to this exact typed,
		// risk-increasing, non-hedge preview path. Persistence failure cannot
		// alter the preview warning or any submit-eligibility field.
		if s.nudges != nil {
			if s.shadowBookkeepingHook != nil {
				s.shadowBookkeepingHook()
			}
			if authority.eligible && authority.capitalNudge.LatchOpen {
				_ = s.nudges.recordShadow(authority.policyIdentity, authority.capitalNudge.Episode, true, false, true)
			}
		}
	default:
		return nil
	}
	consumed := "n/a"
	if v.ConsumedPct != nil {
		consumed = fmt.Sprintf("%.1f%%", *v.ConsumedPct)
	}
	return []rpc.DataWarning{{
		Code:     "capital_drawdown",
		Scope:    "risk_policy",
		Severity: severity,
		Message:  fmt.Sprintf("Drawdown %s tier: %s of declared risk capital consumed from the adjusted peak; this order increases risk.", tier, consumed),
		Impact:   fmt.Sprintf("Advisory constitution cause (enforcement %s); submit eligibility is unaffected.", authority.policy.EffectiveBlockEnforcement()),
		Action:   "Run `ibkr policy show --explain` for the capital state and ladder.",
	}}
}
