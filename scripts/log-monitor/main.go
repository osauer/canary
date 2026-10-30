// Command log-monitor incrementally reads Canary daemon and app logs and
// reports incidents, not line shapes. It emits bounded, redacted JSON so
// scheduled agents never ingest raw logs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	reportVersion      = 4
	defaultMaxSignals  = 10
	maxScannerCapacity = 1024 * 1024

	// A scan window spans however long it has been since the last check, so
	// counting lifecycle markers across it reads a day of deliberate restarts
	// as a crash loop. Only starts this close together are evidence of one.
	restartLoopWindow = 10 * time.Minute
	restartLoopStarts = 4
)

type options struct {
	daemonLog string
	appLog    string
	// daemonCrashLog is the daemon's process-level fatal stream (runtime
	// dumps, panics), kept beside the daemon log. Empty derives it from
	// daemonLog; its cursor likewise derives from daemonOffset.
	daemonCrashLog    string
	daemonCrashOffset string
	daemonOffset      string
	appOffset         string
	// openItems persists what was open at the end of a run; empty derives
	// <daemonOffset>.open.json.
	openItems        string
	states           string
	daemonMirror     string
	holdingsCache    string
	maxSignals       int
	commit           bool
	staleAfter       time.Duration
	mirrorStaleAfter time.Duration
	// engine is the incident configuration; the zero value means defaults.
	engine engineConfig
}

type report struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	// NeedsAttention is true exactly when Attention is non-empty.
	NeedsAttention bool `json:"needs_attention"`
	// Attention names each item that needs the owner and why: error,
	// new_family, long_incident, open_incident, grew or coverage. Everything
	// else in the report is informational.
	Attention        []attentionItem `json:"attention,omitempty"`
	AttentionOmitted int             `json:"attention_omitted,omitempty"`
	// Holdings says where the current-holdings list that masks names came
	// from: store, cache, or unavailable (contract names withheld).
	Holdings     string             `json:"holdings"`
	Declarations declarationsReport `json:"declarations"`
	// CarryState says whether the previous run's open items were read:
	// loaded, missing (first run) or reset (unreadable, started afresh).
	CarryState string    `json:"carry_state"`
	Daemon     logReport `json:"daemon"`
	App        logReport `json:"app"`
	// CrashLog names process stderr, which may contain allocator diagnostics
	// as well as fatal output. Counts describe lines, never crash events.
	CrashLog  string           `json:"crash_log,omitempty"`
	CrashScan *crashScanReport `json:"crash_scan,omitempty"`
}

type crashScanReport struct {
	State                    string    `json:"state"`
	ModifiedAt               time.Time `json:"modified_at"`
	FirstRead                bool      `json:"first_read"`
	OffsetReset              bool      `json:"offset_reset,omitempty"`
	NewLines                 int       `json:"new_lines"`
	AllocatorDiagnosticLines int       `json:"allocator_diagnostic_lines"`
	EventTimesKnown          bool      `json:"event_times_known"`
}

type logReport struct {
	Path          string         `json:"path"`
	ModifiedAt    time.Time      `json:"modified_at,omitzero"`
	State         string         `json:"state"`
	NewLines      int            `json:"new_lines"`
	KnownBenign   int            `json:"known_benign"`
	Informational int            `json:"informational"`
	OffsetReset   bool           `json:"offset_reset,omitempty"`
	Families      map[string]int `json:"families,omitempty"`
	// Builds lists, in order, the daemon builds whose start marker falls in
	// the window, so a line can be told apart from a build already replaced.
	Builds []string `json:"builds,omitempty"`
	// Incidents are connectivity episodes, daemon restarts and failure runs,
	// each with the families attributed to it.
	Incidents        []incident `json:"incidents,omitempty"`
	IncidentsOmitted int        `json:"incidents_omitted,omitempty"`
	// Signals holds the max-signals highest-ranked standalone families —
	// those no incident explains — attention first, then severity, then
	// count.
	Signals []signal `json:"signals,omitempty"`
	// Suppressed summarizes every family the cap cut, one row per family.
	Suppressed        []suppressedSummary `json:"suppressed,omitempty"`
	SuppressedSignals int                 `json:"suppressed_signals,omitempty"`
	// Standing families recur on many prior days at an unchanged rate.
	Standing []standingItem `json:"standing,omitempty"`
	// Declared items match the owner's states file and never need attention.
	Declared []declaredItem `json:"declared,omitempty"`
	// Resolved items were open at the previous run and are absent now; each
	// is reported once.
	Resolved     []resolvedItem `json:"resolved,omitempty"`
	ListsOmitted int            `json:"lists_omitted,omitempty"`
	// Mirror is present when the input is a copy kept by a log mirror.
	Mirror *mirrorReport `json:"mirror,omitempty"`
}

type signal struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Family   string `json:"family"`
	// Message is the family's most frequent redacted sample; OtherSamples
	// are up to three more, and Distinct counts every distinct sample.
	Message      string   `json:"message"`
	OtherSamples []string `json:"other_samples,omitempty"`
	Count        int      `json:"count"`
	Distinct     int      `json:"distinct"`
	// Status is new (never in recurrence history), returning (in history,
	// not open at the previous run) or carried (open at the previous run).
	Status    string    `json:"status"`
	FirstSeen time.Time `json:"first_seen,omitzero"`
	DaysSeen  int       `json:"days_seen,omitempty"`
	First     time.Time `json:"first,omitzero"`
	Last      time.Time `json:"last,omitzero"`
	Attention string    `json:"attention,omitempty"`
}

type suppressedSummary struct {
	Family   string `json:"family"`
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Count    int    `json:"count"`
	Status   string `json:"status"`
	// Sample is the family's most frequent message, already redacted.
	Sample string `json:"sample,omitempty"`
}

type scannedLog struct {
	lines       []string
	total       int
	state       string
	offsetReset bool
	firstRead   bool
	path        string
	modified    time.Time
	coverage    string
	cursor      logCursor
}

var (
	// Match only this known startup diagnostic, never arbitrary allocator or
	// crash text. A panic elsewhere in the same scan remains an ERROR.
	mallocDisabledDiagnostic = regexp.MustCompile(`^(?:[A-Za-z0-9_.-]+\([0-9]+\) )?MallocStackLogging: can't turn off malloc stack logging because it was not enabled\.$`)
	accountPattern           = regexp.MustCompile(`\b(?:DU|U)\d{5,}\b`)
	sensitiveField           = regexp.MustCompile(`(?i)\b(account(?:_id)?|order(?:_id|_?ref)?|preview_token|token|balance|holding|position|symbol|conid|reqid)=("[^"]*"|\S+)`)
	symbolPhrase             = regexp.MustCompile(`(?i)\bfor [A-Z][A-Z0-9.]{0,9} via\b`)
	spacePattern             = regexp.MustCompile(`\s+`)
	statusPattern            = regexp.MustCompile(`(?:^|\s)status=(\d{3})(?:\s|$)`)
	timePattern              = regexp.MustCompile(`(?:^|\s)time=([^\s]+)`)
	// Ephemeral values embedded in messages (socket addresses, event
	// timestamps) would make every occurrence a distinct signal, letting one
	// reconnect storm fill the whole bounded list with unmergeable
	// singletons. Normalize them so repeats collapse into one counted signal.
	addrPattern     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}:\d{1,5}\b`)
	inlineTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`)
	// Elapsed times, retry clocks, option expiries, rates, pids and progress
	// counters vary per attempt too: 172 gamma prewarm failures in one day
	// were 172 singletons, all cut into the suppressed summary. Codes and
	// statuses are kept, so distinct broker errors stay distinct signals.
	durationPattern = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:h|m|s|ms|µs)(?:\d+(?:\.\d+)?(?:m|s|ms|µs))*\b`)
	expiryPattern   = regexp.MustCompile(`\b20\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\b`)
	clockPattern    = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}\b`)
	ratePattern     = regexp.MustCompile(`\b\d+\.\d{3,}\b`)
	pidPattern      = regexp.MustCompile(`\bpid \d+\b`)
	counterPattern  = regexp.MustCompile(`\b([a-z_]+)=\d+(?:[./]\d+)?\b|\(cached \d+ so far\)`)
	// Broker order references also travel in free text: the WhatIf timeout
	// names "order 48213", broker notices "OrderId 48213" or "permId =…", the
	// order wire summary "orderId=…", the modify-token check "permanent ID
	// …, current order is …". A camelCase identifier ending in Order
	// ("openOrder 48213") has no word boundary before "order", so it is
	// matched on its own. The keyword and separator stay; the number does
	// not. "order 2 of 3" is a position, kept by redactOrderRefs.
	orderRefPattern = regexp.MustCompile(`\b(?:[A-Za-z]*[a-z]Order|(?i:order|perm(?:anent)?|parent[ _]?id))(?i:[ _]?id)?(?i:\s*[=:#]\s*|\s+(?:is\s+)?#?)(\d+)\b`)
	ordinalTail     = regexp.MustCompile(`^\s+of\s+\d`)
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "log-monitor: resolve home: %v\n", err)
		os.Exit(2)
	}
	opts := options{engine: defaultEngineConfig()}
	flag.StringVar(&opts.daemonLog, "daemon-log", filepath.Join(home, ".local", "state", "ibkr", "ibkr-daemon.log"), "daemon log path")
	flag.StringVar(&opts.appLog, "app-log", defaultAppLog(home, runtime.GOOS), "app log path (macOS launchd stderr by default)")
	flag.StringVar(&opts.daemonCrashLog, "daemon-crash-log", "", "daemon crash output path (default: the daemon log's name with a .crash.log suffix, beside it)")
	flag.StringVar(&opts.daemonCrashOffset, "daemon-crash-offset", "", "crash output private cursor path (default: -daemon-offset with a .crash suffix)")
	flag.StringVar(&opts.daemonOffset, "daemon-offset", filepath.Join(home, ".claude", "scheduled-tasks", "ibkr-daemon-log-check.offset"), "daemon private cursor path (legacy line offsets are migrated)")
	flag.StringVar(&opts.appOffset, "app-offset", filepath.Join(home, ".claude", "scheduled-tasks", "ibkr-daemon-log-check.app.offset"), "app private cursor path (legacy line offsets are migrated)")
	flag.StringVar(&opts.openItems, "open-items", "", "private record of items open at the end of a run (default: -daemon-offset with a .open.json suffix)")
	flag.StringVar(&opts.states, "states", filepath.Join(xdgDir("XDG_CONFIG_HOME", home, ".config"), "ibkr", "log-monitor-states.json"), "owner-declared states file, read only; a missing file declares nothing")
	flag.IntVar(&opts.maxSignals, "max-signals", defaultMaxSignals, "maximum standalone family signals per log, applied after incident grouping")
	flag.BoolVar(&opts.commit, "commit", true, "persist offsets and open items after a successful scan")
	flag.DurationVar(&opts.staleAfter, "stale-after", 24*time.Hour, "flag log coverage unverified after this inactivity; zero disables")
	flag.StringVar(&opts.daemonMirror, "daemon-mirror-status", "", "sync record of the mirror that copies -daemon-log from another machine; empty for a local log")
	flag.DurationVar(&opts.mirrorStaleAfter, "mirror-stale-after", 20*time.Minute, "flag mirrored log coverage unverified when the last completed sync is older; zero disables")
	flag.StringVar(&opts.holdingsCache, "holdings-cache", filepath.Join(xdgDir("XDG_CACHE_HOME", home, ".cache"), "ibkr", "holdings-denylist"), "the holdings file the daemon writes for the account-data gate: the current holdings a report must not name")
	flag.DurationVar(&opts.engine.before, "incident-before", opts.engine.before, "attribute lines this long before an incident's first anchor to it")
	flag.DurationVar(&opts.engine.after, "incident-after", opts.engine.after, "attribute lines this long after an incident's last anchor to it")
	flag.DurationVar(&opts.engine.restartEdge, "restart-edge", opts.engine.restartEdge, "attribute lines this soon after a daemon start to the restart")
	flag.DurationVar(&opts.engine.gap, "incident-gap", opts.engine.gap, "chain anchors (and a subsystem's failures) this close together into one incident")
	flag.DurationVar(&opts.engine.longIncident, "incident-attention", opts.engine.longIncident, "an incident at least this long needs attention")
	flag.IntVar(&opts.engine.standingDays, "standing-days", opts.engine.standingDays, "prior days a family must recur at an unchanged rate to read as standing")
	flag.Parse()
	held = loadHoldings(opts.holdingsCache)

	result, err := run(opts, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "log-monitor: %v\n", err)
		os.Exit(2)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "log-monitor: encode report: %v\n", err)
		os.Exit(2)
	}
}

func run(opts options, now time.Time) (report, error) {
	if opts.maxSignals < 1 || opts.maxSignals > 100 {
		return report{}, fmt.Errorf("max-signals must be between 1 and 100")
	}
	cfg := opts.engine
	if cfg == (engineConfig{}) {
		cfg = defaultEngineConfig()
	}
	if cfg.gap <= 0 || cfg.before < 0 || cfg.after < 0 || cfg.restartEdge < 0 || cfg.longIncident <= 0 {
		return report{}, fmt.Errorf("incident windows must be positive")
	}
	daemon, err := scanIncremental(opts.daemonLog, opts.daemonOffset)
	if err != nil {
		return report{}, fmt.Errorf("scan daemon log: %w", err)
	}
	app, err := scanIncremental(opts.appLog, opts.appOffset)
	if err != nil {
		return report{}, fmt.Errorf("scan app log: %w", err)
	}
	crashLog, crashOffset := opts.crashPaths()
	crash, err := scanIncremental(crashLog, crashOffset)
	if err != nil {
		return report{}, fmt.Errorf("scan daemon crash log: %w", err)
	}
	openPath := opts.openItemsPath()
	prior, carryState := readOpenItems(openPath)
	decls := loadDeclarations(opts.states)

	d := classifyDaemon(daemon, cfg, seedCluster(prior), prior.LastBuild)
	ap := classifyApp(app)
	result := report{
		Version:      reportVersion,
		GeneratedAt:  now,
		Holdings:     held.source,
		Declarations: decls.report,
		CarryState:   carryState,
	}
	if crash.state != "missing" {
		result.CrashLog = crash.path
		result.CrashScan = &crashScanReport{State: crash.state, ModifiedAt: crash.modified, FirstRead: crash.firstRead, OffsetReset: crash.offsetReset, NewLines: len(crash.lines)}
	}
	// No per-record timestamp is available in this stream. An initial scan
	// can cover old process starts; neither line count nor mtime dates crashes.
	if crash.state == "scanned" {
		first, lines := "", 0
		for _, line := range crash.lines {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				if mallocDisabledDiagnostic.MatchString(trimmed) {
					result.CrashScan.AllocatorDiagnosticLines++
					continue
				}
				lines++
				if first == "" {
					first = redactMessage(trimmed)
				}
			}
		}
		if lines > 0 {
			d.addItem("ERROR", "crash_output", "crash_output", "daemon process stderr (undated lines, not a crash count): "+first, lines)
		}
	}
	applyCoverage(d, daemon, now, opts.staleAfter)
	applyCoverage(ap, app, now, opts.staleAfter)
	if opts.daemonMirror != "" {
		applyMirrorCoverage(d, opts.daemonMirror, now, opts.mirrorStaleAfter)
	}
	dHistory := trackFamilies(d, &daemon, now)
	aHistory := trackFamilies(ap, &app, now)

	f := &finalizer{cfg: cfg, now: now, maxSignals: opts.maxSignals, decls: decls, prior: prior,
		next: openItemsFile{Version: openItemsVersion, LastBuild: d.lastBuild, Items: map[string]openItem{}}}
	result.Daemon = f.finalize("daemon", d, dHistory)
	result.App = f.finalize("app", ap, aHistory)
	result.Attention = f.attention
	if len(result.Attention) > maxAttention {
		result.AttentionOmitted = len(result.Attention) - maxAttention
		result.Attention = result.Attention[:maxAttention:maxAttention]
	}
	result.NeedsAttention = len(result.Attention) > 0

	if opts.commit {
		if daemon.state != "missing" {
			if err := writeCursor(opts.daemonOffset, daemon.cursor); err != nil {
				return report{}, fmt.Errorf("write daemon offset: %w", err)
			}
		}
		if app.state != "missing" {
			if err := writeCursor(opts.appOffset, app.cursor); err != nil {
				return report{}, fmt.Errorf("write app offset: %w", err)
			}
		}
		if crash.state != "missing" {
			if err := writeCursor(crashOffset, crash.cursor); err != nil {
				return report{}, fmt.Errorf("write crash offset: %w", err)
			}
		}
		if err := writeOpenItems(openPath, f.next); err != nil {
			return report{}, fmt.Errorf("write open items: %w", err)
		}
	}
	return result, nil
}

func (o options) openItemsPath() string {
	if o.openItems != "" {
		return o.openItems
	}
	return o.daemonOffset + ".open.json"
}

func (o options) crashPaths() (logPath, offsetPath string) {
	logPath, offsetPath = o.daemonCrashLog, o.daemonCrashOffset
	if logPath == "" {
		base := strings.TrimSuffix(filepath.Base(o.daemonLog), ".log")
		logPath = filepath.Join(filepath.Dir(o.daemonLog), base+".crash.log")
	}
	if offsetPath == "" {
		offsetPath = o.daemonOffset + ".crash"
	}
	return logPath, offsetPath
}

func newLogReport(scanned scannedLog) logReport {
	return logReport{
		State:       scanned.state,
		Path:        scanned.path,
		ModifiedAt:  scanned.modified,
		NewLines:    len(scanned.lines),
		OffsetReset: scanned.offsetReset,
		Families:    map[string]int{},
	}
}

func severityRank(severity string) int {
	switch severity {
	case "ERROR", "FATAL":
		return 2
	case "WARN":
		return 1
	default:
		return 0
	}
}

// finalizeSignals ranks collected signals by severity then occurrence count
// (stable on arrival order for ties), keeps the top max, and rolls everything

// finalizeSignals ranks the standalone family signals — attention first,
// then severity, then count (stable on arrival order for ties) — keeps the
// top max, and rolls each family the cap cut into one suppressed row.
func finalizeSignals(result *logReport, max int) {
	slices.SortStableFunc(result.Signals, func(a, b signal) int {
		if d := boolRank(b.Attention != "") - boolRank(a.Attention != ""); d != 0 {
			return d
		}
		if d := severityRank(b.Severity) - severityRank(a.Severity); d != 0 {
			return d
		}
		return b.Count - a.Count
	})
	if len(result.Signals) <= max {
		return
	}
	dropped := result.Signals[max:]
	result.Signals = result.Signals[:max:max]
	for _, s := range dropped {
		result.SuppressedSignals += s.Count
		if len(result.Suppressed) < maxSuppressed {
			result.Suppressed = append(result.Suppressed, suppressedSummary{Family: s.Family, Severity: s.Severity, Kind: s.Kind, Count: s.Count, Status: s.Status, Sample: s.Message})
		}
	}
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func daemonNoticeFamily(line string) string {
	switch {
	case strings.Contains(line, "code=354"):
		return "broker_code_354"
	case strings.Contains(line, "code=300"):
		return "broker_code_300"
	case strings.Contains(line, "code=2129"):
		return "broker_code_2129_indicative"
	case strings.Contains(line, "code=200") && strings.Contains(line, "No security definition has been found"):
		return "broker_code_200_no_definition"
	case strings.Contains(line, "code=366") && strings.Contains(line, "No historical data query found"):
		return "broker_code_366_no_history"
	default:
		return ""
	}
}

func lifecycleTimes(lines []string, lifecycle func(string) bool) []time.Time {
	var out []time.Time
	for _, line := range lines {
		if !lifecycle(line) {
			continue
		}
		if ts, ok := logTime(line); ok {
			out = append(out, ts)
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

// restartLoopCount reports the most lifecycle markers falling inside any single
// restartLoopWindow, so restarts spread across a day never read as a loop.
func restartLoopCount(sorted []time.Time) int {
	worst := 0
	for i, start := range sorted {
		n := 0
		for _, ts := range sorted[i:] {
			if ts.Sub(start) > restartLoopWindow {
				break
			}
			n++
		}
		worst = max(worst, n)
	}
	return worst
}

func nearRestart(line string, restarts []time.Time) bool {
	ts, ok := logTime(line)
	if !ok {
		return false
	}
	for _, restart := range restarts {
		delta := ts.Sub(restart)
		if delta >= -time.Minute && delta <= time.Minute {
			return true
		}
	}
	return false
}

func logTime(line string) (time.Time, bool) {
	match := timePattern.FindStringSubmatch(line)
	if len(match) != 2 {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, strings.Trim(match[1], `"`))
	return ts, err == nil
}

func severity(line string) string {
	for _, level := range []string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL"} {
		if strings.Contains(line, "level="+level) {
			return level
		}
	}
	fields := strings.Fields(line)
	if len(fields) >= 3 && looksLegacyDate(fields[0]) {
		level := strings.ToUpper(strings.Trim(fields[2], "[]:"))
		switch level {
		case "DEBUG", "INFO", "WARN", "ERROR", "FATAL":
			return level
		}
	}
	return ""
}

func looksLegacyDate(value string) bool {
	_, err := time.Parse("2006/01/02", value)
	return err == nil
}

func safeMessage(line string) string {
	message := extractSlogMessage(line)
	if message == "" {
		fields := strings.Fields(line)
		if len(fields) >= 3 && looksLegacyDate(fields[0]) {
			start := 2
			if severity(line) != "" {
				start = 3
			}
			if len(fields) > start {
				message = strings.Join(fields[start:], " ")
			}
		}
	}
	if message == "" {
		message = "log signal"
	}
	// Counts and ordinals vary per occurrence; a sample keeps the shape.
	return normalizeCounts(redactMessage(message))
}

// redactMessage applies the report's redaction and normalization to free
// text that is already known to be the message, such as the first line of a
// runtime crash dump, which carries no slog fields.
func redactMessage(message string) string {
	message = accountPattern.ReplaceAllString(message, "[account]")
	message = sensitiveField.ReplaceAllString(message, "$1=[redacted]")
	message = symbolPhrase.ReplaceAllString(message, "for [symbol] via")
	message = addrPattern.ReplaceAllString(message, "[addr]")
	message = inlineTimestamp.ReplaceAllString(message, "[time]")
	message = redactOrderRefs(message)
	message = durationPattern.ReplaceAllString(message, "[duration]")
	message = expiryPattern.ReplaceAllString(message, "[expiry]")
	message = clockPattern.ReplaceAllString(message, "[clock]")
	message = ratePattern.ReplaceAllString(message, "[rate]")
	message = pidPattern.ReplaceAllString(message, "pid N")
	message = counterPattern.ReplaceAllStringFunc(message, func(field string) string {
		key, _, ok := strings.Cut(field, "=")
		if !ok {
			return "(cached N so far)"
		}
		if strings.HasSuffix(key, "code") || key == "status" {
			return field
		}
		return key + "=N"
	})
	message = held.mask(message)
	message = spacePattern.ReplaceAllString(strings.TrimSpace(message), " ")
	if len(message) > 240 {
		message = message[:240] + "…"
	}
	return message
}

// redactOrderRefs replaces the number of every broker order reference with
// [ref], keeping the words around it, except an ordinal such as "order 2 of
// 3". An ordinal has at most two digits: "order 48213 of 100 shares" names a
// broker order.
func redactOrderRefs(message string) string {
	var out strings.Builder
	last := 0
	for _, m := range orderRefPattern.FindAllStringSubmatchIndex(message, -1) {
		if m[3]-m[2] <= 2 && ordinalTail.MatchString(message[m[1]:]) {
			continue
		}
		out.WriteString(message[last:m[2]])
		out.WriteString("[ref]")
		last = m[3]
	}
	if last == 0 {
		return message
	}
	out.WriteString(message[last:])
	return out.String()
}

func extractSlogMessage(line string) string {
	_, after, ok := strings.Cut(line, "msg=")
	if !ok {
		return ""
	}
	value := after
	if value == "" {
		return ""
	}
	if value[0] != '"' {
		if before, _, ok := strings.Cut(value, " "); ok {
			return before
		}
		return value
	}
	for end := 1; end < len(value); end++ {
		if value[end] != '"' || value[end-1] == '\\' {
			continue
		}
		decoded, err := strconv.Unquote(value[:end+1])
		if err == nil {
			return decoded
		}
	}
	return ""
}

func httpStatus(line string) int {
	match := statusPattern.FindStringSubmatch(line)
	if len(match) != 2 {
		return 0
	}
	status, _ := strconv.Atoi(match[1])
	return status
}

func isRequestCompleted(line string) bool {
	return strings.Contains(line, "Request completed")
}

// isDaemonLifecycle covers the process start marker, which names the build,
// and the broker-session markers. Restart-loop detection keys on the session
// marker alone (isDaemonStart), so a start is not counted twice.
func isDaemonLifecycle(line string) bool {
	return isDaemonStart(line) || strings.Contains(line, "canary daemon serving") || strings.Contains(line, "IBKR connector stopped")
}

var daemonBuildPattern = regexp.MustCompile(`canary daemon serving \S+ \(build ([^)\s]+)\)`)

// daemonBuild returns the build a daemon start marker names, or "".
func daemonBuild(line string) string {
	if match := daemonBuildPattern.FindStringSubmatch(line); len(match) == 2 {
		return match[1]
	}
	return ""
}

func isDaemonStart(line string) bool {
	return strings.Contains(line, "Connected to IB Gateway")
}

// isAppLifecycle covers a clean start/stop pair. "canary app stopped" is
// deliberately absent: the app logs it only when Run returns an error, so it
// must reach the severity switch and page rather than count as routine.
func isAppLifecycle(line string) bool {
	return isAppStart(line) || strings.Contains(line, "Shutting down server.")
}

func isAppStart(line string) bool {
	return strings.Contains(line, "canary app serving")
}

func isRateLimiterWarning(line string) bool {
	return strings.Contains(line, "RateLimiter") && severity(line) == "WARN" &&
		(strings.Contains(line, "rate limiter stopped") || strings.Contains(line, "RateLimiter draining"))
}

func isPanicLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.HasPrefix(lower, "panic:") || strings.Contains(lower, "level=error msg=panic") || strings.Contains(lower, `msg="panic`)
}

func isStackContinuation(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(line, "\t") || strings.HasPrefix(trimmed, "goroutine ") ||
		strings.HasPrefix(trimmed, "created by ") || strings.Contains(trimmed, ".go:")
}
