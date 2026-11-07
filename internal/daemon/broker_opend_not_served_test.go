package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

// With OpenD up, no method says the broker is down. A read that needs the IBKR
// connector answers that this broker does not serve it; before, it answered
// "OpenD connection unavailable" and sent the reader to a working OpenD.
func TestOpenDUpNeverAnswersGatewayUnavailable(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)

	type answer struct {
		method, params string
		res            rpc.Response
	}
	var (
		mu      sync.Mutex
		answers []answer
		wg      sync.WaitGroup
	)
	slots := make(chan struct{}, 8)
	for _, timing := range rpc.MethodTimings() {
		if timing.Lifetime == rpc.MethodLifetimeStreaming {
			continue
		}
		probes := []string{`{}`, `{"symbol":"AAPL"}`}
		if exact, ok := map[string]string{
			rpc.MethodAddMarket:     `{"underlying":{"symbol":"AAPL","sec_type":"STK","currency":"USD","exchange":"SMART"}}`,
			rpc.MethodSetupsOptions: `{"underlying":{"con_id":265598,"symbol":"AAPL","sec_type":"STK","currency":"USD","exchange":"SMART","primary_exchange":"NASDAQ"}}`,
			rpc.MethodMarketBond:    `{"identifier":"037833100"}`,
		}[timing.Method]; ok {
			probes = append(probes, exact)
		}
		for _, params := range probes {
			wg.Go(func() {
				slots <- struct{}{}
				defer func() { <-slots }()
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				var out bytes.Buffer
				srv.dispatch(ctx, &rpc.Request{ID: "not-served", Method: timing.Method, Params: json.RawMessage(params)},
					json.NewEncoder(&out), bufio.NewReader(strings.NewReader("")))
				var res rpc.Response
				if err := json.Unmarshal(out.Bytes(), &res); err != nil {
					t.Errorf("%s %s: %q: %v", timing.Method, params, out.String(), err)
					return
				}
				mu.Lock()
				answers = append(answers, answer{timing.Method, params, res})
				mu.Unlock()
			})
		}
	}
	wg.Wait()

	if !srv.opend.Connector().IsReady() {
		t.Fatal("the OpenD session dropped during the run, so the answers prove nothing")
	}
	for _, a := range answers {
		if a.res.Error != nil && a.res.Error.Code == rpc.CodeGatewayUnavailable {
			t.Errorf("%s %s with OpenD up: %s %q", a.method, a.params, a.res.Error.Code, a.res.Error.Message)
		}
	}
}

// A read OpenD serves still reports a real outage as one.
func TestOpenDDownStillAnswersGatewayUnavailable(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	sim.Close()
	deadline := time.Now().Add(5 * time.Second)
	for srv.opend.Connector().IsReady() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	res := dispatchRaw(t, srv, rpc.MethodPositionsList, `{}`)
	if res.Ok || res.Error == nil || res.Error.Code != rpc.CodeGatewayUnavailable || !strings.Contains(res.Error.Message, "OpenD") {
		t.Fatalf("positions with OpenD down: %+v", res.Error)
	}
}
