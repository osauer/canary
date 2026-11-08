package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Off for this broker is a result an AI host can read, never a tool error.
func TestNotApplicableIsATypedResult(t *testing.T) {
	if !notApplicable(fmt.Errorf("call: %w", &rpc.Error{Code: rpc.CodeNotApplicable, Message: "IBKR only"})) {
		t.Fatal("a wrapped not_applicable was not recognised")
	}
	if notApplicable(&rpc.Error{Code: rpc.CodeGatewayUnavailable}) || notApplicable(errors.New("broken")) {
		t.Fatal("another failure read as not_applicable")
	}
	var out map[string]string
	daemonSaid := &rpc.Error{Code: rpc.CodeNotApplicable, Message: "IBKR only: this daemon serves OpenD"}
	if err := json.Unmarshal(notApplicableResult("canary_edge", fmt.Errorf("call: %w", daemonSaid)), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "not_applicable" || out["tool"] != "canary_edge" || out["reason"] != daemonSaid.Message {
		t.Fatalf("result %v", out)
	}
	for _, name := range []string{"canary_edge", "canary_recon_status", "canary_reporting", "canary_lending_market", "canary_short_interest_screen"} {
		tool, ok := lookupTool(name)
		if !ok {
			t.Fatalf("no tool %s", name)
		}
		if want := "on OpenD the result reports `not_applicable`"; !strings.Contains(tool.Description, want) {
			t.Errorf("%s: description lacks %q", name, want)
		}
	}
}
