package risk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEvaluateReconcileDueRollingBoundary(t *testing.T) {
	deadline := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	warning := 2
	for _, tc := range []struct {
		name  string
		now   time.Time
		state string
	}{
		{"before rolling window", deadline.Add(-48*time.Hour - time.Nanosecond), ""},
		{"at rolling window", deadline.Add(-48 * time.Hour), NudgeStateDueSoon},
		{"at deadline is still due soon", deadline, NudgeStateDueSoon},
		{"after deadline is overdue", deadline.Add(time.Nanosecond), NudgeStateOverdue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateReconcileDue(ReconcileDueInput{Now: tc.now, Deadline: deadline, WarningDays: &warning})
			if tc.state == "" {
				if got != nil {
					t.Fatalf("candidate = %#v, want nil", got)
				}
				return
			}
			if got == nil || got.State != tc.state || got.DueAt != deadline {
				t.Fatalf("candidate = %#v, want state %s and deadline", got, tc.state)
			}
		})
	}
	if got := EvaluateReconcileDue(ReconcileDueInput{Now: deadline, Deadline: deadline}); got != nil {
		t.Fatalf("unapproved warning horizon yielded candidate: %#v", got)
	}

	dueSoon := EvaluateReconcileDue(ReconcileDueInput{Now: deadline, Deadline: deadline, WarningDays: &warning})
	dueSoonEarlier := EvaluateReconcileDue(ReconcileDueInput{Now: deadline.Add(-time.Hour), Deadline: deadline, WarningDays: &warning})
	if dueSoon.Fingerprint != dueSoonEarlier.Fingerprint {
		t.Fatal("current time changed stable reconcile_due identity")
	}
	overdue := EvaluateReconcileDue(ReconcileDueInput{Now: deadline.Add(time.Second), Deadline: deadline, WarningDays: &warning})
	if dueSoon.Fingerprint == overdue.Fingerprint {
		t.Fatal("due state must be part of reconcile_due identity")
	}
	warning = 3
	changed := EvaluateReconcileDue(ReconcileDueInput{Now: deadline, Deadline: deadline, WarningDays: &warning})
	if dueSoon.Fingerprint == changed.Fingerprint {
		t.Fatal("warning horizon must be part of reconcile_due identity")
	}
}

func TestNudgeCandidateIdentitiesAndResolution(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	exceptionsA := []ReconcileExceptionIdentity{
		{Kind: " date ", Identity: "ROW-B", Material: []string{" later ", "EUR"}},
		{Kind: "amount", Identity: "ROW-A", Material: []string{"outside-bound"}},
	}
	exceptionsB := []ReconcileExceptionIdentity{exceptionsA[1], exceptionsA[0]}
	a := EvaluateReconcileException(exceptionsA, now)
	b := EvaluateReconcileException(exceptionsB, now)
	if a == nil || b == nil || a.Fingerprint != b.Fingerprint {
		t.Fatalf("normalized exception identity is not order-stable: %#v %#v", a, b)
	}
	if got := EvaluateReconcileException(nil, now); got != nil {
		t.Fatalf("resolved exceptions yielded candidate: %#v", got)
	}
	if laterOccurrence := EvaluateReconcileException(exceptionsA, now.Add(time.Hour)); laterOccurrence.Fingerprint != a.Fingerprint {
		t.Fatal("exception occurrence time changed semantic identity")
	}
	exceptionsB[0].Material = []string{"different"}
	if changed := EvaluateReconcileException(exceptionsB, now); changed.Fingerprint == a.Fingerprint {
		t.Fatal("material exception field did not change identity")
	}

	first := EvaluateShadowWouldBlock(ShadowWouldBlockInput{
		PolicyFingerprint: "policy-secret", LatchEpisode: "latch-secret",
		RiskIncreasing: true, WouldBlock: true, PriorCount: 0, OccurredAt: now,
	})
	if first.Candidate == nil || first.Count != 1 {
		t.Fatalf("first qualifying preview = %#v, want candidate and count 1", first)
	}
	firstAgain := EvaluateShadowWouldBlock(ShadowWouldBlockInput{
		PolicyFingerprint: "policy-secret", LatchEpisode: "latch-secret",
		RiskIncreasing: true, WouldBlock: true, PriorCount: 0, OccurredAt: now.Add(time.Hour),
	})
	if firstAgain.Candidate.Fingerprint != first.Candidate.Fingerprint {
		t.Fatal("preview occurrence time changed episode identity")
	}
	later := EvaluateShadowWouldBlock(ShadowWouldBlockInput{
		PolicyFingerprint: "policy-secret", LatchEpisode: "latch-secret",
		RiskIncreasing: true, WouldBlock: true, PriorCount: first.Count, OccurredAt: now.Add(time.Minute),
	})
	if later.Candidate != nil || later.Count != 2 {
		t.Fatalf("later qualifying preview = %#v, want count-only", later)
	}
	if first.Candidate.Fingerprint != firstAgain.Candidate.Fingerprint {
		t.Fatal("episode count must not affect semantic fingerprint")
	}
	exempt := EvaluateShadowWouldBlock(ShadowWouldBlockInput{
		PolicyFingerprint: "policy-secret", LatchEpisode: "latch-secret",
		RiskIncreasing: true, Exempt: true, WouldBlock: true, PriorCount: later.Count, OccurredAt: now,
	})
	if exempt.Candidate != nil || exempt.Count != later.Count {
		t.Fatalf("exempt preview changed episode: %#v", exempt)
	}
	for name, input := range map[string]ShadowWouldBlockInput{
		"not risk increasing": {PolicyFingerprint: "policy", LatchEpisode: "episode", WouldBlock: true},
		"would not block":     {PolicyFingerprint: "policy", LatchEpisode: "episode", RiskIncreasing: true},
		"missing episode":     {PolicyFingerprint: "policy", RiskIncreasing: true, WouldBlock: true},
	} {
		if got := EvaluateShadowWouldBlock(input); got.Candidate != nil || got.Count != 0 {
			t.Errorf("%s preview = %#v, want no candidate/count", name, got)
		}
	}

	latch := EvaluateDrawdownLatched("latch-secret", true, now)
	if latch == nil || EvaluateDrawdownLatched("latch-secret", false, now) != nil {
		t.Fatal("drawdown latch open/closed contract violated")
	}
	if laterLatch := EvaluateDrawdownLatched("latch-secret", true, now.Add(time.Hour)); laterLatch.Fingerprint != latch.Fingerprint {
		t.Fatal("latch occurrence time changed episode identity")
	}

	mismatches := []NudgePinMismatch{
		{Policy: "rulebook", PinnedID: "a", PinnedVersion: "1", LiveID: "b", LiveVersion: "2"},
		{Policy: "stress", PinnedID: "c", PinnedVersion: "1", LiveID: "d", LiveVersion: "2"},
	}
	driftA := EvaluatePolicyDrift(mismatches, now)
	driftB := EvaluatePolicyDrift([]NudgePinMismatch{mismatches[1], mismatches[0]}, now)
	if driftA == nil || driftB == nil || driftA.Fingerprint != driftB.Fingerprint {
		t.Fatalf("policy drift identity is not sorted: %#v %#v", driftA, driftB)
	}
	if got := EvaluatePolicyDrift(nil, now); got != nil {
		t.Fatalf("matching pins yielded candidate: %#v", got)
	}
	if got := EvaluatePolicyDrift([]NudgePinMismatch{
		{Policy: "rulebook", PinnedID: "same", PinnedVersion: "1", LiveID: "same", LiveVersion: "1"},
		{},
	}, now); got != nil {
		t.Fatalf("matching/incomplete pin rows yielded candidate: %#v", got)
	}
	if laterDrift := EvaluatePolicyDrift(mismatches, now.Add(time.Hour)); laterDrift.Fingerprint != driftA.Fingerprint {
		t.Fatal("drift occurrence time changed normalized mismatch identity")
	}
	changedMismatch := append([]NudgePinMismatch(nil), mismatches...)
	changedMismatch[0].LiveVersion = "3"
	if changed := EvaluatePolicyDrift(changedMismatch, now); changed.Fingerprint == driftA.Fingerprint {
		t.Fatal("changed pin mismatch did not change identity")
	}

	flow := EvaluateConfirmedFlow("statement-row-secret", now)
	if flow == nil {
		t.Fatal("confirmed flow did not yield candidate")
	}
	if laterFlow := EvaluateConfirmedFlow("statement-row-secret", now.Add(time.Hour)); laterFlow.Fingerprint != flow.Fingerprint {
		t.Fatal("confirmed flow occurrence time changed row identity")
	}
	if got := EvaluateConfirmedFlow(" ", now); got != nil {
		t.Fatalf("empty confirmed-flow identity yielded candidate: %#v", got)
	}
}

func TestEvaluateMonthlyPulseLocalMonthDSTAndReopen(t *testing.T) {
	cadence := approvedV4Constitution().Cadence
	policyFingerprint := "policy-fingerprint-secret"

	// Berlin has moved to CEST: 07:00Z is 09:00 local on April 1.
	aprilDue := time.Date(2026, 4, 1, 7, 0, 0, 0, time.UTC)
	before := EvaluateMonthlyPulse(MonthlyPulseInput{Now: aprilDue.Add(-time.Nanosecond), Cadence: cadence, PolicyFingerprint: policyFingerprint})
	if before.Status != MonthlyPulseStatusNotDue || before.Candidate != nil || before.Month != "2026-04" {
		t.Fatalf("before April due = %#v", before)
	}
	due := EvaluateMonthlyPulse(MonthlyPulseInput{Now: aprilDue, Cadence: cadence, PolicyFingerprint: policyFingerprint, PolicyEvidenceReady: true})
	if due.Status != MonthlyPulseStatusDue || due.Candidate == nil || !due.DueAt.Equal(aprilDue) {
		t.Fatalf("April due = %#v", due)
	}
	laterApril := EvaluateMonthlyPulse(MonthlyPulseInput{Now: aprilDue.Add(10 * 24 * time.Hour), Cadence: cadence, PolicyFingerprint: policyFingerprint, PolicyEvidenceReady: true})
	if laterApril.Candidate == nil || laterApril.Candidate.Fingerprint != due.Candidate.Fingerprint {
		t.Fatal("wall-clock movement changed local-month identity")
	}

	// Berlin is back on CET: the same 09:00 local policy is 08:00Z.
	novemberDue := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	november := EvaluateMonthlyPulse(MonthlyPulseInput{Now: novemberDue, Cadence: cadence, PolicyFingerprint: policyFingerprint, PolicyEvidenceReady: true})
	if november.Status != MonthlyPulseStatusDue || !november.DueAt.Equal(novemberDue) || november.Month != "2026-11" {
		t.Fatalf("November due across DST = %#v", november)
	}

	completion := &MonthlyPulseCompletion{
		Month: "2026-11", PolicyFingerprint: policyFingerprint,
		CompletedAt: novemberDue, Evidence: MonthlyPulseEvidenceRender,
	}
	completed := EvaluateMonthlyPulse(MonthlyPulseInput{Now: novemberDue, Cadence: cadence, PolicyFingerprint: policyFingerprint, PolicyEvidenceReady: true, Completion: completion})
	if completed.Status != MonthlyPulseStatusCompleted || completed.Candidate != nil {
		t.Fatalf("completed month = %#v", completed)
	}
	reopened := EvaluateMonthlyPulse(MonthlyPulseInput{Now: novemberDue, Cadence: cadence, PolicyFingerprint: "new-policy", PolicyEvidenceReady: true, Completion: completion})
	if reopened.Status != MonthlyPulseStatusDue || reopened.Candidate == nil || reopened.Candidate.Fingerprint == november.Candidate.Fingerprint {
		t.Fatalf("policy change did not reopen month: %#v", reopened)
	}

	blockedCadence := cadence
	blockedCadence.Monthly.NudgeAtLocal = nil
	blocked := EvaluateMonthlyPulse(MonthlyPulseInput{Now: novemberDue, Cadence: blockedCadence, PolicyFingerprint: policyFingerprint})
	if blocked.Status != MonthlyPulseStatusBlocked || blocked.Candidate != nil {
		t.Fatalf("unapproved cadence = %#v, want blocked without candidate", blocked)
	}
}

func TestEvaluateMonthlyPulseRejectsAmbiguousClocksAndHostTimezone(t *testing.T) {
	base := approvedV4Constitution().Cadence
	for _, zone := range []string{"Local", "local", " Europe/Berlin", "Europe/Berlin "} {
		cadence := base
		nudges := *base.Nudges
		cadence.Nudges = &nudges
		cadence.Nudges.Timezone = &zone
		got := EvaluateMonthlyPulse(MonthlyPulseInput{
			Now: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), Cadence: cadence,
			PolicyFingerprint: "policy", PolicyEvidenceReady: true,
		})
		if got.Status != MonthlyPulseStatusBlocked {
			t.Errorf("timezone %q status = %q, want blocked", zone, got.Status)
		}
	}

	for _, tc := range []struct {
		name string
		now  time.Time
		day  int
	}{
		// Europe/Berlin skips 02:00-02:59 on 2027-03-28.
		{"skipped wall time", time.Date(2027, 3, 28, 12, 0, 0, 0, time.UTC), 28},
		// Europe/Berlin repeats 02:00-02:59 on 2026-10-25.
		{"repeated wall time", time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cadence := base
			monthly := *base.Monthly
			cadence.Monthly = &monthly
			cadence.Monthly.DayOfMonth = &tc.day
			cadence.Monthly.NudgeAtLocal = new("02:30")
			got := EvaluateMonthlyPulse(MonthlyPulseInput{
				Now: tc.now, Cadence: cadence, PolicyFingerprint: "policy", PolicyEvidenceReady: true,
			})
			if got.Status != MonthlyPulseStatusBlocked || got.Candidate != nil {
				t.Fatalf("ambiguous wall time = %#v, want blocked", got)
			}
		})
	}
}

func TestEvaluateMonthlyPulseCompletionEvidenceBoundaries(t *testing.T) {
	cadence := approvedV4Constitution().Cadence
	dueAt := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	now := dueAt.Add(time.Hour)
	base := MonthlyPulseInput{
		Now: now, Cadence: cadence, PolicyFingerprint: "policy", PolicyEvidenceReady: true,
	}
	valid := MonthlyPulseCompletion{
		Month: "2026-11", PolicyFingerprint: "policy", CompletedAt: dueAt, Evidence: MonthlyPulseEvidenceRender,
	}
	for _, tc := range []struct {
		name       string
		completion MonthlyPulseCompletion
		want       string
	}{
		{"at due", valid, MonthlyPulseStatusCompleted},
		{"at now", func() MonthlyPulseCompletion { c := valid; c.CompletedAt = now; return c }(), MonthlyPulseStatusCompleted},
		{"pre due", func() MonthlyPulseCompletion { c := valid; c.CompletedAt = dueAt.Add(-time.Nanosecond); return c }(), MonthlyPulseStatusDue},
		{"future", func() MonthlyPulseCompletion { c := valid; c.CompletedAt = now.Add(time.Nanosecond); return c }(), MonthlyPulseStatusDue},
		{"missing time", func() MonthlyPulseCompletion { c := valid; c.CompletedAt = time.Time{}; return c }(), MonthlyPulseStatusDue},
		{"other evidence", func() MonthlyPulseCompletion { c := valid; c.Evidence = "explicit"; return c }(), MonthlyPulseStatusDue},
		{"wrong month", func() MonthlyPulseCompletion { c := valid; c.Month = "2026-10"; return c }(), MonthlyPulseStatusDue},
		{"wrong policy", func() MonthlyPulseCompletion { c := valid; c.PolicyFingerprint = "old"; return c }(), MonthlyPulseStatusDue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			input.Completion = &tc.completion
			if got := EvaluateMonthlyPulse(input); got.Status != tc.want {
				t.Fatalf("status = %q, want %q (%#v)", got.Status, tc.want, got)
			}
		})
	}

	beforeDue := base
	beforeDue.Now = dueAt.Add(-time.Nanosecond)
	beforeDue.PolicyEvidenceReady = false
	if got := EvaluateMonthlyPulse(beforeDue); got.Status != MonthlyPulseStatusNotDue {
		t.Fatalf("before due with unreadable evidence = %q, want not_due", got.Status)
	}
	for _, at := range []time.Time{dueAt, now} {
		blocked := base
		blocked.Now = at
		blocked.PolicyEvidenceReady = false
		blocked.Completion = &valid
		if got := EvaluateMonthlyPulse(blocked); got.Status != MonthlyPulseStatusBlocked || got.Candidate != nil {
			t.Fatalf("at/after due without policy evidence = %#v, want blocked", got)
		}
	}
}

func TestNudgeCandidatesUseOnlySafeTemplateCopy(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	warning := 2
	deadline := now.Add(time.Hour)
	candidates := []struct {
		candidate        *NudgeCandidate
		kind, state      string
		severity, target string
	}{
		{EvaluateReconcileDue(ReconcileDueInput{Now: now, Deadline: deadline, WarningDays: &warning}), NudgeKindReconcileDue, NudgeStateDueSoon, NudgeSeverityWatch, NudgeDestinationBrief},
		{EvaluateReconcileDue(ReconcileDueInput{Now: deadline.Add(time.Second), Deadline: deadline, WarningDays: &warning}), NudgeKindReconcileDue, NudgeStateOverdue, NudgeSeverityAct, NudgeDestinationAlerts},
		{EvaluateReconcileException([]ReconcileExceptionIdentity{{Kind: "amount", Identity: "account-secret", Material: []string{"EUR 12345", "/private/report.xml"}}}, now), NudgeKindReconcileException, NudgeStateOpen, NudgeSeverityAct, NudgeDestinationAlerts},
		{EvaluateShadowWouldBlock(ShadowWouldBlockInput{PolicyFingerprint: "raw-upstream-fingerprint", LatchEpisode: "order-secret", RiskIncreasing: true, WouldBlock: true, OccurredAt: now}).Candidate, NudgeKindShadowWouldBlock, NudgeStateObserved, NudgeSeverityAct, NudgeDestinationAlerts},
		{EvaluateDrawdownLatched("latch-secret", true, now), NudgeKindDrawdownLatched, NudgeStateOpen, NudgeSeverityAct, NudgeDestinationAlerts},
		{EvaluatePolicyDrift([]NudgePinMismatch{{Policy: "secret-symbol", PinnedID: "pin-secret", PinnedVersion: "1", LiveID: "live-secret", LiveVersion: "2"}}, now), NudgeKindPolicyDrift, NudgeStateOpen, NudgeSeverityAct, NudgeDestinationAlerts},
		{EvaluateConfirmedFlow("statement-row-secret", now), NudgeKindConfirmedFlow, NudgeStateObserved, NudgeSeverityWatch, NudgeDestinationBrief},
		{EvaluateMonthlyPulse(MonthlyPulseInput{Now: time.Date(2026, 7, 1, 7, 0, 0, 0, time.UTC), Cadence: approvedV4Constitution().Cadence, PolicyFingerprint: "policy-secret", PolicyEvidenceReady: true}).Candidate, NudgeKindMonthlyPulse, NudgeStateDue, NudgeSeverityWatch, NudgeDestinationBrief},
	}
	serializedCandidates := make([]*NudgeCandidate, 0, len(candidates))
	for _, tc := range candidates {
		candidate := tc.candidate
		if candidate == nil {
			t.Fatal("expected candidate")
		}
		if candidate.Title == "" || candidate.Body == "" || candidate.Fingerprint == "" {
			t.Fatalf("candidate lacks template copy or identity: %#v", candidate)
		}
		if candidate.Kind != tc.kind || candidate.State != tc.state || candidate.Severity != tc.severity || candidate.Destination != tc.target {
			t.Fatalf("candidate enums = %#v, want %s/%s/%s/%s", candidate, tc.kind, tc.state, tc.severity, tc.target)
		}
		serializedCandidates = append(serializedCandidates, candidate)
	}
	raw, err := json.Marshal(serializedCandidates)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(raw)
	if strings.Contains(serialized, `"count"`) {
		t.Fatalf("candidate contract exposed an internal occurrence count: %s", serialized)
	}
	for _, secret := range []string{
		"account-secret", "EUR 12345", "/private/report.xml", "raw-upstream-fingerprint",
		"order-secret", "latch-secret", "secret-symbol", "pin-secret", "live-secret",
		"statement-row-secret", "policy-secret",
	} {
		if strings.Contains(serialized, secret) {
			t.Errorf("candidate leaked raw input %q: %s", secret, serialized)
		}
	}
}

func TestCanonicalizeNudgeCandidateUsesOnlyApprovedSemantics(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	fingerprint := "sha256:" + strings.Repeat("a", 64)
	tests := []NudgeCandidate{
		{Fingerprint: fingerprint, Kind: NudgeKindReconcileDue, State: NudgeStateDueSoon, OccurredAt: now, DueAt: now.Add(time.Hour)},
		{Fingerprint: fingerprint, Kind: NudgeKindReconcileDue, State: NudgeStateOverdue, OccurredAt: now, DueAt: now},
		{Fingerprint: fingerprint, Kind: NudgeKindReconcileException, State: NudgeStateOpen, OccurredAt: now},
		{Fingerprint: fingerprint, Kind: NudgeKindShadowWouldBlock, State: NudgeStateObserved, OccurredAt: now},
		{Fingerprint: fingerprint, Kind: NudgeKindDrawdownLatched, State: NudgeStateOpen, OccurredAt: now},
		{Fingerprint: fingerprint, Kind: NudgeKindPolicyDrift, State: NudgeStateOpen, OccurredAt: now},
		{Fingerprint: fingerprint, Kind: NudgeKindConfirmedFlow, State: NudgeStateObserved, OccurredAt: now},
		{Fingerprint: fingerprint, Kind: NudgeKindMonthlyPulse, State: NudgeStateDue, OccurredAt: now, DueAt: now},
	}
	for _, candidate := range tests {
		candidate.Title = "fixture-private-title"
		candidate.Body = "fixture-private-body"
		candidate.Severity = "caller-selected"
		candidate.Destination = "caller-selected"
		got, err := CanonicalizeNudgeCandidate(candidate)
		if err != nil {
			t.Fatalf("canonicalize %s/%s: %v", candidate.Kind, candidate.State, err)
		}
		title, body, severity := candidateTemplate(candidate.Kind, candidate.State)
		if got.Title != title || got.Body != body || got.Severity != severity {
			t.Fatalf("canonical %s/%s copy = %#v", candidate.Kind, candidate.State, got)
		}
		wantDestination := NudgeDestinationBrief
		if severity == NudgeSeverityAct {
			wantDestination = NudgeDestinationAlerts
		}
		if got.Destination != wantDestination {
			t.Fatalf("canonical %s/%s destination = %q, want %q", candidate.Kind, candidate.State, got.Destination, wantDestination)
		}
	}
}

func TestCanonicalizeNudgeCandidateRejectsInvalidStructure(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	fingerprint := "sha256:" + strings.Repeat("a", 64)
	valid := NudgeCandidate{
		Fingerprint: fingerprint, Kind: NudgeKindPolicyDrift, State: NudgeStateOpen, OccurredAt: now,
	}
	for _, tc := range []struct {
		name      string
		candidate NudgeCandidate
	}{
		{"short fingerprint", func() NudgeCandidate { c := valid; c.Fingerprint = "sha256:a"; return c }()},
		{"uppercase fingerprint", func() NudgeCandidate { c := valid; c.Fingerprint = "sha256:" + strings.Repeat("A", 64); return c }()},
		{"nonhex fingerprint", func() NudgeCandidate { c := valid; c.Fingerprint = "sha256:" + strings.Repeat("g", 64); return c }()},
		{"wrong fingerprint prefix", func() NudgeCandidate { c := valid; c.Fingerprint = "digest:" + strings.Repeat("a", 64); return c }()},
		{"invalid kind", func() NudgeCandidate { c := valid; c.Kind = "caller_kind"; return c }()},
		{"invalid kind state", func() NudgeCandidate { c := valid; c.State = NudgeStateDue; return c }()},
		{"missing occurrence", func() NudgeCandidate { c := valid; c.OccurredAt = time.Time{}; return c }()},
		{"unexpected due", func() NudgeCandidate { c := valid; c.DueAt = now; return c }()},
		{"unexpected expiry", func() NudgeCandidate { c := valid; c.ExpiresAt = now; return c }()},
		{"due soon missing due", NudgeCandidate{Fingerprint: fingerprint, Kind: NudgeKindReconcileDue, State: NudgeStateDueSoon, OccurredAt: now}},
		{"due soon after deadline", NudgeCandidate{Fingerprint: fingerprint, Kind: NudgeKindReconcileDue, State: NudgeStateDueSoon, OccurredAt: now, DueAt: now.Add(-time.Nanosecond)}},
		{"overdue occurrence mismatch", NudgeCandidate{Fingerprint: fingerprint, Kind: NudgeKindReconcileDue, State: NudgeStateOverdue, OccurredAt: now, DueAt: now.Add(-time.Nanosecond)}},
		{"monthly missing due", NudgeCandidate{Fingerprint: fingerprint, Kind: NudgeKindMonthlyPulse, State: NudgeStateDue, OccurredAt: now}},
		{"monthly occurrence mismatch", NudgeCandidate{Fingerprint: fingerprint, Kind: NudgeKindMonthlyPulse, State: NudgeStateDue, OccurredAt: now, DueAt: now.Add(time.Nanosecond)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := CanonicalizeNudgeCandidate(tc.candidate); err == nil {
				t.Fatalf("canonical candidate = %#v, want error", got)
			}
		})
	}
}
