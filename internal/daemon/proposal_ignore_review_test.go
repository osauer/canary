package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestProposalIgnoreRejectsStaleMissingAndCrossAccountCandidates(t *testing.T) {
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	row := rig.stopProposal()
	revision := rig.install(row)
	for _, tc := range []struct {
		name, key, revision string
		changeAccount       bool
	}{
		{"stale revision", row.Key, "stale", false},
		{"unknown key", "trailing_stop:future", revision, false},
		{"account changed", row.Key, revision, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.changeAccount {
				rig.scope.Account = "DU7654321"
			}
			got := rig.engine.Ignore(rpc.TradeProposalIgnoreParams{Key: tc.key, Revision: tc.revision})
			if got.Accepted || rig.engine.isIgnored(rig.scope, tc.key) {
				t.Fatalf("unreviewed ignore was persisted: %+v", got)
			}
			if events, err := loadProposalEvents(t.Context(), rig.core); err != nil {
				t.Fatal(err)
			} else {
				for _, event := range events {
					if event.Type == "ignored" {
						t.Fatal("refused ignore appended an event")
					}
				}
			}
			rig.scope.Account = "DU1234567"
		})
	}
	got := rig.engine.Ignore(rpc.TradeProposalIgnoreParams{Key: row.Key})
	if !got.Accepted || got.Revision != revision || !rig.engine.isIgnored(rig.scope, row.Key) {
		t.Fatalf("current key-only ignore = %+v", got)
	}
	rig.restart()
	if !rig.engine.isIgnored(rig.scope, row.Key) {
		t.Fatal("accepted ignore was not durable")
	}
}
