package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A stalled lane is redialled once it has been stalled for ten minutes (owner
// decision #22), not before, and not again within the hour; a stall that
// outlasts that redial is reported once; recovery clears the report.
func TestAnswerPathRedialRules(t *testing.T) {
	since := time.Date(2026, 9, 28, 6, 31, 0, 0, time.UTC)
	lane := &answerPathLane{}
	step := func(at time.Duration, stalled bool) (bool, bool) {
		return answerPathRedial(lane, since, stalled, since.Add(at))
	}
	if redial, warn := step(9*time.Minute+59*time.Second, true); redial || warn {
		t.Fatal("redialled before ten stalled minutes")
	}
	if redial, _ := step(10*time.Minute, true); !redial {
		t.Fatal("not redialled after ten stalled minutes")
	}
	// The new session stalls again at once: no second redial inside the hour,
	// one report that the Gateway needs a restart.
	since = since.Add(12 * time.Minute)
	if redial, warn := step(10*time.Minute, true); redial || !warn {
		t.Fatalf("second stall inside the hour: redial %v warn %v, want no redial and one report", redial, warn)
	}
	if redial, warn := step(20*time.Minute, true); redial || warn {
		t.Fatal("a stall that outlasted its redial was reported twice")
	}
	if redial, warn := step(21*time.Minute, false); redial || warn || lane.quietWarned {
		t.Fatal("recovery kept the stall report")
	}
	// Past the quiet hour a new stall is redialled again.
	since = since.Add(2 * time.Hour)
	if redial, _ := step(10*time.Minute, true); !redial {
		t.Fatal("a stall more than an hour after the last redial was not redialled")
	}
}

// Status reports each lane; without a session a lane is no_session, and the
// breadth lane is omitted until it has a connector.
func TestAnswerPathsWithoutSessions(t *testing.T) {
	s := &Server{}
	rows := s.answerPaths(time.Now())
	if len(rows) != 1 || rows[0].Lane != rpc.AnswerPathLanePrimary || rows[0].State != rpc.AnswerPathNoSession {
		t.Fatalf("answer paths = %+v", rows)
	}
	s.answerPath.mu.Lock()
	s.answerPath.lane(rpc.AnswerPathLanePrimary).redialedAt = time.Date(2026, 9, 28, 6, 41, 0, 0, time.UTC)
	s.answerPath.mu.Unlock()
	if rows := s.answerPaths(time.Now()); rows[0].RedialedAt.IsZero() {
		t.Fatal("the last redial was not reported")
	}
}

// A breadth fetch that waited out its budget is a timeout, and one the
// connector refused while the Gateway held history is a transport failure.
func TestBreadthFetchFailuresAreClassified(t *testing.T) {
	cases := []struct {
		err  error
		want spx.RefreshFailure
	}{
		{fmt.Errorf("historical data timeout for AAA after 20s: %w", context.DeadlineExceeded), spx.RefreshFailureTimeout},
		{fmt.Errorf("%w: %w", spx.ErrGatewayStalled, ibkrlib.ErrHistoricalServiceStalled), spx.RefreshFailureTransport},
		{context.Canceled, spx.RefreshFailureCancelled},
		{errors.New("coded broker refusal"), spx.RefreshFailureFetch},
	}
	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
			e := spx.New(spx.NewStore(t.TempDir()), failingBarFetcher{err: tc.err}, spx.Options{Clock: func() time.Time { return now }, Workers: 1, Members: []string{"AAA"}})
			_ = e.Refresh(context.Background())
			if progress, ok := e.Progress(); !ok || progress.LastFailure != tc.want {
				t.Fatalf("last failure = %+v (ok %v), want %s", progress, ok, tc.want)
			}
		})
	}
}

type failingBarFetcher struct{ err error }

func (f failingBarFetcher) FetchDaily(context.Context, string, int) ([]spx.Bar, error) {
	return nil, f.err
}
