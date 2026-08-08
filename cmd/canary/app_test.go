package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/auth"
	apphttp "github.com/osauer/canary/v2/internal/app/http"
	"github.com/osauer/canary/v2/internal/app/state"
	hyperserve "github.com/osauer/hyperserve/pkg/server"
)

func TestNewAppLoggerEmitsOneLeveledPhysicalLine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	logger := newAppLogger(&out)
	logger.Info("canary app serving", "detail", "first\nsecond")

	got := strings.TrimSuffix(out.String(), "\n")
	if strings.Contains(got, "\n") {
		t.Fatalf("logger emitted an unlevelled continuation line: %q", got)
	}
	for _, want := range []string{"time=", "level=INFO", `msg="canary app serving"`, `detail="first\nsecond"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("log record %q does not contain %q", got, want)
		}
	}
}

func TestRunAppServeParseFailureIsOneLeveledLine(t *testing.T) {
	oldSlog := slog.Default()
	oldHyperServe := hyperserve.DefaultLogger()
	t.Cleanup(func() {
		slog.SetDefault(oldSlog)
		hyperserve.SetDefaultLogger(oldHyperServe)
	})

	var stdout, stderr bytes.Buffer
	if exit := runAppServeWithIO([]string{"--definitely-not-a-real-flag"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if stdout.Len() != 0 {
		t.Fatalf("parse failure wrote unlevelled stdout: %q", stdout.String())
	}
	got := strings.TrimSuffix(stderr.String(), "\n")
	if strings.Contains(got, "\n") {
		t.Fatalf("parse failure emitted more than one physical line: %q", got)
	}
	for _, want := range []string{"time=", "level=ERROR", `msg="canary app arguments rejected"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("parse-failure record %q does not contain %q", got, want)
		}
	}
}

func TestFetchAppStatusRequiresTypedSchema(t *testing.T) {
	t.Parallel()

	want := apphttp.AppStatusDTO{
		SchemaVersion: apphttp.AppStatusSchemaVersion,
		Version:       "test",
		State:         apphttp.AppStatusStateReady,
		AlertDispatcher: apphttp.AlertDeliveryHealthDTO{
			State: state.AlertDeliveryHealthHealthy,
		},
	}
	addr := startAppStatusServer(t, want)
	got, err := fetchAppStatus(context.Background(), addr)
	if err != nil {
		t.Fatalf("fetchAppStatus: %v", err)
	}
	if got.SchemaVersion != want.SchemaVersion || got.Version != want.Version || got.AlertDispatcher.State != want.AlertDispatcher.State {
		t.Fatalf("status=%+v, want selected fields from %+v", got, want)
	}

	want.SchemaVersion = "app-status-future"
	addr = startAppStatusServer(t, want)
	if _, err := fetchAppStatus(context.Background(), addr); err == nil || !strings.Contains(err.Error(), "unsupported app status schema") {
		t.Fatalf("future schema error=%v", err)
	}
}

func TestCreatePairingSessionUsesAppPublicURLByDefault(t *testing.T) {
	t.Parallel()

	var gotBody string
	addr := startPairingSessionServer(t, func(body []byte) {
		gotBody = string(body)
	})

	session, err := createPairingSession(addr, "")
	if err != nil {
		t.Fatalf("createPairingSession: %v", err)
	}
	if gotBody != "{}" {
		t.Fatalf("request body = %q, want empty JSON object", gotBody)
	}
	if !strings.HasPrefix(session.URL, "http://server.example/pair.html?") {
		t.Fatalf("session URL = %q, want server-provided public URL", session.URL)
	}
}

func TestCreatePairingSessionSendsExplicitPublicURLOverride(t *testing.T) {
	t.Parallel()

	var got struct {
		PublicURL string `json:"public_url"`
	}
	addr := startPairingSessionServer(t, func(body []byte) {
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
	})

	session, err := createPairingSession(addr, "http://127.0.0.1:8765")
	if err != nil {
		t.Fatalf("createPairingSession: %v", err)
	}
	if got.PublicURL != "http://127.0.0.1:8765" {
		t.Fatalf("public_url = %q, want explicit override", got.PublicURL)
	}
	if session.ID == "" {
		t.Fatalf("empty pairing session: %#v", session)
	}
}

func TestAppPairPublicURLOverrideRequiresExplicitFlag(t *testing.T) {
	t.Parallel()

	implicit := flag.NewFlagSet("implicit", flag.ContinueOnError)
	implicit.SetOutput(io.Discard)
	implicitPublicURL := implicit.String("public-url", "http://derived.example", "")
	if err := implicit.Parse(nil); err != nil {
		t.Fatalf("parse implicit flags: %v", err)
	}
	if got := appPairPublicURLOverride(implicit, *implicitPublicURL, false); got != "" {
		t.Fatalf("implicit public URL override = %q, want empty", got)
	}
	if got := appPairPublicURLOverride(implicit, *implicitPublicURL, true); got != "http://derived.example" {
		t.Fatalf("env public URL override = %q, want explicit default", got)
	}

	explicit := flag.NewFlagSet("explicit", flag.ContinueOnError)
	explicit.SetOutput(io.Discard)
	explicitPublicURL := explicit.String("public-url", "", "")
	if err := explicit.Parse([]string{"--public-url", " http://127.0.0.1:8765/ "}); err != nil {
		t.Fatalf("parse explicit flags: %v", err)
	}
	if got := appPairPublicURLOverride(explicit, *explicitPublicURL, false); got != "http://127.0.0.1:8765/" {
		t.Fatalf("explicit public URL override = %q, want trimmed flag value", got)
	}
}

func startPairingSessionServer(t *testing.T, observe func([]byte)) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/pairing/sessions" {
			t.Errorf("request = %s %s, want POST /api/pairing/sessions", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if observe != nil {
			observe(body)
		}
		writePairingSession(t, w)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().String()
}

func startAppStatusServer(t *testing.T, status apphttp.AppStatusDTO) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != apphttp.AppStatusPath {
			t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, apphttp.AppStatusPath)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(status); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().String()
}

func writePairingSession(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	now := time.Now().UTC()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(auth.PairingSession{
		ID:        "pair-test",
		Nonce:     "nonce-test",
		URL:       "http://server.example/pair.html?pair=pair-test&nonce=nonce-test",
		ExpiresAt: now.Add(time.Minute),
		CreatedAt: now,
	}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
