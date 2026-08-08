package apphttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

type reconciliationRouteClient struct {
	routeFakeClient
	statusResult *rpc.ReconStatusResult
	statusErr    error
	checkResult  *rpc.ReconCheckResult
	checkErr     error
	checkCalls   int
	nudges       *rpc.NudgesSnapshotResult
}

func (c *reconciliationRouteClient) ReconcileStatus(context.Context) (*rpc.ReconStatusResult, error) {
	return c.statusResult, c.statusErr
}

func (c *reconciliationRouteClient) ReconcileCheck(context.Context) (*rpc.ReconCheckResult, error) {
	c.checkCalls++
	return c.checkResult, c.checkErr
}

func (c *reconciliationRouteClient) NudgesSnapshot(context.Context) (*rpc.NudgesSnapshotResult, error) {
	return c.nudges, nil
}

func TestReconciliationRoutesRequireAuthentication(t *testing.T) {
	t.Parallel()
	client := &reconciliationRouteClient{routeFakeClient: routeFakeClient{}}
	srv, _, _ := newGovernanceTestHandlerWithoutPoll(t, client)
	handler := srv.Handler()
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/recon/status", nil),
		httptest.NewRequest(http.MethodPost, "/api/recon/check", strings.NewReader(`{}`)),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d body=%s", request.Method, request.URL.Path, response.Code, response.Body.String())
		}
	}
	if client.checkCalls != 0 {
		t.Fatalf("unauthenticated route reached daemon %d times", client.checkCalls)
	}
}

func TestReconciliationCheckRequiresExactlyOneEmptyJSONObject(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 6, 30, 0, 0, time.UTC)
	client := &reconciliationRouteClient{
		routeFakeClient: routeFakeClient{},
		checkResult: &rpc.ReconCheckResult{
			Outcome: rpc.ReconCheckOutcomeStarted,
			Status:  reconciliationRouteCheckingStatus(now),
		},
	}
	srv, _, _ := newGovernanceTestHandlerWithoutPoll(t, client)
	handler := srv.Handler()
	cookie := routeSessionCookie(t, handler)

	invalidBodies := []struct {
		name string
		body *string
	}{
		{name: "absent"},
		{name: "empty", body: new("")},
		{name: "null", body: new("null")},
		{name: "array", body: new("[]")},
		{name: "field", body: new(`{"force":true}`)},
		{name: "duplicate field", body: new(`{"force":true,"force":false}`)},
		{name: "second object", body: new(`{} {}`)},
		{name: "trailing value", body: new("{}\nnull")},
	}
	for _, tc := range invalidBodies {
		t.Run(tc.name, func(t *testing.T) {
			var request *http.Request
			if tc.body == nil {
				request = httptest.NewRequest(http.MethodPost, "/api/recon/check", nil)
			} else {
				request = httptest.NewRequest(http.MethodPost, "/api/recon/check", strings.NewReader(*tc.body))
			}
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if client.checkCalls != 0 {
		t.Fatalf("invalid bodies reached daemon %d times", client.checkCalls)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/recon/check", strings.NewReader(" \n{}\t"))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || client.checkCalls != 1 {
		t.Fatalf("valid body status=%d calls=%d body=%s", response.Code, client.checkCalls, response.Body.String())
	}
}

func TestReconciliationStatusDTOIsAllowlistedAndFormatsDates(t *testing.T) {
	t.Parallel()
	berlin := time.FixedZone("Europe/Berlin", 2*60*60)
	status := rpc.ReconAutomationStatus{
		Report: rpc.ReconFetchStatus{
			Configured: true, State: rpc.ReconReportStateRetryScheduled, Reason: rpc.ReconReportReasonNetworkUnavailable,
			ExpectedCoverageTo: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
			CoverageTo:         time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC),
			LastAttempt:        time.Date(2026, 7, 21, 8, 35, 0, 0, berlin),
			LastSuccess:        time.Date(2026, 7, 20, 8, 36, 0, 0, berlin),
			NextAttempt:        time.Date(2026, 7, 21, 9, 5, 0, 0, berlin),
			RetryAutomatic:     true, CanCheckNow: true,
			LastError: `HOSTILE token=private account=DU123 report=/private/flex.xml`,
		},
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateWaiting, Reason: rpc.ReconEvaluationReasonReportPending},
	}
	client := &reconciliationRouteClient{
		routeFakeClient: routeFakeClient{},
		statusResult:    &rpc.ReconStatusResult{Status: status},
	}
	srv, _, _ := newGovernanceTestHandlerWithoutPoll(t, client)
	handler := srv.Handler()
	cookie := routeSessionCookie(t, handler)
	request := httptest.NewRequest(http.MethodGet, "/api/recon/status", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body reconciliationResponseDTO
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	report := body.Reconciliation.Report
	if report.ExpectedCoverageTo != "2026-07-20" || report.CoverageTo != "2026-07-19" ||
		report.LastAttemptAt != "2026-07-21T06:35:00Z" || report.LastCompletedAt != "2026-07-20T06:36:00Z" ||
		report.NextAttemptAt != "2026-07-21T07:05:00Z" {
		t.Fatalf("formatted report=%+v", report)
	}
	raw := response.Body.String()
	for _, forbidden := range []string{"HOSTILE", "private", "DU123", "flex.xml", `"configured"`, `"busy"`, `"last_error"`, `"report_id"`, `"account"`, `"amount"`} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, raw)
		}
	}
}

func TestReconciliationCheckReturnsEveryTypedOutcome(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 6, 30, 0, 0, time.UTC)
	tests := []struct {
		outcome string
		status  rpc.ReconAutomationStatus
	}{
		{outcome: rpc.ReconCheckOutcomeStarted, status: reconciliationRouteCheckingStatus(now)},
		{outcome: rpc.ReconCheckOutcomeAlreadyChecking, status: reconciliationRouteCheckingStatus(now)},
		{outcome: rpc.ReconCheckOutcomeCooldown, status: reconciliationRouteRetryStatus(now)},
		{outcome: rpc.ReconCheckOutcomeActionRequired, status: reconciliationRouteActionStatus(now)},
	}
	for _, tc := range tests {
		t.Run(tc.outcome, func(t *testing.T) {
			client := &reconciliationRouteClient{
				routeFakeClient: routeFakeClient{},
				checkResult:     &rpc.ReconCheckResult{Outcome: tc.outcome, Status: tc.status},
			}
			srv, _, _ := newGovernanceTestHandlerWithoutPoll(t, client)
			handler := srv.Handler()
			cookie := routeSessionCookie(t, handler)
			request := httptest.NewRequest(http.MethodPost, "/api/recon/check", strings.NewReader(`{}`))
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body reconciliationResponseDTO
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Outcome != tc.outcome || body.Reconciliation.Report.State != tc.status.Report.State {
				t.Fatalf("response=%+v", body)
			}
		})
	}
}

func TestReconciliationRoutesUseFixedSafeErrors(t *testing.T) {
	t.Parallel()
	privateText := "HOSTILE broker token=secret account=DU123 /private/report.xml"
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		client     *reconciliationRouteClient
		wantStatus int
		wantText   string
	}{
		{
			name: "status daemon failure", method: http.MethodGet, path: "/api/recon/status",
			client:     &reconciliationRouteClient{routeFakeClient: routeFakeClient{}, statusErr: errors.New(privateText)},
			wantStatus: http.StatusServiceUnavailable, wantText: "daily report status unavailable",
		},
		{
			name: "check daemon failure", method: http.MethodPost, path: "/api/recon/check", body: `{}`,
			client:     &reconciliationRouteClient{routeFakeClient: routeFakeClient{}, checkErr: errors.New(privateText)},
			wantStatus: http.StatusServiceUnavailable, wantText: "daily report check unavailable",
		},
		{
			name: "invalid status result", method: http.MethodGet, path: "/api/recon/status",
			client: &reconciliationRouteClient{routeFakeClient: routeFakeClient{}, statusResult: &rpc.ReconStatusResult{Status: rpc.ReconAutomationStatus{
				Report: rpc.ReconFetchStatus{State: privateText}, Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateWaiting, Reason: rpc.ReconEvaluationReasonReportPending},
			}}},
			wantStatus: http.StatusBadGateway, wantText: "invalid daily report status",
		},
		{
			name: "invalid check result", method: http.MethodPost, path: "/api/recon/check", body: `{}`,
			client: &reconciliationRouteClient{routeFakeClient: routeFakeClient{}, checkResult: &rpc.ReconCheckResult{
				Outcome: privateText, Status: reconciliationRouteCheckingStatus(time.Date(2026, 7, 21, 6, 30, 0, 0, time.UTC)),
			}},
			wantStatus: http.StatusBadGateway, wantText: "invalid daily report check result",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newGovernanceTestHandlerWithoutPoll(t, tc.client)
			handler := srv.Handler()
			cookie := routeSessionCookie(t, handler)
			var request *http.Request
			if tc.method == http.MethodPost {
				request = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			} else {
				request = httptest.NewRequest(tc.method, tc.path, nil)
			}
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.wantStatus || !strings.Contains(response.Body.String(), tc.wantText) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), privateText) || strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "DU123") {
				t.Fatalf("private daemon text leaked: %s", response.Body.String())
			}
		})
	}
}

func reconciliationRouteCheckingStatus(now time.Time) rpc.ReconAutomationStatus {
	return rpc.ReconAutomationStatus{
		Report: rpc.ReconFetchStatus{
			Configured: true, State: rpc.ReconReportStateChecking, ExpectedCoverageTo: now.AddDate(0, 0, -1),
			LastAttempt: now, RetryAutomatic: true, Busy: true,
		},
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateChecking, Reason: rpc.ReconEvaluationReasonReportPending},
	}
}

func reconciliationRouteRetryStatus(now time.Time) rpc.ReconAutomationStatus {
	return rpc.ReconAutomationStatus{
		Report: rpc.ReconFetchStatus{
			Configured: true, State: rpc.ReconReportStateRetryScheduled, Reason: rpc.ReconReportReasonReportNotReady,
			ExpectedCoverageTo: now.AddDate(0, 0, -1), LastAttempt: now, NextAttempt: now.Add(30 * time.Minute),
			RetryAutomatic: true, CanCheckNow: false,
		},
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateWaiting, Reason: rpc.ReconEvaluationReasonReportPending},
	}
}

func reconciliationRouteActionStatus(now time.Time) rpc.ReconAutomationStatus {
	return rpc.ReconAutomationStatus{
		Report: rpc.ReconFetchStatus{
			State: rpc.ReconReportStateActionRequired, Reason: rpc.ReconReportReasonTokenMissing,
			ExpectedCoverageTo: now.AddDate(0, 0, -1), CanCheckNow: true,
		},
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateWaiting, Reason: rpc.ReconEvaluationReasonReportPending},
	}
}
