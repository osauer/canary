package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/hyperserve/v2/websocket"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestConnectOnceUsesConfiguredHTTPClientForWebSocketUpgrade(t *testing.T) {
	t.Parallel()

	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer connector-token" {
			serverErr <- fmt.Errorf("Authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		if err := conn.CloseWithStatus(websocket.CloseNormalClosure, "fixture complete"); err != nil {
			serverErr <- err
		}
	}))
	defer server.Close()

	upgradeSeen := make(chan struct{})
	var once sync.Once
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			once.Do(func() { close(upgradeSeen) })
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	w := &Worker{
		connectorURL: strings.Replace(server.URL, "http://", "ws://", 1),
		token:        "connector-token",
		httpClient:   client,
		publicURL:    server.URL,
		routeTTL:     defaultRouteTTL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.connectOnce(ctx); err == nil {
		t.Fatal("connectOnce returned nil after the peer closed the fixture connection")
	}
	select {
	case <-upgradeSeen:
	default:
		t.Fatal("configured HTTP client did not perform the WebSocket upgrade")
	}
	select {
	case err := <-serverErr:
		t.Fatal(err)
	default:
	}
}
