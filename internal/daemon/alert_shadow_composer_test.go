package daemon

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestAlertShadowComposerStressNormalRecoveryReopenAndMetrics(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	now := base.Add(2 * time.Second)
	composer.now = func() time.Time { return now }
	relevant := true

	nearMiss := alertShadowTestStress(base, risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "near-miss")
	normal, err := composer.ObserveStress(t.Context(), scope, nearMiss)
	if err != nil {
		t.Fatal(err)
	}
	if normal.CurrentState != rpc.AlertSnapshotUnknown || len(normal.Candidates) != 0 {
		t.Fatalf("near miss manufactured a global clear or candidate: %+v", normal)
	}
	assertAlertShadowCoverage(t, normal.Coverage, []rpc.AlertSource{rpc.AlertSourceStress})

	now = base.Add(time.Minute + 2*time.Second)
	stress := alertShadowTestStress(base.Add(time.Minute), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "stress")
	opened, err := composer.ObserveStress(t.Context(), scope, stress)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Candidates) != 1 {
		t.Fatalf("stress candidates=%+v", opened.Candidates)
	}
	opening := opened.Candidates[0]
	if opening.Source != rpc.AlertSourceStress || opening.Kind != rpc.AlertKindPortfolioRisk ||
		opening.State != rpc.AlertEpisodeOpen || opening.Severity != rpc.AlertSeverityWatch ||
		opening.PresentationCode != rpc.AlertPresentationPortfolioStress || opening.Destination != rpc.AlertDestinationAlerts {
		t.Fatalf("unexpected Canary candidate: %+v", opening)
	}

	duplicate, err := composer.ObserveStress(t.Context(), scope, stress)
	if err != nil {
		t.Fatal(err)
	}
	if len(duplicate.Candidates) != 1 || duplicate.Candidates[0].OccurrenceKey != opening.OccurrenceKey {
		t.Fatalf("duplicate changed occurrence: %+v", duplicate.Candidates)
	}

	now = base.Add(2*time.Minute + 2*time.Second)
	repeatedStress := alertShadowTestStress(base.Add(2*time.Minute), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "stress")
	repeated, err := composer.ObserveStress(t.Context(), scope, repeatedStress)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Candidates[0].OccurrenceKey != opening.OccurrenceKey {
		t.Fatal("semantic replay rotated occurrence")
	}

	now = base.Add(3*time.Minute + 2*time.Second)
	recovery := alertShadowTestStress(base.Add(3*time.Minute), risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "recovery")
	recovered, err := composer.ObserveStress(t.Context(), scope, recovery)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Candidates) != 1 || recovered.Candidates[0].State != rpc.AlertEpisodeRecovered || recovered.Candidates[0].Severity != rpc.AlertSeverityObserve ||
		recovered.Candidates[0].EvidenceFingerprint != recovery.Fingerprint.Key ||
		recovered.Candidates[0].OccurrenceKey != opening.OccurrenceKey {
		t.Fatalf("authoritative recovery invalid: %+v", recovered.Candidates)
	}
	if recovered.CurrentState != rpc.AlertSnapshotUnknown {
		t.Fatalf("global partial coverage reported %q, want unknown", recovered.CurrentState)
	}
	restartDuringRecovery := newAlertShadowComposer(registry)
	restartProjection, ok, err := restartDuringRecovery.Snapshot(scope)
	if err != nil || !ok || len(restartProjection.Candidates) != 0 || restartProjection.CurrentState != rpc.AlertSnapshotUnknown ||
		restartProjection.Coverage.State != rpc.AlertCoverageUnavailable || restartProjection.Coverage.Freshness != rpc.AlertCoverageUnknown ||
		len(restartProjection.Coverage.CoveredSources) != 0 {
		t.Fatalf("restart replayed one-shot recovery: %+v ok=%v err=%v", restartProjection, ok, err)
	}

	now = base.Add(4*time.Minute + 2*time.Second)
	reopenInput := alertShadowTestStress(base.Add(4*time.Minute), risk.SeverityAct, "defend", &relevant, rpc.SourceStatusOK, "reopen")
	reopened, err := composer.ObserveStress(t.Context(), scope, reopenInput)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Candidates) != 1 || reopened.Candidates[0].State != rpc.AlertEpisodeOpen ||
		reopened.Candidates[0].OccurrenceKey == opening.OccurrenceKey {
		t.Fatalf("reopen did not re-arm occurrence: %+v", reopened.Candidates)
	}

	status := composer.Status(scope)
	if len(status.ExpectedSources) != 9 || status.HumanPrecision != alertShadowHumanLabelUnlabelled || status.HumanRecall != alertShadowHumanLabelUnlabelled {
		t.Fatalf("status contract incomplete: %+v", status)
	}
	canary := alertShadowTestSourceStatus(t, status, rpc.AlertSourceStress)
	if canary.Measurements.EpisodesOpened != 1 || canary.Measurements.EpisodesRecovered != 1 || canary.Measurements.EpisodesReopened != 1 {
		t.Fatalf("Canary churn metrics=%+v", canary.Measurements)
	}
	if canary.Measurements.DuplicateInputs != 1 || canary.Measurements.DuplicateCandidates != 0 || canary.Measurements.RepeatedActive == 0 {
		t.Fatalf("Canary duplicate metrics=%+v", canary.Measurements)
	}
	if canary.Measurements.ActiveEvaluations == 0 || canary.Measurements.TimeToObserveSamples != 5 ||
		canary.Measurements.TimeToObserveTotal != 10*time.Second || canary.Measurements.TimeToObserveMax != 2*time.Second {
		t.Fatalf("Canary prevalence/latency metrics=%+v", canary.Measurements)
	}
	regime := alertShadowTestSourceStatus(t, status, rpc.AlertSourceRegime)
	if regime.Status != alertShadowStatusNotObserved || regime.Reason != alertShadowReasonNotObserved || regime.Measurements.Evaluations != 0 || regime.Measurements.CoverageFailures != 0 {
		t.Fatalf("Regime unavailable status=%+v", regime)
	}
	orderIntegrity := alertShadowTestSourceStatus(t, status, rpc.AlertSourceOrderIntegrity)
	if orderIntegrity.Status != alertShadowStatusNotObserved || orderIntegrity.Reason != alertShadowReasonNotObserved {
		t.Fatalf("order-integrity status=%+v", orderIntegrity)
	}
}

func TestAlertShadowComposerRulebookCurrentNegativeAndDegradedHold(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }

	breach := alertShadowTestRulebook(base, risk.RuleStatusWatch)
	opened, err := composer.ObserveRulebook(t.Context(), scope, breach)
	if err != nil || len(opened.Candidates) != 1 || opened.Candidates[0].Source != rpc.AlertSourceRulebook ||
		opened.Candidates[0].State != rpc.AlertEpisodeOpen || opened.Candidates[0].Destination != rpc.AlertDestinationMonitor {
		t.Fatalf("rulebook open=%+v err=%v", opened, err)
	}

	now = base.Add(time.Minute + time.Second)
	degraded := alertShadowTestRulebook(base.Add(time.Minute), risk.RuleStatusPass)
	degraded.Status = "degraded"
	degraded.InputHealth[0].Status = rpc.SourceStatusUnknown
	held, err := composer.ObserveRulebook(t.Context(), scope, degraded)
	if err != nil || len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen || held.Candidates[0].EvidenceHealth == rpc.AlertEvidenceCurrent {
		t.Fatalf("degraded negative cleared rulebook episode: %+v err=%v", held, err)
	}

	now = base.Add(2*time.Minute + time.Second)
	clear := alertShadowTestRulebook(base.Add(2*time.Minute), risk.RuleStatusPass)
	recovered, err := composer.ObserveRulebook(t.Context(), scope, clear)
	if err != nil || len(recovered.Candidates) != 1 || recovered.Candidates[0].State != rpc.AlertEpisodeRecovered {
		t.Fatalf("current rulebook negative did not recover: %+v err=%v", recovered, err)
	}
}

func TestAlertShadowComposerRulebookMissingPnLHoldsUntilCurrentRestored(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }

	active := alertShadowTestRulebook(base, risk.RuleStatusWatch)
	opened, err := composer.ObserveRulebook(t.Context(), scope, active)
	if err != nil || len(opened.Candidates) != 1 || opened.Candidates[0].State != rpc.AlertEpisodeOpen {
		t.Fatalf("rulebook open=%+v err=%v", opened, err)
	}

	now = base.Add(time.Minute + time.Second)
	missing := alertShadowTestRulebook(base.Add(time.Minute), risk.RuleStatusPass)
	alertShadowTestRulebookPnLUnavailable(&missing)
	held, err := composer.ObserveRulebook(t.Context(), scope, missing)
	if err != nil || len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen ||
		held.Candidates[0].EvidenceHealth != rpc.AlertEvidencePartial {
		t.Fatalf("missing P&L cleared or corrupted the active episode: %+v err=%v", held, err)
	}

	now = base.Add(2*time.Minute + time.Second)
	stillIncomplete := alertShadowTestRulebook(base.Add(2*time.Minute), risk.RuleStatusPass)
	alertShadowTestRulebookPnLUnavailable(&stillIncomplete)
	for i := range stillIncomplete.Rules {
		if stillIncomplete.Rules[i].ID == risk.RuleGreenDayAction {
			stillIncomplete.Rules[i].Status = risk.RuleStatusPass
			stillIncomplete.Rules[i].Reason = ""
		}
	}
	held, err = composer.ObserveRulebook(t.Context(), scope, stillIncomplete)
	if err != nil || len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen {
		t.Fatalf("an uncovered P&L result recovered the active episode: %+v err=%v", held, err)
	}

	now = base.Add(3*time.Minute + time.Second)
	restored := alertShadowTestRulebook(base.Add(3*time.Minute), risk.RuleStatusPass)
	recovered, err := composer.ObserveRulebook(t.Context(), scope, restored)
	if err != nil || len(recovered.Candidates) != 1 || recovered.Candidates[0].State != rpc.AlertEpisodeRecovered {
		t.Fatalf("current restored P&L did not recover the active episode: %+v err=%v", recovered, err)
	}
}

func TestAlertShadowRulebookRequiresCanonicalUniverseAndReasons(t *testing.T) {
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	assertUncovered := func(t *testing.T, result rpc.RulesResult) {
		t.Helper()
		batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
		if batch.Covered || batch.Status == alertShadowStatusCurrent {
			t.Fatalf("noncanonical Rulebook evidence was trusted: %+v", batch)
		}
	}

	t.Run("missing rule", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		result.Rules = result.Rules[:len(result.Rules)-1]
		assertUncovered(t, result)
	})
	t.Run("wrong rule number", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		result.Rules[0].Number++
		assertUncovered(t, result)
	})
	t.Run("extra rule", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		result.Rules = append(result.Rules, risk.RuleRow{ID: "future_rule", Number: 15, Status: risk.RuleStatusPass})
		assertUncovered(t, result)
	})
	t.Run("missing health source", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		result.InputHealth = result.InputHealth[:len(result.InputHealth)-1]
		assertUncovered(t, result)
	})
	t.Run("extra health source", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		result.InputHealth = append(result.InputHealth, rpc.SourceHealth{Source: "future_source", Status: rpc.SourceStatusOK, AsOf: base})
		assertUncovered(t, result)
	})
	t.Run("unknown row becomes a disclosed per-rule gap", func(t *testing.T) {
		// Operator decision 2026-08-03: an honest unknown row un-covers its own
		// rule and holds its own episode; it no longer silences the source.
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		result.Rules[0].Status = risk.RuleStatusUnknown
		batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
		if !batch.Covered || batch.Status != alertShadowStatusCurrent || batch.Reason != alertShadowReasonRuleGapDisclosed {
			t.Fatalf("unknown row was not disclosed as a per-rule gap: %+v", batch)
		}
		if len(batch.UncoveredRules) != 1 || batch.UncoveredRules[0] != result.Rules[0].ID || len(batch.NegativeHold) != 1 {
			t.Fatalf("gap disclosure is wrong: %+v", batch.UncoveredRules)
		}
	})
	t.Run("unapproved not evaluated reason", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		result.Rules[0].Status = risk.RuleStatusNotEvaluated
		result.Rules[0].Reason = "future_reason"
		assertUncovered(t, result)
	})
	t.Run("P&L unavailable discloses account-dependent gaps", func(t *testing.T) {
		// Operator decision 2026-08-03: rule 11 is coverage-neutral (its status
		// ceiling is info, so it can never alert), and a degraded account
		// un-covers exactly the rules that rest on it — disclosed, not fatal.
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestRulebookPnLUnavailable(&result)
		batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
		if !batch.Covered || batch.Status != alertShadowStatusCurrent || batch.Reason != alertShadowReasonRuleGapDisclosed {
			t.Fatalf("canonical missing-P&L result was not retained as disclosed gaps: %+v", batch)
		}
		if len(batch.UncoveredRules) != 13 || len(batch.NegativeHold) != 13 ||
			slices.Contains(batch.UncoveredRules, risk.RuleGreenDayAction) {
			t.Fatalf("account-degraded gap set is wrong or includes the neutral rule: %+v", batch.UncoveredRules)
		}
	})
	t.Run("off-session P&L not due is coverage-neutral", func(t *testing.T) {
		// The routine off-session state: account ok with P&L legitimately not
		// due, rule 11 not_evaluated/pnl_unavailable. This used to fabricate
		// candidate_invalid — the flapping reason in issue #19's diagnosis.
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		for i := range result.Rules {
			if result.Rules[i].ID == risk.RuleGreenDayAction {
				result.Rules[i].Status = risk.RuleStatusNotEvaluated
				result.Rules[i].Reason = risk.RuleReasonPnLUnavailable
			}
		}
		batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
		if !batch.Covered || batch.Status != alertShadowStatusCurrent || batch.Reason != alertShadowReasonCurrent || len(batch.UncoveredRules) != 0 {
			t.Fatalf("neutral rule 11 blocked coverage: %+v", batch)
		}
	})

	for _, tc := range []struct {
		name          string
		envelope      string
		accountHealth string
	}{
		{name: "degraded envelope with healthy inputs", envelope: "degraded", accountHealth: rpc.SourceStatusOK},
		{name: "healthy envelope with degraded account", envelope: "ok", accountHealth: rpc.SourceStatusDegraded},
	} {
		t.Run("reject inconsistent snapshot "+tc.name, func(t *testing.T) {
			// The real producer stamps "degraded" exactly when an input health
			// is not ok. Either direction of disagreement is a snapshot the
			// producer cannot emit — the deliberately uncovered unbound clone
			// or a forgery — and must stay unable to claim any coverage.
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			result.Status = tc.envelope
			for i := range result.InputHealth {
				if result.InputHealth[i].Source == "account" {
					result.InputHealth[i].Status = tc.accountHealth
				}
			}
			batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
			if batch.Covered || batch.Status != alertShadowStatusError || batch.EvidenceHealth != rpc.AlertEvidenceError ||
				batch.Reason != alertShadowReasonSnapshotInconsistent {
				t.Fatalf("inconsistent snapshot was trusted: %+v", batch)
			}
		})
	}

	for _, tc := range []struct {
		id     string
		reason string
	}{
		{risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting},
		{risk.RuleOverwriteEarnings, risk.EarningsReasonTerminalNonReporting},
		{risk.RuleEarningsSizeFreeze, risk.EarningsReasonTerminalNonReporting},
		{risk.RuleCatalystCoverage, risk.EarningsReasonBrokerNonIssuer},
		{risk.RuleOverwriteEarnings, risk.EarningsReasonBrokerNonIssuer},
		{risk.RuleEarningsSizeFreeze, risk.EarningsReasonBrokerNonIssuer},
		{risk.RuleCatalystCoverage, risk.EarningsReasonNotApplicable},
		{risk.RuleOverwriteEarnings, risk.EarningsReasonNotApplicable},
		{risk.RuleEarningsSizeFreeze, risk.EarningsReasonNotApplicable},
	} {
		t.Run("reject reason without authority "+tc.id+" "+tc.reason, func(t *testing.T) {
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			alertShadowTestSetEarningsNotEvaluated(&result, tc.id, tc.reason, "SYNTH1")
			assertUncovered(t, result)
		})
	}

	for _, tc := range []struct {
		id     string
		reason string
	}{
		{risk.RuleRedOnGreen, risk.RuleReasonOffSession},
		{risk.RuleWinnerTrim, risk.RuleReasonOffSession},
		{risk.RuleHedgeIntegrity, risk.RuleReasonNoLongBook},
	} {
		t.Run("approved "+tc.id, func(t *testing.T) {
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			for i := range result.Rules {
				if result.Rules[i].ID == tc.id {
					result.Rules[i].Status = risk.RuleStatusNotEvaluated
					result.Rules[i].Reason = tc.reason
				}
			}
			batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
			if !batch.Covered || batch.Status != alertShadowStatusCurrent {
				t.Fatalf("approved not-evaluated reason was not trusted: %+v", batch)
			}
		})
	}
}

func TestAlertShadowRulebookNotEvaluatedRequiresCurrentMatchingEarningsAuthority(t *testing.T) {
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	assertCovered := func(t *testing.T, result rpc.RulesResult) {
		t.Helper()
		batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
		if !batch.Covered || batch.Status != alertShadowStatusCurrent {
			t.Fatalf("current typed earnings authority was not trusted: %+v", batch)
		}
	}
	assertUncovered := func(t *testing.T, result rpc.RulesResult) {
		t.Helper()
		batch := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(time.Second))
		if batch.Covered || batch.Status == alertShadowStatusCurrent || batch.Reason != alertShadowReasonCandidateInvalid {
			t.Fatalf("invalid typed earnings authority was trusted: %+v", batch)
		}
	}

	for _, id := range []string{risk.RuleCatalystCoverage, risk.RuleOverwriteEarnings, risk.RuleEarningsSizeFreeze} {
		t.Run("valid terminal "+id, func(t *testing.T) {
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			alertShadowTestSetEarningsNotEvaluated(&result, id, risk.EarningsReasonTerminalNonReporting, "TERM1")
			result.Earnings = []rpc.EarningsInfo{alertShadowTestTerminalEarnings("TERM1", base)}
			assertCovered(t, result)
		})
		t.Run("valid broker "+id, func(t *testing.T) {
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			alertShadowTestSetEarningsNotEvaluated(&result, id, risk.EarningsReasonBrokerNonIssuer, "FUND1")
			result.Earnings = []rpc.EarningsInfo{alertShadowTestBrokerEarnings("FUND1", base)}
			assertCovered(t, result)
		})
		t.Run("valid retained broker "+id, func(t *testing.T) {
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			alertShadowTestSetEarningsNotEvaluated(&result, id, risk.EarningsReasonBrokerNonIssuer, "FUND1")
			result.Earnings = []rpc.EarningsInfo{alertShadowTestRetainedBrokerEarnings("FUND1", base)}
			assertCovered(t, result)
		})
		t.Run("valid mixed "+id, func(t *testing.T) {
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			alertShadowTestSetEarningsNotEvaluated(&result, id, risk.EarningsReasonNotApplicable, "TERM1", "FUND1")
			result.Earnings = []rpc.EarningsInfo{
				alertShadowTestBrokerEarnings("FUND1", base),
				alertShadowTestTerminalEarnings("TERM1", base),
			}
			assertCovered(t, result)
		})
	}

	t.Run("empty exempt list", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting)
		result.Earnings = []rpc.EarningsInfo{alertShadowTestTerminalEarnings("TERM1", base)}
		assertUncovered(t, result)
	})
	t.Run("wrong symbol", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1")
		result.Earnings = []rpc.EarningsInfo{alertShadowTestTerminalEarnings("OTHER1", base)}
		assertUncovered(t, result)
	})
	t.Run("stale terminal", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1")
		info := alertShadowTestTerminalEarnings("TERM1", base)
		info.Stale = true
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("expired terminal", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1")
		info := alertShadowTestTerminalEarnings("TERM1", base)
		info.Terminal.RevalidateAfter = base
		info.Terminal.AuthorityBinding = rpc.BuildEarningsTerminalAuthorityBinding(info.Symbol, *info.Terminal)
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("future terminal review", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1")
		info := alertShadowTestTerminalEarnings("TERM1", base)
		info.Terminal.AuthorityReviewedAt = base.Add(time.Minute)
		info.Terminal.AuthorityBinding = rpc.BuildEarningsTerminalAuthorityBinding(info.Symbol, *info.Terminal)
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("missing terminal authority binding", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1")
		info := alertShadowTestTerminalEarnings("TERM1", base)
		info.Terminal.AuthorityBinding = ""
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("terminal binding is symbol specific", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1")
		info := alertShadowTestTerminalEarnings("OTHER1", base)
		info.Symbol = "TERM1"
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("terminal binding rejects exact contract substitution", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1")
		info := alertShadowTestTerminalEarnings("TERM1", base)
		info.Terminal.ContractConID++
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("duplicate terminal contract across symbols", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1", "TERM2")
		first := alertShadowTestTerminalEarnings("TERM1", base)
		second := alertShadowTestTerminalEarnings("TERM2", base)
		result.Earnings = []rpc.EarningsInfo{first, second}
		assertUncovered(t, result)
	})
	t.Run("malformed terminal authority", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleCatalystCoverage, risk.EarningsReasonTerminalNonReporting, "TERM1")
		info := alertShadowTestTerminalEarnings("TERM1", base)
		info.Terminal.Classification = "future_classification"
		info.Terminal.AuthorityFingerprint = "private-free-text"
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("stale broker proof", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleOverwriteEarnings, risk.EarningsReasonBrokerNonIssuer, "FUND1")
		info := alertShadowTestBrokerEarnings("FUND1", base)
		info.Stale = true
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("future broker proof", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleOverwriteEarnings, risk.EarningsReasonBrokerNonIssuer, "FUND1")
		info := alertShadowTestBrokerEarnings("FUND1", base)
		info.Identity.ProofObservedAt = base.Add(time.Minute)
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	for _, tc := range []struct {
		name   string
		mutate func(*rpc.EarningsIdentityInfo)
	}{
		{name: "missing revision", mutate: func(info *rpc.EarningsIdentityInfo) { info.AuthorityRevision = 0 }},
		{name: "bad fingerprint", mutate: func(info *rpc.EarningsIdentityInfo) { info.AuthorityFingerprint = "private-free-text" }},
		{name: "missing observation", mutate: func(info *rpc.EarningsIdentityInfo) { info.ObservationID = "" }},
		{name: "invalid observation", mutate: func(info *rpc.EarningsIdentityInfo) { info.ObservationID = "opaque-free-text" }},
		{name: "wrong valid observation", mutate: func(info *rpc.EarningsIdentityInfo) {
			info.ObservationID = "oid:" + opaqueIdentity("alert-rulebook-test-receipt", "OTHER1")
		}},
		{name: "missing authority binding", mutate: func(info *rpc.EarningsIdentityInfo) { info.AuthorityBinding = "" }},
		{name: "wrong proof outcome", mutate: func(info *rpc.EarningsIdentityInfo) { info.ProofOutcome = earningsIdentityIssuer }},
		{name: "issuer outcome", mutate: func(info *rpc.EarningsIdentityInfo) { info.Outcome = earningsIdentityIssuer }},
	} {
		t.Run("malformed broker "+tc.name, func(t *testing.T) {
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleOverwriteEarnings, risk.EarningsReasonBrokerNonIssuer, "FUND1")
			info := alertShadowTestBrokerEarnings("FUND1", base)
			tc.mutate(info.Identity)
			result.Earnings = []rpc.EarningsInfo{info}
			assertUncovered(t, result)
		})
	}

	for _, tc := range []struct {
		name   string
		mutate func(*rpc.EarningsInfo)
	}{
		{name: "entitlement failure", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.LastFailure.Code = rpc.SourceFailureNotEntitled
		}},
		{name: "metadata stage", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.LastFailure.Stage = rpc.SourceFailureStageWSHMetadata
		}},
		{name: "authority persist stage", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.LastFailure.Code = rpc.SourceFailureAuthorityWriteFailed
			info.Identity.LastFailure.Stage = rpc.SourceFailureStageAuthorityPersist
		}},
		{name: "nonretryable failure", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.LastFailure.Retryable = false
		}},
		{name: "missing failure", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.LastFailure = nil
		}},
		{name: "future attempt", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.AttemptedAt = base.Add(time.Minute)
		}},
		{name: "future failure", mutate: func(info *rpc.EarningsInfo) {
			failedAt := base.Add(time.Minute)
			nextAttempt := failedAt.Add(earningsContractResolutionRetry)
			info.Identity.LastFailure.FailedAt = failedAt
			info.Identity.NextAttempt = &nextAttempt
		}},
		{name: "proof after retained attempt", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.ProofObservedAt = base.Add(-30 * time.Second)
			info.Identity.AuthorityBinding = rpc.BuildEarningsIdentityAuthorityBinding(info.Symbol, *info.Identity)
		}},
		{name: "failure before attempt", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.AttemptedAt = base.Add(-30 * time.Second)
		}},
		{name: "missing next attempt", mutate: func(info *rpc.EarningsInfo) {
			info.Identity.NextAttempt = nil
		}},
		{name: "wrong retry interval", mutate: func(info *rpc.EarningsInfo) {
			nextAttempt := info.Identity.LastFailure.FailedAt.Add(earningsContractResolutionRetry + time.Second)
			info.Identity.NextAttempt = &nextAttempt
		}},
	} {
		t.Run("reject retained broker "+tc.name, func(t *testing.T) {
			result := alertShadowTestRulebook(base, risk.RuleStatusPass)
			alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleOverwriteEarnings, risk.EarningsReasonBrokerNonIssuer, "FUND1")
			info := alertShadowTestRetainedBrokerEarnings("FUND1", base)
			tc.mutate(&info)
			result.Earnings = []rpc.EarningsInfo{info}
			assertUncovered(t, result)
		})
	}

	t.Run("broker binding is symbol specific", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleOverwriteEarnings, risk.EarningsReasonBrokerNonIssuer, "FUND1")
		info := alertShadowTestBrokerEarnings("OTHER1", base)
		info.Symbol = "FUND1"
		result.Earnings = []rpc.EarningsInfo{info}
		assertUncovered(t, result)
	})
	t.Run("duplicate broker receipt across symbols", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleOverwriteEarnings, risk.EarningsReasonBrokerNonIssuer, "FUND1", "FUND2")
		first := alertShadowTestBrokerEarnings("FUND1", base)
		second := alertShadowTestBrokerEarnings("FUND2", base)
		second.Identity.ObservationID = first.Identity.ObservationID
		second.Identity.AuthorityBinding = rpc.BuildEarningsIdentityAuthorityBinding(second.Symbol, *second.Identity)
		result.Earnings = []rpc.EarningsInfo{first, second}
		assertUncovered(t, result)
	})
	t.Run("single authority cannot claim mixed reason", func(t *testing.T) {
		result := alertShadowTestRulebook(base, risk.RuleStatusPass)
		alertShadowTestSetEarningsNotEvaluated(&result, risk.RuleEarningsSizeFreeze, risk.EarningsReasonNotApplicable, "TERM1")
		result.Earnings = []rpc.EarningsInfo{alertShadowTestTerminalEarnings("TERM1", base)}
		assertUncovered(t, result)
	})
}

func TestAlertShadowRulebookReagesCurrentInputsAtObservation(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	result := alertShadowTestRulebook(base, risk.RuleStatusPass)
	for i := range result.InputHealth {
		if result.InputHealth[i].Source == "positions" {
			result.InputHealth[i].MaxAgeSeconds = 300
		}
	}

	stillCurrent := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(299*time.Second))
	if !stillCurrent.Covered || stillCurrent.Status != alertShadowStatusCurrent {
		t.Fatalf("input inside its freshness budget was not trusted: %+v", stillCurrent)
	}
	wantFreshUntil := base.Add(300 * time.Second)
	if !stillCurrent.FreshUntil.Equal(wantFreshUntil) {
		t.Fatalf("fresh until = %s, want source expiry %s", stillCurrent.FreshUntil, wantFreshUntil)
	}

	stale := alertShadowMapRulebook(alertShadowTestBrokerScope(t), result, base.Add(301*time.Second))
	if stale.Covered || stale.EvidenceHealth != rpc.AlertEvidenceStale || stale.Reason != alertShadowReasonSourceHealthStale {
		t.Fatalf("expired input did not fail closed as stale: %+v", stale)
	}
	if !stale.FreshUntil.Equal(wantFreshUntil) {
		t.Fatalf("stale fresh until = %s, want source expiry %s", stale.FreshUntil, wantFreshUntil)
	}
}

func TestAlertShadowComposerOrderIntegrityRequiresConfirmationAndCurrentNegative(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }
	order := alertShadowTestMismatchedOrder(base, scope)

	first, err := composer.ObserveOrderIntegrity(t.Context(), scope, orderIntegrityEvaluation{
		AsOf: base, EvidenceAsOf: base, Status: orderIntegrityHealthCurrent, Orders: []rpc.OrderView{order},
	})
	if err != nil || len(first.Candidates) != 0 {
		t.Fatalf("first mismatch pass alerted: %+v err=%v", first, err)
	}
	status := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceOrderIntegrity)
	if status.Covered || status.Reason != alertShadowReasonConfirmationPending {
		t.Fatalf("first mismatch status=%+v", status)
	}

	now = base.Add(time.Minute + time.Second)
	order.BrokerTruthAsOf = base.Add(time.Minute)
	second, err := composer.ObserveOrderIntegrity(t.Context(), scope, orderIntegrityEvaluation{
		AsOf: base.Add(time.Minute), EvidenceAsOf: base.Add(time.Minute), Status: orderIntegrityHealthCurrent, Orders: []rpc.OrderView{order},
	})
	if err != nil || len(second.Candidates) != 1 || second.Candidates[0].State != rpc.AlertEpisodeOpen || second.Candidates[0].Severity != rpc.AlertSeverityUrgent {
		t.Fatalf("second mismatch pass=%+v err=%v", second, err)
	}

	now = base.Add(2*time.Minute + time.Second)
	held, err := composer.ObserveOrderIntegrity(t.Context(), scope, orderIntegrityEvaluation{
		AsOf: base.Add(2 * time.Minute), EvidenceAsOf: base, Status: orderIntegrityHealthStale, Orders: []rpc.OrderView{},
	})
	if err != nil || len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen {
		t.Fatalf("stale negative cleared order episode: %+v err=%v", held, err)
	}

	now = base.Add(3*time.Minute + time.Second)
	recovered, err := composer.ObserveOrderIntegrity(t.Context(), scope, orderIntegrityEvaluation{
		AsOf: base.Add(3 * time.Minute), EvidenceAsOf: base.Add(3 * time.Minute), Status: orderIntegrityHealthCurrent, Orders: []rpc.OrderView{},
	})
	if err != nil || len(recovered.Candidates) != 1 || recovered.Candidates[0].State != rpc.AlertEpisodeRecovered {
		t.Fatalf("trusted order negative did not recover: %+v err=%v", recovered, err)
	}
}

func TestAlertShadowComposerRegimeEscalatesAndDataQualityCannotWarnOrRecover(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }

	early := alertShadowTestRegime(base, rpc.LifecycleEarlyWarning, "ready")
	opened, err := composer.ObserveRegime(t.Context(), scope, early)
	if err != nil || len(opened.Candidates) != 1 || opened.Candidates[0].State != rpc.AlertEpisodeOpen ||
		opened.Candidates[0].Severity != rpc.AlertSeverityWatch || opened.Candidates[0].Source != rpc.AlertSourceRegime {
		t.Fatalf("early warning open=%+v err=%v", opened, err)
	}
	openingOccurrence := opened.Candidates[0].OccurrenceKey
	evaluations := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceRegime).Measurements.Evaluations
	now = base.Add(5 * time.Second)
	if _, err := composer.ObserveRegime(t.Context(), scope, early); err != nil {
		t.Fatal(err)
	}
	if got := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceRegime).Measurements.Evaluations; got != evaluations {
		t.Fatalf("Regime hot-poll throttle evaluations=%d want %d", got, evaluations)
	}

	now = base.Add(time.Minute + time.Second)
	confirmed := alertShadowTestRegime(base.Add(time.Minute), rpc.LifecycleConfirmedStress, "ready")
	escalated, err := composer.ObserveRegime(t.Context(), scope, confirmed)
	if err != nil || len(escalated.Candidates) != 1 || escalated.Candidates[0].State != rpc.AlertEpisodeEscalated ||
		escalated.Candidates[0].Severity != rpc.AlertSeverityAct || escalated.Candidates[0].OccurrenceKey == openingOccurrence {
		t.Fatalf("confirmed escalation=%+v err=%v", escalated, err)
	}
	escalatedOccurrence := escalated.Candidates[0].OccurrenceKey

	now = base.Add(2*time.Minute + time.Second)
	broken := alertShadowTestRegime(base.Add(2*time.Minute), rpc.LifecycleDataQuality, "blocked")
	broken.SourceHealth[0].Status = rpc.SourceStatusUnknown
	broken.Fingerprint = rpc.BuildRegimeFingerprint(&broken)
	held, err := composer.ObserveRegime(t.Context(), scope, broken)
	if err != nil || len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeEscalated ||
		held.Candidates[0].EvidenceHealth == rpc.AlertEvidenceCurrent || held.Candidates[0].OccurrenceKey != escalatedOccurrence {
		t.Fatalf("data-quality observation rewrote Regime episode: %+v err=%v", held, err)
	}
	regimeStatus := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceRegime)
	if regimeStatus.Covered || regimeStatus.Reason != alertShadowReasonLifecycleDataQuality {
		t.Fatalf("data-quality source status=%+v", regimeStatus)
	}

	now = base.Add(3*time.Minute + time.Second)
	quiet := alertShadowTestRegime(base.Add(3*time.Minute), rpc.LifecycleQuiet, "ready")
	recovered, err := composer.ObserveRegime(t.Context(), scope, quiet)
	if err != nil || len(recovered.Candidates) != 1 || recovered.Candidates[0].State != rpc.AlertEpisodeRecovered {
		t.Fatalf("current quiet did not recover Regime: %+v err=%v", recovered, err)
	}

	now = base.Add(4*time.Minute + time.Second)
	brokenEarly := alertShadowTestRegime(base.Add(4*time.Minute), rpc.LifecycleEarlyWarning, "degraded")
	brokenEarly.SourceHealth[0].Status = rpc.SourceStatusPartial
	brokenEarly.Fingerprint = rpc.BuildRegimeFingerprint(&brokenEarly)
	undefined, err := composer.ObserveRegime(t.Context(), scope, brokenEarly)
	if err != nil || len(undefined.Candidates) != 0 {
		t.Fatalf("broken early warning created candidate: %+v err=%v", undefined, err)
	}
}

func TestAlertShadowComposerRegimeHonorsGovernorsAndRejectsStaleWarningEvidence(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 14, 30, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }

	staleCredit := alertShadowTestRegime(base, rpc.LifecycleEarlyWarning, "ready")
	for i := range staleCredit.SourceHealth {
		if staleCredit.SourceHealth[i].Source == "credit" {
			staleCredit.SourceHealth[i].Status = rpc.SourceStatusStale
			staleCredit.SourceHealth[i].RefreshState = rpc.SourceRefreshNotDue
			staleCredit.SourceHealth[i].AsOf = base.Add(-8 * 24 * time.Hour)
		}
	}
	staleCredit.Fingerprint = rpc.BuildRegimeFingerprint(&staleCredit)
	invalid, err := composer.ObserveRegime(t.Context(), scope, staleCredit)
	if err != nil || len(invalid.Candidates) != 0 {
		t.Fatalf("stale Credit not_due fabricated warning: %+v err=%v", invalid, err)
	}
	status := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceRegime)
	if status.Covered || status.Status != alertShadowStatusError {
		t.Fatalf("stale Credit validation=%+v", status)
	}

	now = base.Add(time.Minute + time.Second)
	governed := alertShadowTestRegime(base.Add(time.Minute), rpc.LifecycleConfirmedStress, "ready")
	governed.FundingStress.Thresholds = &rpc.RegimeThresholds{PendingBacktest: true}
	governed.Breadth.Thresholds = &rpc.RegimeThresholds{PendingBacktest: true}
	governed.Lifecycle = rpc.BuildRegimeLifecycle(&governed)
	governed.Fingerprint = rpc.BuildRegimeFingerprint(&governed)
	if governed.Lifecycle.Stage != rpc.LifecycleConfirmedStress || governed.Lifecycle.Severity != "watch" {
		t.Fatalf("test fixture did not exercise governed severity: %+v", governed.Lifecycle)
	}
	opened, err := composer.ObserveRegime(t.Context(), scope, governed)
	if err != nil || len(opened.Candidates) != 1 || opened.Candidates[0].Severity != rpc.AlertSeverityWatch {
		t.Fatalf("governed Regime severity rejected or overridden: %+v err=%v", opened, err)
	}

	now = base.Add(2*time.Minute + time.Second)
	overdue := alertShadowTestRegime(base.Add(2*time.Minute), rpc.LifecycleEarlyWarning, "ready")
	for i := range overdue.SourceHealth {
		if overdue.SourceHealth[i].Source == "funding" {
			overdue.SourceHealth[i].AsOf = now.Add(-time.Duration(overdue.SourceHealth[i].MaxAgeSeconds+1) * time.Second)
		}
	}
	overdue.Fingerprint = rpc.BuildRegimeFingerprint(&overdue)
	held, err := composer.ObserveRegime(t.Context(), scope, overdue)
	if err != nil || len(held.Candidates) != 1 || held.Candidates[0].EvidenceHealth == rpc.AlertEvidenceCurrent {
		t.Fatalf("overdue OK source replaced governed episode: %+v err=%v", held, err)
	}
}

func TestAlertShadowComposerProtectionAlertsOnlyOnReconciliationFacts(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }

	unprotected := alertShadowProtectionInput{AsOf: base, EvidenceAsOf: base, Status: orderIntegrityHealthCurrent, Scope: scope,
		OrderSnapshotAsOf: base, OrderSnapshotComplete: true, OrderUniverse: protectionOrderUniverseJournaledAPI,
		Summary: rpc.ProtectionCoverageSummary{AsOf: base, Status: "review", Counts: rpc.ProtectionCoverageCounts{Unprotected: 1},
			ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateUnprotected}}}}
	clear, err := composer.ObserveProtection(t.Context(), unprotected)
	if err != nil || len(clear.Candidates) != 0 || !alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceProtection).Covered {
		t.Fatalf("unprotected context became alert or uncovered: %+v err=%v", clear, err)
	}
	evaluations := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceProtection).Measurements.Evaluations
	now = base.Add(5 * time.Second)
	unprotected.AsOf, unprotected.EvidenceAsOf, unprotected.Summary.AsOf = now, now, now
	if _, err := composer.ObserveProtection(t.Context(), unprotected); err != nil {
		t.Fatal(err)
	}
	if got := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceProtection).Measurements.Evaluations; got != evaluations+1 {
		t.Fatalf("changed Protection receipt time was throttled: evaluations=%d want %d", got, evaluations+1)
	}

	now = base.Add(time.Minute + time.Second)
	issueAt := base.Add(time.Minute)
	issueOrder := rpc.ProtectionCoverageOrder{OrderRef: "opaque-order", Symbol: "AAA", Action: "SELL", OrderType: "STP", Remaining: 10,
		ReconciliationState: "position_mismatch", UpdatedAt: issueAt}
	issue := alertShadowProtectionInput{AsOf: issueAt, EvidenceAsOf: issueAt, Status: orderIntegrityHealthCurrent, Scope: scope,
		OrderSnapshotAsOf: issueAt, OrderSnapshotComplete: true, OrderUniverse: protectionOrderUniverseJournaledAPI,
		Summary: rpc.ProtectionCoverageSummary{AsOf: issueAt, Status: rpc.ProtectionCoverageStateReconcileRequired,
			Counts: rpc.ProtectionCoverageCounts{ReconcileRequired: 1}, ReconcileRequiredOrders: []rpc.ProtectionCoverageOrder{issueOrder},
			ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateReconcileRequired, Orders: []rpc.ProtectionCoverageOrder{issueOrder}}}}}
	opened, err := composer.ObserveProtection(t.Context(), issue)
	if err != nil || len(opened.Candidates) != 1 || opened.Candidates[0].Kind != rpc.AlertKindProtectionGap ||
		opened.Candidates[0].Severity != rpc.AlertSeverityWatch || opened.Candidates[0].PresentationCode != rpc.AlertPresentationProtectionReconciliationRequired {
		t.Fatalf("protection issue=%+v err=%v", opened, err)
	}

	now = base.Add(75 * time.Second)
	missingReceiptAt := base.Add(75 * time.Second)
	missingReceipt := alertShadowProtectionInput{
		AsOf: missingReceiptAt, EvidenceAsOf: missingReceiptAt, Status: orderIntegrityHealthCurrent, Scope: scope,
		Summary: rpc.ProtectionCoverageSummary{AsOf: missingReceiptAt, Status: "ok"},
	}
	heldMissing, err := composer.ObserveProtection(t.Context(), missingReceipt)
	if err != nil || len(heldMissing.Candidates) != 1 || heldMissing.Candidates[0].State != rpc.AlertEpisodeOpen {
		t.Fatalf("missing API-order receipt recovered Protection issue: %+v err=%v", heldMissing, err)
	}
	if status := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceProtection); status.Covered || status.Status != alertShadowStatusUnavailable {
		t.Fatalf("missing API-order receipt was treated as covered: %+v", status)
	}

	now = base.Add(90*time.Second + time.Second)
	malformedAt := base.Add(90 * time.Second)
	malformed := alertShadowProtectionInput{AsOf: malformedAt, EvidenceAsOf: malformedAt, Status: orderIntegrityHealthCurrent, Scope: scope,
		OrderSnapshotAsOf: malformedAt, OrderSnapshotComplete: true, OrderUniverse: protectionOrderUniverseJournaledAPI,
		Summary: rpc.ProtectionCoverageSummary{AsOf: malformedAt, Status: "ok", ReconcileRequiredOrders: []rpc.ProtectionCoverageOrder{issueOrder}}}
	heldMalformed, err := composer.ObserveProtection(t.Context(), malformed)
	if err != nil || len(heldMalformed.Candidates) != 1 || heldMalformed.Candidates[0].EvidenceHealth == rpc.AlertEvidenceCurrent {
		t.Fatalf("malformed Protection summary cleared issue: %+v err=%v", heldMalformed, err)
	}
	if status := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceProtection); status.Covered || status.Status != alertShadowStatusError {
		t.Fatalf("malformed Protection summary was trusted: %+v", status)
	}

	now = base.Add(2*time.Minute + time.Second)
	stale := alertShadowProtectionInput{AsOf: base.Add(2 * time.Minute), EvidenceAsOf: issueAt, Status: orderIntegrityHealthStale, Scope: scope,
		Summary: rpc.ProtectionCoverageSummary{AsOf: base.Add(2 * time.Minute), Status: "ok"}}
	held, err := composer.ObserveProtection(t.Context(), stale)
	if err != nil || len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen || held.Candidates[0].EvidenceHealth == rpc.AlertEvidenceCurrent {
		t.Fatalf("stale protection evidence cleared issue: %+v err=%v", held, err)
	}

	now = base.Add(3*time.Minute + time.Second)
	clearAt := base.Add(3 * time.Minute)
	current := alertShadowProtectionInput{AsOf: clearAt, EvidenceAsOf: clearAt, Status: orderIntegrityHealthCurrent, Scope: scope,
		OrderSnapshotAsOf: clearAt, OrderSnapshotComplete: true, OrderUniverse: protectionOrderUniverseJournaledAPI,
		Summary: rpc.ProtectionCoverageSummary{AsOf: clearAt, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
			ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}}}}
	recovered, err := composer.ObserveProtection(t.Context(), current)
	if err != nil || len(recovered.Candidates) != 1 || recovered.Candidates[0].State != rpc.AlertEpisodeRecovered {
		t.Fatalf("current protection evidence did not recover: %+v err=%v", recovered, err)
	}
}

func TestAlertShadowComposerDataHealthRequiresEstablishedRoots(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 16, 30, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }
	health := rpc.HealthResult{Connected: false, Subsystems: []rpc.SubsystemHealth{{Name: "storage", Status: "ready"}}}

	starting, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{AsOf: base, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayConnecting})
	if err != nil || len(starting.Candidates) != 0 {
		t.Fatalf("normal startup created Gateway incident: %+v err=%v", starting, err)
	}
	if status := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceDataHealth); status.Covered || status.Reason != alertShadowReasonConfirmationPending {
		t.Fatalf("startup Data Health claimed coverage: %+v", status)
	}

	now = base.Add(time.Minute + time.Second)
	failureAt := base.Add(time.Minute)
	gateway, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{AsOf: failureAt, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayFailed})
	if err != nil || len(gateway.Candidates) != 1 || gateway.Candidates[0].Source != rpc.AlertSourceDataHealth {
		t.Fatalf("established Gateway outage not recorded: %+v err=%v", gateway, err)
	}

	now = base.Add(2*time.Minute + time.Second)
	readyAt := base.Add(2 * time.Minute)
	health = rpc.HealthResult{Connected: true, Subsystems: alertShadowTestHealthySubsystems()}
	health.Subsystems = health.Subsystems[:4]
	missingEngine, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{
		AsOf: readyAt, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayReady, ProposalsExpected: true,
	})
	if err != nil || len(missingEngine.Candidates) != 2 {
		t.Fatalf("missing enabled engine was treated as clear: %+v err=%v", missingEngine, err)
	}

	now = base.Add(3*time.Minute + time.Second)
	missingFarmAt := base.Add(3 * time.Minute)
	health.Subsystems = []rpc.SubsystemHealth{{Name: "storage", Status: "ready"}, {Name: "history", Status: "ready"}, {Name: "chain", Status: "ready"}}
	untrusted, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{AsOf: missingFarmAt, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayReady})
	if err != nil || len(untrusted.Candidates) != 1 || untrusted.Candidates[0].State != rpc.AlertEpisodeOpen {
		t.Fatalf("missing farm readiness lost retained incidents: %+v err=%v", untrusted, err)
	}
	if status := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceDataHealth); status.Covered || status.Status != alertShadowStatusError {
		t.Fatalf("missing farm readiness claimed clear: %+v", status)
	}
}

func TestAlertShadowComposerDataHealthUsesRootIncidentsAndNotDueIsClear(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }
	health := rpc.HealthResult{Connected: true, Subsystems: []rpc.SubsystemHealth{
		{Name: "storage", Status: "ready"}, {Name: "quote", Status: "ready"}, {Name: "history", Status: "ready"}, {Name: "chain", Status: "ready"},
	},
		DataQuality: []rpc.DataQualityHealth{{Surface: "gamma", Status: "partial", CadenceState: rpc.DataCadenceNotDue, AsOf: base.Add(-24 * time.Hour)}}}

	clear, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{AsOf: base, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayReady})
	if err != nil || len(clear.Candidates) != 0 || !alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceDataHealth).Covered {
		t.Fatalf("not-due data became incident: %+v err=%v", clear, err)
	}

	now = base.Add(time.Minute + time.Second)
	failureAt := base.Add(time.Minute)
	health.DataQuality = []rpc.DataQualityHealth{{Surface: "gamma", Status: "partial", CadenceState: rpc.DataCadenceMissedSession, AsOf: failureAt}}
	health.Subsystems[1].Status = "degraded"
	health.DataFarms = []rpc.DataFarmHealth{{Name: "untrusted-farm-name", Type: "market", Status: "broken", Code: 2110, AsOf: failureAt}}
	opened, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{AsOf: failureAt, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayReady})
	if err != nil || len(opened.Candidates) != 2 {
		t.Fatalf("root incidents=%+v err=%v", opened, err)
	}
	for _, candidate := range opened.Candidates {
		if candidate.Source != rpc.AlertSourceDataHealth || candidate.Kind != rpc.AlertKindDataHealth ||
			candidate.Severity != rpc.AlertSeverityWatch || candidate.PresentationCode == "" {
			t.Fatalf("invalid data-health candidate=%+v", candidate)
		}
	}
	evaluations := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceDataHealth).Measurements.Evaluations

	now = failureAt.Add(5 * time.Second)
	if _, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{AsOf: now, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayReady}); err != nil {
		t.Fatal(err)
	}
	if got := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceDataHealth).Measurements.Evaluations; got != evaluations {
		t.Fatalf("hot-poll throttle evaluations=%d want %d", got, evaluations)
	}
	beforeRename := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceDataHealth).Measurements
	now = failureAt.Add(35 * time.Second)
	health.DataFarms[0].Name = "adversarial:new-name"
	renamed, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{AsOf: now, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayReady})
	if err != nil || len(renamed.Candidates) != 2 {
		t.Fatalf("canonical farm identity changed candidate set: %+v err=%v", renamed, err)
	}
	afterRename := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceDataHealth).Measurements
	if afterRename.EpisodesOpened != beforeRename.EpisodesOpened || afterRename.ActiveEvidenceChurn != beforeRename.ActiveEvidenceChurn {
		t.Fatalf("broker farm free text changed durable identity: before=%+v after=%+v", beforeRename, afterRename)
	}

	now = base.Add(2*time.Minute + time.Second)
	recoveryAt := base.Add(2 * time.Minute)
	health.DataQuality, health.DataFarms = nil, nil
	health.Subsystems[1].Status = "ready"
	recovered, err := composer.ObserveDataHealth(t.Context(), alertShadowDataHealthInput{AsOf: recoveryAt, Health: health, Scope: scope, GatewayPhase: alertShadowGatewayReady})
	if err != nil || len(recovered.Candidates) != 2 {
		t.Fatalf("data-health recovery=%+v err=%v", recovered, err)
	}
	for _, candidate := range recovered.Candidates {
		if candidate.State != rpc.AlertEpisodeRecovered {
			t.Fatalf("data-health incident did not recover: %+v", candidate)
		}
	}
}

func TestAlertShadowComposerRetriesFailedApplyExpiresCrossSourceCoverageAndIsolatesScope(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	base := time.Date(2026, 7, 21, 8, 30, 0, 0, time.UTC)
	now := base
	composer.now = func() time.Time { return now }
	scope := alertShadowTestBrokerScope(t)
	relevant := true
	stress := alertShadowTestStress(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "retry-open")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := composer.ObserveStress(cancelled, scope, stress); err == nil {
		t.Fatal("cancelled registry apply unexpectedly succeeded")
	}
	failed := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceStress)
	if composer.Status(scope).RegistryApplyFailures != 1 || failed.Measurements.DuplicateInputs != 0 {
		t.Fatalf("failed apply advanced cursor or missed failure: %+v", composer.Status(scope))
	}
	restartedRegistry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	restartedAfterFailure := newAlertShadowComposer(restartedRegistry)
	restartedAfterFailure.now = func() time.Time { return base }
	if got := restartedAfterFailure.Status(scope); got.RegistryApplyFailures != 1 || got.LastErrorCode != alertShadowReasonRegistryApplyFailed {
		t.Fatalf("restart lost durable apply failure: %+v", got)
	}
	opened, err := composer.ObserveStress(t.Context(), scope, stress)
	if err != nil || len(opened.Candidates) != 1 || opened.Candidates[0].State != rpc.AlertEpisodeOpen {
		t.Fatalf("exact retry did not persist: %+v err=%v", opened, err)
	}
	oldScopeEpisode := opened.Candidates[0].EpisodeKey

	now = base.Add(time.Minute)
	nudgeInput := alertShadowTestNudges(scope, now)
	afterNudges, err := composer.ObserveNudges(t.Context(), nudgeInput)
	if err != nil {
		t.Fatal(err)
	}
	assertAlertShadowCoverage(t, afterNudges.Coverage, []rpc.AlertSource{
		rpc.AlertSourceStress, rpc.AlertSourceRiskPolicy, rpc.AlertSourceReconciliation, rpc.AlertSourceGovernance,
	})
	if len(afterNudges.Candidates) != 1 || afterNudges.Candidates[0].State != rpc.AlertEpisodeOpen ||
		afterNudges.Candidates[0].EvidenceHealth != rpc.AlertEvidenceCurrent {
		t.Fatalf("cross-source poll reasserted or recovered cached Canary: %+v", afterNudges.Candidates)
	}
	if !alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceStress).Covered {
		t.Fatal("fresh Canary coverage expired on unrelated Nudge evaluation")
	}
	statusAfterNudges := composer.Status(scope)
	if got := alertShadowTestSourceStatus(t, statusAfterNudges, rpc.AlertSourceStress).Measurements.Evaluations; got != 1 {
		t.Fatalf("unrelated Nudge poll incremented Canary evaluation opportunities: %d", got)
	}
	for _, source := range alertShadowNudgeSources {
		if got := alertShadowTestSourceStatus(t, statusAfterNudges, source).Measurements.Evaluations; got != 1 {
			t.Fatalf("Nudge source %s evaluations=%d want 1", source, got)
		}
	}

	otherScope, err := newAlertShadowBrokerScope(brokerStateScope{Account: "DU-OTHER", Mode: rpc.AccountModeLive})
	if err != nil {
		t.Fatal(err)
	}
	now = base.Add(2 * time.Minute)
	otherStress := alertShadowTestStress(base, risk.SeverityAct, "defend", &relevant, rpc.SourceStatusOK, "other-scope")
	isolated, err := composer.ObserveStress(t.Context(), otherScope, otherStress)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated.Candidates) != 1 || isolated.Candidates[0].EpisodeKey == oldScopeEpisode {
		t.Fatalf("scope change leaked prior authority: %+v", isolated.Candidates)
	}
	oldScopeSnapshot, ok, err := composer.Snapshot(scope)
	if err != nil || !ok || len(oldScopeSnapshot.Candidates) != 1 || oldScopeSnapshot.Candidates[0].EpisodeKey != oldScopeEpisode {
		t.Fatalf("prior scope audit/current state lost: %+v ok=%v err=%v", oldScopeSnapshot, ok, err)
	}
	document, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, alertEpisodeRegistryStateKind)
	if err != nil || !ok {
		t.Fatalf("load registry document ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(document.JSON), "DU-SHADOW") || strings.Contains(string(document.JSON), "DU-OTHER") {
		t.Fatalf("registry persisted raw broker scope: %s", document.JSON)
	}

	future := alertShadowTestStress(now.Add(time.Minute), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "future")
	if _, err := composer.ObserveStress(t.Context(), otherScope, future); err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("future producer time error=%v", err)
	}
}

func TestAlertShadowComposerStressOutageAndUnstampedNegativeNeverRecover(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	now := base
	composer.now = func() time.Time { return now }
	relevant := true

	stress := alertShadowTestStress(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "outage-open")
	opened, err := composer.ObserveStress(t.Context(), scope, stress)
	if err != nil {
		t.Fatal(err)
	}
	openingOccurrence := opened.Candidates[0].OccurrenceKey

	now = base.Add(time.Minute)
	staleNegative := alertShadowTestStress(now, risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusStale, "stale-negative")
	held, err := composer.ObserveStress(t.Context(), scope, staleNegative)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen || held.Candidates[0].EvidenceHealth != rpc.AlertEvidenceStale || held.Candidates[0].OccurrenceKey != openingOccurrence {
		t.Fatalf("stale negative recovered or rewrote episode: %+v", held.Candidates)
	}
	if held.Coverage.State != rpc.AlertCoverageUnavailable || held.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("outage coverage=%+v state=%s", held.Coverage, held.CurrentState)
	}

	now = base.Add(2 * time.Minute)
	unstamped := alertShadowTestStress(now, risk.SeverityObserve, "observe", nil, rpc.SourceStatusOK, "unstamped-negative")
	held, err = composer.ObserveStress(t.Context(), scope, unstamped)
	if err != nil {
		t.Fatal(err)
	}
	if held.Candidates[0].State != rpc.AlertEpisodeOpen || held.Candidates[0].EvidenceHealth != rpc.AlertEvidencePartial {
		t.Fatalf("unstamped negative recovered episode: %+v", held.Candidates)
	}

	older := alertShadowTestStress(base.Add(90*time.Second), risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "older")
	if _, err := composer.ObserveStress(t.Context(), scope, older); err != nil {
		t.Fatal(err)
	}
	canary := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceStress)
	if canary.Reason != alertShadowReasonMissingRelevanceStamp || canary.Covered || canary.Measurements.StaleSuppressions < 2 || canary.Measurements.CoverageFailures < 2 {
		t.Fatalf("outage metrics/status=%+v", canary)
	}
}

func TestAlertShadowComposerNudgeOwnershipRecoveryAndNoDelivery(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	base := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }
	scope := alertShadowTestBrokerScope(t)

	input := alertShadowTestNudges(scope, base,
		alertShadowTestPolicyDrift(base.Add(-time.Minute)),
		alertShadowTestReconcileException(base.Add(-time.Minute)),
		alertShadowTestMonthlyPulse(base.Add(-time.Minute)),
	)
	opened, err := composer.ObserveNudges(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	assertAlertShadowCoverage(t, opened.Coverage, []rpc.AlertSource{
		rpc.AlertSourceRiskPolicy, rpc.AlertSourceReconciliation, rpc.AlertSourceGovernance,
	})
	if len(opened.Candidates) != 3 || opened.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("Nudge candidates=%+v state=%s", opened.Candidates, opened.CurrentState)
	}
	wantKind := map[rpc.AlertSource]rpc.AlertKind{
		rpc.AlertSourceRiskPolicy:     rpc.AlertKindPolicyDrift,
		rpc.AlertSourceReconciliation: rpc.AlertKindReconciliationException,
		rpc.AlertSourceGovernance:     rpc.AlertKindGovernance,
	}
	wantPresentation := map[rpc.AlertSource]rpc.AlertPresentationCode{
		rpc.AlertSourceRiskPolicy:     rpc.AlertPresentationRiskPolicyDrift,
		rpc.AlertSourceReconciliation: rpc.AlertPresentationReconciliationException,
		rpc.AlertSourceGovernance:     rpc.AlertPresentationGovernanceMonthlyPulse,
	}
	occurrences := make(map[rpc.AlertSource]string)
	for _, candidate := range opened.Candidates {
		if candidate.Kind != wantKind[candidate.Source] || candidate.PresentationCode != wantPresentation[candidate.Source] {
			t.Fatalf("Nudge ownership/delivery mismatch: %+v", candidate)
		}
		occurrences[candidate.Source] = candidate.OccurrenceKey
	}

	now = base.Add(time.Minute + time.Second)
	empty := alertShadowTestNudges(scope, base.Add(time.Minute))
	recovered, err := composer.ObserveNudges(t.Context(), empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Candidates) != 3 || recovered.CurrentState != rpc.AlertSnapshotUnknown {
		t.Fatalf("Nudge recovery snapshot=%+v", recovered)
	}
	for _, candidate := range recovered.Candidates {
		if candidate.State != rpc.AlertEpisodeRecovered || candidate.OccurrenceKey != occurrences[candidate.Source] || candidate.PresentationCode != wantPresentation[candidate.Source] {
			t.Fatalf("Nudge recovery invalid: %+v", candidate)
		}
	}
	status := composer.Status(scope)
	for _, source := range alertShadowNudgeSources {
		item := alertShadowTestSourceStatus(t, status, source)
		if !item.Covered || item.Status != alertShadowStatusCurrent || item.Measurements.EpisodesRecovered != 1 {
			t.Fatalf("source %s status=%+v", source, item)
		}
	}
}

func TestAlertShadowComposerNudgeV3InactiveReminderSourcesRemainCovered(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	now := time.Date(2026, 7, 21, 10, 30, 0, 0, time.UTC)
	composer.now = func() time.Time { return now.Add(time.Second) }
	scope := alertShadowTestBrokerScope(t)
	input := alertShadowTestNudges(scope, now,
		alertShadowTestPolicyDrift(now.Add(-time.Minute)),
		alertShadowTestReconcileException(now.Add(-time.Minute)),
	)
	inactive := rpc.NudgeInputHealth{
		Status: rpc.NudgeInputStatusInactive, Reason: rpc.NudgeHealthReasonProcessRemindersNotEnabled, AsOf: now,
	}
	input.Snapshot.SourceHealth.Cadence = inactive
	input.Snapshot.SourceHealth.ConfirmedFlow = inactive
	input.Snapshot.ConfirmedFlowCoverage = nil

	snapshot, err := composer.ObserveNudges(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	assertAlertShadowCoverage(t, snapshot.Coverage, []rpc.AlertSource{
		rpc.AlertSourceRiskPolicy, rpc.AlertSourceReconciliation, rpc.AlertSourceGovernance,
	})
	if len(snapshot.Candidates) != 2 {
		t.Fatalf("v3 independent candidates=%+v", snapshot.Candidates)
	}
	for _, candidate := range snapshot.Candidates {
		if candidate.EvidenceHealth != rpc.AlertEvidenceCurrent {
			t.Fatalf("v3 independent candidate is not current: %+v", candidate)
		}
	}
}

func TestAlertShadowNudgePositiveDependenciesAreCandidateSpecific(t *testing.T) {
	base := time.Date(2026, 7, 21, 10, 45, 0, 0, time.UTC)
	ok := func() rpc.NudgeInputHealth { return rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: base} }
	stale := func() rpc.NudgeInputHealth {
		return rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusStale, Reason: rpc.NudgeHealthReasonEvidenceStale, AsOf: base}
	}
	unavailable := func() rpc.NudgeInputHealth {
		return rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusUnavailable, Reason: rpc.NudgeHealthReasonSourceUnavailable, AsOf: base}
	}
	tests := []struct {
		name string
		kind string
		edit func(*rpc.NudgeSourceHealth, *rpc.NudgeInputHealth)
		want rpc.AlertEvidenceHealth
	}{
		{"policy drift ignores capital", rpc.NudgeKindPolicyDrift, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Capital = stale() }, rpc.AlertEvidenceCurrent},
		{"policy drift requires pins", rpc.NudgeKindPolicyDrift, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Pins = stale() }, rpc.AlertEvidenceStale},
		{"drawdown ignores pins", rpc.NudgeKindDrawdownLatched, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Pins = stale() }, rpc.AlertEvidenceCurrent},
		{"drawdown requires capital", rpc.NudgeKindDrawdownLatched, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Capital = stale() }, rpc.AlertEvidenceStale},
		{"shadow requires store", rpc.NudgeKindShadowWouldBlock, func(_ *rpc.NudgeSourceHealth, store *rpc.NudgeInputHealth) { *store = unavailable() }, rpc.AlertEvidenceUnavailable},
		{"reconcile exception ignores capital", rpc.NudgeKindReconcileException, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Capital = stale() }, rpc.AlertEvidenceCurrent},
		{"reconcile exception requires report", rpc.NudgeKindReconcileException, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Reconciliation = unavailable() }, rpc.AlertEvidenceUnavailable},
		{"reconcile due ignores report", rpc.NudgeKindReconcileDue, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Reconciliation = unavailable() }, rpc.AlertEvidenceCurrent},
		{"reconcile due requires cadence", rpc.NudgeKindReconcileDue, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Cadence = stale() }, rpc.AlertEvidenceStale},
		{"confirmed flow ignores cadence", rpc.NudgeKindConfirmedFlow, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Cadence = stale() }, rpc.AlertEvidenceCurrent},
		{"confirmed flow requires report", rpc.NudgeKindConfirmedFlow, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Reconciliation = unavailable() }, rpc.AlertEvidenceUnavailable},
		{"monthly ignores capital", rpc.NudgeKindMonthlyPulse, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Capital = stale() }, rpc.AlertEvidenceCurrent},
		{"monthly requires report", rpc.NudgeKindMonthlyPulse, func(h *rpc.NudgeSourceHealth, _ *rpc.NudgeInputHealth) { h.Reconciliation = unavailable() }, rpc.AlertEvidenceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			health := rpc.NudgeSourceHealth{Policy: ok(), Reconciliation: ok(), Capital: ok(), Pins: ok(), Cadence: ok(), ConfirmedFlow: ok()}
			store := ok()
			tc.edit(&health, &store)
			_, got, _, _ := alertShadowNudgeCandidateHealth(tc.kind, base, health, store)
			if got != tc.want {
				t.Fatalf("evidence health=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestAlertShadowNudgeStoreOutageKeepsIndependentPositiveCurrent(t *testing.T) {
	base := time.Date(2026, 7, 21, 10, 50, 0, 0, time.UTC)
	input := alertShadowTestNudges(alertShadowTestBrokerScope(t), base, alertShadowTestPolicyDrift(base.Add(-time.Minute)))
	input.StoreHealth = rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusUnavailable, Reason: rpc.NudgeHealthReasonSourceUnavailable, AsOf: base}
	batches, _, err := alertShadowMapNudges(input, base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	batch := batches[rpc.AlertSourceRiskPolicy]
	if batch.Covered || len(batch.Observations) != 1 || batch.Observations[0].EvidenceHealth != rpc.AlertEvidenceCurrent {
		t.Fatalf("store outage suppressed independent policy-drift positive: %+v", batch)
	}
}

func TestAlertShadowCurrentNegativeRecoversAcrossPolicyChangeExceptProtection(t *testing.T) {
	newComposer := func(t *testing.T, now *time.Time) (*alertShadowComposer, alertShadowBrokerScope) {
		t.Helper()
		store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
		t.Cleanup(func() { store.Close() })
		registry, err := newAlertEpisodeRegistry(t.Context(), store)
		if err != nil {
			t.Fatal(err)
		}
		composer := newAlertShadowComposer(registry)
		composer.now = func() time.Time { return *now }
		return composer, alertShadowTestBrokerScope(t)
	}
	assertRecovered := func(t *testing.T, snapshot rpc.AlertCandidateSnapshot) {
		t.Helper()
		if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].State != rpc.AlertEpisodeRecovered {
			t.Fatalf("cross-policy current negative did not recover: %+v", snapshot.Candidates)
		}
	}
	base := time.Date(2026, 7, 21, 10, 55, 0, 0, time.UTC)
	relevant := true

	t.Run("Canary", func(t *testing.T) {
		now := base.Add(time.Second)
		composer, scope := newComposer(t, &now)
		active := alertShadowTestStress(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "policy-a-active")
		active.PolicyFingerprint.Key = alertShadowTestFingerprint("stress-policy-a")
		if _, err := composer.ObserveStress(t.Context(), scope, active); err != nil {
			t.Fatal(err)
		}
		now = base.Add(time.Minute + time.Second)
		clear := alertShadowTestStress(base.Add(time.Minute), risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "policy-b-clear")
		clear.PolicyFingerprint.Key = alertShadowTestFingerprint("stress-policy-b")
		snapshot, err := composer.ObserveStress(t.Context(), scope, clear)
		if err != nil {
			t.Fatal(err)
		}
		assertRecovered(t, snapshot)
	})

	t.Run("Rulebook", func(t *testing.T) {
		now := base.Add(time.Second)
		composer, scope := newComposer(t, &now)
		active := alertShadowTestRulebook(base, risk.RuleStatusWatch)
		active.PolicyFingerprint.Key = alertShadowTestFingerprint("rulebook-policy-a")
		if _, err := composer.ObserveRulebook(t.Context(), scope, active); err != nil {
			t.Fatal(err)
		}
		now = base.Add(time.Minute + time.Second)
		clear := alertShadowTestRulebook(base.Add(time.Minute), risk.RuleStatusPass)
		clear.PolicyFingerprint.Key = alertShadowTestFingerprint("rulebook-policy-b")
		snapshot, err := composer.ObserveRulebook(t.Context(), scope, clear)
		if err != nil {
			t.Fatal(err)
		}
		assertRecovered(t, snapshot)
	})

	t.Run("Nudge", func(t *testing.T) {
		now := base.Add(time.Second)
		composer, scope := newComposer(t, &now)
		active := alertShadowTestNudges(scope, base, alertShadowTestPolicyDrift(base.Add(-time.Minute)))
		active.PolicyFingerprint.Key = alertShadowTestFingerprint("nudge-policy-a")
		if _, err := composer.ObserveNudges(t.Context(), active); err != nil {
			t.Fatal(err)
		}
		now = base.Add(time.Minute + time.Second)
		clear := alertShadowTestNudges(scope, base.Add(time.Minute))
		clear.PolicyFingerprint.Key = alertShadowTestFingerprint("nudge-policy-b")
		snapshot, err := composer.ObserveNudges(t.Context(), clear)
		if err != nil {
			t.Fatal(err)
		}
		assertRecovered(t, snapshot)
	})
}

func TestAlertShadowComposerNudgeOutageDuplicateAndEquivocation(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	now := base
	composer.now = func() time.Time { return now }
	scope := alertShadowTestBrokerScope(t)
	active := alertShadowTestPolicyDrift(base.Add(-time.Minute))
	input := alertShadowTestNudges(scope, base, active, active)
	opened, err := composer.ObserveNudges(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Candidates) != 1 {
		t.Fatalf("duplicate Nudge was not suppressed: %+v", opened.Candidates)
	}
	riskPolicy := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceRiskPolicy)
	if riskPolicy.Measurements.DuplicateCandidates != 1 {
		t.Fatalf("duplicate Nudge metric=%+v", riskPolicy.Measurements)
	}

	now = base.Add(time.Minute)
	outage := alertShadowTestNudges(scope, now)
	outage.Snapshot.SourceHealth.Policy = rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusStale, Reason: rpc.NudgeHealthReasonEvidenceStale, AsOf: now}
	held, err := composer.ObserveNudges(t.Context(), outage)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen || held.Candidates[0].EvidenceHealth != rpc.AlertEvidenceStale {
		t.Fatalf("Nudge outage recovered episode: %+v", held.Candidates)
	}

	equivocal := outage
	equivocal.Snapshot.Candidates = []rpc.NudgeCandidate{alertShadowTestReconcileException(now.Add(-time.Second))}
	if _, err := composer.ObserveNudges(t.Context(), equivocal); err == nil || !strings.Contains(err.Error(), "equivocation") {
		t.Fatalf("same-time Nudge equivocation error=%v", err)
	}
	status := composer.Status(scope)
	if status.Equivocations != 1 {
		t.Fatalf("equivocations=%d", status.Equivocations)
	}
	for _, source := range alertShadowNudgeSources {
		if alertShadowTestSourceStatus(t, status, source).Measurements.Equivocations != 1 {
			t.Fatalf("source %s missed equivocation", source)
		}
	}
}

func TestAlertShadowComposerRestartFacingEmptyAndConcurrentReplay(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	if snapshot, ok, err := composer.Snapshot(alertShadowTestBrokerScope(t)); err != nil || ok || !snapshot.AsOf.IsZero() {
		t.Fatalf("fresh composer snapshot=%+v ok=%v err=%v", snapshot, ok, err)
	}
	initial := composer.Status(alertShadowTestBrokerScope(t))
	if len(initial.Sources) != 9 || initial.HumanPrecision != alertShadowHumanLabelUnlabelled || initial.HumanRecall != alertShadowHumanLabelUnlabelled {
		t.Fatalf("fresh status=%+v", initial)
	}

	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	relevant := true
	scope := alertShadowTestBrokerScope(t)
	composer.now = func() time.Time { return base }
	if _, err := composer.ObserveStress(t.Context(), scope, alertShadowTestStress(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "restart-open")); err != nil {
		t.Fatal(err)
	}

	restarted := newAlertShadowComposer(registry)
	restarted.now = func() time.Time { return base.Add(time.Minute) }
	durable, ok, err := restarted.Snapshot(scope)
	if err != nil || !ok || len(durable.Candidates) != 1 || durable.CurrentState != rpc.AlertSnapshotActive ||
		durable.Coverage.State != rpc.AlertCoverageUnavailable || durable.Coverage.Freshness != rpc.AlertCoverageUnknown ||
		len(durable.Coverage.CoveredSources) != 0 || durable.Candidates[0].EvidenceHealth != rpc.AlertEvidenceUnavailable {
		t.Fatalf("restart durable snapshot=%+v ok=%v err=%v", durable, ok, err)
	}
	if got := alertShadowTestSourceStatus(t, restarted.Status(scope), rpc.AlertSourceStress); got.Status != alertShadowStatusNotObserved || got.Covered {
		t.Fatalf("restart reconstructed Canary coverage: %+v", got)
	}
	restartStatus := restarted.Status(scope)
	restartStress := alertShadowTestSourceStatus(t, restartStatus, rpc.AlertSourceStress)
	if restartStatus.Evaluations != 1 || restartStress.Measurements.Evaluations != 1 || restartStress.Measurements.EpisodesOpened != 1 || restartStress.Measurements.TimeToObserveSamples != 1 {
		t.Fatalf("restart lost durable commissioning metrics: %+v", restartStatus)
	}

	staleReplay := alertShadowTestStress(base.Add(-time.Second), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "restart-open")
	staleProjection, err := restarted.ObserveStress(t.Context(), scope, staleReplay)
	if err != nil || staleProjection.Coverage.State != rpc.AlertCoverageUnavailable || staleProjection.Coverage.Freshness != rpc.AlertCoverageUnknown ||
		len(staleProjection.Coverage.CoveredSources) != 0 || len(staleProjection.Candidates) != 1 || staleProjection.Candidates[0].EvidenceHealth != rpc.AlertEvidenceUnavailable {
		t.Fatalf("stale restart replay resurrected coverage: %+v err=%v", staleProjection, err)
	}
	if got := alertShadowTestSourceStatus(t, restarted.Status(scope), rpc.AlertSourceStress); got.Status != alertShadowStatusNotObserved || got.Covered {
		t.Fatalf("stale restart replay changed process coverage: %+v", got)
	}
	exactReplay := alertShadowTestStress(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "restart-open")
	reobserved, err := restarted.ObserveStress(t.Context(), scope, exactReplay)
	if err != nil || reobserved.Coverage.State != rpc.AlertCoveragePartial || reobserved.Coverage.Freshness != rpc.AlertCoverageCurrent ||
		len(reobserved.Coverage.CoveredSources) != 1 || reobserved.Coverage.CoveredSources[0] != rpc.AlertSourceStress {
		t.Fatalf("exact restart reobservation did not restore only Canary coverage: %+v err=%v", reobserved, err)
	}

	restarted.now = func() time.Time { return base.Add(time.Minute + time.Second) }
	replay := alertShadowTestStress(base.Add(time.Minute), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "restart-open")
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			_, observeErr := restarted.ObserveStress(context.Background(), scope, replay)
			errs <- observeErr
		})
	}
	wg.Wait()
	close(errs)
	for observeErr := range errs {
		if observeErr != nil {
			t.Fatal(observeErr)
		}
	}
	canary := alertShadowTestSourceStatus(t, restarted.Status(scope), rpc.AlertSourceStress)
	if canary.Measurements.DuplicateInputs != workers-1 {
		t.Fatalf("concurrent duplicate inputs=%d want %d", canary.Measurements.DuplicateInputs, workers-1)
	}
	final, ok, err := restarted.Snapshot(scope)
	if err != nil || !ok || len(final.Candidates) != 1 || final.Candidates[0].PresentationCode != rpc.AlertPresentationPortfolioStress {
		t.Fatalf("concurrent replay snapshot=%+v ok=%v err=%v", final, ok, err)
	}
}

func alertShadowTestStress(at time.Time, severity risk.SignalSeverity, action string, relevant *bool, sourceStatus, seed string) rpc.StressResult {
	source := func(name string) rpc.SourceHealth {
		fingerprint := rpc.Fingerprint{Version: name + "-fp-v1", Key: alertShadowTestFingerprint(seed + "-" + name)}
		return rpc.SourceHealth{
			Source: name, Status: sourceStatus, AsOf: at, MaxAgeSeconds: 300,
			Fingerprint: &fingerprint, FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
		}
	}
	return rpc.StressResult{
		AsOf: at, Fingerprint: rpc.Fingerprint{Version: rpc.StressFingerprintVersion, Key: alertShadowTestFingerprint(seed)},
		PolicyFingerprint: rpc.Fingerprint{Version: "stress-policy-fp-v1", Key: alertShadowTestFingerprint("stress-policy")},
		Action:            action, Severity: severity, PortfolioAlertRelevant: relevant, InputHealth: "ok",
		SourceHealth: []rpc.SourceHealth{source("account"), source("positions"), source("regime")},
	}
}

func alertShadowTestNudges(scope alertShadowBrokerScope, at time.Time, candidates ...rpc.NudgeCandidate) alertShadowNudgeInput {
	ok := func() rpc.NudgeInputHealth {
		return rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: at}
	}
	return alertShadowNudgeInput{
		Scope: scope, PolicyFingerprint: alertShadowTestPolicyFingerprint(), StoreHealth: ok(),
		Snapshot: rpc.NudgesSnapshotResult{
			AsOf: at, Candidates: append([]rpc.NudgeCandidate(nil), candidates...),
			SourceHealth: rpc.NudgeSourceHealth{
				Policy: ok(), Reconciliation: ok(), Capital: ok(), Pins: ok(), Cadence: ok(), ConfirmedFlow: ok(),
			},
			ConfirmedFlowCoverage: &rpc.NudgeConfirmedFlowCoverage{CoverageFrom: at.Add(-24 * time.Hour)},
		},
	}
}

func alertShadowTestRulebook(at time.Time, status string) rpc.RulesResult {
	policy := risk.DefaultRulebookPolicy()
	fingerprint := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: policy.FingerprintKey()}
	rules := make([]risk.RuleRow, 0, len(alertShadowCanonicalRulebookRows))
	for _, canonical := range alertShadowCanonicalRulebookRows {
		rules = append(rules, risk.RuleRow{
			ID: canonical.ID, Number: canonical.Number, Title: canonical.ID,
			Status: risk.RuleStatusPass, Evidence: "classified",
		})
	}
	rules[0].Status = status
	health := make([]rpc.SourceHealth, 0, len(alertShadowCanonicalRulebookHealth))
	for _, source := range alertShadowCanonicalRulebookHealth {
		health = append(health, rpc.SourceHealth{Source: source, Status: rpc.SourceStatusOK, AsOf: at})
	}
	return rpc.RulesResult{
		AsOf: at, Enabled: true, Status: "ok", PolicyID: policy.ID, PolicyVersion: policy.Version,
		PolicyFingerprint: &fingerprint,
		Rules:             rules,
		InputHealth:       health,
	}
}

func alertShadowTestSetEarningsNotEvaluated(result *rpc.RulesResult, id, reason string, symbols ...string) {
	for i := range result.Rules {
		if result.Rules[i].ID != id {
			continue
		}
		result.Rules[i].Status = risk.RuleStatusNotEvaluated
		result.Rules[i].Reason = reason
		result.Rules[i].Offenders = nil
		result.Rules[i].Exempt = nil
		for _, symbol := range symbols {
			result.Rules[i].Exempt = append(result.Rules[i].Exempt, risk.RuleOffender{Symbol: symbol})
		}
		return
	}
}

func alertShadowTestTerminalEarnings(symbol string, at time.Time) rpc.EarningsInfo {
	info := rpc.EarningsInfo{
		Symbol: symbol, Source: "verified_terminal", Status: rpc.EarningsStatusTerminalNonReporting,
		Terminal: &rpc.EarningsTerminalInfo{
			ContractConID: 17, Classification: earningsTerminalClassEquityCancelled,
			EffectiveDate: at.AddDate(0, -1, 0).Format(time.DateOnly),
			VerifiedAt:    at.Add(-2 * time.Hour), RevalidateAfter: at.Add(24 * time.Hour),
			AuthorityRevision: 7, AuthorityReviewedAt: at.Add(-time.Hour),
			AuthorityFingerprint: "sha256:" + strings.Repeat("a", 64),
		},
	}
	info.Terminal.AuthorityBinding = rpc.BuildEarningsTerminalAuthorityBinding(symbol, *info.Terminal)
	return info
}

func alertShadowTestBrokerEarnings(symbol string, at time.Time) rpc.EarningsInfo {
	proofObservedAt := at.Add(-2 * time.Hour)
	nextAttempt := proofObservedAt.Add(earningsFreshWindow)
	info := rpc.EarningsInfo{
		Symbol: symbol, Source: "broker_identity", Status: rpc.EarningsStatusNotApplicable,
		Identity: &rpc.EarningsIdentityInfo{
			Outcome: earningsIdentityNotApplicable, NotApplicable: true,
			AttemptedAt: proofObservedAt.Add(-time.Minute), ProofObservedAt: proofObservedAt,
			ProofOutcome: rpc.EarningsStatusNotApplicable, AuthorityRevision: 8,
			AuthorityFingerprint: "sha256:" + strings.Repeat("b", 64),
			ObservationID:        "oid:" + opaqueIdentity("alert-rulebook-test-receipt", symbol),
			NextAttempt:          &nextAttempt,
		},
	}
	info.Identity.AuthorityBinding = rpc.BuildEarningsIdentityAuthorityBinding(symbol, *info.Identity)
	return info
}

func alertShadowTestRetainedBrokerEarnings(symbol string, at time.Time) rpc.EarningsInfo {
	info := alertShadowTestBrokerEarnings(symbol, at)
	attemptedAt := at.Add(-2 * time.Minute)
	failedAt := at.Add(-time.Minute)
	nextAttempt := failedAt.Add(earningsContractResolutionRetry)
	info.Identity.Outcome = earningsIdentityUnknown
	info.Identity.AttemptedAt = attemptedAt
	info.Identity.NextAttempt = &nextAttempt
	info.Identity.LastFailure = &rpc.SourceFailure{
		Code: rpc.SourceFailureContractUnavailable, Stage: rpc.SourceFailureStageWSHContractResolve,
		FailedAt: failedAt, Retryable: true,
	}
	return info
}

func alertShadowTestRulebookPnLUnavailable(result *rpc.RulesResult) {
	result.Status = "degraded"
	for i := range result.InputHealth {
		if result.InputHealth[i].Source == "account" {
			result.InputHealth[i].Status = rpc.SourceStatusDegraded
		}
	}
	for i := range result.Rules {
		if result.Rules[i].ID == risk.RuleGreenDayAction {
			result.Rules[i].Status = risk.RuleStatusNotEvaluated
			result.Rules[i].Reason = risk.RuleReasonPnLUnavailable
		}
	}
}

func alertShadowTestRegime(at time.Time, stage, readiness string) rpc.RegimeSnapshotResult {
	lastSuccess := at.UTC()
	age := int64(0)
	meta := func(band string, eligible bool) rpc.RegimeIndicatorMeta {
		out := rpc.RegimeIndicatorMeta{Band: band, Freshness: &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh, MaxAgeSeconds: rpc.RegimeSourceMaxAgeSeconds("breadth")}}
		if band == "red" {
			out.Eligibility = &rpc.RegimeEligibility{Eligible: eligible}
		}
		return out
	}
	result := rpc.RegimeSnapshotResult{
		AsOf: at.UTC(), Summary: rpc.RegimeSummary{Confidence: "high"},
		AuthorityHealth:  &rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityFresh, LastSuccessAt: &lastSuccess, LastSuccessAgeSeconds: &age},
		SourceHealth:     make([]rpc.SourceHealth, 0, len(alertShadowRegimeRequiredSources)),
		VIXTermStructure: rpc.RegimeVIXTerm{RegimeIndicatorMeta: meta("green", false), Status: rpc.RegimeStatusOK},
		VolOfVol:         rpc.RegimeVolOfVol{RegimeIndicatorMeta: meta("green", false), Status: rpc.RegimeStatusOK},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{RegimeIndicatorMeta: meta("green", false), Status: rpc.RegimeStatusOK},
		CreditSpreads:    rpc.RegimeCreditSpreads{RegimeIndicatorMeta: meta("green", false), Status: rpc.RegimeStatusOK},
		FundingStress:    rpc.RegimeFundingStress{RegimeIndicatorMeta: meta("green", false), Status: rpc.RegimeStatusOK},
		USDJPY:           rpc.RegimeUSDJPY{RegimeIndicatorMeta: meta("green", false), Status: rpc.RegimeStatusOK},
		GammaZero: rpc.RegimeGammaZero{RegimeIndicatorMeta: meta("green", false), Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{Result: &rpc.GammaZeroComputed{Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable}}}},
		Breadth: rpc.RegimeBreadth{RegimeIndicatorMeta: meta("green", false), Status: rpc.RegimeStatusOK},
	}
	switch stage {
	case rpc.LifecycleEarlyWarning:
		result.FundingStress.RegimeIndicatorMeta = meta("red", false)
	case rpc.LifecycleConfirmedStress:
		result.FundingStress.RegimeIndicatorMeta = meta("red", true)
		result.Breadth.RegimeIndicatorMeta = meta("red", true)
	case rpc.LifecyclePanic:
		result.FundingStress.RegimeIndicatorMeta = meta("red", true)
		result.Breadth.RegimeIndicatorMeta = meta("red", true)
		result.CreditSpreads.RegimeIndicatorMeta = meta("red", true)
	case rpc.LifecycleDataQuality:
		// Two defective clusters: one alone degrades readiness and names
		// itself rather than blanking the whole read
		// (internal-docs/design/regime-input-currency.md).
		result.Breadth.Status = rpc.RegimeStatusUnavailable
		result.Breadth.Band = ""
		result.Breadth.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue, MaxAgeSeconds: rpc.RegimeSourceMaxAgeSeconds("breadth")}
		result.FundingStress.Status = rpc.RegimeStatusUnavailable
		result.FundingStress.Band = ""
		result.FundingStress.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue, MaxAgeSeconds: rpc.RegimeSourceMaxAgeSeconds("funding")}
	}
	for _, source := range alertShadowRegimeRequiredSources {
		result.SourceHealth = append(result.SourceHealth, rpc.SourceHealth{
			Source: source, Status: rpc.SourceStatusOK, AsOf: at.UTC(), MaxAgeSeconds: rpc.RegimeSourceMaxAgeSeconds(source),
			RefreshState: rpc.SourceRefreshCurrent,
		})
	}
	result.Lifecycle = rpc.BuildRegimeLifecycle(&result)
	if readiness != "" && readiness != result.Lifecycle.Readiness {
		result.Lifecycle.Readiness = readiness
		result.Lifecycle.Fingerprint = rpc.BuildLifecycleFingerprint(result.Lifecycle)
	}
	result.Fingerprint = rpc.BuildRegimeFingerprint(&result)
	return result
}

func alertShadowTestMismatchedOrder(at time.Time, scope alertShadowBrokerScope) rpc.OrderView {
	return rpc.OrderView{
		OrderRef: "private-order-ref", Endpoint: "127.0.0.1:4002", ClientID: 7,
		Account: scope.account, Mode: scope.mode, Open: true, Remaining: 100,
		ReconciliationState: "position_mismatch", ReconciliationKind: rpc.OrderReconciliationKindShortEntryExcess,
		ReconciliationSeverity: rpc.OrderReconciliationSeverityCritical, ShortRiskQuantity: 50, ReduceToQuantity: 50,
		BrokerTruthAsOf: at,
	}
}

func alertShadowTestBrokerScope(t *testing.T) alertShadowBrokerScope {
	t.Helper()
	scope, err := newAlertShadowBrokerScope(brokerStateScope{Account: "DU-SHADOW", Mode: rpc.AccountModePaper})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func alertShadowTestPolicyDrift(at time.Time) rpc.NudgeCandidate {
	return rpcNudgeCandidate(risk.EvaluatePolicyDrift([]risk.NudgePinMismatch{{
		Policy: "risk", PinnedID: "pinned", PinnedVersion: "1", LiveID: "live", LiveVersion: "2",
	}}, at))
}

func alertShadowTestReconcileException(at time.Time) rpc.NudgeCandidate {
	return rpcNudgeCandidate(risk.EvaluateReconcileException([]risk.ReconcileExceptionIdentity{{
		Kind: "amount", Identity: "opaque-row", Material: []string{"classified-material"},
	}}, at))
}

func alertShadowTestMonthlyPulse(at time.Time) rpc.NudgeCandidate {
	return rpc.NudgeCandidate{
		Fingerprint: alertShadowTestFingerprint("monthly-pulse"), Kind: rpc.NudgeKindMonthlyPulse,
		State: rpc.NudgeStateDue, Severity: rpc.NudgeSeverityWatch, OccurredAt: at, DueAt: at,
		Destination: rpc.NudgeDestinationBrief,
	}
}

func alertShadowTestPolicyFingerprint() rpc.Fingerprint {
	return rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: alertShadowTestFingerprint("risk-policy")}
}

func alertShadowTestFingerprint(seed string) string {
	fingerprint, err := alertShadowFingerprint(struct {
		Seed string `json:"seed"`
	}{seed})
	if err != nil {
		panic(err)
	}
	return fingerprint
}

func alertShadowTestSourceStatus(t *testing.T, status alertShadowStatusReport, source rpc.AlertSource) alertShadowSourceStatus {
	t.Helper()
	for _, item := range status.Sources {
		if item.Source == source {
			return item
		}
	}
	t.Fatalf("missing source status %s", source)
	return alertShadowSourceStatus{}
}

func assertAlertShadowCoverage(t *testing.T, coverage rpc.AlertCoverage, covered []rpc.AlertSource) {
	t.Helper()
	if coverage.State != rpc.AlertCoveragePartial || coverage.Freshness != rpc.AlertCoverageCurrent || len(coverage.ExpectedSources) != 9 {
		t.Fatalf("coverage=%+v", coverage)
	}
	if len(coverage.CoveredSources) != len(covered) {
		t.Fatalf("covered=%v want %v", coverage.CoveredSources, covered)
	}
	for i := range covered {
		if coverage.CoveredSources[i] != covered[i] {
			t.Fatalf("covered=%v want %v", coverage.CoveredSources, covered)
		}
	}
}

// The margin-cushion producer (2026-07-30 operator decision) alerts on
// account-only danger without market confirmation, holds through an
// unobserved cushion, recovers only from an observed healthy one, and
// coexists with the market-confirmed portfolio-stress occurrence.
func TestAlertShadowComposerMarginCushionIndependentLifecycle(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	now := base.Add(2 * time.Second)
	composer.now = func() time.Time { return now }
	relevant := true
	lowCushion := 2.0

	marginSignals := func(observed *float64) []risk.Signal {
		return []risk.Signal{
			{ID: risk.SignalMarginCushionLow, Direction: risk.DirectionDefensive, Severity: risk.SeverityUrgent, Metric: "cushion", Observed: observed},
			{ID: risk.SignalLookAheadCushionLow, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Metric: "lookahead_cushion", Observed: observed},
		}
	}

	// Account-only danger: the top-level verdict stays calm (observe /
	// stand_down), so the legacy occurrence gate never fires — the margin
	// producer must alert anyway, at the worst margin-signal severity.
	danger := alertShadowTestStress(base, risk.SeverityObserve, "stand_down", &relevant, rpc.SourceStatusOK, "margin-danger")
	danger.Portfolio.CushionPct = &lowCushion
	danger.Signals = marginSignals(&lowCushion)
	opened, err := composer.ObserveStress(t.Context(), scope, danger)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Candidates) != 1 {
		t.Fatalf("account-only danger candidates=%+v", opened.Candidates)
	}
	margin := opened.Candidates[0]
	if margin.Source != rpc.AlertSourceStress || margin.Kind != rpc.AlertKindMarginSafety ||
		margin.PresentationCode != rpc.AlertPresentationMarginCushion || margin.State != rpc.AlertEpisodeOpen ||
		margin.Severity != rpc.AlertSeverityUrgent || margin.Destination != rpc.AlertDestinationAlerts {
		t.Fatalf("unexpected margin candidate: %+v", margin)
	}

	// Unobserved cushion: no margin signals and no cushion values. The
	// synthesized source-level negative must not recover the episode.
	now = base.Add(time.Minute + 2*time.Second)
	unknown := alertShadowTestStress(base.Add(time.Minute), risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "margin-unknown")
	held, err := composer.ObserveStress(t.Context(), scope, unknown)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen ||
		held.Candidates[0].OccurrenceKey != margin.OccurrenceKey {
		t.Fatalf("unobserved cushion did not hold the open episode: %+v", held.Candidates)
	}

	// Observed healthy cushion: values present, no margin signals — the
	// source-level negative now carries real margin evidence and recovers.
	now = base.Add(2*time.Minute + 2*time.Second)
	healthyCushion := 35.0
	healthy := alertShadowTestStress(base.Add(2*time.Minute), risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "margin-healthy")
	healthy.Portfolio.CushionPct = &healthyCushion
	recovered, err := composer.ObserveStress(t.Context(), scope, healthy)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Candidates) != 1 || recovered.Candidates[0].State != rpc.AlertEpisodeRecovered ||
		recovered.Candidates[0].OccurrenceKey != margin.OccurrenceKey {
		t.Fatalf("observed healthy cushion did not recover: %+v", recovered.Candidates)
	}

	// Market-confirmed stress with a low cushion: both occurrences, each
	// under its own episode identity.
	now = base.Add(3*time.Minute + 2*time.Second)
	both := alertShadowTestStress(base.Add(3*time.Minute), risk.SeverityAct, "defend", &relevant, rpc.SourceStatusOK, "margin-both")
	both.Portfolio.CushionPct = &lowCushion
	both.Signals = marginSignals(&lowCushion)
	dual, err := composer.ObserveStress(t.Context(), scope, both)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[rpc.AlertKind]rpc.AlertEpisodeState{}
	for _, candidate := range dual.Candidates {
		kinds[candidate.Kind] = candidate.State
	}
	if len(dual.Candidates) != 2 || kinds[rpc.AlertKindPortfolioRisk] != rpc.AlertEpisodeOpen || kinds[rpc.AlertKindMarginSafety] != rpc.AlertEpisodeOpen {
		t.Fatalf("coexistence candidates=%+v", dual.Candidates)
	}
}
