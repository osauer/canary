package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func unentitledWSHProviderState() earningsProviderState {
	return earningsProviderState{LastAttempt: earningsProviderAttempt{
		Status: rpc.EarningsStatusTransportFailure,
		LastFailure: &rpc.SourceFailure{
			Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageWSHMetadata, Retryable: false,
		},
	}}
}

// TestUnentitledSecondOpinionIsTheOnlyPermanentlyUnusableState pins the shared
// definition of "we do not hold this feed": a retryable outage is a source that
// is merely down, and must not be read as one that will never answer.
func TestUnentitledSecondOpinionIsTheOnlyPermanentlyUnusableState(t *testing.T) {
	unentitled := unentitledWSHProviderState().LastAttempt
	if !earningsProviderUnentitled(unentitled.Status, unentitled.LastFailure) {
		t.Fatal("a non-retryable entitlement refusal was not recognized")
	}
	for name, failure := range map[string]*rpc.SourceFailure{
		"retryable entitlement": {Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageWSHMetadata, Retryable: true},
		"transport timeout":     {Code: rpc.SourceFailureTimeout, Stage: rpc.SourceFailureStageWSHMetadata, Retryable: false},
		"unrelated stage":       {Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageNasdaqSchema, Retryable: false},
		"no failure recorded":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			if earningsProviderUnentitled(rpc.EarningsStatusTransportFailure, failure) {
				t.Fatalf("%s was treated as a permanent entitlement refusal", name)
			}
		})
	}
}

func TestNonIssuerEarningsSecurityTypeVocabularyIsClosed(t *testing.T) {
	for raw, want := range map[string]string{
		"IND": rpc.SecTypeIndex, "INDEX": rpc.SecTypeIndex,
		"FUT": rpc.SecTypeFuture, "FUTURE": rpc.SecTypeFuture,
		"FUND": "FUND", "BOND": "BOND", "BILL": "BILL", "CASH": "CASH", "CMDTY": "CMDTY",
	} {
		got, ok := nonIssuerEarningsSecurityType(raw)
		if !ok || got != want {
			t.Fatalf("secType %q classified %q/%v, want %q/true", raw, got, ok, want)
		}
	}
	// An equity has issuer earnings, and an unrecognized type is not evidence
	// that earnings do not apply — both must stay outside the exemption.
	for _, raw := range []string{"STK", "STOCK", "ETF", "OPT", "OPTION", "", "WARRANT", "unknown"} {
		if got, ok := nonIssuerEarningsSecurityType(raw); ok {
			t.Fatalf("secType %q was wrongly exempted as %q", raw, got)
		}
	}
}

// TestSecurityTypeEarningsAuthorityRequiresTheTypeItself proves Source alone
// cannot carry the exemption: the composer re-derives it from the named type.
func TestSecurityTypeEarningsAuthorityRequiresTheTypeItself(t *testing.T) {
	valid := rpc.EarningsInfo{
		Symbol: "TESTQ", Source: "security_type", Status: rpc.EarningsStatusNotApplicable,
		Reason: risk.EarningsReasonNonIssuerSecurity, SecurityType: rpc.SecTypeIndex,
	}
	if !validRulebookSecurityTypeEarningsAuthority(valid) {
		t.Fatal("a complete security-type authority was rejected")
	}

	forged := map[string]func(*rpc.EarningsInfo){
		"no security type":     func(i *rpc.EarningsInfo) { i.SecurityType = "" },
		"equity security type": func(i *rpc.EarningsInfo) { i.SecurityType = rpc.SecTypeStock },
		"noncanonical casing":  func(i *rpc.EarningsInfo) { i.SecurityType = "index" },
		"raw broker spelling":  func(i *rpc.EarningsInfo) { i.SecurityType = "IND" },
		"wrong source":         func(i *rpc.EarningsInfo) { i.Source = "fetched" },
		"wrong status":         func(i *rpc.EarningsInfo) { i.Status = rpc.EarningsStatusDate },
		"wrong reason":         func(i *rpc.EarningsInfo) { i.Reason = risk.EarningsReasonBrokerNonIssuer },
		"stale":                func(i *rpc.EarningsInfo) { i.Stale = true },
		"carries a date":       func(i *rpc.EarningsInfo) { i.Date = "2026-08-11" },
		"padded symbol":        func(i *rpc.EarningsInfo) { i.Symbol = " TESTQ" },
	}
	for name, mutate := range forged {
		t.Run(name, func(t *testing.T) {
			info := valid
			mutate(&info)
			if validRulebookSecurityTypeEarningsAuthority(info) {
				t.Fatalf("%s was accepted as security-type authority", name)
			}
		})
	}
}

// TestSecurityTypeExemptionRecoversRulebookCoverage is the issue #19 case: a
// held index used to leave rules 6-8 unknown, which made the whole rulebook
// alert source uncovered no matter how healthy everything else was.
func TestSecurityTypeExemptionRecoversRulebookCoverage(t *testing.T) {
	base := time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC)
	result := alertShadowTestRulebook(base, risk.RuleStatusPass)
	result.Earnings = []rpc.EarningsInfo{{
		Symbol: "TESTQ", Source: "security_type", Status: rpc.EarningsStatusNotApplicable,
		Reason: risk.EarningsReasonNonIssuerSecurity, SecurityType: rpc.SecTypeIndex,
	}}
	for _, id := range []string{risk.RuleCatalystCoverage, risk.RuleOverwriteEarnings, risk.RuleEarningsSizeFreeze} {
		alertShadowTestSetEarningsNotEvaluated(&result, id, risk.EarningsReasonNonIssuerSecurity, "TESTQ")
	}

	batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
	if !batch.Covered || batch.Status != alertShadowStatusCurrent || batch.Reason != alertShadowReasonCurrent {
		t.Fatalf("index-only earnings exemption did not reach coverage: covered=%v status=%q reason=%q",
			batch.Covered, batch.Status, batch.Reason)
	}

	// The same rows without the typed authority behind them must stay refused:
	// a row reason is disclosure, never proof.
	unproven := result
	unproven.Earnings = []rpc.EarningsInfo{{
		Symbol: "TESTQ", Source: "security_type", Status: rpc.EarningsStatusNotApplicable,
		Reason: risk.EarningsReasonNonIssuerSecurity,
	}}
	refused := alertShadowMapRulebook(alertShadowTestBrokerScope(t), unproven, base.Add(time.Second))
	if refused.Covered || refused.Reason != alertShadowReasonCandidateInvalid {
		t.Fatalf("an unproven security-type exemption was trusted: covered=%v reason=%q",
			refused.Covered, refused.Reason)
	}
}

// TestMixedExemptionAuthoritiesRequireTheGenericReason stops one authority
// class masquerading as another once a third class exists.
func TestMixedExemptionAuthoritiesRequireTheGenericReason(t *testing.T) {
	base := time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC)
	result := alertShadowTestRulebook(base, risk.RuleStatusPass)
	result.Earnings = []rpc.EarningsInfo{
		alertShadowTestTerminalEarnings("TERMQ", base),
		{Symbol: "IDXQ", Source: "security_type", Status: rpc.EarningsStatusNotApplicable,
			Reason: risk.EarningsReasonNonIssuerSecurity, SecurityType: rpc.SecTypeIndex},
	}
	for _, id := range []string{risk.RuleCatalystCoverage, risk.RuleOverwriteEarnings, risk.RuleEarningsSizeFreeze} {
		alertShadowTestSetEarningsNotEvaluated(&result, id, risk.EarningsReasonNonIssuerSecurity, "TERMQ", "IDXQ")
	}
	batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
	if batch.Covered {
		t.Fatal("a mixed-authority exempt set was accepted under the security-type reason alone")
	}

	for _, id := range []string{risk.RuleCatalystCoverage, risk.RuleOverwriteEarnings, risk.RuleEarningsSizeFreeze} {
		alertShadowTestSetEarningsNotEvaluated(&result, id, risk.EarningsReasonNotApplicable, "TERMQ", "IDXQ")
	}
	mixed := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
	if !mixed.Covered {
		t.Fatalf("a correctly-labelled mixed exempt set was refused: reason=%q", mixed.Reason)
	}
}

// TestPerRuleCoverageScopesEarningsGapToItsOwnRule is the issue #19 layer-2
// outcome: a name in the post-report earnings gap un-covers only rules 6-8,
// holds only their episodes, and leaves every other rule's alert lifecycle —
// including recovery — fully functional.
func TestPerRuleCoverageScopesEarningsGapToItsOwnRule(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }

	// Two breaches open: rule 1 (exposure) and rule 6 (catalyst coverage).
	both := alertShadowTestRulebook(base, risk.RuleStatusWatch)
	for i := range both.Rules {
		if both.Rules[i].ID == risk.RuleCatalystCoverage {
			both.Rules[i].Status = risk.RuleStatusWatch
		}
	}
	opened, err := composer.ObserveRulebook(t.Context(), scope, both)
	if err != nil || len(opened.Candidates) != 2 {
		t.Fatalf("expected two open episodes: %+v err=%v", opened, err)
	}

	// Next cycle: the earnings gap appears. Rule 1 is clean and current; rule 6
	// cannot be assessed (unknown row). Rule 1 must recover, rule 6 must hold.
	now = base.Add(time.Minute + time.Second)
	gap := alertShadowTestRulebook(base.Add(time.Minute), risk.RuleStatusPass)
	for i := range gap.Rules {
		if gap.Rules[i].ID == risk.RuleCatalystCoverage {
			gap.Rules[i].Status = risk.RuleStatusUnknown
			gap.Rules[i].Reason = "earnings_unknown"
		}
	}
	snapshot, err := composer.ObserveRulebook(t.Context(), scope, gap)
	if err != nil || len(snapshot.Candidates) != 2 {
		t.Fatalf("expected both episodes represented: %+v err=%v", snapshot, err)
	}
	states := map[rpc.AlertPresentationCode]rpc.AlertEpisodeState{}
	for _, candidate := range snapshot.Candidates {
		states[candidate.PresentationCode] = candidate.State
	}
	if states[rpc.AlertPresentationRulebookSingleNameExposure] != rpc.AlertEpisodeRecovered {
		t.Fatalf("clean covered rule did not recover during a sibling's earnings gap: %+v", states)
	}
	if states[rpc.AlertPresentationRulebookCatalystCoverage] != rpc.AlertEpisodeOpen {
		t.Fatalf("uncovered rule's episode did not hold through the gap: %+v", states)
	}

	// Gap closes: rule 6 evaluates clean and current — now it recovers too.
	now = base.Add(2*time.Minute + time.Second)
	clean, err := composer.ObserveRulebook(t.Context(), scope, alertShadowTestRulebook(base.Add(2*time.Minute), risk.RuleStatusPass))
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range clean.Candidates {
		if candidate.State != rpc.AlertEpisodeRecovered {
			t.Fatalf("episode did not recover after the gap closed: %+v", clean.Candidates)
		}
	}
}

func TestRegimeStaleUncoversOnlyRegimeConditionalRules(t *testing.T) {
	base := time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC)
	result := alertShadowTestRulebook(base, risk.RuleStatusPass)
	result.Status = "degraded"
	for i := range result.InputHealth {
		if result.InputHealth[i].Source == "regime_stage" {
			result.InputHealth[i].Status = rpc.SourceStatusStale
		}
	}
	batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
	if !batch.Covered || batch.Reason != alertShadowReasonRuleGapDisclosed {
		t.Fatalf("carried regime thresholds were not disclosed as per-rule gaps: %+v", batch)
	}
	expected := []string{risk.RuleCashSellOnly, risk.RuleExtrinsicBudget, risk.RuleHedgeIntegrity}
	if !slices.Equal(batch.UncoveredRules, expected) {
		t.Fatalf("regime-stale gap set = %v, want %v", batch.UncoveredRules, expected)
	}
}
