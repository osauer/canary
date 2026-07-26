package live

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCloneSnapshotDeepCopiesNudgeReconciliation(t *testing.T) {
	t.Parallel()
	status := rpc.ReconAutomationStatus{
		Report:     rpc.ReconFetchStatus{State: rpc.ReconReportStateCurrent},
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateComplete},
	}
	original := Snapshot{Nudges: &rpc.NudgesSnapshotResult{Reconciliation: &status}}

	cloned := cloneSnapshot(original)
	if cloned.Nudges == nil || cloned.Nudges.Reconciliation == nil {
		t.Fatalf("clone lost reconciliation: %+v", cloned.Nudges)
	}
	if cloned.Nudges == original.Nudges || cloned.Nudges.Reconciliation == original.Nudges.Reconciliation {
		t.Fatal("clone retained a mutable reconciliation pointer")
	}
	cloned.Nudges.Reconciliation.Report.State = rpc.ReconReportStateUnavailable
	cloned.Nudges.Reconciliation.Evaluation.State = rpc.ReconEvaluationStateFailed
	if original.Nudges.Reconciliation.Report.State != rpc.ReconReportStateCurrent ||
		original.Nudges.Reconciliation.Evaluation.State != rpc.ReconEvaluationStateComplete {
		t.Fatalf("mutating clone changed original: %+v", original.Nudges.Reconciliation)
	}
}

func TestPublicSnapshotAndNudgeEventRedactReconciliationAndPreserveDegradedHealth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC)
	ok := rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: now}
	stale := rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusStale, Reason: rpc.NudgeHealthReasonEvidenceStale, AsOf: now}
	status := rpc.ReconAutomationStatus{
		Report: rpc.ReconFetchStatus{
			Configured: true, State: rpc.ReconReportStateChecking, Busy: true,
			LastError: "private reconciliation sentinel",
		},
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateChecking, Reason: rpc.ReconEvaluationReasonReportPending},
	}
	nudges := &rpc.NudgesSnapshotResult{
		AsOf: now, Candidates: []rpc.NudgeCandidate{{Title: "safe candidate"}}, Reconciliation: &status,
		SourceHealth: rpc.NudgeSourceHealth{
			Policy: ok, Reconciliation: ok, Capital: ok, Pins: stale, Cadence: ok, ConfirmedFlow: ok,
		},
	}

	for name, value := range map[string]any{
		"bootstrap snapshot": Snapshot{Nudges: nudges},
		"nudges SSE event":   projectPublicNudges(nudges),
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		for _, forbidden := range []string{`"configured"`, `"busy"`, `"last_error"`, "private reconciliation sentinel"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s leaked %q: %s", name, forbidden, body)
			}
		}
		if strings.Count(body, `"reconciliation"`) != 1 {
			t.Fatalf("%s exposed report reconciliation outside source health: %s", name, body)
		}
		if !strings.Contains(body, `"aggregate":"degraded"`) {
			t.Fatalf("%s lost candidate-aware degraded health: %s", name, body)
		}
	}
}
