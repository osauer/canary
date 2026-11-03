package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/feedback"
	"github.com/osauer/canary/v2/internal/rpc"
)

// goldenFeedbackReport is the fixed report both golden cases render: the
// host fields are inputs, so the file is the same on every machine.
func goldenFeedbackReport(reachable bool) feedback.Report {
	r := feedback.Report{Surface: feedback.SurfaceCLI, Version: "v3.33.0", Build: "standard", OS: "darwin", Arch: "arm64", Holdings: "not masked: no daemon reachable",
		Error: "canary: status: order [ref] for [holding] refused by [account]"}
	if reachable {
		r.Daemon = feedback.Daemon{Reachable: true, Version: "v3.33.0", Connected: true, Gateway: "IB Gateway", Phase: rpc.GatewayPhaseReady, APIServer: 203, AccountMode: rpc.AccountModePaper, DataType: "live", Verdict: "READY"}
		r.Data = feedback.Data{Available: true, State: "healthy", Label: "Healthy", Required: 20, Current: 20}
		r.Holdings = "masked"
	}
	return r
}

// Run through the registry with a synthetic daemon: the command reads
// status, positions and data health, prints the report and the URL, and
// exits 0; --json carries the same URL.
func TestFeedbackRunsAgainstTheDaemonAndWithoutOne(t *testing.T) {
	t.Setenv("USER", "synthuser")
	conn := goldenConn{
		rpc.MethodStatusHealth:  rpc.HealthResult{Verdict: rpc.HealthVerdict{State: "READY"}, DaemonVersion: "v3.33.0", Connected: true, GatewayPort: 7496, ServerVersion: 203, AccountMode: rpc.AccountModeLive, Account: "U1234567", GatewayHost: "gw.synth.lan"},
		rpc.MethodPositionsList: rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "SYNTHQ"}}},
		rpc.MethodDataHealth:    rpc.DataHealthResult{Summary: rpc.DataHealthSummary{State: "healthy", Label: "Healthy", Required: 3, Current: 3}},
	}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn, Version: "v3.33.0"}
	if code := Run(context.Background(), env, "feedback", []string{"--surface", "mcp", "--error", "quote for SYNTHQ in U1234567 stale on gw.synth.lan"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, leaked := range []string{"SYNTHQ", "U1234567", "gw.synth.lan", "synthuser"} {
		if strings.Contains(out, leaked) {
			t.Errorf("stdout leaks %q:\n%s", leaked, out)
		}
	}
	for _, want := range []string{"Surface    MCP server", "TWS connected, API server 203", "Holdings   masked", feedback.NewIssueURL + "?"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}

	stdout.Reset()
	env = &Env{Stdout: &stdout, Stderr: &stderr, Version: "v3.33.0"}
	if code := Run(context.Background(), env, "feedback", []string{"--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var res feedback.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(strings.SplitN(res.URL, "?", 2)[1])
	if err != nil || q.Get("template") != "bug_report.yml" || q.Get("surface") != "CLI" || q.Get("version") != "v3.33.0 ("+buildKind()+" build)" {
		t.Fatalf("url fields = %v (%v)", q, err)
	}
	if res.Fields.Daemon.Reachable || !strings.Contains(res.Report, "Daemon     not reachable") || !res.Redacted {
		t.Fatalf("no-daemon result = %+v", res)
	}
}

func TestFeedbackRefusesAnUnknownSurface(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := Run(context.Background(), env, "feedback", []string{"--surface", "web"}); code != 1 || !strings.Contains(stderr.String(), "surface must be one of cli, mcp, app, install") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}
