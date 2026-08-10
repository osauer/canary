package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	appweb "github.com/osauer/canary/v2/web/app"
)

// DefaultWorkerURL is the public relay origin used when WorkerOptions.BaseURL
const DefaultWorkerURL = "https://remote.osauer.dev"

// defaultRouteTTL mirrors ROUTE_TTL_MS in the Cloudflare relay worker. The
const defaultRouteTTL = 7 * 24 * time.Hour

// minCycleEvery floors the forced reconnect interval so a misbehaving relay
const minCycleEvery = time.Minute

// errRouteExpired marks a connect or resume attempt rejected with HTTP 410:
var errRouteExpired = errors.New("remote relay route expired")

// errRouteRejected marks a connect or resume attempt the relay refused as
// unauthorized (401/403/404): the relay-side connector token no longer
// matches what this Mac holds, and only a re-registration can reconcile it.
var errRouteRejected = errors.New("remote relay route rejected")

// WorkerOptions configures one outbound relay connector. OriginURL is the local
// and ResumeConnectorToken must be supplied together; the token is a bearer
// app-local storage and must protect ConnectorToken.
type WorkerOptions struct {
	BaseURL              string
	OriginURL            string
	Version              string
	HTTPClient           *http.Client
	ResumeRouteID        string
	ResumeConnectorToken string
	OnRoute              func(RouteRegistration) error
}

// RouteRegistration contains relay-issued transport coordinates. RouteID is a
// non-authorizing route selector; ConnectorToken is a bearer secret used only
// by the outbound connector. ExpiresAt is the relay-reported route expiry.
type RouteRegistration struct {
	RouteID        string
	PublicURL      string
	ConnectorURL   string
	ConnectorToken string
	ExpiresAt      time.Time
}

// Worker maintains one outbound relay registration and WebSocket connection to
// the local app host. It forwards only package-allowlisted paths and delegates
// started only once.
type Worker struct {
	baseURL    string
	originURL  string
	version    string
	httpClient *http.Client
	onRoute    func(RouteRegistration) error

	// mu guards the route fields too: register() runs again after a 410
	mu           sync.RWMutex
	routeID      string
	publicURL    string
	connectorURL string
	token        string
	routeTTL     time.Duration
	cycleEvery   time.Duration
	connected    bool
	message      string
}

type registerRequest struct {
	Version        string `json:"version,omitempty"`
	RouteID        string `json:"route_id,omitempty"`
	ConnectorToken string `json:"connector_token,omitempty"`
}

type registerResponse struct {
	RouteID      string    `json:"route_id"`
	PublicURL    string    `json:"public_url"`
	ConnectorURL string    `json:"connector_url"`
	Token        string    `json:"connector_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type frame struct {
	Type    string              `json:"type"`
	ID      string              `json:"id,omitempty"`
	Method  string              `json:"method,omitempty"`
	Path    string              `json:"path,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"`
	Status  int                 `json:"status,omitempty"`
	Error   string              `json:"error,omitempty"`
}

type frameSender func(context.Context, frame) error

type requestCancelSet struct {
	mu     sync.Mutex
	active map[string]*requestCancelEntry
}

type requestCancelEntry struct {
	cancel context.CancelFunc
}

func newRequestCancelSet() *requestCancelSet {
	return &requestCancelSet{active: make(map[string]*requestCancelEntry)}
}

func (s *requestCancelSet) start(parent context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	entry := &requestCancelEntry{cancel: cancel}
	s.mu.Lock()
	previous := s.active[id]
	s.active[id] = entry
	s.mu.Unlock()
	if previous != nil {
		previous.cancel()
	}
	return ctx, func() {
		s.mu.Lock()
		if s.active[id] == entry {
			delete(s.active, id)
		}
		s.mu.Unlock()
		cancel()
	}
}

func (s *requestCancelSet) cancel(id string) {
	s.mu.Lock()
	entry := s.active[id]
	if entry != nil {
		delete(s.active, id)
	}
	s.mu.Unlock()
	if entry != nil {
		entry.cancel()
	}
}

// NewWorker validates opts and constructs a stopped Worker without performing
// and resume route ID and connector token must be either both present or both
func NewWorker(opts WorkerOptions) (*Worker, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultWorkerURL
	}
	originURL := strings.TrimRight(strings.TrimSpace(opts.OriginURL), "/")
	if originURL == "" {
		return nil, errors.New("relay origin URL required")
	}
	resumeRouteID := strings.TrimSpace(opts.ResumeRouteID)
	resumeToken := strings.TrimSpace(opts.ResumeConnectorToken)
	if (resumeRouteID == "") != (resumeToken == "") {
		return nil, errors.New("relay resume route requires both route id and connector token")
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = dualStackHTTPClient()
	}
	w := &Worker{
		baseURL:    baseURL,
		originURL:  originURL,
		version:    strings.TrimSpace(opts.Version),
		httpClient: httpClient,
		onRoute:    opts.OnRoute,
		// The worker serves phones from its own origin, so the public URL
		publicURL:  baseURL,
		routeTTL:   defaultRouteTTL,
		cycleEvery: defaultRouteTTL / 2,
		message:    "relay registration pending",
	}
	if resumeRouteID != "" {
		w.routeID = resumeRouteID
		w.token = resumeToken
		w.connectorURL = connectorURLFor(baseURL, resumeRouteID, resumeToken)
	}
	// Registration happens inside Run: a relay or DNS outage at startup
	// must not kill the app (boot races here used to be fatal), and the
	// held route already lets pairing URLs carry the stable route id.
	return w, nil
}

func connectorURLFor(baseURL, routeID, token string) string {
	origin := strings.Replace(baseURL, "https://", "wss://", 1)
	origin = strings.Replace(origin, "http://", "ws://", 1)
	return origin + "/api/connect?route_id=" + url.QueryEscape(routeID) + "&token=" + url.QueryEscape(token)
}

// registerCurrent (re)registers at the relay, resuming the held route when
// one exists. Only a definitive relay rejection (401/403/404/410) abandons
// the held route for a fresh registration; transient failures keep the
// route so paired phones survive relay or network outages.
func (w *Worker) registerCurrent(ctx context.Context) error {
	w.mu.RLock()
	routeID, token := w.routeID, w.token
	w.mu.RUnlock()
	if routeID != "" && token != "" {
		err := w.register(ctx, registerRequest{Version: w.version, RouteID: routeID, ConnectorToken: token})
		if err == nil {
			return nil
		}
		if !errors.Is(err, errRouteExpired) && !errors.Is(err, errRouteRejected) {
			return err
		}
		slog.Warn("canary app relay: relay refused route resume; registering a fresh route", "route_id", routeID, "error", err)
	}
	if err := w.register(ctx, registerRequest{Version: w.version}); err != nil {
		return err
	}
	w.mu.RLock()
	newRouteID := w.routeID
	w.mu.RUnlock()
	if routeID != "" && newRouteID != routeID {
		slog.Warn("canary app relay: route re-registered; previously paired devices must re-pair", "route_id", newRouteID, "previous_route_id", routeID)
	}
	return nil
}

func (w *Worker) hasRoute() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.routeID != "" && w.token != "" && w.connectorURL != ""
}

func (w *Worker) register(ctx context.Context, rrq registerRequest) error {
	body, err := json.Marshal(rrq)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+"/api/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("register remote relay at %s: %w", w.baseURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		switch res.StatusCode {
		case http.StatusGone:
			return fmt.Errorf("register remote relay: %s: %w", res.Status, errRouteExpired)
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return fmt.Errorf("register remote relay: %s: %s: %w", res.Status, strings.TrimSpace(string(msg)), errRouteRejected)
		}
		return fmt.Errorf("register remote relay: %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
	var rr registerResponse
	if err := json.NewDecoder(res.Body).Decode(&rr); err != nil {
		return err
	}
	if rr.RouteID == "" || rr.PublicURL == "" || rr.ConnectorURL == "" || rr.Token == "" {
		return fmt.Errorf("register remote relay: incomplete response %#v", rr)
	}
	ttl := defaultRouteTTL
	cycle := defaultRouteTTL / 2
	if observedTTL := time.Until(rr.ExpiresAt); !rr.ExpiresAt.IsZero() && observedTTL > 0 {
		ttl = observedTTL
		cycle = ttl / 2
	}
	cycle = max(cycle, minCycleEvery)
	w.mu.Lock()
	w.routeID = rr.RouteID
	w.publicURL = strings.TrimRight(rr.PublicURL, "/")
	w.connectorURL = rr.ConnectorURL
	w.token = rr.Token
	w.routeTTL = ttl
	w.cycleEvery = cycle
	w.connected = false
	w.message = "registered remote relay route"
	w.mu.Unlock()
	if err := w.persistRoute(RouteRegistration{
		RouteID:        rr.RouteID,
		PublicURL:      strings.TrimRight(rr.PublicURL, "/"),
		ConnectorURL:   rr.ConnectorURL,
		ConnectorToken: rr.Token,
		ExpiresAt:      rr.ExpiresAt,
	}); err != nil {
		return err
	}
	return nil
}

// Run registers or resumes the relay route and maintains its outbound
// failures are retried with bounded backoff and reported through
// Run returns no error and must not be invoked concurrently on one Worker.
func (w *Worker) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		// Register lazily and retryably: startup must survive a relay or
		// DNS outage, and a held route resumes (and revives) at the relay
		// rather than minting a new route id.
		if !w.hasRoute() {
			if err := w.registerCurrent(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				w.setStatus(false, err.Error())
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			backoff = time.Second
		}
		// Force the connection to cycle at half the route TTL: the relay
		// connection, so a long-lived quiet connection must reconnect
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		cycleTimer := time.AfterFunc(w.cycleDuration(), cancelAttempt)
		err := w.connectOnce(attemptCtx)
		cycled := !cycleTimer.Stop() // Stop() == false: the cycle deadline fired.
		cancelAttempt()
		if ctx.Err() != nil {
			return // parent cancelled: real shutdown, not a forced cycle
		}
		if cycled {
			backoff = time.Second
			continue // reconnect promptly so the relay-side TTL slides
		}
		if errors.Is(err, errRouteExpired) || errors.Is(err, errRouteRejected) {
			regErr := w.registerCurrent(ctx)
			if regErr == nil {
				backoff = time.Second
				continue
			}
			err = fmt.Errorf("relay refused the held route; re-register failed: %w", regErr)
		}
		if err == nil {
			backoff = time.Second
			continue
		}
		w.setStatus(false, err.Error())
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// sleepCtx waits d, returning false when ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *Worker) cycleDuration() time.Duration {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.cycleEvery <= 0 {
		return defaultRouteTTL / 2
	}
	return w.cycleEvery
}

func (w *Worker) connectOnce(ctx context.Context) error {
	w.mu.RLock()
	connectorURL, token := w.connectorURL, w.token
	w.mu.RUnlock()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, res, err := websocket.Dial(ctx, connectorURL, &websocket.DialOptions{
		HTTPClient: w.httpClient,
		HTTPHeader: header,
	})
	if err != nil {
		if res != nil {
			switch res.StatusCode {
			case http.StatusGone:
				return fmt.Errorf("connect remote relay: %s: %w", res.Status, errRouteExpired)
			case http.StatusUnauthorized, http.StatusForbidden:
				return fmt.Errorf("connect remote relay: %s: %w", res.Status, errRouteRejected)
			}
			return fmt.Errorf("connect remote relay: %s: %w", res.Status, err)
		}
		return fmt.Errorf("connect remote relay: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "canary app relay reconnect")
	w.setStatus(true, "connected")
	if err := w.persistRouteExtension(time.Now().UTC()); err != nil {
		slog.Error("canary app relay: persist route extension", "error", err)
	}

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	send := func(ctx context.Context, f frame) error {
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		writeCtx, writeCancel := context.WithTimeout(ctx, 30*time.Second)
		defer writeCancel()
		return conn.Write(writeCtx, websocket.MessageText, data)
	}

	var wg sync.WaitGroup
	requests := newRequestCancelSet()
	for connCtx.Err() == nil {
		typ, data, err := conn.Read(connCtx)
		if err != nil {
			cancel()
			wg.Wait()
			w.setStatus(false, "disconnected")
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.ID == "" {
			continue
		}
		switch f.Type {
		case "request":
			requestFrame := f
			requestCtx, finish := requests.start(connCtx, f.ID)
			wg.Go(func() {
				defer finish()
				requestSend := func(_ context.Context, response frame) error {
					if err := requestCtx.Err(); err != nil {
						return err
					}
					// A per-request cancellation must not become the write
					return send(connCtx, response)
				}
				_ = w.serveRequest(requestCtx, requestFrame, requestSend)
			})
		case "request_cancel":
			requests.cancel(f.ID)
		}
	}
	wg.Wait()
	return connCtx.Err()
}

func (w *Worker) serveRequest(ctx context.Context, reqFrame frame, send frameSender) error {
	if !forwardableAppPath(reqFrame.Path) {
		return send(ctx, frame{
			Type:   "response_error",
			ID:     reqFrame.ID,
			Status: http.StatusForbidden,
			Error:  "route is local-control only",
		})
	}
	body, err := base64.StdEncoding.DecodeString(reqFrame.Body)
	if err != nil {
		return send(ctx, frame{Type: "response_error", ID: reqFrame.ID, Status: http.StatusBadRequest, Error: "invalid request body"})
	}
	method := strings.TrimSpace(reqFrame.Method)
	if method == "" {
		method = http.MethodGet
	}
	target := w.originURL + reqFrame.Path
	localReq, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return send(ctx, frame{Type: "response_error", ID: reqFrame.ID, Status: http.StatusBadRequest, Error: err.Error()})
	}
	copyForwardHeaders(localReq.Header, reqFrame.Headers)
	localReq.Header.Set("X-Forwarded-Proto", "https")
	if u, err := url.Parse(w.PublicURL()); err == nil && u.Host != "" {
		localReq.Header.Set("X-Forwarded-Host", u.Host)
		localReq.Host = u.Host
	}

	res, err := w.httpClient.Do(localReq)
	if err != nil {
		return send(ctx, frame{Type: "response_error", ID: reqFrame.ID, Status: http.StatusBadGateway, Error: err.Error()})
	}
	defer res.Body.Close()
	if err := send(ctx, frame{
		Type:    "response_start",
		ID:      reqFrame.ID,
		Status:  res.StatusCode,
		Headers: responseHeaders(res.Header),
	}); err != nil {
		return err
	}
	buf := make([]byte, 16*1024)
	for {
		n, readErr := res.Body.Read(buf)
		if n > 0 {
			if err := send(ctx, frame{
				Type: "response_chunk",
				ID:   reqFrame.ID,
				Body: base64.StdEncoding.EncodeToString(buf[:n]),
			}); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return send(ctx, frame{Type: "response_end", ID: reqFrame.ID})
		}
		if readErr != nil {
			return send(ctx, frame{Type: "response_error", ID: reqFrame.ID, Status: http.StatusBadGateway, Error: readErr.Error()})
		}
	}
}

// Status returns a detached, concurrency-safe snapshot of the relay connection.
// device authentication, daemon availability, or broker readiness.
func (w *Worker) Status() Status {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return Status{
		Mode:      "cloudflare-worker",
		URL:       w.publicURL,
		Connected: w.connected,
		Message:   w.message,
	}
}

// PairingURL adds the current non-authorizing route ID as the remote query
// Worker, an unavailable route, or an invalid URL, and never includes the
// connector token.
func (w *Worker) PairingURL(raw string) string {
	if w == nil {
		return raw
	}
	w.mu.RLock()
	routeID := w.routeID
	w.mu.RUnlock()
	if routeID == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("remote") == "" {
		q.Set("remote", routeID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// PublicURL returns the current public relay origin. Before registration this
// is the configured base URL; after registration it is the relay-issued URL. A
// nil Worker returns an empty string.
func (w *Worker) PublicURL() string {
	if w == nil {
		return ""
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.publicURL
}

func (w *Worker) setStatus(connected bool, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.connected = connected
	w.message = message
}

func (w *Worker) persistRoute(reg RouteRegistration) error {
	if w.onRoute == nil {
		return nil
	}
	return w.onRoute(reg)
}

func (w *Worker) persistRouteExtension(now time.Time) error {
	if w.onRoute == nil {
		return nil
	}
	w.mu.RLock()
	reg := RouteRegistration{
		RouteID:        w.routeID,
		PublicURL:      w.publicURL,
		ConnectorURL:   w.connectorURL,
		ConnectorToken: w.token,
		ExpiresAt:      now.Add(w.routeTTLDuration()),
	}
	w.mu.RUnlock()
	if reg.RouteID == "" || reg.ConnectorToken == "" {
		return nil
	}
	return w.onRoute(reg)
}

func (w *Worker) routeTTLDuration() time.Duration {
	if w.routeTTL <= 0 {
		return defaultRouteTTL
	}
	return w.routeTTL
}

func forwardableAppPath(raw string) bool {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}
	p := u.Path
	// Local-control surfaces must not be reachable through the relay:
	if p == "/api/pairing/sessions" || p == "/api/devices" || strings.HasPrefix(p, "/api/devices/") || p == "/api/app-status" {
		return false
	}
	if p == "/" || p == "/pair.html" || strings.HasPrefix(p, "/api/") {
		return true
	}
	for _, name := range appweb.EmbeddedJavaScriptFileNames() {
		if p == "/"+name {
			return true
		}
	}
	switch p {
	case "/manifest.webmanifest", "/styles.css",
		"/icon-192.png", "/icon-512.png", "/favicon-16.png", "/favicon-32.png", "/favicon-64.png", "/favicon.ico":
		return true
	default:
		return false
	}
}

func copyForwardHeaders(dst http.Header, src map[string][]string) {
	for k, values := range src {
		if skipForwardHeader(k) {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func responseHeaders(src http.Header) map[string][]string {
	out := make(map[string][]string, len(src))
	for k, values := range src {
		if skipForwardHeader(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		out[k] = append([]string(nil), values...)
	}
	return out
}

func skipForwardHeader(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func dualStackHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:       30 * time.Second,
		KeepAlive:     30 * time.Second,
		FallbackDelay: 300 * time.Millisecond,
		Resolver: &net.Resolver{
			PreferGo: true,
		},
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}
