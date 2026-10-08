package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/update"
)

const testAppSupervisorTarget = "gui/501/" + appLaunchAgentLabel

func testCurrentExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	return path
}

// testRestartStateHome isolates the quiesce record under a temporary
// XDG_STATE_HOME and clears the socket override so the stack path runs.
func testRestartStateHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", root)
	t.Setenv("CANARY_SOCKET", "")
	return root
}

func testAppLaunchAgentPath(root string) string {
	return filepath.Join(root, "Library", "LaunchAgents", appLaunchAgentLabel+".plist")
}

func writeTestAppLaunchAgent(t *testing.T, root, executable string, args ...string) string {
	t.Helper()
	path := testAppLaunchAgentPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\"><dict>\n")
	b.WriteString("<key>Label</key><string>" + appLaunchAgentLabel + "</string>\n")
	b.WriteString("<key>ProgramArguments</key><array>\n<string>" + executable + "</string>\n")
	for _, arg := range args {
		b.WriteString("<string>" + arg + "</string>\n")
	}
	b.WriteString("</array>\n<key>KeepAlive</key><true/>\n</dict></plist>\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestQuiesceMarker(t *testing.T, plistPath, executable string, quiescedAt time.Time, args ...string) string {
	t.Helper()
	path := restartQuiesceMarkerPath()
	sup := appSupervisor{Target: testAppSupervisorTarget, PlistPath: plistPath, Executable: executable, Args: args}
	if err := writeRestartQuiesceMarker(path, sup, quiescedAt); err != nil {
		t.Fatalf("write quiesce record: %v", err)
	}
	return path
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

// stoppedDaemonDeps fakes a daemon that was not running and starts cleanly.
func stoppedDaemonDeps(t *testing.T, onStart func()) restartDeps {
	t.Helper()
	return restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		stop: func(int, time.Duration) error {
			t.Fatal("no daemon was running; nothing to stop")
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			if onStart != nil {
				onStart()
			}
			return 92, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}
}

func TestRunRestartAllCoreRecordsQuiescedSupervisorUntilResumed(t *testing.T) {
	root := testRestartStateHome(t)
	exe := testCurrentExecutable(t)
	plistPath := writeTestAppLaunchAgent(t, root, exe, "app", "--remote")
	recordPath := restartQuiesceMarkerPath()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	loaded := true
	var recorded restartQuiesceMarker
	var recordMode os.FileMode
	sup := appSupervisor{Target: testAppSupervisorTarget, PID: 82, Executable: exe, Args: []string{"app", "--remote"}, PlistPath: plistPath}
	exit := runRestartAllCore(context.Background(), opts, stoppedDaemonDeps(t, func() {
		if loaded {
			t.Fatal("daemon restart began while the app supervisor was loaded")
		}
	}), appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{PID: 82, Command: exe + " app --remote", Args: []string{"app", "--remote"}, CurrentExecutable: true}, nil
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
			if !loaded {
				return appSupervisor{}, false
			}
			now := sup
			if recorded.Target != "" {
				now.PID = 83
			}
			return now, true
		},
		unload: func(context.Context, appSupervisor) error {
			info, err := os.Stat(recordPath)
			if err != nil {
				t.Fatalf("quiesce record must exist before bootout: %v", err)
			}
			recordMode = info.Mode().Perm()
			marker, exists, err := readRestartQuiesceMarker(recordPath)
			if err != nil || !exists {
				t.Fatalf("read quiesce record: exists=%v err=%v", exists, err)
			}
			recorded = marker
			loaded = false
			return nil
		},
		load: func(context.Context, appSupervisor) error {
			wait, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, _, err := dial.WithStartupLock(wait, dial.DefaultSocketPath()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("restart released reservation before app resume: %v", err)
			}
			if !fileExists(t, recordPath) {
				t.Fatal("quiesce record must survive until the job is loaded again")
			}
			loaded = true
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if recordMode != 0o600 {
		t.Fatalf("quiesce record mode = %o, want 600", recordMode)
	}
	if recorded.Version != restartQuiesceMarkerVersion || recorded.Target != testAppSupervisorTarget || recorded.PlistPath != plistPath || recorded.Executable != exe || !slices.Equal(recorded.Args, []string{"app", "--remote"}) {
		t.Fatalf("quiesce record = %+v", recorded)
	}
	if _, err := time.Parse(time.RFC3339, recorded.QuiescedAt); err != nil {
		t.Fatalf("quiesced_at %q: %v", recorded.QuiescedAt, err)
	}
	if fileExists(t, recordPath) {
		t.Fatal("quiesce record must be removed once the job is loaded again")
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App == nil || !res.App.Started || res.App.NewPID != 83 || res.App.QuiesceRecord != "" || res.App.QuiescedAt == "" {
		t.Fatalf("result = %+v", res.App)
	}
}

func TestRunRestartAllCoreDaemonFailureKeepsQuiesceRecord(t *testing.T) {
	root := testRestartStateHome(t)
	exe := testCurrentExecutable(t)
	plistPath := writeTestAppLaunchAgent(t, root, exe, "app", "--remote")
	recordPath := restartQuiesceMarkerPath()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	loaded := true
	sup := appSupervisor{Target: testAppSupervisorTarget, PID: 82, Executable: exe, Args: []string{"app", "--remote"}, PlistPath: plistPath}
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 0, rpc.HealthResult{}, errors.New("refusing exact-executable daemon start: pid 7 already owns /tmp/ibkr.lock")
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		stop: func(int, time.Duration) error {
			t.Fatal("supervised app must not be stopped directly")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("app must stay quiesced after a failed daemon stage")
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			if !loaded {
				return appSupervisor{}, false
			}
			return sup, true
		},
		unload: func(context.Context, appSupervisor) error {
			loaded = false
			return nil
		},
		load: func(context.Context, appSupervisor) error {
			t.Fatal("app must stay quiesced after a failed daemon stage")
			return nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, errBuf.String())
	}
	if !fileExists(t, recordPath) {
		t.Fatal("quiesce record must persist after a failed daemon stage")
	}
	for _, want := range []string{
		"app remains stopped so it cannot autospawn a competing daemon",
		"quiesce record at " + recordPath + " persists",
		"next successful",
	} {
		if !strings.Contains(errBuf.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, errBuf.String())
		}
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App == nil || res.App.Started || res.App.QuiesceRecord != recordPath || res.App.QuiescedAt == "" {
		t.Fatalf("result = %+v", res.App)
	}
}

func TestRunRestartAllCoreResumesSupervisorFromQuiesceRecord(t *testing.T) {
	root := testRestartStateHome(t)
	exe := testCurrentExecutable(t)
	plistPath := writeTestAppLaunchAgent(t, root, exe, "app", "--remote")
	quiescedAt := time.Date(2026, time.September, 30, 17, 16, 0, 0, time.UTC)
	recordPath := writeTestQuiesceMarker(t, plistPath, exe, quiescedAt, "app", "--remote")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	loaded := false
	var events []string
	var loadedSup appSupervisor
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			events = append(events, "daemon.find")
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			if loaded {
				t.Fatal("app supervisor was loaded before the daemon started")
			}
			events = append(events, "daemon.start")
			return 92, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			events = append(events, "app.find")
			return appProcess{}, errAppNotRunning
		},
		stop: func(int, time.Duration) error {
			t.Fatal("nothing to stop")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("a recorded supervisor must be bootstrapped, not started by hand")
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			if !loaded {
				return appSupervisor{}, false
			}
			return appSupervisor{Target: testAppSupervisorTarget, PID: 83, Executable: exe, Args: []string{"app", "--remote"}, PlistPath: plistPath}, true
		},
		unload: func(context.Context, appSupervisor) error {
			t.Fatal("nothing is loaded to unload")
			return nil
		},
		load: func(_ context.Context, sup appSupervisor) error {
			events = append(events, "app.load")
			loadedSup = sup
			loaded = true
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if got, want := strings.Join(events, ","), "app.find,daemon.find,daemon.start,app.load"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	if loadedSup.Target != testAppSupervisorTarget || loadedSup.PlistPath != plistPath || loadedSup.Executable != exe || !slices.Equal(loadedSup.Args, []string{"app", "--remote"}) {
		t.Fatalf("loaded supervisor = %+v", loadedSup)
	}
	if fileExists(t, recordPath) {
		t.Fatal("quiesce record must be removed after the job is loaded again")
	}
	for _, want := range []string{
		"was quiesced by an earlier restart at",
		"resumed app supervisor quiesced by an earlier restart at",
		"started daemon pid 92",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunRestartAppCoreResumesSupervisorFromQuiesceRecord(t *testing.T) {
	root := testRestartStateHome(t)
	exe := testCurrentExecutable(t)
	plistPath := writeTestAppLaunchAgent(t, root, exe, "app", "--remote")
	quiescedAt := time.Now().Add(-20 * 24 * time.Hour).UTC().Truncate(time.Second)
	recordPath := writeTestQuiesceMarker(t, plistPath, exe, quiescedAt, "app", "--remote")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	loaded := false
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		stop: func(int, time.Duration) error {
			t.Fatal("nothing to stop")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("a recorded supervisor must be bootstrapped, not started by hand")
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			if !loaded {
				return appSupervisor{}, false
			}
			return appSupervisor{Target: testAppSupervisorTarget, PID: 83, Executable: exe, Args: []string{"app", "--remote"}, PlistPath: plistPath}, true
		},
		kickstart: func(context.Context, string) error {
			t.Fatal("an unloaded job cannot be kickstarted")
			return nil
		},
		load: func(context.Context, appSupervisor) error {
			loaded = true
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if fileExists(t, recordPath) {
		t.Fatal("quiesce record must be removed after the job is loaded again")
	}
	if !strings.Contains(errBuf.String(), "days old") {
		t.Fatalf("stderr missing the record age:\n%s", errBuf.String())
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "started" || res.Reason != "resumed_quiesced_supervisor" || res.Supervisor != testAppSupervisorTarget || !res.Started || res.NewPID != 83 || res.WasRunning {
		t.Fatalf("result = %+v", res)
	}
	if res.QuiescedAt != quiescedAt.Format(time.RFC3339) || res.QuiesceRecord != "" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunRestartAllCoreDiscardsQuiesceRecordWhenPlistIsGone(t *testing.T) {
	root := testRestartStateHome(t)
	exe := testCurrentExecutable(t)
	recordPath := writeTestQuiesceMarker(t, testAppLaunchAgentPath(root), exe, time.Now(), "app", "--remote")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAllCore(context.Background(), opts, stoppedDaemonDeps(t, nil), appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{}, false
		},
		unload: func(context.Context, appSupervisor) error {
			t.Fatal("nothing is loaded to unload")
			return nil
		},
		load: func(context.Context, appSupervisor) error {
			t.Fatal("a record without its plist must not be bootstrapped")
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if fileExists(t, recordPath) {
		t.Fatal("a record whose plist is gone must be discarded")
	}
	for _, want := range []string{"discarding quiesce record", "no longer exists"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, errBuf.String())
		}
	}
	if !strings.Contains(out.String(), "no app was running; app not restarted") {
		t.Fatalf("stdout missing the not-running line:\n%s", out.String())
	}
}

func TestRunRestartAllCoreDiscardsQuiesceRecordForForeignExecutable(t *testing.T) {
	root := testRestartStateHome(t)
	plistPath := writeTestAppLaunchAgent(t, root, "/usr/bin/true", "app", "--remote")
	recordPath := writeTestQuiesceMarker(t, plistPath, "/usr/bin/true", time.Now(), "app", "--remote")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAllCore(context.Background(), opts, stoppedDaemonDeps(t, nil), appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{}, false
		},
		load: func(context.Context, appSupervisor) error {
			t.Fatal("a plist naming a foreign executable must not be bootstrapped")
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if fileExists(t, recordPath) {
		t.Fatal("a record whose plist names a foreign executable must be discarded")
	}
	for _, want := range []string{"discarding quiesce record", `plist points at "/usr/bin/true"`, "not the current installed Canary executable"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, errBuf.String())
		}
	}
}

func TestRunRestartStackCoreKeepsQuiesceRecordWhenDaemonLeftStopped(t *testing.T) {
	root := testRestartStateHome(t)
	exe := testCurrentExecutable(t)
	plistPath := writeTestAppLaunchAgent(t, root, exe, "app", "--remote")
	recordPath := writeTestQuiesceMarker(t, plistPath, exe, time.Now(), "app", "--remote")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartStackCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			t.Fatal("an update restart must not start a daemon that was not running")
			return 0, rpc.HealthResult{}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{}, false
		},
		load: func(context.Context, appSupervisor) error {
			t.Fatal("the app must not be resumed while the daemon stays stopped")
			return nil
		},
	}, restartStackBehavior{startDaemonWhenMissing: false})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if !fileExists(t, recordPath) {
		t.Fatal("quiesce record must persist while the daemon stays stopped")
	}
	for _, want := range []string{"stays stopped", "quiesce record persists"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunRestartAppCoreDiscardsStaleQuiesceRecordWhenSupervisorIsLoaded(t *testing.T) {
	root := testRestartStateHome(t)
	exe := testCurrentExecutable(t)
	plistPath := writeTestAppLaunchAgent(t, root, exe, "app", "--remote")
	recordPath := writeTestQuiesceMarker(t, plistPath, exe, time.Now(), "app", "--remote")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf}
	kicked := ""
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{PID: 90, Command: exe + " app --remote", Args: []string{"app", "--remote"}, CurrentExecutable: true}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("supervised restart must not SIGTERM the supervised process by hand")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("supervised restart must not spawn an unsupervised app process")
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			pid := 90
			if kicked != "" {
				pid = 91
			}
			return appSupervisor{Target: testAppSupervisorTarget, PID: pid, Executable: exe, Args: []string{"app", "--remote"}, PlistPath: plistPath}, true
		},
		kickstart: func(_ context.Context, target string) error {
			kicked = target
			return nil
		},
		load: func(context.Context, appSupervisor) error {
			t.Fatal("a loaded job must be kickstarted, not bootstrapped again")
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if kicked != testAppSupervisorTarget {
		t.Fatalf("kickstart target = %q", kicked)
	}
	if fileExists(t, recordPath) {
		t.Fatal("a quiesce record is stale while its job is loaded and must be discarded")
	}
	if !strings.Contains(out.String(), "discarded stale quiesce record") {
		t.Fatalf("stdout missing the stale-record line:\n%s", out.String())
	}
}
