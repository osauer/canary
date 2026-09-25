package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appalerts "github.com/osauer/canary/v2/internal/app/alerts"
	apphttp "github.com/osauer/canary/v2/internal/app/http"
)

func TestAppPushTestPostsTheLocalDiagnosticAndExplainsTheWitness(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != apphttp.PushDiagnosticPath {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(appalerts.DiagnosticResult{
			NoticeID: "diagnostic-0123456789abcdef", State: "partial_acceptance", Accepted: true,
			Targets: []appalerts.DiagnosticTarget{
				{Device: "iPhone", DeviceRef: "0123456789ab", Class: "push_service_accepted", HTTPStatus: 201, Accepted: true},
				{Device: "iPhone", DeviceRef: "ba9876543210", Class: "dead_subscription", HTTPStatus: 410},
			},
		})
	}))
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	var stdout, stderr bytes.Buffer
	if code := runAppPushTestWithIO([]string{"--addr", addr}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Diagnostic push diagnostic-0123456789abcdef: partial_acceptance",
		"push_service_accepted 201",
		"dead_subscription 410 (subscription expired and removed",
		"that is not delivery",
		"status` for the device receipt",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if requests != 1 {
		t.Fatalf("requests=%d", requests)
	}

	stdout.Reset()
	if code := runAppPushTestWithIO([]string{"--addr", addr, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("json exit=%d", code)
	}
	var decoded appalerts.DiagnosticResult
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || decoded.NoticeID != "diagnostic-0123456789abcdef" {
		t.Fatalf("json output %q err=%v", stdout.String(), err)
	}
}

func TestAppPushTestFailsWithoutASubscription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(appalerts.DiagnosticResult{State: "no_subscription", Targets: []appalerts.DiagnosticTarget{}})
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := runAppPushTestWithIO([]string{"--addr", strings.TrimPrefix(server.URL, "http://")}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "No active push subscription") || !strings.Contains(stdout.String(), "Enable notifications") {
		t.Fatalf("output:\n%s", stdout.String())
	}
}
