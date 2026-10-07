package canary_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2"
	"github.com/osauer/canary/v2/canarytest"
	"github.com/osauer/canary/v2/canarytest/policyfile"
)

func TestCashPolicyClientKeepsTypesAndCodesAndStaysOutOfTheCatalogue(t *testing.T) {
	s := canarytest.Serve(t)
	s.Handle("policy.cash.get", canarytest.Result(canary.CashPolicySnapshot{Revision: "sha256:r1", FileState: "ok", PolicyVersion: 14}))
	s.Handle("policy.cash.check", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in canary.CashPolicyCheckRequest
		if err := json.Unmarshal(raw, &in); err != nil || in.ExpectedRevision != "sha256:r1" || string(in.Changes["cash.leveling.trigger_base"]) != "5000" {
			t.Fatalf("check request %s", raw)
		}
		return json.Marshal(canary.CashPolicyCheckResult{Revision: "sha256:r1", Terms: "{}", Digest: "sha256:d"})
	})
	s.Handle("policy.cash.apply", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, canarytest.Fail(canary.CodeConfirmationRequired, "confirm on your device")
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	snap, err := c.CashPolicy(t.Context())
	if err != nil || snap.Revision != "sha256:r1" || snap.PolicyVersion != 14 {
		t.Fatalf("get %+v %v", snap, err)
	}
	check, err := c.CheckCashPolicy(t.Context(), canary.CashPolicyCheckRequest{ExpectedRevision: "sha256:r1", Changes: map[string]json.RawMessage{"cash.leveling.trigger_base": json.RawMessage("5000")}})
	if err != nil || check.Digest != "sha256:d" {
		t.Fatalf("check %+v %v", check, err)
	}
	_, err = c.ApplyCashPolicy(t.Context(), canary.CashPolicyApplyRequest{Terms: "{}", Digest: "sha256:d", RequestID: "r"})
	if failure, ok := errors.AsType[*canary.Error](err); !ok || failure.Code != canary.CodeConfirmationRequired {
		t.Fatalf("typed refusal lost: %v", err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if strings.HasPrefix(method, "policy.cash.") {
				t.Fatalf("MCP tool %s reaches %s", tool.Name, method)
			}
		}
	}
}

// End to end through the daemon's own handlers: the client reads, checks
// and saves a temporary file, and the file then says what was saved.
func TestCashPolicyClientSavesThroughTheDaemonHandlers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "protection-policy.toml")
	file := `kind = "canary.protection_policy"
schema_version = 1
policy_id = "protection-test"
policy_version = 3

[authority]
close_reduce_only = true
auto_submit = false

[cash.leveling]
enabled = false
trigger_base = 10000.0  # written by Canary v9.9.9; currency leveling reads it from this file only
cushion_base = 250.0
max_slippage_bp = 2.0
payback_days = 30
`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	s := canarytest.Serve(t)
	policyfile.Serve(t, s, path)
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	snap, err := c.CashPolicy(t.Context())
	if err != nil || snap.FileState != "ok" || !snap.Writable || snap.PolicyVersion != 3 {
		t.Fatalf("get %+v %v", snap, err)
	}
	check, err := c.CheckCashPolicy(t.Context(), canary.CashPolicyCheckRequest{ExpectedRevision: snap.Revision, Changes: map[string]json.RawMessage{"cash.leveling.trigger_base": json.RawMessage("6000")}})
	if err != nil || check.Digest == "" || len(check.Changes) != 1 {
		t.Fatalf("check %+v %v", check, err)
	}
	confirmation := &canary.CashPolicyConfirmation{DeskActionID: "abc123", Credential: "passkey:cred", Envelope: `{"credential":"passkey"}`}
	saved, err := c.ApplyCashPolicy(t.Context(), canary.CashPolicyApplyRequest{Terms: check.Terms, Digest: check.Digest, RequestID: "client-e2e", Confirmation: confirmation})
	if err != nil || saved.SavedVersion != 4 || !saved.InForce || saved.Replay {
		t.Fatalf("apply %+v %v", saved, err)
	}
	text, _ := os.ReadFile(path)
	if !strings.Contains(string(text), "trigger_base = 6000.0  # set in Desk ") || !strings.Contains(string(text), "confirmed with your passkey (action abc123); was 10000.0") ||
		!strings.Contains(string(text), "policy_version = 4  # raised in Desk ") {
		t.Fatalf("saved file:\n%s", text)
	}
	again, err := c.ApplyCashPolicy(t.Context(), canary.CashPolicyApplyRequest{Terms: check.Terms, Digest: check.Digest, RequestID: "client-e2e", Confirmation: confirmation})
	if err != nil || !again.Replay || again.SavedVersion != 4 {
		t.Fatalf("retry %+v %v", again, err)
	}
	_, err = c.ApplyCashPolicy(t.Context(), canary.CashPolicyApplyRequest{Terms: check.Terms, Digest: check.Digest, RequestID: "client-e2e-2", Confirmation: confirmation})
	if failure, ok := errors.AsType[*canary.Error](err); !ok || failure.Code != canary.CodeSettingsConflict {
		t.Fatalf("stale terms after the save: %v", err)
	}
}
