package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestGatewayDiagnosticAbsenceNeverMeansDisabled(t *testing.T) {
	out := gatewayAPIHealth()
	applyGatewayDiagnostic(out, ibkrlib.APIConfigDiagnostic{State: "observed", ServerVersion: 219, Settings: &ibkrlib.APISettings{ReadOnly: new(false), RetainOrdersOnReconnect: new(true)}})
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"read_only":false`) || strings.Contains(string(raw), `"volume_in_lots"`) {
		t.Fatalf("lost observation presence: %s", raw)
	}
}

func TestGatewayDiagnosticsRequireExplicitRequestAndConnectedSession(t *testing.T) {
	srv := newTestServer(t)
	normal, err := srv.handleStatusHealthRequest(t.Context(), &rpc.Request{})
	if err != nil || normal.GatewayAPI == nil || normal.GatewayAPI.ConfigState != "not_requested" || normal.GatewayAPI.TradingProtocolMax != 203 {
		t.Fatalf("%+v %v", normal, err)
	}
	requested, err := srv.handleStatusHealthRequest(t.Context(), &rpc.Request{Params: json.RawMessage(`{"gateway_diagnostics":true}`)})
	if err != nil || requested.GatewayAPI.ConfigState != "unavailable" || requested.GatewayAPI.Reason != "not_connected" || requested.GatewayAPI.Settings != nil {
		t.Fatalf("%+v %v", requested.GatewayAPI, err)
	}
	if _, err := srv.handleStatusHealthRequest(t.Context(), &rpc.Request{Params: json.RawMessage(`{"gateway_diagnostics":"yes"}`)}); err == nil {
		t.Fatal("accepted malformed parameters")
	}
}
