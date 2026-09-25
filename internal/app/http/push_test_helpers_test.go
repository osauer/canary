package apphttp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/push"
	"github.com/osauer/canary/v2/internal/app/state"
)

type routePushSender struct{}

func (routePushSender) Send(_ context.Context, sub state.PushSubscription, _ state.VAPIDKeys, _ push.Payload) state.PushAttempt {
	return state.PushAttempt{At: time.Now().UTC(), SubscriptionID: sub.ID, OK: true, StatusCode: 201, Status: "201 Created", Class: state.GovernanceTransportAccepted}
}

func pushRouteRequest(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

// pushRouteJournaledNotice subscribes the paired session's device and
// journals one accepted push to it, as the dispatcher would.
func pushRouteJournaledNotice(t *testing.T, handler http.Handler, store *state.Store, cookie *http.Cookie, noticeID, kind string) {
	t.Helper()
	if res := pushRouteRequest(t, handler, http.MethodPost, "/api/push/subscribe", `{"endpoint":"https://push.invalid/phone","keys":{"p256dh":"p","auth":"a"}}`, cookie, ""); res.Code != http.StatusOK {
		t.Fatalf("subscribe status=%d body=%s", res.Code, res.Body.String())
	}
	subs := store.PushSubscriptions()
	if len(subs) != 1 {
		t.Fatalf("subscriptions = %d", len(subs))
	}
	if err := store.RecordPushAttempt(kind, noticeID, subs[0], state.PushAttempt{At: time.Now().UTC(), OK: true, StatusCode: 201, Class: state.GovernanceTransportAccepted}); err != nil {
		t.Fatal(err)
	}
}
