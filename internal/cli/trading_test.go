package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRenderTradingStatusTextDisabled(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderTradingStatusText(env, &rpc.TradingStatus{
		Mode:           "disabled",
		Endpoint:       "127.0.0.1:4002",
		AccountOrigin:  "auto",
		ClientID:       15,
		ClientIDOrigin: "default",
		MCPTrading:     rpc.TradingMCPDisabled,
	})
	got := stdout.String()
	for _, want := range []string{
		"Canary Trading  DISABLED",
		"Mode           disabled",
		"MCP trading    disabled",
		"Capabilities   preview=false write=false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trading status missing %q:\n%s", want, got)
		}
	}
}

func TestRenderTradingStatusTextBlocked(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderTradingStatusText(env, &rpc.TradingStatus{
		Mode:           "paper",
		Endpoint:       "127.0.0.1:4002",
		Account:        "DU1234567",
		AccountOrigin:  "pinned",
		ClientID:       15,
		ClientIDOrigin: "default",
		MCPTrading:     rpc.TradingMCPDisabled,
		CanPreview:     false,
		Blocked:        true,
		Blockers: []rpc.TradingBlocker{{
			Code:    "gateway_client_id_unpinned",
			Message: "order submission requires a pinned client ID",
			Action:  "Set [gateway].client_id.",
		}},
	})
	got := stdout.String()
	for _, want := range []string{
		"Canary Trading  BLOCKED",
		"Mode           paper blocked",
		"Capabilities   preview=false write=false",
		"gateway_client_id_unpinned: order submission requires a pinned client ID",
		"action: Set [gateway].client_id.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trading status missing %q:\n%s", want, got)
		}
	}
}

func TestRenderTradingStatusTextWriteBlockers(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderTradingStatusText(env, &rpc.TradingStatus{
		Mode:           "paper",
		Endpoint:       "127.0.0.1:7497",
		Account:        "DU1234567",
		AccountOrigin:  "pinned",
		ClientID:       15,
		ClientIDOrigin: "pinned",
		MCPTrading:     rpc.TradingMCPDisabled,
		CanPreview:     true,
		CanWrite:       false,
		WriteBlockers: []rpc.TradingBlocker{{
			Code:    "order_writes_unavailable",
			Message: "order writes are unavailable in this build",
			Action:  "Rebuild the daemon with the trading write capability.",
		}},
	})
	got := stdout.String()
	for _, want := range []string{
		"Canary Trading  READY",
		"Capabilities   preview=true write=false",
		"Write blockers:",
		"order_writes_unavailable: order writes are unavailable in this build",
		"action: Rebuild the daemon with the trading write capability.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trading status missing %q:\n%s", want, got)
		}
	}
}

// The dispatcher hoists flags ahead of positionals, so `canary trading
// paper-smoke --json` reaches runTrading as ["--json", "paper-smoke"].
// The old dispatch treated a leading flag as "status implied" and
// silently ran `trading status` instead of the paper smoke — which let
// the release gate read a status payload as an empty smoke result.
func TestTradingSubcommandIndexFindsHoistedSubcommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"hoisted flag before subcommand", []string{"--json", "paper-smoke"}, 1},
		{"subcommand first", []string{"paper-smoke", "--json"}, 0},
		{"status with hoisted flag", []string{"--json", "status"}, 1},
		{"flags only implies status", []string{"--json"}, -1},
		{"bare", nil, -1},
		{"unknown token is not a subcommand", []string{"bogus"}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tradingSubcommandIndex(tc.args); got != tc.want {
				t.Fatalf("tradingSubcommandIndex(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}
