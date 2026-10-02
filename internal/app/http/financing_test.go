package apphttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

type financingRouteClient struct {
	routeFakeClient
	calls  int
	params rpc.FinancingFeesParams
	err    error
}

func (c *financingRouteClient) FinancingFees(_ context.Context, params rpc.FinancingFeesParams) (*rpc.FinancingFeesResult, error) {
	c.calls++
	c.params = params
	from := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	return &rpc.FinancingFeesResult{ConID: params.ConID, Summary: rpc.FinancingSummary{SchemaVersion: rpc.FinancingSchemaVersion, State: rpc.FinancingComplete, From: from, To: from.AddDate(0, 0, 2), BaseCurrency: "EUR", EarnedBase: new(0.0), KnownEarnedBase: new(0.0), Native: []rpc.FinancingCurrencyAmount{}, CoveredDays: 2, ExpectedDays: 2, PNLReconciliation: "unproved", PaymentLinkage: "unavailable", Fingerprint: "finance_" + strings.Repeat("a", 32)}, Fees: []rpc.FinancingFee{}}, c.err
}

func TestFinancingRouteReadAuthorityBoundsAndRedaction(t *testing.T) {
	client := &financingRouteClient{}
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
	if res := request("GET", "/api/financing/fees", nil); res.Code != http.StatusUnauthorized || client.calls != 0 {
		t.Fatal("unpaired financing read")
	}
	cookie := routeSessionCookie(t, handler)
	if res := request("GET", "/api/financing/fees?con_id=900901&limit=1&from=2026-09-29&to=2026-10-01", cookie); res.Code != http.StatusOK || client.params.ConID != 900901 || client.params.Limit != 1 || res.Header().Get("Cache-Control") != "no-store" || !strings.Contains(res.Body.String(), `"earned_base":0`) {
		t.Fatalf("typed fee read lost: %d %s", res.Code, res.Body.String())
	}
	for _, query := range []string{"limit=101", "limit=-1", "con_id=-1", "con_id=hello", "window=all", "from=2026-09-29", "from=2026-02-30&to=2026-10-01", "limit=1&limit=2", "unknown=true", "fingerprint=private"} {
		if res := request("GET", "/api/financing/fees?"+query, cookie); res.Code != http.StatusBadRequest || client.calls != 1 {
			t.Fatalf("ambiguous read forwarded: %s %d", query, res.Code)
		}
	}
	if res := request("POST", "/api/financing/fees", cookie); res.Code == http.StatusOK || client.calls != 1 {
		t.Fatal("financing mutation accepted")
	}
	client.err = errors.New("PRIVATE-BROKER-LOAN-ACCOUNT")
	if res := request("GET", "/api/financing/fees", cookie); res.Code != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), "PRIVATE") {
		t.Fatal("private diagnostics exposed")
	}
}
