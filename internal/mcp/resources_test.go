package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/dial"
)

// TestStreamingParity is the binding gate for the streaming MCP surface.
// Parallel to TestParity (tools↔CLI) and TestNoTradingTools (tool names),
// this asserts the resource templates exposed by the server are exactly
// the canonical inventory and that none carry a banned trading verb.
//
// `quote` is in the CLI's ExcludedCLI list with a documented reason that
// points to this test; renaming a template URI must update this test in
// lockstep.
func TestStreamingParity(t *testing.T) {
	t.Parallel()
	wantURIs := map[string]string{
		StockQuoteURITemplate: "stock-quote",
	}
	gotURIs := map[string]string{}
	for _, rt := range ResourceTemplates {
		gotURIs[rt.URITemplate] = rt.Name
		if rt.MIMEType != "application/json" {
			t.Errorf("template %s: mimeType %q, want application/json", rt.URITemplate, rt.MIMEType)
		}
		if banned, verb := uriContainsTradingVerb(rt.URITemplate); banned {
			t.Errorf("template %s contains banned trading verb %q — Canary is read-only", rt.URITemplate, verb)
		}
	}
	for uri, name := range wantURIs {
		if got := gotURIs[uri]; got != name {
			t.Errorf("missing or renamed template: want URI %q name %q, got name %q", uri, name, got)
		}
	}
	for uri := range gotURIs {
		if _, ok := wantURIs[uri]; !ok {
			t.Errorf("unexpected template %q — update wantURIs in TestStreamingParity in lockstep", uri)
		}
	}
}

// TestParseQuoteURIStock covers the stock URI template happy path and
// the validation rejections an MCP client could trigger by typo.
func TestParseQuoteURIStock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		uri     string
		wantSym string
		wantErr bool
	}{
		{"happy", "canary://quote/AAPL", "AAPL", false},
		{"lowercase", "canary://quote/aapl", "AAPL", false},
		{"empty symbol", "canary://quote/", "", true},
		{"trailing slash", "canary://quote/AAPL/", "", true},
		{"option in stock template", "canary://quote/AAPL/240119/C/195", "", true},
		{"unrecognised scheme", "canary://watch/AAPL", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pu, err := parseQuoteURI(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", pu)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if pu.Sym != tc.wantSym {
				t.Errorf("Sym: got %q, want %q", pu.Sym, tc.wantSym)
			}
		})
	}
}

// TestInitializeAdvertisesResourcesCapability covers the capability
// surface MCP clients gate on. Without resources.subscribe=true,
// Claude Desktop won't try resources/subscribe.
func TestInitializeAdvertisesResourcesCapability(t *testing.T) {
	t.Parallel()
	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")

	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	// Pretend the dialer is configured so subscribe=true.
	srv.SetDialer(func() (*dial.Conn, error) { return nil, nil })
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp struct {
		Result struct {
			Capabilities map[string]map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resources, ok := resp.Result.Capabilities["resources"]
	if !ok {
		t.Fatalf("capabilities.resources missing: %+v", resp.Result.Capabilities)
	}
	if got := resources["subscribe"]; got != true {
		t.Errorf("capabilities.resources.subscribe: got %v, want true (dialer is set)", got)
	}
}

// TestResourcesTemplatesListReturnsCanonicalInventory covers the
// resources/templates/list wire shape clients use to discover the
// streaming surface.
func TestResourcesTemplatesListReturnsCanonicalInventory(t *testing.T) {
	t.Parallel()
	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"resources/templates/list"}` + "\n")

	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp struct {
		Result resourcesTemplatesListResult `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Result.ResourceTemplates) != len(ResourceTemplates) {
		t.Errorf("resources count: got %d, want %d", len(resp.Result.ResourceTemplates), len(ResourceTemplates))
	}
	for i, want := range ResourceTemplates {
		if resp.Result.ResourceTemplates[i].URITemplate != want.URITemplate {
			t.Errorf("template[%d].URITemplate: got %q, want %q", i,
				resp.Result.ResourceTemplates[i].URITemplate, want.URITemplate)
		}
	}
}

// TestResourcesSubscribeWithoutDialerErrors covers the configuration
// guard: a server constructed without SetDialer cannot serve
// subscriptions and must say so explicitly.
func TestResourcesSubscribeWithoutDialerErrors(t *testing.T) {
	t.Parallel()
	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"resources/subscribe","params":{"uri":"canary://quote/AAPL"}}` + "\n")

	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error response, got: %s", out.String())
	}
	if !strings.Contains(strings.ToLower(resp.Error.Message), "streaming") {
		t.Errorf("error message should mention streaming: %q", resp.Error.Message)
	}
}

// TestResourcesSubscribeRejectsBadURI covers the synchronous validation
// path: a malformed URI surfaces as an MCP error response (codeInvalidParams)
// and does not start a subscription.
func TestResourcesSubscribeRejectsBadURI(t *testing.T) {
	t.Parallel()
	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"resources/subscribe","params":{"uri":"canary://nonsense/AAPL"}}` + "\n")

	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	srv.SetDialer(func() (*dial.Conn, error) { return nil, nil })
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error response, got: %s", out.String())
	}
	if resp.Error.Code != codeInvalidParams {
		t.Errorf("code: got %d, want %d", resp.Error.Code, codeInvalidParams)
	}
}

// TestResourcesUnsubscribeOnUnknownURIIsNoOp documents the spec posture
// that an unsubscribe call on a URI we don't track succeeds silently.
func TestResourcesUnsubscribeOnUnknownURIIsNoOp(t *testing.T) {
	t.Parallel()
	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"resources/unsubscribe","params":{"uri":"canary://quote/NEVER"}}` + "\n")

	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp struct {
		Result *json.RawMessage `json:"result"`
		Error  *struct{}        `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("expected success, got error response: %s", out.String())
	}
	if resp.Result == nil {
		t.Errorf("expected result, got: %s", out.String())
	}
}
