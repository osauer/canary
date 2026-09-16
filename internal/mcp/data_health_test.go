package mcp

import (
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

// One daemon response must survive every consumer adapter without rebuilding
// counts, source clocks, delayed labels or access conclusions.
func TestDataHealthConsumerParity(t *testing.T) {
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)
	want := rpc.DataHealthResult{SchemaVersion: 1, Revision: "synthetic", AsOf: now, ValidUntil: now.Add(time.Minute), ScopeState: "current", Complete: true,
		Summary: rpc.DataHealthSummary{State: "limited", Label: "1 data problem · 0 unverified", Total: 1, Required: 1, Limited: 1, Problems: 1},
		Sources: []rpc.DataSourceHealth{{ID: "ibkr:quotes", Name: "IBKR quote feed", Required: true, State: "limited", Receiving: "Delayed · last session", DataType: "delayed-frozen", SourceTimeKind: "unknown", ReceivedAt: now,
			Access: &rpc.DataAccessObservation{Code: 354, Reason: "not_subscribed", ObservedAt: now, RetryAt: now.Add(30 * time.Minute)}}},
	}
	raw, _ := json.Marshal(want)
	socket := filepath.Join(t.TempDir(), "health.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requests := make(chan rpc.Request, 8)
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer c.Close()
				var req rpc.Request
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				requests <- req
				_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
			}()
		}
	}()
	connect := func() *dial.Conn {
		t.Helper()
		conn, err := dial.Connect(socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	assertReport := func(raw []byte) {
		t.Helper()
		var got rpc.DataHealthResult
		if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("adapter changed producer evidence: %v %+v", err, got)
		}
	}
	tool, ok := lookupTool("canary_data_health")
	if !ok {
		t.Fatal("missing health tool")
	}
	got, err := tool.Handler(t.Context(), connect(), json.RawMessage(`{"offset":0,"limit":24,"revision":"synthetic"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertReport(got)
	for _, jsonOut := range []bool{true, false} {
		var out, errs bytes.Buffer
		args := []string{"health", "--offset", "0", "--limit", "24", "--revision", "synthetic"}
		if jsonOut {
			args = append(args, "--json")
		}
		exit := cli.Run(t.Context(), &cli.Env{Stdout: &out, Stderr: &errs, Conn: connect()}, "data", args)
		if exit != 0 {
			t.Fatalf("CLI exit %d: %s", exit, errs.String())
		}
		if jsonOut {
			assertReport(out.Bytes())
		} else if !strings.Contains(out.String(), want.Summary.Label) || !strings.Contains(out.String(), "Delayed · last session") || !strings.Contains(out.String(), "IBKR 354") {
			t.Fatal("CLI lost producer assessment")
		}
	}
	appResult, err := (daemonclient.Real{SocketPath: socket}).DataHealth(t.Context(), rpc.DataHealthParams{Limit: 24, Revision: "synthetic"})
	if err != nil || !reflect.DeepEqual(appResult, &want) {
		t.Fatalf("app adapter lost producer report: %v", err)
	}
	for range 4 {
		req := <-requests
		var p rpc.DataHealthParams
		if json.Unmarshal(req.Params, &p) != nil || req.Method != rpc.MethodDataHealth || p != (rpc.DataHealthParams{Limit: 24, Revision: "synthetic"}) {
			t.Fatalf("passive adapter dispatched unexpected call: %s %s", req.Method, req.Params)
		}
	}
}
