package daemon

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// An OpenD daemon that loses its session says so with the code every surface
// already reads, and names the gateway the reader must look at. Each path here
// used to say "IBKR connection unavailable" or fall through to an internal error.
func TestALostOpenDSessionNamesOpenD(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	if got := srv.brokerLinkNotReady(); !strings.Contains(got, "does not send orders") {
		t.Errorf("readiness blocker with the session up: %s", got)
	}
	sim.Close()
	deadline := time.Now().Add(5 * time.Second)
	for srv.opend.Connector().IsReady() {
		if time.Now().After(deadline) {
			t.Fatal("the session did not drop")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, tc := range []struct{ method, params string }{
		{rpc.MethodQuoteSnapshot, `{"contract":{"symbol":"AAPL"},"timeout_ms":500}`},
		{rpc.MethodQuoteSnapshot, `{"contract":{"instrument":{"broker":"opend","id":"US.AAPL"}}}`},
		{rpc.MethodMarketHistory, `{"contract":{"symbol":"NEVERREAD","exchange":"US","currency":"USD"},"range":"1M"}`},
		{rpc.MethodPositionsList, `{}`},
		{rpc.MethodAccountSummary, `{}`},
	} {
		res := dispatchRaw(t, srv, tc.method, tc.params)
		if res.Ok {
			t.Errorf("%s %s answered with the session down: %s", tc.method, tc.params, res.Result)
			continue
		}
		if res.Error.Code != rpc.CodeGatewayUnavailable || !strings.Contains(res.Error.Message, "OpenD") || strings.Contains(res.Error.Message, "IBKR") {
			t.Errorf("%s %s: %s: %s", tc.method, tc.params, res.Error.Code, res.Error.Message)
		}
	}
	if got := srv.brokerLinkNotReady(); got != "the broker link to OpenD is not ready" {
		t.Errorf("readiness blocker with the session down: %s", got)
	}
}

// The errors the client returns when the session drops mid-read take the same code.
func TestOpenDSessionErrorsClassifyAsUnavailable(t *testing.T) {
	for _, err := range []error{futu.ErrNotConnected, futu.ErrSessionEnded, futu.ErrNoScope, fmt.Errorf("read: %w", futu.ErrNotConnected)} {
		code, msg := classifyError(err)
		if code != rpc.CodeGatewayUnavailable || !strings.HasPrefix(msg, "OpenD connection unavailable: ") {
			t.Errorf("%v: %s: %s", err, code, msg)
		}
	}
}

// Everything that reads the sentinel to join an outage still sees it under OpenD,
// and IBKR's own words do not change.
func TestTheUnavailableErrorKeepsItsMeaningOnBothBrokers(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	opend, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	err := opend.brokerUnavailable("detail")
	if !errors.Is(err, ibkrlib.ErrIBKRUnavailable) || err.Error() != "OpenD connection unavailable: detail" {
		t.Fatalf("OpenD: %v", err)
	}
	plain := &Server{}
	if err := plain.gatewayUnavailableError(); !errors.Is(err, ibkrlib.ErrIBKRUnavailable) || err.Error() != "IBKR connection unavailable" {
		t.Fatalf("IBKR: %v", err)
	}
	if got := plain.brokerLinkNotReady(); got != "the broker link to IBKR Gateway/TWS is not ready" {
		t.Fatalf("IBKR readiness: %s", got)
	}
}
