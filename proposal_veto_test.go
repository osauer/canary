package canary_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/osauer/canary/v2"
	"github.com/osauer/canary/v2/canarytest"
)

// The veto reaches the daemon by name with the row, reason and origin, keeps
// the daemon's refusal typed, and never appears in the MCP catalogue.
func TestVetoProposalCallsTheDaemonAndStaysOutOfTheCatalogue(t *testing.T) {
	s := canarytest.Serve(t)
	s.Handle("trade_proposals.veto", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in canary.TradeProposalVetoParams
		if err := json.Unmarshal(raw, &in); err != nil || in.Key != "mandate_reduction:AAA" || in.Revision != "7" || in.Origin != canary.OriginPairedDevice || in.Reason != "vetoed by the owner in Desk" {
			t.Fatalf("veto request %s", raw)
		}
		return json.Marshal(canary.TradeProposalVetoResult{Accepted: true, Key: in.Key, Revision: in.Revision, State: "vetoed", Message: "automatic submission vetoed; the hold lasts until the proposal revision changes"})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	res, err := c.VetoProposal(t.Context(), canary.TradeProposalVetoParams{Key: "mandate_reduction:AAA", Revision: "7", Reason: "vetoed by the owner in Desk", Origin: canary.OriginPairedDevice})
	if err != nil || !res.Accepted || res.State != "vetoed" {
		t.Fatalf("veto %+v %v", res, err)
	}
	s.Handle("trade_proposals.veto", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, canarytest.Fail("bad_request", "proposal veto is human-only: agent, missing and unknown origins are refused")
	})
	if got, err := c.VetoProposal(t.Context(), canary.TradeProposalVetoParams{Key: "x", Origin: "agent"}); got != nil || err == nil || !strings.Contains(err.Error(), "human-only") {
		t.Fatalf("agent origin: %+v %v", got, err)
	} else if _, ok := errors.AsType[*canary.Error](err); !ok {
		t.Fatalf("typed refusal lost: %v", err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if method == "trade_proposals.veto" {
				t.Fatalf("MCP tool %s reaches the veto", tool.Name)
			}
		}
	}
}
