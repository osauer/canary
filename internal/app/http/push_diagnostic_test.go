package apphttp

import (
	"encoding/json"
	"net/http"
	"testing"

	appalerts "github.com/osauer/canary/v2/internal/app/alerts"
	"github.com/osauer/canary/v2/internal/app/relay"
	"github.com/osauer/canary/v2/internal/app/state"
)

func TestDiagnosticPushRouteIsLocalControlAndTheSafeTestNamesItsNotice(t *testing.T) {
	server := newTestHandlerWithDependencies(t, routeFakeClient{}, relay.Noop{PublicURL: "https://relay.example"}, func(deps *Dependencies) {
		deps.AlertController = &appalerts.Dispatcher{
			Store: deps.Store, Sender: routePushSender{}, URL: "https://relay.example",
			NewNoticeID: func() (string, error) { return "diagnostic-0123456789abcdef", nil },
		}
	})
	handler := server.Handler()
	if res := pushRouteRequest(t, handler, http.MethodPost, PushDiagnosticPath, "{}", nil, "192.0.2.10:4000"); res.Code != http.StatusForbidden {
		t.Fatalf("remote diagnostic status=%d, want 403", res.Code)
	}
	local := pushRouteRequest(t, handler, http.MethodPost, PushDiagnosticPath, "{}", nil, "127.0.0.1:4000")
	var result appalerts.DiagnosticResult
	if err := json.Unmarshal(local.Body.Bytes(), &result); err != nil || local.Code != http.StatusOK {
		t.Fatalf("local diagnostic status=%d body=%s err=%v", local.Code, local.Body.String(), err)
	}
	if result.Accepted || result.State != state.GovernanceTransportNoSubscription || len(result.Targets) != 0 {
		t.Fatalf("diagnostic without subscriptions = %+v", result)
	}
	if res := pushRouteRequest(t, handler, http.MethodPost, PushDiagnosticPath, `{"all":true}`, nil, "127.0.0.1:4000"); res.Code != http.StatusBadRequest {
		t.Fatalf("diagnostic with a body status=%d, want 400", res.Code)
	}

	cookie := routeSessionCookie(t, handler)
	if res := pushRouteRequest(t, handler, http.MethodPost, "/api/push/subscribe", `{"endpoint":"https://push.invalid/phone","keys":{"p256dh":"p","auth":"a"}}`, cookie, ""); res.Code != http.StatusOK {
		t.Fatalf("subscribe status=%d", res.Code)
	}
	test := pushRouteRequest(t, handler, http.MethodPost, "/api/push/test", "{}", cookie, "")
	var sent SafePushTestResult
	if err := json.Unmarshal(test.Body.Bytes(), &sent); err != nil || test.Code != http.StatusOK || !sent.PushServiceAccepted || sent.NoticeID != "diagnostic-0123456789abcdef" {
		t.Fatalf("safe test status=%d result=%+v err=%v", test.Code, sent, err)
	}
}
