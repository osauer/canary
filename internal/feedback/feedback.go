// Package feedback composes the redacted diagnostic report behind `canary
// feedback` and the `canary_feedback` MCP tool, and the GitHub new-issue URL
// that opens the bug report form prefilled with it. It reads the daemon when
// one is reachable and never writes: no file, no network call, no GitHub
// API, no daemon start. The human opens the URL and files the issue.
package feedback

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/osauer/canary/v2/internal/redact"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Conn is the daemon read surface the collector needs; *dial.Conn and the
// CLI's DaemonConn satisfy it. A nil Conn means no daemon is reachable.
type Conn interface {
	Call(ctx context.Context, method string, params, out any) error
}

// Surfaces the bug report form offers, as the user names them.
const (
	SurfaceCLI     = "cli"
	SurfaceMCP     = "mcp"
	SurfaceApp     = "app"
	SurfaceInstall = "install"
)

// Surfaces lists the accepted --surface values in form order.
func Surfaces() []string { return []string{SurfaceCLI, SurfaceMCP, SurfaceApp, SurfaceInstall} }

// formSurface maps a surface to the exact dropdown option of
// .github/ISSUE_TEMPLATE/bug_report.yml; GitHub prefills a dropdown only
// from an exact option label.
var formSurface = map[string]string{
	SurfaceCLI:     "CLI",
	SurfaceMCP:     "MCP server",
	SurfaceApp:     "Canary app",
	SurfaceInstall: "Install or update",
}

// NewIssueURL is the repository's new-issue page; the template query selects
// the bug report form.
const NewIssueURL = "https://github.com/osauer/canary/issues/new"

const (
	// MaxTextBytes bounds the rendered report: GitHub answers 414 to an
	// over-long URL, and the report travels inside one query parameter.
	MaxTextBytes = 2000
	// maxErrorBytes bounds the pasted error inside the report, so the
	// environment lines always fit.
	maxErrorBytes = 700
)

// Host is what the running binary knows about itself.
type Host struct {
	Version string
	// Build is "standard" or "trading".
	Build string
	OS    string
	Arch  string
}

// Input is what the user or agent adds.
type Input struct {
	Surface string
	// Error is free text pasted from a failure; it is redacted before use.
	Error string
}

// Report is the redacted diagnostic. Every string is safe to publish.
type Report struct {
	Surface string `json:"surface"`
	Version string `json:"version"`
	Build   string `json:"build"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Daemon  Daemon `json:"daemon"`
	Data    Data   `json:"data"`
	// Error is the pasted error after redaction, bounded.
	Error string `json:"error,omitempty"`
	// Holdings says whether holding names could be masked: "masked" when
	// the daemon's position list was read, otherwise why not.
	Holdings string `json:"holdings"`
	// Notes name reads that failed, each redacted.
	Notes []string `json:"notes,omitempty"`
}

// Daemon is the connectivity picture as `canary status` reports it, without
// the account, host, port or client id.
type Daemon struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	Connected bool   `json:"connected"`
	// Gateway is the application kind the connection's port family names:
	// "IB Gateway", "TWS", "custom port" or "unknown".
	Gateway     string `json:"gateway,omitempty"`
	Phase       string `json:"phase,omitempty"`
	APIServer   int    `json:"api_server,omitempty"`
	AccountMode string `json:"account_mode,omitempty"`
	DataType    string `json:"data_type,omitempty"`
	Verdict     string `json:"verdict,omitempty"`
	// LastError is the daemon's published connect error, redacted.
	LastError string `json:"last_error,omitempty"`
}

// Data is the `canary data health` summary as source coverage: counts of
// services and data products, never instruments or figures.
type Data struct {
	Available   bool     `json:"available"`
	State       string   `json:"state,omitempty"`
	Label       string   `json:"label,omitempty"`
	Required    int      `json:"required"`
	Current     int      `json:"current"`
	Limited     int      `json:"limited"`
	Unavailable int      `json:"unavailable"`
	Unverified  int      `json:"unverified"`
	Expected    int      `json:"expected_delays"`
	Concerns    []string `json:"concerns,omitempty"`
}

// ErrSurface is returned for a surface the form does not offer.
var ErrSurface = errors.New("surface must be one of " + strings.Join(Surfaces(), ", "))

// Collect builds the report. conn nil, or a daemon that refuses the first
// read, yields a report that says so; every other failure is a note. The
// only error is a surface the form does not offer.
func Collect(ctx context.Context, conn Conn, host Host, in Input) (Report, error) {
	surface := strings.ToLower(strings.TrimSpace(in.Surface))
	if surface == "" {
		surface = SurfaceCLI
	}
	if _, ok := formSurface[surface]; !ok {
		return Report{}, fmt.Errorf("%w (got %q)", ErrSurface, in.Surface)
	}
	r := Report{Surface: surface, Version: nonEmpty(host.Version, "dev"), Build: nonEmpty(host.Build, "standard"), OS: host.OS, Arch: host.Arch}
	id := redact.LocalIdentity()
	var held redact.Holdings
	r.Holdings = "not masked: no daemon reachable"
	if conn != nil && ctx != nil {
		var health rpc.HealthResult
		if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &health); err != nil {
			r.Notes = append(r.Notes, "status not read: "+redact.Outbound(id, held).Text(err.Error()))
		} else {
			r.Daemon = daemonPicture(health)
			id.Hosts = append(id.Hosts, health.GatewayHost)
			r.Daemon.LastError = redact.Outbound(id, held).Text(health.LastError)
		}
		var positions rpc.PositionsResult
		if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &positions); err != nil {
			r.Holdings = "not masked: positions not read (" + redact.Outbound(id, held).Text(err.Error()) + ")"
		} else {
			held = holdingsOf(positions)
			r.Holdings = "masked"
		}
		var data rpc.DataHealthResult
		if err := conn.Call(ctx, rpc.MethodDataHealth, rpc.DataHealthParams{}, &data); err != nil {
			r.Notes = append(r.Notes, "data health not read: "+redact.Outbound(id, held).Text(err.Error()))
		} else {
			r.Data = dataPicture(data, redact.Outbound(id, held))
		}
	}
	if text := strings.TrimSpace(in.Error); text != "" {
		r.Error = cut(redact.Outbound(id, held).Text(text), maxErrorBytes)
	}
	return r, nil
}

func daemonPicture(h rpc.HealthResult) Daemon {
	return Daemon{
		Reachable:   true,
		Version:     h.DaemonVersion,
		Connected:   h.Connected,
		Gateway:     gatewayKind(h.GatewayPort),
		Phase:       h.GatewayPhase,
		APIServer:   h.ServerVersion,
		AccountMode: h.AccountMode,
		DataType:    h.DataType,
		Verdict:     h.Verdict.State,
	}
}

// gatewayKind names the application by its port family, as the install
// guide does; the port itself is configuration and stays out.
func gatewayKind(port int) string {
	switch port {
	case 4001, 4002:
		return "IB Gateway"
	case 7496, 7497:
		return "TWS"
	case 0:
		return "unknown"
	default:
		return "custom port"
	}
}

func holdingsOf(p rpc.PositionsResult) redact.Holdings {
	var names []string
	for _, v := range append(append([]rpc.PositionView{}, p.Stocks...), p.Options...) {
		names = append(names, v.Symbol, v.LocalSymbol)
	}
	for _, g := range p.ByUnderlying {
		names = append(names, g.Underlying)
	}
	return redact.NewHoldings(names)
}

func dataPicture(d rpc.DataHealthResult, r redact.Redactor) Data {
	out := Data{Available: true, State: d.Summary.State, Label: r.Text(d.Summary.Label),
		Required: d.Summary.Required, Current: d.Summary.Current, Limited: d.Summary.Limited,
		Unavailable: d.Summary.Unavailable, Unverified: d.Summary.Unverified, Expected: d.Summary.ExpectedDelays}
	for _, c := range d.Concerns {
		out.Concerns = append(out.Concerns, r.Text(c.Label)+" ("+c.State+")")
	}
	return out
}

// Text renders the report as the diagnostics block of the issue and as the
// stdout copy for pasting, bounded at MaxTextBytes.
func (r Report) Text() string {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "%-10s %s\n", label, value)
	}
	b.WriteString("Canary feedback report (redacted: no account, position, order, host or path data)\n")
	line("Surface", formSurface[r.Surface])
	line("Canary", fmt.Sprintf("%s, %s build, %s/%s", r.Version, r.Build, nonEmpty(r.OS, "unknown os"), nonEmpty(r.Arch, "unknown arch")))
	line("Daemon", r.daemonLine())
	line("Data", r.dataLine())
	line("Holdings", r.Holdings)
	for _, n := range r.Notes {
		line("Note", n)
	}
	if r.Error != "" {
		line("Error", r.Error)
	}
	return cut(b.String(), MaxTextBytes)
}

func (r Report) daemonLine() string {
	d := r.Daemon
	if !d.Reachable {
		return "not reachable (no daemon running, or its socket is elsewhere); gateway and data health not read"
	}
	parts := []string{nonEmpty(d.Version, "version unknown")}
	if d.Connected {
		parts = append(parts, fmt.Sprintf("%s connected, API server %s", d.Gateway, apiServer(d.APIServer)))
	} else {
		parts = append(parts, nonEmpty(d.Gateway, "gateway")+" not connected")
	}
	if d.Phase != "" {
		parts = append(parts, "phase "+d.Phase)
	}
	if d.Verdict != "" {
		parts = append(parts, "status "+d.Verdict)
	}
	if d.AccountMode != "" {
		parts = append(parts, d.AccountMode+" account")
	}
	if d.DataType != "" {
		parts = append(parts, "market data "+d.DataType)
	}
	if d.LastError != "" {
		parts = append(parts, "last error: "+d.LastError)
	}
	return strings.Join(parts, "; ")
}

func apiServer(v int) string {
	if v == 0 {
		return "unknown"
	}
	return fmt.Sprint(v)
}

func (r Report) dataLine() string {
	d := r.Data
	if !d.Available {
		return "not read"
	}
	s := fmt.Sprintf("%s; %d required sources: %d current, %d limited, %d unavailable, %d unverified",
		nonEmpty(d.Label, d.State), d.Required, d.Current, d.Limited, d.Unavailable, d.Unverified)
	if d.Expected > 0 {
		s += fmt.Sprintf(" (%d awaiting the publisher)", d.Expected)
	}
	if len(d.Concerns) > 0 {
		s += "; concerns: " + strings.Join(d.Concerns, ", ")
	}
	return s
}

// Environment is the form's environment field: OS, architecture and the
// gateway application.
func (r Report) Environment() string {
	parts := []string{osName(r.OS) + " " + r.Arch}
	if r.Daemon.Reachable && r.Daemon.Gateway != "" && r.Daemon.Gateway != "unknown" {
		parts = append(parts, r.Daemon.Gateway)
	}
	return strings.Join(parts, ", ")
}

func osName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	case "":
		return "unknown OS"
	}
	return goos
}

// Title is the issue title: the form's prefix and the first words of the
// error, or the surface alone.
func (r Report) Title() string {
	if r.Error == "" {
		return "[Bug]: " + formSurface[r.Surface] + " problem"
	}
	first := r.Error
	if i := strings.IndexAny(first, ";\n"); i > 0 {
		first = first[:i]
	}
	return "[Bug]: " + cut(strings.TrimSpace(first), 80)
}

// IssueURL is the new-issue page with the bug report form prefilled: title,
// surface, version, environment and diagnostics from the report, and the
// error as the summary. Steps to reproduce stay for the human.
func (r Report) IssueURL() string {
	q := url.Values{}
	q.Set("template", "bug_report.yml")
	q.Set("title", r.Title())
	q.Set("surface", formSurface[r.Surface])
	q.Set("version", r.Version+" ("+r.Build+" build)")
	q.Set("environment", r.Environment())
	q.Set("diagnostics", r.Text())
	if r.Error != "" {
		q.Set("summary", r.Error)
	}
	return NewIssueURL + "?" + q.Encode()
}

// cut bounds s at n bytes on a rune boundary, marking the cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "…[cut]"
	i := n - len(mark)
	for i > 0 && !isRuneStart(s[i]) {
		i--
	}
	return strings.TrimRight(s[:i], " ") + mark
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// Result is the machine-readable answer of `canary feedback --json` and the
// `canary_feedback` MCP tool.
type Result struct {
	URL      string `json:"url"`
	Report   string `json:"report"`
	Fields   Report `json:"fields"`
	Redacted bool   `json:"redacted"`
	Next     string `json:"next"`
}

// Compose pairs the report with its URL and what to do with it.
func Compose(r Report) Result {
	return Result{URL: r.IssueURL(), Report: r.Text(), Fields: r, Redacted: true,
		Next: "Open the URL in a browser to file the issue; nothing has been sent. Read the report first: it is redacted, and a bug report must never carry account, position, order, host or path details."}
}
