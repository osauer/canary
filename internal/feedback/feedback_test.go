package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// fixtureConn answers each daemon method with one synthetic result,
// round-tripped through JSON as the socket delivers it.
type fixtureConn map[string]any

func (c fixtureConn) Call(_ context.Context, method string, _ any, out any) error {
	result, ok := c[method]
	if !ok {
		return fmt.Errorf("fixture: no result for %s", method)
	}
	if err, isErr := result.(error); isErr {
		return err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// Planted sensitive strings: every one must be absent from the stdout
// report and from the URL, whichever path it takes in (the daemon's own
// fields, the error text, or both).
var planted = []string{
	"U1234567",         // an account id shape
	"1,234,567.89",     // a balance
	"48213",            // an order reference
	"synth-mbp.local",  // a host name
	"/Users/synthuser", // a home path
	"synthuser",        // a user name
	"SYNTHQ",           // a holding
	"gateway.synth.lan",
	"192.168.7.7",
}

func fixtureHealth() rpc.HealthResult {
	return rpc.HealthResult{
		Verdict: rpc.HealthVerdict{State: "ATTENTION", Reason: "backend link down"}, DaemonVersion: "v3.33.0",
		Account: "U1234567", ConnectedAccount: "U1234567", AccountMode: rpc.AccountModeLive,
		GatewayHost: "gateway.synth.lan", GatewayPort: 4001, ClientID: 15, Connected: true,
		GatewayPhase: rpc.GatewayPhaseBackendLinkDown, DataType: "delayed", ServerVersion: 203,
		LastError: "dial gateway.synth.lan:4001 from synth-mbp.local as synthuser: order 48213 for SYNTHQ via reqMktData balance 1,234,567.89 EUR at 192.168.7.7:4001",
	}
}

func fixturePositions() rpc.PositionsResult {
	return rpc.PositionsResult{AccountID: "U1234567", Stocks: []rpc.PositionView{{Symbol: "SYNTHQ", LocalSymbol: "SYNTHQ"}},
		Options: []rpc.PositionView{{Symbol: "SYNTHQ", LocalSymbol: "SYNTHQ 260918C00005000"}}, ByUnderlying: []rpc.PositionGroup{{Underlying: "SYNTHQ"}}}
}

func fixtureData() rpc.DataHealthResult {
	return rpc.DataHealthResult{AsOf: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		Summary:  rpc.DataHealthSummary{State: "attention", Label: "Needs attention", Total: 24, Required: 20, Current: 17, Limited: 2, Unavailable: 1, Unverified: 0, Problems: 2, ExpectedDelays: 1},
		Concerns: []rpc.DataHealthConcern{{SourceID: "broker_history", State: "limited", Label: "IBKR price history for SYNTHQ"}, {SourceID: "nasdaq", State: "unavailable", Label: "Nasdaq earnings"}}}
}

func TestReportAndURLCarryNoPlantedSecret(t *testing.T) {
	t.Setenv("USER", "synthuser")
	conn := fixtureConn{rpc.MethodStatusHealth: fixtureHealth(), rpc.MethodPositionsList: fixturePositions(), rpc.MethodDataHealth: fixtureData()}
	in := Input{Surface: SurfaceMCP, Error: "canary: status: account U1234567 on synth-mbp.local (synthuser) holds SYNTHQ; config /Users/synthuser/.config/ibkr/config.toml; order 48213 rejected; NLV 1,234,567.89 EUR; gateway.synth.lan"}
	r, err := Collect(context.Background(), conn, Host{Version: "v3.33.0", Build: "trading", OS: "darwin", Arch: "arm64"}, in)
	if err != nil {
		t.Fatal(err)
	}
	text, link := r.Text(), r.IssueURL()
	decoded, err := url.QueryUnescape(link)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	for _, secret := range planted {
		for name, body := range map[string]string{"report": text, "url": link, "decoded url": decoded, "json": string(raw)} {
			if strings.Contains(body, secret) {
				t.Errorf("%s carries planted %q:\n%s", name, secret, body)
			}
		}
	}
	for _, want := range []string{"[account]", "[holding]", "order [ref]", "[amount]", "[host]", "~/.config/ibkr/config.toml", "[user]", "IB Gateway connected, API server 203", "live account", "market data delayed", "Needs attention; 20 required sources: 17 current, 2 limited, 1 unavailable, 0 unverified (1 awaiting the publisher)", "Holdings   masked"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	if r.Daemon.LastError == "" || strings.Contains(r.Daemon.LastError, "synth") {
		t.Errorf("last error = %q", r.Daemon.LastError)
	}
}

func TestIssueURLFieldsAndBound(t *testing.T) {
	t.Setenv("USER", "synthuser")
	conn := fixtureConn{rpc.MethodStatusHealth: fixtureHealth(), rpc.MethodPositionsList: fixturePositions(), rpc.MethodDataHealth: fixtureData()}
	long := strings.Repeat("the quote for the index future went stale after the overnight reset and never came back. ", 40)
	r, err := Collect(context.Background(), conn, Host{Version: "v3.33.0", Build: "standard", OS: "linux", Arch: "amd64"}, Input{Surface: SurfaceCLI, Error: long})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(r.IssueURL())
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != NewIssueURL {
		t.Fatalf("base = %s, want %s", got, NewIssueURL)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"template":    "bug_report.yml",
		"surface":     "CLI",
		"version":     "v3.33.0 (standard build)",
		"environment": "Linux amd64, IB Gateway",
		"diagnostics": r.Text(),
		"summary":     r.Error,
		"title":       r.Title(),
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for key := range q {
		switch key {
		case "template", "surface", "version", "environment", "diagnostics", "summary", "title":
		default:
			t.Errorf("unexpected query field %q", key)
		}
	}
	if len(r.Text()) > MaxTextBytes {
		t.Errorf("report is %d bytes, bound %d", len(r.Text()), MaxTextBytes)
	}
	if len(r.Error) > maxErrorBytes || !strings.HasSuffix(r.Error, "…[cut]") {
		t.Errorf("error = %d bytes, want cut at %d with the mark", len(r.Error), maxErrorBytes)
	}
	if !strings.HasPrefix(q.Get("title"), "[Bug]: ") || len(q.Get("title")) > 90 {
		t.Errorf("title = %q", q.Get("title"))
	}
	if n := len(r.IssueURL()); n > 8000 {
		t.Errorf("url is %d bytes; GitHub refuses long URLs", n)
	}
}

func TestNoDaemonStillReports(t *testing.T) {
	r, err := Collect(context.Background(), nil, Host{Version: "dev", OS: "darwin", Arch: "arm64"}, Input{Surface: SurfaceInstall, Error: "install.sh: checksum mismatch for canary-darwin-arm64"})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Text()
	for _, want := range []string{"Daemon     not reachable", "Data       not read", "Holdings   not masked: no daemon reachable", "dev, standard build, darwin/arm64", "Surface    Install or update", "checksum mismatch"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	q, _ := url.ParseQuery(strings.SplitN(r.IssueURL(), "?", 2)[1])
	if q.Get("surface") != "Install or update" || q.Get("environment") != "macOS arm64" {
		t.Errorf("fields = %v", q)
	}
	if r.Daemon.Reachable || r.Data.Available {
		t.Errorf("report claims a daemon: %+v", r)
	}
}

func TestDaemonReadFailuresBecomeNotes(t *testing.T) {
	conn := fixtureConn{rpc.MethodStatusHealth: errors.New("gateway_unavailable: dial 192.168.7.7:4001 refused"), rpc.MethodPositionsList: errors.New("timeout: positions for U1234567"), rpc.MethodDataHealth: errors.New("unknown_method: data.health")}
	r, err := Collect(context.Background(), conn, Host{}, Input{})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Text()
	if strings.Contains(text, "192.168.7.7") || strings.Contains(text, "U1234567") {
		t.Fatalf("notes leak: %s", text)
	}
	for _, want := range []string{"Note       status not read: gateway_unavailable: dial [addr] refused", "Note       data health not read: unknown_method: data.health", "Holdings   not masked: positions not read (timeout: positions for [account])", "Surface    CLI", "dev, standard build"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
}

func TestSurfaceIsValidated(t *testing.T) {
	if _, err := Collect(context.Background(), nil, Host{}, Input{Surface: "web"}); !errors.Is(err, ErrSurface) {
		t.Fatalf("err = %v, want ErrSurface", err)
	}
	r, err := Collect(context.Background(), nil, Host{}, Input{Surface: " App "})
	if err != nil || r.Surface != SurfaceApp {
		t.Fatalf("surface = %q, err %v", r.Surface, err)
	}
}
