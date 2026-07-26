package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/app"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/update"
)

const restartDefaultTimeout = 15 * time.Second
const restartDefaultAppAddr = "0.0.0.0:8765"

type restartOptions struct {
	jsonOut         bool
	force           bool
	app             bool
	timeout         time.Duration
	appAddr         string
	appAddrSet      bool
	appPublicURL    string
	appPublicURLSet bool
	appRemote       bool
	appRemoteSet    bool
	appRemoteURL    string
	appRemoteURLSet bool
	appStateDir     string
	appStateDirSet  bool
	out             io.Writer
	err             io.Writer
}

type restartDeps struct {
	find           func(context.Context, string) (update.DaemonProcess, error)
	stop           func(int, time.Duration) error
	kill           func(int, time.Duration) error
	startAndHealth func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error)
}

type restartResult struct {
	Action     string            `json:"action"`
	Target     string            `json:"target"`
	WasRunning bool              `json:"was_running"`
	Started    bool              `json:"started"`
	Forced     bool              `json:"forced"`
	Graceful   bool              `json:"graceful"`
	OldPID     int               `json:"old_pid,omitempty"`
	NewPID     int               `json:"new_pid,omitempty"`
	OldCommand string            `json:"old_command,omitempty"`
	Foreground bool              `json:"foreground,omitempty"`
	SocketPath string            `json:"socket_path"`
	LockPath   string            `json:"lock_path"`
	Health     rpc.HealthResult  `json:"health"`
	App        *appRestartResult `json:"app,omitempty"`
	ElapsedMS  int64             `json:"elapsed_ms"`
}

type appProcess struct {
	PID               int
	Command           string
	Args              []string
	CurrentExecutable bool
}

type appRestartDeps struct {
	find            func(context.Context) (appProcess, error)
	stop            func(int, time.Duration) error
	kill            func(int, time.Duration) error
	start           func(context.Context, []string) (int, error)
	executablePaths map[string]struct{}
	// supervisor reports a loaded launchd job owning the app. migrate performs
	// the one-shot pre-rename executable rewrite/reload; kickstart restarts an
	// already-canonical job in place. All are nil outside macOS wiring and most
	// tests.
	supervisor func(context.Context) (appSupervisor, bool)
	migrate    func(context.Context, appSupervisor) error
	kickstart  func(context.Context, string) error
	rewrite    func(context.Context, appSupervisor) error
	unload     func(context.Context, appSupervisor) error
	load       func(context.Context, appSupervisor) error
}

type appRestartBehavior struct {
	startWhenMissing bool
	prefix           string
	pairHint         bool
}

type appRestartResult struct {
	Action     string   `json:"action"`
	Reason     string   `json:"reason,omitempty"`
	Target     string   `json:"target"`
	Supervisor string   `json:"supervisor,omitempty"`
	WasRunning bool     `json:"was_running"`
	Started    bool     `json:"started"`
	Forced     bool     `json:"forced"`
	Graceful   bool     `json:"graceful"`
	OldPID     int      `json:"old_pid,omitempty"`
	NewPID     int      `json:"new_pid,omitempty"`
	OldCommand string   `json:"old_command,omitempty"`
	Args       []string `json:"args,omitempty"`
	ElapsedMS  int64    `json:"elapsed_ms"`
}

var (
	errAppNotRunning  = errors.New("app not running")
	errAppUnverified  = errors.New("app process could not be verified")
	errAppStopTimeout = errors.New("app stop timed out")
)

// RunRestart is the top-level `canary restart` entrypoint. It intentionally does
// not take an Env: restart is local process management and must run before the
// normal autospawn+dial path in cmd/canary/main.go.
func RunRestart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts := restartOptions{
		timeout: restartDefaultTimeout,
		out:     stdout,
		err:     stderr,
	}
	env := &Env{Stdout: stdout, Stderr: stderr}
	fs := flagSet(env, "restart")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit machine-readable restart result")
	fs.BoolVar(&opts.force, "force", false, "send SIGKILL if graceful SIGTERM does not stop the target process before --timeout")
	fs.BoolVar(&opts.app, "app", false, "restart/start only the HyperServe app process instead of the daemon")
	fs.DurationVar(&opts.timeout, "timeout", restartDefaultTimeout, "how long to wait for graceful process stop before failing or forcing")
	fs.StringVar(&opts.appAddr, "addr", "", "app listen address to use with --app")
	fs.StringVar(&opts.appPublicURL, "public-url", "", "app public URL to use with --app")
	fs.BoolVar(&opts.appRemote, "remote", false, "enable the outbound Cloudflare Worker relay with --app")
	fs.StringVar(&opts.appRemoteURL, "remote-url", "", "Cloudflare Worker relay base URL to use with --app")
	fs.StringVar(&opts.appStateDir, "state-dir", "", "app state directory to use with --app")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	opts.appAddrSet = restartFlagWasSet(fs, "addr")
	opts.appPublicURLSet = restartFlagWasSet(fs, "public-url")
	opts.appRemoteSet = restartFlagWasSet(fs, "remote")
	opts.appRemoteURLSet = restartFlagWasSet(fs, "remote-url")
	opts.appStateDirSet = restartFlagWasSet(fs, "state-dir")
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	if !opts.app && (opts.appAddrSet || opts.appPublicURLSet || opts.appRemoteSet || opts.appRemoteURLSet || opts.appStateDirSet) {
		fmt.Fprintf(stderr, "%s restart: --addr, --public-url, --remote, --remote-url, and --state-dir require --app\n", productidentity.Executable)
		return 2
	}
	if opts.timeout <= 0 {
		fmt.Fprintf(stderr, "%s restart: --timeout must be positive\n", productidentity.Executable)
		return 2
	}
	appDeps := productionAppRestartDeps()
	if opts.app {
		return runRestartAppCore(ctx, &opts, appDeps)
	}
	return runRestartAllCore(ctx, &opts, productionDaemonRestartDeps(), appDeps)
}

func productionDaemonRestartDeps() restartDeps {
	if executable, err := os.Executable(); err == nil && strings.TrimSpace(executable) != "" {
		return productionDaemonRestartDepsForExecutable(executable)
	}
	return restartDeps{
		find:           update.FindDaemonProcess,
		stop:           update.StopDaemon,
		kill:           update.KillDaemon,
		startAndHealth: startDaemonAndFetchHealth,
	}
}

func productionDaemonRestartDepsForExecutable(executable string) restartDeps {
	return restartDeps{
		find: update.FindDaemonProcess,
		stop: update.StopDaemon,
		kill: update.KillDaemon,
		startAndHealth: func(ctx context.Context, socketPath string, progress io.Writer, quiet bool) (int, rpc.HealthResult, error) {
			return startDaemonAndFetchHealthUsing(ctx, socketPath, progress, quiet, func(ctx context.Context, socketPath string) (*dial.Conn, error) {
				return dial.AutospawnAndConnectContextFromExecutableWithTimeout(ctx, socketPath, executable, restartStartupTimeout(ctx))
			})
		},
	}
}

func productionAppRestartDeps() appRestartDeps {
	return productionAppRestartDepsForExecutable("")
}

func productionAppRestartDepsForExecutable(executable string) appRestartDeps {
	executablePaths := currentExecutablePaths()
	start := startAppProcess
	currentExecutable := strings.TrimSpace(executable)
	if currentExecutable != "" {
		executablePaths = executablePathVariants(executable)
		start = func(ctx context.Context, args []string) (int, error) {
			return startAppProcessFromExecutable(ctx, executable, args)
		}
	} else if bin, err := os.Executable(); err == nil {
		currentExecutable = bin
	}
	return appRestartDeps{
		find: func(ctx context.Context) (appProcess, error) {
			return findAppProcessForExecutables(ctx, executablePaths)
		},
		stop:            stopAppProcess,
		kill:            killAppProcess,
		start:           start,
		executablePaths: executablePaths,
		supervisor:      findAppLaunchAgent,
		migrate: func(ctx context.Context, sup appSupervisor) error {
			return migrateAppLaunchAgent(ctx, sup, currentExecutable)
		},
		kickstart: kickstartLaunchAgent,
		rewrite: func(_ context.Context, sup appSupervisor) error {
			return rewriteAppLaunchAgentForMigration(sup, currentExecutable)
		},
		unload: unloadAppLaunchAgent,
		load:   loadAppLaunchAgent,
	}
}

func runRestartCore(ctx context.Context, opts *restartOptions, deps restartDeps) int {
	res, exit := restartDaemon(ctx, opts, deps)
	if exit != 0 {
		return exit
	}
	if opts.jsonOut {
		return printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res)
	}
	renderRestartStarted(opts.out, res)
	return 0
}

type restartStackBehavior struct {
	startDaemonWhenMissing bool
}

func runRestartAllCore(ctx context.Context, opts *restartOptions, daemonDeps restartDeps, appDeps appRestartDeps) int {
	return runRestartStackCore(ctx, opts, daemonDeps, appDeps, restartStackBehavior{startDaemonWhenMissing: true})
}

func runRestartStackCore(ctx context.Context, opts *restartOptions, daemonDeps restartDeps, appDeps appRestartDeps, behavior restartStackBehavior) int {
	// App discovery is by process name (findAppProcess) and cannot tell
	// which daemon scope a found app belongs to. When CANARY_SOCKET points
	// at a non-default scope, the only app this command could find is one
	// outside that scope, so implicit app management must stay hands-off.
	// Decide that before either process can be mutated. `canary restart --app`
	// remains available as the explicit, separate path.
	var appRes *appRestartResult
	var appPlan appStackRestartPlan
	if dial.SocketPathOverridden() {
		appRes = &appRestartResult{Action: "skipped", Reason: "socket_overridden", Target: "app"}
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s restart: CANARY_SOCKET is set; leaving the app untouched and restarting only that daemon scope (use `%s restart --app` as a separate explicit, non-atomic operation)\n", productidentity.Executable, productidentity.Executable)
		}
	} else {
		var appExit int
		appPlan, appExit = quiesceAppForDaemonRestart(ctx, opts, appDeps)
		if appExit != 0 {
			fmt.Fprintf(opts.err, "%s restart: app quiesce failed; daemon was not touched\n", productidentity.Executable)
			return appExit
		}
		if appPlan.ran {
			appRes = &appPlan.result
		}
	}

	res, exit := restartDaemonWithBehavior(ctx, opts, daemonDeps, behavior.startDaemonWhenMissing)
	res.App = appRes
	if exit != 0 {
		if appPlan.ran {
			fmt.Fprintf(opts.err, "%s restart: daemon stage failed while the app was quiesced; the app remains stopped so it cannot autospawn a competing daemon (fix the reported failure and rerun `%s restart`)\n", productidentity.Executable, productidentity.Executable)
		}
		if opts.jsonOut {
			if jsonExit := printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res); jsonExit != 0 {
				return jsonExit
			}
		}
		return exit
	}
	if appPlan.ran && !res.Started {
		fmt.Fprintf(opts.err, "%s restart: daemon remained stopped while the app was quiesced; refusing to resume the app because it would autospawn a daemon outside this restart stage\n", productidentity.Executable)
		if opts.jsonOut {
			if jsonExit := printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res); jsonExit != 0 {
				return jsonExit
			}
		}
		return 1
	}
	if appPlan.ran {
		completed, appExit := resumeAppAfterDaemonRestart(ctx, opts, appDeps, appPlan)
		res.App = &completed
		if appExit != 0 {
			fmt.Fprintf(opts.err, "%s restart: daemon restarted but app resume failed; the daemon remains running (fix the reported failure and rerun `%s restart`)\n", productidentity.Executable, productidentity.Executable)
			if opts.jsonOut {
				if jsonExit := printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res); jsonExit != 0 {
					return jsonExit
				}
			}
			return appExit
		}
	}
	if !opts.jsonOut && res.Started {
		renderRestartStarted(opts.out, res)
	}
	if opts.jsonOut {
		return printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res)
	}
	return 0
}

type appStackRestartPlan struct {
	result      appRestartResult
	args        []string
	supervisor  *appSupervisor
	currentPath map[string]struct{}
	startedAt   time.Time
	ran         bool
}

// quiesceAppForDaemonRestart removes every running app instance managed by a
// stack restart before the daemon socket can disappear. A running app is a
// daemon client with autospawn authority; restarting it before the daemon
// creates a race in which the app and restart command compete for the daemon
// lock. The app is resumed only after daemon health succeeds.
func quiesceAppForDaemonRestart(ctx context.Context, opts *restartOptions, deps appRestartDeps) (appStackRestartPlan, int) {
	startedAt := time.Now()
	prefix := productidentity.Executable + " restart"
	plan := appStackRestartPlan{
		result:    appRestartResult{Action: "restarted", Target: "app", Args: []string{"app"}},
		args:      []string{"app"},
		startedAt: startedAt,
	}
	proc, findErr := deps.find(ctx)
	if deps.supervisor != nil {
		if sup, ok := deps.supervisor(ctx); ok {
			if supervisedRestartApplies(proc, findErr, sup) {
				return quiesceSupervisedAppForDaemonRestart(ctx, opts, deps, prefix, proc, findErr, sup, plan)
			}
			if findErr == nil {
				// A different --state-dir isolates the app lock, not the daemon
				// socket. The independent process and the loaded shared supervisor
				// can therefore both autospawn the default daemon. This single-result
				// restart contract cannot stop and faithfully restore both app
				// lifecycles, so leave every process untouched rather than restart
				// the daemon under one still-loaded client.
				fmt.Fprintf(opts.err, "%s: an independent app process and the shared launchd supervisor %s are both present; refusing stack restart because both may autospawn the default daemon (stop the isolated app or its supervisor, then rerun)\n", prefix, sup.Target)
				return plan, 1
			}
			// Ambiguous or failed process discovery retains its exact error and
			// flows to the fail-closed switch below. It is not evidence that
			// launchd safely owns every possible app.
		}
	}
	switch {
	case findErr == nil:
		plan.ran = true
		plan.result.WasRunning = true
		plan.result.OldPID = proc.PID
		plan.result.OldCommand = proc.Command
		if len(proc.Args) > 0 {
			plan.args = append([]string(nil), proc.Args...)
		}
		plan.result.Args = append([]string(nil), plan.args...)
		forced, exit := stopAppWithPolicy(opts, deps, prefix, proc.PID)
		if exit != 0 {
			return plan, exit
		}
		plan.result.Forced = forced
		plan.result.Graceful = !forced
		return plan, 0
	case errors.Is(findErr, errAppNotRunning):
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s: no app was running; app not restarted\n", prefix)
		}
		return plan, 0
	default:
		fmt.Fprintf(opts.err, "%s: %v\n", prefix, findErr)
		return plan, 1
	}
}

func quiesceSupervisedAppForDaemonRestart(ctx context.Context, opts *restartOptions, deps appRestartDeps, prefix string, proc appProcess, findErr error, sup appSupervisor, plan appStackRestartPlan) (appStackRestartPlan, int) {
	plan.ran = true
	plan.supervisor = &sup
	plan.currentPath = restartExecutablePaths(deps)
	plan.args = append([]string(nil), sup.Args...)
	plan.result.Supervisor = sup.Target
	plan.result.Args = append([]string(nil), sup.Args...)
	if sup.ParseError != "" || !isAppServerArgs(sup.Args) {
		fmt.Fprintf(opts.err, "%s: launchd job %s is untrusted or unparseable (%s); refusing to rewrite, stop, or restart it\n", prefix, sup.Target, supervisorParseFailure(sup))
		return plan, 1
	}
	migrate := !executablePathMatches(sup.Executable, plan.currentPath)
	if migrate && (filepath.Base(sup.Executable) != productidentity.PreUpgradeExecutable || deps.rewrite == nil) {
		fmt.Fprintf(opts.err, "%s: launchd job %s points at %q, not the current installed Canary executable or the explicit pre-upgrade executable; refusing to rewrite or restart it\n", prefix, sup.Target, sup.Executable)
		return plan, 1
	}
	if deps.unload == nil || deps.load == nil {
		fmt.Fprintf(opts.err, "%s: staged launchd restart adapters are unavailable\n", prefix)
		return plan, 1
	}
	if findErr == nil && proc.PID > 0 {
		plan.result.WasRunning = true
		plan.result.OldPID = proc.PID
		plan.result.OldCommand = proc.Command
		if proc.PID != sup.PID {
			forced, exit := stopAppWithPolicy(opts, deps, prefix, proc.PID)
			if exit != 0 {
				return plan, exit
			}
			plan.result.Forced = forced
			plan.result.Graceful = !forced
		}
	}
	if migrate {
		if err := deps.rewrite(ctx, sup); err != nil {
			fmt.Fprintf(opts.err, "%s: rewrite pre-upgrade launchd app supervisor: %v\n", prefix, err)
			return plan, 1
		}
		plan.result.Reason = "migrated_pre_upgrade_supervisor"
	}
	if err := deps.unload(ctx, sup); err != nil {
		fmt.Fprintf(opts.err, "%s: quiesce launchd app supervisor: %v\n", prefix, err)
		return plan, 1
	}
	if !opts.jsonOut {
		fmt.Fprintf(opts.out, "%s: quiesced supervised app before daemon restart (%s)\n", prefix, sup.Target)
	}
	return plan, 0
}

func resumeAppAfterDaemonRestart(ctx context.Context, opts *restartOptions, deps appRestartDeps, plan appStackRestartPlan) (appRestartResult, int) {
	res := plan.result
	prefix := productidentity.Executable + " restart"
	if plan.supervisor == nil {
		newPID, err := deps.start(ctx, plan.args)
		if err != nil {
			fmt.Fprintf(opts.err, "%s: start app after daemon restart: %v\n", prefix, err)
			return res, 1
		}
		res.Started = true
		res.NewPID = newPID
		res.ElapsedMS = time.Since(plan.startedAt).Milliseconds()
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s: started app pid %d after daemon restart\n", prefix, newPID)
		}
		return res, 0
	}
	sup := *plan.supervisor
	if err := deps.load(ctx, sup); err != nil {
		fmt.Fprintf(opts.err, "%s: resume launchd app supervisor: %v\n", prefix, err)
		return res, 1
	}
	newPID, err := waitForCanonicalSupervisor(ctx, opts.timeout, deps.supervisor, sup, plan.currentPath, res.OldPID)
	if err != nil {
		fmt.Fprintf(opts.err, "%s: %v\n", prefix, err)
		return res, 1
	}
	res.Started = true
	res.NewPID = newPID
	res.ElapsedMS = time.Since(plan.startedAt).Milliseconds()
	if !opts.jsonOut {
		fmt.Fprintf(opts.out, "%s: resumed supervised app pid %d after daemon restart (%s)\n", prefix, newPID, sup.Target)
	}
	return res, 0
}

func restartDaemon(ctx context.Context, opts *restartOptions, deps restartDeps) (restartResult, int) {
	return restartDaemonWithBehavior(ctx, opts, deps, true)
}

func restartDaemonWithBehavior(ctx context.Context, opts *restartOptions, deps restartDeps, startWhenMissing bool) (restartResult, int) {
	startedAt := time.Now()
	socketPath := dial.DefaultSocketPath()
	lockPath := dial.LockPath(socketPath)
	res := restartResult{
		Action:     "started",
		Target:     "daemon",
		SocketPath: socketPath,
		LockPath:   lockPath,
	}

	proc, err := deps.find(ctx, socketPath)
	switch {
	case err == nil:
		res.Action = "restarted"
		res.WasRunning = true
		res.OldPID = proc.PID
		res.OldCommand = proc.Command
		res.Foreground = proc.Foreground
		res.SocketPath = proc.SocketPath
		res.LockPath = proc.LockPath
		stopErr := deps.stop(proc.PID, opts.timeout)
		if stopErr != nil {
			if !opts.force || !errors.Is(stopErr, update.ErrStopTimeout) {
				fmt.Fprintf(opts.err, "%s restart: %v\n", productidentity.Executable, stopErr)
				if !opts.force && errors.Is(stopErr, update.ErrStopTimeout) {
					fmt.Fprintf(opts.err, "%s restart: re-run with --force to send SIGKILL after the graceful timeout\n", productidentity.Executable)
				}
				return res, 1
			}
			if !opts.jsonOut {
				fmt.Fprintf(opts.out, "%s restart: daemon pid %d ignored SIGTERM; forcing SIGKILL\n", productidentity.Executable, proc.PID)
			}
			if err := deps.kill(proc.PID, opts.timeout); err != nil {
				fmt.Fprintf(opts.err, "%s restart: %v\n", productidentity.Executable, err)
				return res, 1
			}
			res.Forced = true
		} else {
			res.Graceful = true
		}
		if !opts.jsonOut {
			mode := "gracefully"
			if res.Forced {
				mode = "with SIGKILL"
			}
			fmt.Fprintf(opts.out, "%s restart: stopped daemon pid %d %s\n", productidentity.Executable, proc.PID, mode)
			fmt.Fprintf(opts.out, "%s restart: starting daemon\n", productidentity.Executable)
		}
	case errors.Is(err, update.ErrDaemonNotRunning):
		if !startWhenMissing {
			res.Action = "not_running"
			res.ElapsedMS = time.Since(startedAt).Milliseconds()
			if !opts.jsonOut {
				fmt.Fprintf(opts.out, "%s restart: no daemon was running; daemon left stopped\n", productidentity.Executable)
			}
			return res, 0
		}
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s restart: no daemon was running; starting daemon\n", productidentity.Executable)
		}
	default:
		fmt.Fprintf(opts.err, "%s restart: %v\n", productidentity.Executable, err)
		return res, 1
	}

	startCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		startCtx, cancel = context.WithTimeout(ctx, opts.timeout)
	}
	defer cancel()
	newPID, health, err := deps.startAndHealth(startCtx, socketPath, opts.err, opts.jsonOut)
	if err != nil {
		fmt.Fprintf(opts.err, "%s restart: start daemon: %v\n", productidentity.Executable, err)
		return res, 1
	}
	res.Started = true
	res.NewPID = newPID
	res.Health = health
	res.ElapsedMS = time.Since(startedAt).Milliseconds()
	return res, 0
}

func restartStartupTimeout(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			return remaining
		}
	}
	return restartDefaultTimeout
}

func runRestartAppCore(ctx context.Context, opts *restartOptions, deps appRestartDeps) int {
	res, _, exit := restartApp(ctx, opts, deps, appRestartBehavior{
		startWhenMissing: true,
		prefix:           productidentity.Executable + " restart --app",
		pairHint:         true,
	})
	if exit != 0 {
		return exit
	}
	if opts.jsonOut {
		return printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res)
	}
	return 0
}

func restartApp(ctx context.Context, opts *restartOptions, deps appRestartDeps, behavior appRestartBehavior) (appRestartResult, bool, int) {
	prefix := strings.TrimSpace(behavior.prefix)
	if prefix == "" {
		prefix = productidentity.Executable + " restart --app"
	}
	startedAt := time.Now()
	res := appRestartResult{Action: "started", Target: "app", Args: []string{"app"}}
	args := []string{"app"}
	argsFinalized := false
	finalizeArgs := func() {
		if argsFinalized {
			return
		}
		args = appArgsWithRestartOverrides(args, opts)
		res.Args = append([]string(nil), args...)
		argsFinalized = true
	}

	proc, err := deps.find(ctx)
	if deps.supervisor != nil {
		if sup, ok := deps.supervisor(ctx); ok && supervisedRestartApplies(proc, err, sup) {
			return restartSupervisedApp(ctx, opts, deps, prefix, startedAt, proc, err, sup)
		}
	}
	switch {
	case err == nil:
		res.Action = "restarted"
		res.WasRunning = true
		res.OldPID = proc.PID
		res.OldCommand = proc.Command
		if len(proc.Args) > 0 {
			args = append([]string(nil), proc.Args...)
		}
		finalizeArgs()
		forced, exit := stopAppWithPolicy(opts, deps, prefix, proc.PID)
		if exit != 0 {
			return res, true, exit
		}
		res.Forced = forced
		res.Graceful = !forced
		if restarted, ok, err := waitForAppRespawn(ctx, deps.find, args, 2*time.Second); err != nil {
			fmt.Fprintf(opts.err, "%s: %v\n", prefix, err)
			return res, true, 1
		} else if ok {
			res.Started = true
			res.NewPID = restarted.PID
			res.ElapsedMS = time.Since(startedAt).Milliseconds()
			if !opts.jsonOut {
				fmt.Fprintf(opts.out, "%s: app respawned by supervisor pid %d\n", prefix, restarted.PID)
			}
			return res, true, 0
		}
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s: starting app\n", prefix)
		}
	case errors.Is(err, errAppNotRunning):
		finalizeArgs()
		if !behavior.startWhenMissing {
			res.Action = "not_running"
			res.ElapsedMS = time.Since(startedAt).Milliseconds()
			if !opts.jsonOut {
				fmt.Fprintf(opts.out, "%s: no app was running; app not restarted\n", prefix)
			}
			return res, false, 0
		}
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s: no app was running; starting app\n", prefix)
		}
	default:
		fmt.Fprintf(opts.err, "%s: %v\n", prefix, err)
		return res, false, 1
	}

	finalizeArgs()
	newPID, err := deps.start(ctx, args)
	if err != nil {
		fmt.Fprintf(opts.err, "%s: start app: %v\n", prefix, err)
		return res, true, 1
	}
	res.Started = true
	res.NewPID = newPID
	res.Args = append([]string(nil), args...)
	res.ElapsedMS = time.Since(startedAt).Milliseconds()
	if !opts.jsonOut {
		fmt.Fprintf(opts.out, "%s: started app pid %d\n", prefix, newPID)
		if behavior.pairHint {
			fmt.Fprintf(opts.out, "%s: pair a phone with `%s app pair`\n", prefix, productidentity.Executable)
		}
	}
	return res, true, 0
}

// stopAppWithPolicy SIGTERMs pid with the graceful/force policy shared by
// the supervised and unsupervised restart paths, reporting forced=true when
// SIGKILL was needed. A non-zero exit means the stop failed.
func stopAppWithPolicy(opts *restartOptions, deps appRestartDeps, prefix string, pid int) (forced bool, exit int) {
	stopErr := deps.stop(pid, opts.timeout)
	if stopErr != nil {
		if !opts.force || !errors.Is(stopErr, errAppStopTimeout) {
			fmt.Fprintf(opts.err, "%s: %v\n", prefix, stopErr)
			if !opts.force && errors.Is(stopErr, errAppStopTimeout) {
				fmt.Fprintf(opts.err, "%s: re-run with --force to send SIGKILL after the graceful timeout\n", prefix)
			}
			return false, 1
		}
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s: app pid %d ignored SIGTERM; forcing SIGKILL\n", prefix, pid)
		}
		if err := deps.kill(pid, opts.timeout); err != nil {
			fmt.Fprintf(opts.err, "%s: %v\n", prefix, err)
			return false, 1
		}
		forced = true
	}
	if !opts.jsonOut {
		mode := "gracefully"
		if forced {
			mode = "with SIGKILL"
		}
		fmt.Fprintf(opts.out, "%s: stopped app pid %d %s\n", prefix, pid, mode)
	}
	return forced, 0
}

// supervisedRestartApplies reports whether the launchd job owns this
// restart. The app lock lives in the state directory, so lock identity —
// not pid or argv equality — is the orphan test: a found process that
// resolves to the job's state dir is the supervised app or its orphan,
// while one resolving elsewhere is an independent instance (an isolated
// smoke or preview app with its own --state-dir) — kickstarting the
// shared host in its place would restart the wrong app and strand the
// instance the caller asked about.
func supervisedRestartApplies(proc appProcess, findErr error, sup appSupervisor) bool {
	if findErr != nil {
		return errors.Is(findErr, errAppNotRunning)
	}
	if proc.PID == sup.PID {
		return true
	}
	return appArgsStateDir(proc.Args) == appArgsStateDir(sup.Args)
}

// appArgsStateDir resolves the state directory a `canary app` argv locks:
// the --state-dir value when present, else the shared default (a plist
// whose arguments could not be parsed resolves to the default too).
// Symlinked spellings (macOS /tmp vs /private/tmp) resolve to one
// canonical path so equal locks never read as different.
func appArgsStateDir(args []string) string {
	dir := strings.TrimSpace(appValueArg(args, "state-dir"))
	if dir == "" {
		dir = app.DefaultStateDir()
	}
	dir = filepath.Clean(dir)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != "" {
		return resolved
	}
	return dir
}

// restartSupervisedApp restarts the app through its launchd job. Stopping
// the supervised process by hand races launchd's KeepAlive respawn: whoever
// loses the race leaves an orphan holding the app state lock while launchd
// crash-loops against it, and the app then runs without any supervisor.
func restartSupervisedApp(ctx context.Context, opts *restartOptions, deps appRestartDeps, prefix string, startedAt time.Time, proc appProcess, findErr error, sup appSupervisor) (appRestartResult, bool, int) {
	res := appRestartResult{Action: "restarted", Target: "app", Supervisor: sup.Target, Args: sup.Args}
	if sup.ParseError != "" || !isAppServerArgs(sup.Args) {
		fmt.Fprintf(opts.err, "%s: launchd job %s is untrusted or unparseable (%s); refusing to rewrite, stop, or restart it (repair it with `%s setup app`, reload it, then rerun)\n", prefix, sup.Target, supervisorParseFailure(sup), productidentity.Executable)
		return res, true, 1
	}
	if opts.appAddrSet || opts.appPublicURLSet || opts.appRemoteSet || opts.appRemoteURLSet || opts.appStateDirSet {
		fmt.Fprintf(opts.err, "%s: app flag overrides do not apply to the launchd-supervised app (%s); rewrite and reload its plist with `%s setup app`, then rerun\n", prefix, sup.Target, productidentity.Executable)
		return res, true, 1
	}
	currentPaths := restartExecutablePaths(deps)
	migrate := !executablePathMatches(sup.Executable, currentPaths)
	if migrate && (filepath.Base(sup.Executable) != productidentity.PreUpgradeExecutable || deps.migrate == nil) {
		fmt.Fprintf(opts.err, "%s: launchd job %s points at %q, not the current installed Canary executable or the explicit pre-upgrade executable; refusing to rewrite or restart it\n", prefix, sup.Target, sup.Executable)
		return res, true, 1
	}
	if findErr == nil && proc.PID > 0 {
		res.WasRunning = true
		res.OldPID = proc.PID
		res.OldCommand = proc.Command
		if proc.PID != sup.PID {
			// Orphan from an earlier hand-spawned restart: it holds the app
			// state lock, so launchd's respawns crash-loop until it is gone.
			if !opts.jsonOut {
				fmt.Fprintf(opts.out, "%s: stopping unsupervised app pid %d so the launchd job can own the app again\n", prefix, proc.PID)
			}
			forced, exit := stopAppWithPolicy(opts, deps, prefix, proc.PID)
			if exit != 0 {
				return res, true, exit
			}
			res.Forced = forced
			res.Graceful = !forced
		}
	}
	if migrate {
		if err := deps.migrate(ctx, sup); err != nil {
			fmt.Fprintf(opts.err, "%s: migrate pre-upgrade launchd app supervisor: %v\n", prefix, err)
			return res, true, 1
		}
		res.Reason = "migrated_pre_upgrade_supervisor"
	} else {
		if deps.kickstart == nil {
			fmt.Fprintf(opts.err, "%s: launchd restart adapter is unavailable; daemon was not touched\n", prefix)
			return res, true, 1
		}
		if err := deps.kickstart(ctx, sup.Target); err != nil {
			fmt.Fprintf(opts.err, "%s: %v\n", prefix, err)
			return res, true, 1
		}
	}
	newPID, err := waitForCanonicalSupervisor(ctx, opts.timeout, deps.supervisor, sup, currentPaths, res.OldPID)
	if err != nil {
		fmt.Fprintf(opts.err, "%s: %v\n", prefix, err)
		return res, true, 1
	}
	res.Started = true
	res.NewPID = newPID
	res.ElapsedMS = time.Since(startedAt).Milliseconds()
	if !opts.jsonOut {
		action := "kickstart"
		if migrate {
			action = "one-shot executable migration"
		}
		fmt.Fprintf(opts.out, "%s: restarted supervised app pid %d via launchctl %s (%s)\n", prefix, res.NewPID, action, sup.Target)
	}
	return res, true, 0
}

func waitForCanonicalSupervisor(ctx context.Context, timeout time.Duration, find func(context.Context) (appSupervisor, bool), before appSupervisor, currentPaths map[string]struct{}, oldPID int) (int, error) {
	if find == nil {
		return 0, errors.New("launchd supervisor verification adapter is unavailable")
	}
	// launchd may throttle the respawn (about 10s after a crash-loop), so
	// the wait budget must comfortably exceed the throttle interval. Accept
	// the new pid only after two consecutive samples agree: the first spawn
	// can lose a short lock race and be replaced by a throttled second spawn.
	deadline := time.Now().Add(max(timeout, 25*time.Second))
	lastPID := 0
	for {
		now, ok := find(ctx)
		canonical := ok &&
			now.ParseError == "" &&
			now.Target == before.Target &&
			executablePathMatches(now.Executable, currentPaths) &&
			slices.Equal(now.Args, before.Args)
		if canonical && now.PID > 0 && now.PID != before.PID && now.PID != oldPID {
			if now.PID == lastPID {
				return now.PID, nil
			}
			lastPID = now.PID
		} else {
			lastPID = 0
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("launchd did not respawn %s in time; check `launchctl print %s` and ~/Library/Logs/ibkr/app.err.log", before.Target, before.Target)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func supervisorParseFailure(sup appSupervisor) string {
	if strings.TrimSpace(sup.ParseError) != "" {
		return sup.ParseError
	}
	return "ProgramArguments is not a Canary app server command"
}

func restartFlagWasSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

func appArgsWithRestartOverrides(args []string, opts *restartOptions) []string {
	if len(args) == 0 {
		args = []string{"app"}
	}
	out := append([]string(nil), args...)
	if opts == nil {
		return out
	}
	if opts.appAddrSet {
		out = setAppValueArg(out, "addr", strings.TrimSpace(opts.appAddr))
		if !opts.appPublicURLSet {
			out = removeAppValueArg(out, "public-url")
		}
	}
	if opts.appPublicURLSet {
		out = setAppValueArg(out, "public-url", strings.TrimSpace(opts.appPublicURL))
	}
	if opts.appRemoteSet {
		if opts.appRemote {
			out = setAppBoolArg(out, "remote")
		} else {
			out = removeAppBoolArg(out, "remote")
		}
	}
	if opts.appRemoteURLSet {
		out = setAppValueArg(out, "remote-url", strings.TrimSpace(opts.appRemoteURL))
	}
	if opts.appStateDirSet {
		out = setAppValueArg(out, "state-dir", strings.TrimSpace(opts.appStateDir))
	}
	return out
}

func setAppBoolArg(args []string, name string) []string {
	out := removeAppBoolArg(args, name)
	return append(out, "--"+name)
}

func removeAppBoolArg(args []string, name string) []string {
	flagName := "--" + name
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == flagName || strings.HasPrefix(arg, flagName+"=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func setAppValueArg(args []string, name, value string) []string {
	out := removeAppValueArg(args, name)
	return append(out, "--"+name, value)
}

func removeAppValueArg(args []string, name string) []string {
	flagName := "--" + name
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == flagName:
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(arg, flagName+"="):
			continue
		default:
			out = append(out, arg)
		}
	}
	return out
}

func startDaemonAndFetchHealth(ctx context.Context, socketPath string, progress io.Writer, quiet bool) (int, rpc.HealthResult, error) {
	return startDaemonAndFetchHealthUsing(ctx, socketPath, progress, quiet, dial.AutospawnAndConnectContext)
}

type daemonConnectFunc func(context.Context, string) (*dial.Conn, error)

func startDaemonAndFetchHealthUsing(ctx context.Context, socketPath string, progress io.Writer, quiet bool, connect daemonConnectFunc) (int, rpc.HealthResult, error) {
	conn, err := connect(ctx, socketPath)
	if err != nil {
		return 0, rpc.HealthResult{}, err
	}
	defer conn.Close()

	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		var res rpc.HealthResult
		err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &res)
		return res, err
	}
	health, err := fetch(ctx)
	if err != nil {
		return 0, rpc.HealthResult{}, err
	}
	if isHandshakeInFlight(health) {
		if quiet {
			health = waitForHandshakeQuiet(ctx, fetch, health, handshakeWaitBudget, handshakePollInterval)
		} else {
			health = waitForHandshake(ctx, progress, fetch, health, handshakeWaitBudget, handshakePollInterval)
		}
	}
	return dial.LockHolderPID(dial.LockPath(socketPath)), health, nil
}

func waitForHandshakeQuiet(ctx context.Context, fetch healthFetcher, initial rpc.HealthResult, budget, pollInterval time.Duration) rpc.HealthResult {
	res := initial
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return res
		case <-time.After(pollInterval):
		}
		next, err := fetch(ctx)
		if err != nil {
			return res
		}
		res = next
		if !isHandshakeInFlight(res) {
			return res
		}
	}
	return res
}

func renderRestartStarted(w io.Writer, res restartResult) {
	fmt.Fprintf(w, "%s restart: started daemon pid %d", productidentity.Executable, res.NewPID)
	if res.Health.DaemonVersion != "" {
		fmt.Fprintf(w, " (%s)", res.Health.DaemonVersion)
	}
	fmt.Fprintln(w)
	if res.Foreground {
		fmt.Fprintf(w, "%s restart: previous daemon was foreground; replacement is detached\n", productidentity.Executable)
	}
	switch {
	case res.Health.Connected:
		fmt.Fprintf(w, "%s restart: gateway connected at %s:%d (client %d)\n", productidentity.Executable, res.Health.GatewayHost, res.Health.GatewayPort, res.Health.ClientID)
	case res.Health.LastError != "":
		fmt.Fprintf(w, "%s restart: daemon is running; gateway not connected: %s\n", productidentity.Executable, res.Health.LastError)
	default:
		fmt.Fprintf(w, "%s restart: daemon is running; gateway handshake still in progress\n", productidentity.Executable)
	}
}

func waitForAppRespawn(ctx context.Context, find func(context.Context) (appProcess, error), expectedArgs []string, timeout time.Duration) (appProcess, bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := find(ctx)
		if err == nil {
			if slices.Equal(proc.Args, expectedArgs) {
				if !proc.CurrentExecutable {
					return appProcess{}, false, errors.New("app respawned from a different executable; refusing to accept a stale binary")
				}
				return proc, true, nil
			}
			return appProcess{}, false, nil
		}
		// An ambiguous find (other app instances, e.g. the shared host or
		// a preview) means "no unambiguous respawn yet", not a failure —
		// aborting here would leave the just-stopped app stopped.
		if !errors.Is(err, errAppNotRunning) && !errors.Is(err, errAppUnverified) {
			return appProcess{}, false, err
		}
		select {
		case <-ctx.Done():
			return appProcess{}, false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return appProcess{}, false, nil
}

func findAppProcessForExecutables(ctx context.Context, executablePaths map[string]struct{}) (appProcess, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,args=")
	out, err := cmd.Output()
	if err != nil {
		return appProcess{}, err
	}
	self := os.Getpid()
	var exactMatches []appProcess
	var genericMatches []appProcess
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		pidText, cmdline, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil || pid == self || !dial.IsProcessAlive(pid) {
			continue
		}
		args, exact, ok := appCommandMatch(cmdline, executablePaths)
		if !ok {
			continue
		}
		proc := appProcess{PID: pid, Command: strings.TrimSpace(cmdline), Args: args, CurrentExecutable: exact}
		if exact {
			exactMatches = append(exactMatches, proc)
		} else {
			genericMatches = append(genericMatches, proc)
		}
	}
	if err := sc.Err(); err != nil {
		return appProcess{}, err
	}
	matches := genericMatches
	if len(exactMatches) > 0 {
		matches = exactMatches
	}
	switch len(matches) {
	case 0:
		return appProcess{}, errAppNotRunning
	case 1:
		return matches[0], nil
	default:
		return appProcess{}, fmt.Errorf("%w: multiple Canary app processes found", errAppUnverified)
	}
}

func appCommandArgs(cmdline string) ([]string, bool) {
	args, _, ok := appCommandMatch(cmdline, nil)
	return args, ok
}

func appCommandMatch(cmdline string, exactPaths map[string]struct{}) ([]string, bool, bool) {
	fields := strings.Fields(cmdline)
	if len(fields) < 2 ||
		!productidentity.IsManagedProcessExecutableBase(filepath.Base(fields[0])) ||
		fields[1] != "app" {
		return nil, false, false
	}
	args := append([]string(nil), fields[1:]...)
	if !isAppServerArgs(args) {
		return nil, false, false
	}
	exact := false
	for candidate := range executablePathVariants(fields[0]) {
		if _, ok := exactPaths[candidate]; ok {
			exact = true
			break
		}
	}
	return args, exact, true
}

func currentExecutablePaths() map[string]struct{} {
	paths := map[string]struct{}{}
	bin, err := os.Executable()
	if err != nil || bin == "" {
		return paths
	}
	for path := range executablePathVariants(bin) {
		paths[path] = struct{}{}
	}
	return paths
}

func restartExecutablePaths(deps appRestartDeps) map[string]struct{} {
	if len(deps.executablePaths) > 0 {
		return deps.executablePaths
	}
	return currentExecutablePaths()
}

func executablePathMatches(path string, current map[string]struct{}) bool {
	for candidate := range executablePathVariants(path) {
		if _, ok := current[candidate]; ok {
			return true
		}
	}
	return false
}

func executablePathVariants(path string) map[string]struct{} {
	paths := map[string]struct{}{}
	path = strings.TrimSpace(path)
	if path == "" {
		return paths
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	paths[path] = struct{}{}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != "" {
		paths[filepath.Clean(resolved)] = struct{}{}
	}
	return paths
}

func isAppServerArgs(args []string) bool {
	if len(args) == 0 || args[0] != "app" {
		return false
	}
	if len(args) == 1 {
		return true
	}
	switch args[1] {
	case "pair", "help", "--help", "-h", "-help":
		return false
	default:
		return true
	}
}

func stopAppProcess(pid int, timeout time.Duration) error {
	return signalAppProcess(pid, syscall.SIGTERM, timeout, errAppStopTimeout)
}

func killAppProcess(pid int, timeout time.Duration) error {
	return signalAppProcess(pid, syscall.SIGKILL, timeout, errors.New("app kill timed out"))
}

func signalAppProcess(pid int, sig syscall.Signal, timeout time.Duration, timeoutErr error) error {
	if pid <= 0 {
		return errors.New("invalid app PID")
	}
	if timeout <= 0 {
		timeout = restartDefaultTimeout
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find app pid %d: %w", pid, err)
	}
	if err := proc.Signal(sig); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("signal app pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !dial.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%w: app (pid %d) did not exit within %s after %s", timeoutErr, pid, timeout, sig)
}

func startAppProcess(ctx context.Context, args []string) (int, error) {
	bin, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate self: %w", err)
	}
	return startAppProcessFromExecutable(ctx, bin, args)
}

func startAppProcessFromExecutable(ctx context.Context, executable string, args []string) (int, error) {
	if len(args) == 0 {
		args = []string{"app"}
	}
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return 0, errors.New("app executable is empty")
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdin = nil
	logFile, err := openAppRestartLog()
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	} else {
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return 0, fmt.Errorf("open app stdio fallback: %w", err)
		}
		cmd.Stdout = devnull
		cmd.Stderr = devnull
		defer devnull.Close()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return 0, err
	}
	if err := waitForAppProcessReady(ctx, pid, args, 5*time.Second); err != nil {
		return 0, err
	}
	return pid, nil
}

func waitForAppProcessReady(ctx context.Context, pid int, args []string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	addr := appValueArg(args, "addr")
	if strings.TrimSpace(addr) == "" {
		addr = restartDefaultAppAddr
	}
	url := "http://" + appLoopbackAddrForLocalConnect(addr) + "/manifest.webmanifest"
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	var lastErr error
	for time.Now().Before(deadline) {
		if !dial.IsProcessAlive(pid) {
			return fmt.Errorf("app pid %d exited before becoming ready; check %s", pid, appRestartLogPath())
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("GET %s: %s", url, res.Status)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("app pid %d did not become ready at %s within %s: %w; check %s", pid, url, timeout, lastErr, appRestartLogPath())
	}
	return fmt.Errorf("app pid %d did not become ready at %s within %s; check %s", pid, url, timeout, appRestartLogPath())
}

func appValueArg(args []string, name string) string {
	flagName := "--" + name
	for i := range args {
		arg := args[i]
		if arg == flagName {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if value, ok := strings.CutPrefix(arg, flagName+"="); ok {
			return value
		}
	}
	return ""
}

func appLoopbackAddrForLocalConnect(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = restartDefaultAppAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func openAppRestartLog() (*os.File, error) {
	path := appRestartLogPath()
	if path == "" {
		return nil, errors.New("no app log path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func appRestartLogPath() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "ibkr-app.log")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "ibkr", "ibkr-app.log")
}
