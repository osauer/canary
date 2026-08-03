package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

// handshakeWaitBudget bounds how long `canary status` waits for the daemon's
// IB Gateway handshake to land. The daemon publishes a bounded connection
// error when TCP succeeds but the API handshake does not; this larger budget
// also covers TLS fallback and slow healthy gateways.
const handshakeWaitBudget = 25 * time.Second

// handshakePollInterval is the cadence at which `status` re-asks the
// daemon for its health snapshot during the wait.
const handshakePollInterval = 500 * time.Millisecond

func runStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.HealthResult
	if err := env.Conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
		return fail(env, "status: %v", err)
	}

	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		var r rpc.HealthResult
		err := env.Conn.Call(ctx, rpc.MethodStatusHealth, nil, &r)
		return r, err
	}
	res = waitForStatusVerdict(ctx, env.Stderr, *jsonOut, res, fetch)

	if *jsonOut {
		return printJSON(env, res)
	}
	// Best-effort: an older daemon without the method just loses the row.
	var alerts *rpc.AlertCandidateSnapshot
	var snapshot rpc.AlertCandidateSnapshot
	if err := env.Conn.Call(ctx, rpc.MethodAlertCandidates, rpc.AlertCandidatesParams{}, &snapshot); err == nil {
		alerts = &snapshot
	}
	renderStatusText(env, &res, alerts)
	if !res.Connected {
		return 1
	}
	return 0
}

// renderStatusText prints the health snapshot as the user-facing status
// screen. Split out from runStatus so the preview tool and future tests
// can exercise the rendering without a live socket.
func renderStatusText(env *Env, res *rpc.HealthResult, alerts *rpc.AlertCandidateSnapshot) {
	out := env.Stdout
	cliVersion := env.Version

	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Gateway  %s\n", env.statusBadge(statusVerdict(*res, cliVersion)))
	fmt.Fprintln(out)

	statusRow(env, out, "Session", formatSessionValue(env, *res))
	statusRow(env, out, "Market data", formatMarketDataValue(env, *res))
	if len(res.DataFarms) > 0 {
		statusRow(env, out, "Data farms", env.yellow(formatDataFarmsValue(res.DataFarms)))
	}
	statusRow(env, out, "Daemon", formatDaemonValue(*res))
	switch {
	case res.Connected:
		statusRow(env, out, "TWS", formatTWSValue(*res))
	case isHandshakeInFlight(*res):
		statusRow(env, out, "TWS", env.dim("handshake in progress"))
	default:
		statusRow(env, out, "TWS", env.red("not connected"))
	}
	if len(res.BackgroundTasks) > 0 {
		statusRow(env, out, "Background", formatBackgroundTasks(res.BackgroundTasks))
	}
	if len(res.Subsystems) > 0 {
		statusRow(env, out, "Subsystems", formatSubsystemsValue(env, res.Subsystems))
	}
	if res.Trading.Mode != "" {
		statusRow(env, out, "Trading", formatTradingStatusValue(env, res.Trading))
	}
	if alerts != nil {
		if value := formatAlertCoverageValue(env, alerts.Coverage); value != "" {
			statusRow(env, out, "Alerts", value)
		}
	}
	if len(res.DataQuality) > 0 {
		statusRow(env, out, "Data quality", env.yellow(formatDataQualityValue(res.DataQuality)))
	}
	if members := formatMembersValue(res.Members); members != "" {
		statusRow(env, out, "SPX members", members)
	}
	if concern := nextConcern(*res, cliVersion); concern.Level != statusConcernNone {
		statusRow(env, out, "Next concern", env.concernText(concern))
	}

	if isHandshakeInFlight(*res) {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  Handshake did not complete within %s. Check the daemon log for\n", handshakeWaitBudget)
		fmt.Fprintln(out, "  the underlying error, then verify in IB Gateway:")
		fmt.Fprintln(out, "    Configure → Settings → API → Settings → 'Enable ActiveX and Socket Clients'")
		fmt.Fprintln(out, "    Trusted IPs include 127.0.0.1 (or empty)")
		fmt.Fprintln(out, "    Login fully completed (not paused at 2FA)")
		fmt.Fprintln(out, env.dim("  Daemon log: "+dial.DisplayPath(dial.DefaultLogPath())))
	} else if !res.Connected {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  Daemon log: "+dial.DisplayPath(dial.DefaultLogPath())))
	}
	fmt.Fprintln(out)
}

// formatAlertCoverageValue is the push-delivery readiness headline: delivery
// waits on every expected alert source claiming coverage at once, so the row
// names the stragglers instead of a bare fraction. `canary alerts` has the
// per-source and per-rule detail.
func formatAlertCoverageValue(env *Env, coverage rpc.AlertCoverage) string {
	total := len(coverage.ExpectedSources)
	if total == 0 {
		return ""
	}
	covered := make(map[rpc.AlertSource]struct{}, len(coverage.CoveredSources))
	for _, source := range coverage.CoveredSources {
		covered[source] = struct{}{}
	}
	missing := make([]string, 0, total)
	for _, source := range coverage.ExpectedSources {
		if _, ok := covered[source]; !ok {
			missing = append(missing, string(source))
		}
	}
	if len(missing) == 0 && coverage.Freshness == rpc.AlertCoverageCurrent {
		return env.green(fmt.Sprintf("%d/%d sources covered", total, total))
	}
	value := fmt.Sprintf("%d/%d sources covered", total-len(missing), total)
	if coverage.Freshness != rpc.AlertCoverageCurrent && coverage.Freshness != "" {
		value += fmt.Sprintf(" (%s)", coverage.Freshness)
	}
	if len(missing) > 0 {
		value += " — missing: " + strings.Join(missing, ", ")
	}
	return env.yellow(value)
}

func statusRow(env *Env, out io.Writer, label, value string) {
	fmt.Fprintf(out, "%s %s\n", env.dim(fmt.Sprintf("%-14s", label)), value)
}

type statusConcernLevel int

const (
	statusConcernNone statusConcernLevel = iota
	statusConcernNotice
	statusConcernWarn
	statusConcernBad
)

type statusConcern struct {
	Text  string
	Level statusConcernLevel
}

func statusVerdict(res rpc.HealthResult, cliVersion string) statusConcern {
	concern := nextConcern(res, cliVersion)
	switch {
	case isHandshakeInFlight(res):
		return statusConcern{Text: "STARTING", Level: statusConcernNotice}
	case !res.Connected:
		return statusConcern{Text: "OFFLINE", Level: statusConcernBad}
	case (concern.Level == statusConcernWarn || concern.Level == statusConcernBad) && !isDataQualityConcern(concern):
		return statusConcern{Text: "ATTENTION", Level: statusConcernWarn}
	default:
		return statusConcern{Text: "READY", Level: statusConcernNone}
	}
}

func (e *Env) statusBadge(c statusConcern) string {
	text := e.bold(c.Text)
	switch c.Level {
	case statusConcernBad:
		return e.red(text)
	case statusConcernWarn:
		return e.yellow(text)
	case statusConcernNotice:
		return e.dim(text)
	default:
		return e.green(text)
	}
}

func (e *Env) concernText(c statusConcern) string {
	switch c.Level {
	case statusConcernBad:
		return e.red(c.Text)
	case statusConcernWarn:
		return e.yellow(c.Text)
	case statusConcernNotice:
		return e.dim(c.Text)
	default:
		return e.green(c.Text)
	}
}

func nextConcern(res rpc.HealthResult, cliVersion string) statusConcern {
	switch {
	case !res.Connected && res.LastError != "":
		return statusConcern{Text: "Gateway offline: " + res.LastError, Level: statusConcernBad}
	case isHandshakeInFlight(res):
		return statusConcern{Text: "Gateway handshake still in progress", Level: statusConcernNotice}
	case daemonVersionDrift(res.DaemonVersion, cliVersion):
		return statusConcern{
			Text:  fmt.Sprintf("CLI version %s differs from daemon %s; run `canary restart` to pick up the new binary", cliVersion, res.DaemonVersion),
			Level: statusConcernWarn,
		}
	case len(res.DataFarms) > 0:
		return statusConcern{Text: "Data farm issue: " + formatDataFarmsValue(res.DataFarms), Level: statusConcernWarn}
	case firstSubsystemConcernText(res.Subsystems) != "":
		return statusConcern{Text: "Subsystem issue: " + firstSubsystemConcernText(res.Subsystems), Level: statusConcernWarn}
	case res.Connected && !rpc.IsLiveDataType(res.DataType):
		return statusConcern{Text: "Market data is " + dataTypeLabel(res.DataType), Level: statusConcernWarn}
	case res.Connected && res.GatewayTLS != res.NegotiatedTLS:
		return statusConcern{
			Text:  fmt.Sprintf("TLS fallback: configured %v, negotiated %v", res.GatewayTLS, res.NegotiatedTLS),
			Level: statusConcernWarn,
		}
	case membersRefreshNeedsAttention(res.Members):
		return statusConcern{Text: "SPX members refresh " + res.Members.RefreshState, Level: statusConcernWarn}
	case res.Trading.Mode != config.TradingModeDisabled && res.Trading.Blocked:
		return statusConcern{Text: "Trading blocked: " + firstTradingBlockerMessage(res.Trading), Level: statusConcernWarn}
	case len(res.BackgroundTasks) > 0:
		return statusConcern{Text: "Background work: " + formatBackgroundTasks(res.BackgroundTasks), Level: statusConcernNotice}
	default:
		return statusConcern{Text: "None", Level: statusConcernNone}
	}
}

func firstSubsystemIssue(subs []rpc.SubsystemHealth) (rpc.SubsystemHealth, bool) {
	for _, sub := range subs {
		switch strings.ToLower(strings.TrimSpace(sub.Status)) {
		case "degraded", "unavailable", "error":
			return sub, true
		}
	}
	return rpc.SubsystemHealth{}, false
}

func firstSubsystemConcernText(subs []rpc.SubsystemHealth) string {
	sub, ok := firstSubsystemIssue(subs)
	if !ok {
		return ""
	}
	return formatSubsystemConcern(sub)
}

func formatSubsystemConcern(sub rpc.SubsystemHealth) string {
	name := strings.TrimSpace(sub.Name)
	if name == "" {
		name = "unknown"
	}
	status := strings.TrimSpace(sub.Status)
	if status == "" {
		status = "unhealthy"
	}
	out := name + " " + status
	if msg := strings.TrimSpace(sub.Message); msg != "" {
		out += ": " + msg
	}
	return out
}

func firstTradingBlockerMessage(st rpc.TradingStatus) string {
	if len(st.Blockers) == 0 {
		return "local gate blocked"
	}
	return st.Blockers[0].Message
}

func isDataQualityConcern(c statusConcern) bool {
	return c.Level == statusConcernWarn && strings.HasPrefix(c.Text, "Data quality:")
}

func daemonVersionDrift(daemonVersion, cliVersion string) bool {
	return daemonVersion != "" && daemonVersion != "dev" &&
		cliVersion != "" && cliVersion != "dev" &&
		daemonVersion != cliVersion
}

func formatSessionValue(env *Env, res rpc.HealthResult) string {
	account := statusAccountID(res)
	if mode := formatStatusAccountMode(env, res.AccountMode); mode != "" {
		account += " (" + mode + ")"
	}
	endpoint := formatGatewayAddress(res)
	value := fmt.Sprintf("%s via %s %s, client %d", account, endpoint, formatGatewayBadge(res), res.ClientID)
	if len(res.Alternates) > 0 {
		value += "; also up: " + joinPorts(res.Alternates)
	}
	return value
}

func statusAccountID(res rpc.HealthResult) string {
	// A login carrying several accounts advertises the comma-joined
	// managedAccounts list, which is a session inventory rather than an account
	// code. Printing it would name every sibling and claim the session is
	// scoped to all of them; the configured pin is the account it is actually
	// scoped to.
	if account := strings.TrimSpace(res.ConnectedAccount); account != "" && !strings.ContainsAny(account, ", \t") {
		return account
	}
	return nonEmpty(res.Account, "auto-detect")
}

func formatStatusAccountMode(env *Env, mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case rpc.AccountModePaper:
		return env.yellow(env.bold("PAPER"))
	case rpc.AccountModeLive:
		return "live"
	case rpc.AccountModeUnknown:
		return env.dim("unknown")
	default:
		return strings.TrimSpace(mode)
	}
}

func formatGatewayAddress(res rpc.HealthResult) string {
	host := nonEmpty(res.GatewayHost, "auto-detect")
	if res.GatewayPort == 0 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, res.GatewayPort)
}

func formatMarketDataValue(env *Env, res rpc.HealthResult) string {
	if !res.Connected {
		return env.dim("unavailable until connected")
	}
	label := dataTypeLabel(nonEmpty(res.DataType, rpc.MarketDataLive))
	if !rpc.IsLiveDataType(res.DataType) {
		return env.yellow(label + " ⚠")
	}
	return label
}

func formatDataFarmsValue(farms []rpc.DataFarmHealth) string {
	parts := make([]string, 0, len(farms))
	for _, farm := range farms {
		name := strings.TrimSpace(farm.Name)
		if name == "" {
			name = strings.TrimSpace(farm.Type)
		}
		if name == "" {
			name = "unknown"
		}
		if farm.Type != "" {
			name = farm.Type + ":" + name
		}
		status := strings.TrimSpace(farm.Status)
		if status == "" {
			status = "unhealthy"
		}
		part := name + " " + status
		if farm.Code != 0 {
			part += fmt.Sprintf(" (IBKR %d)", farm.Code)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func dataTypeLabel(dt string) string {
	dt = nonEmpty(dt, rpc.MarketDataLive)
	switch dt {
	case rpc.MarketDataLive:
		return "Live"
	case rpc.MarketDataFrozen:
		return "Frozen"
	case rpc.MarketDataDelayed:
		return "Delayed"
	case rpc.MarketDataDelayedFrozen:
		return "Delayed frozen"
	default:
		return dt
	}
}

func formatDaemonValue(res rpc.HealthResult) string {
	uptime := time.Duration(res.UptimeSeconds) * time.Second
	if uptime <= 0 {
		return nonEmpty(res.DaemonVersion, "unknown") + ", just started"
	}
	return fmt.Sprintf("%s, up %s", nonEmpty(res.DaemonVersion, "unknown"), uptime)
}

func formatTWSValue(res rpc.HealthResult) string {
	if res.ServerVersion == 0 {
		return "connected, API server unknown"
	}
	return fmt.Sprintf("API server %d", res.ServerVersion)
}

func formatBackgroundTasks(tasks []rpc.BackgroundTaskStatus) string {
	phrases := make([]string, 0, len(tasks))
	for _, t := range tasks {
		phrases = append(phrases, backgroundTaskPhrase(t.Name))
	}
	return strings.Join(phrases, ", ")
}

// formatMembersValue renders the S&P 500 members metadata that lives
// under the breadth surface. Returns the empty string when the daemon
// hasn't populated the field yet.
func formatMembersValue(m rpc.MembersHealth) string {
	if m.Source == "" {
		return ""
	}
	asOf := "unknown"
	if !m.AsOf.IsZero() {
		asOf = m.AsOf.Format("2006-01-02")
	}
	name := "names"
	if m.Count == 1 {
		name = "name"
	}
	base := fmt.Sprintf("%s:%s, %d %s", m.Source, asOf, m.Count, name)
	if !membersRefreshNeedsAttention(m) {
		return base
	}
	return base + ", refresh " + m.RefreshState
}

func formatSubsystemsValue(env *Env, subs []rpc.SubsystemHealth) string {
	// Ten "name:ready" entries carry no information; only the exceptions do.
	// Collapse the healthy majority and keep every non-ready entry verbatim.
	ready := 0
	parts := make([]string, 0, len(subs))
	for _, s := range subs {
		if s.Name == "" || s.Status == "" {
			continue
		}
		if s.Status == "ready" {
			ready++
			continue
		}
		status := s.Status
		switch s.Status {
		case "computing", "degraded":
			status = env.yellow(status)
		case "unavailable", "error":
			status = env.red(status)
		default:
			status = env.dim(status)
		}
		part := s.Name + ":" + status
		if s.Progress > 0 {
			part += fmt.Sprintf(" %d%%", s.Progress)
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		if ready == 0 {
			return ""
		}
		return env.green(fmt.Sprintf("all %d ready", ready))
	}
	if ready > 0 {
		parts = append(parts, env.dim(fmt.Sprintf("+%d ready", ready)))
	}
	return strings.Join(parts, ", ")
}

func formatDataQualityValue(items []rpc.DataQualityHealth) string {
	return formatDataQualityValueAt(items, time.Now())
}

func formatDataQualityValueAt(items []rpc.DataQualityHealth, now time.Time) string {
	parts := make([]string, 0, len(items))
	for _, q := range items {
		surface := strings.TrimSpace(q.Surface)
		summary := strings.TrimSpace(q.Summary)
		if surface == "" {
			continue
		}
		if len(q.DegradedClusters) > 0 {
			summary = formatDataQualityDegraded(q)
		} else if len(q.StaleClusters) > 0 {
			summary = formatDataQualityStale(q, now)
		} else if summary == "" {
			summary = strings.TrimSpace(q.Status)
		}
		if strings.TrimSpace(summary) == "" {
			parts = append(parts, surface)
			continue
		}
		parts = append(parts, surface+" "+summary)
	}
	return strings.Join(parts, "; ")
}

func formatDataQualityDegraded(q rpc.DataQualityHealth) string {
	if len(q.DegradedClusters) == 1 && q.DegradedClusters[0] == "gamma" && strings.Contains(q.Summary, "SPX excluded") {
		return "degraded (SPX excluded)"
	}
	if q.Summary != "" {
		return q.Summary
	}
	return "degraded: " + strings.Join(q.DegradedClusters, ", ")
}

func formatDataQualityStale(q rpc.DataQualityHealth, now time.Time) string {
	clusters := strings.Join(q.StaleClusters, ", ")
	if clusters == "" {
		return q.Summary
	}
	if rpc.ClassifySession(now) != rpc.SessionRTH {
		return "stale (off-hours: " + clusters + ")"
	}
	return "stale: " + clusters
}

func membersRefreshNeedsAttention(m rpc.MembersHealth) bool {
	return m.Source != "" && m.RefreshState != "" && m.RefreshState != "healthy"
}

// isHandshakeInFlight reports whether the daemon has reported neither a
// successful connection nor a connect error yet — i.e. the gateway
// handshake goroutine started but hasn't produced a verdict.
func isHandshakeInFlight(res rpc.HealthResult) bool {
	return !res.Connected && res.LastError == ""
}

// backgroundTaskPhrase renders a stable wire token (the one the
// daemon's backgroundTasks() emits) as a short verb phrase suitable
// for the status row. Unknown tokens fall through verbatim so a
// daemon shipping a new task name still appears in `canary status`
// even before the CLI has been updated.
func backgroundTaskPhrase(token string) string {
	switch token {
	case "breadth-spx":
		return "refreshing rolling SPX breadth"
	case "gamma-zero":
		return "computing dealer zero-gamma"
	case "regime-prewarm":
		return "warming regime cache"
	default:
		return token
	}
}

// healthFetcher is the closure waitForHandshake uses to re-poll the
// daemon. Indirected so tests can drive the wait deterministically
// without a live socket.
type healthFetcher func(ctx context.Context) (rpc.HealthResult, error)

// waitForStatusVerdict waits out the daemon's first IB Gateway handshake
// verdict. JSON output must stay clean, so its progress UI is discarded
// instead of being mixed into stdout.
func waitForStatusVerdict(ctx context.Context, progress io.Writer, jsonOut bool, initial rpc.HealthResult, fetch healthFetcher) rpc.HealthResult {
	if !isHandshakeInFlight(initial) {
		return initial
	}
	if jsonOut {
		progress = io.Discard
	}
	return waitForHandshake(ctx, progress, fetch, initial, handshakeWaitBudget, handshakePollInterval)
}

// waitForHandshake polls fetch until it returns a verdict (Connected, or
// LastError set) or budget elapses.
func waitForHandshake(ctx context.Context, w io.Writer, fetch healthFetcher, initial rpc.HealthResult, budget, pollInterval time.Duration) rpc.HealthResult {
	fmt.Fprintf(w, "canary: waiting for IB Gateway handshake (up to %s)", budget)
	defer fmt.Fprintln(w)

	res := initial
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return res
		case <-time.After(pollInterval):
		}
		fmt.Fprint(w, ".")
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

// formatGatewayBadge renders the TLS state and the discovery origin
// compactly. Examples:
//
//	(tls=false, discovered)
//	(tls=true, pinned)
//	(tls=true, configured=false ⚠ fallback)
//
// The fallback marker only fires when configured ≠ negotiated, which can
// only happen when EnableTLSFallback is on (i.e. TLS auto). Pinned TLS
// (true or false) suppresses fallback entirely, so configured always
// equals negotiated.
func formatGatewayBadge(res rpc.HealthResult) string {
	tlsTok := fmt.Sprintf("tls=%v", res.GatewayTLS)
	if res.Connected && res.GatewayTLS != res.NegotiatedTLS {
		tlsTok = fmt.Sprintf("tls=%v, configured=%v ⚠ fallback", res.NegotiatedTLS, res.GatewayTLS)
	}
	origin := res.PortOrigin
	if origin == "" {
		// Pre-AUTO daemon or empty discovery info — render compactly.
		return fmt.Sprintf("(%s)", tlsTok)
	}
	return fmt.Sprintf("(%s, %s)", tlsTok, origin)
}

func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = fmt.Sprintf("%d", p)
	}
	return strings.Join(parts, ", ")
}
