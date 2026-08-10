package apphttp

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestAlertProjectionExposesOnlyCurrentOccurrences(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	active := state.AlertDeliveryOccurrenceView{
		DisplayID:        "alert-current",
		Source:           rpc.AlertSourceRulebook,
		Kind:             rpc.AlertKindGovernance,
		PresentationCode: rpc.AlertPresentationRulebookHedgeIntegrity,
		State:            rpc.AlertEpisodeOpen,
		Severity:         rpc.AlertSeverityAct,
		EvidenceHealth:   rpc.AlertEvidenceCurrent,
		Destination:      rpc.AlertDestinationAlerts,
		EvidenceAsOf:     now,
		StateChangedAt:   now,
		FirstSeenAt:      now,
		LastSeenAt:       now,
		AttentionSeq:     2,
		Disposition:      state.AlertDispositionEligible,
	}
	ended := active
	ended.DisplayID = "alert-ended"
	ended.State = rpc.AlertEpisodeRecovered
	ended.EndedAt = now
	ended.EndReason = state.AlertDeliveryEndRecovered
	ended.AttentionSeq = 1
	retained := active
	retained.DisplayID = "alert-retained"
	retained.EvidenceHealth = rpc.AlertEvidencePartial
	retained.AttentionSeq = 3

	got := newAlertOccurrenceDTOs([]state.AlertDeliveryOccurrenceView{ended, retained, active})
	if len(got) != 1 || got[0].DisplayID != active.DisplayID {
		t.Fatalf("projected occurrences=%+v, want only current alert", got)
	}
	attention := newAlertAttentionDTO(state.AlertDeliveryAttention{
		UnreadCount:    2,
		HighWaterSeq:   2,
		ReadThroughSeq: 0,
		UnreadRefs: []state.AlertDeliveryAttentionRef{
			{DisplayID: ended.DisplayID, Source: ended.Source, Kind: ended.Kind},
			{DisplayID: active.DisplayID, Source: active.Source, Kind: active.Kind},
		},
	}, got)
	if attention.UnreadCount != 1 || len(attention.UnreadRefs) != 1 || attention.UnreadRefs[0].DisplayID != active.DisplayID {
		t.Fatalf("projected attention=%+v, want only current alert", attention)
	}
	if attention.HighWaterSeq != 2 {
		t.Fatalf("high_water_seq=%d, want 2 so reading current alerts also advances hidden terminal delivery rows", attention.HighWaterSeq)
	}
}
