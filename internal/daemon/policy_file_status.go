package daemon

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// policyFileStatuses reports every policy file for `canary policy show`:
// each manager's status, the unreviewed-template marker, what the file lacks
// or still carries, recommendations Canary reports but never applies (a dry
// run of the ensure step), and the features that stay off until the owner
// writes a number or a decision.
func (s *Server) policyFileStatuses(mgr riskPolicySnapshot) []rpc.PolicyFileStatus {
	if s == nil {
		return nil
	}
	set := PolicyFileSetFor(s.cfg)
	dry := map[string]PolicyFileAction{}
	for _, a := range EnsurePolicyFiles(set, EnsureOptions{DryRun: true, Release: s.version}) {
		dry[a.Policy] = a
	}
	withDry := func(row rpc.PolicyFileStatus) rpc.PolicyFileStatus {
		a, ok := dry[row.Policy]
		if !ok {
			return row
		}
		switch a.Action {
		case PolicyFileWouldCreate:
			row.Notes = append(row.Notes, "no file yet: the next daemon start writes Canary's defaults (or run `canary policy ensure`)")
		case PolicyFileWouldMigrate:
			row.Notes = append(row.Notes, "migration pending at the next daemon start: "+strings.Join(a.Changes, "; "))
		case PolicyFileUnreadable, PolicyFileFailed:
			row.Notes = append(row.Notes, "cannot be read: "+a.Error+"; the policy in force stays")
		}
		row.Notes = append(row.Notes, a.Notes...)
		return row
	}
	var out []rpc.PolicyFileStatus

	rb, rbStatus := s.activeRulebookPolicy()
	row := rpc.PolicyFileStatus{Policy: PolicyFileRulebook, Path: nonEmptyString(rbStatus.Path, set.Rulebook), Status: rbStatus.Status,
		Review: rbStatus.Review, PolicyID: rb.ID, PolicyVersion: strconv.Itoa(rb.Version)}
	if rbStatus.Message != "" {
		row.Notes = append(row.Notes, rbStatus.Message)
	}
	if len(rbStatus.Missing) > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("%d key(s) are not in the file and follow Canary's defaults: %s", len(rbStatus.Missing), strings.Join(rbStatus.Missing, ", ")))
	}
	if len(rb.IssuerGroups) == 0 {
		row.Notes = append(row.Notes, "no issuer groups declared: every symbol is its own issuer")
	}
	if len(rb.Clusters) == 0 {
		row.Notes = append(row.Notes, "no clusters declared: rule 17 is not evaluated")
	}
	out = append(out, withDry(row))

	if s.protectionPolicies != nil {
		p, st := s.protectionPolicies.Active()
		row := rpc.PolicyFileStatus{Policy: PolicyFileProtection, Path: nonEmptyString(st.Path, set.Protection), Status: st.Status,
			Review: st.Review, PolicyID: p.PolicyID, PolicyVersion: strconv.Itoa(p.PolicyVersion)}
		if st.Message != "" {
			row.Notes = append(row.Notes, st.Message)
		}
		if st.AutomationPaused {
			row.Notes = append(row.Notes, "pre-authorised submission is paused until the file reads cleanly; reduce-only proposals continue")
		}
		if len(p.Authority.PreAuthorised) == 0 {
			row.NeedsYourNumber = append(row.NeedsYourNumber, "automatic submission: off until you list reduce-only buckets under [authority].pre_authorised")
		}
		switch budget := p.Buckets.BudgetReduction; {
		case budget == nil:
			row.NeedsYourNumber = append(row.NeedsYourNumber, "premium budget governor: off until you write [buckets.budget_reduction] with your caps")
		case len(budget.missingNumbers()) > 0:
			row.NeedsYourNumber = append(row.NeedsYourNumber, "premium budget governor: off until you write "+strings.Join(budget.missingNumbers(), ", ")+" in [buckets.budget_reduction]")
		case budget.enabled() && budget.basis() == rpc.BudgetBasisDeclaredRiskCapital && (mgr.policy == nil || mgr.policy.Capital.DeclaredRiskCapital == nil):
			row.NeedsYourNumber = append(row.NeedsYourNumber, "premium budget governor: needs capital.declared_risk_capital in risk-policy.toml")
		}
		out = append(out, withDry(row))
	}

	if s.opportunityPolicies != nil {
		p, st := s.opportunityPolicies.Active()
		row := rpc.PolicyFileStatus{Policy: PolicyFileOpportunity, Path: nonEmptyString(st.Path, set.Opportunity), Status: st.Status,
			Review: st.Review, PolicyID: p.PolicyID, PolicyVersion: strconv.Itoa(p.PolicyVersion)}
		if st.Message != "" {
			row.Notes = append(row.Notes, st.Message)
		}
		out = append(out, withDry(row))
	}

	row = rpc.PolicyFileStatus{Policy: PolicyFileConstitution, Path: nonEmptyString(mgr.path, set.Constitution), Status: mgr.status, Review: mgr.review}
	if mgr.message != "" {
		row.Notes = append(row.Notes, mgr.message)
	}
	if c := mgr.policy; c != nil {
		row.PolicyID, row.PolicyVersion = c.PolicyID, strconv.Itoa(c.PolicyVersion)
		if keys := c.UnapprovedKeys(); len(keys) > 0 {
			row.NeedsYourNumber = append(row.NeedsYourNumber, "capital controls and the rule 18 loss budget: "+strings.Join(keys, ", "))
		}
		if strings.TrimSpace(c.Drawdown.Release) == "" {
			row.NeedsYourNumber = append(row.NeedsYourNumber, `automatic brake release: off (a latched brake waits for your reset) until you choose drawdown.release`)
		}
	} else {
		row.NeedsYourNumber = append(row.NeedsYourNumber, "capital controls and the rule 18 loss budget: no constitution is loaded")
	}
	out = append(out, withDry(row))
	return out
}
