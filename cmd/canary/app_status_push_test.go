package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRenderAppStatusShowsThePushEvidence(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	silentSince := time.Date(2026, 8, 11, 13, 42, 47, 0, time.UTC)
	var out bytes.Buffer
	renderAppPushDelivery(&out, rpc.PushDeliveryProof{
		SchemaVersion: rpc.PushDeliveryProofVersion, ReportedAt: now, Mode: "act_only", Dispatcher: "healthy",
		ActiveSubscriptions: 3, SilentSince: &silentSince, Recent: []rpc.PushNoticeFact{},
		LastSent: &rpc.PushSendFact{At: silentSince, Kind: rpc.PushNoticeKindAlert, NoticeID: "alert-0123456789abcdef", Class: "push_service_accepted", Accepted: true},
	}, now)
	for _, want := range []string{"Phone push        silent since", "never witnessed on a device", "Last push sent", "alert-0123456789abcdef (push_service_accepted)", "Subscriptions     3 active, notification mode act_only"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("app status lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	renderAppPushDelivery(&out, rpc.PushDeliveryProof{}, now)
	if out.Len() != 0 {
		t.Fatalf("an older app host without a proof rendered push lines:\n%s", out.String())
	}
}
