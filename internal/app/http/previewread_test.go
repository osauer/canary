package apphttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/osauer/canary/v2/internal/app/relay"
)

func newPreviewGrantHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandlerWithDependencies(t, routeFakeClient{}, relay.Noop{PublicURL: "https://relay.example"}, func(deps *Dependencies) {
		deps.Addr = "127.0.0.1:8766"
		deps.PreviewReadGrant = true
	}).Handler()
}

func previewGet(path, host, remote, origin string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	req.RemoteAddr = remote
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

func TestPreviewReadGrantDisabledByDefault(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, previewGet("/api/snapshot", "127.0.0.1:8766", "127.0.0.1:54321", ""))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("unpaired read without the grant status=%d, want 401", res.Code)
	}
}

func TestPreviewReadGrantServesLoopbackReads(t *testing.T) {
	t.Parallel()
	handler := newPreviewGrantHandler(t)
	cases := []struct {
		name string
		req  *http.Request
	}{
		{"loopback host", previewGet("/api/snapshot", "127.0.0.1:8766", "127.0.0.1:54321", "")},
		{"localhost host", previewGet("/api/snapshot", "localhost:8766", "127.0.0.1:54321", "")},
		{"same-origin fetch", previewGet("/api/proposals", "127.0.0.1:8766", "127.0.0.1:54321", "http://127.0.0.1:8766")},
	}
	for _, tc := range cases {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, tc.req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s status=%d, want 200; body=%s", tc.name, res.Code, res.Body.String())
		}
	}
}

func TestPreviewReadGrantRejectsForeignCallers(t *testing.T) {
	t.Parallel()
	handler := newPreviewGrantHandler(t)
	cases := []struct {
		name string
		req  *http.Request
	}{
		{"relay or rebinding host", previewGet("/api/snapshot", "preview.example:8766", "127.0.0.1:54321", "")},
		{"wrong port host", previewGet("/api/snapshot", "127.0.0.1:9999", "127.0.0.1:54321", "")},
		{"non-loopback peer", previewGet("/api/snapshot", "127.0.0.1:8766", "192.168.1.20:33333", "")},
		{"foreign origin", previewGet("/api/snapshot", "127.0.0.1:8766", "127.0.0.1:54321", "https://evil.example")},
	}
	for _, tc := range cases {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, tc.req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d, want 401", tc.name, res.Code)
		}
	}
}

func TestPreviewReadGrantNeverCoversActions(t *testing.T) {
	t.Parallel()
	handler := newPreviewGrantHandler(t)
	for _, target := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/proposals/refresh"},
		{http.MethodPatch, "/api/settings"},
		{http.MethodPost, "/api/orders/ord-1/cancel"},
	} {
		req := httptest.NewRequest(target.method, "http://127.0.0.1:8766"+target.path, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d, want 401", target.method, target.path, res.Code)
		}
	}
}

func TestPreviewReadGrantBootstrapReportsReadOnly(t *testing.T) {
	t.Parallel()
	handler := newPreviewGrantHandler(t)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, previewGet("/api/bootstrap", "127.0.0.1:8766", "127.0.0.1:54321", ""))
	if res.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d, want 200; body=%s", res.Code, res.Body.String())
	}
	var body struct {
		Auth struct {
			Authenticated bool `json:"authenticated"`
			ReadOnly      bool `json:"read_only"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if !body.Auth.Authenticated || !body.Auth.ReadOnly {
		t.Fatalf("bootstrap auth=%+v, want authenticated read-only", body.Auth)
	}
}
