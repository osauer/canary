package apphttp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"net/http"
	"net/http/httptest"
	"strings"

	"testing"
	"time"

	hyperserve "github.com/osauer/hyperserve/v2"

	"github.com/osauer/canary/v2/internal/app/alerts"
	"github.com/osauer/canary/v2/internal/app/auth"
	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/app/relay"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

func routeOpenOrderView() rpc.OrderView {
	return rpc.OrderView{
		OrderRef: "ord-1", PreviewTokenID: "tok-1", Account: "DU123",
		Endpoint: "127.0.0.1:7497", Mode: "paper", Symbol: "SPY", SecType: "STK",
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		Quantity: 2, LimitPrice: 450.25, Status: "submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted, SendState: "sent", Open: true,
		UpdatedAt: time.Now().UTC(),
	}
}

func TestBootstrapRequiresAuth(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", res.Code, res.Body.String())
	}
}

type routeEdgeClient struct {
	routeFakeClient
	params rpc.EdgeSnapshotParams
	calls  int
}

func (c *routeEdgeClient) EdgeSnapshot(_ context.Context, params rpc.EdgeSnapshotParams) (*rpc.EdgeResult, error) {
	c.calls++
	c.params = params
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	mode := "explicit"
	reason := "explicit_override"
	if params.AutomaticHorizon {
		mode = "automatic"
		reason = "best_available"
	}
	return &rpc.EdgeResult{
		SchemaVersion: "canary-edge-v3", State: rpc.EdgeStateCurrent, AsOf: now, Window: params.Window,
		HorizonSessions: params.HorizonSessions, AutomaticHorizon: params.AutomaticHorizon,
		HorizonSelection: rpc.EdgeHorizonSelection{Mode: mode, Reason: reason, MinimumSample: 3, MinimumCoveragePct: 25},
		ActionRollups:    []rpc.EdgeActionRollup{}, Findings: []rpc.EdgeFinding{},
		Options: rpc.EdgeOptionReview{Realized: rpc.EdgeOptionRealizedReview{Episodes: []rpc.EdgeOptionEpisodeSummary{}}, Open: rpc.EdgeOptionOpenReview{Positions: []rpc.EdgeOptionOpenPositionSummary{}}}, Coverage: rpc.EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}},
		Method:      rpc.EdgeMethod{Metric: "Decision price impact", HeadlineSelection: "disclosed", FindingRanking: "disclosed", MaterialityGate: "disclosed", AutomaticHorizon: "disclosed", MarketContext: "disclosed", NoCausalClaim: true, NoPredictiveClaim: true, NotInvestmentAdvice: true},
		Fingerprint: "edge_opaque", NotExecution: true,
	}, nil
}

func TestEdgeRouteRequiresReadAuthAndForwardsOnlyBoundedTypedInputs(t *testing.T) {
	client := &routeEdgeClient{}
	handler := newTestHandlerWithClient(t, client).Handler()

	unauth := httptest.NewRequest(http.MethodGet, "/api/edge?window=365d&horizon=5&limit=2&change=change_opaque", nil)
	unauthRes := httptest.NewRecorder()
	handler.ServeHTTP(unauthRes, unauth)
	if unauthRes.Code != http.StatusUnauthorized || client.calls != 0 {
		t.Fatalf("unauthenticated Edge read status=%d calls=%d", unauthRes.Code, client.calls)
	}

	cookie := routeSessionCookie(t, handler)
	req := httptest.NewRequest(http.MethodGet, "/api/edge?window=365d&horizon=5&limit=2&change=change_opaque", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("Edge status=%d body=%s", res.Code, res.Body.String())
	}
	want := (rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 5, Limit: 2, ChangeID: "change_opaque"})
	if client.params != want || client.calls != 1 {
		t.Fatalf("Edge params=%+v calls=%d want %+v/1", client.params, client.calls, want)
	}
	if res.Header().Get("Cache-Control") != "no-store" || !strings.Contains(res.Body.String(), `"not_execution":true`) {
		t.Fatalf("Edge response headers/body = %v %s", res.Header(), res.Body.String())
	}

	automatic := httptest.NewRequest(http.MethodGet, "/api/edge", nil)
	automatic.AddCookie(cookie)
	automaticRes := httptest.NewRecorder()
	handler.ServeHTTP(automaticRes, automatic)
	if automaticRes.Code != http.StatusOK {
		t.Fatalf("automatic Edge status=%d body=%s", automaticRes.Code, automaticRes.Body.String())
	}
	automaticWant := rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 20, AutomaticHorizon: true, Limit: rpc.MaxEdgeFindings}
	if client.params != automaticWant || client.calls != 2 {
		t.Fatalf("automatic Edge params=%+v calls=%d want %+v/2", client.params, client.calls, automaticWant)
	}
	option := httptest.NewRequest(http.MethodGet, "/api/edge?option=option_opaque", nil)
	option.AddCookie(cookie)
	optionRes := httptest.NewRecorder()
	handler.ServeHTTP(optionRes, option)
	optionWant := rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 20, AutomaticHorizon: true, Limit: rpc.MaxEdgeFindings, OptionID: "option_opaque"}
	if optionRes.Code != http.StatusOK || client.params != optionWant || client.calls != 3 {
		t.Fatalf("option Edge params=%+v status=%d calls=%d want %+v/3", client.params, optionRes.Code, client.calls, optionWant)
	}

	bad := httptest.NewRequest(http.MethodGet, "/api/edge?horizon=twenty", nil)
	bad.AddCookie(cookie)
	badRes := httptest.NewRecorder()
	handler.ServeHTTP(badRes, bad)
	if badRes.Code != http.StatusBadRequest || client.calls != 3 {
		t.Fatalf("invalid horizon status=%d calls=%d", badRes.Code, client.calls)
	}
}

func TestJSONRequestBodyLimitReturnsPayloadTooLarge(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	body := `{"public_url":"` + strings.Repeat("a", int(maxJSONRequestBytes)) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", strings.NewReader(body))
	req.ContentLength = -1 // Exercise the streaming limit, not only the length precheck.
	req.RemoteAddr = "127.0.0.1:12345"
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413; body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "1 MiB") {
		t.Fatalf("body=%q, want documented request limit", res.Body.String())
	}
}

func TestSendSSEChecksWriteAndFlushErrors(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	writeFailure := &controlledSSEWriter{writeErr: writeErr}
	err := sendSSE(writeFailure, http.NewResponseController(writeFailure), hyperserve.SSEMessage{Event: "snapshot", Data: "test"})
	if !errors.Is(err, writeErr) {
		t.Fatalf("write error=%v, want %v", err, writeErr)
	}
	if writeFailure.flushes != 0 {
		t.Fatalf("flushes=%d after failed write, want 0", writeFailure.flushes)
	}

	flushErr := errors.New("flush failed")
	flushFailure := &controlledSSEWriter{flushErr: flushErr}
	wrapper := &legacyFlushWrapper{ResponseWriter: flushFailure}
	err = sendSSE(wrapper, checkedResponseController(wrapper), hyperserve.SSEMessage{Event: "snapshot", Data: "test"})
	if !errors.Is(err, flushErr) {
		t.Fatalf("flush error=%v, want %v", err, flushErr)
	}
	if wrapper.flushes != 0 || flushFailure.flushes != 1 || flushFailure.body.Len() == 0 {
		t.Fatalf("wrapper flushes=%d checked flushes=%d bytes=%d, want the error-capable underlying flush", wrapper.flushes, flushFailure.flushes, flushFailure.body.Len())
	}
}

// legacyFlushWrapper matches HyperServe's logging response-writer shape: it
// offers a no-error Flush for compatibility and an Unwrap path to the socket.
type legacyFlushWrapper struct {
	http.ResponseWriter
	flushes int
}

func (w *legacyFlushWrapper) Flush() {
	w.flushes++
}

func (w *legacyFlushWrapper) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type controlledSSEWriter struct {
	header   http.Header
	body     bytes.Buffer
	writeErr error
	flushErr error
	flushes  int
}

func (w *controlledSSEWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *controlledSSEWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(p)
}

func (*controlledSSEWriter) WriteHeader(int) {}

func (w *controlledSSEWriter) FlushError() error {
	w.flushes++
	return w.flushErr
}

func TestAppStatusReadyRequiresBothAlertAuthorities(t *testing.T) {
	t.Parallel()
	coverage := &AlertCoverageDTO{
		State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceDataHealth},
		CoveredSources:  []rpc.AlertSource{rpc.AlertSourceDataHealth},
	}
	ready := AppStatusDTO{
		AlertProducer:   AlertProducerStatusDTO{Initialized: true, Coverage: coverage},
		AlertDispatcher: AlertDeliveryHealthDTO{State: state.AlertDeliveryHealthHealthy},
	}
	if !AppStatusReady(ready) {
		t.Fatal("complete producer plus healthy dispatcher was not ready")
	}
	ready.AlertDispatcher.State = state.AlertDeliveryHealthUnavailable
	if AppStatusReady(ready) {
		t.Fatal("dispatcher outage was hidden by healthy producer coverage")
	}
}

func TestSettingsGetPatchRequiresAuthAndRejectsReadOnly(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	unauth := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	unauthRes := httptest.NewRecorder()
	handler.ServeHTTP(unauthRes, unauth)
	if unauthRes.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d, want 401", unauthRes.Code)
	}
	cookie := routeSessionCookie(t, handler)
	getReq := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	getReq.AddCookie(cookie)
	getRes := httptest.NewRecorder()
	handler.ServeHTTP(getRes, getReq)
	if getRes.Code != http.StatusOK {
		t.Fatalf("settings get status=%d, want 200; body=%s", getRes.Code, getRes.Body.String())
	}
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader([]byte(`{"trading":{"enabled":true}}`)))
	patchReq.AddCookie(cookie)
	patchRes := httptest.NewRecorder()
	handler.ServeHTTP(patchRes, patchReq)
	if patchRes.Code != http.StatusBadRequest {
		t.Fatalf("settings patch status=%d, want 400; body=%s", patchRes.Code, patchRes.Body.String())
	}
}

func TestUpdateRoutesRequireAuthAndPinDisplayedTarget(t *testing.T) {
	t.Parallel()
	controller := &routeUpdateController{status: UpdateStatusDTO{
		SchemaVersion: UpdateStatusSchemaVersion, State: UpdateStateAvailable,
		CurrentVersion: "v3.0.1", LatestVersion: "v3.0.2", Available: true,
	}}
	server := newTestHandlerWithDependencies(t, routeFakeClient{}, relay.Noop{PublicURL: "https://relay.example"}, func(deps *Dependencies) {
		deps.UpdateController = controller
	})
	handler := server.Handler()

	unauth := httptest.NewRequest(http.MethodGet, "/api/update", nil)
	unauthRes := httptest.NewRecorder()
	handler.ServeHTTP(unauthRes, unauth)
	if unauthRes.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d, want 401", unauthRes.Code)
	}

	cookie := routeSessionCookie(t, handler)
	getReq := httptest.NewRequest(http.MethodGet, "/api/update", nil)
	getReq.AddCookie(cookie)
	getRes := httptest.NewRecorder()
	handler.ServeHTTP(getRes, getReq)
	if getRes.Code != http.StatusOK {
		t.Fatalf("update status=%d, want 200; body=%s", getRes.Code, getRes.Body.String())
	}

	wrongReq := httptest.NewRequest(http.MethodPost, "/api/update", bytes.NewReader([]byte(`{"target_version":"v3.0.3"}`)))
	wrongReq.AddCookie(cookie)
	wrongRes := httptest.NewRecorder()
	handler.ServeHTTP(wrongRes, wrongReq)
	if wrongRes.Code != http.StatusConflict {
		t.Fatalf("wrong target status=%d, want 409; body=%s", wrongRes.Code, wrongRes.Body.String())
	}

	startReq := httptest.NewRequest(http.MethodPost, "/api/update", bytes.NewReader([]byte(`{"target_version":"v3.0.2"}`)))
	startReq.AddCookie(cookie)
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted || controller.started != "v3.0.2" {
		t.Fatalf("start status=%d target=%q, want 202/v3.0.2; body=%s", startRes.Code, controller.started, startRes.Body.String())
	}
}

type routeUpdateController struct {
	status  UpdateStatusDTO
	started string
}

func (c *routeUpdateController) Status() UpdateStatusDTO { return c.status }

func (c *routeUpdateController) Start(target string) (UpdateStatusDTO, error) {
	if target != c.status.LatestVersion {
		return c.status, fmt.Errorf("%w: target changed", ErrUpdateConflict)
	}
	c.started = target
	c.status.State = UpdateStateUpdating
	c.status.TargetVersion = target
	return c.status, nil
}

func TestOrderWritesRequireCurrentConfirmation(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClient(t, routeWriteFakeClient{}).Handler()
	cookie := routeSessionCookie(t, handler)

	for name, tc := range map[string]struct {
		method string
		path   string
		body   string
	}{
		"cancel_missing": {
			method: http.MethodPost,
			path:   "/api/orders/ord-1/cancel",
			body:   `{}`,
		},
		"modify_wrong_mode": {
			method: http.MethodPost,
			path:   "/api/orders/ord-1/modify",
			body:   `{"preview_token":"modify-token","confirm_account":"DU123","confirm_mode":"live"}`,
		},
		"proposal_submit_missing": {
			method: http.MethodPost,
			path:   "/api/proposals/submit",
			body:   `{"key":"proposal","revision":"rev-1"}`,
		},
		"request_stop_missing": {
			method: http.MethodPost,
			path:   "/api/proposals/request-stop",
			body:   `{"symbol":"SYN"}`,
		},
		"request_stop_wrong_mode": {
			method: http.MethodPost,
			path:   "/api/proposals/request-stop",
			body:   `{"symbol":"SYN","confirm_account":"DU123","confirm_mode":"live"}`,
		},
		"opportunity_exercise_missing": {
			method: http.MethodPost,
			path:   "/api/opportunities/exercise",
			body:   `{"key":"opportunity","revision":"rev-1"}`,
		},
		"strategy_submit_missing": {
			method: http.MethodPost,
			path:   "/api/strategies/submit",
			body:   `{"preview_token":"strategy-token"}`,
		},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(tc.body)))
		req.AddCookie(cookie)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400; body=%s", name, res.Code, res.Body.String())
		}
	}
}

func TestOpportunityExerciseHTTPDoesNotAuthorizeWrites(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClient(t, routeFrozenFakeClient{}).Handler()
	cookie := routeSessionCookie(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/api/opportunities/exercise", bytes.NewReader([]byte(`{"key":"opportunity","revision":"rev-1","confirm_account":"DU123","confirm_mode":"paper"}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("opportunities exercise status=%d, want daemon-level response; body=%s", res.Code, res.Body.String())
	}
	var exercise rpc.OpportunityExerciseSubmitResult
	if err := json.NewDecoder(res.Body).Decode(&exercise); err != nil {
		t.Fatalf("decode opportunities exercise: %v", err)
	}
	if exercise.Accepted || len(exercise.Blockers) == 0 {
		t.Fatalf("unexpected opportunity exercise result: %#v", exercise)
	}
}

func TestStrategyPreviewAndSubmitUseDaemonAuthority(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClient(t, routeWriteFakeClient{}).Handler()
	cookie := routeSessionCookie(t, handler)

	previewReq := httptest.NewRequest(http.MethodPost, "/api/strategies/preview", bytes.NewReader([]byte(`{
		"strategy_id":"strategy-1","expected_revision":3,"operation":"close","source":"client-claimed"
	}`)))
	previewReq.AddCookie(cookie)
	previewRes := httptest.NewRecorder()
	handler.ServeHTTP(previewRes, previewReq)
	if previewRes.Code != http.StatusOK {
		t.Fatalf("strategy preview status=%d, want 200; body=%s", previewRes.Code, previewRes.Body.String())
	}
	var preview rpc.OrderPreviewResult
	if err := json.NewDecoder(previewRes.Body).Decode(&preview); err != nil {
		t.Fatalf("decode strategy preview: %v", err)
	}
	if preview.Draft.StrategyGroup == nil || preview.Draft.StrategyGroup.StrategyID != "strategy-1" {
		t.Fatalf("strategy preview lost daemon-authored group: %#v", preview.Draft.StrategyGroup)
	}
	if preview.Draft.Source != "strategy_app" {
		t.Fatalf("strategy preview source=%q, want server-assigned strategy_app", preview.Draft.Source)
	}

	submitReq := httptest.NewRequest(http.MethodPost, "/api/strategies/submit", bytes.NewReader([]byte(`{
		"preview_token":"redacted-strategy-token","confirm_account":"DU123","confirm_mode":"paper"
	}`)))
	submitReq.AddCookie(cookie)
	submitRes := httptest.NewRecorder()
	handler.ServeHTTP(submitRes, submitReq)
	if submitRes.Code != http.StatusOK {
		t.Fatalf("strategy submit status=%d, want 200; body=%s", submitRes.Code, submitRes.Body.String())
	}
	var placed rpc.OrderPlaceResult
	if err := json.NewDecoder(submitRes.Body).Decode(&placed); err != nil {
		t.Fatalf("decode strategy submit: %v", err)
	}
	if !placed.Accepted {
		t.Fatalf("strategy submit result=%#v, want accepted fake result", placed)
	}
}

func TestOrderCancelAllowedWhileFrozen(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClient(t, routeFrozenFakeClient{}).Handler()
	cookie := routeSessionCookie(t, handler)

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/cancel", bytes.NewReader([]byte(`{"confirm_account":"DU123","confirm_mode":"paper"}`)))
	cancelReq.AddCookie(cookie)
	cancelRes := httptest.NewRecorder()
	handler.ServeHTTP(cancelRes, cancelReq)
	if cancelRes.Code != http.StatusOK {
		t.Fatalf("cancel while frozen status=%d, want 200; body=%s", cancelRes.Code, cancelRes.Body.String())
	}

	modifyBody := bytes.NewReader([]byte(`{"preview_token":"modify-token","confirm_account":"DU123","confirm_mode":"paper"}`))
	modifyReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/modify", modifyBody)
	modifyReq.AddCookie(cookie)
	modifyRes := httptest.NewRecorder()
	handler.ServeHTTP(modifyRes, modifyReq)
	if modifyRes.Code != http.StatusBadRequest {
		t.Fatalf("modify while frozen status=%d, want 400; body=%s", modifyRes.Code, modifyRes.Body.String())
	}
}

func newTestHandler(t *testing.T) *hyperserve.Server {
	t.Helper()
	return newTestHandlerWithClient(t, routeFakeClient{})
}

func newTestHandlerWithClient(t *testing.T, fakeClient daemonclient.Client) *hyperserve.Server {
	t.Helper()
	return newTestHandlerWithClientAndRelay(t, fakeClient, relay.Noop{PublicURL: "https://relay.example"})
}

func newTestHandlerWithClientAndRelay(t *testing.T, fakeClient daemonclient.Client, relayClient relay.Client) *hyperserve.Server {
	t.Helper()
	return newTestHandlerWithDependencies(t, fakeClient, relayClient, nil)
}

func newTestHandlerWithDependencies(t *testing.T, fakeClient daemonclient.Client, relayClient relay.Client, mutate func(*Dependencies)) *hyperserve.Server {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.EnsureVAPID(time.Now().UTC(), func() (string, string, error) {
		return "private", "public", nil
	}); err != nil {
		t.Fatalf("EnsureVAPID: %v", err)
	}
	alertController := &alerts.Dispatcher{Store: store, URL: "https://relay.example"}
	authMgr := auth.NewManager(store, alertController, time.Minute)
	liveSvc := live.New(fakeClient, time.Minute, time.Minute)
	liveSvc.PollOnce(t.Context())
	srv, err := hyperserve.New(
		hyperserve.WithAddr("127.0.0.1:0"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	deps := Dependencies{
		Server:          srv,
		Store:           store,
		Auth:            authMgr,
		Daemon:          fakeClient,
		Live:            liveSvc,
		Relay:           relayClient,
		PublicURL:       "https://relay.example",
		Version:         "test-version",
		AlertController: alertController,
	}
	if mutate != nil {
		mutate(&deps)
	}
	Register(deps)
	return srv
}

func routeSessionCookie(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	pairReq := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", bytes.NewReader([]byte("{}")))
	pairReq.RemoteAddr = "127.0.0.1:12345"
	pairRes := httptest.NewRecorder()
	handler.ServeHTTP(pairRes, pairReq)
	if pairRes.Code != http.StatusOK {
		t.Fatalf("pair status=%d, want 200; body=%s", pairRes.Code, pairRes.Body.String())
	}
	var pairing auth.PairingSession
	if err := json.NewDecoder(pairRes.Body).Decode(&pairing); err != nil {
		t.Fatalf("decode pairing: %v", err)
	}
	key := newRouteTestKey(t)
	completeBody, err := json.Marshal(auth.CompletePairingRequest{
		PairingID:    pairing.ID,
		Nonce:        pairing.Nonce,
		DeviceName:   "iPhone",
		PublicKeyJWK: routeTestJWK(t, key),
		Signature:    routeTestSignature(t, key, pairing.Nonce),
	})
	if err != nil {
		t.Fatalf("marshal complete body: %v", err)
	}
	completeReq := httptest.NewRequest(http.MethodPost, "/api/pairing/complete", bytes.NewReader(completeBody))
	completeRes := httptest.NewRecorder()
	handler.ServeHTTP(completeRes, completeReq)
	if completeRes.Code != http.StatusOK {
		t.Fatalf("complete status=%d, want 200; body=%s", completeRes.Code, completeRes.Body.String())
	}
	cookies := completeRes.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("pairing response did not set a session cookie")
	}
	return cookies[0]
}

type routeFakeClient struct{}

func (routeFakeClient) NudgesSnapshot(context.Context) (*rpc.NudgesSnapshotResult, error) {
	return nil, nil
}

func (routeFakeClient) Status(context.Context) (*rpc.HealthResult, error) {
	return &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497}, nil
}

func (routeFakeClient) MarketCalendar(context.Context) (*rpc.MarketCalendarResult, error) {
	return &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}}, nil
}

func (routeFakeClient) MarketCalendarFor(_ context.Context, market string) (*rpc.MarketCalendarResult, error) {
	return &rpc.MarketCalendarResult{Market: market, Label: market, Session: rpc.MarketSession{Market: market, State: "regular", IsOpen: true}}, nil
}

func (routeFakeClient) Account(context.Context) (*rpc.AccountResult, error) {
	return &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000}, nil
}

func (routeFakeClient) Positions(context.Context) (*rpc.PositionsResult, error) {
	return &rpc.PositionsResult{}, nil
}

func (routeFakeClient) Quote(_ context.Context, contract rpc.ContractParams) (*rpc.Quote, error) {
	return &rpc.Quote{Symbol: contract.Symbol, Price: new(500.0), ChangePct: new(0.4), DataType: rpc.MarketDataLive}, nil
}

func (routeFakeClient) StreamQuote(context.Context, rpc.ContractParams, func(rpc.Frame) error) error {
	return nil
}

func (routeFakeClient) MarketEvents(context.Context, rpc.MarketEventsParams) (*rpc.MarketEventsResult, error) {
	return &rpc.MarketEventsResult{Kind: rpc.MarketEventsKind, SchemaVersion: rpc.MarketEventsSchemaVersion, Fingerprint: rpc.Fingerprint{Key: "market-events-1"}}, nil
}

func (routeFakeClient) Stress(context.Context) (*rpc.StressResult, error) {
	return &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}}, nil
}

func (routeFakeClient) StressWithRegime(context.Context) (*rpc.StressResult, *rpc.RegimeMonitorResult, error) {
	return &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		&rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}},
		nil
}

func (routeFakeClient) Rules(context.Context) (*rpc.RulesResult, error) {
	return &rpc.RulesResult{Enabled: true, Status: "ok"}, nil
}

func (routeFakeClient) Brief(context.Context) (*rpc.BriefResult, error) {
	return &rpc.BriefResult{BriefFingerprint: "brief-1"}, nil
}

func (routeFakeClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	return &rpc.TradingStatus{
		Mode:       "paper",
		Account:    "DU123",
		Endpoint:   "127.0.0.1:7497",
		ClientID:   7,
		CanPreview: true,
		CanWrite:   false,
	}, nil
}

func (routeFakeClient) AutoTradeStatus(context.Context) (*rpc.AutoTradeStatus, error) {
	return &rpc.AutoTradeStatus{ProposalsEnabled: true, FastPathEnabled: true}, nil
}

func (routeFakeClient) OpportunitiesStatus(context.Context) (*rpc.OpportunityStatus, error) {
	return &rpc.OpportunityStatus{Enabled: true}, nil
}

func (routeFakeClient) OpportunitiesSnapshot(context.Context, rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error) {
	return &rpc.OpportunitySnapshot{Kind: rpc.OpportunitySnapshotKind, SchemaVersion: rpc.OpportunitySnapshotSchemaVersion, Revision: "empty", Opportunities: []rpc.Opportunity{}}, nil
}

func (routeFakeClient) OpportunitiesRefresh(context.Context, rpc.OpportunityRefreshParams) (*rpc.OpportunitySnapshot, error) {
	return routeFakeClient{}.OpportunitiesSnapshot(context.Background(), rpc.OpportunitySnapshotParams{})
}

func (routeFakeClient) OpportunitiesPreviewExercise(context.Context, rpc.OpportunityExercisePreviewParams) (*rpc.OpportunityExercisePreviewResult, error) {
	return &rpc.OpportunityExercisePreviewResult{Accepted: true, PreviewTokenID: "opprev-1"}, nil
}

func (routeFakeClient) OpportunitiesSubmitExercise(context.Context, rpc.OpportunityExerciseSubmitParams) (*rpc.OpportunityExerciseSubmitResult, error) {
	return &rpc.OpportunityExerciseSubmitResult{Accepted: false, Blockers: []rpc.TradingBlocker{{Code: "test", Message: "blocked"}}}, nil
}

func (routeFakeClient) OpportunitiesIgnore(context.Context, rpc.OpportunityIgnoreParams) (*rpc.OpportunityIgnoreResult, error) {
	return &rpc.OpportunityIgnoreResult{Accepted: true, Key: "opportunity"}, nil
}

func (routeFakeClient) TradeProposalsSnapshot(context.Context, rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error) {
	return &rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, Revision: "empty", Proposals: []rpc.TradeProposal{}}, nil
}

func (routeFakeClient) TradeProposalsRefresh(context.Context, rpc.TradeProposalRefreshParams) (*rpc.TradeProposalSnapshot, error) {
	return routeFakeClient{}.TradeProposalsSnapshot(context.Background(), rpc.TradeProposalSnapshotParams{})
}

func (routeFakeClient) TradeProposalsPreview(context.Context, rpc.TradeProposalPreviewParams) (*rpc.TradeProposalPreviewResult, error) {
	return &rpc.TradeProposalPreviewResult{Accepted: true, PreviewTokenID: "tok-1"}, nil
}

func (routeFakeClient) TradeProposalsSubmit(context.Context, rpc.TradeProposalSubmitParams) (*rpc.TradeProposalSubmitResult, error) {
	return &rpc.TradeProposalSubmitResult{Accepted: false, Blockers: []rpc.TradingBlocker{{Code: "test", Message: "blocked"}}}, nil
}

func (routeFakeClient) TradeProposalsReducePreview(context.Context, rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	return &rpc.TradeProposalReduceResult{Accepted: true, PreviewTokenID: "tok-reduce"}, nil
}

func (routeFakeClient) TradeProposalsReduceSubmit(context.Context, rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	return &rpc.TradeProposalReduceResult{Accepted: false, Blockers: []rpc.TradingBlocker{{Code: "test", Message: "blocked"}}}, nil
}

func (routeFakeClient) TradeProposalsReducePortfolioPreview(context.Context, rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	return &rpc.TradeProposalReducePortfolioResult{Accepted: true, LegCount: 1}, nil
}

func (routeFakeClient) TradeProposalsReducePortfolioSubmit(context.Context, rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	return &rpc.TradeProposalReducePortfolioResult{Accepted: false, Blockers: []rpc.TradingBlocker{{Code: "test", Message: "blocked"}}}, nil
}

func (routeFakeClient) TradeProposalsRequestStop(context.Context, rpc.TradeProposalRequestStopParams) (*rpc.TradeProposalRequestStopResult, error) {
	return &rpc.TradeProposalRequestStopResult{Accepted: true, Symbol: "SYN", ProposalKey: "trailing_stop:abc", Revision: "sha256:rev"}, nil
}

func (routeFakeClient) TradeProposalsIgnore(context.Context, rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error) {
	return &rpc.TradeProposalIgnoreResult{Accepted: true, Key: "proposal"}, nil
}

func (routeFakeClient) Settings(context.Context) (*rpc.PlatformSettings, error) {
	return &rpc.PlatformSettings{
		Kind: "ibkr.platform_settings",
		Trading: rpc.PlatformTradingSettings{
			Mode: rpc.SettingsString{Value: "paper", Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceConfig},
			Limits: rpc.TradingLimitSettings{
				MaxNotional: rpc.SettingsFloat{Value: 10000, Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceConfig, Reason: "stable build"},
			},
		},
		MarketData: rpc.PlatformMarketDataSetting{
			Quality: rpc.PlatformMarketDataQuality{Status: "ok", Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceObserved},
		},
	}, nil
}

func (routeFakeClient) UpdateSettings(_ context.Context, patch json.RawMessage) (*rpc.PlatformSettings, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(patch, &obj); err != nil {
		return nil, err
	}
	if _, ok := obj["trading"]; ok {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "settings field trading.mode is read-only"}
	}
	return routeFakeClient{}.Settings(context.Background())
}

func (routeFakeClient) OrderPreview(_ context.Context, params rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	limit := 0.0
	if params.LimitPrice != nil {
		limit = *params.LimitPrice
	}
	return &rpc.OrderPreviewResult{
		PreviewToken:          "redacted-test-token",
		PreviewTokenID:        "tok-1",
		PreviewTokenExpiresAt: time.Now().UTC().Add(time.Minute),
		TokenMinted:           true,
		SubmitEligible:        true,
		Executable:            true,
		Mode:                  "paper",
		Account:               "DU123",
		Endpoint:              "127.0.0.1:7497",
		ClientID:              7,
		Draft: rpc.OrderDraft{
			Action:     params.Action,
			Contract:   params.Contract,
			Quantity:   params.Quantity,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: limit,
			TIF:        rpc.OrderTIFDay,
			Strategy:   params.Strategy,
			OrderRef:   "ord-1",
		},
		WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true},
		AsOf:   time.Now().UTC(),
	}, nil
}

func (routeFakeClient) StrategyPreview(_ context.Context, params rpc.StrategyPreviewParams) (*rpc.OrderPreviewResult, error) {
	return &rpc.OrderPreviewResult{
		PreviewToken:          "redacted-strategy-token",
		PreviewTokenID:        "strategy-token-1",
		PreviewTokenScope:     rpc.OrderTokenScopeStrategy,
		PreviewTokenExpiresAt: time.Now().UTC().Add(time.Minute),
		TokenMinted:           true,
		SubmitEligible:        true,
		Executable:            true,
		Mode:                  "paper",
		Account:               "DU123",
		Draft: rpc.OrderDraft{
			Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, LimitPrice: 1.25,
			TIF: rpc.OrderTIFDay, Strategy: "group-close", OrderRef: "strategy-order-1", Source: params.Source,
			StrategyGroup: &rpc.StrategyOrderDraft{
				StrategyID: params.StrategyID, StrategyRevision: params.ExpectedRevision,
				Operation: params.Operation, Units: 1, UnitsBefore: 1, UnitsAfter: 0,
				GuaranteedCombo: true,
			},
		},
		WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true},
		AsOf:   time.Now().UTC(),
	}, nil
}

func (routeFakeClient) OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return nil, nil
}

func (routeFakeClient) OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	return nil, nil
}

func (routeFakeClient) OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	return nil, nil
}

func (routeFakeClient) OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	return &rpc.OrdersOpenResult{Orders: []rpc.OrderView{routeOpenOrderView()}}, nil
}

func (routeFakeClient) OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	return &rpc.OrderStatusResult{Found: true, Order: routeOpenOrderView(), AsOf: time.Now().UTC()}, nil
}

type routeWriteFakeClient struct {
	routeFakeClient
}

type routeFrozenFakeClient struct {
	routeWriteFakeClient
}

func (routeFrozenFakeClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	return &rpc.TradingStatus{
		Mode:       "paper",
		Account:    "DU123",
		Endpoint:   "127.0.0.1:7497",
		ClientID:   7,
		CanPreview: true,
		CanWrite:   false,
		WriteBlockers: []rpc.TradingBlocker{{
			Code:    "trading_frozen",
			Message: "trading writes are frozen by runtime platform settings",
		}},
	}, nil
}

func (routeWriteFakeClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	return &rpc.TradingStatus{
		Mode:       "paper",
		Account:    "DU123",
		Endpoint:   "127.0.0.1:7497",
		ClientID:   7,
		CanPreview: true,
		CanWrite:   true,
	}, nil
}

func (routeWriteFakeClient) OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return &rpc.OrderPlaceResult{
		Accepted:        true,
		Mode:            "paper",
		Account:         "DU123",
		Endpoint:        "127.0.0.1:7497",
		ClientID:        7,
		OrderRef:        "ord-1",
		PreviewTokenID:  "tok-1",
		ReservedOrderID: 42,
		Draft: rpc.OrderDraft{
			Action:     rpc.OrderActionSell,
			Contract:   rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:   3,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: 450.25,
			TIF:        rpc.OrderTIFDay,
			Strategy:   rpc.OrderStrategyPatientLimit,
			OrderRef:   "ord-1",
		},
		Status:          "submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       "sent",
		AsOf:            time.Now().UTC(),
	}, nil
}

func (routeWriteFakeClient) OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	return &rpc.OrderModifyResult{
		Accepted:        true,
		Mode:            "paper",
		Account:         "DU123",
		Endpoint:        "127.0.0.1:7497",
		ClientID:        7,
		OrderRef:        "ord-1",
		PreviewTokenID:  "modify-token",
		ReservedOrderID: 42,
		Draft: rpc.OrderDraft{
			Action:     rpc.OrderActionSell,
			Contract:   rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:   1,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: 449.50,
			TIF:        rpc.OrderTIFDay,
			Strategy:   rpc.OrderStrategyExplicitLimit,
			OrderRef:   "ord-1",
		},
		Status:          "submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       "sent",
		AsOf:            time.Now().UTC(),
	}, nil
}

func (routeWriteFakeClient) OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	return &rpc.OrderCancelResult{
		Accepted: true,
		Order: rpc.OrderView{
			OrderRef:        "ord-1",
			Symbol:          "SPY",
			Status:          "cancelled",
			LifecycleStatus: rpc.OrderLifecycleCancelled,
			SendState:       "sent",
			UpdatedAt:       time.Now().UTC(),
		},
		Status:          "cancelled",
		LifecycleStatus: rpc.OrderLifecycleCancelled,
		SendState:       "sent",
		AsOf:            time.Now().UTC(),
	}, nil
}

func newRouteTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func routeTestJWK(t *testing.T, key *ecdsa.PrivateKey) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(routeLeftPad32(key.X)),
		Y:   base64.RawURLEncoding.EncodeToString(routeLeftPad32(key.Y)),
	})
	if err != nil {
		t.Fatalf("marshal jwk: %v", err)
	}
	return raw
}

func routeTestSignature(t *testing.T, key *ecdsa.PrivateKey, message string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(message))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

func routeLeftPad32(v *big.Int) []byte {
	b := v.Bytes()
	if len(b) >= 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}
