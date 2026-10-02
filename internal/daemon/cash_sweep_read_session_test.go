package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestCashSweepReadSessionsRejectRestartsReplacementsAndUnstampedReads(t *testing.T) {
	original := rpc.BrokerReadSession{DaemonStartedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), ConnectorGeneration: 7, SocketEpoch: 3}
	if !cashSweepReadSessionsMatch(original, original, original) {
		t.Fatal("matching original receipts rejected")
	}
	for _, variant := range []string{"daemon_restart", "connector_replacement", "socket_reconnect", "missing_account", "missing_positions", "no_current_session"} {
		t.Run(variant, func(t *testing.T) {
			account, positions, current := original, original, original
			switch variant {
			case "daemon_restart":
				current.DaemonStartedAt = current.DaemonStartedAt.Add(time.Second)
			case "connector_replacement":
				positions.ConnectorGeneration++
			case "socket_reconnect":
				positions.SocketEpoch++
			case "missing_account":
				account = rpc.BrokerReadSession{}
			case "missing_positions":
				positions = rpc.BrokerReadSession{}
			case "no_current_session":
				current = rpc.BrokerReadSession{}
			}
			if cashSweepReadSessionsMatch(account, positions, current) {
				t.Fatal("different or unstamped receipt matched current session")
			}
		})
	}
	var absent *Server
	if got := absent.cashSweepReadSession(nil, ibkrlib.ConnectorSessionBinding{}); got != (rpc.BrokerReadSession{}) {
		t.Fatal("missing server minted provenance")
	}
}

func TestCashSweepAccountRetirementClearsOriginalReadSession(t *testing.T) {
	receipt := rpc.BrokerReadSession{DaemonStartedAt: time.Now().UTC(), ConnectorGeneration: 1, SocketEpoch: 1}
	res := &rpc.AccountResult{Authority: &rpc.AccountDataAuthority{BrokerReadSession: receipt}}
	finalizeAccountSummarySession(res, accountSummaryAuthority{}, false)
	if res.Authority.BrokerReadSession != (rpc.BrokerReadSession{}) {
		t.Fatal("retired account retained current read-session claim")
	}
}
