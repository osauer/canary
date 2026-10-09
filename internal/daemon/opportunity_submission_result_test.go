package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestExerciseSubmissionResultPreservesRefusalAndSuccess(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	opp := rpc.Opportunity{Key: "synthetic-exercise"}
	preview := rpc.OpportunityExercisePreviewResult{Opportunity: opp, Accepted: true, SubmitEligible: true}
	payload := orderPreviewTokenPayload{TokenID: "synthetic-token", Draft: rpc.OrderDraft{OrderRef: "synthetic-order"}}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "refused", err: ErrTradingDisabled},
		{name: "uncertain", err: errors.New("synthetic broker failure")},
		{name: "sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &opportunityEngine{}
			got := e.recordExerciseSubmission(preview, opp, payload, now, tc.err)
			if got.Accepted != (tc.err == nil) || got.PreviewTokenID != payload.TokenID || got.OrderRef != payload.Draft.OrderRef || got.Opportunity.Key != opp.Key || !got.AsOf.Equal(now) || got.Preview == nil || !got.Preview.SubmitEligible {
				t.Fatalf("submission result lost outcome or reviewed identity: %+v", got)
			}
			if tc.err != nil {
				if len(got.Blockers) != 1 || got.Blockers[0].Code != "exercise_submit_failed" || got.Blockers[0].Message != tc.err.Error() {
					t.Fatalf("refusal did not retain its blocker: %+v", got)
				}
			} else if len(got.Blockers) != 0 {
				t.Fatalf("successful submission has blockers: %+v", got)
			}
		})
	}
}
