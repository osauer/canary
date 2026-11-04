package futu_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/common"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/notify"
)

var record = flag.Bool("record", false, "re-record pkg/futu/testdata/frames from the simulator")

const (
	simAccount  uint64 = 1000001
	realAccount uint64 = 1000002
)

func connect(t *testing.T, srv *futusim.Server, account uint64) *futu.Connector {
	t.Helper()
	c := futu.NewConnector(futu.Config{
		Addr:          srv.Addr(),
		Account:       account,
		ClientID:      "canary-test",
		ClientVersion: 333,
		RedialDelay:   20 * time.Millisecond,
	})
	c.Start(context.Background())
	t.Cleanup(c.Stop)
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitReady(t *testing.T, c *futu.Connector) {
	t.Helper()
	waitFor(t, "a ready session", c.IsReady)
}

// The simulator starts in process quickly enough to use from any test, and a
// connector reaches ready over the real wire: handshake, state, account list.
func TestConnectorReachesReadyAgainstTheSimulator(t *testing.T) {
	start := time.Now()
	srv := futusim.Start(t, futusim.Options{})
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the simulator took %s to start", took)
	}
	c := connect(t, srv, simAccount)
	if _, ok := c.CaptureSession(); ok {
		t.Fatal("a binding before the session is ready")
	}
	waitReady(t, c)

	binding, ok := c.CaptureSession()
	if !ok || binding.Epoch == 0 || !c.Current(binding) || c.Current(futu.SessionBinding{}) {
		t.Fatalf("binding %+v ok=%v", binding, ok)
	}
	account, env := c.Scope()
	if account != simAccount || env != futu.EnvironmentSimulate {
		t.Fatalf("scope %d %q", account, env)
	}
	h := c.Health()
	if h.Phase != futu.PhaseReady || !h.Ready || !h.Connected || h.Session.ServerVersion != futu.MinServerVersion ||
		h.Session.ConnID == 0 || h.Session.KeepAliveInterval != 10*time.Second ||
		h.State.ProgramStatus != "Ready" || !h.State.QuoteLoggedIn || !h.State.TradeLoggedIn || h.State.BuildNo == 0 ||
		h.AccountType != "Margin" || h.Firm != "FutuSecurities" || len(h.Markets) != 3 || h.Markets[0] != "HK" {
		t.Fatalf("health %+v", h)
	}
	if h.State.Markets["HK"] != "Morning" || h.State.Markets["US"] != "Closed" {
		t.Fatalf("market states %v", h.State.Markets)
	}

	accounts, err := c.Accounts(context.Background())
	if err != nil || len(accounts) != 2 || accounts[1].Environment != futu.EnvironmentReal {
		t.Fatalf("accounts %+v %v", accounts, err)
	}
	if err := c.RefreshSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	c.Stop()
	if c.IsReady() || c.Current(binding) || c.Health().Phase != futu.PhaseStopped {
		t.Fatalf("after stop: %+v", c.Health())
	}
	c.Stop() // a second stop is harmless
}

// The version gate refuses below the minimum and admits at it.
func TestVersionGateRefusesBelowTheMinimumAndAdmitsAtIt(t *testing.T) {
	old := futusim.Start(t, futusim.Options{ServerVersion: futu.MinServerVersion - 1})
	c := connect(t, old, simAccount)
	waitFor(t, "the version refusal", func() bool { return errors.Is(c.Health().LastError, futu.ErrServerVersion) })
	var refused *futu.ServerVersionError
	if !errors.As(c.Health().LastError, &refused) || refused.Server != futu.MinServerVersion-1 || refused.Minimum != futu.MinServerVersion {
		t.Fatalf("refusal %+v", c.Health().LastError)
	}
	if c.IsReady() {
		t.Fatal("ready below the minimum")
	}
	waitReady(t, connect(t, futusim.Start(t, futusim.Options{}), simAccount))
}

// An OpenD with an RSA key refuses the handshake, and the refusal says so
// without the client reading its free text.
func TestRSARequiredOpenDRefusesTheHandshakeTyped(t *testing.T) {
	c := connect(t, futusim.Start(t, futusim.Options{RequireRSA: true}), simAccount)
	waitFor(t, "the handshake refusal", func() bool { return errors.Is(c.Health().LastError, futu.ErrHandshakeRefused) })
	var reply *futu.Error
	if !errors.As(c.Health().LastError, &reply) || reply.ProtoID != futu.ProtoInitConnect {
		t.Fatalf("refusal %+v", c.Health().LastError)
	}
	if h := c.Health(); h.Connected || h.Ready {
		t.Fatalf("connected through a refused handshake: %+v", h)
	}
}

// Nothing listening reads as port down.
func TestNothingListeningIsPortDown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	c := futu.NewConnector(futu.Config{Addr: addr, Account: simAccount, ClientVersion: 333, RedialDelay: 10 * time.Millisecond})
	c.Start(context.Background())
	t.Cleanup(c.Stop)
	waitFor(t, "port down", func() bool { return c.Health().Phase == futu.PhasePortDown && c.Health().LastError != nil })
}

// A listener that closes before answering reads as a port rejecting.
func TestSocketClosedBeforeTheAnswerIsPortRejecting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			nc, err := listener.Accept()
			if err != nil {
				return
			}
			_ = nc.Close()
		}
	}()
	c := futu.NewConnector(futu.Config{Addr: listener.Addr().String(), Account: simAccount, ClientVersion: 333, RedialDelay: 10 * time.Millisecond})
	c.Start(context.Background())
	t.Cleanup(c.Stop)
	waitFor(t, "port rejecting", func() bool { return c.Health().Phase == futu.PhasePortRejecting })
}

// A silent OpenD loses the session after two keep-alive intervals, however
// many requests it accepted: the heartbeat is kept by received frames.
func TestSilentOpenDLosesTheSessionAfterTwoIntervals(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{KeepAliveSeconds: 1})
	c := connect(t, srv, simAccount)
	waitReady(t, c)
	binding, _ := c.CaptureSession()

	// While OpenD answers, the keep-alive holds the session past two intervals.
	time.Sleep(2500 * time.Millisecond)
	if !c.Current(binding) {
		t.Fatalf("an answering OpenD lost the session: %+v", c.Health())
	}

	srv.SetSilent(true)
	silentAt := time.Now()
	waitFor(t, "the heartbeat to drop the session", func() bool { return !c.Current(binding) })
	if took := time.Since(silentAt); took > 3*time.Second {
		t.Fatalf("the session outlived a silent OpenD by %s", took)
	}
	var end *futu.SessionEndError
	waitFor(t, "the typed reason", func() bool { return errors.As(c.Health().LastError, &end) })
	if end.Reason != futu.EndHeartbeat {
		t.Fatalf("reason %+v", end)
	}

	// The connector redials on its own, and the new session is a new binding.
	srv.SetSilent(false)
	waitReady(t, c)
	next, _ := c.CaptureSession()
	if next == binding || c.Current(binding) || !c.Current(next) {
		t.Fatalf("after redial: old %+v new %+v", binding, next)
	}
	if c.Health().LastError != nil {
		t.Fatalf("a new session keeps the old error: %v", c.Health().LastError)
	}
}

// Kicked out, and the other events that end a session, close it with a typed
// reason; an event that leaves it open is recorded for the surface.
func TestGatewayEventsEndTheSessionTyped(t *testing.T) {
	for _, tc := range []struct {
		event notify.GtwEventType
		want  futu.SessionEnd
	}{
		{notify.GtwEventType_GtwEventType_KickedOut, futu.EndKickedOut},
		{notify.GtwEventType_GtwEventType_LoginPwdChanged, futu.EndPasswordChanged},
		{notify.GtwEventType_GtwEventType_BanLogin, futu.EndLoginBanned},
		{notify.GtwEventType_GtwEventType_EnableDeviceLock, futu.EndDeviceLock},
		{notify.GtwEventType_GtwEventType_ForceUpdate, futu.EndForceUpdate},
	} {
		srv := futusim.Start(t, futusim.Options{})
		// A long redial keeps the reason readable before the next session.
		c := futu.NewConnector(futu.Config{Addr: srv.Addr(), Account: simAccount, ClientVersion: 333, RedialDelay: time.Minute})
		c.Start(context.Background())
		waitReady(t, c)
		srv.PushGatewayEvent(tc.event, "synthetic event text")
		var end *futu.SessionEndError
		waitFor(t, string(tc.want), func() bool { return errors.As(c.Health().LastError, &end) })
		if end.Reason != tc.want || end.Detail != "synthetic event text" || !errors.Is(end, futu.ErrSessionEnded) {
			t.Fatalf("%v: %+v", tc.event, end)
		}
		if h := c.Health(); h.Ready || h.Connected || h.Phase != futu.PhaseDisconnected || h.Environment != futu.EnvironmentUnknown {
			t.Fatalf("%v: %+v", tc.event, h)
		}
		if _, err := c.Accounts(context.Background()); !errors.Is(err, futu.ErrNotConnected) {
			t.Fatalf("%v: accounts after the end: %v", tc.event, err)
		}
		if err := c.RefreshSession(context.Background()); !errors.Is(err, futu.ErrNotConnected) {
			t.Fatalf("%v: refresh after the end: %v", tc.event, err)
		}
		c.Stop()
	}

	srv := futusim.Start(t, futusim.Options{})
	c := connect(t, srv, simAccount)
	waitReady(t, c)
	binding, _ := c.CaptureSession()
	srv.PushGatewayEvent(notify.GtwEventType_GtwEventType_TradePwdChanged, "synthetic trade password changed")
	waitFor(t, "the recorded event", func() bool { return c.Health().LastGatewayEvent == "synthetic trade password changed" })
	if !c.Current(binding) {
		t.Fatal("an event that leaves the session open closed it")
	}
}

// OpenD's program status and logins hold the session out of ready, and a push
// brings it back without waiting for a poll.
func TestProgramStatusAndLoginsHoldTheSessionOutOfReady(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{ProgramStatus: common.ProgramStatusType_ProgramStatusType_UnAgreeDisclaimer})
	c := connect(t, srv, simAccount)
	waitFor(t, "the disclaimer state", func() bool {
		return c.Health().Phase == futu.PhaseProgramStatus && c.Health().State.ProgramStatus == "UnAgreeDisclaimer"
	})
	if c.IsReady() {
		t.Fatal("ready before the disclaimer")
	}
	srv.SetProgramStatus(common.ProgramStatusType_ProgramStatusType_Ready)
	waitReady(t, c)

	srv.SetLoggedIn(false, true)
	waitFor(t, "the quote logout", func() bool { return c.Health().Phase == futu.PhaseNotLoggedIn })
	if c.IsReady() {
		t.Fatal("ready with the quote server logged out")
	}
	srv.SetLoggedIn(true, true)
	waitReady(t, c)
}

// A ForceUpdate program status ends the session as "OpenD requires a newer
// client", not as a state to wait out.
func TestForceUpdateStatusEndsTheSession(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{ProgramStatus: common.ProgramStatusType_ProgramStatusType_ForceUpdate})
	c := connect(t, srv, simAccount)
	var end *futu.SessionEndError
	waitFor(t, "the force-update end", func() bool { return errors.As(c.Health().LastError, &end) })
	if end.Reason != futu.EndForceUpdate {
		t.Fatalf("reason %+v", end)
	}
}

// The scope comes from the list and is never re-scoped: an account the list
// does not report leaves the session unscoped, and a list that drops the pin
// ends the session with a typed reason.
func TestAccountListScopesThePinAndLosingItDropsTheSession(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	unknown := connect(t, srv, 42)
	waitFor(t, "the unscoped phase", func() bool { return unknown.Health().Phase == futu.PhaseUnscoped })
	if _, env := unknown.Scope(); env != futu.EnvironmentUnknown || unknown.IsReady() {
		t.Fatalf("scope of an unreported account: %q", env)
	}

	real := connect(t, srv, realAccount)
	waitReady(t, real)
	if _, env := real.Scope(); env != futu.EnvironmentReal {
		t.Fatalf("real account environment %q", env)
	}

	srv.SetAccountListed(realAccount, false)
	if err := real.RefreshSession(context.Background()); err == nil {
		t.Fatal("a list without the pin left the session open")
	}
	var end *futu.SessionEndError
	waitFor(t, "the scope-lost end", func() bool { return errors.As(real.Health().LastError, &end) })
	if end.Reason != futu.EndScopeLost {
		t.Fatalf("reason %+v", end)
	}

	// The next session scopes afresh once the list reports the pin again.
	srv.SetAccountListed(realAccount, true)
	waitReady(t, real)
}

// A trade server not logged in refuses the list; the session stays open and
// scopes once the login is back.
func TestScopeWaitsForTheTradeLogin(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{KeepAliveSeconds: 1})
	srv.SetLoggedIn(true, false)
	c := connect(t, srv, simAccount)
	waitFor(t, "the logged-out phase", func() bool { return c.Health().Phase == futu.PhaseNotLoggedIn })
	if _, env := c.Scope(); env != futu.EnvironmentUnknown {
		t.Fatalf("scope without a login: %q", env)
	}
	srv.SetLoggedIn(true, true)
	waitReady(t, c)
}

// Quota and quote-right pushes reach what the status surface reads.
func TestQuotaAndQuoteRightPushesUpdateHealth(t *testing.T) {
	srv := futusim.Start(t, futusim.Options{})
	c := connect(t, srv, simAccount)
	waitReady(t, c)
	srv.PushQuotaState()
	waitFor(t, "the quota", func() bool {
		h := c.Health()
		return h.SubscriptionQuota == 100 && h.HistoryQuota == 100
	})
	srv.PushQuoteRights(3, 2) // QotRight_Level2, QotRight_Level1
	waitFor(t, "the quote rights", func() bool {
		h := c.Health()
		return h.QuoteRights["HK"] == "Level2" && h.QuoteRights["US"] == "Level1"
	})
}

// The simulator fails the test that owns it on a trade-unlock frame, whoever
// sent it; no encoder in the client can.
func TestSimulatorRefusesTheTradeUnlock(t *testing.T) {
	var reason string
	srv, err := futusim.Listen(futusim.Options{OnForbidden: func(r string) { reason = r }})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	nc, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if err := futu.WriteFrame(nc, futu.Frame{ProtoID: 2005, Serial: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := futu.ReadFrame(nc); err == nil {
		t.Fatal("the simulator answered a trade unlock")
	}
	if srv.ForbiddenFrames() != 1 || reason == "" {
		t.Fatalf("forbidden %d reason %q", srv.ForbiddenFrames(), reason)
	}
}

// fixtureScenario is one scripted exchange: the simulator it runs against, the
// requests the client sends, and what to do after them.
type fixtureScenario struct {
	name     string
	options  futusim.Options
	setup    func(*futusim.Server)
	requests []string
	after    func(*futusim.Server)
	pushed   int
}

func fixtureScenarios() []fixtureScenario {
	disclaimer := common.ProgramStatusType_ProgramStatusType_UnAgreeDisclaimer
	return []fixtureScenario{
		{name: "init_connect_ok", requests: []string{"init_connect"}},
		{name: "init_connect_rsa_required", options: futusim.Options{RequireRSA: true}, requests: []string{"init_connect"}},
		{name: "init_connect_old_server", options: futusim.Options{ServerVersion: futu.MinServerVersion - 1}, requests: []string{"init_connect"}},
		{name: "keep_alive", requests: []string{"init_connect", "keep_alive"}},
		{name: "global_state_ready", requests: []string{"init_connect", "global_state"}},
		{name: "global_state_disclaimer", options: futusim.Options{ProgramStatus: disclaimer}, requests: []string{"init_connect", "global_state"}},
		{name: "notify_kicked_out", requests: []string{"init_connect"}, after: (*futusim.Server).KickOut, pushed: 1},
		{name: "qot_sub_ok", requests: []string{"init_connect", "qot_sub", "qot_basic"}, after: (*futusim.Server).PushQuotes, pushed: 1},
		{name: "qot_sub_quota_spent", setup: func(s *futusim.Server) { s.Book().SetSubscriptionQuota(0) }, requests: []string{"init_connect", "qot_sub", "qot_sub_info"}},
		{name: "qot_snapshot_ok", requests: []string{"init_connect", "qot_snapshot"}},
		{name: "qot_history_ok", requests: []string{"init_connect", "qot_history", "qot_history_quota"}},
		{name: "qot_static_ok", requests: []string{"init_connect", "qot_static"}},
		{name: "trd_funds_ok", requests: []string{"init_connect", "trd_funds"}},
		{name: "trd_positions_ok", requests: []string{"init_connect", "trd_positions"}},
		{name: "trd_orders_ok", setup: fixtureOrders, requests: []string{"init_connect", "trd_orders", "trd_fills"}},
		{name: "trd_max_ok", requests: []string{"init_connect", "trd_max"}},
		{name: "trd_max_cash_refused", requests: []string{"init_connect", "trd_max_cash"}},
	}
}

// fixtureOrders places one filled order on the simulated account.
func fixtureOrders(s *futusim.Server) {
	if _, _, err := s.Book().PlaceOrder(simAccount, "HK.00700", futusim.Buy, 100, nil); err != nil {
		panic(err)
	}
}

func fixturePath(name string) string { return filepath.Join("testdata", "frames", name+".hex") }

// One set of frames, two readers: the simulator must still emit exactly these
// bytes for this script, and session_replay_test.go replays them at the
// client. -record rewrites them from the simulator.
func TestFixturesTheSimulatorReemitsTheFramesByteExact(t *testing.T) {
	for _, sc := range fixtureScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			options := sc.options
			recorder := &futusim.Recorder{}
			options.Record = recorder
			options.Book = futusim.Default()
			srv := futusim.Start(t, options)
			clock := options.Book.Clock.Now()
			nc, err := net.Dial("tcp", srv.Addr())
			if err != nil {
				t.Fatal(err)
			}
			defer nc.Close()
			if sc.setup != nil {
				sc.setup(srv)
			}
			for i, name := range sc.requests {
				if _, err := nc.Write(futu.FixtureRequest(name, uint32(i+1), clock)); err != nil {
					t.Fatal(err)
				}
				if _, err := futu.ReadFrame(nc); err != nil {
					t.Fatal(err)
				}
			}
			if sc.after != nil {
				sc.after(srv)
				for range sc.pushed {
					if _, err := futu.ReadFrame(nc); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := recorder.Steps()
			path := fixturePath(sc.name)
			if *record {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := futusim.WriteScript(path, got); err != nil {
					t.Fatal(err)
				}
			}
			want, err := futusim.ReadScript(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%d frames, fixture has %d", len(got), len(want))
			}
			for i := range got {
				if got[i].Direction != want[i].Direction || !bytes.Equal(got[i].Raw, want[i].Raw) {
					t.Fatalf("frame %d differs:\n got %s %x\nwant %s %x", i, got[i].Direction, got[i].Raw, want[i].Direction, want[i].Raw)
				}
			}
		})
	}
}
