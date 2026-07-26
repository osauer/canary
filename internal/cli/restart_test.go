package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/update"
)

func testCurrentExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	return path
}

func TestProductionDaemonRestartDepsForExecutableUsesRequestedBinary(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ibkr-restart-exact-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("CANARY_LOG", filepath.Join(dir, "daemon.log"))

	target := filepath.Join(dir, "missing-installed-ibkr")
	deps := productionDaemonRestartDepsForExecutable(target)
	_, _, err = deps.startAndHealth(context.Background(), filepath.Join(dir, "s"), io.Discard, true)
	if err == nil {
		t.Fatal("exact executable start unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), target) {
		t.Fatalf("start error = %q, want requested executable %q", err, target)
	}
}

func TestRunRestartCoreStartsWhenNoDaemonWasRunning(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	started := false
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			started = true
			return 42, rpc.HealthResult{DaemonVersion: "test", Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 4001, ClientID: 15}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if !started {
		t.Fatal("daemon was not started")
	}
	if !strings.Contains(out.String(), "no daemon was running") || !strings.Contains(out.String(), "started daemon pid 42") {
		t.Fatalf("output missing start message:\n%s", out.String())
	}
}

func TestRunRestartCoreRestartsGracefullyWithJSON(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	stoppedPID := 0
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 11, Command: "/tmp/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			stoppedPID = pid
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 12, rpc.HealthResult{DaemonVersion: "test", Connected: false, LastError: "no gateway"}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if stoppedPID != 11 {
		t.Fatalf("stoppedPID = %d, want 11", stoppedPID)
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "restarted" || !res.WasRunning || !res.Graceful || res.OldPID != 11 || res.NewPID != 12 {
		t.Fatalf("result = %+v", res)
	}
	if res.Health.LastError != "no gateway" {
		t.Fatalf("health = %+v", res.Health)
	}
}

func TestRunRestartCoreReportsStoppedBeforeHealthWait(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")

	var combined bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &combined, err: &combined}
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 15, Command: "/tmp/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			return nil
		},
		startAndHealth: func(_ context.Context, _ string, progress io.Writer, _ bool) (int, rpc.HealthResult, error) {
			fmt.Fprintln(progress, "health wait")
			return 16, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d\n%s", exit, combined.String())
	}
	got := combined.String()
	stopped := strings.Index(got, "stopped daemon pid 15 gracefully")
	wait := strings.Index(got, "health wait")
	started := strings.Index(got, "started daemon pid 16")
	if stopped < 0 || wait < 0 || started < 0 {
		t.Fatalf("missing expected output:\n%s", got)
	}
	if strings.Contains(got, "stopping daemon pid 15 gracefully") {
		t.Fatalf("graceful restart should not print duplicate stop progress and confirmation lines:\n%s", got)
	}
	if stopped > wait || wait > started {
		t.Fatalf("output order is wrong:\n%s", got)
	}
}

func TestRunRestartCoreForceEscalatesOnlyAfterGracefulTimeout(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{force: true, timeout: time.Second, out: &out, err: &errBuf}
	killedPID := 0
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 21, Command: "/tmp/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			return fmt.Errorf("wrapped: %w", update.ErrStopTimeout)
		},
		kill: func(pid int, _ time.Duration) error {
			killedPID = pid
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 22, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if killedPID != 21 {
		t.Fatalf("killedPID = %d, want 21", killedPID)
	}
	if !strings.Contains(out.String(), "forcing SIGKILL") {
		t.Fatalf("output missing force message:\n%s", out.String())
	}
}

func TestRunRestartCoreTimeoutWithoutForceFails(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 31, Command: "/tmp/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			return fmt.Errorf("wrapped: %w", update.ErrStopTimeout)
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			t.Fatal("startAndHealth should not run after non-forced timeout")
			return 0, rpc.HealthResult{}, nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "--force") {
		t.Fatalf("stderr missing force hint:\n%s", errBuf.String())
	}
}

func TestRunRestartAllCoreRestartsDaemonAndRunningApp(t *testing.T) {
	// Empty = default daemon scope: a set CANARY_SOCKET makes plain restart
	// skip app management entirely, which is tested separately below. All
	// deps are fakes, so the default scope touches nothing real.
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	daemonStopped := 0
	appStopped := 0
	appStartCalled := false
	var events []string
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			events = append(events, "daemon.find")
			return update.DaemonProcess{PID: 41, Command: "/tmp/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			events = append(events, "daemon.stop")
			daemonStopped = pid
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			events = append(events, "daemon.start")
			return 42, rpc.HealthResult{DaemonVersion: "test", Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7496, ClientID: 15}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			events = append(events, "app.find")
			return appProcess{
				PID:     51,
				Command: "/tmp/canary app --remote",
				Args:    []string{"app", "--remote"},
			}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			events = append(events, "app.stop")
			appStopped = pid
			return nil
		},
		start: func(_ context.Context, args []string) (int, error) {
			events = append(events, "app.start")
			appStartCalled = true
			if !slices.Equal(args, []string{"app", "--remote"}) {
				t.Fatalf("app restart args = %q", args)
			}
			return 52, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if daemonStopped != 41 {
		t.Fatalf("daemonStopped = %d, want 41", daemonStopped)
	}
	if appStopped != 51 {
		t.Fatalf("appStopped = %d, want 51", appStopped)
	}
	if !appStartCalled {
		t.Fatal("app was not resumed after daemon health succeeded")
	}
	if got, want := strings.Join(events, ","), "app.find,app.stop,daemon.find,daemon.stop,daemon.start,app.start"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "restarted" || res.Target != "daemon" || res.OldPID != 41 || res.NewPID != 42 || !res.Graceful {
		t.Fatalf("daemon result = %+v", res)
	}
	if res.App == nil {
		t.Fatalf("app result missing: %+v", res)
	}
	if res.App.Action != "restarted" || res.App.Target != "app" || res.App.OldPID != 51 || res.App.NewPID != 52 || !res.App.Graceful {
		t.Fatalf("app result = %+v", *res.App)
	}
	if strings.Join(res.App.Args, " ") != "app --remote" {
		t.Fatalf("app args = %q", strings.Join(res.App.Args, " "))
	}
}

func TestRunRestartAllCoreSkipsAppWhenNotRunning(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	appStartCalled := false
	var events []string
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			events = append(events, "daemon.find")
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			events = append(events, "daemon.start")
			return 61, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			events = append(events, "app.find")
			return appProcess{}, errAppNotRunning
		},
		start: func(context.Context, []string) (int, error) {
			appStartCalled = true
			return 0, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if appStartCalled {
		t.Fatal("plain restart should not start a new app when none was running")
	}
	if got, want := strings.Join(events, ","), "app.find,daemon.find,daemon.start"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App != nil {
		t.Fatalf("app result = %+v, want omitted", res.App)
	}
}

func TestRunRestartStackCorePreservesAbsentProcessesForUpdate(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	var events []string
	exit := runRestartStackCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			events = append(events, "daemon.find")
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			t.Fatal("update restart must not wake an absent daemon")
			return 0, rpc.HealthResult{}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			events = append(events, "app.find")
			return appProcess{}, errAppNotRunning
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("update restart must not wake an absent app")
			return 0, nil
		},
	}, restartStackBehavior{startDaemonWhenMissing: false})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if got, want := strings.Join(events, ","), "app.find,daemon.find"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "app not restarted") || !strings.Contains(out.String(), "daemon left stopped") {
		t.Fatalf("output missing preserved-liveness explanation: %s", out.String())
	}
}

func TestRunRestartAllCoreAppFailureLeavesDaemonUntouched(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			t.Fatal("daemon discovery must not run after app-stage failure")
			return update.DaemonProcess{}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("daemon stop must not run after app-stage failure")
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			t.Fatal("daemon start must not run after app-stage failure")
			return 0, rpc.HealthResult{}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{PID: 71, Command: "/tmp/canary app --remote", Args: []string{"app", "--remote"}}, nil
		},
		stop: func(int, time.Duration) error {
			return fmt.Errorf("app stop failed")
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "daemon was not touched") {
		t.Fatalf("stderr missing untouched-daemon guarantee: %s", errBuf.String())
	}
}

func TestRunRestartAllCoreAppResumeFailureHappensAfterDaemonRestart(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	var events []string
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			events = append(events, "daemon.find")
			return update.DaemonProcess{PID: 75, Command: "/current/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			events = append(events, "daemon.stop")
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			events = append(events, "daemon.start")
			return 76, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			events = append(events, "app.find")
			return appProcess{PID: 74, Command: "/current/canary app --remote", Args: []string{"app", "--remote"}}, nil
		},
		stop: func(int, time.Duration) error {
			events = append(events, "app.stop")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			events = append(events, "app.start-failed")
			return 0, errors.New("app start failed")
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if got, want := strings.Join(events, ","), "app.find,app.stop,daemon.find,daemon.stop,daemon.start,app.start-failed"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	if !strings.Contains(errBuf.String(), "daemon remains running") {
		t.Fatalf("stderr missing post-daemon app failure boundary: %s", errBuf.String())
	}
}

func TestRunRestartAllCoreDaemonFailureDoesNotRollBackApp(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	appStops := 0
	var events []string
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			events = append(events, "daemon.find")
			return update.DaemonProcess{PID: 81, Command: "/tmp/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			events = append(events, "daemon.stop-failed")
			return fmt.Errorf("daemon stop failed")
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			t.Fatal("daemon start must not run after daemon stop failure")
			return 0, rpc.HealthResult{}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			events = append(events, "app.find")
			return appProcess{PID: 72, Command: "/tmp/canary app --remote", Args: []string{"app", "--remote"}}, nil
		},
		stop: func(int, time.Duration) error {
			appStops++
			events = append(events, "app.stop")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("app must remain stopped after daemon failure")
			return 0, nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if appStops != 1 {
		t.Fatalf("app stop calls = %d, want one initial restart and no rollback", appStops)
	}
	if got, want := strings.Join(events, ","), "app.find,app.stop,daemon.find,daemon.stop-failed"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	if !strings.Contains(errBuf.String(), "app remains stopped so it cannot autospawn a competing daemon") {
		t.Fatalf("stderr missing fail-closed quiescence guidance: %s", errBuf.String())
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode partial restart result: %v\n%s", err, out.String())
	}
	if res.App == nil || res.App.Started || res.App.OldPID != 72 {
		t.Fatalf("partial result lost quiesced app stage: %+v", res)
	}
	if res.Started {
		t.Fatalf("partial result falsely reports daemon start: %+v", res)
	}
}

func TestRunRestartAllCoreQuiescesSupervisedAppAcrossDaemonRestart(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	var events []string
	supervisorCalls := 0
	appLoaded := true
	daemonHealthy := false
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			events = append(events, "daemon.find")
			return update.DaemonProcess{PID: 91, Command: "/tmp/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			events = append(events, "daemon.stop")
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			// This fake's context is checked in the dedicated startup-budget
			// assertion below; this callback focuses on the app/daemon order.
			if appLoaded {
				t.Fatal("daemon restart began while the autospawning app supervisor was loaded")
			}
			events = append(events, "daemon.start")
			daemonHealthy = true
			return 92, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			events = append(events, "app.find")
			return appProcess{PID: 82, Command: "/tmp/canary app --remote", Args: []string{"app", "--remote"}}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("supervised app must not be stopped directly")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("supervised app must not be started directly")
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			supervisorCalls++
			pid := 82
			name := "app.supervisor-detect"
			if supervisorCalls > 1 {
				pid = 83
				name = fmt.Sprintf("app.supervisor-verify-%d", supervisorCalls-1)
			}
			events = append(events, name)
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: pid, Executable: testCurrentExecutable(t), Args: []string{"app", "--remote"}, PlistPath: "/Users/test/Library/LaunchAgents/com.osauer.ibkr-app.plist"}, true
		},
		unload: func(_ context.Context, sup appSupervisor) error {
			events = append(events, "app.unload")
			if sup.PID != 82 {
				t.Fatalf("unload supervisor = %+v", sup)
			}
			appLoaded = false
			return nil
		},
		load: func(_ context.Context, sup appSupervisor) error {
			if !daemonHealthy {
				t.Fatal("app supervisor was resumed before daemon health succeeded")
			}
			events = append(events, "app.load")
			if sup.PID != 82 {
				t.Fatalf("load supervisor = %+v", sup)
			}
			appLoaded = true
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	want := "app.find,app.supervisor-detect,app.unload,daemon.find,daemon.stop,daemon.start,app.load,app.supervisor-verify-1,app.supervisor-verify-2"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App == nil || res.App.Supervisor == "" || res.App.NewPID != 83 || res.NewPID != 92 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunRestartAllCoreRejectsIndependentAppBesideSharedSupervisor(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")
	t.Setenv("CANARY_APP_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			t.Fatal("daemon discovery must not run while an autospawning supervisor remains loaded")
			return update.DaemonProcess{}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("daemon must remain untouched for a mixed app topology")
			return nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{
				PID:     60,
				Command: "/tmp/canary app --state-dir /tmp/isolated-canary-app",
				Args:    []string{"app", "--state-dir", "/tmp/isolated-canary-app"},
			}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("independent app must be preserved when the mixed topology is rejected")
			return nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{
				Target:     "gui/501/com.osauer.ibkr-app",
				PID:        0,
				Executable: testCurrentExecutable(t),
				Args:       []string{"app", "--remote"},
				PlistPath:  "/Users/test/Library/LaunchAgents/com.osauer.ibkr-app.plist",
			}, true
		},
		unload: func(context.Context, appSupervisor) error {
			t.Fatal("shared supervisor must remain untouched when the mixed topology is rejected")
			return nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "independent app process and the shared launchd supervisor") ||
		!strings.Contains(errBuf.String(), "daemon was not touched") {
		t.Fatalf("stderr missing mixed-topology safety boundary:\n%s", errBuf.String())
	}
}

func TestRunRestartAllCoreRejectsAmbiguousAppDiscoveryBeforeMutation(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	discoveryErr := fmt.Errorf("%w: multiple Canary app processes found", errAppUnverified)
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			t.Fatal("daemon discovery must not run after ambiguous app discovery")
			return update.DaemonProcess{}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, discoveryErr
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{
				Target:     "gui/501/com.osauer.ibkr-app",
				PID:        82,
				Executable: testCurrentExecutable(t),
				Args:       []string{"app", "--remote"},
			}, true
		},
		stop: func(int, time.Duration) error {
			t.Fatal("unknown app processes must not be stopped through an unverified PID")
			return nil
		},
		unload: func(context.Context, appSupervisor) error {
			t.Fatal("launchd must not be unloaded while other app processes are ambiguous")
			return nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "multiple Canary app processes found") ||
		!strings.Contains(errBuf.String(), "daemon was not touched") {
		t.Fatalf("stderr missing ambiguous-discovery boundary:\n%s", errBuf.String())
	}
}

func TestRunRestartAppCoreRejectsAmbiguousDiscoveryBeforeLaunchdMutation(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, fmt.Errorf("%w: multiple Canary app processes found", errAppUnverified)
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{
				Target:     "gui/501/com.osauer.ibkr-app",
				PID:        82,
				Executable: testCurrentExecutable(t),
				Args:       []string{"app", "--remote"},
			}, true
		},
		kickstart: func(context.Context, string) error {
			t.Fatal("launchd must not be kickstarted after ambiguous app discovery")
			return nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "multiple Canary app processes found") {
		t.Fatalf("stderr missing ambiguous-discovery error:\n%s", errBuf.String())
	}
}

func TestRestartDaemonStartUsesExplicitTimeoutBudget(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")

	timeout := 2300 * time.Millisecond
	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: timeout, out: &out, err: &errBuf}
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(ctx context.Context, _ string, _ io.Writer, _ bool) (int, rpc.HealthResult, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("daemon start context has no restart timeout deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > timeout {
				t.Fatalf("daemon startup budget = %s, want (0,%s]", remaining, timeout)
			}
			return 93, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
}

func TestRunRestartAllCoreRejectsStaleLaunchdExecutableBeforeDaemon(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			t.Fatal("daemon discovery must not run for a stale launchd executable")
			return update.DaemonProcess{}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("daemon stop must not run for a stale launchd executable")
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			t.Fatal("daemon start must not run for a stale launchd executable")
			return 0, rpc.HealthResult{}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{PID: 101, Args: []string{"app", "--remote"}}, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: 101, Executable: "/tmp/stale/ibkr", Args: []string{"app", "--remote"}}, true
		},
		kickstart: func(context.Context, string) error {
			t.Fatal("stale launchd job must not be kickstarted")
			return nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "not the current installed Canary executable") || !strings.Contains(errBuf.String(), "daemon was not touched") {
		t.Fatalf("stderr missing stale-executable safety explanation: %s", errBuf.String())
	}
}

func TestRunRestartAllCoreMigratesPreUpgradeLaunchdExecutableBeforeDaemon(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	oldSupervisor := appSupervisor{
		Target:     "gui/501/com.osauer.ibkr-app",
		PID:        101,
		Executable: "/opt/canary-legacy/ibkr",
		Args:       []string{"app", "--remote", "--remote-url", "https://relay.example.test"},
		PlistPath:  "/Users/test/Library/LaunchAgents/com.osauer.ibkr-app.plist",
	}
	canonicalExecutable := "/opt/canary/current/canary"
	var events []string
	migrated := false
	verifyCalls := 0
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			events = append(events, "daemon.find")
			return update.DaemonProcess{PID: 201, Command: canonicalExecutable + " daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			events = append(events, "daemon.stop")
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			events = append(events, "daemon.start")
			return 202, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			events = append(events, "app.find")
			return appProcess{
				PID:     oldSupervisor.PID,
				Command: oldSupervisor.Executable + " " + strings.Join(oldSupervisor.Args, " "),
				Args:    append([]string(nil), oldSupervisor.Args...),
			}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("the loaded legacy process must be quiesced by launchctl bootout, not signaled directly")
			return nil
		},
		executablePaths: map[string]struct{}{canonicalExecutable: {}},
		supervisor: func(context.Context) (appSupervisor, bool) {
			if !migrated {
				events = append(events, "app.supervisor-old")
				return oldSupervisor, true
			}
			verifyCalls++
			events = append(events, fmt.Sprintf("app.supervisor-canonical-%d", verifyCalls))
			return appSupervisor{
				Target:     oldSupervisor.Target,
				PID:        102,
				Executable: canonicalExecutable,
				Args:       append([]string(nil), oldSupervisor.Args...),
				PlistPath:  oldSupervisor.PlistPath,
			}, true
		},
		rewrite: func(_ context.Context, got appSupervisor) error {
			events = append(events, "app.rewrite")
			if got.Executable != oldSupervisor.Executable || !slices.Equal(got.Args, oldSupervisor.Args) {
				t.Fatalf("migration supervisor = %+v, want %+v", got, oldSupervisor)
			}
			migrated = true
			return nil
		},
		unload: func(context.Context, appSupervisor) error {
			events = append(events, "app.unload")
			return nil
		},
		load: func(context.Context, appSupervisor) error {
			events = append(events, "app.load")
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	want := "app.find,app.supervisor-old,app.rewrite,app.unload,daemon.find,daemon.stop,daemon.start,app.load,app.supervisor-canonical-1,app.supervisor-canonical-2"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App == nil || res.App.Reason != "migrated_pre_upgrade_supervisor" || res.App.NewPID != 102 || !res.App.Started {
		t.Fatalf("migration result = %+v", res.App)
	}
}

func TestRunRestartAllCoreRejectsUnparseableSupervisorBeforeMigrationAndDaemon(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			t.Fatal("daemon must remain untouched after an unparseable supervisor")
			return update.DaemonProcess{}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{
				Target:     "gui/501/com.osauer.ibkr-app",
				Executable: "/opt/legacy/ibkr",
				ParseError: "ProgramArguments contains a wrapper command",
			}, true
		},
		migrate: func(context.Context, appSupervisor) error {
			t.Fatal("unparseable supervisor must never be rewritten")
			return nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "untrusted or unparseable") || !strings.Contains(errBuf.String(), "daemon was not touched") {
		t.Fatalf("stderr missing fail-closed explanation: %s", errBuf.String())
	}
}

func TestRunRestartAllCoreMigrationFailureLeavesDaemonUntouched(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			t.Fatal("daemon must remain untouched when launchd reload fails")
			return update.DaemonProcess{}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{PID: 101, Command: "/opt/legacy/ibkr app --remote", Args: []string{"app", "--remote"}}, nil
		},
		executablePaths: map[string]struct{}{"/opt/current/canary": {}},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{
				Target:     "gui/501/com.osauer.ibkr-app",
				PID:        101,
				Executable: "/opt/legacy/ibkr",
				Args:       []string{"app", "--remote"},
			}, true
		},
		rewrite: func(context.Context, appSupervisor) error {
			return errors.New("plist rewrite failed")
		},
		unload: func(context.Context, appSupervisor) error { return nil },
		load:   func(context.Context, appSupervisor) error { return nil },
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "plist rewrite failed") || !strings.Contains(errBuf.String(), "daemon was not touched") {
		t.Fatalf("stderr missing migration failure boundary: %s", errBuf.String())
	}
}

func TestRunRestartAllCoreSkipsAppWhenSocketOverridden(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	appFindCalled := false
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 71, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			appFindCalled = true
			return appProcess{}, errAppNotRunning
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("app start must not run when CANARY_SOCKET is overridden")
			return 0, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if appFindCalled {
		t.Fatal("app discovery must not run when CANARY_SOCKET is overridden")
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App == nil {
		t.Fatalf("app result missing, want explicit skip marker: %+v", res)
	}
	if res.App.Action != "skipped" || res.App.Reason != "socket_overridden" || res.App.Target != "app" {
		t.Fatalf("app result = %+v", *res.App)
	}
}

func TestRunRestartAllCoreAnnouncesSocketHandsOffBeforeDaemonMutation(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			if !strings.Contains(out.String(), "separate explicit, non-atomic operation") {
				t.Fatal("socket-override decision was not announced before daemon discovery")
			}
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 72, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			t.Fatal("app discovery must remain hands-off for an overridden socket")
			return appProcess{}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("app stop must remain hands-off for an overridden socket")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("app start must remain hands-off for an overridden socket")
			return 0, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
}

func TestRunRestartAppCoreKickstartsLaunchdJob(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	kicked := ""
	supCalls := 0
	startCalled := false
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{PID: 90, Command: "/tmp/canary app --remote", Args: []string{"app", "--remote"}}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("supervised restart must not SIGTERM the supervised process by hand")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			startCalled = true
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			supCalls++
			pid := 90
			if kicked != "" {
				pid = 91
			}
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: pid, Executable: testCurrentExecutable(t), Args: []string{"app", "--remote"}}, true
		},
		kickstart: func(_ context.Context, target string) error {
			kicked = target
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if kicked != "gui/501/com.osauer.ibkr-app" {
		t.Fatalf("kickstart target = %q", kicked)
	}
	if startCalled {
		t.Fatal("supervised restart must not spawn an unsupervised app process")
	}
	if supCalls < 2 {
		t.Fatalf("supervisor calls = %d, want detection plus respawn wait", supCalls)
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "restarted" || res.Supervisor != "gui/501/com.osauer.ibkr-app" || !res.WasRunning || res.OldPID != 90 || res.NewPID != 91 || !res.Started {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunRestartAppCoreStopsOrphanBeforeKickstart(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf}
	stoppedPID := 0
	kicked := false
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			// The orphan (pid 4098-style) is NOT the supervised process:
			// launchd shows no live pid while it crash-loops on the lock.
			return appProcess{PID: 70, Command: "/tmp/canary app --remote", Args: []string{"app", "--remote"}}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			stoppedPID = pid
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("must not spawn a fresh orphan")
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			pid := 0
			if kicked {
				pid = 71
			}
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: pid, Executable: testCurrentExecutable(t), Args: []string{"app", "--remote"}}, true
		},
		kickstart: func(context.Context, string) error {
			if stoppedPID == 0 {
				t.Fatal("kickstart before the orphan was stopped")
			}
			kicked = true
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if stoppedPID != 70 {
		t.Fatalf("stoppedPID = %d, want the orphan 70", stoppedPID)
	}
	if !strings.Contains(out.String(), "restarted supervised app pid 71") {
		t.Fatalf("output missing supervised restart confirmation:\n%s", out.String())
	}
}

func TestRunRestartAppCoreLeavesIsolatedInstanceToUnsupervisedPath(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf}
	isolatedArgs := []string{"app", "--addr", "127.0.0.1:18765", "--state-dir", "/tmp/ibkr-smoke"}
	stoppedPID := 0
	startedArgs := []string{}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			if stoppedPID != 0 {
				return appProcess{}, errAppNotRunning
			}
			return appProcess{PID: 60, Command: "/tmp/canary " + strings.Join(isolatedArgs, " "), Args: isolatedArgs}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			stoppedPID = pid
			return nil
		},
		start: func(_ context.Context, args []string) (int, error) {
			startedArgs = append([]string(nil), args...)
			return 61, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			// The shared host's LaunchAgent is loaded, but the running app
			// is an isolated smoke/preview instance with its own state dir.
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: 0, Executable: testCurrentExecutable(t), Args: []string{"app", "--remote"}}, true
		},
		kickstart: func(context.Context, string) error {
			t.Fatal("must not kickstart the shared host for an isolated app instance")
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if stoppedPID != 60 {
		t.Fatalf("stoppedPID = %d, want the isolated instance 60", stoppedPID)
	}
	if !slices.Equal(startedArgs, isolatedArgs) {
		t.Fatalf("restarted args = %q, want the isolated instance's own args", strings.Join(startedArgs, " "))
	}
}

func TestSupervisedRestartAppliesComparesStateLocks(t *testing.T) {
	// The orphan test is state-lock identity, not pid or argv equality —
	// pin the default state dir so the cases stay hermetic.
	t.Setenv("CANARY_APP_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shared := appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: 4098, Executable: testCurrentExecutable(t), Args: []string{"app", "--remote"}}
	cases := []struct {
		name    string
		proc    appProcess
		findErr error
		sup     appSupervisor
		want    bool
	}{
		{"supervised pid itself", appProcess{PID: 4098, Args: []string{"app", "--remote"}}, nil, shared, true},
		{"explicit absence defers to the job", appProcess{}, errAppNotRunning, shared, true},
		{"ambiguous find never defers to the job", appProcess{}, errAppUnverified, shared, false},
		{"unexpected find error never defers to the job", appProcess{}, errors.New("ps failed"), shared, false},
		{"default-dir orphan with different argv", appProcess{PID: 70, Args: []string{"app", "--addr", "0.0.0.0:8765"}}, nil, shared, true},
		{"isolated instance with its own state dir", appProcess{PID: 60, Args: []string{"app", "--state-dir", "/tmp/ibkr-smoke"}}, nil, shared, false},
		{"same explicit state dir is the job's orphan", appProcess{PID: 61, Args: []string{"app", "--state-dir", "/var/lib/ibkr-app"}}, nil, appSupervisor{Target: shared.Target, Executable: shared.Executable, Args: []string{"app", "--state-dir", "/var/lib/ibkr-app"}}, true},
		{"unparsed plist args resolve to the default dir", appProcess{PID: 62, Args: []string{"app", "--state-dir", "/tmp/ibkr-smoke"}}, nil, appSupervisor{Target: shared.Target, Executable: shared.Executable}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisedRestartApplies(tc.proc, tc.findErr, tc.sup); got != tc.want {
				t.Fatalf("supervisedRestartApplies = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAppArgsStateDirResolvesSymlinkedSpellings(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	a := appArgsStateDir([]string{"app", "--state-dir", real})
	b := appArgsStateDir([]string{"app", "--state-dir", link})
	if a != b {
		t.Fatalf("state dir identity should survive symlinked spellings: %q vs %q", a, b)
	}
}

func TestExecutablePathMatchesCurrentThroughSymlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "ibkr")
	if err := os.Symlink(testCurrentExecutable(t), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if !executablePathMatches(link, currentExecutablePaths()) {
		t.Fatalf("symlink %q should resolve to the current executable", link)
	}
	if executablePathMatches(filepath.Join(t.TempDir(), "stale-ibkr"), currentExecutablePaths()) {
		t.Fatal("unrelated executable path matched current executable")
	}
}

func TestRunRestartAppCoreRejectsOverridesForSupervisedApp(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf, appRemote: true, appRemoteSet: true}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", Executable: testCurrentExecutable(t)}, true
		},
		kickstart: func(context.Context, string) error {
			t.Fatal("must not kickstart when overrides were rejected")
			return nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "canary setup app") {
		t.Fatalf("stderr should point at `canary setup app`:\n%s", errBuf.String())
	}
}

func TestLaunchdProgramArgumentsParsesPrintOutput(t *testing.T) {
	t.Parallel()

	out := "gui/501/com.osauer.ibkr-app = {\n" +
		"\tactive count = 1\n" +
		"\tpid = 4098\n" +
		"\targuments = {\n" +
		"\t\t/Users/osauer/.local/bin/ibkr\n" +
		"\t\tapp\n" +
		"\t\t--remote\n" +
		"\t}\n" +
		"}\n"
	executable, args := launchdProgramArguments(out)
	if executable != "/Users/osauer/.local/bin/ibkr" {
		t.Fatalf("executable = %q", executable)
	}
	if strings.Join(args, " ") != "app --remote" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}
	if m := launchdPIDRe.FindStringSubmatch(out); m == nil || m[1] != "4098" {
		t.Fatalf("pid parse = %v", m)
	}
}

func TestLaunchdProgramArgumentsRejectsAppLaterInArgv(t *testing.T) {
	t.Parallel()

	out := "gui/501/com.osauer.ibkr-app = {\n" +
		"\targuments = {\n" +
		"\t\t/usr/bin/echo\n" +
		"\t\t/tmp/canary\n" +
		"\t\tapp\n" +
		"\t}\n" +
		"}\n"
	if _, _, err := parseLaunchdProgramArguments(out); err == nil {
		t.Fatal("wrapper command with canary app later in argv was trusted")
	}
}

func TestLaunchdProgramArgumentsRejectsProgramOverride(t *testing.T) {
	t.Parallel()

	out := "gui/501/com.osauer.ibkr-app = {\n" +
		"\tprogram = /usr/bin/echo\n" +
		"\targuments = {\n" +
		"\t\t/tmp/canary\n" +
		"\t\tapp\n" +
		"\t}\n" +
		"}\n"
	if _, _, err := parseLaunchdProgramArguments(out); err == nil {
		t.Fatal("launchd Program override of canary app argv was trusted")
	}
}

func TestMigrateAppLaunchAgentUsingRewritesAndReloadsPinnedJob(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	currentExecutable := filepath.Join(root, "bin", "canary")
	if err := os.MkdirAll(filepath.Dir(currentExecutable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentExecutable, []byte("test executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(agentDir, "com.osauer.ibkr-app.plist")
	legacyExecutable := filepath.Join(root, "bin", "ibkr")
	original := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<plist version=\"1.0\"><dict>\n" +
		"<key>Label</key><string>com.osauer.ibkr-app</string>\n" +
		"<key>ProgramArguments</key><array>\n" +
		"<string>" + legacyExecutable + "</string><string>app</string>\n" +
		"<string>--remote</string><string>--remote-url</string><string>https://relay.example.test</string>\n" +
		"</array>\n" +
		"<key>EnvironmentVariables</key><dict><key>CANARY_TEST</key><string>preserved</string></dict>\n" +
		"<key>KeepAlive</key><true/>\n" +
		"</dict></plist>\n"
	if err := os.WriteFile(plistPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	sup := appSupervisor{
		Target:     fmt.Sprintf("gui/%d/com.osauer.ibkr-app", os.Getuid()),
		PID:        44,
		Executable: legacyExecutable,
		Args:       []string{"app", "--remote", "--remote-url", "https://relay.example.test"},
		PlistPath:  plistPath,
	}
	var calls []string
	err := migrateAppLaunchAgentUsing(context.Background(), sup, currentExecutable, func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	wantCalls := []string{
		"bootout " + sup.Target,
		"bootstrap " + fmt.Sprintf("gui/%d", os.Getuid()) + " " + plistPath,
	}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("launchctl calls = %q, want %q", calls, wantCalls)
	}
	rewritten, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(rewritten)
	if strings.Contains(got, "<string>"+legacyExecutable+"</string>") {
		t.Fatalf("legacy executable remains in plist:\n%s", got)
	}
	for _, preserved := range []string{
		"<string>" + currentExecutable + "</string>",
		"<string>app</string>",
		"<string>--remote</string>",
		"<string>https://relay.example.test</string>",
		"<key>CANARY_TEST</key><string>preserved</string>",
		"<key>KeepAlive</key><true/>",
	} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("rewritten plist lost %q:\n%s", preserved, got)
		}
	}
	if strings.Count(got, "com.osauer.ibkr-app") != 1 {
		t.Fatalf("migration changed the pinned label cardinality:\n%s", got)
	}

	// A failed bootout after publication leaves a canonical plist and the old
	// loaded job. The retry must be idempotent and reload that same label.
	calls = nil
	if err := migrateAppLaunchAgentUsing(context.Background(), sup, currentExecutable, func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("retry launchctl calls = %q, want %q", calls, wantCalls)
	}
}

func TestRewriteLaunchAgentExecutableRejectsProgramOverride(t *testing.T) {
	t.Parallel()

	legacyExecutable := "/opt/legacy/ibkr"
	data := []byte("<plist><dict>" +
		"<key>Label</key><string>com.osauer.ibkr-app</string>" +
		"<key>Program</key><string>/usr/bin/echo</string>" +
		"<key>ProgramArguments</key><array><string>" + legacyExecutable + "</string><string>app</string></array>" +
		"</dict></plist>")
	_, err := rewriteLaunchAgentExecutable(data, appSupervisor{
		Target:     "gui/501/com.osauer.ibkr-app",
		Executable: legacyExecutable,
		Args:       []string{"app"},
	}, "/opt/current/canary")
	if err == nil || !strings.Contains(err.Error(), "Program override") {
		t.Fatalf("rewrite error = %v, want Program override rejection", err)
	}
}

func TestRunRestartAppCoreStartsWhenNoAppWasRunning(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	startArgs := []string{}
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		start: func(_ context.Context, args []string) (int, error) {
			startArgs = append([]string(nil), args...)
			return 44, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if strings.Join(startArgs, " ") != "app" {
		t.Fatalf("start args = %q, want app", strings.Join(startArgs, " "))
	}
	if !strings.Contains(out.String(), "no app was running") || !strings.Contains(out.String(), "started app pid 44") {
		t.Fatalf("output missing app start messages:\n%s", out.String())
	}
}

func TestRunRestartAppCorePreservesArgsAndDetectsSupervisorRespawn(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	findCalls := 0
	stoppedPID := 0
	startCalled := false
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			findCalls++
			if findCalls == 1 {
				return appProcess{
					PID:     51,
					Command: "/tmp/canary app --addr 127.0.0.1:18765",
					Args:    []string{"app", "--addr", "127.0.0.1:18765"},
				}, nil
			}
			return appProcess{
				PID:               52,
				Command:           "/tmp/canary app --addr 127.0.0.1:18765",
				Args:              []string{"app", "--addr", "127.0.0.1:18765"},
				CurrentExecutable: true,
			}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			stoppedPID = pid
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			startCalled = true
			return 0, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if stoppedPID != 51 {
		t.Fatalf("stoppedPID = %d, want 51", stoppedPID)
	}
	if strings.Contains(out.String(), "stopping app pid 51 gracefully") {
		t.Fatalf("app restart should not print duplicate stop progress and confirmation lines:\n%s", out.String())
	}
	if startCalled {
		t.Fatal("manual start should not run when supervisor respawned the app")
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "restarted" || res.Target != "app" || !res.WasRunning || !res.Graceful || res.OldPID != 51 || res.NewPID != 52 {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(res.Args, " ") != "app --addr 127.0.0.1:18765" {
		t.Fatalf("args = %q", strings.Join(res.Args, " "))
	}
}

func TestRunRestartAppCoreDoesNotTreatDifferentArgsAsRespawn(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	findCalls := 0
	startArgs := []string{}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			findCalls++
			if findCalls == 1 {
				return appProcess{
					PID:     61,
					Command: "/tmp/canary app --addr 127.0.0.1:18765",
					Args:    []string{"app", "--addr", "127.0.0.1:18765"},
				}, nil
			}
			return appProcess{
				PID:     62,
				Command: "canary app",
				Args:    []string{"app"},
			}, nil
		},
		stop: func(int, time.Duration) error {
			return nil
		},
		start: func(_ context.Context, args []string) (int, error) {
			startArgs = append([]string(nil), args...)
			return 63, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if strings.Join(startArgs, " ") != "app --addr 127.0.0.1:18765" {
		t.Fatalf("start args = %q", strings.Join(startArgs, " "))
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.NewPID != 63 {
		t.Fatalf("new pid = %d, want manually started pid 63", res.NewPID)
	}
}

func TestRunRestartAppCoreOverridesAddrAndClearsStalePublicURL(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{
		app:        true,
		jsonOut:    true,
		timeout:    time.Second,
		appAddr:    "0.0.0.0:8765",
		appAddrSet: true,
		out:        &out,
		err:        &errBuf,
	}
	findCalls := 0
	startArgs := []string{}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			findCalls++
			if findCalls == 1 {
				return appProcess{
					PID:     71,
					Command: "/tmp/canary app --addr 127.0.0.1:8765 --public-url http://127.0.0.1:8765 --state-dir /tmp/app-state",
					Args:    []string{"app", "--addr", "127.0.0.1:8765", "--public-url", "http://127.0.0.1:8765", "--state-dir", "/tmp/app-state"},
				}, nil
			}
			return appProcess{PID: 72, Command: "canary app", Args: []string{"app"}}, nil
		},
		stop: func(int, time.Duration) error {
			return nil
		},
		start: func(_ context.Context, args []string) (int, error) {
			startArgs = append([]string(nil), args...)
			return 73, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	want := "app --state-dir /tmp/app-state --addr 0.0.0.0:8765"
	if strings.Join(startArgs, " ") != want {
		t.Fatalf("start args = %q, want %q", strings.Join(startArgs, " "), want)
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if strings.Join(res.Args, " ") != want {
		t.Fatalf("result args = %q, want %q", strings.Join(res.Args, " "), want)
	}
}

func TestAppArgsWithRestartOverridesKeepsExplicitPublicURL(t *testing.T) {
	t.Parallel()

	opts := &restartOptions{
		appAddr:         "0.0.0.0:8765",
		appAddrSet:      true,
		appPublicURL:    "http://192.168.1.42:8765",
		appPublicURLSet: true,
	}
	got := appArgsWithRestartOverrides(
		[]string{"app", "--addr=127.0.0.1:8765", "--public-url=http://127.0.0.1:8765"},
		opts,
	)
	want := "app --addr 0.0.0.0:8765 --public-url http://192.168.1.42:8765"
	if strings.Join(got, " ") != want {
		t.Fatalf("args = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestAppArgsWithRestartOverridesRemoteFlags(t *testing.T) {
	t.Parallel()

	opts := &restartOptions{
		appRemote:       true,
		appRemoteSet:    true,
		appRemoteURL:    "https://remote.example.test",
		appRemoteURLSet: true,
	}
	got := appArgsWithRestartOverrides(
		[]string{"app", "--addr", "0.0.0.0:8765", "--remote-url=https://old.example.test"},
		opts,
	)
	want := "app --addr 0.0.0.0:8765 --remote --remote-url https://remote.example.test"
	if strings.Join(got, " ") != want {
		t.Fatalf("args = %q, want %q", strings.Join(got, " "), want)
	}

	opts.appRemote = false
	got = appArgsWithRestartOverrides(
		[]string{"app", "--remote", "--remote-url", "https://remote.example.test"},
		opts,
	)
	want = "app --remote-url https://remote.example.test"
	if strings.Join(got, " ") != want {
		t.Fatalf("disable remote args = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestAppValueArgReadsSplitAndEqualsForms(t *testing.T) {
	t.Parallel()

	if got := appValueArg([]string{"app", "--addr", "0.0.0.0:8765"}, "addr"); got != "0.0.0.0:8765" {
		t.Fatalf("split addr = %q", got)
	}
	if got := appValueArg([]string{"app", "serve", "--addr=127.0.0.1:8765"}, "addr"); got != "127.0.0.1:8765" {
		t.Fatalf("equals addr = %q", got)
	}
}

func TestAppCommandArgsIgnoresPairCommand(t *testing.T) {
	t.Parallel()

	if _, ok := appCommandArgs("/tmp/canary app pair --json"); ok {
		t.Fatalf("app pair should not be treated as the long-running app server")
	}
	for _, executable := range []string{"ibkr", "canary"} {
		args, ok := appCommandArgs("/tmp/" + executable + " app --addr 127.0.0.1:8765")
		if !ok {
			t.Fatalf("%s app server command was not detected", executable)
		}
		if strings.Join(args, " ") != "app --addr 127.0.0.1:8765" {
			t.Fatalf("%s args = %q", executable, strings.Join(args, " "))
		}
	}
}

func TestAppCommandMatchReportsExactExecutable(t *testing.T) {
	t.Parallel()

	args, exact, ok := appCommandMatch("/tmp/canary app --addr 127.0.0.1:8765", map[string]struct{}{"/tmp/ibkr": {}})
	if !ok {
		t.Fatalf("app server command was not detected")
	}
	if exact {
		t.Fatalf("different executable paths must not be treated as exact matches")
	}
	if strings.Join(args, " ") != "app --addr 127.0.0.1:8765" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}
	_, exact, ok = appCommandMatch("canary app", map[string]struct{}{"/tmp/ibkr": {}})
	if !ok {
		t.Fatalf("generic app command was not detected")
	}
	if exact {
		t.Fatalf("generic command should not be an exact executable match")
	}
	_, exact, ok = appCommandMatch("/tmp/canary app", map[string]struct{}{"/tmp/canary": {}})
	if !ok || !exact {
		t.Fatalf("canonical executable match = ok %v exact %v, want true/true", ok, exact)
	}
}

func TestAppCommandMatchRequiresExecutableAtCommandPosition(t *testing.T) {
	t.Parallel()

	for _, cmdline := range []string{
		"echo /tmp/canary app",
		"/usr/bin/echo /tmp/canary app --remote",
		"sh -c /tmp/canary app",
		"python helper.py /tmp/ibkr app",
	} {
		if args, exact, ok := appCommandMatch(cmdline, map[string]struct{}{"/tmp/canary": {}}); ok {
			t.Fatalf("wrapper %q matched as args=%q exact=%v", cmdline, args, exact)
		}
	}
	for _, cmdline := range []string{
		"/tmp/canary app --remote",
		"/tmp/ibkr app --remote",
	} {
		if _, _, ok := appCommandMatch(cmdline, nil); !ok {
			t.Fatalf("managed executable in command position was not recognized: %q", cmdline)
		}
	}
}

func TestRunRestartRejectsUnexpectedArgument(t *testing.T) {
	var out, errBuf bytes.Buffer
	exit := RunRestart(context.Background(), []string{"extra"}, &out, &errBuf)
	if exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Fatalf("stderr missing argument error:\n%s", errBuf.String())
	}
}

func TestRunRestartAppFlagOverridesRequireApp(t *testing.T) {
	var out, errBuf bytes.Buffer
	exit := RunRestart(context.Background(), []string{"--remote"}, &out, &errBuf)
	if exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if !strings.Contains(errBuf.String(), "require --app") {
		t.Fatalf("stderr missing --app requirement:\n%s", errBuf.String())
	}
}
