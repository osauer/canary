package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"time"
)

func defaultAppLog(home, goos string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Logs", "ibkr", "app.err.log")
	}
	return filepath.Join(home, ".local", "state", "ibkr", "ibkr-app.log")
}

func applyCoverage(result *logReport, scanned scannedLog, now time.Time, staleAfter time.Duration) {
	if scanned.state == "missing" {
		addSignal(result, "WARN", "log_coverage", "expected log is missing; service health is unverified", 0)
	}
	if scanned.coverage != "" {
		addSignal(result, "WARN", "log_coverage", scanned.coverage, 0)
	}
	if scanned.state != "missing" && staleAfter > 0 && now.Sub(scanned.modified) > staleAfter {
		addSignal(result, "WARN", "log_coverage", "log is inactive beyond the configured coverage interval; service health is unverified", 0)
	}
}

type familyTrend struct {
	Family      string `json:"family"`
	Days        int    `json:"days"`
	Occurrences int    `json:"occurrences"`
	FirstDay    string `json:"first_day"`
	LastDay     string `json:"last_day"`
}

// Retain 28 UTC date buckets of closed-family counts, not raw log content.
// These are recurrence evidence, never a claim that a broker failure recovered.
func trackFamilies(result *logReport, scanned *scannedLog, now time.Time) {
	if scanned.cursor.Days == nil {
		scanned.cursor.Days = map[string]map[string]int{}
	}
	cutoff := now.UTC().AddDate(0, 0, -27).Format("2006-01-02")
	today := now.UTC().Format("2006-01-02")
	changed := map[string]bool{}
	starts := scanned.cursor.Starts
	for _, line := range scanned.lines {
		ts, ok := logTime(line)
		if !ok || ts.After(now) {
			continue
		}
		if (isDaemonStart(line) || isAppStart(line)) && ts.After(now.Add(-restartLoopWindow)) {
			starts = append(starts, ts)
		}
		family := daemonNoticeFamily(line)
		day := ts.UTC().Format("2006-01-02")
		if family == "" || day < cutoff {
			continue
		}
		if scanned.cursor.Days[day] == nil {
			scanned.cursor.Days[day] = map[string]int{}
		}
		scanned.cursor.Days[day][family]++
		changed[day+"\x00"+family] = true
	}
	trends := map[string]familyTrend{}
	for day, counts := range scanned.cursor.Days {
		if day < cutoff || day > today {
			delete(scanned.cursor.Days, day)
			continue
		}
		for family, count := range counts {
			trend := trends[family]
			trend.Family = family
			trend.Days++
			trend.Occurrences += count
			if trend.FirstDay == "" || day < trend.FirstDay {
				trend.FirstDay = day
			}
			if day > trend.LastDay {
				trend.LastDay = day
			}
			trends[family] = trend
			if family == "broker_code_2129_indicative" && changed[day+"\x00"+family] && count > 150 {
				addSignal(result, "WARN", "noise_loop", fmt.Sprintf("indicative-data notices exceeded 150 occurrences on %s", day), count)
			}
		}
	}
	for _, trend := range trends {
		if trend.Days > 1 {
			result.Recurring = append(result.Recurring, trend)
		}
	}
	slices.SortFunc(result.Recurring, func(a, b familyTrend) int {
		if a.Family < b.Family {
			return -1
		}
		if a.Family > b.Family {
			return 1
		}
		return 0
	})
	starts = slices.DeleteFunc(starts, func(ts time.Time) bool { return ts.Before(now.Add(-restartLoopWindow)) || ts.After(now) })
	slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
	if len(scanned.lines) > 0 && restartLoopCount(starts) > restartLoopStarts {
		found := false
		for _, s := range result.Signals {
			if s.Kind == "restart_loop" {
				found = true
			}
		}
		if !found {
			addSignal(result, "WARN", "restart_loop", "service started repeatedly across scan windows within "+restartLoopWindow.String(), len(starts))
		}
	}
	if len(starts) > 128 {
		starts = starts[len(starts)-128:]
	}
	scanned.cursor.Starts = starts
}
