package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Resource template URIs exposed by the streaming MCP surface. Streaming
// resources currently support stock quotes only. An option resource requires
// an explicit option contract at the resource-to-daemon seam and end-to-end
// subscription verification; it must not reuse the stock template contract.
const (
	StockQuoteURITemplate = "canary://quote/{symbol}"

	stockQuoteScheme = "canary://quote/"
)

// ResourceTemplate is the wire shape returned by resources/templates/list.
// Mirrors the MCP spec's resourceTemplate object: URI template (RFC 6570
// flavor), human name, mimeType for the read response.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MIMEType    string `json:"mimeType"`
}

// ResourceTemplates is the canonical streaming-resource inventory. The
// TestStreamingParity gate asserts this list is exactly what we expose,
// so a contributor adding a new template (or renaming one) updates the
// test in lockstep.
var ResourceTemplates = []ResourceTemplate{
	{
		URITemplate: StockQuoteURITemplate,
		Name:        "stock-quote",
		Description: "Live snapshot + streaming quote for an equity / ETF symbol. resources/read returns the latest tick; resources/subscribe streams coalesced ticks until unsubscribe.",
		MIMEType:    "application/json",
	},
}

// parsedURI is what URI parsing produces: the symbol the daemon
// subscribes against.
type parsedURI struct {
	// Sym is the canonical symbol passed to the daemon's subscribe RPC
	// (uppercased bare ticker for the stock template).
	Sym string

	// OriginalURI is preserved for use as the resource notification
	// target — MCP clients track subscriptions by URI string.
	OriginalURI string
}

// parseQuoteURI accepts a fully-qualified MCP resource URI and returns the
// canonical symbol the daemon expects, or an error suitable for surfacing
// as an MCP invalid-params response.
//
// Accepted shape:
//   - canary://quote/AAPL  → sym="AAPL"
func parseQuoteURI(uri string) (parsedURI, error) {
	uri = strings.TrimSpace(uri)
	if !strings.HasPrefix(uri, stockQuoteScheme) {
		return parsedURI{}, fmt.Errorf("unrecognised resource URI %q (expected %s)", uri, StockQuoteURITemplate)
	}
	rest := uri[len(stockQuoteScheme):]
	if rest == "" || strings.Contains(rest, "/") {
		return parsedURI{}, fmt.Errorf("invalid stock quote URI %q: expected %s", uri, StockQuoteURITemplate)
	}
	return parsedURI{Sym: strings.ToUpper(rest), OriginalURI: uri}, nil
}

// uriContainsTradingVerb is the safety counterpart for resource URIs,
// parallel to TestNoTradingTools' check on tool names. Catches a
// contributor adding a `canary://order/...` template by mistake.
//
//lint:ignore U1000 Retained as a package-local helper for the safety test.
func uriContainsTradingVerb(uri string) (bool, string) {
	low := strings.ToLower(uri)
	for _, banned := range []string{"order", "trade", "cancel", "submit", "place"} {
		if strings.Contains(low, banned) {
			return true, banned
		}
	}
	return false, ""
}

// resourcesListResult is the wire shape for resources/list. We don't
// enumerate concrete resources (would be infinite — every symbol is a
// resource), so this is always empty; templates carry the actual surface.
type resourcesListResult struct {
	Resources []json.RawMessage `json:"resources"`
}

// resourcesTemplatesListResult is the wire shape for resources/templates/list.
type resourcesTemplatesListResult struct {
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
}

// resourceReadResult is the wire shape for resources/read responses.
// Contents always carries one entry — the snapshot frame as a JSON string
// in a `text` block — matching the embedded-content notification format
// resources/subscribe later uses.
type resourceReadResult struct {
	Contents []resourceContent `json:"contents"`
}

// resourceSubscribeParams is the input to resources/subscribe and
// resources/unsubscribe.
type resourceSubscribeParams struct {
	URI string `json:"uri"`
}

// resourceContent is the per-content-block shape used in both reads and
// subscription notifications. text carries the JSON-stringified Frame.
type resourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType"`
	Text     string `json:"text"`
}

// snapshotReadTimeout bounds how long resources/read waits for the daemon
// to deliver a snapshot quote. Keep it longer than the CLI's common happy path:
// MCP resource reads often happen beside a long-lived resource subscription or
// a gamma/breadth background job, and a five-second budget produced avoidable
// off-hours context-deadline failures while the ordinary quote tool succeeded.
const snapshotReadTimeout = 10 * time.Second

// handleResourcesList satisfies MCP's resources/list. We don't enumerate
// concrete resources because every symbol is implicitly a resource — the
// inventory would be unbounded. Clients discover the surface via
// resources/templates/list and parametrize URIs themselves.
func (s *Server) handleResourcesList(id json.RawMessage) {
	res := resourcesListResult{Resources: []json.RawMessage{}}
	b, _ := json.Marshal(res)
	s.writeResult(id, b)
}

// handleResourcesTemplatesList returns the URI templates exposed by this
// server. Stable across daemon restarts; gated by TestStreamingParity so
// drift is caught at build time.
func (s *Server) handleResourcesTemplatesList(id json.RawMessage) {
	res := resourcesTemplatesListResult{ResourceTemplates: ResourceTemplates}
	b, _ := json.Marshal(res)
	s.writeResult(id, b)
}

// handleResourcesRead returns a current snapshot quote for the given URI.
// Contents always carries one entry — the Quote JSON in a text block —
// matching the embedded-content notification shape resources/subscribe uses.
func (s *Server) handleResourcesRead(ctx context.Context, id, params json.RawMessage) {
	var p resourceSubscribeParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	pu, err := parseQuoteURI(p.URI)
	if err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}

	readCtx, cancel := context.WithTimeout(ctx, snapshotReadTimeout)
	defer cancel()

	snapParams := rpc.QuoteSnapshotParams{
		Contract:         rpc.ContractParams{Symbol: pu.Sym, SecType: "STK", Currency: "USD"},
		TimeoutMs:        int(snapshotReadTimeout.Milliseconds()),
		IncludeLiquidity: true,
	}
	daemonConn, closeConn, err := s.toolConn(readCtx)
	if err != nil {
		s.writeError(id, codeInternalError, err.Error())
		return
	}
	defer closeConn()
	var quote rpc.Quote
	if err := daemonConn.Call(readCtx, rpc.MethodQuoteSnapshot, snapParams, &quote); err != nil {
		s.writeError(id, codeInternalError, err.Error())
		return
	}
	body, err := json.Marshal(quote)
	if err != nil {
		s.writeError(id, codeInternalError, err.Error())
		return
	}
	res := resourceReadResult{
		Contents: []resourceContent{{
			URI:      pu.OriginalURI,
			MIMEType: "application/json",
			Text:     string(body),
		}},
	}
	out, _ := json.Marshal(res)
	s.writeResult(id, out)
}

// handleResourcesSubscribe acks the subscription synchronously, opens a
// dedicated daemon connection, and spawns a goroutine that translates each
// streaming Frame into a notifications/resources/updated message with the
// frame JSON embedded in params.contents. Idempotent on the URI: a duplicate
// subscribe to a URI we already serve is a no-op success.
//
// Per the H semantics from the design session, no transparent reconnect:
// when the daemon closes the stream (gateway lost, daemon shutdown, …)
// the goroutine emits a final terminal-error notification and exits. The
// MCP client decides whether to retry.
func (s *Server) handleResourcesSubscribe(ctx context.Context, id, params json.RawMessage) {
	var p resourceSubscribeParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	pu, err := parseQuoteURI(p.URI)
	if err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	if s.dialer == nil {
		s.writeError(id, codeInternalError, "streaming subscriptions not configured on this server")
		return
	}

	s.subMu.Lock()
	if _, exists := s.subs[pu.OriginalURI]; exists {
		// Idempotent: already subscribed. Ack and move on.
		s.subMu.Unlock()
		s.writeResult(id, json.RawMessage(`{}`))
		return
	}
	s.subMu.Unlock()

	// Open a dedicated daemon conn for this subscription so it doesn't
	// contend with unary tools/call traffic on the shared conn. dial.Conn
	// holds the per-conn mutex for the stream's whole lifetime.
	daemonConn, err := s.dial(ctx)
	if err != nil {
		s.writeError(id, codeInternalError, fmt.Sprintf("dial daemon: %v", err))
		return
	}

	// Parent the streaming context on the Serve()-scoped context so a
	// client EOF (or ctx cancel from the outer process) propagates to
	// every active subscription without relying solely on
	// shutdownSubscriptions() draining the map. Falls back to Background
	// only if Serve hasn't initialized yet — defensive, not a path that
	// fires in practice.
	parent := s.serveCtx
	if parent == nil {
		parent = context.Background()
	}
	streamCtx, cancel := context.WithCancel(parent)
	s.subMu.Lock()
	s.subs[pu.OriginalURI] = cancel
	s.subMu.Unlock()

	go s.runResourceSubscription(streamCtx, pu, daemonConn)

	s.writeResult(id, json.RawMessage(`{}`))
}

// handleResourcesUnsubscribe cancels the named subscription. Idempotent
// against an unknown URI — silently succeeds, matching the spec's "client
// should not have to know subscription state precisely" posture.
func (s *Server) handleResourcesUnsubscribe(id, params json.RawMessage) {
	var p resourceSubscribeParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	s.subMu.Lock()
	cancel, exists := s.subs[p.URI]
	if exists {
		delete(s.subs, p.URI)
	}
	s.subMu.Unlock()
	if exists {
		cancel()
	}
	s.writeResult(id, json.RawMessage(`{}`))
}

// runResourceSubscription owns one streaming subscription's lifecycle.
// Opens a Stream against the daemon's MethodQuoteSubscribe, translates each
// Frame into an MCP resources/updated notification, and on stream end (for
// any reason) cleans up its subs entry and closes the daemon conn.
//
// On transport-level errors (daemon socket dropped, daemon shutdown) the
// daemon may not have delivered an in-band Frame.Error — emit a synthetic
// terminal frame so the MCP client always gets a structured signal.
func (s *Server) runResourceSubscription(ctx context.Context, pu parsedURI, conn *dial.Conn) {
	defer conn.Close()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, pu.OriginalURI)
		s.subMu.Unlock()
	}()

	s.emitInitialResourceSnapshot(ctx, pu, conn)

	subParams := rpc.QuoteSubscribeParams{
		Contract: rpc.ContractParams{Symbol: pu.Sym, SecType: "STK", Currency: "USD"},
	}

	streamErr := conn.Stream(ctx, rpc.MethodQuoteSubscribe, subParams, func(raw json.RawMessage) error {
		s.emitResourceUpdate(pu.OriginalURI, raw)
		return nil
	})

	// The daemon emits a terminal Frame.Error before closing the stream
	// for known scenarios (gateway_lost, daemon_shutdown). For transport-
	// level failures (socket dropped without {end:true}) we synthesize an
	// error frame so the consumer sees a structured terminator.
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		code := rpc.FrameErrSubscriptionRejected
		if rpcErr, ok := streamErr.(*rpc.Error); ok && rpcErr.Code == rpc.CodeGatewayUnavailable {
			code = rpc.FrameErrGatewayLost
		}
		frame := rpc.Frame{
			T:     time.Now(),
			Error: &rpc.FrameError{Code: code, Message: streamErr.Error()},
		}
		raw, _ := json.Marshal(frame)
		s.emitResourceUpdate(pu.OriginalURI, raw)
	}
}

func (s *Server) emitInitialResourceSnapshot(ctx context.Context, pu parsedURI, conn *dial.Conn) {
	readCtx, cancel := context.WithTimeout(ctx, snapshotReadTimeout)
	defer cancel()

	params := rpc.QuoteSnapshotParams{
		Contract:  rpc.ContractParams{Symbol: pu.Sym, SecType: "STK", Currency: "USD"},
		TimeoutMs: int(snapshotReadTimeout.Milliseconds()),
	}
	var q rpc.Quote
	if err := conn.Call(readCtx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
		return
	}
	frame := rpc.Frame{
		T:        q.AsOf,
		Bid:      q.Bid,
		Ask:      q.Ask,
		Last:     q.Last,
		BidSize:  q.BidSize,
		AskSize:  q.AskSize,
		DataType: q.DataType,
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return
	}
	s.emitResourceUpdate(pu.OriginalURI, raw)
}

// emitResourceUpdate writes a notifications/resources/updated message with
// the frame JSON embedded in params.contents. The empty extension key
// (`contents`) is the de-facto standard for shipping live content with the
// notification — the MCP spec is permissive.
func (s *Server) emitResourceUpdate(uri string, frameJSON json.RawMessage) {
	payload := map[string]any{
		"uri": uri,
		"contents": []resourceContent{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(frameJSON),
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.writeNotification("notifications/resources/updated", body)
}

// writeNotification is the JSON-RPC notification counterpart to writeResult
// (no id). Serializes alongside other writes via s.mu so notifications
// don't interleave with response bytes.
func (s *Server) writeNotification(method string, params json.RawMessage) {
	notif := struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	b, err := json.Marshal(notif)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		return
	}
	_, _ = s.out.Write(b)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

// shutdownSubscriptions cancels every active subscription. Called by the
// MCP server's main Serve loop on EOF / context cancel so the per-sub
// goroutines unwind and the daemon-side socket connections close, which
// in turn triggers the daemon's subManager refcount decrement.
func (s *Server) shutdownSubscriptions() {
	s.subMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.subs))
	for _, c := range s.subs {
		cancels = append(cancels, c)
	}
	s.subs = map[string]context.CancelFunc{}
	s.subMu.Unlock()
	for _, c := range cancels {
		c()
	}
}

func (s *Server) hasActiveSubscriptions() bool {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return len(s.subs) > 0
}
