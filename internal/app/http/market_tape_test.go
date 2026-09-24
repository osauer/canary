package apphttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type tapeRouteClient struct {
	routeFakeClient
	calls  int
	params rpc.MarketTapeParams
	err    error
}

func (c *tapeRouteClient) MarketTape(_ context.Context, p rpc.MarketTapeParams) (*rpc.MarketTapeResult, error) {
	c.calls++
	c.params = p
	return &rpc.MarketTapeResult{SchemaVersion: "market-tape-v1", NotPredictive: true}, c.err
}

func TestMarketTapeReadRouteAuthorityBoundsAndErrorRedaction(t *testing.T) {
	client := &tapeRouteClient{}
	handler := newTestHandlerWithClient(t, client).Handler()
	request := func(method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	if res := request(http.MethodGet, "/api/market-tape", nil); res.Code != http.StatusUnauthorized || client.calls != 0 {
		t.Fatal("unauthenticated acquisition")
	}
	cookie := routeSessionCookie(t, handler)
	if res := request(http.MethodGet, "/api/market-tape?sessions=10", cookie); res.Code != http.StatusOK || client.params.Sessions != 10 || res.Header().Get("Cache-Control") != "no-store" || !strings.Contains(res.Body.String(), `"not_predictive":true`) {
		t.Fatalf("typed read lost: %d %s", res.Code, res.Body.String())
	}
	for _, query := range []string{"-1", "4", "61", "foo"} {
		if res := request(http.MethodGet, "/api/market-tape?sessions="+query, cookie); res.Code != http.StatusBadRequest || client.calls != 1 {
			t.Fatal("invalid sessions forwarded")
		}
	}
	if res := request(http.MethodPost, "/api/market-tape", cookie); res.Code == http.StatusOK || client.calls != 1 {
		t.Fatal("tape accepted mutation")
	}
	client.err = errors.New("PRIVATE-BROKER-DIAGNOSTIC")
	res := request(http.MethodGet, "/api/market-tape", cookie)
	if res.Code != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), "PRIVATE") || client.params.Sessions != 20 {
		t.Fatal("failure leaked or default lost")
	}
}
