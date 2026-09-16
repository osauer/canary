package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/loglevel"
)

func TestPairingCredentialsStayOutOfRequestLogs(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var logs bytes.Buffer
			slog.SetDefault(slog.New(loglevel.NewTextHandler(&logs, level)))
			server, err := newHTTPServer(Options{Addr: "127.0.0.1:0"})
			if err != nil {
				t.Fatal(err)
			}
			server.GET("/pair.html", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("nonce") != "synthetic-secret" {
					t.Error("pairing request changed")
				}
				w.WriteHeader(http.StatusNoContent)
			})
			for _, target := range []string{
				"/pair.html?pair=synthetic-pair&nonce=synthetic-secret",
				"http://synthetic-user:synthetic-password@localhost/pair%2ehtml?pair=synthetic-pair&nonce=synthetic-secret",
			} {
				logs.Reset()
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, target, nil)
				originalURL := request.URL.String()
				server.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusNoContent || request.URL.String() != originalURL {
					t.Fatal("logging changed the pairing request or response")
				}
				if strings.Contains(logs.String(), "synthetic-") {
					t.Fatal("pairing credentials or URL user information reached Canary's request log")
				}
				if !strings.Contains(logs.String(), "url="+request.URL.EscapedPath()) {
					t.Fatal("escaped route diagnostics missing")
				}
			}
		})
	}
}
