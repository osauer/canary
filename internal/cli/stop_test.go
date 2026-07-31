package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/update"
)

// stopCalls records which processes a stop attempt signalled, so a test can
// assert that a refusal signalled nothing at all.
type stopCalls struct {
	appStopped    []int
	appKilled     []int
	daemonStopped []int
	daemonKilled  []int
	unloaded      []string
	order         []string
}

func testStopOptions(stdin string, out, errBuf *bytes.Buffer) *stopOptions {
	return &stopOptions{
		app:     true,
		daemon:  true,
		timeout: time.Second,
		in:      strings.NewReader(stdin),
		out:     out,
		err:     errBuf,
	}
}

// runningStopDeps wires one running app and one running daemon with an idle
// daemon health read. Tests override the pieces they care about.
func runningStopDeps(calls *stopCalls) stopDeps {
	return stopDeps{
		app: appRestartDeps{
			find: func(context.Context) (appProcess, error) {
				return appProcess{PID: 31, Command: "/tmp/canary app", Args: []string{"app"}, CurrentExecutable: true}, nil
			},
			stop: func(pid int, _ time.Duration) error {
				calls.appStopped = append(calls.appStopped, pid)
				calls.order = append(calls.order, "app")
				return nil
			},
			kill: func(pid int, _ time.Duration) error {
				calls.appKilled = append(calls.appKilled, pid)
				return nil
			},
		},
		daemon: restartDeps{
			find: func(context.Context, string) (update.DaemonProcess, error) {
				return update.DaemonProcess{PID: 41, Command: "/tmp/canary daemon", SocketPath: "sock", LockPath: "lock"}, nil
			},
			stop: func(pid int, _ time.Duration) error {
				calls.daemonStopped = append(calls.daemonStopped, pid)
				calls.order = append(calls.order, "daemon")
				return nil
			},
			kill: func(pid int, _ time.Duration) error {
				calls.daemonKilled = append(calls.daemonKilled, pid)
				return nil
			},
		},
		health: func(context.Context, string) (rpc.HealthResult, bool, error) {
			return rpc.HealthResult{BackgroundTasks: []rpc.BackgroundTaskStatus{}}, true, nil
		},
	}
}

func workingOrdersHealth(_ context.Context, _ string) (rpc.HealthResult, bool, error) {
	return rpc.HealthResult{BackgroundTasks: []rpc.BackgroundTaskStatus{
		{Name: "open-orders", Status: "3 working"},
	}}, true, nil
}

func TestRunStopCoreStopsAppBeforeDaemon(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), runningStopDeps(calls))
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	// The app is a daemon client with autospawn authority: stopping the
	// daemon first lets it spawn a replacement mid-stop.
	if got := strings.Join(calls.order, ","); got != "app,daemon" {
		t.Fatalf("signal order = %q, want app,daemon", got)
	}
	got := out.String()
	for _, want := range []string{"stopped app pid 31 gracefully", "stopped daemon pid 41 gracefully", "any canary command starts the daemon again"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCoreRefusesWhileDaemonHasWorkInFlight(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.health = workingOrdersHealth
	exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), deps)
	if exit == 0 {
		t.Fatalf("exit = 0, want refusal\n%s%s", out.String(), errBuf.String())
	}
	if len(calls.appStopped) != 0 || len(calls.daemonStopped) != 0 {
		t.Fatalf("refusal still signalled processes: %+v", calls)
	}
	stderr := errBuf.String()
	for _, want := range []string{"open-orders", "3 working", "pass --yes"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

func TestRunStopCoreYesStopsDespiteWorkInFlight(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.health = workingOrdersHealth
	opts := testStopOptions("", &out, &errBuf)
	opts.yes = true
	if exit := runStopCore(context.Background(), opts, deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if len(calls.daemonStopped) != 1 {
		t.Fatalf("daemon was not stopped: %+v", calls)
	}
}

func TestRunStopCorePromptDeclineLeavesEverythingRunning(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.health = workingOrdersHealth
	opts := testStopOptions("n\n", &out, &errBuf)
	opts.isTTY = true
	exit := runStopCore(context.Background(), opts, deps)
	if exit == 0 {
		t.Fatalf("exit = 0, want refusal")
	}
	if len(calls.appStopped) != 0 || len(calls.daemonStopped) != 0 {
		t.Fatalf("declined stop still signalled processes: %+v", calls)
	}
	if !strings.Contains(out.String(), "Stop anyway? [y/N]") {
		t.Fatalf("prompt missing:\n%s", out.String())
	}
	if !strings.Contains(errBuf.String(), "left everything running") {
		t.Fatalf("stderr missing decline notice:\n%s", errBuf.String())
	}
}

func TestRunStopCorePromptAcceptStops(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.health = workingOrdersHealth
	opts := testStopOptions("y\n", &out, &errBuf)
	opts.isTTY = true
	if exit := runStopCore(context.Background(), opts, deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if len(calls.daemonStopped) != 1 {
		t.Fatalf("daemon was not stopped after confirmation: %+v", calls)
	}
}

// A JSON caller cannot answer a prompt, and writing one to stdout would
// corrupt the document — so --json without --yes must refuse rather than ask.
func TestRunStopCoreJSONNeverPrompts(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.health = workingOrdersHealth
	opts := testStopOptions("y\n", &out, &errBuf)
	opts.isTTY = true
	opts.jsonOut = true
	exit := runStopCore(context.Background(), opts, deps)
	if exit == 0 {
		t.Fatalf("exit = 0, want refusal")
	}
	if strings.Contains(out.String(), "Stop anyway?") {
		t.Fatalf("json mode prompted on stdout:\n%s", out.String())
	}
	var res stopResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "refused" || len(res.Blockers) != 1 || res.Blockers[0].Name != "open-orders" {
		t.Fatalf("result = %+v", res)
	}
}

// An unreadable daemon is unknown, not clean: it may still hold working
// orders, so it takes the same confirmation.
func TestRunStopCoreUnreadableHealthBlocks(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.health = func(context.Context, string) (rpc.HealthResult, bool, error) {
		return rpc.HealthResult{}, true, errors.New("socket timeout")
	}
	if exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), deps); exit == 0 {
		t.Fatalf("exit = 0, want refusal")
	}
	if !strings.Contains(errBuf.String(), "socket timeout") {
		t.Fatalf("stderr missing health failure:\n%s", errBuf.String())
	}
}

func TestRunStopCoreForceEscalatesOnlyAfterGracefulTimeout(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.daemon.stop = func(pid int, _ time.Duration) error {
		calls.daemonStopped = append(calls.daemonStopped, pid)
		return fmt.Errorf("wrapped: %w", update.ErrStopTimeout)
	}
	opts := testStopOptions("", &out, &errBuf)
	opts.force = true
	if exit := runStopCore(context.Background(), opts, deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if len(calls.daemonKilled) != 1 || calls.daemonKilled[0] != 41 {
		t.Fatalf("daemonKilled = %v, want [41]", calls.daemonKilled)
	}
	if !strings.Contains(out.String(), "forcing SIGKILL") {
		t.Fatalf("output missing force message:\n%s", out.String())
	}
}

func TestRunStopCoreWithoutForceReportsTheTimeout(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.daemon.stop = func(int, time.Duration) error {
		return fmt.Errorf("wrapped: %w", update.ErrStopTimeout)
	}
	if exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), deps); exit == 0 {
		t.Fatal("exit = 0, want failure")
	}
	if len(calls.daemonKilled) != 0 {
		t.Fatalf("SIGKILL sent without --force: %v", calls.daemonKilled)
	}
	if !strings.Contains(errBuf.String(), "re-run with --force") {
		t.Fatalf("stderr missing force hint:\n%s", errBuf.String())
	}
}

func TestRunStopCoreNothingRunningIsSuccess(t *testing.T) {
	var out, errBuf bytes.Buffer
	deps := stopDeps{
		app: appRestartDeps{
			find: func(context.Context) (appProcess, error) { return appProcess{}, errAppNotRunning },
		},
		daemon: restartDeps{
			find: func(context.Context, string) (update.DaemonProcess, error) {
				return update.DaemonProcess{}, update.ErrDaemonNotRunning
			},
		},
		health: func(context.Context, string) (rpc.HealthResult, bool, error) {
			return rpc.HealthResult{}, false, nil
		},
	}
	if exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "no app was running") || !strings.Contains(got, "no daemon was running") {
		t.Fatalf("output missing idle report:\n%s", got)
	}
}

// launchd KeepAlive respawns a SIGTERMed app within seconds, so the job must
// be unloaded first and must not be reloaded afterwards.
func TestRunStopCoreUnloadsSupervisedApp(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	sup := appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: 31, Executable: "/usr/local/bin/canary", Args: []string{"app"}}
	deps.app.supervisor = func(context.Context) (appSupervisor, bool) { return sup, true }
	deps.app.unload = func(_ context.Context, got appSupervisor) error {
		calls.unloaded = append(calls.unloaded, got.Target)
		calls.order = append(calls.order, "unload")
		return nil
	}
	deps.app.load = func(context.Context, appSupervisor) error {
		t.Fatal("stop reloaded the launchd job")
		return nil
	}
	deps.app.kickstart = func(context.Context, string) error {
		t.Fatal("stop kickstarted the launchd job")
		return nil
	}
	if exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if got := strings.Join(calls.order, ","); got != "unload,app,daemon" {
		t.Fatalf("order = %q, want unload,app,daemon", got)
	}
	if len(calls.unloaded) != 1 || calls.unloaded[0] != sup.Target {
		t.Fatalf("unloaded = %v", calls.unloaded)
	}
	got := out.String()
	for _, want := range []string{"unloaded launchd job " + sup.Target, "stays stopped until `canary restart --app`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

// bootout terminates the job's own process, so a stop that then signalled
// the pid it had just booted out could hit whatever inherited that number.
func TestRunStopCoreSignalsNothingWhenBootoutLeftNoProcess(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.app.find = func(context.Context) (appProcess, error) { return appProcess{}, errAppNotRunning }
	deps.app.supervisor = func(context.Context) (appSupervisor, bool) {
		return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: 31, Executable: "/usr/local/bin/canary", Args: []string{"app"}}, true
	}
	deps.app.unload = func(_ context.Context, sup appSupervisor) error {
		calls.unloaded = append(calls.unloaded, sup.Target)
		return nil
	}
	opts := testStopOptions("", &out, &errBuf)
	opts.jsonOut = true
	if exit := runStopCore(context.Background(), opts, deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if len(calls.appStopped) != 0 || len(calls.appKilled) != 0 {
		t.Fatalf("signalled a booted-out pid: %+v", calls)
	}
	var res stopResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App == nil || res.App.Action != "unloaded" || !res.App.Unloaded || res.App.WasRunning {
		t.Fatalf("app = %+v", res.App)
	}
}

func TestRunStopCoreRefusesUntrustedSupervisor(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.app.supervisor = func(context.Context) (appSupervisor, bool) {
		return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: 31, ParseError: "malformed launchd ProgramArguments"}, true
	}
	deps.app.unload = func(context.Context, appSupervisor) error {
		t.Fatal("unloaded an unparseable launchd job")
		return nil
	}
	if exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), deps); exit == 0 {
		t.Fatal("exit = 0, want refusal")
	}
	if len(calls.daemonStopped) != 0 {
		t.Fatalf("daemon stopped after an app-stage failure: %+v", calls)
	}
}

func TestRunStopCoreDaemonOnlyLeavesTheAppAlone(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.app.find = func(context.Context) (appProcess, error) {
		t.Fatal("--daemon looked for an app process")
		return appProcess{}, nil
	}
	opts := testStopOptions("", &out, &errBuf)
	opts.app = false
	if exit := runStopCore(context.Background(), opts, deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if len(calls.appStopped) != 0 {
		t.Fatalf("app was signalled: %+v", calls)
	}
}

// --app must not ask the daemon for permission to stop something else, and
// must say the daemon is still up.
func TestRunStopCoreAppOnlySkipsThePreflight(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.health = func(context.Context, string) (rpc.HealthResult, bool, error) {
		t.Fatal("--app read daemon health")
		return rpc.HealthResult{}, false, nil
	}
	opts := testStopOptions("", &out, &errBuf)
	opts.daemon = false
	if exit := runStopCore(context.Background(), opts, deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if len(calls.daemonStopped) != 0 {
		t.Fatalf("daemon was signalled: %+v", calls)
	}
	if !strings.Contains(out.String(), "the daemon is still running") {
		t.Fatalf("output missing daemon notice:\n%s", out.String())
	}
}

func TestRunStopCoreSkipsTheAppForAnOverriddenSocket(t *testing.T) {
	t.Setenv("CANARY_SOCKET", t.TempDir()+"/ibkr.sock")
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.app.find = func(context.Context) (appProcess, error) {
		t.Fatal("an overridden socket scope still looked for an app process")
		return appProcess{}, nil
	}
	if exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if len(calls.appStopped) != 0 {
		t.Fatalf("app was signalled outside its scope: %+v", calls)
	}
	if !strings.Contains(errBuf.String(), "CANARY_SOCKET is set") {
		t.Fatalf("stderr missing scope notice:\n%s", errBuf.String())
	}
}

// MCP servers are children of their AI host and already exit with it. Stop
// names them so "what is still running" is answerable, and signals nothing.
func TestRunStopCoreReportsMCPWithoutSignallingIt(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	deps := runningStopDeps(calls)
	deps.mcp = func(context.Context) []mcpProcess {
		return []mcpProcess{
			{PID: 71, Host: "Claude", Action: "left_running"},
			{PID: 72, Host: "codex", Action: "left_running"},
		}
	}
	if exit := runStopCore(context.Background(), testStopOptions("", &out, &errBuf), deps); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "2 MCP servers (Claude, codex) left running") {
		t.Fatalf("output missing MCP report:\n%s", got)
	}
	if !strings.Contains(got, "stops when its AI client quits") {
		t.Fatalf("output missing MCP explanation:\n%s", got)
	}
}

func TestRunStopCoreJSONResult(t *testing.T) {
	var out, errBuf bytes.Buffer
	calls := &stopCalls{}
	opts := testStopOptions("", &out, &errBuf)
	opts.jsonOut = true
	if exit := runStopCore(context.Background(), opts, runningStopDeps(calls)); exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	var res stopResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "stopped" {
		t.Fatalf("action = %q, want stopped", res.Action)
	}
	if res.App == nil || res.App.PID != 31 || !res.App.Graceful {
		t.Fatalf("app = %+v", res.App)
	}
	if res.Daemon == nil || res.Daemon.PID != 41 || !res.Daemon.Graceful {
		t.Fatalf("daemon = %+v", res.Daemon)
	}
}

func TestParseMCPPSLine(t *testing.T) {
	pid, ppid, args, ok := parseMCPPSLine("  4711   901 /usr/local/bin/canary mcp --profile monitor")
	if !ok || pid != 4711 || ppid != 901 {
		t.Fatalf("pid=%d ppid=%d ok=%v", pid, ppid, ok)
	}
	if len(args) != 4 || args[0] != "/usr/local/bin/canary" || args[1] != "mcp" {
		t.Fatalf("args = %v", args)
	}
	if _, _, _, ok := parseMCPPSLine("PID PPID ARGS"); ok {
		t.Fatal("header line parsed as a process")
	}
}

// AI hosts live under paths with spaces, which argv splitting would report
// as "Application" instead of the client's name.
func TestParseCommandNameLineKeepsSpacedPaths(t *testing.T) {
	pid, name, ok := parseCommandNameLine("  9169 /Users/o/Library/Application Support/Claude/claude.app/Contents/MacOS/claude")
	if !ok || pid != 9169 || name != "claude" {
		t.Fatalf("pid=%d name=%q ok=%v", pid, name, ok)
	}
	if _, _, ok := parseCommandNameLine("  9169"); ok {
		t.Fatal("a pid without a command parsed as a name")
	}
}
