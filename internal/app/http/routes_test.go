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
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hyperserve "github.com/osauer/hyperserve/pkg/server"

	"github.com/osauer/canary/v2/internal/app/alerts"
	"github.com/osauer/canary/v2/internal/app/auth"
	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/app/push"
	"github.com/osauer/canary/v2/internal/app/relay"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
	appweb "github.com/osauer/canary/v2/web/app"
)

func TestEmbeddedJavaScriptRoutes(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
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
		req := httptest.NewRequest(http.MethodGet, "/"+entry.Name(), nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(readerFromRecorder{res}, req)
		if res.Code != http.StatusOK {
			t.Errorf("GET /%s status=%d, want 200; body=%s", entry.Name(), res.Code, res.Body.String())
		}
		if got := res.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
			t.Errorf("GET /%s Content-Type=%q, want text/javascript; charset=utf-8", entry.Name(), got)
		}
		if got := res.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET /%s Cache-Control=%q, want no-cache", entry.Name(), got)
		}
	}
	if jsCount == 0 {
		t.Fatal("embedded app contains no JavaScript files")
	}

	req := httptest.NewRequest(http.MethodGet, "/not-embedded.js", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(readerFromRecorder{res}, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("GET /not-embedded.js status=%d, want 404", res.Code)
	}
}

// readerFromRecorder avoids the recursive io.Copy fallback in HyperServe's
// logging response writer when its underlying test recorder lacks ReaderFrom.
type readerFromRecorder struct {
	*httptest.ResponseRecorder
}

func (r readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(r.ResponseRecorder, src)
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

func TestAppStatusIsLocalAndSeparatesProducerFromDispatcher(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()

	remoteReq := httptest.NewRequest(http.MethodGet, AppStatusPath, nil)
	remoteReq.RemoteAddr = "203.0.113.9:4242"
	remoteRes := httptest.NewRecorder()
	handler.ServeHTTP(remoteRes, remoteReq)
	if remoteRes.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d, want 403", remoteRes.Code)
	}

	localReq := httptest.NewRequest(http.MethodGet, AppStatusPath, nil)
	localReq.RemoteAddr = "127.0.0.1:4242"
	localRes := httptest.NewRecorder()
	handler.ServeHTTP(localRes, localReq)
	if localRes.Code != http.StatusOK {
		t.Fatalf("local status=%d body=%s", localRes.Code, localRes.Body.String())
	}
	var got AppStatusDTO
	if err := json.Unmarshal(localRes.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode app status: %v", err)
	}
	if got.SchemaVersion != AppStatusSchemaVersion || got.Version != "test-version" {
		t.Fatalf("identity=%+v", got)
	}
	if got.AlertProducer.Sources == nil || got.AlertDispatcher.State == "" {
		t.Fatalf("producer/dispatcher projections are not independently present: %+v", got)
	}
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

func TestSafeDiagnosticUsesAuthenticatedDeviceFixedCopyAndHonorsNone(t *testing.T) {
	t.Parallel()
	srv, store, sender := newGovernanceTestHandler(t, routeFakeClient{})
	handler := srv.Handler()
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/push/test", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", unauth.Code)
	}
	cookie := routeSessionCookie(t, handler)
	devices := store.Devices()
	if len(devices) != 1 {
		t.Fatalf("devices=%+v", devices)
	}
	if err := store.AddPushSubscription(state.PushSubscription{ID: "diagnostic-sub", DeviceID: devices[0].ID, Endpoint: "https://push.example/diagnostic", P256DH: "key", Auth: "auth", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	rejectedReq := httptest.NewRequest(http.MethodPost, "/api/push/test", strings.NewReader(`{"title":"arbitrary","destination":"https://evil.example"}`))
	rejectedReq.AddCookie(cookie)
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, rejectedReq)
	if rejected.Code != http.StatusBadRequest || len(sender.payloads) != 0 {
		t.Fatalf("arbitrary diagnostic status=%d sends=%d", rejected.Code, len(sender.payloads))
	}

	if err := store.SetAlertMode(state.AlertModeNone); err != nil {
		t.Fatal(err)
	}
	noneReq := httptest.NewRequest(http.MethodPost, "/api/push/test", nil)
	noneReq.AddCookie(cookie)
	noneRes := httptest.NewRecorder()
	handler.ServeHTTP(noneRes, noneReq)
	if noneRes.Code != http.StatusOK || len(sender.payloads) != 0 || !strings.Contains(noneRes.Body.String(), `"state":"suppressed"`) {
		t.Fatalf("none diagnostic status=%d sends=%d body=%s", noneRes.Code, len(sender.payloads), noneRes.Body.String())
	}
	if err := store.SetAlertMode(state.AlertModeWatchAndAct); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/push/test", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || len(sender.payloads) != 1 {
		t.Fatalf("paired diagnostic status=%d sends=%d body=%s", res.Code, len(sender.payloads), res.Body.String())
	}
	want := push.SafeDiagnosticPayload()
	if sender.payloads[0] != want {
		t.Fatalf("diagnostic payload=%+v, want fixed=%+v", sender.payloads[0], want)
	}
	if diagnostic := store.GovernanceDiagnostic(); diagnostic.State != state.GovernanceTransportAccepted {
		t.Fatalf("diagnostic state=%+v", diagnostic)
	}
	if delivery := store.AlertDelivery(time.Now()); len(delivery.Occurrences) != 0 {
		t.Fatalf("diagnostic contaminated delivery evidence: %+v", delivery)
	}
}

func TestPairingBootstrap(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
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

	bootReq := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootReq.AddCookie(cookies[0])
	bootRes := httptest.NewRecorder()
	handler.ServeHTTP(bootRes, bootReq)
	if bootRes.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d, want 200; body=%s", bootRes.Code, bootRes.Body.String())
	}
	var boot map[string]any
	if err := json.NewDecoder(bootRes.Body).Decode(&boot); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if boot["version"] != "test-version" {
		t.Fatalf("version=%v, want test-version", boot["version"])
	}
	if boot["settings"] == nil {
		t.Fatalf("bootstrap missing settings: %#v", boot)
	}
	snapshot, ok := boot["snapshot"].(map[string]any)
	if !ok || snapshot["market_calendar"] == nil {
		t.Fatalf("bootstrap snapshot missing market_calendar: %#v", boot["snapshot"])
	}
}

// The iOS home-screen web app inherits Safari's cookies but not its
// localStorage/IndexedDB, and sessions die with every app restart. The
// long-lived device cookie must therefore mint a fresh session on its own,
// with no script-storage credential and no prior session.
func TestDeviceCookieMintsSessionAfterRestart(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	pairReq := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", bytes.NewReader([]byte("{}")))
	pairReq.RemoteAddr = "127.0.0.1:12345"
	pairRes := httptest.NewRecorder()
	handler.ServeHTTP(pairRes, pairReq)
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
		t.Fatalf("complete status=%d: %s", completeRes.Code, completeRes.Body.String())
	}
	var deviceCookie *http.Cookie
	for _, c := range completeRes.Result().Cookies() {
		if c.Name == deviceCookieName {
			deviceCookie = c
		}
	}
	if deviceCookie == nil {
		t.Fatalf("pairing did not set the device cookie; cookies=%v", completeRes.Result().Cookies())
	}
	if deviceCookie.MaxAge < 300*24*60*60 {
		t.Fatalf("device cookie Max-Age=%d, want long-lived", deviceCookie.MaxAge)
	}
	if !deviceCookie.HttpOnly {
		t.Fatalf("device cookie must be HttpOnly")
	}

	// Simulate the restarted app + home-screen container: no session
	// cookie, no bearer token — only the device cookie survives.
	bootReq := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootReq.AddCookie(deviceCookie)
	bootRes := httptest.NewRecorder()
	handler.ServeHTTP(bootRes, bootReq)
	if bootRes.Code != http.StatusOK {
		t.Fatalf("bootstrap via device cookie status=%d: %s", bootRes.Code, bootRes.Body.String())
	}
	gotSession, gotDevice := false, false
	for _, c := range bootRes.Result().Cookies() {
		switch c.Name {
		case "ibkr_app_session":
			gotSession = c.Value != ""
		case deviceCookieName:
			// The value must not rotate: Safari and the installed app hold
			// twin copies of the same cookie jar snapshot.
			gotDevice = c.Value == deviceCookie.Value
		}
	}
	if !gotSession || !gotDevice {
		t.Fatalf("device-cookie login must set a fresh session and re-set the same device cookie (session=%v device=%v)", gotSession, gotDevice)
	}

	// A tampered device cookie must stay locked out.
	badReq := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	badReq.AddCookie(&http.Cookie{Name: deviceCookieName, Value: deviceCookie.Value + "x"})
	badRes := httptest.NewRecorder()
	handler.ServeHTTP(badRes, badReq)
	if badRes.Code != http.StatusUnauthorized {
		t.Fatalf("tampered device cookie status=%d, want 401", badRes.Code)
	}

	// A key login re-provisions a fresh device cookie for the twin that
	// lost its jar — and the OTHER twin's older cookie must stay valid.
	var paired auth.CompletePairingResult
	if err := json.NewDecoder(bytes.NewReader(completeRes.Body.Bytes())).Decode(&paired); err != nil {
		t.Fatalf("decode pairing result: %v", err)
	}
	chBody, _ := json.Marshal(map[string]string{"device_id": paired.DeviceID})
	chReq := httptest.NewRequest(http.MethodPost, "/api/auth/challenge", bytes.NewReader(chBody))
	chRes := httptest.NewRecorder()
	handler.ServeHTTP(chRes, chReq)
	if chRes.Code != http.StatusOK {
		t.Fatalf("challenge status=%d: %s", chRes.Code, chRes.Body.String())
	}
	var ch auth.Challenge
	if err := json.NewDecoder(chRes.Body).Decode(&ch); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	sessBody, _ := json.Marshal(map[string]string{
		"device_id": paired.DeviceID,
		"challenge": ch.Challenge,
		"signature": routeTestSignature(t, key, ch.Challenge),
	})
	sessReq := httptest.NewRequest(http.MethodPost, "/api/auth/session", bytes.NewReader(sessBody))
	sessRes := httptest.NewRecorder()
	handler.ServeHTTP(sessRes, sessReq)
	if sessRes.Code != http.StatusOK {
		t.Fatalf("session status=%d: %s", sessRes.Code, sessRes.Body.String())
	}
	var reissued *http.Cookie
	for _, c := range sessRes.Result().Cookies() {
		if c.Name == deviceCookieName {
			reissued = c
		}
	}
	if reissued == nil || reissued.Value == deviceCookie.Value {
		t.Fatalf("key login must re-provision a fresh device cookie (got %v)", reissued)
	}
	twinReq := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	twinReq.AddCookie(deviceCookie)
	twinRes := httptest.NewRecorder()
	handler.ServeHTTP(twinRes, twinReq)
	if twinRes.Code != http.StatusOK {
		t.Fatalf("older twin cookie rejected after re-provisioning: status=%d", twinRes.Code)
	}
}

func TestDeviceManagementIsLocalMacOnly(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	listReq := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	listReq.RemoteAddr = "192.0.2.10:44321"
	listRes := httptest.NewRecorder()
	handler.ServeHTTP(listRes, listReq)
	if listRes.Code != http.StatusForbidden {
		t.Fatalf("remote devices list status=%d, want 403", listRes.Code)
	}
	pruneReq := httptest.NewRequest(http.MethodPost, "/api/devices/prune", bytes.NewReader([]byte(`{"keep_days":7}`)))
	pruneReq.RemoteAddr = "192.0.2.10:44321"
	pruneRes := httptest.NewRecorder()
	handler.ServeHTTP(pruneRes, pruneReq)
	if pruneRes.Code != http.StatusForbidden {
		t.Fatalf("remote devices prune status=%d, want 403", pruneRes.Code)
	}

	localList := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	localList.RemoteAddr = "127.0.0.1:44321"
	localListRes := httptest.NewRecorder()
	handler.ServeHTTP(localListRes, localList)
	if localListRes.Code != http.StatusOK {
		t.Fatalf("local devices list status=%d: %s", localListRes.Code, localListRes.Body.String())
	}
	localPrune := httptest.NewRequest(http.MethodPost, "/api/devices/prune", bytes.NewReader([]byte(`{"keep_days":0}`)))
	localPrune.RemoteAddr = "127.0.0.1:44321"
	localPruneRes := httptest.NewRecorder()
	handler.ServeHTTP(localPruneRes, localPrune)
	if localPruneRes.Code != http.StatusBadRequest {
		t.Fatalf("keep_days=0 status=%d, want 400 (a zero-day prune would delete every device)", localPruneRes.Code)
	}
}

func TestPairingSessionUsesRelayURLWithoutExplicitOverride(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClientAndRelay(t, routeFakeClient{}, routeTestRelay{route: "r_route"}).Handler()

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
	if !strings.Contains(pairing.URL, "remote=r_route") {
		t.Fatalf("pairing URL = %q, want relay route", pairing.URL)
	}

	explicitReq := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", bytes.NewReader([]byte(`{"public_url":"http://127.0.0.1:8765"}`)))
	explicitReq.RemoteAddr = "127.0.0.1:12345"
	explicitRes := httptest.NewRecorder()
	handler.ServeHTTP(explicitRes, explicitReq)
	if explicitRes.Code != http.StatusOK {
		t.Fatalf("explicit pair status=%d, want 200; body=%s", explicitRes.Code, explicitRes.Body.String())
	}
	if err := json.NewDecoder(explicitRes.Body).Decode(&pairing); err != nil {
		t.Fatalf("decode explicit pairing: %v", err)
	}
	if strings.Contains(pairing.URL, "remote=r_route") {
		t.Fatalf("explicit pairing URL = %q, want no relay rewrite", pairing.URL)
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

func TestPatchSettingsReplacesClientClaimedOrigin(t *testing.T) {
	t.Parallel()
	client := &routeSettingsPatchCaptureClient{}
	handler := newTestHandlerWithClient(t, client).Handler()
	cookie := routeSessionCookie(t, handler)
	req := httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader([]byte(`{
		"origin":"human-tty",
		"features":{"stock_protection":{"enabled":false}}
	}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", res.Code, res.Body.String())
	}
	if client.calls != 1 {
		t.Fatalf("daemon update calls=%d, want 1", client.calls)
	}
	var forwarded map[string]json.RawMessage
	if err := json.Unmarshal(client.patch, &forwarded); err != nil {
		t.Fatalf("decode forwarded patch: %v", err)
	}
	var origin string
	if err := json.Unmarshal(forwarded["origin"], &origin); err != nil {
		t.Fatalf("decode forwarded origin: %v", err)
	}
	if origin != rpc.OrderOriginPairedDevice {
		t.Fatalf("forwarded origin=%q, want %q", origin, rpc.OrderOriginPairedDevice)
	}
	if got := string(forwarded["features"]); got != `{"stock_protection":{"enabled":false}}` {
		t.Fatalf("forwarded features=%s, want unchanged feature patch", got)
	}
}

func TestPatchSettingsRejectsTradingBeforeDaemonCall(t *testing.T) {
	t.Parallel()
	client := &routeSettingsPatchCaptureClient{}
	handler := newTestHandlerWithClient(t, client).Handler()
	cookie := routeSessionCookie(t, handler)
	req := httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader([]byte(`{"trading":{"freeze":false}}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "trading settings are not writable from the app; use the CLI") {
		t.Fatalf("body=%q, want app trading rejection", res.Body.String())
	}
	if client.calls != 0 {
		t.Fatalf("daemon update calls=%d, want 0", client.calls)
	}
}

func TestPatchSettingsForwardsFeatureToggleWithPairedDeviceOrigin(t *testing.T) {
	t.Parallel()
	client := &routeSettingsPatchCaptureClient{}
	handler := newTestHandlerWithClient(t, client).Handler()
	cookie := routeSessionCookie(t, handler)
	req := httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader([]byte(`{"features":{"stock_protection":{"enabled":false}}}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", res.Code, res.Body.String())
	}
	if client.calls != 1 {
		t.Fatalf("daemon update calls=%d, want 1", client.calls)
	}
	var forwarded struct {
		Origin   string          `json:"origin"`
		Features json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(client.patch, &forwarded); err != nil {
		t.Fatalf("decode forwarded patch: %v", err)
	}
	if forwarded.Origin != rpc.OrderOriginPairedDevice {
		t.Fatalf("forwarded origin=%q, want %q", forwarded.Origin, rpc.OrderOriginPairedDevice)
	}
	if got := string(forwarded.Features); got != `{"stock_protection":{"enabled":false}}` {
		t.Fatalf("forwarded features=%s, want unchanged feature patch", got)
	}
}

func TestSettingsRoutesKeepDaemonMarketDataQualityAuthority(t *testing.T) {
	t.Parallel()
	liveSvc := live.New(routeSettingsStatusClient{status: "live-snapshot"}, time.Minute, time.Minute)
	liveSvc.PollOnce(t.Context())
	h := &handler{deps: Dependencies{
		Daemon: routeSettingsStatusClient{status: "daemon-authority"},
		Live:   liveSvc,
	}}
	assertDaemonStatus := func(t *testing.T, label string, settings *rpc.PlatformSettings) {
		t.Helper()
		if settings == nil {
			t.Fatalf("%s settings missing", label)
		}
		if got := settings.MarketData.Quality.Status; got != "daemon-authority" {
			t.Fatalf("%s market-data quality status = %q, want daemon-authority", label, got)
		}
	}

	assertDaemonStatus(t, "snapshot", h.settingsSnapshot(t.Context()))

	getReq := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	getRes := httptest.NewRecorder()
	h.handleGetSettings(getRes, getReq)
	if getRes.Code != http.StatusOK {
		t.Fatalf("settings get status=%d, want 200; body=%s", getRes.Code, getRes.Body.String())
	}
	var got rpc.PlatformSettings
	if err := json.NewDecoder(getRes.Body).Decode(&got); err != nil {
		t.Fatalf("decode get settings: %v", err)
	}
	assertDaemonStatus(t, "get", &got)

	patchReq := httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader([]byte(`{}`)))
	patchRes := httptest.NewRecorder()
	h.handlePatchSettings(patchRes, patchReq)
	if patchRes.Code != http.StatusOK {
		t.Fatalf("settings patch status=%d, want 200; body=%s", patchRes.Code, patchRes.Body.String())
	}
	got = rpc.PlatformSettings{}
	if err := json.NewDecoder(patchRes.Body).Decode(&got); err != nil {
		t.Fatalf("decode patch settings: %v", err)
	}
	assertDaemonStatus(t, "patch", &got)
}

func TestOrdersOpenHTTPAdapter(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	cookie := routeSessionCookie(t, handler)

	openReq := httptest.NewRequest(http.MethodGet, "/api/orders/open", nil)
	openReq.AddCookie(cookie)
	openRes := httptest.NewRecorder()
	handler.ServeHTTP(openRes, openReq)
	if openRes.Code != http.StatusOK {
		t.Fatalf("orders open status=%d, want 200; body=%s", openRes.Code, openRes.Body.String())
	}
	var open rpc.OrdersOpenResult
	if err := json.NewDecoder(openRes.Body).Decode(&open); err != nil {
		t.Fatalf("decode orders open: %v", err)
	}
	if len(open.Orders) != 1 || open.Orders[0].OrderRef != "ord-1" {
		t.Fatalf("unexpected open orders: %#v", open.Orders)
	}
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
		"opportunity_exercise_missing": {
			method: http.MethodPost,
			path:   "/api/opportunities/exercise",
			body:   `{"key":"opportunity","revision":"rev-1"}`,
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

func TestOpportunityHTTPAdapters(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClient(t, routeWriteFakeClient{}).Handler()
	cookie := routeSessionCookie(t, handler)

	snapshotReq := httptest.NewRequest(http.MethodGet, "/api/opportunities", nil)
	snapshotReq.AddCookie(cookie)
	snapshotRes := httptest.NewRecorder()
	handler.ServeHTTP(snapshotRes, snapshotReq)
	if snapshotRes.Code != http.StatusOK {
		t.Fatalf("opportunities snapshot status=%d, want 200; body=%s", snapshotRes.Code, snapshotRes.Body.String())
	}
	var snapshot rpc.OpportunitySnapshot
	if err := json.NewDecoder(snapshotRes.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode opportunities snapshot: %v", err)
	}
	if snapshot.Kind != rpc.OpportunitySnapshotKind {
		t.Fatalf("snapshot kind=%q, want %q", snapshot.Kind, rpc.OpportunitySnapshotKind)
	}

	previewReq := httptest.NewRequest(http.MethodPost, "/api/opportunities/preview-exercise", bytes.NewReader([]byte(`{"key":"opportunity","revision":"rev-1"}`)))
	previewReq.AddCookie(cookie)
	previewRes := httptest.NewRecorder()
	handler.ServeHTTP(previewRes, previewReq)
	if previewRes.Code != http.StatusOK {
		t.Fatalf("opportunities preview status=%d, want 200; body=%s", previewRes.Code, previewRes.Body.String())
	}
	var preview rpc.OpportunityExercisePreviewResult
	if err := json.NewDecoder(previewRes.Body).Decode(&preview); err != nil {
		t.Fatalf("decode opportunities preview: %v", err)
	}
	if !preview.Accepted || preview.PreviewTokenID == "" {
		t.Fatalf("unexpected opportunity preview: %#v", preview)
	}

	exerciseReq := httptest.NewRequest(http.MethodPost, "/api/opportunities/exercise", bytes.NewReader([]byte(`{"key":"opportunity","revision":"rev-1","confirm_account":"DU123","confirm_mode":"paper"}`)))
	exerciseReq.AddCookie(cookie)
	exerciseRes := httptest.NewRecorder()
	handler.ServeHTTP(exerciseRes, exerciseReq)
	if exerciseRes.Code != http.StatusOK {
		t.Fatalf("opportunities exercise status=%d, want 200; body=%s", exerciseRes.Code, exerciseRes.Body.String())
	}
	var exercise rpc.OpportunityExerciseSubmitResult
	if err := json.NewDecoder(exerciseRes.Body).Decode(&exercise); err != nil {
		t.Fatalf("decode opportunities exercise: %v", err)
	}
	if exercise.Accepted || len(exercise.Blockers) == 0 {
		t.Fatalf("unexpected opportunity exercise result: %#v", exercise)
	}

	ignoreReq := httptest.NewRequest(http.MethodPost, "/api/opportunities/ignore", bytes.NewReader([]byte(`{"key":"opportunity","revision":"rev-1"}`)))
	ignoreReq.AddCookie(cookie)
	ignoreRes := httptest.NewRecorder()
	handler.ServeHTTP(ignoreRes, ignoreReq)
	if ignoreRes.Code != http.StatusOK {
		t.Fatalf("opportunities ignore status=%d, want 200; body=%s", ignoreRes.Code, ignoreRes.Body.String())
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

func TestOrderWriteHTTPAdapters(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClient(t, routeWriteFakeClient{}).Handler()
	cookie := routeSessionCookie(t, handler)

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/cancel", bytes.NewReader([]byte(`{"confirm_account":"DU123","confirm_mode":"paper"}`)))
	cancelReq.AddCookie(cookie)
	cancelRes := httptest.NewRecorder()
	handler.ServeHTTP(cancelRes, cancelReq)
	if cancelRes.Code != http.StatusOK {
		t.Fatalf("cancel status=%d, want 200; body=%s", cancelRes.Code, cancelRes.Body.String())
	}

	modPreviewBody := bytes.NewReader([]byte(`{"action":"SELL","quantity":1,"limit_price":449.5,"tif":"DAY"}`))
	modPreviewReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/preview-modify", modPreviewBody)
	modPreviewReq.AddCookie(cookie)
	modPreviewRes := httptest.NewRecorder()
	handler.ServeHTTP(modPreviewRes, modPreviewReq)
	if modPreviewRes.Code != http.StatusOK {
		t.Fatalf("preview-modify status=%d, want 200; body=%s", modPreviewRes.Code, modPreviewRes.Body.String())
	}
	var modPreview rpc.OrderPreviewResult
	if err := json.NewDecoder(modPreviewRes.Body).Decode(&modPreview); err != nil {
		t.Fatalf("decode preview-modify: %v", err)
	}
	if modPreview.Draft.Quantity != 1 || modPreview.Draft.OrderRef == "" {
		t.Fatalf("unexpected modify preview draft: %#v", modPreview.Draft)
	}

	modifyBody := bytes.NewReader([]byte(`{"preview_token":"modify-token","confirm_account":"DU123","confirm_mode":"paper"}`))
	modifyReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/modify", modifyBody)
	modifyReq.AddCookie(cookie)
	modifyRes := httptest.NewRecorder()
	handler.ServeHTTP(modifyRes, modifyReq)
	if modifyRes.Code != http.StatusOK {
		t.Fatalf("modify status=%d, want 200; body=%s", modifyRes.Code, modifyRes.Body.String())
	}
}

func TestOrderPreviewModifyUsesOrderViewContract(t *testing.T) {
	t.Parallel()
	client := &routeModifyPreviewContractClient{order: routeModifyPreviewOrderView()}
	handler := newTestHandlerWithClient(t, client).Handler()
	cookie := routeSessionCookie(t, handler)

	body := bytes.NewReader([]byte(`{
		"action":"SELL",
		"contract":{
			"symbol":"MSFT",
			"sec_type":"STK",
			"exchange":"NYSE",
			"primary_exchange":"NASDAQ",
			"currency":"USD",
			"local_symbol":"MSFT",
			"trading_class":"NMS"
		},
		"quantity":1,
		"limit_price":151.25,
		"tif":"DAY"
	}`))
	req := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/preview-modify", body)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("preview-modify status=%d, want 200; body=%s", res.Code, res.Body.String())
	}
	want := rpc.ContractParams{
		ConID:        29622935,
		Symbol:       "SAP",
		SecType:      "STK",
		Exchange:     "SMART",
		PrimaryExch:  "IBIS",
		Currency:     "EUR",
		LocalSymbol:  "SAP",
		TradingClass: "SAP",
		Multiplier:   1,
	}
	if got := client.previewParams.Contract; got != want {
		t.Fatalf("preview modify contract = %#v, want current order contract %#v", got, want)
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

func TestProposalRoutesRejectUnknownFields(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClient(t, routeWriteFakeClient{}).Handler()
	cookie := routeSessionCookie(t, handler)

	for name, tc := range map[string]struct{ path, body string }{
		"submit_live_confirmation":     {path: "/api/proposals/submit", body: `{"key":"p","revision":"r","confirm_account":"DU123","confirm_mode":"paper","live_confirmation":"live/DU123"}`},
		"preview_unknown":              {path: "/api/proposals/preview", body: `{"key":"p","revision":"r","bogus":true}`},
		"ignore_unknown":               {path: "/api/proposals/ignore", body: `{"key":"p","revision":"r","bogus":true}`},
		"opportunity_preview_unknown":  {path: "/api/opportunities/preview-exercise", body: `{"key":"p","revision":"r","bogus":true}`},
		"opportunity_exercise_unknown": {path: "/api/opportunities/exercise", body: `{"key":"p","revision":"r","confirm_account":"DU123","confirm_mode":"paper","bogus":true}`},
		"opportunity_ignore_unknown":   {path: "/api/opportunities/ignore", body: `{"key":"p","revision":"r","bogus":true}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(tc.body)))
		req.AddCookie(cookie)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400; body=%s", name, res.Code, res.Body.String())
		}
		if !strings.Contains(res.Body.String(), "unknown field") {
			t.Fatalf("%s error should name the unknown field; body=%s", name, res.Body.String())
		}
	}
}

func TestPairingSessionAcceptsLocalPublicURLOverride(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t).Handler()
	body := bytes.NewReader([]byte(`{"public_url":"http://192.168.1.42:8765"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", body)
	req.RemoteAddr = "127.0.0.1:12345"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", res.Code, res.Body.String())
	}
	var pairing auth.PairingSession
	if err := json.NewDecoder(res.Body).Decode(&pairing); err != nil {
		t.Fatalf("decode pairing: %v", err)
	}
	if !strings.HasPrefix(pairing.URL, "http://192.168.1.42:8765/pair.html?") {
		t.Fatalf("pairing URL = %q, want LAN public URL", pairing.URL)
	}
}

func TestPairingSessionRejectsInvalidPublicURLOverride(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t).Handler()
	body := bytes.NewReader([]byte(`{"public_url":"ftp://192.168.1.42:8765"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", body)
	req.RemoteAddr = "127.0.0.1:12345"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", res.Code, res.Body.String())
	}
}

func TestPairingSessionStillRequiresLocalMac(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t).Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", bytes.NewReader([]byte(`{"public_url":"http://192.168.1.42:8765"}`)))
	req.RemoteAddr = "203.0.113.99:12345"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", res.Code, res.Body.String())
	}
}

func TestIsLocalMacAcceptsOwnInterfaceAddress(t *testing.T) {
	t.Parallel()

	got := isLocalMacWithAddrs("192.168.1.42:54321", func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.42"), Mask: net.CIDRMask(24, 32)}}, nil
	})
	if !got {
		t.Fatalf("isLocalMacWithAddrs should accept the Mac's own LAN interface address")
	}
	if isLocalMacWithAddrs("203.0.113.99:54321", func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.42"), Mask: net.CIDRMask(24, 32)}}, nil
	}) {
		t.Fatalf("isLocalMacWithAddrs accepted a non-local remote address")
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
	srv, err := hyperserve.NewServer(
		hyperserve.WithAddr("127.0.0.1:0"),
		hyperserve.WithSuppressBanner(true),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	Register(Dependencies{
		Server:          srv,
		Store:           store,
		Auth:            authMgr,
		Daemon:          fakeClient,
		Live:            liveSvc,
		Relay:           relayClient,
		PublicURL:       "https://relay.example",
		Version:         "test-version",
		AlertController: alertController,
	})
	return srv
}

type governanceHTTPTestSender struct {
	payloads []push.Payload
}

func (s *governanceHTTPTestSender) Send(_ context.Context, sub state.PushSubscription, _ state.VAPIDKeys, payload push.Payload) state.PushAttempt {
	s.payloads = append(s.payloads, payload)
	return state.PushAttempt{SubscriptionID: sub.ID, OK: true, Class: state.GovernanceTransportAccepted}
}

func newGovernanceTestHandler(t *testing.T, fakeClient daemonclient.Client) (*hyperserve.Server, *state.Store, *governanceHTTPTestSender) {
	return newGovernanceTestHandlerWithPoll(t, fakeClient, true)
}

func newGovernanceTestHandlerWithPoll(t *testing.T, fakeClient daemonclient.Client, poll bool) (*hyperserve.Server, *state.Store, *governanceHTTPTestSender) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureVAPID(time.Now().UTC(), func() (string, string, error) { return "private", "public", nil }); err != nil {
		t.Fatal(err)
	}
	liveSvc := live.New(fakeClient, time.Minute, time.Minute)
	if poll {
		liveSvc.PollOnce(t.Context())
	}
	srv, err := hyperserve.NewServer(hyperserve.WithAddr("127.0.0.1:0"), hyperserve.WithSuppressBanner(true))
	if err != nil {
		t.Fatal(err)
	}
	sender := &governanceHTTPTestSender{}
	alertController := &alerts.Dispatcher{Store: store, Sender: sender, URL: "https://relay.example"}
	authMgr := auth.NewManager(store, alertController, time.Minute)
	Register(Dependencies{
		Server: srv, Store: store, Auth: authMgr, Daemon: fakeClient, Live: liveSvc,
		Relay: relay.Noop{PublicURL: "https://relay.example"}, PublicURL: "https://relay.example", Version: "test-version",
		AlertController: alertController,
	})
	return srv, store, sender
}

type routeTestRelay struct {
	route string
}

func (r routeTestRelay) Run(context.Context) {}

func (r routeTestRelay) Status() relay.Status {
	return relay.Status{Mode: "test", URL: "https://relay.example", Connected: true}
}

func (r routeTestRelay) PairingURL(raw string) string {
	if strings.Contains(raw, "?") {
		return raw + "&remote=" + r.route
	}
	return raw + "?remote=" + r.route
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

type routeSettingsStatusClient struct {
	routeFakeClient
	status string
}

type routeSettingsPatchCaptureClient struct {
	routeFakeClient
	calls int
	patch json.RawMessage
}

func (c *routeSettingsPatchCaptureClient) UpdateSettings(_ context.Context, patch json.RawMessage) (*rpc.PlatformSettings, error) {
	c.calls++
	c.patch = append(c.patch[:0], patch...)
	return c.Settings(context.Background())
}

func (c routeSettingsStatusClient) Settings(ctx context.Context) (*rpc.PlatformSettings, error) {
	settings, err := c.routeFakeClient.Settings(ctx)
	if err != nil {
		return nil, err
	}
	settings.MarketData.Quality.Status = c.status
	settings.MarketData.Quality.Summary = c.status
	return settings, nil
}

func (c routeSettingsStatusClient) UpdateSettings(ctx context.Context, _ json.RawMessage) (*rpc.PlatformSettings, error) {
	return c.Settings(ctx)
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

// routeFrozenFakeClient reports a runtime trading freeze: writes blocked,
// cancels still expected to reach the daemon (which strips the freeze
// blocker on its cancel path).
type routeFrozenFakeClient struct {
	routeWriteFakeClient
}

type routeModifyPreviewContractClient struct {
	routeWriteFakeClient
	order         rpc.OrderView
	previewParams rpc.OrderPreviewParams
}

func (c *routeModifyPreviewContractClient) OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	return &rpc.OrderStatusResult{Found: true, Order: c.order, AsOf: time.Now().UTC()}, nil
}

func (c *routeModifyPreviewContractClient) OrderPreview(ctx context.Context, params rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	c.previewParams = params
	return c.routeWriteFakeClient.OrderPreview(ctx, params)
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

func routeOpenOrderView() rpc.OrderView {
	return rpc.OrderView{
		OrderRef:        "ord-1",
		PreviewTokenID:  "tok-1",
		Account:         "DU123",
		Endpoint:        "127.0.0.1:7497",
		Mode:            "paper",
		Symbol:          "SPY",
		SecType:         "STK",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        2,
		LimitPrice:      450.25,
		Status:          "submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       "sent",
		Open:            true,
		UpdatedAt:       time.Now().UTC(),
	}
}

func routeModifyPreviewOrderView() rpc.OrderView {
	order := routeOpenOrderView()
	order.Symbol = "SAP"
	order.ConID = 29622935
	order.Exchange = "SMART"
	order.PrimaryExch = "IBIS"
	order.Currency = "EUR"
	order.LocalSymbol = "SAP"
	order.TradingClass = "SAP"
	order.Multiplier = 1
	return order
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
