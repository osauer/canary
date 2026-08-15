package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func signoffTestConstitution(require *bool) *risk.Constitution {
	c := &risk.Constitution{
		Kind:          risk.ConstitutionKind,
		SchemaVersion: 1,
		PolicyID:      "risk-constitution",
		PolicyVersion: 4,
	}
	c.Inventory.RequireSignoff = require
	return c
}

func driftInventory() []rpc.PolicyPinStatus {
	return []rpc.PolicyPinStatus{
		{Policy: "rulebook", PinnedID: "rulebook-v2", PinnedVersion: "2", LiveID: "rulebook-v2", LiveVersion: "2", Status: "match"},
		{Policy: "protection", PinnedID: "protection-mvp", PinnedVersion: "3", LiveID: "protection-mvp", LiveVersion: "5", Status: "drift"},
		{Policy: "stress", PinnedID: "active-v1", PinnedVersion: "risk-policy-v1", LiveID: "active-v1", LiveVersion: "risk-policy-v1", Status: "match"},
	}
}

func TestComposeBriefRiskPolicyDriftFollowsSignoffMode(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)

	informational := composeBriefRisk(&rpc.RiskPolicyResult{Inventory: driftInventory()}, now)
	if got := informational.PolicyDrift.Status; got != rpc.BriefStatusOK {
		t.Fatalf("drift without sign-off = %s, want ok (informational)", got)
	}
	if len(informational.PolicyDrift.Rows) != 1 || informational.PolicyDrift.Rows[0].Policy != "protection" {
		t.Fatalf("informational mode must still disclose the changed sibling: %+v", informational.PolicyDrift.Rows)
	}
	if !strings.Contains(informational.PolicyDrift.Detail, "sign-off is not required") {
		t.Fatalf("informational detail = %q", informational.PolicyDrift.Detail)
	}

	strict := composeBriefRisk(&rpc.RiskPolicyResult{Inventory: driftInventory(), SignoffRequired: true}, now)
	if got := strict.PolicyDrift.Status; got != rpc.BriefStatusDegraded {
		t.Fatalf("drift with sign-off required = %s, want degraded", got)
	}

	unavailable := driftInventory()
	unavailable[1] = rpc.PolicyPinStatus{Policy: "protection", Status: "unavailable"}
	unreadable := composeBriefRisk(&rpc.RiskPolicyResult{Inventory: unavailable}, now)
	if got := unreadable.PolicyDrift.Status; got != rpc.BriefStatusDegraded {
		t.Fatalf("an unreadable live identity is a data gap in any mode, got %s", got)
	}
}

func TestGovernanceMonthlyPulseIgnoresPinsWithoutSignoff(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	var s Server

	run := func(c *risk.Constitution) risk.MonthlyPulseEvaluation {
		authority := nudgeAuthorityState{cadenceEligible: true, policyIdentity: nudgePolicyIdentity(c)}
		authority.report.Inventory = driftInventory()
		return s.governanceMonthlyPulseForAuthority(authority, c, nil, now)
	}

	if got := run(signoffTestConstitution(nil)); got.Status != risk.MonthlyPulseStatusCompleted {
		t.Fatalf("drift without sign-off must not hold the pulse open: %+v", got)
	}
	if got := run(signoffTestConstitution(new(true))); got.Status != risk.MonthlyPulseStatusBlocked {
		t.Fatalf("drift with require_signoff=true must block the pulse: %+v", got)
	}
}

func TestRulesEarningsSourceHealthQuietsUnentitledProvider(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	failedAt := now.Add(-2 * time.Hour)
	unentitled := &rpc.SourceFailure{Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageWSHMetadata, FailedAt: failedAt, Retryable: false}
	transport := &rpc.SourceFailure{Code: rpc.SourceFailureTransportFailed, Stage: rpc.SourceFailureStageWSHContractResolve, FailedAt: failedAt.Add(-time.Hour), Retryable: true}
	rejected := &rpc.SourceFailure{Code: rpc.SourceFailureProtocolRejected, Stage: rpc.SourceFailureStageWSHDecode, FailedAt: failedAt.Add(-2 * time.Hour), Retryable: false}

	infos := []rpc.EarningsInfo{
		{Symbol: "SPY", Status: rpc.EarningsStatusNotApplicable, Providers: []rpc.EarningsProviderInfo{
			{Provider: "ibkr_wsh", Status: rpc.EarningsStatusTransportFailure, LastFailure: unentitled},
		}},
		{Symbol: "HGENQ", Status: rpc.EarningsStatusTerminalNonReporting, Providers: []rpc.EarningsProviderInfo{
			{Provider: "ibkr_wsh", Status: rpc.EarningsStatusTransportFailure, LastFailure: transport},
		}},
		{Symbol: "AAA", Status: rpc.EarningsStatusDate, Providers: []rpc.EarningsProviderInfo{
			{Provider: "ibkr_wsh", Status: rpc.EarningsStatusTransportFailure, LastFailure: rejected},
		}},
	}
	health, degraded := rulesEarningsSourceHealth(infos, now)
	if degraded {
		t.Fatalf("resolved symbols must not degrade earnings health: %+v", health)
	}
	notes := strings.Join(health.Notes, "\n")
	if strings.Contains(notes, "not_entitled") {
		t.Errorf("permanently-unentitled provider must not surface a retained-issue note: %q", notes)
	}
	if !strings.Contains(notes, "source=ibkr_wsh code=transport_failed stage=wsh_contract_resolve retry=scheduled") {
		t.Errorf("retryable failure lost its retained-issue note: %q", notes)
	}
	if !strings.Contains(notes, "code=protocol_rejected stage=wsh_decode retry=daily") {
		t.Errorf("non-retryable failure must disclose the daily cadence, not retry=scheduled: %q", notes)
	}
}

func TestBriefEarningsOverrideHintNamesTheOperatorAction(t *testing.T) {
	rules := &rpc.RulesResult{Earnings: []rpc.EarningsInfo{
		{Symbol: "NOW", Status: rpc.EarningsStatusNoDatePublished},
		{Symbol: "SPY", Status: rpc.EarningsStatusNotApplicable},
	}}
	hint := briefEarningsOverrideHint(rules)
	if !strings.Contains(hint, "NOW") || !strings.Contains(hint, "features.rulebook.earnings_overrides") {
		t.Fatalf("hint = %q", hint)
	}
	if briefEarningsOverrideHint(&rpc.RulesResult{}) != "" {
		t.Fatal("hint must be empty without a no_date_published name")
	}
}

func TestFlagClosedOptionSessionUsesTheOfficialCalendar(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	options := []rpc.PositionView{{Symbol: "NOW", Expiry: "20261016", Right: "C", Strike: 100}}

	// Thanksgiving 2026: a weekday the holiday-blind clock helper calls open.
	flagClosedOptionSession(options, time.Date(2026, time.November, 26, 12, 0, 0, 0, newYork))
	if !positionWarningHasCode(options[0].WarningDetails, "options_closed") {
		t.Fatal("holiday session must carry the options_closed warning")
	}

	open := []rpc.PositionView{{Symbol: "NOW", Expiry: "20261016", Right: "C", Strike: 100}}
	flagClosedOptionSession(open, time.Date(2026, time.August, 13, 12, 0, 0, 0, newYork))
	if positionWarningHasCode(open[0].WarningDetails, "options_closed") {
		t.Fatal("regular open session must not flag options_closed")
	}
}
