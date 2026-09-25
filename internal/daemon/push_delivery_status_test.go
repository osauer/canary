package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// status.health carries the app host's latest delivery proof, so `canary
// status`, MCP and Desk read one record; it never invents one.
func TestStatusHealthCarriesThePushDeliveryProof(t *testing.T) {
	t.Parallel()
	if got := newTestServer(t).statusHealthSnapshot().PushDelivery; got != nil {
		t.Fatalf("status invented a proof no app host reported: %+v", got)
	}
	s := newTestServer(t)
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	raw, _ := json.Marshal(testPushDeliveryProof(now))
	if _, err := s.handleAlertDeliveryProof(t.Context(), &rpc.Request{Method: rpc.MethodAlertDeliveryProof, Params: raw}); err != nil {
		t.Fatal(err)
	}
	got := s.statusHealthSnapshot().PushDelivery
	if got == nil || !got.Proof.Witnessed || !got.ReceivedAt.Equal(now) || got.Proof.LastOpened == nil || got.Proof.LastOpened.Device != "iPhone" {
		t.Fatalf("status push delivery = %+v", got)
	}
}
