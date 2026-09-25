package apphttp

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	appalerts "github.com/osauer/canary/v2/internal/app/alerts"
	"github.com/osauer/canary/v2/internal/app/relay"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestPushReceiptIsWitnessedOnlyFromTheAuthenticatedDevice(t *testing.T) {
	var store *state.Store
	server := newTestHandlerWithDependencies(t, routeFakeClient{}, relay.Noop{PublicURL: "https://relay.example"}, func(deps *Dependencies) {
		store = deps.Store
		deps.AlertController = &appalerts.Dispatcher{Store: deps.Store, Sender: routePushSender{}, URL: "https://relay.example"}
	})
	handler := server.Handler()
	cookie := routeSessionCookie(t, handler)
	const notice = "alert-0123456789abcdef"
	pushRouteJournaledNotice(t, handler, store, cookie, notice, rpc.PushNoticeKindAlert)
	if proof := store.PushDeliveryProof(time.Now()); proof.Witnessed {
		t.Fatalf("push-service acceptance read as witnessed: %+v", proof)
	}

	receipt := `{"notice_id":"alert-0123456789abcdef","event":"displayed","at":"2026-09-26T07:00:01.5Z"}`
	for name, tc := range map[string]struct {
		body   string
		cookie *http.Cookie
		want   int
	}{
		"no session":          {receipt, nil, http.StatusUnauthorized},
		"another device":      {`{"notice_id":"alert-0123456789abcdef","event":"displayed","device_id":"someone-else"}`, cookie, http.StatusForbidden},
		"unsent notice":       {`{"notice_id":"alert-ffffffffffffffff","event":"displayed"}`, cookie, http.StatusNotFound},
		"unknown event":       {`{"notice_id":"alert-0123456789abcdef","event":"read"}`, cookie, http.StatusBadRequest},
		"hostile notice id":   {`{"notice_id":"../api/devices","event":"opened"}`, cookie, http.StatusBadRequest},
		"unknown field":       {`{"notice_id":"alert-0123456789abcdef","event":"opened","witnessed":true}`, cookie, http.StatusBadRequest},
		"malformed timestamp": {`{"notice_id":"alert-0123456789abcdef","event":"opened","at":"yesterday"}`, cookie, http.StatusBadRequest},
	} {
		if res := pushRouteRequest(t, handler, http.MethodPost, PushAckPath, tc.body, tc.cookie, ""); res.Code != tc.want {
			t.Errorf("%s: status=%d want %d body=%s", name, res.Code, tc.want, res.Body.String())
		}
	}

	first := pushRouteRequest(t, handler, http.MethodPost, PushAckPath, receipt, cookie, "")
	var recorded PushAckResult
	if err := json.Unmarshal(first.Body.Bytes(), &recorded); err != nil || first.Code != http.StatusOK || !recorded.Recorded || recorded.Kind != rpc.PushNoticeKindAlert {
		t.Fatalf("receipt status=%d result=%+v err=%v", first.Code, recorded, err)
	}
	again := pushRouteRequest(t, handler, http.MethodPost, PushAckPath, receipt, cookie, "")
	var duplicate PushAckResult
	if err := json.Unmarshal(again.Body.Bytes(), &duplicate); err != nil || again.Code != http.StatusOK || duplicate.Recorded || !duplicate.ReceivedAt.Equal(recorded.ReceivedAt) {
		t.Fatalf("duplicate receipt status=%d result=%+v", again.Code, duplicate)
	}
	proof := store.PushDeliveryProof(time.Now())
	if !proof.Witnessed || proof.LastDisplayed == nil || proof.LastDisplayed.Device != "iPhone" || proof.LastDisplayed.Kind != rpc.PushNoticeKindAlert {
		t.Fatalf("witnessed proof = %+v", proof)
	}
}
