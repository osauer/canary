// Command log-monitor incrementally classifies Canary daemon and app logs.
// It emits bounded, redacted JSON so scheduled agents never ingest raw logs.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	reportVersion      = 1
	defaultMaxSignals  = 10
	maxScannerCapacity = 1024 * 1024

	// A scan window spans however long it has been since the last check, so
	// counting lifecycle markers across it reads a day of deliberate restarts
	// as a crash loop. Only starts this close together are evidence of one.
	restartLoopWindow = 10 * time.Minute
	restartLoopStarts = 4
)

type options struct {
	daemonLog    string
	appLog       string
	daemonOffset string
	appOffset    string
	maxSignals   int
	commit       bool
}

type report struct {
	Version        int       `json:"version"`
	GeneratedAt    time.Time `json:"generated_at"`
	NeedsAttention bool      `json:"needs_attention"`
	Daemon         logReport `json:"daemon"`
	App            logReport `json:"app"`
}

type logReport struct {
	State             string         `json:"state"`
	NewLines          int            `json:"new_lines"`
	KnownBenign       int            `json:"known_benign"`
	Informational     int            `json:"informational"`
	OffsetReset       bool           `json:"offset_reset,omitempty"`
	Families          map[string]int `json:"families,omitempty"`
	Signals           []signal       `json:"signals,omitempty"`
	SuppressedSignals int            `json:"suppressed_signals,omitempty"`
}

type signal struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Count    int    `json:"count,omitempty"`
}

type scannedLog struct {
	lines       []string
	total       int
	state       string
	offsetReset bool
}

var (
	accountPattern = regexp.MustCompile(`\b(?:DU|U)\d{5,}\b`)
	sensitiveField = regexp.MustCompile(`(?i)\b(account(?:_id)?|order(?:_id|_ref)?|preview_token|token|balance|holding|position|symbol|conid|reqid)=("[^"]*"|\S+)`)
	symbolPhrase   = regexp.MustCompile(`(?i)\bfor [A-Z][A-Z0-9.]{0,9} via\b`)
	spacePattern   = regexp.MustCompile(`\s+`)
	statusPattern  = regexp.MustCompile(`(?:^|\s)status=(\d{3})(?:\s|$)`)
	timePattern    = regexp.MustCompile(`(?:^|\s)time=([^\s]+)`)
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "log-monitor: resolve home: %v\n", err)
		os.Exit(2)
	}
	opts := options{}
	flag.StringVar(&opts.daemonLog, "daemon-log", filepath.Join(home, ".local", "state", "ibkr", "ibkr-daemon.log"), "daemon log path")
	flag.StringVar(&opts.appLog, "app-log", filepath.Join(home, ".local", "state", "ibkr", "ibkr-app.log"), "app log path")
	flag.StringVar(&opts.daemonOffset, "daemon-offset", filepath.Join(home, ".claude", "scheduled-tasks", "ibkr-daemon-log-check.offset"), "daemon line-offset path")
	flag.StringVar(&opts.appOffset, "app-offset", filepath.Join(home, ".claude", "scheduled-tasks", "ibkr-daemon-log-check.app.offset"), "app line-offset path")
	flag.IntVar(&opts.maxSignals, "max-signals", defaultMaxSignals, "maximum signal samples per log")
	flag.BoolVar(&opts.commit, "commit", true, "persist offsets after a successful scan")
	flag.Parse()

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

	result := report{
		Version:     reportVersion,
		GeneratedAt: now,
		Daemon:      classifyDaemon(daemon, opts.maxSignals),
		App:         classifyApp(app, opts.maxSignals),
	}
	result.NeedsAttention = len(result.Daemon.Signals) != 0 || len(result.App.Signals) != 0 ||
		result.Daemon.SuppressedSignals != 0 || result.App.SuppressedSignals != 0

	if opts.commit {
		if daemon.state != "missing" {
			if err := writeOffset(opts.daemonOffset, daemon.total); err != nil {
				return report{}, fmt.Errorf("write daemon offset: %w", err)
			}
		}
		if app.state != "missing" {
			if err := writeOffset(opts.appOffset, app.total); err != nil {
				return report{}, fmt.Errorf("write app offset: %w", err)
			}
		}
	}
	return result, nil
}

func scanIncremental(logPath, offsetPath string) (scannedLog, error) {
	offset, err := readOffset(offsetPath)
	if err != nil {
		return scannedLog{}, err
	}
	f, err := os.Open(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return scannedLog{state: "missing"}, nil
	}
	if err != nil {
		return scannedLog{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxScannerCapacity)
	var all []string
	for scanner.Scan() {
		all = append(all, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return scannedLog{}, err
	}
	reset := offset > len(all)
	if reset {
		offset = 0
	}
	state := "unchanged"
	if len(all) > offset {
		state = "scanned"
	}
	return scannedLog{
		lines:       append([]string(nil), all[offset:]...),
		total:       len(all),
		state:       state,
		offsetReset: reset,
	}, nil
}

func readOffset(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(value)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid offset %q", value)
	}
	return offset, nil
}

func writeOffset(path string, value int) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := fmt.Fprintf(tmp, "%d\n", value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
			addSignal(&result, maxSignals, "ERROR", "panic", "daemon panic detected", 0)
		case isStackContinuation(line):
			result.Informational++
		case isDaemonLifecycle(trimmed):
			result.Families["lifecycle"]++
			result.Informational++
		case daemonBenignFamily(trimmed) != "":
			family := daemonBenignFamily(trimmed)
			result.Families[family]++
			result.KnownBenign++
		case strings.Contains(trimmed, "code=2108"):
			result.Families["market_data_farm_disconnect"]++
			result.KnownBenign++
		case isRateLimiterWarning(trimmed) && nearRestart(trimmed, restarts):
			result.Families["shutdown_rate_limiter"]++
			result.KnownBenign++
		case severity(trimmed) == "WARN" || severity(trimmed) == "ERROR":
			addSignal(&result, maxSignals, severity(trimmed), "log_level", safeMessage(trimmed), 0)
		default:
			result.Informational++
		}
	}
	if count := result.Families["market_data_farm_disconnect"]; count > 2 {
		addSignal(&result, maxSignals, "WARN", "repeated_farm_disconnect", "market-data farm disconnected repeatedly", count)
	}
	if count := restartLoopCount(lifecycleTimes(scanned.lines, isDaemonStart)); count > restartLoopStarts {
		addSignal(&result, maxSignals, "WARN", "restart_loop", "daemon connected repeatedly within "+restartLoopWindow.String(), count)
	}
	for family, count := range result.Families {
		if family != "lifecycle" && family != "market_data_farm_disconnect" && count > 150 {
			addSignal(&result, maxSignals, "WARN", "noise_loop", "known-benign log family exceeded its daily volume limit: "+family, count)
		}
	}
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
			addSignal(&result, maxSignals, "ERROR", "panic", "app panic detected", 0)
			continue
		}
		if panicActive && isStackContinuation(line) {
			result.Informational++
			continue
		}
		panicActive = false

		level := severity(trimmed)
		if isRequestCompleted(trimmed) {
			status := httpStatus(trimmed)
			if status >= 500 {
				addSignal(&result, maxSignals, "ERROR", "http_5xx", fmt.Sprintf("app request completed with status %d", status), 0)
			} else if status >= 100 {
				result.Families["request_completed"]++
				result.KnownBenign++
			} else {
				addSignal(&result, maxSignals, "WARN", "malformed_access_log", "app access log omitted a valid HTTP status", 0)
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
			addSignal(&result, maxSignals, level, "log_level", safeMessage(trimmed), 0)
		case "DEBUG", "INFO":
			result.Informational++
		default:
			addSignal(&result, maxSignals, "WARN", "missing_level", "production app line omitted an explicit severity", 0)
		}
	}
	if count := restartLoopCount(lifecycleTimes(scanned.lines, isAppStart)); count > restartLoopStarts {
		addSignal(&result, maxSignals, "WARN", "restart_loop", "app started repeatedly within "+restartLoopWindow.String(), count)
	}
	return result
}

func newLogReport(scanned scannedLog) logReport {
	return logReport{
		State:       scanned.state,
		NewLines:    len(scanned.lines),
		OffsetReset: scanned.offsetReset,
		Families:    map[string]int{},
	}
}

func addSignal(result *logReport, max int, severity, kind, message string, count int) {
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
	item := signal{Severity: severity, Kind: kind, Message: message, Count: count}
	if len(result.Signals) < max {
		result.Signals = append(result.Signals, item)
		return
	}
	result.SuppressedSignals++
}

func daemonBenignFamily(line string) string {
	switch {
	case strings.Contains(line, "code=354"):
		return "broker_code_354"
	case strings.Contains(line, "code=300"):
		return "broker_code_300"
	case strings.Contains(line, "code=2129") && strings.Contains(line, "HGENQ"):
		return "broker_code_2129_hgenq"
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
	message = accountPattern.ReplaceAllString(message, "[account]")
	message = sensitiveField.ReplaceAllString(message, "$1=[redacted]")
	message = symbolPhrase.ReplaceAllString(message, "for [symbol] via")
	message = spacePattern.ReplaceAllString(strings.TrimSpace(message), " ")
	if len(message) > 240 {
		message = message[:240] + "…"
	}
	return message
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

func isDaemonLifecycle(line string) bool {
	return isDaemonStart(line) || strings.Contains(line, "IBKR connector stopped")
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
	return strings.Contains(line, "RateLimiter") && severity(line) == "WARN"
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
