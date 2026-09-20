package daemon

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func utcDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func TestLatestCompletedFlexDateTargetsTheLatestSessionDay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{name: "saturday targets friday", now: berlinTestTime(t, 2026, 9, 19, 7, 0), want: utcDate(2026, 9, 18)},
		{name: "sunday still targets friday", now: berlinTestTime(t, 2026, 9, 20, 19, 0), want: utcDate(2026, 9, 18)},
		{name: "monday before eastern midnight still targets friday", now: berlinTestTime(t, 2026, 9, 21, 5, 0), want: utcDate(2026, 9, 18)},
		{name: "tuesday after eastern midnight targets monday", now: berlinTestTime(t, 2026, 9, 22, 6, 31), want: utcDate(2026, 9, 21)},
		{name: "tuesday after labor day targets the friday before", now: berlinTestTime(t, 2026, 9, 8, 6, 31), want: utcDate(2026, 9, 4)},
		{name: "early close counts as a session", now: berlinTestTime(t, 2026, 11, 28, 7, 0), want: utcDate(2026, 11, 27)},
		{name: "holiday beside a weekend walks back to the early close", now: berlinTestTime(t, 2026, 12, 28, 7, 0), want: utcDate(2026, 12, 24)},
		{name: "outside calendar coverage keeps the plain completed date", now: berlinTestTime(t, 2029, 3, 4, 12, 0), want: utcDate(2029, 3, 3)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := latestCompletedFlexDate(test.now); !got.Equal(test.want) {
				t.Fatalf("latest completed date = %s, want %s", got, test.want)
			}
		})
	}
}

func TestNextFlexDailyWindowFollowsTheNextSessionDay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target time.Time
		want   time.Time
	}{
		{name: "friday waits for tuesday morning", target: utcDate(2026, 9, 18), want: berlinTestTime(t, 2026, 9, 22, 6, 30)},
		{name: "monday waits for wednesday morning, after tuesday completes", target: utcDate(2026, 9, 21), want: berlinTestTime(t, 2026, 9, 23, 6, 30)},
		{name: "friday before labor day skips the holiday", target: utcDate(2026, 9, 4), want: berlinTestTime(t, 2026, 9, 9, 6, 30)},
		{name: "christmas eve waits past the holiday weekend", target: utcDate(2026, 12, 24), want: berlinTestTime(t, 2026, 12, 29, 6, 30)},
		{name: "outside calendar coverage names no window", target: utcDate(2029, 3, 2)},
		{name: "zero target names no window"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := nextFlexDailyWindow(test.target)
			if test.want.IsZero() {
				if !got.IsZero() {
					t.Fatalf("next window = %s, want none", got)
				}
				return
			}
			if !got.Equal(test.want) {
				t.Fatalf("next window = %s, want %s", got, test.want)
			}
		})
	}
}

// newFlexScheduleTestServer binds a Flex fetch state to a private store and
// records every broker fetch the scheduler starts, failing each one so the
// test can steer state by hand.
func newFlexScheduleTestServer(t *testing.T, clock *time.Time) (*Server, *[]time.Time) {
	t.Helper()
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	s := &Server{
		now:    func() time.Time { return *clock },
		cfg:    &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "daily-report"}},
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
	if err := s.flexFetch.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	targets := &[]time.Time{}
	s.flexFetchOnceFn = func(_ context.Context, target time.Time) (flexFetchOutcome, error) {
		*targets = append(*targets, target)
		return flexFetchOutcome{}, errors.New("test fetch stopped")
	}
	return s, targets
}

func persistFlexScheduleTestState(t *testing.T, s *Server, state flexFetchStateV2) {
	t.Helper()
	state.Version = flexFetchStateVersion
	state.QueryFingerprint = flexQueryFingerprint(s.cfg.Flex.QueryID)
	s.flexFetch.mu.Lock()
	defer s.flexFetch.mu.Unlock()
	s.flexFetch.state = state
	if err := s.flexFetch.persistLocked(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestFlexFetchStaysCurrentAcrossNonSessionDays(t *testing.T) {
	saturdayRun := berlinTestTime(t, 2026, 9, 19, 7, 7)
	now := saturdayRun
	s, targets := newFlexScheduleTestServer(t, &now)
	friday := utcDate(2026, 9, 18)
	writeFlexFixture(t, "flex-"+flexQueryFingerprint("daily-report")+"-friday.xml", "20260919;070700", "20250919", "20260918", "")
	persistFlexScheduleTestState(t, s, flexFetchStateV2{
		Stage: rpc.ReconReportStateCurrent, LastAttempt: saturdayRun.UTC(), LastSuccess: saturdayRun.UTC(),
		TargetDate: friday, CoverageTo: friday,
	})
	tuesdayWindow := berlinTestTime(t, 2026, 9, 22, 6, 30)

	expectCurrent := func(note string) {
		t.Helper()
		status := s.flexFetchStatusAt(now)
		if status.State != rpc.ReconReportStateCurrent || !status.NextAttempt.Equal(tuesdayWindow) {
			t.Fatalf("%s: status=%+v, want current with next attempt %s", note, status, tuesdayWindow)
		}
		s.maybeFetchFlex(t.Context())
		s.flexFetch.wg.Wait()
		if len(*targets) != 0 {
			t.Fatalf("%s: scheduler fetched %v, want no broker call", note, *targets)
		}
	}
	now = berlinTestTime(t, 2026, 9, 20, 19, 0)
	expectCurrent("sunday evening")
	now = berlinTestTime(t, 2026, 9, 21, 9, 0)
	expectCurrent("monday, before the session day has completed")
	now = berlinTestTime(t, 2026, 9, 22, 5, 0)
	expectCurrent("tuesday before eastern midnight")

	now = berlinTestTime(t, 2026, 9, 22, 6, 29)
	if status := s.flexFetchStatusAt(now); status.State != rpc.ReconReportStateWaiting || status.Reason != rpc.ReconReportReasonBeforeDailyWindow {
		t.Fatalf("tuesday before the window: status=%+v, want waiting before the daily window", status)
	}
	now = berlinTestTime(t, 2026, 9, 22, 6, 31)
	if status := s.flexFetchStatusAt(now); status.State != rpc.ReconReportStateDue {
		t.Fatalf("tuesday in the window: status=%+v, want due", status)
	}
	s.maybeFetchFlex(t.Context())
	s.flexFetch.wg.Wait()
	if monday := utcDate(2026, 9, 21); len(*targets) != 1 || !(*targets)[0].Equal(monday) {
		t.Fatalf("tuesday fetch targets=%v, want exactly %s", *targets, monday)
	}
}

func TestFlexFetchLeavesShortCoverageOpenForLatchRechecks(t *testing.T) {
	saturdayRun := berlinTestTime(t, 2026, 9, 19, 7, 7)
	now := saturdayRun
	s, _ := newFlexScheduleTestServer(t, &now)
	// IBKR answered with a range that stops short of the completed target.
	writeFlexFixture(t, "flex-"+flexQueryFingerprint("daily-report")+"-short.xml", "20260919;070700", "20250918", "20260917", "")
	persistFlexScheduleTestState(t, s, flexFetchStateV2{
		Stage: rpc.ReconReportStateCurrent, LastAttempt: saturdayRun.UTC(), LastSuccess: saturdayRun.UTC(),
		TargetDate: utcDate(2026, 9, 18), CoverageTo: utcDate(2026, 9, 17),
	})
	now = berlinTestTime(t, 2026, 9, 20, 19, 0)
	status := s.flexFetchStatusAt(now)
	if status.State != rpc.ReconReportStateCurrent || !status.NextAttempt.IsZero() {
		t.Fatalf("short coverage status=%+v, want current with no next window claimed", status)
	}
}

func TestFlexFetchRecoversFromAWeekendDatedTarget(t *testing.T) {
	// The persisted cursor is what an upgraded daemon inherits from the
	// incident: a Sunday run asked for a Saturday-ended range, IBKR answered
	// with code 1025, and a retry was scheduled for the same weekend target.
	saturdayRun := berlinTestTime(t, 2026, 9, 19, 7, 7)
	now := berlinTestTime(t, 2026, 9, 20, 19, 30)
	s, targets := newFlexScheduleTestServer(t, &now)
	writeFlexFixture(t, "flex-"+flexQueryFingerprint("daily-report")+"-friday.xml", "20260919;070700", "20250919", "20260918", "")
	persistFlexScheduleTestState(t, s, flexFetchStateV2{
		Stage: rpc.ReconReportStateRetryScheduled, LastAttempt: berlinTestTime(t, 2026, 9, 20, 19, 29).UTC(), LastSuccess: saturdayRun.UTC(),
		LastReason: rpc.ReconReportReasonResponseInvalid, LastBrokerCode: "1025", LastRetryable: true,
		TargetDate: utcDate(2026, 9, 19), CoverageTo: utcDate(2026, 9, 18), NextAttempt: berlinTestTime(t, 2026, 9, 20, 19, 59).UTC(),
	})
	if status := s.flexFetchStatusAt(now); status.State != rpc.ReconReportStateDue {
		t.Fatalf("weekend-dated cursor status=%+v, want due for the session-day target", status)
	}
	s.maybeFetchFlex(t.Context())
	s.flexFetch.wg.Wait()
	if friday := utcDate(2026, 9, 18); len(*targets) != 1 || !(*targets)[0].Equal(friday) {
		t.Fatalf("recovery fetch targets=%v, want exactly %s", *targets, friday)
	}
}
