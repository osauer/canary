package canary_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	canary "github.com/osauer/canary/v3"
	"github.com/osauer/canary/v3/canarytest"
)

// The Go client reaches desk.execution.submit privately, outside the MCP
// catalogue, and hands Desk the receipts as the daemon answers them.
func TestDeskExecutionClientSubmitsAndStaysOutOfTheCatalogue(t *testing.T) {
	s := canarytest.Serve(t)
	item := canary.DeskExecutionItem{RequestID: "req-1", EpisodeID: "ep-1", Intent: canary.DeskExecutionIntent{Class: canary.DeskClassAdd, Proposal: &canary.DeskExecutionProposal{Key: "core_rebalance:syn", Revision: "r1"}}}
	s.Handle("desk.execution.submit", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in canary.DeskExecutionSubmitParams
		if err := json.Unmarshal(raw, &in); err != nil || in.Automatic == nil || in.Automatic.ID != "mandate" || len(in.Items) != 1 || in.Items[0].RequestID != "req-1" {
			t.Fatalf("submit request %s", raw)
		}
		return json.Marshal(canary.DeskExecutionSubmitResult{Receipts: []canary.DeskExecutionReceipt{{RequestID: "req-1", EpisodeID: "ep-1", Digest: canary.DeskIntentDigest(item), Class: canary.DeskClassAdd, Outcome: canary.DeskOutcomeAccepted, OrderRef: "canary-syn-1"}}})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	got, err := c.SubmitDeskExecution(t.Context(), canary.DeskExecutionSubmitParams{Automatic: &canary.DeskAutomaticAuthorization{ID: "mandate", Capability: strings.Repeat("c", 40), ExpectedGeneration: 2, PreferencesHash: strings.Repeat("a", 64)}, Items: []canary.DeskExecutionItem{item}})
	if err != nil || len(got.Receipts) != 1 || got.Receipts[0].Validate(item, false) != nil || got.Receipts[0].OrderRef != "canary-syn-1" {
		t.Fatalf("submit %+v %v", got, err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if strings.HasPrefix(method, "desk.") {
				t.Fatalf("MCP tool %s reaches %s", tool.Name, method)
			}
		}
	}
}
