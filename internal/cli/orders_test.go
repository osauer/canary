package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRunOrdersHistoryForwardsParamsAndRendersJSON(t *testing.T) {
	t.Parallel()

	result := rpc.OrdersHistoryResult{
		Orders: []rpc.OrdersHistoryRow{
			{
				Order: rpc.OrderView{
					OrderRef:        "ibkr-20260618-130000",
					Account:         "U1234567",
					Mode:            rpc.AccountModeLive,
					Symbol:          "AMD",
					LifecycleStatus: rpc.OrderLifecycleSubmitted,
					UpdatedAt:       time.Date(2026, 6, 18, 13, 0, 0, 0, time.UTC),
				},
				Events: []rpc.OrderEvent{
					{
						At:      time.Date(2026, 6, 18, 13, 0, 0, 0, time.UTC),
						Type:    "status-updated",
						Account: "U1234567",
						Mode:    rpc.AccountModeLive,
						Symbol:  "AMD",
					},
				},
				EventsCount:      1,
				TotalEventsCount: 1,
			},
		},
		AsOf:               time.Date(2026, 6, 19, 10, 16, 0, 0, time.UTC),
		Since:              time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC),
		Until:              time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
		Account:            "U1234567",
		Mode:               rpc.AccountModeLive,
		Count:              1,
		TotalCount:         1,
		EventsCount:        1,
		TotalEventsCount:   1,
		Limit:              2,
		EventLimit:         3,
		NotBrokerStatement: "local order journal only; not an IBKR Activity Statement, trade confirmation, execution report, or historical broker audit",
		Limitations:        []string{"local journal only"},
	}
	conn, calls := startOrdersHistoryFakeConn(t, result)
	defer conn.Close()

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	code := Run(context.Background(), env, "orders", []string{
		"history",
		"--since", "2026-06-18",
		"--until", "2026-06-19",
		"--limit", "2",
		"--event-limit", "3",
		"--json",
	})
	if code != 0 {
		t.Fatalf("runOrdersHistory exit=%d stderr=%s", code, stderr.String())
	}

	call := <-calls
	if call.method != rpc.MethodOrdersHistory {
		t.Fatalf("method = %s, want %s", call.method, rpc.MethodOrdersHistory)
	}
	if call.params.Since != "2026-06-18" || call.params.Until != "2026-06-19" || call.params.Limit != 2 || call.params.EventLimit != 3 {
		t.Fatalf("params = %+v, want since/until/limit/event-limit forwarded", call.params)
	}
	out := stdout.String()
	for _, want := range []string{`"not_broker_statement"`, `"event_limit": 3`, `"orders"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("JSON output missing %q: %s", want, out)
		}
	}
}

func TestRunOrdersHistoryRejectsTrailingArgsBeforeDial(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := runOrdersHistory(context.Background(), env, []string{"history", "extra"})
	if code != 1 {
		t.Fatalf("runOrdersHistory exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "canary orders history") {
		t.Fatalf("stderr missing usage: %s", stderr.String())
	}
}

func TestRenderOrdersHistoryTextShowsLocalLimitations(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderOrdersHistoryText(env, &rpc.OrdersHistoryResult{
		Orders: []rpc.OrdersHistoryRow{
			{
				Order: rpc.OrderView{
					OrderRef:        "ibkr-20260618-130000",
					Symbol:          "AMD",
					Action:          rpc.OrderActionSell,
					OrderType:       rpc.OrderTypeTRAIL,
					TIF:             rpc.OrderTIFGTC,
					LifecycleStatus: rpc.OrderLifecycleSubmitted,
					UpdatedAt:       time.Date(2026, 6, 18, 13, 5, 0, 0, time.UTC),
				},
				Events: []rpc.OrderEvent{
					{At: time.Date(2026, 6, 18, 13, 0, 0, 0, time.UTC), Type: "previewed"},
				},
				EventsCount:      1,
				TotalEventsCount: 3,
				EventsTruncated:  true,
			},
		},
		Since:              time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC),
		Until:              time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
		Account:            "U1234567",
		Mode:               rpc.AccountModeLive,
		Count:              1,
		EventsCount:        1,
		TotalEventsCount:   3,
		EventLimit:         1,
		EventsTruncated:    true,
		NotBrokerStatement: "local order journal only; not an IBKR Activity Statement",
		Limitations:        []string{"Broker callbacks remain authoritative when journaled."},
	})
	out := stdout.String()
	for _, want := range []string{"Source", "showing 1 of 3 events", "Limitations:", "not an IBKR Activity Statement"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text output missing %q: %s", want, out)
		}
	}
}

func TestRenderOrdersOpenTextShowsScopeAndLimitations(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderOrdersOpenText(env, &rpc.OrdersOpenResult{
		Orders:             []rpc.OrderView{},
		Account:            "U1234567",
		Mode:               rpc.AccountModeLive,
		LastLocalEventAt:   time.Date(2026, 6, 18, 13, 0, 0, 0, time.UTC),
		NotBrokerStatement: "local order journal only; not an IBKR Activity Statement",
		Limitations:        []string{"local journal only"},
	})
	out := stdout.String()
	for _, want := range []string{"Scope", "U1234567 live", "Latest local event", "Source", "not an IBKR Activity Statement", "Limitations:", "local journal only"} {
		if !strings.Contains(out, want) {
			t.Fatalf("open text missing %q: %s", want, out)
		}
	}
}

func TestRenderOrderStatusTextNotFoundShowsScopeAndLimitations(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderOrderStatusText(env, &rpc.OrderStatusResult{
		Found:              false,
		Account:            "DU1234567",
		Mode:               rpc.AccountModePaper,
		LastLocalEventAt:   time.Date(2026, 6, 18, 13, 0, 0, 0, time.UTC),
		NotBrokerStatement: "local order journal only; not an IBKR Activity Statement",
		Limitations:        []string{"local journal only"},
	}, "missing-order")
	out := stdout.String()
	for _, want := range []string{"NOT FOUND", "DU1234567 paper", "Latest local event", "Source", "Limitations:", "No locally tracked order matched missing-order"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status text missing %q: %s", want, out)
		}
	}
}

type ordersHistoryCall struct {
	method string
	params rpc.OrdersHistoryParams
}

func startOrdersHistoryFakeConn(t *testing.T, result rpc.OrdersHistoryResult) (*dial.Conn, <-chan ordersHistoryCall) {
	t.Helper()

	socketPath := filepath.Join("/tmp", fmt.Sprintf("ibkr-cli-orders-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	calls := make(chan ordersHistoryCall, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var req rpc.Request
		if err := json.NewDecoder(c).Decode(&req); err != nil {
			return
		}
		var params rpc.OrdersHistoryParams
		_ = json.Unmarshal(req.Params, &params)
		calls <- ordersHistoryCall{method: req.Method, params: params}
		raw, _ := json.Marshal(result)
		_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
	}()

	conn, err := dial.Connect(socketPath)
	if err != nil {
		t.Fatalf("connect fake daemon: %v", err)
	}
	return conn, calls
}
