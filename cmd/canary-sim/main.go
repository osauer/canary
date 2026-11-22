// Command canary-sim (build it with -tags trading to place orders) runs the real Canary daemon against the TWS simulator in
// pkg/ibkr/twssim, in live mode against a placeholder account (U1234567), inside one directory of its own: socket,
// config, log, policies, state and home. It prints the environment a caller
// sets to point the real canary CLI (or Desk) at it, then serves until a
// signal. It is not a release binary. It refuses to start when any path it
// would use lies outside its directory, when its socket is the operator's
// daemon socket, or when the broker port is one of IBKR's API ports, so it
// can never reach a live daemon, TWS or IB Gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/daemon"
	"github.com/osauer/canary/v3/internal/discover"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// stack is one simulated desk's directory and the environment that names it.
type stack struct {
	dir, socket, config, log string
	env                      []string // KEY=value, in print order
}

func newStack(dir string) stack {
	s := stack{dir: dir, socket: filepath.Join(dir, "ibkr.sock"), config: filepath.Join(dir, "config.toml"), log: filepath.Join(dir, "ibkr-daemon.log")}
	s.env = []string{
		"CANARY_SOCKET=" + s.socket,
		"CANARY_CONFIG=" + s.config,
		"CANARY_LOG=" + s.log,
		"HOME=" + filepath.Join(dir, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "data"),
		"XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"),
	}
	return s
}

// operatorSockets are the daemon sockets the operator's environment resolves
// to before canary-sim changes anything: CANARY_SOCKET, the XDG runtime
// socket and the home cache socket.
func operatorSockets(getenv func(string) string) []string {
	var out []string
	if v := getenv("CANARY_SOCKET"); v != "" {
		out = append(out, v)
	}
	if v := getenv("XDG_RUNTIME_DIR"); v != "" {
		out = append(out, filepath.Join(v, "ibkr", "ibkr.sock"))
	}
	if home := getenv("HOME"); home != "" {
		out = append(out, filepath.Join(home, ".cache", "ibkr", "ibkr.sock"))
	}
	return out
}

// checkIsolation refuses a stack that could touch the operator's daemon or a
// real broker: every path in its environment must sit inside its directory,
// its socket must not be one the operator's environment names, and the
// broker port must be on loopback and off IBKR's API ports.
func checkIsolation(s stack, operator []string, host string, port int) error {
	var problems []string
	for _, entry := range s.env {
		key, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(value, s.dir+string(filepath.Separator)) {
			problems = append(problems, fmt.Sprintf("%s=%s is outside %s", key, value, s.dir))
		}
	}
	for _, live := range operator {
		if filepath.Clean(live) == filepath.Clean(s.socket) {
			problems = append(problems, fmt.Sprintf("socket %s is the operator's daemon socket", s.socket))
		}
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		problems = append(problems, fmt.Sprintf("broker host %s is not loopback", host))
	}
	if port <= 0 || slices.Contains(discover.StandardPorts, port) || slices.Contains(twssim.LiveAPIPorts, port) {
		problems = append(problems, fmt.Sprintf("broker port %d is not the simulator's own", port))
	}
	if len(problems) > 0 {
		return errors.New("canary-sim refused: " + strings.Join(problems, "; "))
	}
	return nil
}

// buildVersion names the daemon as a canary built from the same tree would:
// the module version when Go stamped one, else "dev".
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// tradingBuild reports whether this binary was built with -tags trading,
// without which the daemon previews but never places an order.
func tradingBuild() bool {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, setting := range bi.Settings {
		if setting.Key == "-tags" && slices.Contains(strings.Split(setting.Value, ","), "trading") {
			return true
		}
	}
	return false
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "ctl" {
		return runCtl(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("canary-sim", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "canary-sim [-dir DIR] [-listen 127.0.0.1:0]: run the real daemon (live mode, placeholder account U1234567) against the TWS simulator in DIR")
		flags.PrintDefaults()
		fmt.Fprintln(stderr, "\nThe exchange sessions start as the official calendar has them now; the control channel changes them.")
		fmt.Fprintln(stderr, ctlUsage)
	}
	dirFlag := flags.String("dir", "", "the stack's directory (created; default a new one under /tmp); keep it short, a Unix socket path is limited to about 100 bytes")
	listen := flags.String("listen", "127.0.0.1:0", "the simulator's loopback address; port 0 picks a free one")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	operator := operatorSockets(os.Getenv)
	dir := *dirFlag
	var err error
	if dir == "" {
		dir, err = os.MkdirTemp("/tmp", "canary-sim-")
	} else {
		dir, err = filepath.Abs(dir)
		if err == nil {
			err = os.MkdirAll(dir, 0o700)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	s := newStack(dir)

	frames, err := os.OpenFile(filepath.Join(dir, "twssim-frames.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	defer frames.Close()
	sim, err := twssim.Listen(twssim.Options{Listen: *listen, Recorder: &twssim.Recorder{Sink: frames}})
	if err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 2
	}
	defer sim.Close()
	if err := checkIsolation(s, operator, "127.0.0.1", sim.Port()); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	// The simulated exchange opens and closes as the official calendar has it
	// now, until the control channel says otherwise.
	for name, cal := range ctlMarkets {
		session, err := marketcal.New().SessionAt(cal, time.Now())
		sim.SetSession(name, err == nil && session.IsOpen)
	}
	ctl, err := net.Listen("unix", filepath.Join(dir, "ctl.sock"))
	if err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	defer ctl.Close()
	go serveControl(ctl, sim)
	for _, entry := range s.env {
		key, value, _ := strings.Cut(entry, "=")
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(stderr, "canary-sim:", err)
			return 1
		}
		if strings.HasPrefix(key, "XDG_") || key == "HOME" {
			if err := os.MkdirAll(value, 0o700); err != nil {
				fmt.Fprintln(stderr, "canary-sim:", err)
				return 1
			}
		}
	}
	book := sim.Book()
	cfgText := fmt.Sprintf("# canary-sim: the simulated broker. A CLI that autospawns a daemon from this\n# file reaches only this port.\n[gateway]\nhost = \"127.0.0.1\"\nport = %d\nclient_id = 15\naccount = %q\ntls = false\n\n[trading]\nmode = \"live\"\n\n[cash_ledger]\nurl = \"http://127.0.0.1:1/v1/api\"\n", sim.Port(), book.Account())
	if err := os.WriteFile(s.config, []byte(cfgText), 0o600); err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	if err := daemon.WriteSimulatorPolicies(config.PolicyDir(), book.Base()); err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	cfg, issues, err := config.LoadForDaemon(s.config)
	if err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	resolved.Daemon.SetIdleTimeout(0)
	logFile, err := os.OpenFile(s.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	defer logFile.Close()

	fmt.Fprintf(stdout, "canary-sim: TWS simulator on %s, account %s (live mode, placeholder account), daemon socket %s\n", sim.Addr(), book.Account(), s.socket)
	if !tradingBuild() {
		fmt.Fprintln(stdout, "canary-sim: this build previews but cannot place orders; build it with: go build -tags trading ./cmd/canary-sim")
	}
	fmt.Fprintf(stdout, "canary-sim: broker frames in %s, daemon log in %s\n", frames.Name(), s.log)
	fmt.Fprintf(stdout, "canary-sim: control: canary-sim ctl -dir %s COMMAND (canary-sim -h lists them)\n", dir)
	fmt.Fprintln(stdout, "# set these before running canary against the simulator:")
	for _, entry := range s.env {
		key, value, _ := strings.Cut(entry, "=")
		fmt.Fprintf(stdout, "export %s=%q\n", key, value)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := daemon.New(daemon.Options{
		Config: resolved, SocketPath: s.socket, Version: buildVersion(), Logger: daemon.NewLogger(logFile, resolved.Daemon.LogLevel),
		DisableMacroSources: true, ConfigIssues: issues, Simulator: true,
	})
	defer srv.Stop()
	if err := srv.Start(ctx); err != nil {
		fmt.Fprintln(stderr, "canary-sim:", err)
		return 1
	}
	return 0
}
