package alerts

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestConnectionCopyNamesTheBrokersProcess(t *testing.T) {
	for _, tc := range []struct {
		broker, want, not string
	}{{"", "TWS", "OpenD"}, {"ibkr", "TWS", "OpenD"}, {"opend", "OpenD", "TWS"}} {
		got, ok := PresentationForBroker(rpc.AlertPresentationDataHealthGateway, rpc.AlertEpisodeOpen, tc.broker)
		if !ok || !strings.Contains(got.Title, tc.want) || !strings.Contains(got.Body, tc.want) || strings.Contains(got.Title+got.Body, tc.not) {
			t.Errorf("%q: %+v", tc.broker, got)
		}
	}
	if _, ok := PresentationForBroker("no-such-code", rpc.AlertEpisodeOpen, "opend"); ok {
		t.Error("an unknown code read as a presentation")
	}
}
