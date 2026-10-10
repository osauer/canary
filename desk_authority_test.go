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

// The authority methods reach the daemon by name with their exact payloads,
// keep the brake's hold visible and never appear in the MCP catalogue.
func TestDeskAuthorityClientCallsTheDaemonAndStaysOutOfTheCatalogue(t *testing.T) {
	s := canarytest.Serve(t)
	s.Handle("desk.authority.status", canarytest.Result(canary.DeskAuthorityStatus{Generation: 3, Scope: "protect", HeldBy: "drawdown_brake"}))
	s.Handle("desk.authority.prepare", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in canary.DeskAuthorityPrepareParams
		if err := json.Unmarshal(raw, &in); err != nil || in.Scope != "full" || in.ControllerHash != "hash" {
			t.Fatalf("prepare request %s", raw)
		}
		return json.Marshal(canary.DeskAuthorityPrepared{Terms: "{}", Digest: "sha256:d"})
	})
	s.Handle("desk.authority.confirm", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, canarytest.Fail("bad_request", "full automatic trading can be confirmed after the drawdown brake clears; protection can be armed now")
	})
	s.Handle("desk.authority.control", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in canary.DeskAuthorityControlParams
		if err := json.Unmarshal(raw, &in); err != nil || in.ExpectedGeneration != 3 || in.Scope != "protect" {
			t.Fatalf("control request %s", raw)
		}
		return json.Marshal(canary.DeskAuthorityStatus{Generation: 4, Scope: "protect"})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	st, err := c.DeskAuthority(t.Context())
	if err != nil || st.Scope != "protect" || st.HeldBy != "drawdown_brake" {
		t.Fatalf("status %+v %v", st, err)
	}
	prepared, err := c.PrepareDeskAuthority(t.Context(), canary.DeskAuthorityPrepareParams{Scope: "full", ControllerHash: "hash"})
	if err != nil || prepared.Digest != "sha256:d" {
		t.Fatalf("prepare %+v %v", prepared, err)
	}
	if got, err := c.ConfirmDeskAuthority(t.Context(), canary.DeskAuthorityConfirmParams{Terms: "{}", Digest: "sha256:d"}); got != nil || err == nil || !strings.Contains(err.Error(), "drawdown brake") {
		t.Fatalf("confirm under the brake: %+v %v", got, err)
	} else if _, ok := errors.AsType[*canary.Error](err); !ok {
		t.Fatalf("typed refusal lost: %v", err)
	}
	if got, err := c.ControlDeskAuthority(t.Context(), canary.DeskAuthorityControlParams{ExpectedGeneration: 3, Scope: "protect"}); err != nil || got.Generation != 4 {
		t.Fatalf("control %+v %v", got, err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if strings.HasPrefix(method, "desk.") {
				t.Fatalf("MCP tool %s reaches %s", tool.Name, method)
			}
		}
	}
}
