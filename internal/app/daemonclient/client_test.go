package daemonclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

var _ func(Real, context.Context, rpc.NudgesCutoverReviewParams) (*rpc.NudgesCutoverReviewResult, error) = Real.NudgesCutoverReview
var _ func(Real, context.Context) (*rpc.AlertCandidateSnapshot, error) = Real.AlertCandidates

func TestAlertCandidatesHasFixedTypedSignatureAndValidatesResult(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	want := daemonAlertCandidateSnapshot(t, now)
	got, err := alertCandidates(context.Background(), func(_ context.Context, method string, rawParams, rawOut any) error {
		if method != rpc.MethodAlertCandidates {
			t.Fatalf("method=%q", method)
		}
		if _, ok := rawParams.(rpc.AlertCandidatesParams); !ok {
			t.Fatalf("params=%T, want rpc.AlertCandidatesParams", rawParams)
		}
		typedOut, ok := rawOut.(*rpc.AlertCandidateSnapshot)
		if !ok {
			t.Fatalf("result destination=%T", rawOut)
		}
		*typedOut = want
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.CurrentState != rpc.AlertSnapshotClear || !got.AsOf.Equal(now) {
		t.Fatalf("result=%+v want=%+v", got, want)
	}
}

func TestAlertCandidatesRejectsInvalidTypedResult(t *testing.T) {
	t.Parallel()
	got, err := alertCandidates(context.Background(), func(_ context.Context, _ string, _ any, _ any) error {
		return nil
	})
	if got != nil || !errors.Is(err, ErrInvalidAlertCandidateSnapshot) {
		t.Fatalf("result=%+v err=%v, want typed validation error", got, err)
	}
}

func TestAlertCandidatesPreservesTransportFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("transport unavailable")
	got, err := alertCandidates(context.Background(), func(_ context.Context, _ string, _ any, _ any) error {
		return wantErr
	})
	if got != nil || !errors.Is(err, wantErr) {
		t.Fatalf("result=%+v err=%v, want transport error", got, err)
	}
}

func daemonAlertCandidateSnapshot(t *testing.T, at time.Time) rpc.AlertCandidateSnapshot {
	t.Helper()
	authorityScope, err := rpc.BuildAlertAuthorityScope("DAEMONCLIENT-TEST", rpc.AccountModePaper)
	if err != nil {
		t.Fatal(err)
	}
	return rpc.AlertCandidateSnapshot{
		SchemaVersion:  rpc.AlertCandidateSnapshotVersion,
		AsOf:           at,
		AuthorityScope: authorityScope,
		CurrentState:   rpc.AlertSnapshotClear,
		Coverage: rpc.AlertCoverage{
			State:           rpc.AlertCoverageComplete,
			Freshness:       rpc.AlertCoverageCurrent,
			AsOf:            at,
			ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress},
			CoveredSources:  []rpc.AlertSource{rpc.AlertSourceStress},
		},
		Sources: []rpc.AlertSourceCoverage{{
			Source:         rpc.AlertSourceStress,
			Status:         "current",
			Reason:         "source_current",
			EvidenceHealth: rpc.AlertEvidenceCurrent,
			InputAsOf:      at,
			ObservedAt:     at,
			EvidenceAsOf:   at,
			FreshUntil:     at.Add(time.Hour),
			Covered:        true,
		}},
		Candidates: []rpc.AlertCandidate{},
	}
}

func TestNudgesCutoverReviewHasFixedTypedSignature(t *testing.T) {
	t.Parallel()
	reviewedAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	want := rpc.NudgesCutoverReviewResult{
		OK: true, ReviewedAt: reviewedAt, CoverageFrom: reviewedAt.Add(-time.Hour),
		Evidence: rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}
	params := rpc.NudgesCutoverReviewParams{
		Origin:   rpc.NudgeCutoverReviewOriginPairedDevice,
		Evidence: rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}
	got, err := nudgesCutoverReview(context.Background(), params, func(_ context.Context, method string, rawParams, rawOut any) error {
		if method != rpc.MethodNudgesCutoverReview {
			t.Fatalf("method=%q", method)
		}
		typedParams, ok := rawParams.(rpc.NudgesCutoverReviewParams)
		if !ok || typedParams != params {
			t.Fatalf("params=%T %+v", rawParams, rawParams)
		}
		typedOut, ok := rawOut.(*rpc.NudgesCutoverReviewResult)
		if !ok {
			t.Fatalf("result destination=%T", rawOut)
		}
		*typedOut = want
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != want {
		t.Fatalf("result=%+v want=%+v", got, want)
	}
}

func TestNudgesCutoverReviewRejectsMissingOrInvalidTypedResult(t *testing.T) {
	t.Parallel()
	reviewedAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	params := rpc.NudgesCutoverReviewParams{
		Origin:   rpc.NudgeCutoverReviewOriginPairedDevice,
		Evidence: rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}
	for _, tc := range []struct {
		name   string
		result *rpc.NudgesCutoverReviewResult
	}{
		{name: "success envelope leaves destination untouched"},
		{name: "invalid populated result", result: &rpc.NudgesCutoverReviewResult{
			OK: true, ReviewedAt: reviewedAt, CoverageFrom: reviewedAt.Add(-time.Hour), Evidence: "HOSTILE_PRIVATE_EVIDENCE",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nudgesCutoverReview(context.Background(), params, func(_ context.Context, _ string, _ any, rawOut any) error {
				if tc.result != nil {
					*rawOut.(*rpc.NudgesCutoverReviewResult) = *tc.result
				}
				return nil
			})
			if err == nil || got != nil || !errors.Is(err, ErrInvalidNudgesCutoverReviewResult) {
				t.Fatalf("result=%+v err=%v, want nil result and validation error", got, err)
			}
		})
	}
}
