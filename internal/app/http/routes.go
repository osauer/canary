package apphttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	nethttp "net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	hyperserve "github.com/osauer/hyperserve/pkg/server"

	"github.com/osauer/canary/v2/internal/app/auth"
	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/app/relay"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
	appweb "github.com/osauer/canary/v2/web/app"
)

// Dependencies supplies the app host's explicit adapters and authorities to
// the HTTP layer. Registering a daemon client makes RPC methods reachable but
// does not transfer daemon policy or broker-write authority to HTTP.
type Dependencies struct {
	Server          *hyperserve.Server
	Store           *state.Store
	Auth            *auth.Manager
	Daemon          daemonclient.Client
	Live            *live.Service
	Relay           relay.Client
	PublicURL       string
	Version         string
	AlertController AlertDeliveryController
}

// AlertDeliveryController serializes every production mode or target-topology
// mutation with final alert confirmation and transport.
type AlertDeliveryController interface {
	SetAlertMode(string) error
	PruneDevices(time.Time) (int, error)
	AddPushSubscription(state.PushSubscription) error
	RemovePushSubscription(string) error
	SendSafeDiagnostic(context.Context, string) (state.GovernanceDiagnosticStatus, bool, error)
}

type handler struct {
	deps Dependencies
	web  nethttp.Handler
}

type contextKey string

const sessionKey contextKey = "canary-app-session"

// Register installs the embedded SPA, pairing, authenticated read, settings,
// preview, and paired-device action routes on deps.Server.
func Register(deps Dependencies) {
	h := &handler{deps: deps}
	sub, err := fs.Sub(appweb.Files, ".")
	if err != nil {
		panic(err)
	}
	h.web = nethttp.FileServer(nethttp.FS(sub))

	srv := deps.Server
	srv.GET("/", h.serveIndex)
	srv.GET("/pair.html", h.serveIndex)
	srv.GET("/manifest.webmanifest", h.serveStatic)
	// Imported modules do not inherit app.js's query string. All embedded JS
	// therefore uses serveStatic's no-cache revalidation; new embedded bytes
	// become visible after reinstalling, restarting the app, and reloading.
	for _, name := range appweb.EmbeddedJavaScriptFileNames() {
		srv.GET("/"+name, h.serveStatic)
	}
	srv.GET("/styles.css", h.serveStatic)
	srv.GET("/icon-192.png", h.serveStatic)
	srv.GET("/icon-512.png", h.serveStatic)
	srv.GET("/favicon-16.png", h.serveStatic)
	srv.GET("/favicon-32.png", h.serveStatic)
	srv.GET("/favicon-64.png", h.serveStatic)
	srv.GET("/favicon.ico", h.serveIcon)

	srv.POST("/api/pairing/sessions", h.handleStartPairing)
	srv.GET("/api/devices", h.handleDevicesList)
	srv.POST("/api/devices/prune", h.handleDevicesPrune)
	srv.GET(AppStatusPath, h.handleAppStatus)
	srv.POST("/api/pairing/complete", h.handleCompletePairing)
	srv.POST("/api/auth/challenge", h.handleAuthChallenge)
	srv.POST("/api/auth/session", h.handleAuthSession)

	srv.GET("/api/bootstrap", h.requireAuth(h.handleBootstrap))
	srv.GET("/api/snapshot", h.requireAuth(h.handleSnapshot))
	srv.GET("/api/settings", h.requireAuth(h.handleGetSettings))
	srv.PATCH("/api/settings", h.requireAuth(h.handlePatchSettings))
	srv.GET("/api/market-calendar", h.requireAuth(h.handleMarketCalendar))
	srv.GET("/api/events", h.requireAuth(h.handleEvents))
	srv.GET("/api/alerts/settings", h.requireAuth(h.handleGetAlertSettings))
	srv.PUT("/api/alerts/settings", h.requireAuth(h.handlePutAlertSettings))
	srv.GET("/api/alerts", h.requireAuth(h.handleAlerts))
	srv.GET("/api/alerts/attention", h.requireAuth(h.handleAlertAttention))
	srv.POST("/api/alerts/attention/read", h.requireAuth(h.handleAlertAttentionRead))
	srv.GET("/api/orders/open", h.requireAuth(h.handleOrdersOpen))
	srv.GET("/api/orders/{id}", h.requireAuth(h.handleOrderStatus))
	srv.POST("/api/orders/{id}/cancel", h.requireAuth(h.handleOrderCancel))
	srv.POST("/api/orders/{id}/preview-modify", h.requireAuth(h.handleOrderPreviewModify))
	srv.POST("/api/orders/{id}/modify", h.requireAuth(h.handleOrderModify))
	srv.GET("/api/proposals", h.requireAuth(h.handleProposalsSnapshot))
	srv.POST("/api/proposals/refresh", h.requireAuth(h.handleProposalsRefresh))
	srv.POST("/api/proposals/preview", h.requireAuth(h.handleProposalsPreview))
	srv.POST("/api/proposals/submit", h.requireAuth(h.handleProposalsSubmit))
	srv.POST("/api/proposals/reduce/preview", h.requireAuth(h.handleProposalsReducePreview))
	srv.POST("/api/proposals/reduce/submit", h.requireAuth(h.handleProposalsReduceSubmit))
	srv.POST("/api/proposals/reduce-portfolio/preview", h.requireAuth(h.handleProposalsReducePortfolioPreview))
	srv.POST("/api/proposals/reduce-portfolio/submit", h.requireAuth(h.handleProposalsReducePortfolioSubmit))
	srv.POST("/api/proposals/ignore", h.requireAuth(h.handleProposalsIgnore))
	srv.GET("/api/opportunities", h.requireAuth(h.handleOpportunitiesSnapshot))
	srv.POST("/api/opportunities/refresh", h.requireAuth(h.handleOpportunitiesRefresh))
	srv.POST("/api/opportunities/preview-exercise", h.requireAuth(h.handleOpportunitiesPreviewExercise))
	srv.POST("/api/opportunities/exercise", h.requireAuth(h.handleOpportunitiesSubmitExercise))
	srv.POST("/api/opportunities/ignore", h.requireAuth(h.handleOpportunitiesIgnore))
	srv.POST("/api/brief/seen", h.requireAuth(h.handleBriefSeen))
	srv.POST("/api/recon/signoff", h.requireAuth(h.handleReconcileSignoff))
	srv.GET("/api/recon/status", h.requireAuth(h.handleReconcileStatus))
	srv.POST("/api/recon/check", h.requireAuth(h.handleReconcileCheck))
	srv.POST("/api/push/subscribe", h.requireAuth(h.handlePushSubscribe))
	srv.DELETE("/api/push/{id}", h.requireAuth(h.handlePushDelete))
	srv.POST("/api/push/test", h.requireAuth(h.handleSafePushTest))
}

func (h *handler) serveIndex(w nethttp.ResponseWriter, r *nethttp.Request) {
	// GET / is a subtree pattern in net/http. Keep unknown JavaScript paths
	// and removed or unknown API paths from falling through to index.html.
	// Only exact routes registered above may return those resources.
	if strings.HasSuffix(r.URL.Path, ".js") || strings.HasPrefix(r.URL.Path, "/api/") {
		nethttp.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	nethttp.ServeFileFS(w, r, appweb.Files, "index.html")
}

func (h *handler) serveStatic(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	h.web.ServeHTTP(w, r)
}

func (h *handler) serveIcon(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Content-Type", "image/png")
	nethttp.ServeFileFS(w, r, appweb.Files, "favicon-32.png")
}

func (h *handler) handleStartPairing(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !isLocalMac(r.RemoteAddr) {
		writeError(w, nethttp.StatusForbidden, "pairing sessions can only be created from the local Mac")
		return
	}
	publicURL := h.deps.PublicURL
	var req struct {
		PublicURL string `json:"public_url"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, nethttp.StatusBadRequest, err.Error())
			return
		}
	}
	explicitPublicURL := strings.TrimSpace(req.PublicURL) != ""
	if explicitPublicURL {
		clean, err := cleanPairingPublicURL(req.PublicURL)
		if err != nil {
			writeError(w, nethttp.StatusBadRequest, err.Error())
			return
		}
		publicURL = clean
	}
	session, err := h.deps.Auth.StartPairing(publicURL)
	if err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	if !explicitPublicURL {
		session.URL = h.deps.Relay.PairingURL(session.URL)
	}
	writeJSON(w, session)
}

type deviceSummary struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	CreatedAt       time.Time `json:"created_at"`
	LastSeenAt      time.Time `json:"last_seen_at,omitzero"`
	HasKey          bool      `json:"has_key"`
	CookieCredCount int       `json:"cookie_credentials"`
}

// handleDevicesList and handleDevicesPrune are local-Mac management
// surfaces, like pairing-session creation: the relay refuses to forward
// them (forwardableAppPath), because relay-forwarded requests reach this
// process from 127.0.0.1 and would otherwise pass the local gate.
func (h *handler) handleDevicesList(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !isLocalMac(r.RemoteAddr) {
		writeError(w, nethttp.StatusForbidden, "device management is local-Mac only")
		return
	}
	grants := h.deps.Store.Devices()
	devices := make([]deviceSummary, 0, len(grants))
	for _, d := range grants {
		devices = append(devices, deviceSummary{
			ID:              d.ID,
			Name:            d.Name,
			CreatedAt:       d.CreatedAt,
			LastSeenAt:      d.LastSeenAt,
			HasKey:          strings.TrimSpace(d.PublicKeyJWK) != "",
			CookieCredCount: len(d.DeviceCookieHashes),
		})
	}
	writeJSON(w, map[string]any{"devices": devices, "total": len(devices)})
}

func (h *handler) handleDevicesPrune(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !isLocalMac(r.RemoteAddr) {
		writeError(w, nethttp.StatusForbidden, "device management is local-Mac only")
		return
	}
	var req struct {
		KeepDays int `json:"keep_days"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	if req.KeepDays < 1 {
		writeError(w, nethttp.StatusBadRequest, "keep_days must be at least 1")
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -req.KeepDays)
	removed, err := h.deps.AlertController.PruneDevices(cutoff)
	if err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	kept := len(h.deps.Store.Devices())
	slog.Info("canary app auth: pruned device grants", "removed", removed, "older_than_days", req.KeepDays, "kept", kept)
	writeJSON(w, map[string]any{"removed": removed, "kept": kept, "keep_days": req.KeepDays})
}

func cleanPairingPublicURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("public_url must use http or https")
	}
	if strings.TrimSpace(u.Host) == "" {
		return "", errors.New("public_url must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("public_url must be a base URL without query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (h *handler) handleCompletePairing(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req auth.CompletePairingRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	res, err := h.deps.Auth.CompletePairing(req)
	if err != nil {
		slog.Info("canary app auth: pairing rejected", "error", err)
		writeError(w, nethttp.StatusUnauthorized, err.Error())
		return
	}
	setSessionCookie(w, r, res.Token, res.ExpiresAt)
	if cookie, err := h.deps.Auth.IssueDeviceCookie(res.DeviceID); err == nil {
		setDeviceCookie(w, r, cookie)
	} else {
		slog.Error("canary app auth: issue device cookie", "device_id", res.DeviceID, "error", err)
	}
	slog.Info("canary app auth: paired device", "device_id", res.DeviceID)
	writeJSON(w, res)
}

func (h *handler) handleAuthChallenge(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	ch, err := h.deps.Auth.StartChallenge(req.DeviceID)
	if err != nil {
		slog.Info("canary app auth: challenge rejected", "device_id", req.DeviceID, "error", err)
		writeError(w, nethttp.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, ch)
}

func (h *handler) handleAuthSession(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		DeviceID  string `json:"device_id"`
		Challenge string `json:"challenge"`
		Signature string `json:"signature"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	sess, err := h.deps.Auth.CompleteChallenge(req.DeviceID, req.Challenge, req.Signature)
	if err != nil {
		slog.Info("canary app auth: session rejected", "device_id", req.DeviceID, "error", err)
		writeError(w, nethttp.StatusUnauthorized, err.Error())
		return
	}
	setSessionCookie(w, r, sess.Token, sess.ExpiresAt)
	// Re-provision the device cookie on every key/secret login: a client
	// that lost its cookie gets a fresh one, and because grants keep a
	// capped list of valid cookie hashes, issuing to this twin never
	// invalidates the copy Safari or the installed home-screen app holds.
	if cookie, err := h.deps.Auth.IssueDeviceCookie(sess.DeviceID); err == nil {
		setDeviceCookie(w, r, cookie)
	} else {
		slog.Error("canary app auth: issue device cookie", "device_id", sess.DeviceID, "error", err)
	}
	slog.Info("canary app auth: device login", "device_id", sess.DeviceID)
	writeJSON(w, sess)
}

func (h *handler) handleBootstrap(w nethttp.ResponseWriter, r *nethttp.Request) {
	vapid, _ := h.deps.Store.VAPID()
	writeJSON(w, map[string]any{
		"version":          h.deps.Version,
		"public_url":       h.deps.PublicURL,
		"snapshot":         h.deps.Live.Snapshot(),
		"settings":         h.settingsSnapshot(r.Context()),
		"alert_settings":   h.deps.Store.AlertSettings(),
		"alerts":           h.alertDTO(),
		"relay":            h.deps.Relay.Status(),
		"vapid_public_key": vapid.PublicKey,
		"auth":             h.authStatus(r),
	})
}

type reconciliationResponseDTO struct {
	Outcome        string              `json:"outcome,omitempty"`
	Reconciliation live.Reconciliation `json:"reconciliation"`
}

func reconciliationTerminal(status rpc.ReconAutomationStatus) bool {
	if status.Report.State == rpc.ReconReportStateDue || status.Report.State == rpc.ReconReportStateChecking {
		return false
	}
	return status.Evaluation.State != rpc.ReconEvaluationStateChecking
}

func (h *handler) handleReconcileStatus(w nethttp.ResponseWriter, r *nethttp.Request) {
	client, ok := h.deps.Daemon.(daemonclient.ReconciliationClient)
	if !ok {
		writeError(w, nethttp.StatusServiceUnavailable, "daily report status unavailable")
		return
	}
	result, err := client.ReconcileStatus(r.Context())
	if err != nil || result == nil {
		writeError(w, nethttp.StatusServiceUnavailable, "daily report status unavailable")
		return
	}
	reconciliation, err := live.ProjectReconciliation(&result.Status)
	if err != nil || reconciliation == nil {
		writeError(w, nethttp.StatusBadGateway, "invalid daily report status")
		return
	}
	if reconciliationTerminal(result.Status) && h.deps.Live != nil {
		h.deps.Live.PollNudgesOnce(r.Context())
	}
	writeJSON(w, reconciliationResponseDTO{Reconciliation: *reconciliation})
}

func (h *handler) handleReconcileCheck(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := decodeRequiredEmptyJSONObject(r); err != nil {
		writeError(w, nethttp.StatusBadRequest, "daily report check body must be an empty JSON object")
		return
	}
	client, ok := h.deps.Daemon.(daemonclient.ReconciliationClient)
	if !ok {
		writeError(w, nethttp.StatusServiceUnavailable, "daily report check unavailable")
		return
	}
	result, err := client.ReconcileCheck(r.Context())
	if err != nil || result == nil {
		writeError(w, nethttp.StatusServiceUnavailable, "daily report check unavailable")
		return
	}
	reconciliation, err := live.ProjectReconciliation(&result.Status)
	if err != nil || reconciliation == nil || rpc.ValidateReconCheckResult(*result) != nil {
		writeError(w, nethttp.StatusBadGateway, "invalid daily report check result")
		return
	}
	if reconciliationTerminal(result.Status) && h.deps.Live != nil {
		h.deps.Live.PollNudgesOnce(r.Context())
	}
	writeJSON(w, reconciliationResponseDTO{Outcome: result.Outcome, Reconciliation: *reconciliation})
}

func decodeRequiredEmptyJSONObject(r *nethttp.Request) error {
	if r.Body == nil || r.Body == nethttp.NoBody {
		return errors.New("body is required")
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	var fixed map[string]json.RawMessage
	if err := decoder.Decode(&fixed); err != nil {
		return err
	}
	if fixed == nil || len(fixed) != 0 {
		return errors.New("body must be an empty JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("body must contain exactly one empty JSON object")
	}
	return nil
}

func (h *handler) settingsSnapshot(ctx context.Context) *rpc.PlatformSettings {
	settings, err := h.deps.Daemon.Settings(ctx)
	if err != nil {
		snap := h.deps.Live.Snapshot()
		return snap.Settings
	}
	return settings
}

func (h *handler) handleGetSettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	settings, err := h.deps.Daemon.Settings(r.Context())
	if err != nil {
		writeDaemonSettingsError(w, err)
		return
	}
	writeJSON(w, settings)
}

func (h *handler) handlePatchSettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	var patch map[string]json.RawMessage
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	if patch == nil {
		writeError(w, nethttp.StatusBadRequest, "settings patch must be a JSON object")
		return
	}
	if _, ok := patch["trading"]; ok {
		writeError(w, nethttp.StatusBadRequest, "trading settings are not writable from the app; use the CLI")
		return
	}

	// Origin is server-assigned, never client-claimed: every authenticated
	// app caller is a paired device regardless of what the body says.
	delete(patch, "origin")
	patch["origin"] = json.RawMessage(strconv.Quote(rpc.OrderOriginPairedDevice))
	forwardedPatch, err := json.Marshal(patch)
	if err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	settings, err := h.deps.Daemon.UpdateSettings(r.Context(), forwardedPatch)
	if err != nil {
		writeDaemonSettingsError(w, err)
		return
	}
	writeJSON(w, settings)
}

func writeDaemonSettingsError(w nethttp.ResponseWriter, err error) {
	var rpcErr *rpc.Error
	if errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeBadRequest {
		writeError(w, nethttp.StatusBadRequest, rpcErr.Message)
		return
	}
	writeError(w, nethttp.StatusBadGateway, err.Error())
}

func (h *handler) handleSnapshot(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.deps.Live.Snapshot())
}

func (h *handler) handleMarketCalendar(w nethttp.ResponseWriter, r *nethttp.Request) {
	market := strings.TrimSpace(r.URL.Query().Get("market"))
	if market == "" {
		market = "us"
	}
	calendar, err := h.deps.Daemon.MarketCalendarFor(r.Context(), market)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, calendar)
}

func (h *handler) handleEvents(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(nethttp.Flusher)
	if !ok {
		writeError(w, nethttp.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, release := h.deps.Live.Subscribe()
	defer release()
	msg := hyperserve.SSEMessage{Event: "snapshot", Data: h.deps.Live.Snapshot()}
	fmt.Fprint(w, msg.String())
	alerts := h.alertDTO()
	alertCursor := newAlertStreamCursor(alerts)
	msg = hyperserve.SSEMessage{Event: "alerts", Data: alerts}
	fmt.Fprint(w, msg.String())
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	alertPoll := time.NewTicker(alertPollInterval)
	defer alertPoll.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			msg := hyperserve.SSEMessage{Event: "heartbeat", Data: map[string]any{"at": time.Now().UTC()}}
			fmt.Fprint(w, msg.String())
			flusher.Flush()
		case <-alertPoll.C:
			next := h.alertDTO()
			nextCursor := newAlertStreamCursor(next)
			if nextCursor == alertCursor {
				continue
			}
			alertCursor = nextCursor
			msg := hyperserve.SSEMessage{Event: "alerts", Data: next}
			fmt.Fprint(w, msg.String())
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			msg := hyperserve.SSEMessage{Event: ev.Type, Data: ev.Data}
			fmt.Fprint(w, msg.String())
			flusher.Flush()
		}
	}
}

func (h *handler) handleGetAlertSettings(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.deps.Store.AlertSettings())
}

func (h *handler) handlePutAlertSettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req state.AlertSettings
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	if err := h.deps.AlertController.SetAlertMode(req.Mode); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, h.deps.Store.AlertSettings())
}

func (h *handler) handlePushSubscribe(w nethttp.ResponseWriter, r *nethttp.Request) {
	sess, _ := h.session(r)
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	sub := state.PushSubscription{
		ID:         fmt.Sprintf("%d", time.Now().UnixNano()),
		DeviceID:   sess.DeviceID,
		Endpoint:   req.Endpoint,
		P256DH:     req.Keys.P256DH,
		Auth:       req.Keys.Auth,
		CreatedAt:  time.Now().UTC(),
		LastSeenAt: time.Now().UTC(),
	}
	if err := h.deps.AlertController.AddPushSubscription(sub); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, sub)
}

func (h *handler) handlePushDelete(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.deps.AlertController.RemovePushSubscription(r.PathValue("id")); err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// SafePushTestResult reports a redacted diagnostic transport class. A true
// PushServiceAccepted is not proof of device display or human attention.
type SafePushTestResult struct {
	State               string `json:"state"`
	PushServiceAccepted bool   `json:"push_service_accepted"`
}

func (h *handler) handleSafePushTest(w nethttp.ResponseWriter, r *nethttp.Request) {
	if r.Body != nil && r.ContentLength != 0 {
		var fixed map[string]json.RawMessage
		if err := decodeJSON(r, &fixed); err != nil || fixed == nil || len(fixed) != 0 {
			if err == nil {
				err = errors.New("safe notification test body must be empty")
			}
			writeError(w, nethttp.StatusBadRequest, err.Error())
			return
		}
	}
	sess, ok := h.session(r)
	if !ok {
		writeError(w, nethttp.StatusUnauthorized, "unauthorized")
		return
	}
	status, accepted, err := h.deps.AlertController.SendSafeDiagnostic(r.Context(), sess.DeviceID)
	if err != nil {
		writeError(w, nethttp.StatusInternalServerError, "diagnostic dispatch failed")
		return
	}
	writeJSON(w, SafePushTestResult{State: status.State, PushServiceAccepted: accepted})
}

func (h *handler) requireAuth(next nethttp.HandlerFunc) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		token := bearerToken(r)
		if token == "" {
			if c, err := r.Cookie("ibkr_app_session"); err == nil {
				token = c.Value
			}
		}
		sess, ok := h.deps.Auth.Authenticate(token)
		if !ok {
			sess, ok = h.deviceCookieSession(w, r)
		}
		if !ok {
			writeError(w, nethttp.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	}
}

// deviceCookieSession mints a session from the long-lived device cookie.
// This is the continuity path for clients whose script storage does not
// survive — the iOS home-screen web app inherits Safari's cookies but not
// its localStorage/IndexedDB, so the key/secret re-login can never run
// there. Every outcome is logged: a silent 401 on the phone is otherwise
// undiagnosable from the Mac.
func (h *handler) deviceCookieSession(w nethttp.ResponseWriter, r *nethttp.Request) (auth.Session, bool) {
	c, err := r.Cookie(deviceCookieName)
	if err != nil || strings.TrimSpace(c.Value) == "" {
		return auth.Session{}, false
	}
	sess, err := h.deps.Auth.AuthenticateDeviceCookie(c.Value)
	if err != nil {
		slog.Info("canary app auth: device cookie rejected", "method", r.Method, "path", r.URL.Path, "error", err)
		return auth.Session{}, false
	}
	slog.Info("canary app auth: session minted from device cookie", "device_id", sess.DeviceID, "method", r.Method, "path", r.URL.Path)
	setSessionCookie(w, r, sess.Token, sess.ExpiresAt)
	// Slide the cookie's clock without rotating its value (twin jars).
	setDeviceCookie(w, r, c.Value)
	return sess, true
}

func (h *handler) session(r *nethttp.Request) (auth.Session, bool) {
	v := r.Context().Value(sessionKey)
	sess, ok := v.(auth.Session)
	return sess, ok
}

func (h *handler) authStatus(r *nethttp.Request) map[string]any {
	sess, ok := h.session(r)
	if !ok {
		return map[string]any{"authenticated": false}
	}
	device, _ := h.deps.Store.Device(sess.DeviceID)
	return map[string]any{
		"authenticated": true,
		"session":       sess,
		"device":        device,
	}
}

func decodeJSON(r *nethttp.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func decodeAttentionReadRequest(r *nethttp.Request) (uint64, error) {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return 0, errors.New("attention read body must be an object")
	}
	key, err := dec.Token()
	if err != nil || key != "through_seq" {
		return 0, errors.New("attention read body must contain through_seq")
	}
	var throughSeq *uint64
	if err := dec.Decode(&throughSeq); err != nil || throughSeq == nil {
		return 0, errors.New("through_seq must be an unsigned integer")
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') {
		return 0, errors.New("attention read body must contain only through_seq")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return 0, errors.New("attention read body must contain exactly one object")
	}
	return *throughSeq, nil
}

func writeJSON(w nethttp.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w nethttp.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func bearerToken(r *nethttp.Request) string {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ""
	}
	typ, token, ok := strings.Cut(raw, " ")
	if !ok || !strings.EqualFold(typ, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func setSessionCookie(w nethttp.ResponseWriter, r *nethttp.Request, token string, expires time.Time) {
	nethttp.SetCookie(w, &nethttp.Cookie{
		Name:     "ibkr_app_session",
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: nethttp.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
	})
}

const (
	deviceCookieName = "ibkr_app_device"
	// 400 days, the browser cookie-lifetime cap; the clock slides on every
	// device-cookie login.
	deviceCookieMaxAge = 400 * 24 * 60 * 60
)

func setDeviceCookie(w nethttp.ResponseWriter, r *nethttp.Request, value string) {
	nethttp.SetCookie(w, &nethttp.Cookie{
		Name:     deviceCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   deviceCookieMaxAge,
		HttpOnly: true,
		// Lax, not Strict: the first navigation into the app after a QR
		// scan is a cross-site top-level navigation and must still carry
		// the continuity credential.
		SameSite: nethttp.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})
}

func requestIsHTTPS(r *nethttp.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

type interfaceAddrsFunc func() ([]net.Addr, error)

func isLocalMac(remote string) bool {
	return isLocalMacWithAddrs(remote, net.InterfaceAddrs)
}

func isLocalMacWithAddrs(remote string, addrs interfaceAddrsFunc) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	return isLocalInterfaceIP(ip, addrs)
}

func isLocalInterfaceIP(ip net.IP, addrs interfaceAddrsFunc) bool {
	if addrs == nil {
		return false
	}
	items, err := addrs()
	if err != nil {
		return false
	}
	for _, item := range items {
		var local net.IP
		switch v := item.(type) {
		case *net.IPNet:
			local = v.IP
		case *net.IPAddr:
			local = v.IP
		default:
			continue
		}
		if local != nil && local.Equal(ip) {
			return true
		}
	}
	return false
}
