package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TWS's 23:45 restart lands inside the breadth refresh that starts at 22:35.
// On 2026-10-01, 10-02, 10-06 and 10-07 a read went out on the bulk lane just
// before TWS ended both sessions; nothing ended that read, so the refresh
// waited out its two-minute budget and only then logged, at WARN and after
// the gateway incident had closed, that the lane was not ready, that six
// reads saw the session change and that a seventh found no connector. The
// refresh must end with the session, and the lane outage is its connect
// path's to report, not the engine's.
func TestTWSRestartEndsBreadthRefreshWithTheSession(t *testing.T) {
	f := startFakeTWS(t)
	s, out := sessionLossServer(t, f, "2026-10-07T18:00:00Z") // on duty: a warning would show
	lane := startBreadthLaneForTest(t, s)
	members := []string{"AAA", "BBB", "CCC", "DDD"}
	s.breadth = spx.New(spx.NewStore(t.TempDir()), newBreadthFetcher(s.breadthGatewayConnector), spx.Options{
		Logger: s.logger, Clock: s.now, Members: members, Workers: 2,
		HealthGate: s.breadthLaneHealth, ConnectionOutage: s.breadthConnectionOutage,
	})
	for i, sym := range members {
		lane.SeedContractDetails(sym, ibkrlib.ContractDetailsLite{Symbol: sym, SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", ConID: 70001 + i})
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.breadth.Run(ctx)
	// The fake never answers: both workers' reads are out, the third name
	// waits for a worker, as a seventh name did behind six reads on 2026-10-07.
	for deadline := time.Now().Add(5 * time.Second); lane.AnswerPath(time.Now()).InFlight < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the refresh's reads never reached the gateway")
		}
	}
	mark := len(out.String())

	f.rejecting.Store(true)
	f.drop()
	lost := time.Now()
	for {
		if _, slept := s.breadth.NextAttempt(); slept {
			break
		}
		if time.Since(lost) > 10*time.Second {
			t.Fatal("breadth refresh outlived the bulk lane's session")
		}
		time.Sleep(5 * time.Millisecond)
	}

	logged := settledLog(out, mark)
	if warns := append(logLinesAt(logged, "WARN"), logLinesAt(logged, "ERROR")...); len(warns) != 0 {
		t.Fatalf("the lane outage warned from the breadth engine:\n%s", strings.Join(warns, "\n"))
	}
	for _, symptom := range []string{"breadth bulk connector is not ready", "historical session changed during read"} {
		if !strings.Contains(logged, symptom) {
			t.Fatalf("%q missing from the debug log:\n%s", symptom, logged)
		}
	}
}
