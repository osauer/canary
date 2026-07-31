package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseGoTestSummaryReportsCasesSkipsAndElapsed(t *testing.T) {
	t.Parallel()
	raw := strings.Join([]string{
		`{"Action":"run","Package":"example.test","Test":"TestPass"}`,
		`{"Action":"pass","Package":"example.test","Test":"TestPass","Elapsed":0.01}`,
		`{"Action":"skip","Package":"example.test","Test":"TestSkipped","Elapsed":0,"Output":"--- SKIP: TestSkipped (0.00s)\n"}`,
		`{"Action":"skip","Package":"example.test","Test":"TestSkipped","Elapsed":0}`,
		`{"Action":"fail","Package":"example.test","Test":"TestFailed","Elapsed":0.02,"Output":"--- FAIL: TestFailed (0.02s)\n"}`,
		`{"Action":"fail","Package":"example.test","Elapsed":1.25}`,
		"",
	}, "\n")
	got, err := parseGoTestSummary([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got.passed != 1 || got.failed != 1 || got.skipped != 2 {
		t.Fatalf("summary = passed %d failed %d skipped %d", got.passed, got.failed, got.skipped)
	}
	if len(got.skippedNames) != 1 || got.skippedNames[0] != "TestSkipped" {
		t.Fatalf("skipped names = %v", got.skippedNames)
	}
	if got.packageElapsed != 1250*time.Millisecond {
		t.Fatalf("package elapsed = %s, want 1.25s", got.packageElapsed)
	}
	if !strings.Contains(got.output, "TestSkipped") || !strings.Contains(got.output, "TestFailed") {
		t.Fatalf("rendered failure output missing details: %q", got.output)
	}
	if got.telemetry() != "cases_passed=1 cases_failed=1 cases_skipped=2 package_elapsed=1.25s" {
		t.Fatalf("telemetry = %q", got.telemetry())
	}
}

func TestParseGoTestSummaryFailsClosedOnMalformedJSON(t *testing.T) {
	t.Parallel()
	if _, err := parseGoTestSummary([]byte(`not-json`)); err == nil {
		t.Fatal("malformed go test JSON was accepted")
	}
}
