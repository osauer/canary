package apphttp

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/osauer/canary/v2/internal/loglevel"
	"github.com/osauer/hyperserve/v2"
)

func TestSendSSEPreservesFlushErrorThroughLogging(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelWarn, slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var logs bytes.Buffer
			server, err := hyperserve.New(hyperserve.WithLogger(slog.New(loglevel.NewTextHandler(&logs, level))))
			if err != nil {
				t.Fatal(err)
			}
			expected := errors.New("synthetic stream failure")
			var result error
			server.GET("/events", func(w http.ResponseWriter, _ *http.Request) {
				result = sendSSE(w, http.NewResponseController(w), hyperserve.SSEMessage{Event: "snapshot", Data: "synthetic"})
			})
			writer := &controlledSSEWriter{flushErr: expected}
			server.Handler().ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/events", nil))
			if !errors.Is(result, expected) {
				t.Fatalf("Canary send error = %v, want %v", result, expected)
			}
			if writer.flushes != 1 || len(writer.deadlines) != 1 || writer.deadlines[0].IsZero() {
				t.Fatal("failed stream did not preserve its send deadline", writer.flushes, writer.deadlines)
			}
		})
	}
}
