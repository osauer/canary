package apphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type briefRouteClient struct {
	routeFakeClient
	ackParams     rpc.BriefAckParams
	signoffParams rpc.CapitalEventParams
	ackResult     *rpc.BriefAckResult
	signoffResult *rpc.RiskPolicyWriteResult
	ackErr        error
	signoffErr    error
	ackCalls      int
	signoffCalls  int
}

func (c *briefRouteClient) BriefAck(_ context.Context, params rpc.BriefAckParams) (*rpc.BriefAckResult, error) {
	c.ackCalls++
	c.ackParams = params
	return c.ackResult, c.ackErr
}

func (c *briefRouteClient) ReconcileSignoff(_ context.Context, params rpc.CapitalEventParams) (*rpc.RiskPolicyWriteResult, error) {
	c.signoffCalls++
	c.signoffParams = params
	return c.signoffResult, c.signoffErr
}

func TestBriefWriteRoutesRequireAuth(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	for _, path := range []string{"/api/brief/seen", "/api/recon/signoff"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{}`)))
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Errorf("POST %s status=%d, want 401; body=%s", path, res.Code, res.Body.String())
		}
	}
}

func TestBriefSeenAssignsPairedDeviceOrigin(t *testing.T) {
	t.Parallel()
	client := &briefRouteClient{ackResult: &rpc.BriefAckResult{OK: true, Kind: "morning", Day: "2026-07-18"}}
	handler := newTestHandlerWithClient(t, client).Handler()
	cookie := routeSessionCookie(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/api/brief/seen", bytes.NewReader([]byte(`{
		"kind":"morning",
		"brief_fingerprint":"sha256:brief",
		"origin":"agent"
	}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", res.Code, res.Body.String())
	}
	if client.ackCalls != 1 || client.ackParams.Kind != "morning" || client.ackParams.BriefFingerprint != "sha256:brief" || client.ackParams.Origin != rpc.OrderOriginPairedDevice {
		t.Fatalf("ack params=%+v calls=%d", client.ackParams, client.ackCalls)
	}
	var got rpc.BriefAckResult
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Day != "2026-07-18" {
		t.Fatalf("ack result=%+v", got)
	}
}

func TestReconcileSignoffPinsReportAndAssignsPairedDeviceOrigin(t *testing.T) {
	t.Parallel()
	client := &briefRouteClient{signoffResult: &rpc.RiskPolicyWriteResult{OK: true, Message: "reconciled"}}
	handler := newTestHandlerWithClient(t, client).Handler()
	cookie := routeSessionCookie(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/api/recon/signoff", bytes.NewReader([]byte(`{
		"report_id":"recon-pinned",
		"origin":"agent"
	}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", res.Code, res.Body.String())
	}
	if client.signoffCalls != 1 || client.signoffParams.Type != "reconcile" || client.signoffParams.Report != "recon-pinned" || client.signoffParams.Origin != rpc.OrderOriginPairedDevice {
		t.Fatalf("signoff params=%+v calls=%d", client.signoffParams, client.signoffCalls)
	}
}

func TestReconcileSignoffRejectsEmptyReportID(t *testing.T) {
	t.Parallel()
	client := &briefRouteClient{}
	handler := newTestHandlerWithClient(t, client).Handler()
	cookie := routeSessionCookie(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/api/recon/signoff", bytes.NewReader([]byte(`{"report_id":"  "}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "report_id required") {
		t.Fatalf("status=%d, want 400 report_id error; body=%s", res.Code, res.Body.String())
	}
	if client.signoffCalls != 0 {
		t.Fatalf("daemon signoff calls=%d, want 0", client.signoffCalls)
	}
}

func TestReconcileSignoffSurfacesDaemonRefusal(t *testing.T) {
	t.Parallel()
	client := &briefRouteClient{signoffErr: fmt.Errorf("policy.capital_event: %w", &rpc.Error{
		Code: rpc.CodeBadRequest, Message: "report recon-old is superseded",
	})}
	handler := newTestHandlerWithClient(t, client).Handler()
	cookie := routeSessionCookie(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/api/recon/signoff", bytes.NewReader([]byte(`{"report_id":"recon-old"}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", res.Code, res.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "report recon-old is superseded" {
		t.Fatalf("daemon refusal=%q, want exact daemon message", body["error"])
	}
}
