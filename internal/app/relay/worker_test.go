package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	appweb "github.com/osauer/canary/v2/web/app"
)

func TestWorkerPairingURLAddsRemoteRoute(t *testing.T) {
	t.Parallel()

	w := &Worker{routeID: "r_test"}
	got := w.PairingURL("https://remote.osauer.dev/pair.html?pair=p1&nonce=n1")
	if !strings.Contains(got, "remote=r_test") {
		t.Fatalf("PairingURL = %q, want remote route", got)
	}
	if !strings.Contains(got, "pair=p1") || !strings.Contains(got, "nonce=n1") {
		t.Fatalf("PairingURL = %q, lost pairing parameters", got)
	}
}

func TestForwardableAppPathBlocksPairingSessionCreation(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/api/pairing/sessions", "/api/devices", "/api/devices/prune", "/api/app-status"} {
		if forwardableAppPath(path) {
			t.Fatalf("local-control path %q should not be forwarded through remote relay", path)
		}
	}
	for _, path := range []string{"/", "/pair.html?remote=r1&pair=p&nonce=n", "/api/bootstrap", "/api/events", "/app.js?v=1"} {
		if !forwardableAppPath(path) {
			t.Fatalf("path %q should be forwardable", path)
		}
	}
}

func TestForwardableAppPathUsesEmbeddedJavaScriptFiles(t *testing.T) {
	t.Parallel()

	entries, err := appweb.Files.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded app root: %v", err)
	}
	jsCount := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		jsCount++
		for _, path := range []string{"/" + entry.Name(), "/" + entry.Name() + "?v=test"} {
			if !forwardableAppPath(path) {
				t.Errorf("embedded JavaScript path %q should be forwardable", path)
			}
		}
	}
	if jsCount == 0 {
		t.Fatal("embedded app contains no JavaScript files")
	}
	for _, path := range []string{
		"/not-embedded.js",
		"/nested/app.js",
		"/app.js/extra",
		"/app.js.map",
		"/../app.js",
		"/%2e%2e/app.js",
		"//app.js",
	} {
		if forwardableAppPath(path) {
			t.Errorf("non-embedded JavaScript path %q should not be forwardable", path)
		}
	}
}

func TestWorkerServeRequestForwardsAllowedPath(t *testing.T) {
	t.Parallel()

	var gotProto, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	worker := &Worker{
		originURL:  srv.URL,
		publicURL:  "https://remote.osauer.dev",
		httpClient: srv.Client(),
	}
	var frames []frame
	err := worker.serveRequest(context.Background(), frame{
		Type:   "request",
		ID:     "req-1",
		Method: http.MethodPost,
		Path:   "/api/settings",
		Body:   base64.StdEncoding.EncodeToString([]byte(`{"x":1}`)),
	}, func(_ context.Context, f frame) error {
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatalf("serveRequest: %v", err)
	}
	if gotProto != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want https", gotProto)
	}
	if gotBody != `{"x":1}` {
		t.Fatalf("body = %q, want forwarded JSON", gotBody)
	}
	if len(frames) != 3 {
		t.Fatalf("frames len = %d, want 3: %#v", len(frames), frames)
	}
	if frames[0].Type != "response_start" || frames[0].Status != http.StatusOK {
		t.Fatalf("start frame = %#v", frames[0])
	}
	var payload map[string]bool
	chunk, err := base64.StdEncoding.DecodeString(frames[1].Body)
	if err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("decode response chunk: %v", err)
	}
	if !payload["ok"] || frames[2].Type != "response_end" {
		t.Fatalf("unexpected response frames: %#v", frames)
	}
}

func TestWorkerRequestCancellationIsPerRequest(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	cancelled := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(origin.Close)

	relayResult := make(chan error, 1)
	relay := httptest.NewServer(http.HandlerFunc(func(wr http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(wr, r, nil)
		if err != nil {
			relayResult <- err
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "test complete")
		write := func(f frame) error {
			raw, err := json.Marshal(f)
			if err != nil {
				return err
			}
			return conn.Write(r.Context(), websocket.MessageText, raw)
		}
		if err := write(frame{Type: "request", ID: "stream", Path: "/api/events"}); err != nil {
			relayResult <- err
			return
		}
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			relayResult <- errors.New("stream request did not reach the local app")
			return
		}
		if err := write(frame{Type: "request", ID: "other", Path: "/api/bootstrap"}); err != nil {
			relayResult <- err
			return
		}
		if err := write(frame{Type: "request_cancel", ID: "stream"}); err != nil {
			relayResult <- err
			return
		}
		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			relayResult <- errors.New("request_cancel frame did not cancel the local app request")
			return
		}

		readCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for {
			_, raw, err := conn.Read(readCtx)
			if err != nil {
				relayResult <- err
				return
			}
			var got frame
			if err := json.Unmarshal(raw, &got); err != nil {
				relayResult <- err
				return
			}
			if got.ID == "other" && got.Type == "response_end" {
				relayResult <- nil
				return
			}
		}
	}))
	t.Cleanup(relay.Close)

	worker := &Worker{
		originURL:    origin.URL,
		publicURL:    "https://remote.osauer.dev",
		connectorURL: "ws" + strings.TrimPrefix(relay.URL, "http"),
		token:        "test-connector-token",
		httpClient:   http.DefaultClient,
	}
	connectDone := make(chan error, 1)
	go func() { connectDone <- worker.connectOnce(t.Context()) }()

	select {
	case err := <-relayResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay cancellation exchange did not finish")
	}
	select {
	case <-connectDone:
	case <-time.After(2 * time.Second):
		t.Fatal("connector did not return after relay closed")
	}
}

// fakeRegisterJSON mimics the relay's /api/register response, pointing the
// connector websocket URL back at the fake relay server.
func fakeRegisterJSON(baseURL, routeID string, expiresAt time.Time) []byte {
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http")
	return fmt.Appendf(nil,
		`{"route_id":%q,"public_url":%q,"connector_url":%q,"connector_token":%q,"expires_at":%q}`,
		routeID, baseURL, wsURL+"/api/connect?route_id="+routeID, "tok-"+routeID,
		expiresAt.UTC().Format(time.RFC3339))
}

func TestNewWorkerResumesStoredRoute(t *testing.T) {
	t.Parallel()

	var got registerRequest
	var persisted RouteRegistration
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode register request: %v", err)
		}
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, got.RouteID, time.Now().Add(defaultRouteTTL)))
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	w, err := NewWorker(WorkerOptions{
		BaseURL:              srv.URL,
		OriginURL:            "http://127.0.0.1:1",
		HTTPClient:           srv.Client(),
		ResumeRouteID:        "r_stable",
		ResumeConnectorToken: "tok-r_stable",
		OnRoute: func(reg RouteRegistration) error {
			persisted = reg
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	// The held route already names the stable route id before any network
	// round trip, so pairing URLs stay correct through a relay outage.
	if !strings.Contains(w.PairingURL("https://relay.example/pair.html?pair=p"), "remote=r_stable") {
		t.Fatalf("worker did not seed the held route before registration")
	}
	if err := w.registerCurrent(t.Context()); err != nil {
		t.Fatalf("registerCurrent: %v", err)
	}
	if got.RouteID != "r_stable" || got.ConnectorToken != "tok-r_stable" {
		t.Fatalf("register request = %#v, want stored route credentials", got)
	}
	if !strings.Contains(w.PairingURL("https://relay.example/pair.html?pair=p"), "remote=r_stable") {
		t.Fatalf("worker did not keep resumed route")
	}
	if persisted.RouteID != "r_stable" || persisted.ConnectorToken != "tok-r_stable" {
		t.Fatalf("persisted route = %#v, want resumed route", persisted)
	}
}

func TestNewWorkerFallsBackToFreshRouteWhenStoredRouteExpired(t *testing.T) {
	t.Parallel()

	var requests []registerRequest
	var persisted RouteRegistration
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, r *http.Request) {
		var got registerRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode register request: %v", err)
		}
		requests = append(requests, got)
		if got.RouteID == "r_expired" {
			wr.Header().Set("Content-Type", "application/json")
			wr.WriteHeader(http.StatusGone)
			_, _ = wr.Write([]byte(`{"error":"route expired"}`))
			return
		}
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, "r_fresh", time.Now().Add(defaultRouteTTL)))
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	w, err := NewWorker(WorkerOptions{
		BaseURL:              srv.URL,
		OriginURL:            "http://127.0.0.1:1",
		HTTPClient:           srv.Client(),
		ResumeRouteID:        "r_expired",
		ResumeConnectorToken: "tok-r_expired",
		OnRoute: func(reg RouteRegistration) error {
			persisted = reg
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if err := w.registerCurrent(t.Context()); err != nil {
		t.Fatalf("registerCurrent: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("register requests = %#v, want resume attempt then fresh registration", requests)
	}
	if requests[0].RouteID != "r_expired" || requests[1].RouteID != "" {
		t.Fatalf("register requests = %#v, want expired route then fresh route", requests)
	}
	if !strings.Contains(w.PairingURL("https://relay.example/pair.html?pair=p"), "remote=r_fresh") {
		t.Fatalf("worker did not switch to fresh route after expiry")
	}
	if persisted.RouteID != "r_fresh" {
		t.Fatalf("persisted route = %#v, want fresh route", persisted)
	}
}

func TestWorkerRunRetriesFailedInitialRegistration(t *testing.T) {
	t.Parallel()

	var registerCalls atomic.Int64
	connected := make(chan struct{}, 1)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, _ *http.Request) {
		if registerCalls.Add(1) == 1 {
			// Simulate the relay (or DNS) being unreachable at app boot.
			wr.WriteHeader(http.StatusBadGateway)
			return
		}
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, "r_lazy", time.Now().Add(defaultRouteTTL)))
	})
	mux.HandleFunc("/api/connect", func(wr http.ResponseWriter, r *http.Request) {
		acceptAndHold(wr, r, func() {
			select {
			case connected <- struct{}{}:
			default:
			}
		})
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewWorker(WorkerOptions{BaseURL: srv.URL, OriginURL: "http://127.0.0.1:1", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewWorker must not fail on registration outages: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("connector never connected after failed initial registration (register calls: %d)", registerCalls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after parent cancel")
	}
}

func TestRegisterCurrentFallsBackOnUnauthorizedResume(t *testing.T) {
	t.Parallel()

	var requests []registerRequest
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, r *http.Request) {
		var got registerRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode register request: %v", err)
		}
		requests = append(requests, got)
		if got.RouteID != "" {
			// Relay-side token mismatch: e.g. the Durable Object storage
			// was wiped by a redeploy while the Mac kept the old token.
			wr.Header().Set("Content-Type", "application/json")
			wr.WriteHeader(http.StatusUnauthorized)
			_, _ = wr.Write([]byte(`{"error":"unauthorized route resume"}`))
			return
		}
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, "r_fresh", time.Now().Add(defaultRouteTTL)))
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	w, err := NewWorker(WorkerOptions{
		BaseURL:              srv.URL,
		OriginURL:            "http://127.0.0.1:1",
		HTTPClient:           srv.Client(),
		ResumeRouteID:        "r_stale",
		ResumeConnectorToken: "tok-stale",
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if err := w.registerCurrent(t.Context()); err != nil {
		t.Fatalf("registerCurrent: %v", err)
	}
	if len(requests) != 2 || requests[0].RouteID != "r_stale" || requests[1].RouteID != "" {
		t.Fatalf("register requests = %#v, want rejected resume then fresh registration", requests)
	}
	if !strings.Contains(w.PairingURL("https://relay.example/pair.html?pair=p"), "remote=r_fresh") {
		t.Fatalf("worker did not switch to the fresh route after unauthorized resume")
	}
}

func TestRegisterCurrentKeepsRouteOnTransientFailure(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, _ *http.Request) {
		wr.WriteHeader(http.StatusServiceUnavailable)
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	w, err := NewWorker(WorkerOptions{
		BaseURL:              srv.URL,
		OriginURL:            "http://127.0.0.1:1",
		HTTPClient:           srv.Client(),
		ResumeRouteID:        "r_keep",
		ResumeConnectorToken: "tok-keep",
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if err := w.registerCurrent(t.Context()); err == nil {
		t.Fatalf("registerCurrent should surface the transient failure")
	}
	// The held route survives so a later retry resumes it instead of
	// minting a new route id (which would orphan paired phones).
	if !strings.Contains(w.PairingURL("https://relay.example/pair.html?pair=p"), "remote=r_keep") {
		t.Fatalf("transient registration failure must not drop the held route")
	}
}

// acceptAndHold upgrades the connection, calls onAccept, then holds the
// connection open until the connector closes it (forced cycle or shutdown).
func acceptAndHold(wr http.ResponseWriter, r *http.Request, onAccept func()) {
	conn, err := websocket.Accept(wr, r, nil)
	if err != nil {
		return
	}
	if onAccept != nil {
		onAccept()
	}
	_, _, _ = conn.Read(r.Context())
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func TestWorkerRunCyclesConnectionToSlideRouteTTL(t *testing.T) {
	t.Parallel()

	var accepts atomic.Int64
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, _ *http.Request) {
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, "r_cycle", time.Now().Add(defaultRouteTTL)))
	})
	mux.HandleFunc("/api/connect", func(wr http.ResponseWriter, r *http.Request) {
		acceptAndHold(wr, r, func() { accepts.Add(1) })
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewWorker(WorkerOptions{BaseURL: srv.URL, OriginURL: "http://127.0.0.1:1", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	// Register before Run so the response-derived cycle interval can be
	// overridden; Run skips registration for a held route.
	if err := w.registerCurrent(ctx); err != nil {
		t.Fatalf("registerCurrent: %v", err)
	}
	w.mu.Lock()
	w.cycleEvery = 50 * time.Millisecond // bypass the prod minCycleEvery floor
	w.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	// Three accepts within 2s prove forced cycles reconnect promptly with
	// reset backoff (the backoff path would only manage two: 0s, 1s, 3s).
	deadline := time.Now().Add(2 * time.Second)
	for accepts.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("relay accepted %d connector connections, want >= 3 (forced cycle must reconnect without backoff)", accepts.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after parent cancel")
	}
}

func TestWorkerRunReRegistersOnGoneRoute(t *testing.T) {
	t.Parallel()

	var (
		mu        sync.Mutex
		registers int
		goneSent  bool
	)
	connected := make(chan struct{}, 4)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		registers++
		routeID := fmt.Sprintf("r_%d", registers)
		mu.Unlock()
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, routeID, time.Now().Add(defaultRouteTTL)))
	})
	mux.HandleFunc("/api/connect", func(wr http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := !goneSent
		goneSent = true
		mu.Unlock()
		if first {
			// Simulate a reaped route: the Mac was offline past the TTL.
			wr.Header().Set("Content-Type", "application/json")
			wr.WriteHeader(http.StatusGone)
			_, _ = wr.Write([]byte(`{"error":"route expired"}`))
			return
		}
		acceptAndHold(wr, r, func() { connected <- struct{}{} })
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewWorker(WorkerOptions{BaseURL: srv.URL, OriginURL: "http://127.0.0.1:1", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if err := w.registerCurrent(ctx); err != nil {
		t.Fatalf("registerCurrent: %v", err)
	}
	if got := w.PairingURL("https://relay.example/pair.html?pair=p"); !strings.Contains(got, "remote=r_1") {
		t.Fatalf("initial PairingURL = %q, want remote=r_1", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatalf("connector did not reconnect after 410 re-register")
	}
	mu.Lock()
	gotRegisters := registers
	mu.Unlock()
	if gotRegisters != 2 {
		t.Fatalf("registers = %d, want 2 (initial registration, resume after 410)", gotRegisters)
	}
	if got := w.PairingURL("https://relay.example/pair.html?pair=p"); !strings.Contains(got, "remote=r_2") {
		t.Fatalf("PairingURL after re-register = %q, want remote=r_2 (new pairings must use the fresh route)", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after parent cancel")
	}
}

func TestWorkerRunReturnsOnParentCancel(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, _ *http.Request) {
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, "r_stop", time.Now().Add(defaultRouteTTL)))
	})
	mux.HandleFunc("/api/connect", func(wr http.ResponseWriter, r *http.Request) {
		acceptAndHold(wr, r, nil)
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewWorker(WorkerOptions{BaseURL: srv.URL, OriginURL: "http://127.0.0.1:1", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !w.Status().Connected {
		if time.Now().After(deadline) {
			t.Fatalf("connector never connected: %#v", w.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after parent cancel")
	}
	if w.Status().Connected {
		t.Fatalf("status still connected after shutdown: %#v", w.Status())
	}
}
