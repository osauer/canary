package apphttp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	appalerts "github.com/osauer/canary/v2/internal/app/alerts"
	"github.com/osauer/canary/v2/internal/app/relay"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestAlertsFeedAndAppStatusCarryThePushDeliveryProof(t *testing.T) {
	var store *state.Store
	server := newTestHandlerWithDependencies(t, routeFakeClient{}, relay.Noop{PublicURL: "https://relay.example"}, func(deps *Dependencies) {
		store = deps.Store
		deps.AlertController = &appalerts.Dispatcher{Store: deps.Store, Sender: routePushSender{}, URL: "https://relay.example"}
	})
	handler := server.Handler()
	cookie := routeSessionCookie(t, handler)
	feed := func() AlertPushDeliveryDTO {
		t.Helper()
		res := pushRouteRequest(t, handler, http.MethodGet, "/api/alerts", "", cookie, "")
		var dto AlertDTO
		if err := json.Unmarshal(res.Body.Bytes(), &dto); err != nil || res.Code != http.StatusOK {
			t.Fatalf("alerts status=%d err=%v", res.Code, err)
		}
		if strings.Contains(res.Body.String(), "push.invalid") || strings.Contains(res.Body.String(), "device_ref") {
			t.Fatalf("alerts feed leaked subscription or device identity: %s", res.Body.String())
		}
		return dto.PushDelivery
	}
	if empty := feed(); empty.Witnessed || empty.LastSent != nil || empty.SilentSince != nil {
		t.Fatalf("empty push delivery = %+v", empty)
	}

	const notice = "diagnostic-0123456789abcdef"
	pushRouteJournaledNotice(t, handler, store, cookie, notice, rpc.PushNoticeKindDiagnostic)
	sent := feed()
	if sent.Witnessed || sent.LastSent == nil || sent.LastSent.Kind != rpc.PushNoticeKindDiagnostic || sent.LastSent.HTTPStatus != 201 || sent.ActiveSubscriptions != 1 {
		t.Fatalf("sent push delivery = %+v", sent)
	}
	if res := pushRouteRequest(t, handler, http.MethodPost, PushAckPath, `{"notice_id":"`+notice+`","event":"opened"}`, cookie, ""); res.Code != http.StatusOK {
		t.Fatalf("receipt status=%d", res.Code)
	}
	opened := feed()
	if !opened.Witnessed || opened.LastOpened == nil || opened.LastOpened.Device != "iPhone" || opened.LastOpened.Kind != rpc.PushNoticeKindDiagnostic {
		t.Fatalf("opened push delivery = %+v", opened)
	}

	status := pushRouteRequest(t, handler, http.MethodGet, AppStatusPath, "", nil, "127.0.0.1:4000")
	var dto AppStatusDTO
	if err := json.Unmarshal(status.Body.Bytes(), &dto); err != nil || status.Code != http.StatusOK {
		t.Fatalf("app status=%d err=%v", status.Code, err)
	}
	if err := rpc.ValidatePushDeliveryProof(dto.PushDelivery); err != nil || !dto.PushDelivery.Witnessed || len(dto.PushDelivery.Recent) != 1 {
		t.Fatalf("app status proof = %+v, %v", dto.PushDelivery, err)
	}
}
