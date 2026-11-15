package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/discover"
)

// notStartedAttempter is a Gateway that accepted the API connection and sent
// messages after START_API while the session did not start.
type notStartedAttempter struct {
	fakeAttempter
	messages int
	err      error
}

func (f *notStartedAttempter) SessionStartFailure() (int, error) { return f.messages, f.err }

// quietScheduleServer is a daemon whose logging schedule declares Monday
// 00:24 CEST off duty, so a Gateway outage logs at INFO.
func quietScheduleServer(t *testing.T, out *lockedBuffer) *Server {
	t.Helper()
	var clock atomic.Int64
	clock.Store(scheduleTime(t, "2026-10-04T22:24:00Z").UnixNano())
	s := newTestServer(t)
	s.cfg.Daemon = config.Daemon{LogCalendarMode: "scheduled", LogMarkets: []string{"us_equity"}, LogBeforeOpenMinutes: new(120), LogAfterCloseMinutes: new(90)}
	s.logger = NewLogger(out, "info")
	s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.startGatewaySchedule(ctx)
	s.gatewaySchedule.wg.Wait()
	return s
}

// A Gateway that accepted the API connection and answered is up with its
// socket enabled: the watchdog and the attempt's verdict say what it sent,
// at WARN whatever the logging schedule declares. On 2026-10-19 the watchdog
// blamed the Gateway's settings while it sent thirteen startup messages and
// the attempt's own error stayed in the debug log. A silent listener keeps
// the checklist and the schedule.
func TestSessionStartFailureIsTheVerdictAtWarn(t *testing.T) {
	ep := discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15, PortOrigin: discover.OriginPinned}
	startErr := errors.New("ibkr start api failure: nextValidId not received during startup after 13 messages within 5s")
	const want = "gateway 127.0.0.1:4001 accepted the API connection and answered, but the session did not start: ibkr start api failure: nextValidId not received during startup after 13 messages within 5s"

	t.Run("watchdog", func(t *testing.T) {
		var out lockedBuffer
		s := quietScheduleServer(t, &out)
		s.handshakeWatchdog(t.Context(), &notStartedAttempter{messages: 13, err: startErr}, time.Millisecond, ep)
		if s.lastConnectError != want {
			t.Fatalf("watchdog hint = %q", s.lastConnectError)
		}
		if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], want) {
			t.Fatalf("watchdog log:\n%s", out.String())
		}
	})

	t.Run("silent listener", func(t *testing.T) {
		var out lockedBuffer
		s := quietScheduleServer(t, &out)
		s.handshakeWatchdog(t.Context(), &notStartedAttempter{messages: 0, err: startErr}, time.Millisecond, ep)
		if !strings.Contains(s.lastConnectError, "not responding to TWS handshake") {
			t.Fatalf("silent-listener hint = %q", s.lastConnectError)
		}
		if logged := out.String(); strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "level=INFO") {
			t.Fatalf("off-duty silent listener not quiet:\n%s", logged)
		}
	})

	// A primary whose session did not start and a silent alternate fail
	// differently in every cycle. The incident keeps its attention, so the
	// alternate does not flip it back off and each cycle re-emit a WARN.
	t.Run("mixed candidates", func(t *testing.T) {
		var out lockedBuffer
		s := quietScheduleServer(t, &out)
		mixed := ep
		mixed.Alternates = []int{7496}
		s.attempterFactory = func(cand discover.Endpoint) connectAttempter {
			if cand.Port == 4001 {
				return &notStartedAttempter{messages: 13, err: startErr}
			}
			return &fakeAttempter{lastError: "synthetic silence"}
		}
		for range 5 {
			s.connectWithFailover(t.Context(), mixed)
		}
		warned := 0
		for line := range strings.SplitSeq(out.String(), "\n") {
			if strings.Contains(line, "Gateway unavailable") && strings.Contains(line, "level=WARN") {
				warned++
			}
		}
		if warned != 1 {
			t.Fatalf("five cycles warned %d times, want once:\n%s", warned, out.String())
		}
	})

	t.Run("attempt and exhaustion", func(t *testing.T) {
		var out lockedBuffer
		s := quietScheduleServer(t, &out)
		s.attempterFactory = func(discover.Endpoint) connectAttempter {
			return &notStartedAttempter{messages: 13, err: startErr}
		}
		s.connectWithFailover(t.Context(), ep)
		if s.lastConnectError != want {
			t.Fatalf("exhaustion verdict = %q", s.lastConnectError)
		}
		logged := out.String()
		if strings.Contains(logged, "Enable ActiveX") || !strings.Contains(logged, "level=WARN") || strings.Contains(logged, "level=INFO") {
			t.Fatalf("session-start failure log:\n%s", logged)
		}
	})
}
