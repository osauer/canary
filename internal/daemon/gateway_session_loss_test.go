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
}

func startFakeTWS(t *testing.T) *fakeTWS {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTWS{ln: ln}
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
		go f.serve(c)
	}
}

func (f *fakeTWS) serve(c net.Conn) {
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
	for {
		if _, err := readTWSFrame(r); err != nil {
			return
		}
		f.requests.Add(1)
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
	now := scheduleTime(t, at)
	out := &lockedBuffer{}
	s := newTestServer(t)
	s.cfg.Gateway.Port = new(f.port())
	s.cfg.Gateway.TLS = new(false)
	s.cfg.Daemon = config.Daemon{LogCalendarMode: "scheduled", LogMarkets: []string{"us_equity"}, LogBeforeOpenMinutes: new(120), LogAfterCloseMinutes: new(90)}
	s.logger = NewLogger(out, "debug")
	s.now = func() time.Time { return now }
	s.gatewaySchedule.view.Store(compileGatewaySchedule(now, []marketcal.Market{marketcal.MarketUSEquity, marketcal.MarketUSOptions}, 2*time.Hour, 90*time.Minute, false))
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
