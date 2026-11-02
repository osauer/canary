package canary_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2"
	"github.com/osauer/canary/v2/canarytest"
)

func TestVetoProposalForwardsTheNotificationWindow(t *testing.T) {
	want := canary.TradeProposalVetoParams{Key: "mandate_reduction:AAA", Revision: "7",
		Origin: canary.OriginPairedDevice, NoticeID: "synthetic-window-identity",
		VetoUntil: time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)}
	s := canarytest.Serve(t)
	s.Handle("trade_proposals.veto_window", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in canary.TradeProposalVetoParams
		if err := json.Unmarshal(raw, &in); err != nil || in.Key != want.Key || in.Revision != want.Revision ||
			in.Origin != want.Origin || in.NoticeID != want.NoticeID || !in.VetoUntil.Equal(want.VetoUntil) {
			t.Fatalf("notification authority was lost in the daemon request: %s", raw)
		}
		return json.Marshal(canary.TradeProposalVetoResult{Accepted: false, Key: in.Key, Revision: in.Revision, Message: "window ended"})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	result, err := c.VetoProposal(t.Context(), want)
	if err != nil || result.Accepted || result.Message != "window ended" {
		t.Fatalf("conditional veto refusal was lost: %+v %v", result, err)
	}
}

func TestVetoProposalNeverDowngradesNotificationAuthorityForAnOlderDaemon(t *testing.T) {
	s := canarytest.Serve(t)
	var legacyCalls atomic.Int32
	s.Handle("trade_proposals.veto", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		legacyCalls.Add(1)
		return json.Marshal(canary.TradeProposalVetoResult{Accepted: true})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	result, err := c.VetoProposal(t.Context(), canary.TradeProposalVetoParams{Key: "mandate_reduction:AAA",
		Origin: canary.OriginPairedDevice, NoticeID: "synthetic-window-identity",
		VetoUntil: time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)})
	if result != nil || err == nil || legacyCalls.Load() != 0 {
		t.Fatalf("notification authority was silently dropped or retried through the legacy method: result=%+v err=%v legacy=%d", result, err, legacyCalls.Load())
	}
}

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
			if method == "trade_proposals.veto" || method == "trade_proposals.veto_window" {
				t.Fatalf("MCP tool %s reaches the veto", tool.Name)
			}
		}
	}
}
