package canary_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2"
	"github.com/osauer/canary/v2/canarytest"
)

func TestCallRunsTheCatalogueHandlerOverTheSocket(t *testing.T) {
	server := canarytest.Serve(t)
	server.Handle("status.health", canarytest.Result(map[string]any{"daemon_version": "v-test"}))
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	out, err := client.Call(t.Context(), "canary_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		DaemonVersion string `json:"daemon_version"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.DaemonVersion != "v-test" {
		t.Fatalf("daemon_version=%q out=%s", got.DaemonVersion, out)
	}
	if calls := server.Calls(); len(calls) != 1 || calls[0] != "status.health" {
		t.Fatalf("calls=%v", calls)
	}
}

func TestDataHealthPreservesAdditiveProducerEvidence(t *testing.T) {
	server := canarytest.Serve(t)
	// These fields deliberately have no SDK DTO members. Their numeric and
	// string lexemes also must not be normalized by an intermediate adapter.
	want := json.RawMessage(`{"schema_version":1,"revision":"synthetic","complete":false,"next_offset":48,"future_report":{"count":9007199254740993,"ratio":1.2300,"absent":null},"sources":[{"id":"synthetic","state":"limited","future_source":{"reason":"\u0061","signed_zero":-0.0,"evidence":[true,null,{"observed":false}]}}]}`)
	server.Handle("data.health", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Offset   int    `json:"offset"`
			Limit    int    `json:"limit"`
			Revision string `json:"revision"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Offset != 24 || p.Limit != 24 || p.Revision != "synthetic" {
			return nil, canarytest.Fail(canary.CodeBadRequest, "pagination parameters changed")
		}
		return want, nil
	})
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	out, err := client.Call(t.Context(), "canary_data_health", json.RawMessage(`{"offset":24,"limit":24,"revision":"synthetic"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(want) {
		t.Fatalf("health adapter changed additive producer evidence:\nwant %s\n got %s", want, out)
	}
	if calls := server.Calls(); len(calls) != 1 || calls[0] != "data.health" {
		t.Fatalf("passive health call dispatched unexpected methods: %v", calls)
	}
}

// The read tools Desk consumes carry the operating facts it audits: each
// order's journaled origin, the broker orders Canary never placed, the
// freeze and its control generation, and the risk constitution's version.
// The catalogue decodes into typed results before re-encoding, so a field
// the typed contract lacks would vanish here.
func TestReadToolsCarryOriginFreezeAndPolicyVersion(t *testing.T) {
	server := canarytest.Serve(t)
	server.Handle("orders.open", canarytest.Result(map[string]any{
		"orders":           []map[string]any{{"order_ref": "canary-a", "origin": "agent", "lifecycle_status": "submitted", "open": true}},
		"untracked":        []map[string]any{{"perm_id": 7777, "lifecycle_status": "submitted", "open": true}},
		"untracked_status": "current",
		"as_of":            "2026-09-25T14:00:00Z", "not_broker_statement": "local", "limitations": []string{},
	}))
	server.Handle("trading.status", canarytest.Result(map[string]any{"mode": "paper", "freeze": true, "trading_control_generation": 3}))
	server.Handle("brief.snapshot", canarytest.Result(map[string]any{
		"ready": map[string]any{"policy_drift": map[string]any{"status": "ok", "rows": []any{}, "policy_id": "risk-constitution", "policy_version": 4}},
	}))
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	call := func(tool string, into any) {
		t.Helper()
		out, err := client.Call(t.Context(), tool, nil)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if err := json.Unmarshal(out, into); err != nil {
			t.Fatalf("%s: %v in %s", tool, err, out)
		}
	}
	var orders struct {
		Orders []struct {
			Origin string `json:"origin"`
		} `json:"orders"`
		Untracked []map[string]any `json:"untracked"`
		Status    string           `json:"untracked_status"`
	}
	call("canary_orders_open", &orders)
	if len(orders.Orders) != 1 || orders.Orders[0].Origin != "agent" || len(orders.Untracked) != 1 || orders.Status != "current" {
		t.Fatalf("orders open = %+v", orders)
	}
	if _, ok := orders.Untracked[0]["origin"]; ok || orders.Untracked[0]["perm_id"] != float64(7777) {
		t.Fatalf("untracked row = %v", orders.Untracked[0])
	}
	var trading struct {
		Freeze     bool   `json:"freeze"`
		Generation uint64 `json:"trading_control_generation"`
	}
	call("canary_trading_status", &trading)
	if !trading.Freeze || trading.Generation != 3 {
		t.Fatalf("trading status = %+v", trading)
	}
	var brief struct {
		Ready struct {
			PolicyDrift struct {
				PolicyID      string `json:"policy_id"`
				PolicyVersion int    `json:"policy_version"`
			} `json:"policy_drift"`
		} `json:"ready"`
	}
	call("canary_brief", &brief)
	if brief.Ready.PolicyDrift.PolicyID != "risk-constitution" || brief.Ready.PolicyDrift.PolicyVersion != 4 {
		t.Fatalf("brief drift row = %+v", brief.Ready.PolicyDrift)
	}
}

func TestDataHealthPreservesArgumentAndDaemonErrors(t *testing.T) {
	server := canarytest.Serve(t)
	server.Handle("data.health", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, canarytest.Fail(canary.CodeBadRequest, "health revision expired; restart pagination")
	})
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	if _, err := client.Call(t.Context(), "canary_data_health", json.RawMessage(`{"offset":"invalid"}`)); err == nil {
		t.Fatal("invalid pagination arguments were accepted")
	}
	if calls := server.Calls(); len(calls) != 0 {
		t.Fatalf("invalid pagination dispatched a daemon call: %v", calls)
	}
	out, err := client.Call(t.Context(), "canary_data_health", json.RawMessage(`{"revision":"expired"}`))
	var reported *canary.Error
	if len(out) != 0 || !errors.As(err, &reported) || reported.Code != canary.CodeBadRequest || reported.Message != "health revision expired; restart pagination" {
		t.Fatalf("health adapter changed daemon failure: out=%s err=%v", out, err)
	}
}

func TestCallReportsDaemonFailuresByCode(t *testing.T) {
	server := canarytest.Serve(t)
	server.Handle("status.health", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, canarytest.Fail(canary.CodeGatewayUnavailable, "no gateway")
	})
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	_, err := client.Call(t.Context(), "canary_status", nil)
	var reported *canary.Error
	if !errors.As(err, &reported) || reported.Code != canary.CodeGatewayUnavailable || reported.Message != "no gateway" {
		t.Fatalf("err=%v", err)
	}
}

func TestCallRejectsNamesOutsideTheCatalogue(t *testing.T) {
	client := canary.New(canary.Options{SocketPath: filepath.Join(t.TempDir(), "none.sock")})
	_, err := client.Call(t.Context(), "canary_order_place", nil)
	if !errors.Is(err, canary.ErrUnknownTool) {
		t.Fatalf("err=%v", err)
	}
}

func TestMissingSocketWithoutAnExecutableIsUnavailable(t *testing.T) {
	client := canary.New(canary.Options{SocketPath: filepath.Join(t.TempDir(), "none.sock")})
	_, err := client.Call(t.Context(), "canary_status", nil)
	if !errors.Is(err, canary.ErrDaemonUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestMissingExecutableCannotStartADaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	client := canary.New(canary.Options{SocketPath: filepath.Join(dir, "none.sock"), Executable: filepath.Join(dir, "missing-canary")})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := client.Call(ctx, "canary_status", nil)
	if !errors.Is(err, canary.ErrDaemonUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestConcurrentCallsNeverQueueBehindEachOther(t *testing.T) {
	server := canarytest.Serve(t)
	release := make(chan struct{})
	server.Handle("status.health", func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`{"daemon_version":"slow"}`), nil
	})
	server.Handle("account.summary", canarytest.Result(map[string]any{}))
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	slow := make(chan error, 1)
	go func() {
		_, err := client.Call(t.Context(), "canary_status", nil)
		slow <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(server.Calls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the slow call never reached the daemon")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := client.Call(t.Context(), "canary_account", nil); err != nil {
		t.Fatalf("the fast call waited on the slow one: %v", err)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
}

func TestCallEndsAtTheCallerDeadline(t *testing.T) {
	server := canarytest.Serve(t)
	server.Handle("status.health", func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Call(ctx, "canary_status", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("the deadline was ignored for %s", waited)
	}
}

func TestSubscribeDeliversFramesUntilTheDaemonEnds(t *testing.T) {
	server := canarytest.Serve(t)
	server.HandleStream("display.subscribe", func(_ context.Context, _ json.RawMessage, emit func(json.RawMessage) error) error {
		for i := 1; i <= 3; i++ {
			if err := emit(json.RawMessage(fmt.Sprintf(`{"sequence":%d}`, i))); err != nil {
				return err
			}
		}
		return nil
	})
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	var frames []string
	err := client.Subscribe(t.Context(), func(frame json.RawMessage) error {
		frames = append(frames, string(frame))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[2] != `{"sequence":3}` {
		t.Fatalf("frames=%q", frames)
	}
}

func TestSubscribeStopsWhenTheCallbackRejectsAFrame(t *testing.T) {
	server := canarytest.Serve(t)
	server.HandleStream("display.subscribe", func(ctx context.Context, _ json.RawMessage, emit func(json.RawMessage) error) error {
		if err := emit(json.RawMessage(`{"sequence":1}`)); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	})
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	rejected := errors.New("invalid display envelope")
	err := client.Subscribe(t.Context(), func(json.RawMessage) error { return rejected })
	if !errors.Is(err, rejected) {
		t.Fatalf("err=%v", err)
	}
}

func TestSubscribeReportsARefusedSubscription(t *testing.T) {
	server := canarytest.Serve(t)
	server.HandleStream("display.subscribe", func(context.Context, json.RawMessage, func(json.RawMessage) error) error {
		return canarytest.Fail(canary.CodeBadRequest, "display reader limit or shutdown")
	})
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	err := client.Subscribe(t.Context(), func(json.RawMessage) error { return nil })
	var reported *canary.Error
	if !errors.As(err, &reported) || reported.Code != canary.CodeBadRequest {
		t.Fatalf("err=%v", err)
	}
}

func TestVersionReadsTheDaemonStamp(t *testing.T) {
	server := canarytest.Serve(t)
	server.Handle("status.health", canarytest.Result(map[string]any{"daemon_version": "v9.9.9"}))
	client := canary.New(canary.Options{SocketPath: server.SocketPath()})
	version, err := client.Version(t.Context())
	if err != nil || version != "v9.9.9" {
		t.Fatalf("version=%q err=%v", version, err)
	}
}

func TestToolsMirrorTheCatalogue(t *testing.T) {
	tools := canary.Tools()
	if len(tools) == 0 {
		t.Fatal("empty catalogue")
	}
	for _, tool := range tools {
		if tool.Name == "" || tool.Description == "" || len(tool.Methods) == 0 || !json.Valid(tool.InputSchema) {
			t.Fatalf("incomplete catalogue entry %+v", tool)
		}
		if !tool.ReadOnly {
			t.Fatalf("%s is not read-only", tool.Name)
		}
	}
	status, ok := canary.Lookup("canary_status")
	if !ok || len(status.Methods) != 1 || status.Methods[0] != "status.health" {
		t.Fatalf("status=%+v ok=%v", status, ok)
	}
	status.InputSchema[0] = ' '
	again, _ := canary.Lookup("canary_status")
	if !json.Valid(again.InputSchema) {
		t.Fatal("Lookup shares its schema bytes with callers")
	}
	if _, ok := canary.Lookup("canary_order_place"); ok {
		t.Fatal("a write tool is in the catalogue")
	}
}
