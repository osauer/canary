package risk_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func readyNudgeHealth(at time.Time) rpc.NudgeSourceHealth {
	ok := func() rpc.NudgeInputHealth {
		return rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: at}
	}
	return rpc.NudgeSourceHealth{
		Policy: ok(), Reconciliation: ok(), Capital: ok(), Pins: ok(), Cadence: ok(), ConfirmedFlow: ok(),
	}
}

func reviewedNudgeCoverage(at time.Time) *rpc.NudgeConfirmedFlowCoverage {
	return &rpc.NudgeConfirmedFlowCoverage{CoverageFrom: at}
}

func validRPCNudgeCandidate(at time.Time) rpc.NudgeCandidate {
	return rpc.NudgeCandidate{
		Fingerprint: "sha256:" + strings.Repeat("a", 64),
		Kind:        rpc.NudgeKindPolicyDrift,
		State:       rpc.NudgeStateOpen,
		OccurredAt:  at,
	}
}

func TestNudgeSourceHealthReadyEmptyVersusSuppressedEmpty(t *testing.T) {
	at := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	ready := readyNudgeHealth(at)
	ready.Aggregate = rpc.AggregateNudgeSourceHealth(ready, 0)
	readyResult := rpc.NudgesSnapshotResult{
		AsOf: at, Candidates: []rpc.NudgeCandidate{}, SourceHealth: ready,
		ConfirmedFlowCoverage: reviewedNudgeCoverage(at),
	}
	if ready.Aggregate != rpc.NudgeAggregateReady || !readyResult.IsCleanEmpty() {
		t.Fatalf("ready empty result = %#v, want clean", readyResult)
	}

	suppressed := readyNudgeHealth(at)
	suppressed.Cadence = rpc.NudgeInputHealth{
		Status: rpc.NudgeInputStatusUnapproved, Reason: rpc.NudgeHealthReasonCadenceUnapproved, AsOf: at,
	}
	suppressed.Aggregate = rpc.AggregateNudgeSourceHealth(suppressed, 0)
	suppressedResult := rpc.NudgesSnapshotResult{
		AsOf: at, Candidates: []rpc.NudgeCandidate{}, SourceHealth: suppressed,
		ConfirmedFlowCoverage: reviewedNudgeCoverage(at),
	}
	if suppressed.Aggregate != rpc.NudgeAggregateSuppressed || suppressedResult.IsCleanEmpty() {
		t.Fatalf("suppressed empty result = %#v, must not reassure", suppressedResult)
	}

	degraded := suppressed
	degraded.Aggregate = rpc.AggregateNudgeSourceHealth(degraded, 1)
	if degraded.Aggregate != rpc.NudgeAggregateDegraded {
		t.Fatalf("partially covered result aggregate = %q, want degraded", degraded.Aggregate)
	}
}

func TestNudgeRPCWireShapeIsRestricted(t *testing.T) {
	candidate := validRPCNudgeCandidate(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	candidate.Severity = rpc.NudgeSeverityAct
	candidate.Title = "Policy pins need review"
	candidate.Body = "Review the policy pin status."
	candidate.Destination = rpc.NudgeDestinationAlerts
	raw, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"body", "destination", "fingerprint", "kind", "occurred_at", "severity", "state", "title"}
	got := make([]string, 0, len(fields))
	for key := range fields {
		got = append(got, key)
	}
	slicesSort(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate JSON fields = %v, want restricted %v", got, want)
	}
}

func TestNudgeSnapshotMissingAsOfFailsClosed(t *testing.T) {
	at := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	result := rpc.NudgesSnapshotResult{
		Candidates:   []rpc.NudgeCandidate{},
		SourceHealth: readyNudgeHealth(at),
	}
	if _, err := json.Marshal(result); err == nil {
		t.Fatal("zero-as-of snapshot serialized without error")
	}
	if result.IsCleanEmpty() {
		t.Fatal("zero-as-of snapshot reported clean empty")
	}
}

func TestNudgeSnapshotCanonicalizesCandidateCopy(t *testing.T) {
	at := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	candidate := validRPCNudgeCandidate(at)
	candidate.Title = "fixture-private-title"
	candidate.Body = "fixture-private-body"
	candidate.Severity = "caller-selected"
	candidate.Destination = "caller-selected"
	result := rpc.NudgesSnapshotResult{
		AsOf: at, Candidates: []rpc.NudgeCandidate{candidate}, SourceHealth: readyNudgeHealth(at),
		ConfirmedFlowCoverage: reviewedNudgeCoverage(at),
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, hostile := range []string{candidate.Title, candidate.Body, candidate.Severity, candidate.Destination} {
		if strings.Contains(string(raw), hostile) {
			t.Fatalf("caller-authored candidate text reached wire: %s", raw)
		}
	}
	var got rpc.NudgesSnapshotResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want, err := risk.CanonicalizeNudgeCandidate(risk.NudgeCandidate{
		Fingerprint: candidate.Fingerprint, Kind: candidate.Kind, State: candidate.State, OccurredAt: candidate.OccurredAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Title != want.Title || got.Candidates[0].Body != want.Body || got.Candidates[0].Severity != want.Severity || got.Candidates[0].Destination != want.Destination {
		t.Fatalf("wire candidate = %#v, want canonical %#v", got.Candidates, want)
	}
}

func TestNudgeSnapshotRejectsStructurallyInvalidCandidates(t *testing.T) {
	at := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	valid := validRPCNudgeCandidate(at)
	for _, tc := range []struct {
		name      string
		candidate rpc.NudgeCandidate
	}{
		{"malformed fingerprint", func() rpc.NudgeCandidate { c := valid; c.Fingerprint = "sha256:bad"; return c }()},
		{"uppercase fingerprint", func() rpc.NudgeCandidate { c := valid; c.Fingerprint = "sha256:" + strings.Repeat("A", 64); return c }()},
		{"invalid kind", func() rpc.NudgeCandidate { c := valid; c.Kind = "caller_kind"; return c }()},
		{"invalid kind state", func() rpc.NudgeCandidate { c := valid; c.State = rpc.NudgeStateDue; return c }()},
		{"missing occurrence", func() rpc.NudgeCandidate { c := valid; c.OccurredAt = time.Time{}; return c }()},
		{"unexpected due", func() rpc.NudgeCandidate { c := valid; c.DueAt = at; return c }()},
		{"unexpected expiry", func() rpc.NudgeCandidate { c := valid; c.ExpiresAt = at; return c }()},
		{"monthly missing due", rpc.NudgeCandidate{Fingerprint: valid.Fingerprint, Kind: rpc.NudgeKindMonthlyPulse, State: rpc.NudgeStateDue, OccurredAt: at}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := rpc.NudgesSnapshotResult{
				AsOf: at, Candidates: []rpc.NudgeCandidate{tc.candidate}, SourceHealth: readyNudgeHealth(at),
				ConfirmedFlowCoverage: reviewedNudgeCoverage(at),
			}
			if raw, err := json.Marshal(result); err == nil {
				t.Fatalf("invalid candidate serialized: %s", raw)
			}
		})
	}
}

func TestMonthlyBriefAndAckFoundationContract(t *testing.T) {
	due := time.Date(2026, 8, 1, 7, 0, 0, 0, time.UTC)
	row := &rpc.BriefMonthlyPulseRow{Status: rpc.BriefMonthlyPulseDue, Month: "2026-08", DueAt: due}
	process := rpc.BriefProcessSection{MonthlyPulse: row}
	if process.MonthlyPulse.Status != rpc.BriefMonthlyPulseDue {
		t.Fatalf("monthly pulse status = %q", process.MonthlyPulse.Status)
	}
	ack := rpc.BriefAckParams{
		Kind: rpc.BriefKindMonthly, BriefFingerprint: "brief-fp", Month: "2026-08",
		Evidence: rpc.BriefAckEvidenceRender, Origin: "paired-device",
	}
	if ack.Kind != rpc.BriefKindMonthly || ack.Evidence != rpc.BriefAckEvidenceRender {
		t.Fatalf("monthly ack is not paired-render evidence: %#v", ack)
	}
}

func TestNudgeSnapshotJSONNormalizesSourceHealth(t *testing.T) {
	at := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	decode := func(t *testing.T, result rpc.NudgesSnapshotResult) rpc.NudgesSnapshotResult {
		t.Helper()
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) == "" {
			t.Fatal("empty JSON")
		}
		var decoded rpc.NudgesSnapshotResult
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}

	t.Run("inconsistent aggregate", func(t *testing.T) {
		for _, inputHealth := range []rpc.NudgeInputHealth{
			{Status: rpc.NudgeInputStatusUnavailable, Reason: rpc.NudgeHealthReasonSourceUnavailable, AsOf: at},
			{Status: rpc.NudgeInputStatusUnapproved, Reason: rpc.NudgeHealthReasonCadenceUnapproved, AsOf: at},
			{Status: rpc.NudgeInputStatusStale, Reason: rpc.NudgeHealthReasonEvidenceStale, AsOf: at},
		} {
			health := readyNudgeHealth(at)
			health.Aggregate = rpc.NudgeAggregateReady
			health.Cadence = inputHealth
			input := rpc.NudgesSnapshotResult{
				AsOf: at, Candidates: []rpc.NudgeCandidate{}, SourceHealth: health,
				ConfirmedFlowCoverage: reviewedNudgeCoverage(at),
			}
			got := decode(t, input)
			if got.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed || input.IsCleanEmpty() {
				t.Fatalf("normalized health = %#v, false ready survived", got.SourceHealth)
			}
			rawHealth, err := json.Marshal(health)
			if err != nil {
				t.Fatal(err)
			}
			var standalone rpc.NudgeSourceHealth
			if err := json.Unmarshal(rawHealth, &standalone); err != nil {
				t.Fatal(err)
			}
			if standalone.Aggregate != rpc.NudgeAggregateSuppressed {
				t.Fatalf("standalone false-ready health serialized as %q", standalone.Aggregate)
			}
		}
	})

	t.Run("zero as of", func(t *testing.T) {
		health := readyNudgeHealth(at)
		health.Aggregate = rpc.NudgeAggregateReady
		health.Pins.AsOf = time.Time{}
		got := decode(t, rpc.NudgesSnapshotResult{
			AsOf: at, Candidates: []rpc.NudgeCandidate{}, SourceHealth: health,
			ConfirmedFlowCoverage: reviewedNudgeCoverage(at),
		})
		if got.SourceHealth.Pins.Status != rpc.NudgeInputStatusError || got.SourceHealth.Pins.Reason != rpc.NudgeHealthReasonInvalid || !got.SourceHealth.Pins.AsOf.IsZero() {
			t.Fatalf("zero-as-of health = %#v, want safe error without fabricated timestamp", got.SourceHealth.Pins)
		}
		if got.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed {
			t.Fatalf("zero-as-of aggregate = %q", got.SourceHealth.Aggregate)
		}
	})

	for _, tc := range []struct {
		name   string
		status string
		reason string
	}{
		{"raw status", "totally_ready", rpc.NudgeHealthReasonNone},
		{"raw reason", rpc.NudgeInputStatusUnavailable, "open /private/account.log"},
		{"mismatched reason", rpc.NudgeInputStatusOK, rpc.NudgeHealthReasonSourceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := readyNudgeHealth(at)
			health.Aggregate = rpc.NudgeAggregateReady
			health.Policy = rpc.NudgeInputHealth{Status: tc.status, Reason: tc.reason, AsOf: at}
			result := rpc.NudgesSnapshotResult{
				AsOf: at, Candidates: []rpc.NudgeCandidate{}, SourceHealth: health,
				ConfirmedFlowCoverage: reviewedNudgeCoverage(at),
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), tc.status) && tc.status != rpc.NudgeInputStatusOK && tc.status != rpc.NudgeInputStatusUnavailable {
				t.Fatalf("raw status survived normalization: %s", raw)
			}
			if tc.reason != "" && strings.Contains(string(raw), tc.reason) {
				t.Fatalf("raw/mismatched reason survived normalization: %s", raw)
			}
			var got rpc.NudgesSnapshotResult
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.SourceHealth.Policy.Status != rpc.NudgeInputStatusError || got.SourceHealth.Policy.Reason != rpc.NudgeHealthReasonInvalid || got.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed || result.IsCleanEmpty() {
				t.Fatalf("unsafe health was not normalized: %#v", got.SourceHealth)
			}
		})
	}

	t.Run("candidate count degrades partial coverage", func(t *testing.T) {
		health := readyNudgeHealth(at)
		health.Cadence = rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusStale, Reason: rpc.NudgeHealthReasonEvidenceStale, AsOf: at}
		got := decode(t, rpc.NudgesSnapshotResult{
			AsOf: at, Candidates: []rpc.NudgeCandidate{validRPCNudgeCandidate(at)},
			SourceHealth:          health,
			ConfirmedFlowCoverage: reviewedNudgeCoverage(at),
		})
		if got.SourceHealth.Aggregate != rpc.NudgeAggregateDegraded {
			t.Fatalf("partial candidate aggregate = %q, want degraded", got.SourceHealth.Aggregate)
		}
	})
}

func slicesSort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
