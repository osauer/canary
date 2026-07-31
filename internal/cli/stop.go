package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/update"
)

type stopOptions struct {
	jsonOut bool
	force   bool
	yes     bool
	app     bool
	daemon  bool
	timeout time.Duration
	// isTTY reports whether the confirmation can be asked. Tests inject it
	// rather than touching os.Stdin.
	isTTY bool
	in    io.Reader
	out   io.Writer
	err   io.Writer
}

func (o *stopOptions) signalPolicy() signalPolicy {
	return signalPolicy{force: o.force, quiet: o.jsonOut, timeout: o.timeout, out: o.out, err: o.err}
}

// interactive reports whether the working-daemon confirmation can be asked
// here. --json is machine output: a prompt written to the same stream would
// corrupt it, so those callers pass --yes or get the refusal.
func (o *stopOptions) interactive() bool { return o.isTTY && !o.jsonOut }

type stopDeps struct {
	daemon restartDeps
	app    appRestartDeps
	// health reads daemon status without autospawning one. running=false
	// means no daemon is listening, which is not an error.
	health func(ctx context.Context, socketPath string) (health rpc.HealthResult, running bool, err error)
	mcp    func(context.Context) []mcpProcess
}

type stopResult struct {
	Action    string            `json:"action"`
	Daemon    *daemonStopResult `json:"daemon,omitempty"`
	App       *appStopResult    `json:"app,omitempty"`
	Blockers  []stopBlocker     `json:"blockers,omitempty"`
	MCP       []mcpProcess      `json:"mcp,omitempty"`
	ElapsedMS int64             `json:"elapsed_ms"`
}

type daemonStopResult struct {
	Target     string `json:"target"`
	Action     string `json:"action"`
	WasRunning bool   `json:"was_running"`
	Forced     bool   `json:"forced"`
	Graceful   bool   `json:"graceful"`
	PID        int    `json:"pid,omitempty"`
	Command    string `json:"command,omitempty"`
	SocketPath string `json:"socket_path"`
}

type appStopResult struct {
	Target     string `json:"target"`
	Action     string `json:"action"`
	Reason     string `json:"reason,omitempty"`
	Supervisor string `json:"supervisor,omitempty"`
	Unloaded   bool   `json:"unloaded,omitempty"`
	WasRunning bool   `json:"was_running"`
	Forced     bool   `json:"forced"`
	Graceful   bool   `json:"graceful"`
	PID        int    `json:"pid,omitempty"`
	Command    string `json:"command,omitempty"`
}

// stopBlocker is one reason the daemon would rather keep running. It mirrors
// the daemon's own idle-exit gate: whatever defers idle shutdown there is
// what this command asks about here.
type stopBlocker struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// mcpProcess is a reported-only stdio MCP server. The MCP host owns that
// process lifetime — it already exits with its parent — so `stop` names it
// and leaves it alone rather than breaking a live AI session's tools.
type mcpProcess struct {
	PID    int    `json:"pid"`
	Host   string `json:"host,omitempty"`
	Action string `json:"action"`
}

// RunStop is the top-level `canary stop` entrypoint. Like RunRestart it takes
// no Env: stopping is local process management and must run before the
// autospawn path in cmd/canary/main.go, which would otherwise start the very
// daemon this command exists to stop.
func RunStop(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts := stopOptions{
		timeout: restartDefaultTimeout,
		isTTY:   isStdinTTY(stdin),
		in:      stdin,
		out:     stdout,
		err:     stderr,
	}
	env := &Env{Stdout: stdout, Stderr: stderr}
	fs := flagSet(env, "stop")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit machine-readable stop result")
	fs.BoolVar(&opts.force, "force", false, "send SIGKILL if graceful SIGTERM does not stop the target process before --timeout")
	fs.BoolVar(&opts.yes, "yes", false, "stop without confirmation even while the daemon still has work in flight")
	fs.BoolVar(&opts.app, "app", false, "stop only the app process")
	fs.BoolVar(&opts.daemon, "daemon", false, "stop only the daemon process")
	fs.DurationVar(&opts.timeout, "timeout", restartDefaultTimeout, "how long to wait for graceful process stop before failing or forcing")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	if opts.timeout <= 0 {
		fmt.Fprintf(stderr, "%s stop: --timeout must be positive\n", productidentity.Executable)
		return 2
	}
	if !opts.app && !opts.daemon {
		opts.app, opts.daemon = true, true
	}
	return runStopCore(ctx, &opts, productionStopDeps())
}

func productionStopDeps() stopDeps {
	return stopDeps{
		daemon: productionDaemonRestartDeps(),
		app:    productionAppRestartDeps(),
		health: readDaemonHealth,
		mcp:    findMCPProcesses,
	}
}

func runStopCore(ctx context.Context, opts *stopOptions, deps stopDeps) int {
	startedAt := time.Now()
	prefix := productidentity.Executable + " stop"
	res := stopResult{Action: "not_running"}
	if deps.mcp != nil {
		res.MCP = deps.mcp(ctx)
	}

	// Every early return still reports what it did and did not touch, which
	// matters most when a stage failed halfway.
	abort := func(action string, exit int) int {
		res.Action = action
		res.ElapsedMS = time.Since(startedAt).Milliseconds()
		if opts.jsonOut {
			if jsonExit := printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res); jsonExit != 0 {
				return jsonExit
			}
		}
		return exit
	}

	stopApp := opts.app
	if stopApp && dial.SocketPathOverridden() {
		// App discovery is by process name and cannot tell which daemon
		// scope a found app belongs to, so an overridden socket means the
		// only app this command could find is outside the scope it was
		// asked about. Same hands-off rule as `restart`.
		res.App = &appStopResult{Target: "app", Action: "skipped", Reason: "socket_overridden"}
		stopApp = false
		fmt.Fprintf(opts.err, "%s: CANARY_SOCKET is set; the app is not part of that daemon scope and was left untouched\n", prefix)
	}

	if opts.daemon {
		blockers, exit := stopPreflight(ctx, opts, deps, prefix)
		res.Blockers = blockers
		if exit != 0 {
			return abort("refused", exit)
		}
	}

	if stopApp {
		appRes, exit := stopAppStage(ctx, opts, deps.app, prefix)
		res.App = &appRes
		if exit != 0 {
			return abort("failed", exit)
		}
	}

	if opts.daemon {
		daemonRes, exit := stopDaemonStage(ctx, opts, deps.daemon, prefix)
		res.Daemon = &daemonRes
		if exit != 0 {
			return abort("failed", exit)
		}
	}

	res.Action = overallStopAction(res)
	res.ElapsedMS = time.Since(startedAt).Milliseconds()
	if opts.jsonOut {
		return printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res)
	}
	renderStopFooter(opts, prefix, res)
	return 0
}

// stopPreflight asks the running daemon whether it still has work that its
// own idle watcher would refuse to exit on — working broker orders above all,
// since a stopped daemon goes dark on fills, cancels, and the order journal.
// A daemon that cannot be read is treated as unknown, not clean.
func stopPreflight(ctx context.Context, opts *stopOptions, deps stopDeps, prefix string) ([]stopBlocker, int) {
	health, running, err := deps.health(ctx, dial.DefaultSocketPath())
	var blockers []stopBlocker
	switch {
	case err != nil:
		blockers = []stopBlocker{{Name: "daemon-health", Status: "unreadable: " + err.Error()}}
	case !running:
		return nil, 0
	default:
		for _, task := range health.BackgroundTasks {
			blockers = append(blockers, stopBlocker{Name: task.Name, Status: task.Status})
		}
	}
	if len(blockers) == 0 || opts.yes {
		return blockers, 0
	}

	fmt.Fprintf(opts.err, "%s: the daemon still has work in flight:\n", prefix)
	for _, blocker := range blockers {
		fmt.Fprintf(opts.err, "  %-14s %s\n", blocker.Name, blocker.Status)
	}
	fmt.Fprintf(opts.err, "%s: stopping ends order tracking, protection proposals, and phone alerts until it runs again\n", prefix)
	if !opts.interactive() {
		fmt.Fprintf(opts.err, "%s: refusing to stop without confirmation; pass --yes to stop anyway\n", prefix)
		return blockers, 1
	}
	if !promptStop(opts.in, opts.out) {
		fmt.Fprintf(opts.err, "%s: left everything running\n", prefix)
		return blockers, 1
	}
	return blockers, 0
}

// promptStop reads stdin for a [y/N] response. Unlike the update prompt this
// defaults to no: enter or EOF keeps the desk running.
func promptStop(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Stop anyway? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func stopAppStage(ctx context.Context, opts *stopOptions, deps appRestartDeps, prefix string) (appStopResult, int) {
	res := appStopResult{Target: "app", Action: "not_running"}
	proc, findErr := deps.find(ctx)
	if deps.supervisor != nil {
		if sup, ok := deps.supervisor(ctx); ok {
			if supervisedRestartApplies(proc, findErr, sup) {
				return stopSupervisedApp(ctx, opts, deps, prefix, proc, findErr, sup)
			}
			if findErr == nil {
				// One app runs with its own --state-dir while the shared
				// launchd job owns another. Stopping "the app" would have to
				// pick one, and the isolated instance is usually a smoke or
				// preview host the caller did not mean to lose.
				fmt.Fprintf(opts.err, "%s: an independent app process and the launchd supervisor %s are both present; refusing to guess which one to stop (stop the isolated app yourself, or run `%s stop --daemon`)\n", prefix, sup.Target, productidentity.Executable)
				return res, 1
			}
		}
	}
	switch {
	case findErr == nil:
		res.Action = "stopped"
		res.WasRunning = true
		res.PID = proc.PID
		res.Command = proc.Command
		forced, exit := stopAppWithPolicy(opts.signalPolicy(), deps, prefix, proc.PID)
		if exit != 0 {
			return res, exit
		}
		res.Forced = forced
		res.Graceful = !forced
		return res, 0
	case errors.Is(findErr, errAppNotRunning):
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s: no app was running\n", prefix)
		}
		return res, 0
	default:
		fmt.Fprintf(opts.err, "%s: %v\n", prefix, findErr)
		return res, 1
	}
}

// stopSupervisedApp unloads the launchd job before signalling anything.
// KeepAlive respawns a SIGTERMed app within seconds, so a stop that only
// signalled would report success and leave the app running.
func stopSupervisedApp(ctx context.Context, opts *stopOptions, deps appRestartDeps, prefix string, proc appProcess, findErr error, sup appSupervisor) (appStopResult, int) {
	res := appStopResult{Target: "app", Action: "stopped", Supervisor: sup.Target}
	if sup.ParseError != "" || !isAppServerArgs(sup.Args) {
		fmt.Fprintf(opts.err, "%s: launchd job %s is untrusted or unparseable (%s); refusing to unload or stop it\n", prefix, sup.Target, supervisorParseFailure(sup))
		return res, 1
	}
	if deps.unload == nil {
		fmt.Fprintf(opts.err, "%s: launchd unload adapter is unavailable\n", prefix)
		return res, 1
	}
	if findErr == nil && proc.PID > 0 {
		res.WasRunning = true
		res.PID = proc.PID
		res.Command = proc.Command
	}
	if err := deps.unload(ctx, sup); err != nil {
		fmt.Fprintf(opts.err, "%s: unload launchd app supervisor: %v\n", prefix, err)
		return res, 1
	}
	res.Unloaded = true
	if !opts.jsonOut {
		fmt.Fprintf(opts.out, "%s: unloaded launchd job %s\n", prefix, sup.Target)
	}
	// bootout terminates the job's own process, but an orphan from an earlier
	// hand-started app can outlive it and keep holding the state lock. Re-read
	// what is left instead of signalling the pid just booted out: that pid is
	// expected to be gone, and the number may belong to something else by now.
	remaining, remainingErr := deps.find(ctx)
	switch {
	case remainingErr == nil:
		forced, exit := stopAppWithPolicy(opts.signalPolicy(), deps, prefix, remaining.PID)
		if exit != 0 {
			return res, exit
		}
		res.WasRunning = true
		res.Forced = forced
		res.Graceful = !forced
		if res.PID == 0 {
			res.PID = remaining.PID
			res.Command = remaining.Command
		}
	case errors.Is(remainingErr, errAppNotRunning):
		if !res.WasRunning {
			res.Action = "unloaded"
		}
	default:
		fmt.Fprintf(opts.err, "%s: %v\n", prefix, remainingErr)
		return res, 1
	}
	return res, 0
}

func stopDaemonStage(ctx context.Context, opts *stopOptions, deps restartDeps, prefix string) (daemonStopResult, int) {
	socketPath := dial.DefaultSocketPath()
	res := daemonStopResult{Target: "daemon", Action: "not_running", SocketPath: socketPath}
	proc, err := deps.find(ctx, socketPath)
	switch {
	case err == nil:
		res.Action = "stopped"
		res.WasRunning = true
		res.PID = proc.PID
		res.Command = proc.Command
		res.SocketPath = proc.SocketPath
		forced, exit := stopDaemonWithPolicy(opts.signalPolicy(), deps, prefix, proc.PID)
		if exit != 0 {
			return res, exit
		}
		res.Forced = forced
		res.Graceful = !forced
		return res, 0
	case errors.Is(err, update.ErrDaemonNotRunning):
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "%s: no daemon was running\n", prefix)
		}
		return res, 0
	default:
		fmt.Fprintf(opts.err, "%s: %v\n", prefix, err)
		return res, 1
	}
}

func overallStopAction(res stopResult) string {
	if (res.App != nil && (res.App.Action == "stopped" || res.App.Action == "unloaded")) ||
		(res.Daemon != nil && res.Daemon.Action == "stopped") {
		return "stopped"
	}
	if res.App != nil && res.App.Action == "skipped" && res.Daemon == nil {
		return "skipped"
	}
	return "not_running"
}

// renderStopFooter says how each stopped process comes back, and names the
// MCP servers this command deliberately did not touch.
func renderStopFooter(opts *stopOptions, prefix string, res stopResult) {
	if res.App != nil && res.App.Unloaded {
		fmt.Fprintf(opts.out, "%s: the app stays stopped until `%s restart --app` or your next login\n", prefix, productidentity.Executable)
	}
	switch {
	case res.Daemon != nil && res.Daemon.Action == "stopped":
		fmt.Fprintf(opts.out, "%s: any %s command starts the daemon again\n", prefix, productidentity.Executable)
	case res.Daemon == nil && res.App != nil && res.App.WasRunning:
		fmt.Fprintf(opts.out, "%s: the daemon is still running; `%s stop --daemon` stops it too\n", prefix, productidentity.Executable)
	}
	if len(res.MCP) > 0 {
		fmt.Fprintf(opts.out, "%s: %s left running; each one stops when its AI client quits\n", prefix, describeMCPProcesses(res.MCP))
	}
}

func describeMCPProcesses(procs []mcpProcess) string {
	noun := "MCP servers"
	if len(procs) == 1 {
		noun = "MCP server"
	}
	var hosts []string
	for _, proc := range procs {
		if proc.Host != "" && !slices.Contains(hosts, proc.Host) {
			hosts = append(hosts, proc.Host)
		}
	}
	if len(hosts) == 0 {
		return fmt.Sprintf("%d %s", len(procs), noun)
	}
	return fmt.Sprintf("%d %s (%s)", len(procs), noun, strings.Join(hosts, ", "))
}

// stopHealthReadTimeout bounds the pre-flight read. A daemon wedged badly
// enough to never answer is exactly what `stop` is for, so the question must
// not outlast a few seconds.
const stopHealthReadTimeout = 5 * time.Second

func readDaemonHealth(ctx context.Context, socketPath string) (rpc.HealthResult, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, stopHealthReadTimeout)
	defer cancel()
	conn, err := dial.Connect(socketPath)
	if err != nil {
		if errors.Is(err, dial.ErrSocketMissing) {
			return rpc.HealthResult{}, false, nil
		}
		return rpc.HealthResult{}, true, err
	}
	defer conn.Close()
	var health rpc.HealthResult
	if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &health); err != nil {
		return rpc.HealthResult{}, true, err
	}
	return health, true, nil
}

// findMCPProcesses lists local stdio MCP servers with the command name of
// the AI host that owns each one. Reporting is a courtesy, so a ps failure
// yields no rows rather than an error: it must never block a stop.
func findMCPProcesses(ctx context.Context) []mcpProcess {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,args=").Output()
	if err != nil {
		return nil
	}
	parents := map[int]int{}
	var pids []int
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		pid, ppid, args, ok := parseMCPPSLine(sc.Text())
		if !ok || len(args) < 2 {
			continue
		}
		if productidentity.IsManagedProcessExecutableBase(filepath.Base(args[0])) && args[1] == "mcp" {
			parents[pid] = ppid
			pids = append(pids, pid)
		}
	}
	if err := sc.Err(); err != nil || len(pids) == 0 {
		return nil
	}
	slices.Sort(pids)
	hosts := processCommandNames(ctx, parents)
	procs := make([]mcpProcess, 0, len(pids))
	for _, pid := range pids {
		procs = append(procs, mcpProcess{PID: pid, Host: hosts[parents[pid]], Action: "left_running"})
	}
	return procs
}

func parseMCPPSLine(line string) (pid, ppid int, args []string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 0, 0, nil, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, nil, false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, nil, false
	}
	return pid, ppid, fields[2:], true
}

// processCommandNames maps each parent pid to its executable name. It reads
// `comm` rather than reusing the argv scan above because AI hosts live under
// paths with spaces ("…/Library/Application Support/Claude/…"), which argv
// splitting turns into "Application".
func processCommandNames(ctx context.Context, parents map[int]int) map[int]string {
	pids := make([]string, 0, len(parents))
	for _, ppid := range parents {
		if ppid > 1 && !slices.Contains(pids, strconv.Itoa(ppid)) {
			pids = append(pids, strconv.Itoa(ppid))
		}
	}
	if len(pids) == 0 {
		return nil
	}
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=,comm=", "-p", strings.Join(pids, ",")).Output()
	if err != nil {
		return nil
	}
	names := map[int]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		pid, name, ok := parseCommandNameLine(sc.Text())
		if ok {
			names[pid] = name
		}
	}
	return names
}

func parseCommandNameLine(line string) (int, string, bool) {
	pidText, command, ok := strings.Cut(strings.TrimSpace(line), " ")
	if !ok {
		return 0, "", false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return 0, "", false
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return 0, "", false
	}
	return pid, filepath.Base(command), true
}
