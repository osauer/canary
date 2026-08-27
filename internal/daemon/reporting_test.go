package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestActiveFlexRetentionRemainsInsideBrokerLaneAndPreservesReturnedRange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	raw := reportingFlexFixture("20260801", "20260824")
	srv := &Server{cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "123", TokenPath: "/unused"}}}
	retainInsideLane := false
	srv.flexRawDateRangeLockedFn = func(context.Context, time.Time, time.Time, int, string, string) ([]byte, error) {
		return slices.Clone(raw), nil
	}
	srv.flexRetainRawFn = func(data []byte) (flexFetchOutcome, error) {
		if srv.flexBrokerMu.TryLock() {
			srv.flexBrokerMu.Unlock()
			t.Fatal("active retention ran outside flexBrokerMu")
		}
		retainInsideLane = true
		return retainFlexStatement(data)
	}
	outcome, err := srv.fetchFlexDateRangeWithPollAttempts(t.Context(), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !retainInsideLane {
		t.Fatal("active retain seam was not called")
	}
	if got := outcome.CoverageFrom.Format("2006-01-02"); got != "2026-08-01" {
		t.Fatalf("CoverageFrom = %s", got)
	}
	if got := outcome.CoverageTo.Format("2006-01-02"); got != "2026-08-24" {
		t.Fatalf("CoverageTo = %s", got)
	}
}

func TestReportingCandidateAndActiveFetchSerialize(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	tokenPath := writeReportingTestToken(t)
	raw := reportingFlexFixture("20260801", "20260824")
	srv := &Server{
		cfg:      &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "222", TokenPath: "/unused"}},
		now:      func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
		edgeWake: make(chan struct{}, 1),
	}
	candidateEntered := make(chan struct{})
	releaseCandidate := make(chan struct{})
	activeEntered := make(chan struct{})
	srv.flexRawDateRangeLockedFn = func(_ context.Context, _, _ time.Time, _ int, queryID, _ string) ([]byte, error) {
		switch queryID {
		case "111":
			close(candidateEntered)
			<-releaseCandidate
		case "222":
			close(activeEntered)
		}
		return slices.Clone(raw), nil
	}

	params, _ := json.Marshal(rpc.ReportingValidateParams{QueryID: "111", TokenPath: tokenPath})
	candidateDone := make(chan error, 1)
	go func() {
		_, err := srv.handleReportingValidate(t.Context(), &rpc.Request{Params: params})
		candidateDone <- err
	}()
	<-candidateEntered
	activeDone := make(chan error, 1)
	go func() {
		_, err := srv.fetchFlexDateRangeWithPollAttempts(t.Context(), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), 1)
		activeDone <- err
	}()
	select {
	case <-activeEntered:
		t.Fatal("active fetch entered while candidate owned flexBrokerMu")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCandidate)
	if err := <-candidateDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-activeEntered:
	case <-time.After(time.Second):
		t.Fatal("active fetch did not enter after candidate released flexBrokerMu")
	}
	if err := <-activeDone; err != nil {
		t.Fatal(err)
	}
}

func TestReportingCandidateEndsAtLatestCompletedFlexDate(t *testing.T) {
	tokenPath := writeReportingTestToken(t)
	now := time.Date(2026, 8, 25, 5, 50, 0, 0, time.UTC) // 01:50 in New York.
	srv := &Server{now: func() time.Time { return now }}
	var gotFrom, gotTo time.Time
	srv.flexRawDateRangeLockedFn = func(_ context.Context, from, to time.Time, _ int, _, _ string) ([]byte, error) {
		gotFrom, gotTo = from, to
		return reportingFlexFixture("20260721", "20260824"), nil
	}
	params, _ := json.Marshal(rpc.ReportingValidateParams{QueryID: "111", TokenPath: tokenPath})
	if _, err := srv.handleReportingValidate(t.Context(), &rpc.Request{Params: params}); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC); !gotTo.Equal(want) {
		t.Fatalf("candidate to = %s, want %s", gotTo, want)
	}
	if want := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC); !gotFrom.Equal(want) {
		t.Fatalf("candidate from = %s, want %s", gotFrom, want)
	}
}

func TestLatestCompletedFlexDateUsesNewYorkCalendar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "after eastern midnight",
			now:  time.Date(2026, 8, 25, 5, 50, 0, 0, time.UTC),
			want: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "before eastern midnight",
			now:  time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
			want: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "winter offset",
			now:  time.Date(2026, 1, 6, 5, 30, 0, 0, time.UTC),
			want: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		},
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

func TestReportingCandidateValidationLeavesNoDurableOrProjectionSideEffects(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	tokenPath := writeReportingTestToken(t)
	raw := reportingFlexFixture("20260801", "20260824")
	projectionCalls := 0
	srv := &Server{
		cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "222", TokenPath: "/active"}},
		now: func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
		flexProjectionFn: func(context.Context) error {
			projectionCalls++
			return nil
		},
		edgeWake: make(chan struct{}, 1),
	}
	srv.flexFetch.state = flexFetchStateV2{
		Version: flexFetchStateVersion, Stage: rpc.ReconReportStateRetryScheduled,
		LastReason: rpc.ReconReportReasonResponseInvalid, LastBrokerCode: "1025", LastRetryable: true,
	}
	beforeState := srv.flexFetch.state
	srv.flexRawDateRangeLockedFn = func(context.Context, time.Time, time.Time, int, string, string) ([]byte, error) {
		return slices.Clone(raw), nil
	}
	params, _ := json.Marshal(rpc.ReportingValidateParams{QueryID: "111", TokenPath: tokenPath})
	result, err := srv.handleReportingValidate(t.Context(), &rpc.Request{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ReadyForRotation || result.Outcome != rpc.ReportingValidationUnproved || len(result.MissingRequirements) != 0 {
		t.Fatalf("candidate result = %+v", result)
	}
	if !reflect.DeepEqual(srv.flexFetch.state, beforeState) {
		t.Fatalf("fetch state changed: before %+v after %+v", beforeState, srv.flexFetch.state)
	}
	if projectionCalls != 0 {
		t.Fatalf("projection calls = %d", projectionCalls)
	}
	select {
	case <-srv.edgeWake:
		t.Fatal("candidate validation scheduled an Edge rebuild")
	default:
	}
	dir, err := flexStatementsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Fatalf("candidate validation retained %d files", len(entries))
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestReportingCandidateNamesAbsentSectionsWithoutCallingThemEmpty(t *testing.T) {
	tokenPath := writeReportingTestToken(t)
	srv := &Server{now: func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }}
	srv.flexRawDateRangeLockedFn = func(context.Context, time.Time, time.Time, int, string, string) ([]byte, error) {
		return []byte(`<FlexQueryResponse><FlexStatements><FlexStatement accountId="REDACTED" fromDate="20260801" toDate="20260823" whenGenerated="20260824;120000"><EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="20260823" total="100"/></EquitySummaryInBase></FlexStatement></FlexStatements></FlexQueryResponse>`), nil
	}
	params, _ := json.Marshal(rpc.ReportingValidateParams{QueryID: "111", TokenPath: tokenPath})
	result, err := srv.handleReportingValidate(t.Context(), &rpc.Request{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != rpc.ReportingValidationUnproved || !result.ReadyForRotation || result.Reason != rpc.ReportingReasonAbsentSectionsUnproved || !strings.Contains(result.Action, "Select All") {
		t.Fatalf("absent candidate diagnosis = %+v", result)
	}
	for _, requirement := range result.Requirements {
		if requirement.Key == "trades" && requirement.Status != flexstmt.QueryRequirementAbsent {
			t.Fatalf("Trades evidence = %+v", requirement)
		}
	}
}

func TestLatestReportingStatementsDoesNotLetOldSchemaMaskSavedQueryEdit(t *testing.T) {
	to := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	fields := append([]string(nil), flexstmt.CanonicalQueryManifest()[0].RequiredFields...)
	statements := []flexstmt.Statement{
		{FromDate: to.AddDate(0, 0, -34), ToDate: to, WhenGenerated: to.Add(time.Hour), Coverage: []flexstmt.SectionCoverage{{Key: "trades", Present: true, RowCount: 1, ObservedFields: fields}}},
		{FromDate: to.AddDate(0, 0, -34), ToDate: to, WhenGenerated: to.Add(2 * time.Hour), Coverage: []flexstmt.SectionCoverage{{Key: "equity", Present: true, RowCount: 1, ObservedFields: []string{"reportDate", "total"}}}},
	}
	latest := latestReportingStatements(statements)
	if len(latest) != 1 || !latest[0].WhenGenerated.Equal(to.Add(2*time.Hour)) {
		t.Fatalf("latest report selection = %+v", latest)
	}
	if got := flexstmt.QueryRequirementEvidence(latest)[0].Status; got != flexstmt.QueryRequirementAbsent {
		t.Fatalf("latest Trades status = %q", got)
	}
}

func TestLatestReportingStatementsPrefersDailyWindowOverLaterBackfillChunk(t *testing.T) {
	to := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	daily := flexstmt.Statement{
		FromDate: to.AddDate(0, 0, -34), ToDate: to, WhenGenerated: to.Add(time.Hour),
		Coverage: []flexstmt.SectionCoverage{{Key: "trades", Present: true, RowCount: 1}},
	}
	backfill := flexstmt.Statement{
		FromDate: to.AddDate(0, 0, -90), ToDate: to, WhenGenerated: to.Add(2 * time.Hour),
		Coverage: []flexstmt.SectionCoverage{{Key: "option_events", Present: true, RowCount: 1}},
	}
	latest := latestReportingStatements([]flexstmt.Statement{daily, backfill})
	if len(latest) != 1 || !latest[0].WhenGenerated.Equal(daily.WhenGenerated) {
		t.Fatalf("latest report selection = %+v", latest)
	}
}

func TestReportingStatusPublishesOnlyTypedBrokerAndManifestDiagnostics(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	tokenPath := writeReportingTestToken(t)
	dir, err := flexStatementsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureName := "flex-" + flexQueryFingerprint("123") + "-fixture.xml"
	if err := os.WriteFile(filepath.Join(dir, fixtureName), reportingFlexFixture("20260801", "20260824"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := &Server{cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "123", TokenPath: tokenPath}}, coreStore: store, now: func() time.Time { return now }}
	if err := srv.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	srv.flexFetch.state = flexFetchStateV2{
		Version: flexFetchStateVersion, QueryFingerprint: flexQueryFingerprint("123"), Stage: rpc.ReconReportStateRetryScheduled,
		LastAttempt: now.Add(-time.Minute), TargetDate: dateOnlyUTC(now),
		LastReason: rpc.ReconReportReasonResponseInvalid, LastBrokerCode: "1025", LastRetryable: true,
		NextAttempt: now.Add(flexRetryAfterFail),
	}
	result, err := srv.handleReportingStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != rpc.ReportingStateActionRequired || result.Reason != rpc.ReportingReasonBrokerResponseUndocumented || result.Broker.BrokerCode != "1025" {
		t.Fatalf("reporting state = %+v", result)
	}
	if !slices.Contains(result.UnprovedSections, "transfers") || len(result.MissingRequirements) != 0 || result.Evidence.SchemaFingerprint == "" {
		t.Fatalf("reporting evidence = %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{fixtureName, tokenPath, "123\""} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("reporting result exposed forbidden value %q", forbidden)
		}
	}
}

func TestReportingRetryDoesNotPromiseUnprovedSectionsWillArrive(t *testing.T) {
	result := &rpc.ReportingStatusResult{
		Local:            rpc.ReportingLocalStatus{Enabled: true, QueryConfigured: true, TokenFilePresent: true, TokenFilePrivate: true},
		Broker:           rpc.ReportingBrokerStatus{State: rpc.ReconReportStateRetryScheduled, Reason: rpc.ReconReportReasonReportNotReady},
		Evidence:         rpc.ReportingEvidenceStatus{State: rpc.ReportingEvidenceObserved},
		Requirements:     []rpc.ReportingSectionRequirement{{Key: "trades", Status: flexstmt.QueryRequirementEmpty}},
		UnprovedSections: []string{"trades"},
	}
	setReportingOverallStatus(result, false)
	if result.State != rpc.ReportingStateBackfilling || !strings.Contains(result.Action, "Empty sections remain unproved") {
		t.Fatalf("reporting retry contract=%+v", result)
	}
}

func TestReportingCandidateRejectsConsolidatedOrWrongAccountWithoutExposingIdentity(t *testing.T) {
	tokenPath := writeReportingTestToken(t)
	tests := []struct {
		name     string
		raw      []byte
		gateway  string
		wantCode string
	}{
		{
			name: "consolidated",
			raw: []byte(`<?xml version="1.0"?><FlexQueryResponse><FlexStatements count="2">` +
				`<FlexStatement accountId="ACCOUNT_A" fromDate="20260801" toDate="20260824" whenGenerated="20260824;120000"><Transfers/></FlexStatement>` +
				`<FlexStatement accountId="ACCOUNT_B" fromDate="20260801" toDate="20260824" whenGenerated="20260824;120000"><Transfers/></FlexStatement>` +
				`</FlexStatements></FlexQueryResponse>`),
			wantCode: rpc.ReportingReasonAccountScopeInvalid,
		},
		{
			name:     "wrong selected account",
			raw:      reportingFlexFixture("20260801", "20260824"),
			gateway:  "ANOTHER_ACCOUNT",
			wantCode: rpc.ReportingReasonAccountScopeMismatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{
				cfg: &config.Resolved{Gateway: config.Gateway{Account: tc.gateway}},
				now: func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
			}
			srv.flexRawDateRangeLockedFn = func(context.Context, time.Time, time.Time, int, string, string) ([]byte, error) {
				return slices.Clone(tc.raw), nil
			}
			params, _ := json.Marshal(rpc.ReportingValidateParams{QueryID: "111", TokenPath: tokenPath})
			result, err := srv.handleReportingValidate(t.Context(), &rpc.Request{Params: params})
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != rpc.ReportingValidationActionRequired || result.Reason != tc.wantCode || result.ReadyForRotation {
				t.Fatalf("result = %+v", result)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, identity := range []string{"ACCOUNT_A", "ACCOUNT_B", "ANOTHER_ACCOUNT", "REDACTED"} {
				if strings.Contains(string(encoded), identity) {
					t.Fatalf("result exposed account identity %q", identity)
				}
			}
		})
	}
}

func writeReportingTestToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func reportingFlexFixture(from, to string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<FlexQueryResponse><FlexStatements count="1"><FlexStatement accountId="REDACTED" fromDate="` + from + `" toDate="` + to + `" whenGenerated="20260824;120000">
<Trades></Trades>
<SecuritiesInfo></SecuritiesInfo>
<OpenPositions></OpenPositions>
<OptionEAE></OptionEAE>
<CorporateActions></CorporateActions>
<Transfers></Transfers>
<CashTransactions></CashTransactions>
<EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="20260824" total="100"/></EquitySummaryInBase>
</FlexStatement></FlexStatements></FlexQueryResponse>`)
}
