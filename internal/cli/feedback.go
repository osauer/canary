package cli

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/osauer/canary/v2/internal/feedback"
	"github.com/osauer/canary/v2/internal/productidentity"
)

// runFeedback composes the redacted diagnostic report and prints it with
// the GitHub new-issue URL that opens the bug report form prefilled. It
// writes nothing and sends nothing: the human opens the URL and files the
// issue. env.Conn is nil when no daemon is reachable; cmd/canary dials
// without starting one, so asking for the report never starts a daemon.
func runFeedback(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "feedback")
	surface := fs.String("surface", feedback.SurfaceCLI, "where it happened: "+strings.Join(feedback.Surfaces(), ", "))
	errText := fs.String("error", "", "the error text to include, redacted before use")
	jsonOut := fs.Bool("json", false, "emit the report and URL as JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "feedback: unexpected argument %q", fs.Arg(0))
	}
	var conn feedback.Conn
	if env.Conn != nil {
		conn = env.Conn
	}
	report, err := feedback.Collect(ctx, conn, feedback.Host{Version: env.Version, Build: buildKind(), OS: runtime.GOOS, Arch: runtime.GOARCH},
		feedback.Input{Surface: *surface, Error: *errText})
	if err != nil {
		return fail(env, "feedback: %v", err)
	}
	if *jsonOut {
		return printJSON(env, feedback.Compose(report))
	}
	renderFeedback(env, report)
	return 0
}

// buildKind names the binary as the update channel does: the trading build
// is the one that can send orders.
func buildKind() string {
	if updateTradingBuild {
		return "trading"
	}
	return "standard"
}

// renderFeedback prints the report for pasting, then the URL on its own
// line so a terminal makes it clickable.
func renderFeedback(env *Env, r feedback.Report) {
	out := env.Stdout
	for i, line := range strings.Split(strings.TrimRight(r.Text(), "\n"), "\n") {
		if i == 0 {
			fmt.Fprintln(out, env.bold(line))
			continue
		}
		label, value, _ := strings.Cut(line, " ")
		fmt.Fprintf(out, "%s %s\n", env.dim(fmt.Sprintf("%-10s", label)), strings.TrimLeft(value, " "))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Open this link to file the report (it opens GitHub's bug form, prefilled; nothing is sent until you submit):")
	fmt.Fprintln(out, env.cyan(r.IssueURL()))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s\n", env.dim("Read the report before filing. Private or broker-sensitive context goes by email instead: https://osauer.dev/"+productidentity.Executable+"/feedback/#private"))
}
