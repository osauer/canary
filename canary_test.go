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
