package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFlexUserAgentUsesCanonicalProductIdentity(t *testing.T) {
	t.Parallel()
	if flexUserAgent != "canary-flex-reconciliation/1" {
		t.Fatalf("Flex User-Agent = %q", flexUserAgent)
	}
}

func TestFlexDailyWindowUsesBerlinMorningAndCalendarDayIdentity(t *testing.T) {
	berlin, err := time.LoadLocation(flexScheduleZone)
	if err != nil {
		t.Fatal(err)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name            string
		now             time.Time
		wantTargetUTC   time.Time
		wantFirstTryUTC time.Time
		wantTargetDate  string
	}{
		{
			name:            "winter",
			now:             time.Date(2026, 1, 21, 5, 0, 0, 0, time.UTC),
			wantTargetUTC:   time.Date(2026, 1, 21, 0, 0, 0, 0, time.UTC),
			wantFirstTryUTC: time.Date(2026, 1, 21, 5, 30, 0, 0, time.UTC),
			wantTargetDate:  "2026-01-21",
		},
		{
			name:            "summer",
			now:             time.Date(2026, 7, 21, 4, 0, 0, 0, time.UTC),
			wantTargetUTC:   time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
			wantFirstTryUTC: time.Date(2026, 7, 21, 4, 30, 0, 0, time.UTC),
			wantTargetDate:  "2026-07-21",
		},
		{
			name:            "monday calendar target",
			now:             time.Date(2026, 7, 20, 5, 0, 0, 0, time.UTC),
			wantTargetUTC:   time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
			wantFirstTryUTC: time.Date(2026, 7, 20, 4, 30, 0, 0, time.UTC),
			wantTargetDate:  "2026-07-20",
		},
		{
			name:            "day after spring DST change",
			now:             time.Date(2026, 3, 30, 5, 0, 0, 0, time.UTC),
			wantTargetUTC:   time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC),
			wantFirstTryUTC: time.Date(2026, 3, 30, 4, 30, 0, 0, time.UTC),
			wantTargetDate:  "2026-03-30",
		},
		{
			name:            "day after autumn DST change",
			now:             time.Date(2026, 10, 26, 6, 0, 0, 0, time.UTC),
			wantTargetUTC:   time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
			wantFirstTryUTC: time.Date(2026, 10, 26, 5, 30, 0, 0, time.UTC),
			wantTargetDate:  "2026-10-26",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, supplied := range []time.Time{tt.now, tt.now.In(newYork), tt.now.In(tokyo)} {
				target, firstTry := flexDailyWindow(supplied)
				if !target.Equal(tt.wantTargetUTC) {
					t.Fatalf("target for %s = %s, want %s", supplied, target, tt.wantTargetUTC)
				}
				if !firstTry.Equal(tt.wantFirstTryUTC) {
					t.Fatalf("first attempt for %s = %s, want %s", supplied, firstTry, tt.wantFirstTryUTC)
				}
				if got := target.Format("2006-01-02"); got != tt.wantTargetDate {
					t.Fatalf("target date = %s, want %s", got, tt.wantTargetDate)
				}
				if got := firstTry.In(berlin).Format("15:04"); got != "06:30" {
					t.Fatalf("Berlin first-attempt time = %s, want 06:30", got)
				}
			}
		})
	}
}

func TestFlexFetchStatusBecomesDueAtBerlin0630(t *testing.T) {
	berlin, err := time.LoadLocation(flexScheduleZone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 6, 29, 59, 0, berlin)
	s, _ := newFlexAutomationTestServer(t, &now)

	before := s.flexFetchStatusAt(now)
	if before.State != rpc.ReconReportStateWaiting || before.Reason != rpc.ReconReportReasonBeforeDailyWindow {
		t.Fatalf("06:29 status = %+v, want waiting before daily window", before)
	}
	if want := time.Date(2026, 7, 21, 6, 30, 0, 0, berlin); !before.NextAttempt.Equal(want) {
		t.Fatalf("next attempt = %s, want %s", before.NextAttempt, want)
	}

	now = time.Date(2026, 7, 21, 6, 30, 0, 0, berlin)
	due := s.flexFetchStatusAt(now)
	if due.State != rpc.ReconReportStateDue || due.Reason != rpc.ReconReportReasonCoveragePending {
		t.Fatalf("06:30 status = %+v, want due/coverage_pending", due)
	}
}

func TestPreviousDayFailureCannotStartNextDayBeforeBerlin0630(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 22, 6, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	previousTarget, _ := flexDailyWindow(now.AddDate(0, 0, -1))
	s.flexFetch.mu.Lock()
	s.flexFetch.state.Stage = rpc.ReconReportStateRetryScheduled
	s.flexFetch.state.LastAttempt = now.Add(-23 * time.Hour)
	s.flexFetch.state.LastReason = rpc.ReconReportReasonReportNotReady
	s.flexFetch.state.LastRetryable = true
	s.flexFetch.state.TargetDate = previousTarget
	s.flexFetch.state.NextAttempt = now.Add(-22*time.Hour - 30*time.Minute)
	if err := s.flexFetch.persistLocked(t.Context()); err != nil {
		s.flexFetch.mu.Unlock()
		t.Fatal(err)
	}
	s.flexFetch.mu.Unlock()

	var calls int
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		calls++
		return flexFetchOutcome{}, &flexFetchFailure{
			reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "safe test failure",
		}
	}
	s.flexProjectionFn = func(context.Context) error { return nil }
	s.maybeFetchFlex(t.Context())
	if calls != 0 || s.flexFetch.isBusy() {
		t.Fatalf("pre-window scheduler calls=%d busy=%v, want no fetch", calls, s.flexFetch.isBusy())
	}
	status := s.flexFetchStatusAt(now)
	if status.State != rpc.ReconReportStateWaiting || status.Reason != rpc.ReconReportReasonBeforeDailyWindow {
		t.Fatalf("pre-window status = %+v, want waiting for today's window", status)
	}

	now = berlinTestTime(t, 2026, 7, 22, 6, 30)
	s.maybeFetchFlex(t.Context())
	s.flexFetch.wg.Wait()
	if calls != 1 {
		t.Fatalf("06:30 scheduler calls = %d, want 1", calls)
	}
}

func TestDailyFlexCheckAcceptsLastBusinessDateOnMondayAndHoliday(t *testing.T) {
	tests := []struct {
		name          string
		now           time.Time
		coverage      time.Time
		coverageWire  string
		generatedWire string
	}{
		{
			name: "Monday keeps Friday coverage", now: berlinTestTime(t, 2026, 7, 20, 7, 0),
			coverage: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC), coverageWire: "20260717", generatedWire: "20260720;063000",
		},
		{
			name: "New Years Day keeps prior business coverage", now: berlinTestTime(t, 2027, 1, 1, 7, 0),
			coverage: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), coverageWire: "20261231", generatedWire: "20270101;063000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := tt.now
			s, _ := newFlexAutomationTestServer(t, &now)
			writeFlexFixture(t, "flex-business-day.xml", tt.generatedWire, tt.coverageWire, tt.coverageWire, "")
			s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
				return flexFetchOutcome{Path: "flex-business-day.xml", CoverageTo: tt.coverage}, nil
			}
			s.flexProjectionFn = func(context.Context) error { return nil }

			if !s.startFlexFetch(t.Context(), false) {
				t.Fatal("daily report check did not start")
			}
			s.flexFetch.wg.Wait()
			status := s.flexFetchStatusAt(now)
			if status.State != rpc.ReconReportStateCurrent || !status.CoverageTo.Equal(tt.coverage) {
				t.Fatalf("daily status = %+v, want current through %s", status, tt.coverage)
			}
		})
	}
}

func TestDailyFlexCheckRereadsSameReportGeneration(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 30)
	s, _ := newFlexAutomationTestServer(t, &now)
	raw := flexRawFixture("20260721;010932", "20260707", "20260720", cashLine("cash-1", "Deposits/Withdrawals", -100, "20260720"))
	first, err := retainFlexStatement(raw)
	if err != nil {
		t.Fatal(err)
	}
	var fetchCalls, projectionCalls int
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		fetchCalls++
		return retainFlexStatement(raw)
	}
	s.flexProjectionFn = func(context.Context) error {
		projectionCalls++
		return nil
	}

	if !s.startFlexFetch(t.Context(), false) {
		t.Fatal("same-generation daily report check did not start")
	}
	s.flexFetch.wg.Wait()
	status := s.flexFetchStatusAt(now)
	if fetchCalls != 1 || projectionCalls != 1 {
		t.Fatalf("same-generation calls fetch=%d projection=%d, want one of each", fetchCalls, projectionCalls)
	}
	if status.State != rpc.ReconReportStateCurrent || status.Reason != "" || !status.CoverageTo.Equal(first.CoverageTo) {
		t.Fatalf("same-generation status = %+v, want current through %s", status, first.CoverageTo)
	}
}

func TestFlexReportNotReadySchedulesAutomaticRetry(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	var projectionCalled bool
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		return flexFetchOutcome{}, &flexFetchFailure{
			reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "safe test failure",
		}
	}
	s.flexProjectionFn = func(context.Context) error {
		projectionCalled = true
		return nil
	}

	if !s.startFlexFetch(t.Context(), false) {
		t.Fatal("due automatic report check did not start")
	}
	s.flexFetch.wg.Wait()

	status := s.flexFetchStatusAt(now)
	if status.State != rpc.ReconReportStateRetryScheduled || status.Reason != rpc.ReconReportReasonReportNotReady {
		t.Fatalf("status = %+v, want retry_scheduled/report_not_ready", status)
	}
	if want := now.Add(flexRetryAfterFail); !status.NextAttempt.Equal(want) {
		t.Fatalf("next attempt = %s, want %s", status.NextAttempt, want)
	}
	if !status.RetryAutomatic || status.Busy {
		t.Fatalf("retry flags = %+v, want automatic and idle", status)
	}
	if projectionCalled {
		t.Fatal("projection ran without a current broker report")
	}

	now = now.Add(flexRetryAfterFail - time.Second)
	if got := s.flexFetchStatusAt(now).State; got != rpc.ReconReportStateRetryScheduled {
		t.Fatalf("state just before retry = %s, want retry_scheduled", got)
	}
	now = now.Add(time.Second)
	if got := s.flexFetchStatusAt(now).State; got != rpc.ReconReportStateDue {
		t.Fatalf("state at retry time = %s, want due", got)
	}
}

func TestFlexCredentialFailureRequiresUserAction(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	var projectionCalled bool
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		return flexFetchOutcome{}, &flexFetchFailure{
			reason: rpc.ReconReportReasonTokenExpired, detail: "safe test failure",
		}
	}
	s.flexProjectionFn = func(context.Context) error {
		projectionCalled = true
		return nil
	}

	if !s.startFlexFetch(t.Context(), true) {
		t.Fatal("manual report check did not start")
	}
	s.flexFetch.wg.Wait()

	status := s.flexFetchStatusAt(now)
	if status.State != rpc.ReconReportStateActionRequired || status.Reason != rpc.ReconReportReasonTokenExpired {
		t.Fatalf("status = %+v, want action_required/token_expired", status)
	}
	if status.RetryAutomatic || !status.NextAttempt.IsZero() {
		t.Fatalf("non-retryable credential state = %+v", status)
	}
	if projectionCalled {
		t.Fatal("projection ran after a credential failure")
	}
}

func TestFlexServiceCodesSeparateAutomaticRetryFromUserAction(t *testing.T) {
	tests := []struct {
		code      string
		reason    string
		retryable bool
	}{
		{code: "1005", reason: rpc.ReconReportReasonReportNotReady, retryable: true},
		{code: "1009", reason: rpc.ReconReportReasonServiceBusy, retryable: true},
		{code: "1018", reason: rpc.ReconReportReasonRateLimited, retryable: true},
		{code: "1019", reason: rpc.ReconReportReasonServiceBusy, retryable: true},
		{code: "1010", reason: rpc.ReconReportReasonQueryInvalid},
		{code: "1011", reason: rpc.ReconReportReasonServiceInactive},
		{code: "1012", reason: rpc.ReconReportReasonTokenExpired},
		{code: "1013", reason: rpc.ReconReportReasonIPRestricted},
		{code: "1014", reason: rpc.ReconReportReasonQueryInvalid},
		{code: "1015", reason: rpc.ReconReportReasonTokenInvalid},
	}
	for _, tt := range tests {
		reason, retryable := flexFailureStatus(flexEnvelopeFailure(tt.code))
		if reason != tt.reason || retryable != tt.retryable {
			t.Errorf("code %s = %s retryable=%v, want %s retryable=%v", tt.code, reason, retryable, tt.reason, tt.retryable)
		}
	}
}

func TestRetainFlexStatementRereadsSameGenerationAndRejectsRegression(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	_, _ = newFlexAutomationTestServer(t, &now)
	firstRaw := flexRawFixture("20260720;063000", "20260713", "20260717", cashLine("cash-1", "Deposits/Withdrawals", -100, "20260717"))
	first, err := retainFlexStatement(firstRaw)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := retainFlexStatement(firstRaw); err != nil || duplicate.Path != first.Path {
		t.Fatalf("exact duplicate re-read = %+v err=%v, want retained success", duplicate, err)
	}
	equalGenerationChangedRaw := flexRawFixture("20260720;063000", "20260713", "20260717", cashLine("cash-1", "Deposits/Withdrawals", -125, "20260717"))
	equalGeneration, err := retainFlexStatement(equalGenerationChangedRaw)
	if err != nil || equalGeneration.Path == "" || equalGeneration.Path == first.Path {
		t.Fatalf("equal broker generation with changed query result = %+v err=%v, want new retained evidence", equalGeneration, err)
	}
	statements, problems, err := loadRetainedFlexStatements()
	if err != nil || len(problems) != 0 {
		t.Fatalf("equal-generation retained statements err=%v problems=%v", err, problems)
	}
	merged := mergeRetainedStatements(statements)
	if len(merged.flows) != 1 || merged.flows[0].amountBase != -125 {
		t.Fatalf("equal-generation re-read winner = %+v, want latest retained amount -125", merged.flows)
	}

	correctedRaw := flexRawFixture("20260720;073000", "20260713", "20260717", cashLine("cash-1", "Deposits/Withdrawals", -200, "20260717"))
	corrected, err := retainFlexStatement(correctedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if corrected.Path == first.Path || !corrected.CoverageTo.Equal(first.CoverageTo) || !corrected.WhenGenerated.After(first.WhenGenerated) {
		t.Fatalf("correction = %+v, first = %+v", corrected, first)
	}

	regressedRaw := flexRawFixture("20260720;070000", "20260713", "20260717", cashLine("cash-1", "Deposits/Withdrawals", -150, "20260717"))
	if regressed, err := retainFlexStatement(regressedRaw); err == nil || regressed.Path != "" {
		t.Fatalf("older generation = %+v err=%v, want retryable rejection", regressed, err)
	}
	statements, problems, err = loadRetainedFlexStatements()
	if err != nil || len(problems) != 0 {
		t.Fatalf("retained statements err=%v problems=%v", err, problems)
	}
	merged = mergeRetainedStatements(statements)
	if len(merged.flows) != 1 || merged.flows[0].amountBase != -200 {
		t.Fatalf("restatement winner = %+v, want newer broker generation amount -200", merged.flows)
	}
}

func TestActionRequiredCanRecoverThroughCheckAgain(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonTokenExpired, detail: "safe fixture"}
	}
	s.flexProjectionFn = func(context.Context) error { return nil }
	if !s.startFlexFetch(t.Context(), false) {
		t.Fatal("initial check did not start")
	}
	s.flexFetch.wg.Wait()
	if got := s.flexFetchStatusAt(now); got.State != rpc.ReconReportStateActionRequired || got.CanCheckNow {
		t.Fatalf("credential failure = %+v, want action required during cooldown", got)
	}

	now = now.Add(flexManualRetryFloor)
	writeFlexFixture(t, "flex-recovered.xml", "20260721;070100", "20260714", "20260720", "")
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		return flexFetchOutcome{Path: "flex-recovered.xml", CoverageTo: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)}, nil
	}
	receipt, err := s.handleReconCheck(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != rpc.ReconCheckOutcomeStarted {
		t.Fatalf("recovery receipt = %+v, want started", receipt)
	}
	s.flexFetch.wg.Wait()
	if got := s.flexFetchStatusAt(now); got.State != rpc.ReconReportStateCurrent || got.Reason != rpc.ReconReportReasonNone {
		t.Fatalf("recovered status = %+v, want current", got)
	}
}

func TestPreWindowCheckCannotSatisfyDailyRun(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 5, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	writeFlexFixture(t, "flex-safe-window.xml", "20260721;063000", "20260714", "20260720", "")
	var calls int
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		calls++
		return flexFetchOutcome{Path: "flex-safe-window.xml", CoverageTo: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)}, nil
	}
	s.flexProjectionFn = func(context.Context) error { return nil }
	status := s.flexFetchStatusAt(now)
	if status.State != rpc.ReconReportStateWaiting || status.CanCheckNow || s.startFlexFetch(t.Context(), true) {
		t.Fatalf("05:00 status=%+v calls=%d, want waiting with manual check disabled", status, calls)
	}

	now = berlinTestTime(t, 2026, 7, 21, 6, 30)
	s.maybeFetchFlex(t.Context())
	s.flexFetch.wg.Wait()
	if calls != 1 || s.flexFetchStatusAt(now).State != rpc.ReconReportStateCurrent {
		t.Fatalf("06:30 calls=%d status=%+v, want one completed safe-window check", calls, s.flexFetchStatusAt(now))
	}
}

func TestFlexFetchRestartRecoversInterruptedCheckAsDurableRetry(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	dbPath := filepath.Join(stateHome, "daemon.db")

	firstCore, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	first := &Server{
		now:    func() time.Time { return now },
		cfg:    &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "daily-report"}},
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
	if err := first.flexFetch.bindCore(t.Context(), firstCore); err != nil {
		t.Fatal(err)
	}
	first.flexFetch.mu.Lock()
	first.flexFetch.state.Stage = rpc.ReconReportStateChecking
	first.flexFetch.state.LastAttempt = now.Add(-10 * time.Minute)
	first.flexFetch.state.TargetDate, _ = flexDailyWindow(now)
	if err := first.flexFetch.persistLocked(t.Context()); err != nil {
		first.flexFetch.mu.Unlock()
		t.Fatal(err)
	}
	first.flexFetch.mu.Unlock()
	if err := firstCore.Close(); err != nil {
		t.Fatal(err)
	}

	restartedCore, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedCore.Close() })
	restarted := &Server{
		now:    func() time.Time { return now },
		cfg:    first.cfg,
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
	if err := restarted.flexFetch.bindCore(t.Context(), restartedCore); err != nil {
		t.Fatal(err)
	}

	status := restarted.flexFetchStatusAt(now)
	if status.State != rpc.ReconReportStateRetryScheduled || status.Reason != rpc.ReconReportReasonNetworkUnavailable {
		t.Fatalf("recovered status = %+v, want retry_scheduled/network_unavailable", status)
	}
	if want := now.Add(20 * time.Minute); !status.NextAttempt.Equal(want) {
		t.Fatalf("recovered next attempt = %s, want %s", status.NextAttempt, want)
	}
}

func TestFlexFetchStateMigratesV1ToDailyTargetV2(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	legacy := flexFetchStateV1{
		Version: 1, Stage: rpc.ReconReportStateRetryScheduled,
		LastAttempt: now, LastReason: rpc.ReconReportReasonReportNotReady, LastRetryable: true,
		ExpectedCoverageTo: now.AddDate(0, 0, -1), CoverageTo: now.AddDate(0, 0, -2),
		NextAttempt: now.Add(flexRetryAfterFail),
	}
	raw, _ := json.Marshal(legacy)
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: flexFetchStateKind, JSON: raw,
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{now: func() time.Time { return now }, cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "daily-report"}}, logger: NewLogger(&bytes.Buffer{}, "error")}
	if err := s.flexFetch.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	target, _ := flexDailyWindow(now)
	if s.flexFetch.state.Version != flexFetchStateVersion || !s.flexFetch.state.TargetDate.Equal(target) {
		t.Fatalf("migrated state = %+v, want v2 target %s", s.flexFetch.state, target)
	}
	doc, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, flexFetchStateKind)
	if err != nil || !ok {
		t.Fatalf("migrated document ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(doc.JSON), "expected_coverage_to") || !strings.Contains(string(doc.JSON), `"version":2`) {
		t.Fatalf("migrated JSON kept ambiguous v1 schema: %s", doc.JSON)
	}
}

func TestRestartAfterRawRetentionResumesProjectionWithoutRedownload(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	dbPath := filepath.Join(stateHome, "daemon.db")
	firstCore, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	first := &Server{now: func() time.Time { return now }, cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "daily-report"}}, logger: NewLogger(&bytes.Buffer{}, "error")}
	if err := first.flexFetch.bindCore(t.Context(), firstCore); err != nil {
		t.Fatal(err)
	}
	writeFlexFixture(t, "flex-crash.xml", "20260721;070000", "20260714", "20260720", "")
	dir, _ := flexStatementsDirPath()
	if err := os.Chtimes(filepath.Join(dir, "flex-crash.xml"), now, now); err != nil {
		t.Fatal(err)
	}
	target, _ := flexDailyWindow(now)
	first.flexFetch.mu.Lock()
	first.flexFetch.state.Stage = rpc.ReconReportStateChecking
	first.flexFetch.state.LastAttempt = now
	first.flexFetch.state.TargetDate = target
	if err := first.flexFetch.persistLocked(t.Context()); err != nil {
		first.flexFetch.mu.Unlock()
		t.Fatal(err)
	}
	first.flexFetch.mu.Unlock()
	if err := firstCore.Close(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(flexRetryAfterFail - time.Second)
	restartedCore, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedCore.Close() })
	restarted := &Server{now: func() time.Time { return now }, cfg: first.cfg, logger: NewLogger(&bytes.Buffer{}, "error")}
	if err := restarted.flexFetch.bindCore(t.Context(), restartedCore); err != nil {
		t.Fatal(err)
	}
	if got := restarted.flexFetchStatusAt(now); got.State != rpc.ReconReportStateRetryScheduled || got.Reason != rpc.ReconReportReasonProjectionFailed {
		t.Fatalf("recovered post-retain status = %+v, want scheduled projection retry", got)
	}
	now = now.Add(time.Second)
	var fetchCalls, projectionCalls int
	restarted.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		fetchCalls++
		return flexFetchOutcome{}, errors.New("broker redownload must not run")
	}
	restarted.flexProjectionFn = func(context.Context) error { projectionCalls++; return nil }
	if !restarted.startFlexFetch(t.Context(), false) {
		t.Fatal("projection recovery did not start")
	}
	restarted.flexFetch.wg.Wait()
	if fetchCalls != 0 || projectionCalls != 1 || restarted.flexFetchStatusAt(now).State != rpc.ReconReportStateCurrent {
		t.Fatalf("recovery fetch=%d projection=%d status=%+v", fetchCalls, projectionCalls, restarted.flexFetchStatusAt(now))
	}
}

func TestPersistedSuccessWithoutReadableEvidenceRefetches(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	target, _ := flexDailyWindow(now)
	s.flexFetch.mu.Lock()
	s.flexFetch.state.Stage = rpc.ReconReportStateCurrent
	s.flexFetch.state.TargetDate = target
	s.flexFetch.state.LastAttempt = now.Add(-time.Hour)
	s.flexFetch.state.LastSuccess = now.Add(-time.Hour)
	if err := s.flexFetch.persistLocked(t.Context()); err != nil {
		s.flexFetch.mu.Unlock()
		t.Fatal(err)
	}
	s.flexFetch.mu.Unlock()
	status := s.flexFetchStatusAt(now)
	if status.State != rpc.ReconReportStateDue || status.Reason != rpc.ReconReportReasonCoveragePending {
		t.Fatalf("missing evidence status = %+v, want automatic refetch due", status)
	}
}

func TestDailyFlexCheckDoesNotRequireCalendarDayCoverage(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	writeFlexFixture(t, "flex-stale.xml", "20260720;063000", "20260713", "20260719", "")

	before := s.flexFetchStatusAt(now)
	if before.State != rpc.ReconReportStateDue || before.Reason != rpc.ReconReportReasonCoveragePending {
		t.Fatalf("unchecked daily status = %+v, want due/coverage_pending", before)
	}

	var calls, projections int
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		calls++
		return flexFetchOutcome{Path: "flex-stale.xml", CoverageTo: before.CoverageTo}, nil
	}
	s.flexProjectionFn = func(context.Context) error { projections++; return nil }
	s.maybeFetchFlex(t.Context())
	s.flexFetch.wg.Wait()
	if calls != 1 || projections != 1 {
		t.Fatalf("automatic calls fetch=%d projection=%d, want one each", calls, projections)
	}
	after := s.flexFetchStatusAt(now)
	if after.State != rpc.ReconReportStateCurrent || after.Reason != rpc.ReconReportReasonNone {
		t.Fatalf("post-fetch status = %+v, want current despite business-date coverage", after)
	}
}

func TestFlexProjectionFailureRetriesLocallyThenCompletes(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	writeFlexFixture(t, "flex-current.xml", "20260721;063000", "20260714", "20260720", "")
	target, _ := flexDailyWindow(now)

	var fetchCalls, projectionCalls int
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		fetchCalls++
		return flexFetchOutcome{Path: "flex-current.xml", CoverageTo: target.AddDate(0, 0, -1)}, nil
	}
	s.flexProjectionFn = func(context.Context) error {
		projectionCalls++
		if projectionCalls == 1 {
			return errors.New("projection unavailable")
		}
		return nil
	}

	if !s.startFlexFetch(t.Context(), true) {
		t.Fatal("first manual check did not start")
	}
	s.flexFetch.wg.Wait()
	failed := s.flexFetchStatusAt(now)
	if failed.State != rpc.ReconReportStateRetryScheduled || failed.Reason != rpc.ReconReportReasonProjectionFailed {
		t.Fatalf("projection failure status = %+v", failed)
	}
	if !failed.LastSuccess.IsZero() {
		t.Fatalf("projection failure was recorded as full success at %s", failed.LastSuccess)
	}

	now = now.Add(flexRetryAfterFail)
	if !s.startFlexFetch(t.Context(), false) {
		t.Fatal("local projection retry did not start")
	}
	s.flexFetch.wg.Wait()
	if fetchCalls != 1 {
		t.Fatalf("broker report was downloaded %d times, want one local projection retry", fetchCalls)
	}
	if projectionCalls != 2 {
		t.Fatalf("projection calls = %d, want 2", projectionCalls)
	}
	complete := s.flexFetchStatusAt(now)
	if complete.State != rpc.ReconReportStateCurrent || complete.Reason != rpc.ReconReportReasonNone || complete.LastSuccess.IsZero() {
		t.Fatalf("completed status = %+v, want current with a full-success time", complete)
	}
}

func TestPriorDayProjectionFailureCannotSatisfyTodaysFetch(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 22, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	writeFlexFixture(t, "flex-yesterday.xml", "20260721;070000", "20260714", "20260720", "")
	yesterdayTarget, _ := flexDailyWindow(now.AddDate(0, 0, -1))
	s.flexFetch.mu.Lock()
	s.flexFetch.state.Stage = rpc.ReconReportStateRetryScheduled
	s.flexFetch.state.TargetDate = yesterdayTarget
	s.flexFetch.state.LastAttempt = now.AddDate(0, 0, -1)
	s.flexFetch.state.LastReason = rpc.ReconReportReasonProjectionFailed
	s.flexFetch.state.LastRetryable = true
	s.flexFetch.state.NextAttempt = now.Add(-time.Hour)
	if err := s.flexFetch.persistLocked(t.Context()); err != nil {
		s.flexFetch.mu.Unlock()
		t.Fatal(err)
	}
	s.flexFetch.mu.Unlock()

	writeFlexFixture(t, "flex-today.xml", "20260722;070000", "20260715", "20260721", "")
	var fetchCalls, projectionCalls int
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		fetchCalls++
		return flexFetchOutcome{Path: "flex-today.xml", CoverageTo: time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)}, nil
	}
	s.flexProjectionFn = func(context.Context) error { projectionCalls++; return nil }
	if !s.startFlexFetch(t.Context(), false) {
		t.Fatal("today's fetch did not start")
	}
	s.flexFetch.wg.Wait()
	if fetchCalls != 1 || projectionCalls != 1 || s.flexFetchStatusAt(now).State != rpc.ReconReportStateCurrent {
		t.Fatalf("cross-day recovery fetch=%d projection=%d status=%+v", fetchCalls, projectionCalls, s.flexFetchStatusAt(now))
	}
}

func TestManualFlexCheckIsSingleFlightAndRateLimitedLocally(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	started := make(chan struct{})
	release := make(chan struct{})
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		close(started)
		<-release
		return flexFetchOutcome{}, &flexFetchFailure{
			reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "safe test failure",
		}
	}
	s.flexProjectionFn = func(context.Context) error { return nil }

	if !s.kickFlexFetch(t.Context()) {
		t.Fatal("first manual check did not start")
	}
	<-started
	if s.kickFlexFetch(t.Context()) {
		t.Fatal("concurrent manual check escaped single-flight guard")
	}
	close(release)
	s.flexFetch.wg.Wait()
	if s.kickFlexFetch(t.Context()) {
		t.Fatal("manual check escaped one-minute local cooldown")
	}

	now = now.Add(flexManualRetryFloor)
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		return flexFetchOutcome{}, &flexFetchFailure{
			reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "safe test failure",
		}
	}
	if !s.kickFlexFetch(t.Context()) {
		t.Fatal("manual check stayed blocked after local cooldown")
	}
	s.flexFetch.wg.Wait()
}

func TestReconCheckHandlerReturnsTypedManualReceipts(t *testing.T) {
	now := berlinTestTime(t, 2026, 7, 21, 7, 0)
	s, _ := newFlexAutomationTestServer(t, &now)
	started := make(chan struct{})
	release := make(chan struct{})
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		close(started)
		<-release
		return flexFetchOutcome{}, &flexFetchFailure{
			reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "safe test failure",
		}
	}
	s.flexProjectionFn = func(context.Context) error { return nil }
	req := &rpc.Request{Params: json.RawMessage(`{}`)}

	first, err := s.handleReconCheck(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != rpc.ReconCheckOutcomeStarted || first.Status.Report.State != rpc.ReconReportStateChecking {
		t.Fatalf("first receipt = %+v, want started/checking", first)
	}
	<-started
	concurrent, err := s.handleReconCheck(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if concurrent.Outcome != rpc.ReconCheckOutcomeAlreadyChecking || concurrent.Status.Report.State != rpc.ReconReportStateChecking {
		t.Fatalf("concurrent receipt = %+v, want already_checking/checking", concurrent)
	}

	close(release)
	s.flexFetch.wg.Wait()
	cooldown, err := s.handleReconCheck(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if cooldown.Outcome != rpc.ReconCheckOutcomeCooldown || cooldown.Status.Report.State != rpc.ReconReportStateRetryScheduled {
		t.Fatalf("cooldown receipt = %+v, want cooldown/retry_scheduled", cooldown)
	}

	disabledNow := now
	disabled, _ := newFlexAutomationTestServer(t, &disabledNow)
	disabled.cfg.Flex.Enabled = false
	action, err := disabled.handleReconCheck(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if action.Outcome != rpc.ReconCheckOutcomeActionRequired || action.Status.Report.State != rpc.ReconReportStateActionRequired {
		t.Fatalf("disabled receipt = %+v, want action_required", action)
	}
}

func newFlexAutomationTestServer(t *testing.T, now *time.Time) (*Server, *corestore.Store) {
	t.Helper()
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		now:    func() time.Time { return *now },
		cfg:    &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "daily-report"}},
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
	if err := s.flexFetch.bindCore(t.Context(), core); err != nil {
		_ = core.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.flexFetch.wg.Wait()
		_ = core.Close()
	})
	return s, core
}

func berlinTestTime(t *testing.T, year int, month time.Month, day, hour, minute int) time.Time {
	t.Helper()
	berlin, err := time.LoadLocation(flexScheduleZone)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(year, month, day, hour, minute, 0, 0, berlin)
}

func flexRawFixture(whenGenerated, from, to, body string) []byte {
	return fmt.Appendf(nil, `<FlexQueryResponse queryName="recon" type="AF">
 <FlexStatements count="1">
  <FlexStatement accountId="U1234567" fromDate="%s" toDate="%s" whenGenerated="%s">
%s
  </FlexStatement>
 </FlexStatements>
</FlexQueryResponse>`, from, to, whenGenerated, body)
}
