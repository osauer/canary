package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/dial"
	"github.com/osauer/canary/v3/internal/rpc"
)

// TestMain lets a test run canary-sim itself as a child process.
func TestMain(m *testing.M) {
	if args := os.Getenv("CANARY_SIM_TEST_CHILD"); args != "" {
		os.Exit(run(strings.Fields(args), os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// canary-sim writes only under -dir. The child runs with HOME and every XDG
// root pointed at a sentinel "operator home"; it starts, answers a status
// and an add plan over its socket and a control command, and stops. The
// sentinel stays empty.
func TestCanarySimWritesOnlyUnderItsDirectory(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "csimt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	operator, stackDir := filepath.Join(root, "op"), filepath.Join(root, "s")
	if err := os.MkdirAll(operator, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = []string{
		"CANARY_SIM_TEST_CHILD=-dir " + stackDir, "PATH=/usr/bin:/bin",
		"HOME=" + operator, "XDG_CONFIG_HOME=" + filepath.Join(operator, "config"), "XDG_STATE_HOME=" + filepath.Join(operator, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(operator, "cache"), "XDG_DATA_HOME=" + filepath.Join(operator, "data"), "XDG_RUNTIME_DIR=" + filepath.Join(operator, "run"),
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	socket := filepath.Join(stackDir, "ibkr.sock")
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no socket:\n%s", out.String())
		}
	}
	call := func(method string, params, result any) error {
		conn, err := dial.Connect(socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return conn.Call(ctx, method, params, result)
	}
	var health rpc.HealthResult
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if err := call(rpc.MethodStatusHealth, rpc.HealthParams{}, &health); err == nil && health.Connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not connected: %+v\n%s", health, out.String())
		}
	}
	var plan rpc.AddPlanResult
	if err := call(rpc.MethodAddPlan, rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNA", Currency: "USD"}, LimitPrice: 50.10, Quantity: 10}, &plan); err != nil || len(plan.Blockers) > 0 || plan.Quantity != 10 {
		t.Fatalf("add.plan: %v %+v", err, plan.Blockers)
	}
	var ctlOut, ctlErr bytes.Buffer
	if code := runCtl([]string{"-dir", stackDir, "frames"}, &ctlOut, &ctlErr); code != 0 {
		t.Fatalf("ctl frames: %d %s", code, ctlErr.String())
	}
	var frames struct {
		Next   int               `json:"next"`
		Frames []json.RawMessage `json:"frames"`
	}
	if err := json.Unmarshal(ctlOut.Bytes(), &frames); err != nil || frames.Next == 0 || !bytes.Contains(ctlOut.Bytes(), []byte(`"placeOrder"`)) {
		t.Fatalf("ctl frames: %v %d", err, frames.Next)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("canary-sim did not stop")
	}
	var written []string
	_ = filepath.WalkDir(operator, func(path string, e fs.DirEntry, err error) error {
		if err == nil && path != operator {
			written = append(written, path)
		}
		return nil
	})
	if len(written) > 0 {
		t.Fatalf("canary-sim wrote outside its directory: %v", written)
	}
}
