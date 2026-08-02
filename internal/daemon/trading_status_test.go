package daemon

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestTradingStatusDefaultDisabled(t *testing.T) {
	t.Parallel()
	srv := &Server{cfg: &config.Resolved{}}
	st := srv.tradingStatus(discover.Endpoint{})

	if st.Mode != config.TradingModeDisabled {
		t.Fatalf("Mode = %q, want disabled", st.Mode)
	}
	if st.Blocked {
		t.Fatalf("disabled trading should not render as blocked: %+v", st.Blockers)
	}
	if st.CanPreview || st.CanWrite {
		t.Fatalf("disabled capabilities should all be false: %+v", st)
	}
}

func TestTradingStatusBlocksEnabledWithoutPinnedGateway(t *testing.T) {
	t.Parallel()
	srv := &Server{cfg: &config.Resolved{
		Trading: config.Trading{Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 15})

	for _, code := range []string{"daemon_storage_unavailable", "gateway_port_unpinned", "gateway_account_unpinned", "gateway_client_id_unpinned"} {
		if !hasTradingBlocker(st, code) {
			t.Fatalf("missing blocker %q in %+v", code, st.Blockers)
		}
	}
}

func TestTradingStatusBlocksPaperModeOnLiveLookingEndpoint(t *testing.T) {
	t.Parallel()
	port := 4001
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "U1234567"},
		Trading: config.Trading{Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 31, Account: "U1234567", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "paper_endpoint_unconfirmed") {
		t.Fatalf("missing paper endpoint blocker in %+v", st.Blockers)
	}
}

func TestAccountModeForStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		port    int
		account string
		want    string
	}{
		{name: "gateway paper port", port: 4002, want: rpc.AccountModePaper},
		{name: "tws paper port", port: 7497, want: rpc.AccountModePaper},
		{name: "paper account", port: 4001, account: "DU1234567", want: rpc.AccountModePaper},
		{name: "gateway live port", port: 4001, want: rpc.AccountModeLive},
		{name: "tws live port", port: 7496, want: rpc.AccountModeLive},
		{name: "live account on custom port", port: 5000, account: "U1234567", want: rpc.AccountModeLive},
		{name: "aggregate account on custom port", port: 5000, account: "All", want: rpc.AccountModeUnknown},
		{name: "unknown", port: 5000, want: rpc.AccountModeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountModeForStatus(tc.port, tc.account); got != tc.want {
				t.Fatalf("accountModeForStatus(%d, %q) = %q, want %q", tc.port, tc.account, got, tc.want)
			}
		})
	}
}

func TestTradingStatusBlocksAggregateAccount(t *testing.T) {
	t.Parallel()
	port := 7497
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "All"},
		Trading: config.Trading{Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 7497, ClientID: 31, Account: "All", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "gateway_account_not_concrete") {
		t.Fatalf("missing aggregate account blocker in %+v", st.Blockers)
	}
	if st.CanPreview || st.CanWrite {
		t.Fatalf("blocked capabilities should all be false: %+v", st)
	}
}

func TestAccountMismatchesConnectedAllowsAggregateManagedAccount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		configured string
		connected  string
		want       bool
	}{
		{name: "same concrete account", configured: "DU1234567", connected: "DU1234567", want: false},
		{name: "case-insensitive concrete account", configured: "du1234567", connected: "DU1234567", want: false},
		{name: "connected aggregate all", configured: "DU1234567", connected: "All", want: false},
		{name: "configured account is in managed account list", configured: "DU1234567", connected: "DU7654321, DU1234567", want: false},
		{name: "configured account is in managed account list case-insensitively", configured: "du1234567", connected: "DU7654321,DU1234567", want: false},
		{name: "blank connected account unknown", configured: "DU1234567", connected: "", want: false},
		{name: "different concrete account", configured: "DU1234567", connected: "DU7654321", want: true},
		{name: "configured account is absent from managed account list", configured: "DU1234567", connected: "DU7654321,DU9999999", want: true},
		// IBKR terminates managedAccounts with a trailing comma; issue #14's
		// reporter pasted "U…,U…,". Reading that empty final entry as malformed
		// refused every broker write on the multi-account logins this check scopes.
		{name: "broker trailing comma is list syntax, not a malformed member", configured: "DU1234567", connected: "DU1234567,", want: false},
		{name: "trailing comma on a multi-account list still matches a member", configured: "DU1234567", connected: "DU7654321,DU1234567,", want: false},
		{name: "trailing comma does not rescue an absent account", configured: "DU1234567", connected: "DU7654321,DU9999999,", want: true},
		{name: "empty entries only still fails closed", configured: "DU1234567", connected: ",,", want: true},
		{name: "aggregate member beside a trailing comma still fails closed", configured: "DU1234567", connected: "DU1234567,All,", want: true},
		{name: "aggregate member in managed account list fails closed", configured: "DU1234567", connected: "DU1234567,All", want: true},
		{name: "configured aggregate fails closed", configured: "DU1234567,DU7654321", connected: "DU1234567,DU7654321", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := accountMismatchesConnected(tc.configured, tc.connected); got != tc.want {
				t.Fatalf("accountMismatchesConnected(%q, %q) = %v, want %v", tc.configured, tc.connected, got, tc.want)
			}
		})
	}
}

func TestTradingStatusBlocksClientIDMismatch(t *testing.T) {
	t.Parallel()
	port := 4002
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
		Trading: config.Trading{Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 32, Account: "DU1234567", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "gateway_client_id_mismatch") {
		t.Fatalf("missing client-id mismatch blocker in %+v", st.Blockers)
	}
	if st.CanPreview || st.CanWrite {
		t.Fatalf("blocked capabilities should all be false: %+v", st)
	}
}

func TestTradingStatusReadyPaperPreviewCapability(t *testing.T) {
	t.Parallel()
	port := 4002
	clientID := 31
	srv := &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
			Trading: config.Trading{Mode: config.TradingModePaper}.WithDefaults(),
		},
		gatewayReadyForTrading: func() bool { return true },
	}
	attachTradingStatusTestAuthority(t, srv)
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU1234567", PortOrigin: discover.OriginPinned})

	if st.Blocked {
		t.Fatalf("paper status should be ready, got blockers %+v", st.Blockers)
	}
	if !st.CanPreview {
		t.Fatalf("ready paper gate should allow preview: %+v", st)
	}
	if st.CanWrite != orderWritesAvailable {
		t.Fatalf("write capabilities mismatch build mode: %+v", st)
	}
	if orderWritesAvailable {
		if len(st.WriteBlockers) > 0 {
			t.Fatalf("write-ready status should not expose write blockers: %+v", st.WriteBlockers)
		}
	} else if !hasTradingWriteBlocker(st, "order_writes_unavailable") {
		t.Fatalf("missing write blocker for non-trading build: %+v", st.WriteBlockers)
	}
}

func TestTradingStatusPrefersPinnedAccountOverEndpointAggregate(t *testing.T) {
	t.Parallel()
	port := 7497
	clientID := 31
	srv := &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
			Trading: config.Trading{Mode: config.TradingModePaper}.WithDefaults(),
		},
		gatewayReadyForTrading: func() bool { return true },
	}
	attachTradingStatusTestAuthority(t, srv)
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 7497, ClientID: 31, Account: "All", PortOrigin: discover.OriginPinned})

	if st.Blocked {
		t.Fatalf("paper status should be ready, got blockers %+v", st.Blockers)
	}
	if st.Account != "DU1234567" {
		t.Fatalf("Account = %q, want pinned concrete account", st.Account)
	}
}

func TestTradingStatusLiveReadyWithPinsAndConnectedGateway(t *testing.T) {
	t.Parallel()
	port := 4001
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "U1234567"},
		Trading: config.Trading{Mode: config.TradingModeLive}.WithDefaults(),
	}, gatewayReadyForTrading: func() bool { return true }}
	attachTradingStatusTestAuthority(t, srv)
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 31, Account: "U1234567", PortOrigin: discover.OriginPinned})

	// Live-gate simplification 2026-06-11: mode="live" plus the gateway pins
	// on a live-looking endpoint is the whole config gate — no allow_live,
	// no ack keys.
	if st.Blocked {
		t.Fatalf("live status with pins and connected gateway should be ready, got blockers %+v", st.Blockers)
	}
	if st.LiveOverride != rpc.TradingLiveOverrideReady {
		t.Fatalf("LiveOverride = %q, want ready", st.LiveOverride)
	}
	// Re-gated 2026-06-10: paper-smoke evidence is informational, never a
	// live blocker - the smoke is enforced in the release pipeline instead.
	for _, b := range st.Blockers {
		if strings.HasPrefix(b.Code, "paper_smoke") {
			t.Fatalf("paper-smoke must not block live readiness, got %+v", st.Blockers)
		}
	}
	if st.PaperSmoke != tradingPaperSmokeStatusMissing {
		t.Fatalf("paper-smoke status should still be reported informationally, got %q", st.PaperSmoke)
	}
}

func TestTradingStatusBlocksWhenGatewayUnavailable(t *testing.T) {
	t.Parallel()
	port := 4001
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "U1234567"},
		Trading: config.Trading{Mode: config.TradingModeLive}.WithDefaults(),
	}}
	attachTradingStatusTestAuthority(t, srv)
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 31, Account: "U1234567", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "gateway_unavailable") {
		t.Fatalf("missing gateway unavailable blocker in %+v", st.Blockers)
	}
	if st.CanPreview || st.CanWrite {
		t.Fatalf("gateway-unavailable status should not allow preview/write: %+v", st)
	}
	if st.LiveOverride == rpc.TradingLiveOverrideReady {
		t.Fatalf("gateway-unavailable live status must not report live override ready: %+v", st)
	}
}

func TestTradingStatusBlocksLiveModeOnPaperLookingEndpoint(t *testing.T) {
	t.Parallel()
	port := 4002
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
		Trading: config.Trading{Mode: config.TradingModeLive}.WithDefaults(),
	}}
	attachTradingStatusTestAuthority(t, srv)
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU1234567", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "live_endpoint_unconfirmed") {
		t.Fatalf("missing live endpoint blocker in %+v", st.Blockers)
	}
}

func TestTradingStatusLiveReadyWithMatchingPaperSmoke(t *testing.T) {
	t.Parallel()
	port := 4001
	clientID := 31
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	srv := &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "U1234567"},
			Trading: config.Trading{Mode: config.TradingModeLive}.WithDefaults(),
		},
		version:                "test-version",
		now:                    func() time.Time { return now },
		gatewayReadyForTrading: func() bool { return true },
	}
	attachTradingStatusTestAuthority(t, srv)
	store := newTradingReadinessStore(filepath.Join(t.TempDir(), "trading-readiness.json"), newTestPaperSmokeSigner(t))
	if err := store.UseCoreStore(t.Context(), srv.coreStore); err != nil {
		t.Fatalf("attach readiness authority: %v", err)
	}
	if err := store.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SavePaperSmoke: %v", err)
	}
	srv.tradingReadiness = store
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 31, Account: "U1234567", PortOrigin: discover.OriginPinned})

	if st.Blocked {
		t.Fatalf("live status should be ready, got blockers %+v", st.Blockers)
	}
	if st.PaperSmoke != tradingPaperSmokeStatusValid {
		t.Fatalf("PaperSmoke = %q, want valid", st.PaperSmoke)
	}
	if st.PaperSmokeAt == nil || !st.PaperSmokeAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("PaperSmokeAt = %s", st.PaperSmokeAt)
	}
	if st.LiveOverride != rpc.TradingLiveOverrideReady {
		t.Fatalf("LiveOverride = %q, want ready", st.LiveOverride)
	}
	if !st.CanPreview {
		t.Fatalf("live-ready gate should allow preview: %+v", st)
	}
	if st.CanWrite != orderWritesAvailable {
		t.Fatalf("write capabilities mismatch build mode: %+v", st)
	}
	if orderWritesAvailable {
		if len(st.WriteBlockers) > 0 {
			t.Fatalf("write-ready status should not expose write blockers: %+v", st.WriteBlockers)
		}
	} else if !hasTradingWriteBlocker(st, "order_writes_unavailable") {
		t.Fatalf("missing write blocker for non-trading build: %+v", st.WriteBlockers)
	}
}

func hasTradingBlocker(st rpc.TradingStatus, code string) bool {
	return slices.ContainsFunc(st.Blockers, func(b rpc.TradingBlocker) bool {
		return b.Code == code
	})
}

func hasTradingWriteBlocker(st rpc.TradingStatus, code string) bool {
	return slices.ContainsFunc(st.WriteBlockers, func(b rpc.TradingBlocker) bool {
		return b.Code == code
	})
}

func attachTradingStatusTestAuthority(t *testing.T, srv *Server) {
	t.Helper()
	journal := newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	authority, err := journal.coreStore()
	if err != nil {
		t.Fatalf("test trading status authority: %v", err)
	}
	srv.orderJournal = journal
	srv.coreStore = authority
}
