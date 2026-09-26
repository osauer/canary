package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// riskPolicyEvaluation captures the policy in force, file health and scoped
// capital together. Reminder eligibility is a consumer of this read, never the
// authority through which capital calculations obtain their inputs.
type riskPolicyEvaluation struct {
	manager      riskPolicySnapshot
	policy       *risk.Constitution
	scope        brokerStateScope
	report       rpc.RiskPolicyResult
	loadedAt     time.Time
	capitalNudge riskCapitalNudgeSnapshot
}

func (s *Server) acceptedRiskPolicy(now time.Time) riskPolicyEvaluation {
	state := riskPolicyEvaluation{}
	if s == nil {
		return state
	}
	state.scope = s.currentBrokerStateScope()
	mgr := s.riskPolicies.snapshot()
	state.manager, state.policy, state.loadedAt = mgr, mgr.policy, mgr.loadedAt.UTC()
	state.report = rpc.RiskPolicyResult{
		AsOf: now.UTC(), Status: mgr.status, Source: mgr.source, Path: mgr.path, Message: mgr.message, Review: mgr.review,
	}
	if c := mgr.policy; c != nil {
		state.report.PolicyID, state.report.PolicyVersion = c.PolicyID, c.PolicyVersion
		state.report.Unapproved = c.UnapprovedKeys()
		state.report.Inventory = s.riskPolicyInventory(c)
		state.report.SignoffRequired = c.SignoffRequired()
		state.report.PolicyFingerprint = &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: c.FingerprintKey()}
		if s.riskCapital != nil {
			state.capitalNudge = s.riskCapital.NudgeSnapshotForScope(c, nil, state.scope)
			state.report.Capital = state.capitalNudge.Report
		}
	}
	return state
}
