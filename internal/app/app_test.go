package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewRefusesPreviewReadGrantOffLoopback(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"0.0.0.0:8765", "192.168.1.5:8765", ":8765"} {
		opts := Options{Addr: addr, StateDir: t.TempDir(), Version: "test", PreviewReadGrant: true}
		if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("New(%q) err=%v, want loopback refusal", addr, err)
		}
	}
}

func TestHTTPServerClosesAmbientHyperServeCapabilities(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "hyperserve.json")
	config := `{
		"tls": true,
		"tls_addr": ":9443",
		"cert_file": "/tmp/ambient-cert.pem",
		"key_file": "/tmp/ambient-key.pem",
		"run_health_server": true,
		"health_addr": ":9999",
		"static_dir": "/",
		"template_dir": "/",
		"mcp_enabled": true,
		"mcp_dev": true,
		"mcp_tools_enabled": true,
		"mcp_resources_enabled": true,
		"csp_web_worker_support": true,
		"cors": {"allowed_origins": ["https://ambient.example"]},
		"read_timeout": 1,
		"write_timeout": 1,
		"idle_timeout": 1,
		"read_header_timeout": 1
	}`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write ambient config: %v", err)
	}
	t.Setenv("HS_CONFIG_PATH", configPath)
	t.Setenv("HS_MCP_ENABLED", "true")
	t.Setenv("HS_MCP_DEV", "true")
	t.Setenv("HS_CORS_ALLOWED_ORIGINS", "https://ambient.example")
	t.Setenv("HS_CSP_WEB_WORKER_SUPPORT", "true")

	srv, err := newHTTPServer(Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("newHTTPServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(); err != nil {
			t.Errorf("stop server: %v", err)
		}
	})

	opts := srv.Options
	if opts.EnableTLS || opts.RunHealthServer || opts.MCPEnabled || srv.MCPEnabled() {
		t.Fatalf("ambient capability survived: tls=%v health=%v mcp=%v", opts.EnableTLS, opts.RunHealthServer, opts.MCPEnabled)
	}
	if opts.CORS != nil || opts.CSPWebWorkerSupport {
		t.Fatalf("ambient browser policy survived: cors=%v worker=%v", opts.CORS, opts.CSPWebWorkerSupport)
	}
	if opts.StaticDir != "" || opts.TemplateDir != "" {
		t.Fatalf("ambient filesystem roots survived: static=%q template=%q", opts.StaticDir, opts.TemplateDir)
	}
	if opts.ReadTimeout != 30*time.Second || opts.WriteTimeout != 30*time.Second || opts.IdleTimeout != 2*time.Minute || opts.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("transport budgets not pinned: read=%s write=%s idle=%s header=%s", opts.ReadTimeout, opts.WriteTimeout, opts.IdleTimeout, opts.ReadHeaderTimeout)
	}

	srv.GET("/headers", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/headers", nil)
	req.Header.Set("Origin", "https://ambient.example")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", res.Code)
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := res.Header().Get(name); got != want {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}
	if csp := res.Header().Get("Content-Security-Policy"); csp == "" || strings.Contains(csp, "blob:") {
		t.Errorf("CSP=%q, want present without ambient blob: permission", csp)
	}
	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin=%q, want no ambient CORS policy", got)
	}
	if got := res.Header().Get("Server"); got != "" {
		t.Errorf("Server=%q, want suppressed in hardened mode", got)
	}
}
