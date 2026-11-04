package discover

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/procinfo"
)

// IBKRApp identifies which Interactive Brokers desktop app — if any — is
// running on the host. Used to enrich the "no listener found" error with a
// hint that distinguishes "no app running" (start one) from "app running
// but API socket closed" (check the API settings).
//
// Name is one of "TWS", "IB Gateway", "IBKR Desktop", or "" (unknown / not
// found / lookup failed). PID is 0 when Name is "".
type IBKRApp struct {
	Name string
	PID  int
}

// ProcessLister enumerates running processes as raw lines, one per process,
// each containing at least the PID and the command line. Exposed so tests
// can stub the OS query. Returning nil (no processes) is the correct
// fallback when the underlying lookup fails — callers treat that as "no
// info" rather than "no app running."
var ProcessLister = listProcesses

// DetectIBKRApp returns the first matching IBKR desktop app found in the
// process list. Best-effort: any error or absence of a match yields a zero
// IBKRApp. Callers must treat the zero value as "no information available"
// rather than asserting nothing is running.
//
// Matching is substring + case-insensitive against the full process command
// line so it works whether the OS reports `Trader Workstation.app/.../...`,
// `ibgateway`, or `IBKR Desktop`.
func DetectIBKRApp(ctx context.Context) IBKRApp {
	app, _ := InspectIBKRApp(ctx)
	return app
}

// InspectIBKRApp is DetectIBKRApp with the lookup's own outcome: known is
// false when the process list came back empty — ps failed, timed out, or
// the platform has no lister — so a zero IBKRApp with known true is the
// affirmative "no IBKR app process is running" and a zero IBKRApp with
// known false is "no information". A real process list is never empty: it
// lists at least the lister itself.
func InspectIBKRApp(ctx context.Context) (app IBKRApp, known bool) {
	lines := ProcessLister(ctx)
	if len(lines) == 0 {
		return IBKRApp{}, false
	}
	return matchIBKRApp(lines), true
}

func matchIBKRApp(lines []string) IBKRApp {
	for _, line := range lines {
		l := strings.ToLower(line)
		switch {
		case strings.Contains(l, "trader workstation"):
			return IBKRApp{Name: "TWS", PID: firstPID(line)}
		case strings.Contains(l, "ibgateway"), strings.Contains(l, "ib gateway"):
			return IBKRApp{Name: "IB Gateway", PID: firstPID(line)}
		case strings.Contains(l, "ibkr desktop"):
			return IBKRApp{Name: "IBKR Desktop", PID: firstPID(line)}
		}
	}
	return IBKRApp{}
}

// firstPID extracts the first whitespace-separated integer from a process
// list line. Returns 0 when no leading integer is present (e.g. CSV from
// Windows tasklist, where the PID is in a later column).
func firstPID(line string) int {
	for tok := range strings.FieldsSeq(line) {
		if pid, err := strconv.Atoi(tok); err == nil {
			return pid
		}
	}
	return 0
}

// listProcesses returns "<pid> <full-command-line>" rows from the
// platform-native process lister:
//
//	linux:       procfs, through procinfo (BusyBox ps has no -A)
//	darwin/bsd:  `ps -A -o pid=,args=`
//	windows:     `tasklist /FO CSV /NH` → CSV with image name first
//
// Other platforms: returns nil (best-effort, no error to caller). The
// 2-second guardrail uses ctx so a wedged ps doesn't stall discovery; the
// caller's ctx already governs the larger discovery deadline.
func listProcesses(ctx context.Context) []string {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		procs, err := procinfo.List(ctx)
		if err != nil {
			return nil
		}
		lines := make([]string, 0, len(procs))
		for _, p := range procs {
			lines = append(lines, strconv.Itoa(p.PID)+" "+p.Command)
		}
		return lines
	case "darwin", "freebsd", "openbsd", "netbsd":
		cmd = exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,args=")
	case "windows":
		cmd = exec.CommandContext(ctx, "tasklist", "/FO", "CSV", "/NH")
	default:
		return nil
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// OpenDProcessRunning reports whether OpenD appears in the process list. Absence
// from a failed lookup reads the same way, so a caller words it as "not found".
func OpenDProcessRunning(ctx context.Context) bool {
	for _, line := range ProcessLister(ctx) {
		if strings.Contains(strings.ToLower(line), "opend") {
			return true
		}
	}
	return false
}

// OpenDPortDownHint is the verdict for an OpenD port nothing listens on. It is
// the only thing this package does under the OpenD kind: it reads the process
// list and dials no port.
func OpenDPortDownHint(ctx context.Context, host string, port int) string {
	if OpenDProcessRunning(ctx) {
		return "OpenD is running but its API port " + host + ":" + strconv.Itoa(port) + " is closed: it is still logging in, or api_port in its configuration differs"
	}
	return "no OpenD process found and nothing listens on " + host + ":" + strconv.Itoa(port) + ": start OpenD"
}
