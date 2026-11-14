package daemon

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// fakeTWS speaks just enough of the TWS API for a daemon connector to reach
// a ready session: the version handshake and a nextValidId answer to
// startAPI. Every later request is read and ignored. drop ends every live
// session the way TWS's daily restart does; while rejecting, it accepts and
// closes connections the way a restarting TWS turns clients away.
type fakeTWS struct {
	ln        net.Listener
	rejecting atomic.Bool
	requests  atomic.Int64 // frames received after startAPI
	mu        sync.Mutex
	sessions  []net.Conn
	// accepted counts the sessions served; openOrderRequests the
	// reqAllOpenOrders frames. answerOpenOrders, when set, decides whether
	// the nth request of the session'th session gets its openOrderEnd
	// (both from 1); unset, nothing is answered.
	accepted          atomic.Int32
	openOrderRequests atomic.Int32
	answerOpenOrders  func(session, nth int) bool
	// account is sent as managedAccounts after nextValidId. Without it the
	// connector waits out its one-second startup grace for the list.
	account string
}

func startFakeTWS(t *testing.T) *fakeTWS {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTWS{ln: ln, account: "DU1234567"}
	go f.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		f.drop()
	})
	return f
}

func (f *fakeTWS) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeTWS) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		if f.rejecting.Load() {
			_ = c.Close()
			continue
		}
		f.mu.Lock()
		f.sessions = append(f.sessions, c)
		f.mu.Unlock()
		session := int(f.accepted.Add(1))
		go f.serve(c, session)
	}
}

func (f *fakeTWS) serve(c net.Conn, session int) {
	defer c.Close()
	r := bufio.NewReader(c)
	prefix := make([]byte, 4) // "API\0"
	if _, err := io.ReadFull(r, prefix); err != nil {
		return
	}
	if _, err := readTWSFrame(r); err != nil { // version descriptor
		return
	}
	if err := writeTWSFrame(c, []byte("176\x0020261007 23:45:20 CET\x00")); err != nil {
		return
	}
	if _, err := readTWSFrame(r); err != nil { // startAPI
		return
	}
	nextValidID := binary.BigEndian.AppendUint32(nil, 9)
	if err := writeTWSFrame(c, append(nextValidID, "1\x001\x00"...)); err != nil {
		return
	}
	if f.account != "" {
		accounts := binary.BigEndian.AppendUint32(nil, 15)
		if err := writeTWSFrame(c, append(accounts, "1\x00"+f.account+"\x00"...)); err != nil {
			return
		}
	}
	nth := 0
	for {
		frame, err := readTWSFrame(r)
		if err != nil {
			return
		}
		f.requests.Add(1)
		if len(frame) >= 4 && binary.BigEndian.Uint32(frame[:4]) == 16 { // reqAllOpenOrders
			f.openOrderRequests.Add(1)
			nth++
			if f.answerOpenOrders != nil && f.answerOpenOrders(session, nth) {
				end := binary.BigEndian.AppendUint32(nil, 53) // openOrderEnd
				if err := writeTWSFrame(c, append(end, "1\x00"...)); err != nil {
					return
				}
			}
		}
	}
}

// drop ends every live session with a FIN, as TWS's restart does (the
// client then reads EOF, as on 2026-10-07 23:45:00). serve closes the socket
// once the client has hung up, so no unread request turns the FIN into a
// reset.
func (f *fakeTWS) drop() {
	f.mu.Lock()
	sessions := f.sessions
	f.sessions = nil
	f.mu.Unlock()
	for _, c := range sessions {
		_ = c.(*net.TCPConn).CloseWrite()
	}
}

func readTWSFrame(r *bufio.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	body := make([]byte, binary.BigEndian.Uint32(n[:]))
	_, err := io.ReadFull(r, body)
	return body, err
}

func writeTWSFrame(w io.Writer, body []byte) error {
	_, err := w.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...))
	return err
}

// sessionLossServer is a daemon pinned to the fake TWS, with the scheduled
// duty declaration of the operator's config (us_equity, 120 min before, 90
// min after; the daemon adds us_options) published for a fixed clock.
func sessionLossServer(t *testing.T, f *fakeTWS, at string) (*Server, *lockedBuffer) {
	t.Helper()
	return sessionLossServerWith(t, f, at, nil)
}

// sessionLossServerWith is sessionLossServer with configure applied before
// the first connect.
func sessionLossServerWith(t *testing.T, f *fakeTWS, at string, configure func(*Server)) (*Server, *lockedBuffer) {
	t.Helper()
	now := scheduleTime(t, at)
	out := &lockedBuffer{}
	s := newTestServer(t)
	s.cfg.Gateway.Port = new(f.port())
	s.cfg.Gateway.TLS = new(false)
	s.cfg.Gateway.Account = f.account
	s.cfg.Daemon = config.Daemon{LogCalendarMode: "scheduled", LogMarkets: []string{"us_equity"}, LogBeforeOpenMinutes: new(120), LogAfterCloseMinutes: new(90)}
	s.logger = NewLogger(out, "debug")
	s.now = func() time.Time { return now }
	s.gatewaySchedule.view.Store(compileGatewaySchedule(now, []marketcal.Market{marketcal.MarketUSEquity, marketcal.MarketUSOptions}, 2*time.Hour, 90*time.Minute, false))
	if configure != nil {
		configure(s)
	}
	ep, err := discover.Resolve(t.Context(), partialFromConfig(s.cfg.Gateway))
	if err != nil {
		t.Fatal(err)
	}
	s.endpoint = ep
	s.connectWithFailover(t.Context(), ep)
	if c := s.gatewayConnectorForTest(); c == nil || !c.IsReady() || !s.postConnectSetupDone.Load() {
		t.Fatalf("initial session not ready:\n%s", out.String())
	}
	waitRegimePrewarm(t, s)
	t.Cleanup(func() {
		s.stopConnector()
		s.stopBreadthConnector()
	})
	return s, out
}

func (s *Server) gatewayConnectorForTest() *ibkrlib.Connector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connector
}

// startBreadthLaneForTest seats the bulk lane on the fake TWS the way
// postConnectSetup does, without starting the breadth engine.
func startBreadthLaneForTest(t *testing.T, s *Server) *ibkrlib.Connector {
	t.Helper()
	s.mu.Lock()
	s.breadthConnectInFlight = true
	ep := s.endpoint
	s.mu.Unlock()
	s.breadthConnectFlow(t.Context(), ep)
	s.mu.Lock()
	c := s.breadthConnector
	s.mu.Unlock()
	if c == nil || !c.IsReady() {
		t.Fatal("bulk lane not ready")
	}
	return c
}

func waitSessionLost(t *testing.T, cs ...*ibkrlib.Connector) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, c := range cs {
		for c.IsReady() {
			if time.Now().After(deadline) {
				t.Fatal("session loss not observed")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// reconnectForTest runs one reconnect cycle the way triggerReconnect's
// goroutine does, holding the dial slot.
func reconnectForTest(t *testing.T, s *Server) {
	t.Helper()
	s.mu.Lock()
	s.connectInFlight = true
	s.mu.Unlock()
	s.reconnectFlow(t.Context())
	s.mu.Lock()
	s.connectInFlight = false
	s.mu.Unlock()
	if c := s.gatewayConnectorForTest(); c == nil || !c.IsReady() {
		t.Fatal("reconnect did not seat a new session")
	}
	waitRegimePrewarm(t, s)
}

// waitRegimePrewarm lets postConnectSetup's seeded pre-warm finish, so a
// session ended by the test cannot cut it short (a real Gateway stays up
// for more than the microseconds the seeded pre-warm takes).
func waitRegimePrewarm(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.regimePrewarming.Load() {
		if time.Now().After(deadline) {
			t.Fatal("regime pre-warm did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func logLinesAt(logged, level string) []string {
	var out []string
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, "level="+level) {
			out = append(out, line)
		}
	}
	return out
}

// settledLog waits for the reader goroutines' last lines, then returns
// everything logged after mark.
func settledLog(out *lockedBuffer, mark int) string {
	time.Sleep(50 * time.Millisecond)
	return out.String()[mark:]
}

// One TWS restart on duty is one incident: a single warning opens it with
// the cause, a single warning closes it, and the wire layer's per-client
// EOF/closed/disconnected and startAPI lines stay in the debug log. The
// night of 2026-10-07 logged six wire warnings for the same restart and no
// incident line at all.
func TestLostSessionOpensOneIncidentOnDuty(t *testing.T) {
	f := startFakeTWS(t)
	s, out := sessionLossServer(t, f, "2026-10-07T18:00:00Z") // Wed 20:00 CEST, US session open
	old := s.gatewayConnectorForTest()
	mark := len(out.String())

	f.drop()
	waitSessionLost(t, old)
	reconnectForTest(t, s)

	logged := settledLog(out, mark)
	warns := logLinesAt(logged, "WARN")
	if len(warns) != 2 || !strings.Contains(warns[0], "Gateway unavailable: IBKR API session lost: EOF") || !strings.Contains(warns[1], "Gateway connection recovered") {
		t.Fatalf("want one incident warning and one recovery warning, got %d:\n%s", len(warns), strings.Join(warns, "\n"))
	}
	if errs := logLinesAt(logged, "ERROR"); len(errs) != 0 {
		t.Fatalf("session loss logged errors:\n%s", strings.Join(errs, "\n"))
	}
	if !strings.Contains(logged, "level=DEBUG msg=\"Disconnection detected (Client ID: 15): EOF\"") {
		t.Fatalf("wire diagnostics missing from the debug log:\n%s", logged)
	}
}

// Off duty the same restart is information: the operator declared those
// hours as not needing attention (2026-10-07 23:45:00 CEST is the first
// second after the us_options duty window).
func TestLostSessionOffDutyLogsInfoOnly(t *testing.T) {
	f := startFakeTWS(t)
	s, out := sessionLossServer(t, f, "2026-10-07T21:45:00.172Z") // Wed 23:45:00 CEST
	old := s.gatewayConnectorForTest()
	mark := len(out.String())

	f.drop()
	waitSessionLost(t, old)
	reconnectForTest(t, s)

	logged := settledLog(out, mark)
	if warns := append(logLinesAt(logged, "WARN"), logLinesAt(logged, "ERROR")...); len(warns) != 0 {
		t.Fatalf("off-duty restart warned:\n%s", strings.Join(warns, "\n"))
	}
	infos := logLinesAt(logged, "INFO")
	opened, recovered := 0, 0
	for _, line := range infos {
		if strings.Contains(line, "Gateway unavailable: IBKR API session lost") {
			opened++
		}
		if strings.Contains(line, "Gateway connection recovered") {
			recovered++
		}
	}
	if opened != 1 || recovered != 1 {
		t.Fatalf("want one INFO incident and one INFO recovery, got %d and %d:\n%s", opened, recovered, logged)
	}
}

// TWS ends both of the daemon's clients at once. The bulk lane shares the
// primary lane's incident: its wire lines stay in debug and a failed bulk
// redial while the primary session is lost joins that incident instead of
// warning on its own.
func TestBothLanesLostShareOneIncident(t *testing.T) {
	f := startFakeTWS(t)
	s, out := sessionLossServer(t, f, "2026-10-07T18:00:00Z")
	primary := s.gatewayConnectorForTest()
	bulk := startBreadthLaneForTest(t, s)
	mark := len(out.String())

	f.rejecting.Store(true)
	f.drop()
	waitSessionLost(t, primary, bulk)
	// The bulk lane notices first and cannot redial while TWS restarts.
	s.mu.Lock()
	s.breadthConnectInFlight = true
	ep := s.endpoint
	s.mu.Unlock()
	s.breadthConnectFlow(t.Context(), ep)
	f.rejecting.Store(false)
	reconnectForTest(t, s)
	startBreadthLaneForTest(t, s)

	logged := settledLog(out, mark)
	warns := logLinesAt(logged, "WARN")
	if len(warns) != 2 || !strings.Contains(warns[0], "IBKR API session lost") || !strings.Contains(warns[1], "Gateway connection recovered") {
		t.Fatalf("want one incident for both lanes, got %d warnings:\n%s", len(warns), strings.Join(warns, "\n"))
	}
	if errs := logLinesAt(logged, "ERROR"); len(errs) != 0 {
		t.Fatalf("session loss logged errors:\n%s", strings.Join(errs, "\n"))
	}
	if !strings.Contains(logged, "Disconnection detected (Client ID: 16)") {
		t.Fatalf("bulk lane loss missing from the debug log:\n%s", logged)
	}
}

// A session the answer-path supervisor drops on purpose was announced by the
// supervisor; the reconnect that follows must not open a second warning.
func TestAnswerPathRedialOpensNoSecondWarning(t *testing.T) {
	f := startFakeTWS(t)
	s, out := sessionLossServer(t, f, "2026-10-07T18:00:00Z")
	old := s.gatewayConnectorForTest()
	mark := len(out.String())

	now := s.now()
	s.redialStalledLane(rpc.AnswerPathLanePrimary, old, now.Add(-10*time.Minute), now)
	waitSessionLost(t, old)
	// redialStalledLane's triggerReconnect needs serverCtx; run the cycle here.
	reconnectForTest(t, s)

	logged := settledLog(out, mark)
	warns := logLinesAt(logged, "WARN")
	if len(warns) != 1 || !strings.Contains(warns[0], "redialling that connection") {
		t.Fatalf("want only the supervisor's warning, got %d:\n%s", len(warns), strings.Join(warns, "\n"))
	}
}

// The reads that fail in the instant between the socket's end and the
// reconnect are symptoms of the same loss. The 23:45:00.223 history lines of
// 2026-10-07 landed 51 ms after the EOF, before anything had redialled.
func TestHistoryReadInSessionGapJoinsIncident(t *testing.T) {
	f := startFakeTWS(t)
	s, out := sessionLossServer(t, f, "2026-10-07T18:00:00Z")
	old := s.gatewayConnectorForTest()
	time.Sleep(50 * time.Millisecond) // let postConnectSetup's requests land
	mark := len(out.String())
	sent := f.requests.Load()

	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SPY"}, Range: "1Y"}
	read := make(chan error, 1)
	go func() {
		// The fake never answers; the deadline ends the read soon after the
		// session is gone, and the reader then sees the session change.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := s.fetchMarketHistoryDays(ctx, p, 0, s.now())
		read <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.requests.Load() == sent {
		if time.Now().After(deadline) {
			t.Fatal("history request never reached the gateway")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.drop()
	waitSessionLost(t, old)
	var err error
	select {
	case err = <-read:
	case <-time.After(10 * time.Second):
		t.Fatal("history read outlived its session")
	}
	if !errors.Is(err, ibkrlib.ErrIBKRUnavailable) || !strings.Contains(err.Error(), "broker session changed") {
		t.Fatalf("a read cut by the session loss is a broker outage: %v", err)
	}
	saved := &storedMarketHistory{}
	saved.Result.End = s.now()
	s.logMarketHistoryFallback(p, saved, err)
	reconnectForTest(t, s)

	logged := settledLog(out, mark)
	warns := logLinesAt(logged, "WARN")
	if len(warns) != 2 || strings.Contains(logged, "level=WARN msg=\"market history") {
		t.Fatalf("history symptom warned beside the incident:\n%s", strings.Join(warns, "\n"))
	}
}

// The bulk lane's own dial failure, with the primary lane healthy, follows
// the duty declaration like every gateway incident: WARN on duty, INFO off
// duty, and only the first failure of a streak.
func TestBreadthConnectFailureFollowsDuty(t *testing.T) {
	for _, tc := range []struct {
		at, level string
	}{{"2026-10-07T18:00:00Z", "WARN"}, {"2026-10-07T22:30:00Z", "INFO"}} {
		t.Run(tc.level, func(t *testing.T) {
			now := scheduleTime(t, tc.at)
			out := &lockedBuffer{}
			s := &Server{cfg: &config.Resolved{Daemon: config.Daemon{LogCalendarMode: "scheduled"}}, logger: NewLogger(out, "info"), now: func() time.Time { return now }}
			s.gatewaySchedule.view.Store(compileGatewaySchedule(now, []marketcal.Market{marketcal.MarketUSEquity, marketcal.MarketUSOptions}, 2*time.Hour, 90*time.Minute, false))
			s.breadthConnectWarnf("breadth bulk connector: not ready after Start (cid=%d); skipping", 16)
			s.releaseBreadthConnect(false)
			s.breadthConnectWarnf("breadth bulk connector: not ready after Start (cid=%d); skipping", 16)
			lines := logLinesAt(out.String(), tc.level)
			if len(lines) != 1 || strings.Count(out.String(), "breadth bulk connector") != 1 {
				t.Fatalf("want one %s line:\n%s", tc.level, out.String())
			}
		})
	}
}

// postConnectSetup dials the bulk lane inline while the primary's connect
// still holds the dial slot. A ready primary proves the gateway is up, so a
// bulk dial TWS turns away there (a zombie session holding client ID 16, a
// handshake that never completes) is the lane's own failure and follows the
// duty schedule; the pending dial used to claim it, leaving it at debug.
func TestBreadthDialFailureBesideReadyPrimaryFollowsDuty(t *testing.T) {
	for _, tc := range []struct {
		at, level string
	}{{"2026-10-07T18:00:00Z", "WARN"}, {"2026-10-07T22:30:00Z", "INFO"}} {
		t.Run(tc.level, func(t *testing.T) {
			f := startFakeTWS(t)
			s, out := sessionLossServer(t, f, tc.at)
			mark := len(out.String())

			f.rejecting.Store(true) // TWS turns the bulk lane's client away
			s.mu.Lock()
			s.connectInFlight = true // as inside postConnectSetup
			s.breadthConnectInFlight = true
			ep := s.endpoint
			s.mu.Unlock()
			s.breadthConnectFlow(t.Context(), ep)
			s.mu.Lock()
			s.connectInFlight = false
			s.mu.Unlock()

			logged := settledLog(out, mark)
			var own []string
			for _, line := range logLinesAt(logged, tc.level) {
				if strings.Contains(line, "breadth bulk connector") {
					own = append(own, line)
				}
			}
			if len(own) != 1 {
				t.Fatalf("want one %s line for the bulk lane's failure beside a ready primary, got %d:\n%s", tc.level, len(own), logged)
			}
			if tc.level == "INFO" && len(logLinesAt(logged, "WARN")) != 0 {
				t.Fatalf("off-duty bulk failure warned:\n%s", logged)
			}
		})
	}
}

// A bulk-lane failure that starts off duty logs INFO; when duty begins while
// it continues, it warns once, as the primary's incident does. The streak
// check kept every later failure at debug, so the lane could stay down all
// session with no warning.
func TestBreadthFailureStartedOffDutyWarnsWhenDutyBegins(t *testing.T) {
	now := scheduleTime(t, "2026-10-08T10:00:00Z") // Thu 12:00 CEST, before the US duty window
	out := &lockedBuffer{}
	s := &Server{cfg: &config.Resolved{Daemon: config.Daemon{LogCalendarMode: "scheduled"}}, logger: NewLogger(out, "debug"), now: func() time.Time { return now }}
	s.gatewaySchedule.view.Store(compileGatewaySchedule(now, []marketcal.Market{marketcal.MarketUSEquity, marketcal.MarketUSOptions}, 2*time.Hour, 90*time.Minute, false))
	fail := func() {
		s.breadthConnectWarnf("breadth bulk connector: not ready after Start (cid=%d); skipping", 16)
		s.releaseBreadthConnect(false)
	}
	fail()
	now = now.Add(30 * time.Second)
	fail()
	now = scheduleTime(t, "2026-10-08T12:00:00Z") // duty began at 11:30Z
	fail()
	now = now.Add(30 * time.Second)
	fail()
	lines := func(level string) []string { return logLinesAt(out.String(), level) }
	if len(lines("INFO")) != 1 || len(lines("WARN")) != 1 || len(lines("DEBUG")) != 2 {
		t.Fatalf("want INFO, then one WARN when duty begins, repeats at debug:\n%s", out.String())
	}

	// A seated lane ends the episode; the next failure is a new one.
	s.releaseBreadthConnect(true)
	now = now.Add(time.Minute)
	fail()
	if len(lines("WARN")) != 2 {
		t.Fatalf("a new failure after recovery did not warn:\n%s", out.String())
	}
}

// A poisoned open-order snapshot stays poisoned until the socket changes, so
// the supervisor redials the primary session at once: one line, no second
// incident from the reconnect. Like a history redial it happens at most once
// an hour (owner decision #22), never under a broker write, and a redialled
// session poisoned again is said once, as the Gateway needing a restart. The
// lines follow the logging schedule. On 2026-10-20 the poison from IBKR's
// nightly reset lasted until a manual restart at 08:25 CEST.
func TestPoisonedOpenOrderSnapshotRedialsThePrimarySession(t *testing.T) {
	for _, tc := range []struct {
		name, at, level string
	}{
		{"on duty", "2026-10-07T18:00:00Z", "WARN"},
		{"off duty", "2026-10-04T22:24:00Z", "INFO"}, // Monday 00:24 CEST
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startFakeTWS(t)
			s, out := sessionLossServer(t, f, tc.at)
			old := s.gatewayConnectorForTest()
			now := s.now()
			s.snapshotPoisonedFn = func(*ibkrlib.Connector) (time.Time, error, bool) {
				return now.Add(-40 * time.Second), errors.New("synthetic: no openOrderEnd"), true
			}

			s.brokerWriteMu.Lock()
			s.redialPoisonedOpenOrderSnapshot(old, now)
			s.brokerWriteMu.Unlock()
			if !old.IsReady() {
				t.Fatal("the session was dropped under a broker write")
			}

			mark := len(out.String())
			s.redialPoisonedOpenOrderSnapshot(old, now)
			// A tick that read the session before another tick dropped it
			// still holds the dropped connector, whose poison never clears;
			// it is not the redialled connection and says nothing.
			s.redialPoisonedOpenOrderSnapshot(old, now)
			waitSessionLost(t, old)
			reconnectForTest(t, s)
			logged := settledLog(out, mark)
			if warns := logLinesAt(logged, "WARN"); tc.level == "INFO" && len(warns) != 0 {
				t.Fatalf("off duty, the redial warned:\n%s", strings.Join(warns, "\n"))
			}
			lines := snapshotLines(logLinesAt(logged, tc.level))
			if len(lines) != 1 || !strings.Contains(lines[0], "open-order snapshot poisoned at") || !strings.Contains(lines[0], "it is redialled") {
				t.Fatalf("want only the supervisor's %s line, got %d:\n%s", tc.level, len(lines), strings.Join(lines, "\n"))
			}

			fresh := s.gatewayConnectorForTest()
			if fresh == nil || fresh == old || !fresh.IsReady() {
				t.Fatal("the primary session was not redialled")
			}
			mark = len(out.String())
			s.redialPoisonedOpenOrderSnapshot(fresh, now.Add(time.Minute))
			s.redialPoisonedOpenOrderSnapshot(fresh, now.Add(2*time.Minute))
			time.Sleep(50 * time.Millisecond)
			if !fresh.IsReady() {
				t.Fatal("a second redial within the hour")
			}
			again := snapshotLines(logLinesAt(settledLog(out, mark), tc.level))
			if len(again) != 1 || !strings.Contains(again[0], "poisoned again") || !strings.Contains(again[0], "until the Gateway restarts") {
				t.Fatalf("want one restart line for a redialled session poisoned again, got:\n%s", strings.Join(again, "\n"))
			}
			s.snapshotPoisonedFn = func(*ibkrlib.Connector) (time.Time, error, bool) { return time.Time{}, nil, false }
			s.redialPoisonedOpenOrderSnapshot(fresh, now.Add(2*time.Hour))
			if !fresh.IsReady() {
				t.Fatal("a session whose snapshot is not poisoned was dropped")
			}
		})
	}
}

// While TWS has no connection to IBKR a redial cannot bring the open orders
// back, so none is spent and the line says what is true. When the link
// returns the read is redialled at once, also inside the hour after an
// earlier redial. On 2026-11-13 the link was down for more than five hours of
// the US session and the open orders stayed unread for 45 minutes after it
// returned.
func TestPoisonedOpenOrderSnapshotWaitsForTheBackendLink(t *testing.T) {
	f := startFakeTWS(t)
	s, out := sessionLossServer(t, f, "2026-10-07T18:00:00Z")
	c := s.gatewayConnectorForTest()
	now := s.now()
	s.snapshotPoisonedFn = func(*ibkrlib.Connector) (time.Time, error, bool) {
		return now.Add(-40 * time.Second), errors.New("synthetic: no openOrderEnd"), true
	}
	// An earlier redial stands inside its quiet hour.
	s.answerPath.mu.Lock()
	s.answerPath.lane(rpc.AnswerPathLanePrimary).snapshotRedialedAt = now.Add(-10 * time.Minute)
	s.answerPath.mu.Unlock()

	link := ibkrlib.BackendLinkReport{Down: true, ChangedAt: now.Add(-time.Minute)}
	s.backendLinkFn = func(*ibkrlib.Connector) ibkrlib.BackendLinkReport { return link }
	mark := len(out.String())
	s.redialPoisonedOpenOrderSnapshot(c, now)
	s.redialPoisonedOpenOrderSnapshot(c, now.Add(30*time.Second))
	time.Sleep(50 * time.Millisecond)
	if !c.IsReady() {
		t.Fatal("the session was redialled while TWS has no connection to IBKR")
	}
	lines := logLinesAt(settledLog(out, mark), "WARN")
	if len(lines) != 1 || !strings.Contains(lines[0], "while TWS has no connection to IBKR") || strings.Contains(lines[0], "Gateway restarts") {
		t.Fatalf("want one line naming the lost link, got:\n%s", strings.Join(lines, "\n"))
	}

	// The link returns: the read is redialled although the hour is not over.
	link = ibkrlib.BackendLinkReport{ChangedAt: now.Add(time.Minute)}
	s.redialPoisonedOpenOrderSnapshot(c, now.Add(90*time.Second))
	waitSessionLost(t, c)
	reconnectForTest(t, s)
	fresh := s.gatewayConnectorForTest()
	if fresh == nil || fresh == c || !fresh.IsReady() {
		t.Fatal("the read was not redialled when the link returned")
	}
	// That redial is the one the returned link earned; a poison after it
	// keeps the hour.
	mark = len(out.String())
	s.redialPoisonedOpenOrderSnapshot(fresh, now.Add(3*time.Minute))
	time.Sleep(50 * time.Millisecond)
	if !fresh.IsReady() {
		t.Fatal("a second redial for the same return of the link")
	}
	if again := snapshotLines(logLinesAt(settledLog(out, mark), "WARN")); len(again) != 1 || !strings.Contains(again[0], "poisoned again") {
		t.Fatalf("want the poisoned-again line after the earned redial, got:\n%s", strings.Join(again, "\n"))
	}
}

func snapshotLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, "open-order snapshot") {
			out = append(out, line)
		}
	}
	return out
}

// The review of 2026-10-20 asked for the whole recovery to be proven. A
// missing openOrderEnd poisons the session's open-order snapshot. Readers
// then fail closed, send nothing and trigger nothing. The supervisor redials
// through the daemon's own reconnect (triggerReconnect) once, however many
// ticks race it. The new session's complete answer restores the inventory
// the proposals read; the protection cache never serves the old session's or
// another account's snapshot; the outcome is said once.
func TestPoisonedSnapshotRecoversOnTheRedialledSession(t *testing.T) {
	f := startFakeTWS(t)
	f.account = "DU1234567"
	// Session 1 answers its first request only; session 2 answers all.
	f.answerOpenOrders = func(session, nth int) bool { return session > 1 || nth == 1 }
	s, out := sessionLossServerWith(t, f, "2026-10-07T18:00:00Z", func(s *Server) {
		s.cfg.Gateway.Account = "DU1234567"
		s.openOrderSnapshotProtocolTimeout = 200 * time.Millisecond
		s.now = time.Now // snapshot receipts carry real times
	})
	old := s.gatewayConnectorForTest()
	ctx := t.Context()

	cached, _, err := s.brokerOpenOrderInventory(ctx, false)
	if err != nil || !cached.Complete {
		t.Fatalf("first inventory=%+v err=%v, want complete", cached, err)
	}
	if _, _, err := s.brokerOpenOrderInventory(ctx, true); err == nil || !strings.Contains(err.Error(), "poisoned") {
		t.Fatalf("unanswered inventory err=%v, want poisoned", err)
	}
	if _, cause, poisoned := old.OpenOrderSnapshotPoisoned(); !poisoned || !errors.Is(cause, ibkrlib.ErrOpenOrderSnapshotPoisoned) {
		t.Fatalf("poisoned=%t cause=%v", poisoned, cause)
	}

	// Readers alone: each is refused as poisoned, none sends a request and
	// none redials.
	requests, accepted := f.openOrderRequests.Load(), f.accepted.Load()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, _, err := s.brokerOpenOrderInventory(ctx, true); err == nil || !strings.Contains(err.Error(), "poisoned") {
				t.Errorf("reader of a poisoned session err=%v, want poisoned", err)
			}
		})
	}
	wg.Wait()
	if got := f.openOrderRequests.Load(); got != requests {
		t.Fatalf("readers of a poisoned session sent %d reqAllOpenOrders, want none", got-requests)
	}
	if !old.IsReady() || f.accepted.Load() != accepted {
		t.Fatal("a reader ended or redialled the session")
	}

	// Supervisor ticks racing each other redial once, through the daemon's
	// own reconnect.
	s.serverCtx = ctx
	mark := len(out.String())
	now := s.now()
	for range 8 {
		wg.Go(func() { s.superviseAnswerPaths(now) })
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	var fresh *ibkrlib.Connector
	for {
		s.mu.Lock()
		fresh, inFlight := s.connector, s.connectInFlight
		s.mu.Unlock()
		if fresh != nil && fresh != old && fresh.IsReady() && !inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the redial did not seat a new session")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitRegimePrewarm(t, s)
	fresh = s.gatewayConnectorForTest()
	time.Sleep(100 * time.Millisecond)
	if got := f.accepted.Load() - accepted; got != 1 {
		t.Fatalf("recovery dialled %d sessions, want one", got)
	}
	if redials := snapshotLines(attentionLines(settledLog(out, mark))); len(redials) != 1 || !strings.Contains(redials[0], "so it is redialled") {
		t.Fatalf("want one redial line, got:\n%s", strings.Join(redials, "\n"))
	}

	if _, _, poisoned := fresh.OpenOrderSnapshotPoisoned(); poisoned {
		t.Fatal("the new session reads poisoned")
	}
	restored, _, err := s.brokerOpenOrderInventory(ctx, false)
	if err != nil || !restored.Complete || restored.Session == cached.Session {
		t.Fatalf("restored inventory=%+v err=%v, want complete on the new session", restored, err)
	}
	if _, _, err := s.brokerOpenOrderInventory(ctx, true); err != nil {
		t.Fatalf("fresh inventory on the new session: %v", err)
	}
	other := s.currentProtectionOrderSnapshotBinding()
	other.scope.Account = "DU7654321"
	if _, ok := s.cachedProtectionOrderSnapshot(other, s.orderNow()); ok {
		t.Fatal("another account's binding reused the cached inventory")
	}

	mark = len(out.String())
	s.superviseAnswerPaths(now.Add(time.Minute))
	s.superviseAnswerPaths(now.Add(2 * time.Minute))
	outcome := snapshotLines(attentionLines(settledLog(out, mark)))
	if len(outcome) != 1 || !strings.Contains(outcome[0], "complete again") {
		t.Fatalf("want the recovery said once, got:\n%s", strings.Join(outcome, "\n"))
	}
}

// attentionLines are the WARN and INFO lines: which one the logging schedule
// picks depends on the hour a real-clock test runs.
func attentionLines(logged string) []string {
	return append(logLinesAt(logged, "WARN"), logLinesAt(logged, "INFO")...)
}
