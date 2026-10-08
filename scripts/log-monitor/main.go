// Command log-monitor incrementally classifies Canary daemon and app logs.
// It emits bounded, redacted JSON so scheduled agents never ingest raw logs.
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
	reportVersion      = 3
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
	daemonMirror      string
	holdingsStore     string
	holdingsCache     string
	maxSignals        int
	commit            bool
	staleAfter        time.Duration
	mirrorStaleAfter  time.Duration
}

type report struct {
	Version        int       `json:"version"`
	GeneratedAt    time.Time `json:"generated_at"`
	NeedsAttention bool      `json:"needs_attention"`
	// Holdings says where the current-holdings list that masks names came
	// from: store, cache, or unavailable (contract names withheld).
	Holdings string    `json:"holdings"`
	Daemon   logReport `json:"daemon"`
	App      logReport `json:"app"`
	// CrashLog names the daemon's crash output file when it exists; new
	// content there is one ERROR signal on the daemon report.
	CrashLog string `json:"crash_log,omitempty"`
}

type logReport struct {
	Path          string         `json:"path"`
	ModifiedAt    time.Time      `json:"modified_at,omitzero"`
	Recurring     []familyTrend  `json:"recurring,omitempty"`
	State         string         `json:"state"`
	NewLines      int            `json:"new_lines"`
	KnownBenign   int            `json:"known_benign"`
	Informational int            `json:"informational"`
	OffsetReset   bool           `json:"offset_reset,omitempty"`
	Families      map[string]int `json:"families,omitempty"`
	// Builds lists, in order, the daemon builds whose start marker falls in
	// the window, so a line can be told apart from a build already replaced.
	Builds []string `json:"builds,omitempty"`
	// Signals holds the max-signals highest-ranked distinct signals —
	// severity first, then occurrence count — never arrival order, so an
	// escalating loop outranks a one-off stale quote no matter which the
	// scan met first.
	Signals []signal `json:"signals,omitempty"`
	// Suppressed summarizes every signal the cap cut, one row per
	// severity/kind pair, so nothing material drops without a trace. The
	// row count is bounded by the closed kind set.
	Suppressed        []suppressedSummary `json:"suppressed,omitempty"`
	SuppressedSignals int                 `json:"suppressed_signals,omitempty"`
	// Mirror is present when the input is a copy kept by a log mirror.
	Mirror *mirrorReport `json:"mirror,omitempty"`
}

type signal struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Count    int    `json:"count,omitempty"`
}

type suppressedSummary struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	// Count is every occurrence the cap cut for the pair, across all of its
	// distinct messages; it is not how often Sample occurred.
	Count int `json:"count"`
	// Sample is the highest-ranked suppressed message of the pair, already
	// redacted by safeMessage.
	Sample string `json:"sample,omitempty"`
	// SampleCount is how often Sample itself occurred, and Distinct how many
	// distinct messages the row holds, so the row never reads as Count
	// repeats of its sample.
	SampleCount int `json:"sample_count"`
	Distinct    int `json:"distinct"`
}

type scannedLog struct {
	lines       []string
	total       int
	state       string
	offsetReset bool
	path        string
	modified    time.Time
	coverage    string
	cursor      logCursor
}

var (
	accountPattern = regexp.MustCompile(`\b(?:DU|U)\d{5,}\b`)
	sensitiveField = regexp.MustCompile(`(?i)\b(account(?:_id)?|order(?:_id|_?ref)?|preview_token|token|balance|holding|position|symbol|conid|reqid)=("[^"]*"|\S+)`)
	symbolPhrase   = regexp.MustCompile(`(?i)\bfor [A-Z][A-Z0-9.]{0,9} via\b`)
	spacePattern   = regexp.MustCompile(`\s+`)
	statusPattern  = regexp.MustCompile(`(?:^|\s)status=(\d{3})(?:\s|$)`)
	timePattern    = regexp.MustCompile(`(?:^|\s)time=([^\s]+)`)
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
	opts := options{}
	flag.StringVar(&opts.daemonLog, "daemon-log", filepath.Join(home, ".local", "state", "ibkr", "ibkr-daemon.log"), "daemon log path")
	flag.StringVar(&opts.appLog, "app-log", defaultAppLog(home, runtime.GOOS), "app log path (macOS launchd stderr by default)")
	flag.StringVar(&opts.daemonCrashLog, "daemon-crash-log", "", "daemon crash output path (default: the daemon log's name with a .crash.log suffix, beside it)")
	flag.StringVar(&opts.daemonCrashOffset, "daemon-crash-offset", "", "crash output private cursor path (default: -daemon-offset with a .crash suffix)")
	flag.StringVar(&opts.daemonOffset, "daemon-offset", filepath.Join(home, ".claude", "scheduled-tasks", "ibkr-daemon-log-check.offset"), "daemon private cursor path (legacy line offsets are migrated)")
	flag.StringVar(&opts.appOffset, "app-offset", filepath.Join(home, ".claude", "scheduled-tasks", "ibkr-daemon-log-check.app.offset"), "app private cursor path (legacy line offsets are migrated)")
	flag.IntVar(&opts.maxSignals, "max-signals", defaultMaxSignals, "maximum signal samples per log")
	flag.BoolVar(&opts.commit, "commit", true, "persist offsets after a successful scan")
	flag.DurationVar(&opts.staleAfter, "stale-after", 24*time.Hour, "flag log coverage unverified after this inactivity; zero disables")
	flag.StringVar(&opts.daemonMirror, "daemon-mirror-status", "", "sync record of the mirror that copies -daemon-log from another machine; empty for a local log")
	flag.DurationVar(&opts.mirrorStaleAfter, "mirror-stale-after", 20*time.Minute, "flag mirrored log coverage unverified when the last completed sync is older; zero disables")
	flag.StringVar(&opts.holdingsStore, "holdings-store", filepath.Join(xdgDir("XDG_STATE_HOME", home, ".local", "state"), "ibkr", "daemon.db"), "daemon store read (read-only) for the current holdings a report must not name")
	flag.StringVar(&opts.holdingsCache, "holdings-cache", filepath.Join(xdgDir("XDG_CACHE_HOME", home, ".cache"), "ibkr", "holdings-denylist"), "the account-data gate's private holdings cache, used when the store cannot be read")
	flag.Parse()
	held = loadHoldings(opts.holdingsStore, opts.holdingsCache)

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

	result := report{
		Version:     reportVersion,
		GeneratedAt: now,
		Holdings:    held.source,
		Daemon:      classifyDaemon(daemon, int(^uint(0)>>1)),
		App:         classifyApp(app, int(^uint(0)>>1)),
	}
	if crash.state != "missing" {
		result.CrashLog = crash.path
	}
	// The crash log is the runtime's own output — a goroutine dump, a panic —
	// so its content is one finding, not a line-by-line classification; the
	// first line names the cause ("SIGQUIT: quit", "panic: …", "fatal error:
	// …") and the count says how much landed.
	if crash.state == "scanned" {
		first, lines := "", 0
		for _, line := range crash.lines {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				lines++
				if first == "" {
					first = redactMessage(trimmed)
				}
			}
		}
		if lines > 0 {
			addSignal(&result.Daemon, "ERROR", "crash_output", "daemon crash output written: "+first, lines)
		}
	}
	applyCoverage(&result.Daemon, daemon, now, opts.staleAfter)
	applyCoverage(&result.App, app, now, opts.staleAfter)
	if opts.daemonMirror != "" {
		applyMirrorCoverage(&result.Daemon, opts.daemonMirror, now, opts.mirrorStaleAfter)
	}
	trackFamilies(&result.Daemon, &daemon, now)
	trackFamilies(&result.App, &app, now)
	finalizeSignals(&result.Daemon, opts.maxSignals)
	finalizeSignals(&result.App, opts.maxSignals)
	result.NeedsAttention = len(result.Daemon.Signals) != 0 || len(result.App.Signals) != 0 ||
		result.Daemon.SuppressedSignals != 0 || result.App.SuppressedSignals != 0

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
	}
	return result, nil
}

// crashPaths resolves the crash log and its cursor, deriving them from the
// daemon log and cursor when not given: the daemon writes
// <daemon-log-name>.crash.log beside its log.
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

func classifyDaemon(scanned scannedLog, maxSignals int) logReport {
	result := newLogReport(scanned)
	if len(scanned.lines) == 0 {
		return result
	}
	restarts := lifecycleTimes(scanned.lines, isDaemonLifecycle)
	for _, line := range scanned.lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			result.Informational++
		case isPanicLine(trimmed):
			addSignal(&result, "ERROR", "panic", "daemon panic detected", 0)
		case severity(trimmed) == "FATAL":
			addSignal(&result, "FATAL", "log_level", safeMessage(trimmed), 0)
		case isStackContinuation(line):
			result.Informational++
		case isDaemonLifecycle(trimmed):
			result.Families["lifecycle"]++
			result.Informational++
			if build := daemonBuild(trimmed); build != "" && !slices.Contains(result.Builds, build) {
				result.Builds = append(result.Builds, build)
			}
		case daemonNoticeFamily(trimmed) != "":
			family := daemonNoticeFamily(trimmed)
			result.Families[family]++
			if level := severity(trimmed); level != "WARN" && level != "ERROR" {
				// The daemon already judged this notice routine (a known
				// entitlement gap, a verdict its requester records).
				result.Informational++
			} else if family == "broker_code_2129_indicative" && severity(trimmed) != "ERROR" {
				result.KnownBenign++
			} else {
				level := "WARN"
				if severity(trimmed) == "ERROR" {
					level = "ERROR"
				}
				message := "broker read notice requires outcome assessment: " + family
				if family == "broker_code_200_no_definition" {
					// A definition failure is about one contract; name it so
					// a recurring miss can be traced without the raw log.
					message += " (" + noticeContractLabel(trimmed) + ")"
				}
				addSignal(&result, level, "broker_read_notice", message, 0)
			}
		case strings.Contains(trimmed, "code=2108"):
			result.Families["market_data_farm_disconnect"]++
			result.KnownBenign++
		case isRateLimiterWarning(trimmed) && nearRestart(trimmed, restarts):
			result.Families["shutdown_rate_limiter"]++
			result.KnownBenign++
		case severity(trimmed) == "WARN" || severity(trimmed) == "ERROR":
			addSignal(&result, severity(trimmed), "log_level", safeMessage(trimmed), 0)
		default:
			result.Informational++
		}
	}
	if count := result.Families["market_data_farm_disconnect"]; count > 2 {
		addSignal(&result, "WARN", "repeated_farm_disconnect", "market-data farm disconnected repeatedly", count)
	}
	if count := restartLoopCount(lifecycleTimes(scanned.lines, isDaemonStart)); count > restartLoopStarts {
		addSignal(&result, "WARN", "restart_loop", "daemon connected repeatedly within "+restartLoopWindow.String(), count)
	}
	finalizeSignals(&result, maxSignals)
	return result
}

func classifyApp(scanned scannedLog, maxSignals int) logReport {
	result := newLogReport(scanned)
	panicActive := false
	for _, line := range scanned.lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			result.Informational++
			continue
		}
		if isPanicLine(trimmed) {
			panicActive = true
			addSignal(&result, "ERROR", "panic", "app panic detected", 0)
			continue
		}
		if panicActive && isStackContinuation(line) {
			result.Informational++
			continue
		}
		panicActive = false

		level := severity(trimmed)
		if level == "FATAL" {
			addSignal(&result, level, "log_level", safeMessage(trimmed), 0)
			continue
		}
		if isRequestCompleted(trimmed) {
			status := httpStatus(trimmed)
			if status >= 500 {
				addSignal(&result, "ERROR", "http_5xx", fmt.Sprintf("app request completed with status %d", status), 0)
			} else if status >= 100 {
				result.Families["request_completed"]++
				result.KnownBenign++
			} else {
				addSignal(&result, "WARN", "malformed_access_log", "app access log omitted a valid HTTP status", 0)
			}
			continue
		}
		if isAppLifecycle(trimmed) {
			result.Families["lifecycle"]++
			result.Informational++
			continue
		}
		switch level {
		case "WARN", "ERROR", "FATAL":
			addSignal(&result, level, "log_level", safeMessage(trimmed), 0)
		case "DEBUG", "INFO":
			result.Informational++
		default:
			addSignal(&result, "WARN", "missing_level", "production app line omitted an explicit severity", 0)
		}
	}
	if count := restartLoopCount(lifecycleTimes(scanned.lines, isAppStart)); count > restartLoopStarts {
		addSignal(&result, "WARN", "restart_loop", "app started repeatedly within "+restartLoopWindow.String(), count)
	}
	finalizeSignals(&result, maxSignals)
	return result
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

// addSignal collects without a cap, merging repeats of the same
// severity/kind/message into one counted signal. The max-signals bound is
// applied by finalizeSignals after classification, once every signal's true
// weight is known.
func addSignal(result *logReport, severity, kind, message string, count int) {
	for i := range result.Signals {
		item := &result.Signals[i]
		if item.Severity != severity || item.Kind != kind || item.Message != message {
			continue
		}
		if item.Count == 0 {
			item.Count = 2
		} else {
			item.Count++
		}
		return
	}
	result.Signals = append(result.Signals, signal{Severity: severity, Kind: kind, Message: message, Count: count})
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

// effectiveCount reads a signal's occurrence weight; Count 0 means the signal
// fired once.
func effectiveCount(s signal) int {
	return max(s.Count, 1)
}

// finalizeSignals ranks collected signals by severity then occurrence count
// (stable on arrival order for ties), keeps the top max, and rolls everything
// cut into the bounded per-severity/kind suppressed summary.
func finalizeSignals(result *logReport, max int) {
	slices.SortStableFunc(result.Signals, func(a, b signal) int {
		if d := severityRank(b.Severity) - severityRank(a.Severity); d != 0 {
			return d
		}
		return effectiveCount(b) - effectiveCount(a)
	})
	if len(result.Signals) <= max {
		return
	}
	dropped := result.Signals[max:]
	result.Signals = result.Signals[:max:max]
	index := map[string]int{}
	for _, s := range dropped {
		key := s.Severity + "\x00" + s.Kind
		i, ok := index[key]
		if !ok {
			i = len(result.Suppressed)
			index[key] = i
			result.Suppressed = append(result.Suppressed, suppressedSummary{
				Severity:    s.Severity,
				Kind:        s.Kind,
				Sample:      s.Message,
				SampleCount: effectiveCount(s),
			})
		}
		// addSignal merged repeats, so each dropped signal of a pair is one
		// distinct message.
		result.Suppressed[i].Distinct++
		result.Suppressed[i].Count += effectiveCount(s)
		result.SuppressedSignals += effectiveCount(s)
	}
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
	return redactMessage(message)
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
