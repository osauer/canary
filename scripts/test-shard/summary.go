package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type goTestEvent struct {
	Action  string  `json:"Action"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

type goTestSummary struct {
	passed         int
	failed         int
	skipped        int
	skippedNames   []string
	packageElapsed time.Duration
	output         string
}

func parseGoTestSummary(raw []byte) (goTestSummary, error) {
	var summary goTestSummary
	var rendered strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event goTestEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return goTestSummary{}, fmt.Errorf("decode go test -json event: %w", err)
		}
		rendered.WriteString(event.Output)
		if event.Test != "" {
			switch event.Action {
			case "pass":
				summary.passed++
			case "fail":
				summary.failed++
			case "skip":
				summary.skipped++
				summary.skippedNames = append(summary.skippedNames, event.Test)
			}
			continue
		}
		if (event.Action == "pass" || event.Action == "fail") && event.Elapsed > 0 {
			summary.packageElapsed = time.Duration(event.Elapsed * float64(time.Second))
		}
	}
	if err := scanner.Err(); err != nil {
		return goTestSummary{}, fmt.Errorf("read go test -json output: %w", err)
	}
	sort.Strings(summary.skippedNames)
	summary.skippedNames = dedupe(summary.skippedNames)
	summary.output = rendered.String()
	return summary, nil
}

func dedupe(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func (s goTestSummary) telemetry() string {
	return fmt.Sprintf(
		"cases_passed=%d cases_failed=%d cases_skipped=%d package_elapsed=%s",
		s.passed,
		s.failed,
		s.skipped,
		s.packageElapsed.Round(time.Millisecond),
	)
}
