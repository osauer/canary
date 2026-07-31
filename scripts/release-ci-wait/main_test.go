package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testWorkflowID int64 = 17
	testRunID      int64 = 4101
)

var testSHA = strings.Repeat("a", 40)

func TestParseConfigSupportsRepeatedAndCommaSeparatedJobs(t *testing.T) {
	t.Parallel()
	cfg, err := parseConfig([]string{
		"-workflow", "pages-check.yml",
		"-workflow-name", "pages check",
		"-sha", testSHA,
		"-branch", "release/exact-sha",
		"-event", "workflow_dispatch",
		"-job", "pages / links, pages / metadata",
		"-job", "pages / local browser",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	wantJobs := []string{"pages / links", "pages / metadata", "pages / local browser"}
	if !reflect.DeepEqual(cfg.requiredJobs, wantJobs) {
		t.Fatalf("requiredJobs = %v, want %v", cfg.requiredJobs, wantJobs)
	}
	if cfg.repository != "osauer/canary" ||
		cfg.workflow != "pages-check.yml" ||
		cfg.workflowName != "pages check" ||
		cfg.branch != "release/exact-sha" ||
		cfg.event != "workflow_dispatch" {
		t.Fatalf("parsed config = %+v", cfg)
	}
	if cfg.poll != 15*time.Second || cfg.timeout != 30*time.Minute {
		t.Fatalf("defaults poll=%s timeout=%s", cfg.poll, cfg.timeout)
	}
}

func TestParseConfigFailsClosed(t *testing.T) {
	t.Parallel()
	base := []string{
		"-workflow-name", "ci",
		"-sha", testSHA,
		"-job", "linux",
	}
	cases := map[string][]string{
		"missing workflow name": {"-sha", testSHA, "-job", "linux"},
		"short sha":             {"-workflow-name", "ci", "-sha", "abc", "-job", "linux"},
		"uppercase sha": {
			"-workflow-name", "ci",
			"-sha", strings.Repeat("A", 40),
			"-job", "linux",
		},
		"duplicate job": append(append([]string(nil), base...), "-job", "linux"),
		"bad workflow path": {
			"-workflow", ".github/workflows/ci.yml",
			"-workflow-name", "ci",
			"-sha", testSHA,
			"-job", "linux",
		},
		"zero poll":    append(append([]string(nil), base...), "-poll", "0s"),
		"zero timeout": append(append([]string(nil), base...), "-timeout", "0s"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseConfig(args, io.Discard); err == nil {
				t.Fatalf("parseConfig(%v) succeeded, want error", args)
			}
		})
	}
}

func TestGHAPIArgsAreReadOnlyAndDoNotUseShellSplitting(t *testing.T) {
	t.Parallel()
	query := url.Values{
		"head_sha": {testSHA},
		"branch":   {"release/name with spaces"},
		"event":    {"push"},
	}
	got := ghAPIArgs("repos/osauer/canary/actions/workflows/17/runs", query)
	want := []string{
		"api",
		"--method", "GET",
		"--hostname", "github.com",
		"-H", "Accept: application/vnd.github+json",
		"-H", "X-GitHub-Api-Version: 2022-11-28",
		"repos/osauer/canary/actions/workflows/17/runs",
		"-f", "branch=release/name with spaces",
		"-f", "event=push",
		"-f", "head_sha=" + testSHA,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gh args = %#v, want %#v", got, want)
	}
}

func TestContractRunEvidenceIsSortedAndLogSafe(t *testing.T) {
	t.Parallel()
	workflows := map[string]waitResult{
		"pages-check.yml": {
			RunID:      4102,
			RunAttempt: 1,
		},
		"untrusted\nworkflow\x1b[2J.yml": {
			RunID:      4103,
			RunAttempt: 3,
		},
		"ci.yml": {
			RunID:      4101,
			RunAttempt: 2,
		},
	}

	got := contractRunEvidence(workflows)
	want := `[workflow="ci.yml",run_id=4101,attempt=2; ` +
		`workflow="pages-check.yml",run_id=4102,attempt=1; ` +
		`workflow="untrusted\nworkflow\x1b[2J.yml",run_id=4103,attempt=3]`
	if got != want {
		t.Fatalf("contract run evidence = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\n\r\x1b") {
		t.Fatalf("contract run evidence contains a raw control character: %q", got)
	}
}

func TestWaitForSuccess(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux", "macOS")
	run := validRun(cfg, 1, "completed", new("success"))
	jobs := []workflowJob{
		validJob(cfg, run, 1, 501, "linux", "completed", new("success")),
		validJob(cfg, run, 1, 502, "macOS", "completed", new("success")),
	}
	api := newStaticAPI(cfg, []workflowRecord{validWorkflow(cfg)}, []workflowRun{run}, jobs)
	timer := newFakeClock()

	result, err := waitForSuccess(context.Background(), cfg, api, timer, io.Discard)
	if err != nil {
		t.Fatalf("waitForSuccess: %v", err)
	}
	if result.WorkflowID != testWorkflowID || result.RunID != testRunID || result.RunAttempt != 1 {
		t.Fatalf("result = %+v", result)
	}
	wantAttempts := map[string]int{"linux": 1, "macOS": 1}
	if !reflect.DeepEqual(result.JobAttempt, wantAttempts) {
		t.Fatalf("job attempts = %v, want %v", result.JobAttempt, wantAttempts)
	}
	assertEndpointCalls(t, api.callsSnapshot(), workflowEndpoint(cfg), 2)
	assertEndpointCalls(t, api.callsSnapshot(), runEndpoint(cfg, testWorkflowID), 2)
	assertEndpointCalls(t, api.callsSnapshot(), jobEndpoint(cfg, testRunID), 1)
}

func TestMissingRunTimesOut(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	cfg.poll = time.Second
	cfg.timeout = 3 * time.Second
	api := newStaticAPI(cfg, []workflowRecord{validWorkflow(cfg)}, nil, nil)
	timer := newFakeClock()

	_, err := waitForSuccess(context.Background(), cfg, api, timer, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "timed out after 3s") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if got := timer.sleepCount(); got != 3 {
		t.Fatalf("sleep count = %d, want 3", got)
	}
}

func TestPendingRunThenSuccess(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	cfg.poll = time.Second
	cfg.timeout = 5 * time.Second
	queued := validRun(cfg, 1, "queued", nil)
	success := validRun(cfg, 1, "completed", new("success"))
	job := validJob(cfg, success, 1, 501, "linux", "completed", new("success"))
	var runCalls int
	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		switch endpoint {
		case workflowEndpoint(cfg):
			return pagePayload("workflows", []workflowRecord{validWorkflow(cfg)}), nil
		case runEndpoint(cfg, testWorkflowID):
			runCalls++
			if runCalls == 1 {
				return pagePayload("workflow_runs", []workflowRun{queued}), nil
			}
			return pagePayload("workflow_runs", []workflowRun{success}), nil
		case jobEndpoint(cfg, testRunID):
			return pagePayload("jobs", []workflowJob{job}), nil
		default:
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
	}
	timer := newFakeClock()

	result, err := waitForSuccess(context.Background(), cfg, api, timer, io.Discard)
	if err != nil {
		t.Fatalf("waitForSuccess: %v", err)
	}
	if result.RunAttempt != 1 || timer.sleepCount() != 1 {
		t.Fatalf("result=%+v sleeps=%d", result, timer.sleepCount())
	}
}

func TestPendingJobThenSuccess(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	cfg.poll = time.Second
	cfg.timeout = 5 * time.Second
	run := validRun(cfg, 1, "completed", new("success"))
	pending := validJob(cfg, run, 1, 501, "linux", "in_progress", nil)
	success := validJob(cfg, run, 1, 502, "linux", "completed", new("success"))
	var jobCalls int
	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		switch endpoint {
		case workflowEndpoint(cfg):
			return pagePayload("workflows", []workflowRecord{validWorkflow(cfg)}), nil
		case runEndpoint(cfg, testWorkflowID):
			return pagePayload("workflow_runs", []workflowRun{run}), nil
		case jobEndpoint(cfg, testRunID):
			jobCalls++
			if jobCalls == 1 {
				return pagePayload("jobs", []workflowJob{pending}), nil
			}
			return pagePayload("jobs", []workflowJob{success}), nil
		default:
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
	}
	timer := newFakeClock()

	if _, err := waitForSuccess(context.Background(), cfg, api, timer, io.Discard); err != nil {
		t.Fatalf("waitForSuccess: %v", err)
	}
	if timer.sleepCount() != 1 {
		t.Fatalf("sleep count = %d, want 1", timer.sleepCount())
	}
}

func TestExactJobSetRejectsMissingAndExtraNames(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux", "macOS")
	run := validRun(cfg, 1, "completed", new("success"))
	cases := map[string][]workflowJob{
		"missing": {
			validJob(cfg, run, 1, 501, "linux", "completed", new("success")),
		},
		"extra": {
			validJob(cfg, run, 1, 501, "linux", "completed", new("success")),
			validJob(cfg, run, 1, 502, "macOS", "completed", new("success")),
			validJob(cfg, run, 1, 503, "renamed", "completed", new("success")),
		},
	}
	for name, jobs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newStaticAPI(cfg, []workflowRecord{validWorkflow(cfg)}, []workflowRun{run}, jobs)
			_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
			if err == nil || !strings.Contains(err.Error(), "exact workflow job set mismatch") {
				t.Fatalf("error = %v, want exact-set mismatch", err)
			}
		})
	}
}

func TestExternalJobNamesAndCommandErrorsAreLogSafe(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	run := validRun(cfg, 1, "completed", new("success"))
	jobs := []workflowJob{
		validJob(cfg, run, 1, 501, "linux", "completed", new("success")),
		validJob(cfg, run, 1, 502, "untrusted\njob\x1b[2J", "completed", new("success")),
	}
	api := newStaticAPI(cfg, []workflowRecord{validWorkflow(cfg)}, []workflowRun{run}, jobs)
	_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
	if err == nil {
		t.Fatal("waitForSuccess succeeded with an extra untrusted job")
	}
	if strings.ContainsAny(err.Error(), "\n\r\x1b") {
		t.Fatalf("job-set error contains a raw control character: %q", err)
	}
	if !strings.Contains(err.Error(), `untrusted\njob\x1b[2J`) {
		t.Fatalf("job-set error did not preserve escaped evidence: %q", err)
	}

	detail := quoteExternalDetail("first line\nsecond line\x1b[31m")
	if strings.ContainsAny(detail, "\n\r\x1b") {
		t.Fatalf("external command detail contains a raw control character: %q", detail)
	}
	if !strings.Contains(detail, `\n`) || !strings.Contains(detail, `\x1b`) {
		t.Fatalf("external command detail did not escape controls: %q", detail)
	}
}

func TestTerminalWorkflowAndJobStatesFailImmediately(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	for _, conclusion := range []string{
		"action_required",
		"cancelled",
		"failure",
		"neutral",
		"skipped",
		"stale",
		"startup_failure",
		"timed_out",
	} {
		t.Run("workflow_"+conclusion, func(t *testing.T) {
			t.Parallel()
			run := validRun(cfg, 1, "completed", new(conclusion))
			api := newStaticAPI(cfg, []workflowRecord{validWorkflow(cfg)}, []workflowRun{run}, nil)
			_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
			if err == nil || !strings.Contains(err.Error(), `terminal conclusion="`+conclusion+`"`) {
				t.Fatalf("error = %v", err)
			}
		})
		t.Run("job_"+conclusion, func(t *testing.T) {
			t.Parallel()
			run := validRun(cfg, 1, "completed", new("success"))
			job := validJob(cfg, run, 1, 501, "linux", "completed", new(conclusion))
			api := newStaticAPI(cfg, []workflowRecord{validWorkflow(cfg)}, []workflowRun{run}, []workflowJob{job})
			_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
			if err == nil || !strings.Contains(err.Error(), `terminal conclusion="`+conclusion+`"`) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSameRunIDUsesHighestAttemptAndCarriesPartialRerunSuccesses(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux", "macOS")
	first := validRun(cfg, 1, "completed", new("failure"))
	second := validRun(cfg, 2, "completed", new("success"))
	jobs := []workflowJob{
		validJob(cfg, second, 1, 501, "linux", "completed", new("success")),
		validJob(cfg, second, 1, 502, "macOS", "completed", new("failure")),
		validJob(cfg, second, 2, 503, "macOS", "completed", new("success")),
	}
	api := newStaticAPI(
		cfg,
		[]workflowRecord{validWorkflow(cfg)},
		[]workflowRun{first, second},
		jobs,
	)

	result, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
	if err != nil {
		t.Fatalf("waitForSuccess: %v", err)
	}
	want := map[string]int{"linux": 1, "macOS": 2}
	if result.RunAttempt != 2 || !reflect.DeepEqual(result.JobAttempt, want) {
		t.Fatalf("result = %+v, want job attempts %v", result, want)
	}
	for _, call := range api.callsSnapshot() {
		if call.endpoint == jobEndpoint(cfg, testRunID) && call.query.Get("filter") != "all" {
			t.Fatalf("jobs query filter = %q, want all", call.query.Get("filter"))
		}
		if strings.Contains(call.endpoint, "/attempts/") {
			t.Fatalf("used attempt-specific jobs endpoint: %s", call.endpoint)
		}
	}
}

func TestSnapshotRereadRestartsWhenRerunBegins(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux", "macOS")
	cfg.poll = time.Second
	cfg.timeout = 5 * time.Second
	first := validRun(cfg, 1, "completed", new("success"))
	second := validRun(cfg, 2, "completed", new("success"))
	firstJobs := []workflowJob{
		validJob(cfg, first, 1, 501, "linux", "completed", new("success")),
		validJob(cfg, first, 1, 502, "macOS", "completed", new("success")),
	}
	secondJobs := []workflowJob{
		firstJobs[0],
		firstJobs[1],
		validJob(cfg, second, 2, 503, "macOS", "completed", new("success")),
	}
	var runCalls int
	var jobCalls int
	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		switch endpoint {
		case workflowEndpoint(cfg):
			return pagePayload("workflows", []workflowRecord{validWorkflow(cfg)}), nil
		case runEndpoint(cfg, testWorkflowID):
			runCalls++
			if runCalls == 1 {
				return pagePayload("workflow_runs", []workflowRun{first}), nil
			}
			return pagePayload("workflow_runs", []workflowRun{second}), nil
		case jobEndpoint(cfg, testRunID):
			jobCalls++
			if jobCalls == 1 {
				return pagePayload("jobs", firstJobs), nil
			}
			return pagePayload("jobs", secondJobs), nil
		default:
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
	}
	timer := newFakeClock()

	result, err := waitForSuccess(context.Background(), cfg, api, timer, io.Discard)
	if err != nil {
		t.Fatalf("waitForSuccess: %v", err)
	}
	if timer.sleepCount() != 1 || result.RunAttempt != 2 {
		t.Fatalf("sleeps=%d result=%+v", timer.sleepCount(), result)
	}
	want := map[string]int{"linux": 1, "macOS": 2}
	if !reflect.DeepEqual(result.JobAttempt, want) {
		t.Fatalf("job attempts = %v, want %v", result.JobAttempt, want)
	}
}

func TestPaginatesWorkflowCatalogRunsAndJobs(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.requiredJobs = make([]string, 0, 101)

	workflows := make([]workflowRecord, 0, 101)
	for index := range 100 {
		workflows = append(workflows, workflowRecord{
			ID:    int64(1000 + index),
			Name:  fmt.Sprintf("other-%d", index),
			Path:  fmt.Sprintf(".github/workflows/other-%d.yml", index),
			State: "active",
		})
	}
	workflows = append(workflows, validWorkflow(cfg))

	runs := make([]workflowRun, 0, 101)
	for attempt := 1; attempt <= 101; attempt++ {
		runs = append(runs, validRun(cfg, attempt, "completed", new("success")))
	}

	jobs := make([]workflowJob, 0, 101)
	current := runs[len(runs)-1]
	for index := range 101 {
		name := fmt.Sprintf("job-%03d", index)
		cfg.requiredJobs = append(cfg.requiredJobs, name)
		jobs = append(jobs, validJob(
			cfg,
			current,
			current.RunAttempt,
			int64(5000+index),
			name,
			"completed",
			new("success"),
		))
	}

	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		switch endpoint {
		case workflowEndpoint(cfg):
			return paginatedPayload("workflows", workflows, query)
		case runEndpoint(cfg, testWorkflowID):
			return paginatedPayload("workflow_runs", runs, query)
		case jobEndpoint(cfg, testRunID):
			return paginatedPayload("jobs", jobs, query)
		default:
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
	}

	result, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
	if err != nil {
		t.Fatalf("waitForSuccess: %v", err)
	}
	if result.RunAttempt != 101 || len(result.JobAttempt) != 101 {
		t.Fatalf("result = %+v", result)
	}
	calls := api.callsSnapshot()
	assertEndpointPageCalls(t, calls, workflowEndpoint(cfg), "2", 2)
	assertEndpointPageCalls(t, calls, runEndpoint(cfg, testWorkflowID), "2", 2)
	assertEndpointPageCalls(t, calls, jobEndpoint(cfg, testRunID), "2", 1)
}

func TestAPIErrorsAndMalformedResponsesFailClosed(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	run := validRun(cfg, 1, "completed", new("success"))
	job := validJob(cfg, run, 1, 501, "linux", "completed", new("success"))
	cases := map[string]func(string, url.Values) ([]byte, error){
		"workflow API error": func(endpoint string, query url.Values) ([]byte, error) {
			return nil, errors.New("fixture API unavailable")
		},
		"run API error": func(endpoint string, query url.Values) ([]byte, error) {
			switch endpoint {
			case workflowEndpoint(cfg):
				return pagePayload("workflows", []workflowRecord{validWorkflow(cfg)}), nil
			case runEndpoint(cfg, testWorkflowID):
				return nil, errors.New("fixture runs API unavailable")
			default:
				return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
			}
		},
		"jobs API error": func(endpoint string, query url.Values) ([]byte, error) {
			switch endpoint {
			case workflowEndpoint(cfg):
				return pagePayload("workflows", []workflowRecord{validWorkflow(cfg)}), nil
			case runEndpoint(cfg, testWorkflowID):
				return pagePayload("workflow_runs", []workflowRun{run}), nil
			case jobEndpoint(cfg, testRunID):
				return nil, errors.New("fixture jobs API unavailable")
			default:
				return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
			}
		},
		"malformed workflow JSON": func(endpoint string, query url.Values) ([]byte, error) {
			return []byte(`{"total_count":1,"workflows":[`), nil
		},
		"missing workflow array": func(endpoint string, query url.Values) ([]byte, error) {
			return []byte(`{"total_count":1}`), nil
		},
		"malformed run state": func(endpoint string, query url.Values) ([]byte, error) {
			switch endpoint {
			case workflowEndpoint(cfg):
				return pagePayload("workflows", []workflowRecord{validWorkflow(cfg)}), nil
			case runEndpoint(cfg, testWorkflowID):
				bad := run
				bad.Status = "mystery"
				return pagePayload("workflow_runs", []workflowRun{bad}), nil
			default:
				return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
			}
		},
		"malformed jobs JSON": func(endpoint string, query url.Values) ([]byte, error) {
			switch endpoint {
			case workflowEndpoint(cfg):
				return pagePayload("workflows", []workflowRecord{validWorkflow(cfg)}), nil
			case runEndpoint(cfg, testWorkflowID):
				return pagePayload("workflow_runs", []workflowRun{run}), nil
			case jobEndpoint(cfg, testRunID):
				return append(pagePayload("jobs", []workflowJob{job}), byte('{')), nil
			default:
				return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
			}
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := &fakeAPI{handle: handler}
			_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
			if err == nil {
				t.Fatal("waitForSuccess succeeded, want fail-closed error")
			}
		})
	}
}

func TestTransientGitHubAPIFailureRetriesWithinDeadline(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	run := validRun(cfg, 1, "completed", new("success"))
	job := validJob(cfg, run, 1, 501, "linux", "completed", new("success"))
	var calls int
	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, &retryableAPIError{err: errors.New("HTTP 502 fixture")}
		}
		switch endpoint {
		case workflowEndpoint(cfg):
			return pagePayload("workflows", []workflowRecord{validWorkflow(cfg)}), nil
		case runEndpoint(cfg, testWorkflowID):
			return pagePayload("workflow_runs", []workflowRun{run}), nil
		case jobEndpoint(cfg, testRunID):
			return pagePayload("jobs", []workflowJob{job}), nil
		default:
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
	}
	timer := newFakeClock()

	if _, err := waitForSuccess(context.Background(), cfg, api, timer, io.Discard); err != nil {
		t.Fatalf("waitForSuccess: %v", err)
	}
	if timer.sleepCount() != 1 {
		t.Fatalf("sleep count = %d, want one transient retry", timer.sleepCount())
	}
}

func TestTransientGitHubFailureClassification(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		detail    string
		transient bool
	}{
		"http server error": {
			detail:    "gh: HTTP 502",
			transient: true,
		},
		"rate limit": {
			detail:    "API rate limit exceeded",
			transient: true,
		},
		"gh connection wrapper": {
			detail:    "error connecting to api.github.com",
			transient: true,
		},
		"unreachable proxy": {
			detail:    `Get "https://api.github.com/rate_limit": proxyconnect tcp: dial tcp 127.0.0.1:9: connect: operation not permitted`,
			transient: true,
		},
		"direct dial": {
			detail:    "dial tcp 140.82.114.6:443: connect: network is unreachable",
			transient: true,
		},
		"curl dns wording": {
			detail:    "Could not resolve host: api.github.com",
			transient: true,
		},
		"authentication": {
			detail:    "HTTP 401: Bad credentials",
			transient: false,
		},
		"authorization": {
			detail:    "HTTP 403: Resource not accessible by integration",
			transient: false,
		},
		"invalid invocation": {
			detail:    "unknown flag: --not-a-real-flag",
			transient: false,
		},
		"local execution denial": {
			detail:    "fork/exec gh: operation not permitted",
			transient: false,
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isTransientGHFailure(test.detail); got != test.transient {
				t.Fatalf("isTransientGHFailure(%q) = %t, want %t", test.detail, got, test.transient)
			}
		})
	}
}

func TestPaginationInconsistencyFailsClosed(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	items := make([]workflowRecord, 100)
	for index := range items {
		items[index] = workflowRecord{
			ID:    int64(1000 + index),
			Name:  fmt.Sprintf("other-%d", index),
			Path:  fmt.Sprintf(".github/workflows/other-%d.yml", index),
			State: "active",
		}
	}
	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		if query.Get("page") == "1" {
			return explicitPagePayload("workflows", 101, items), nil
		}
		return explicitPagePayload("workflows", 102, []workflowRecord{validWorkflow(cfg)}), nil
	}

	_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "total_count changed across pages") {
		t.Fatalf("error = %v", err)
	}
}

func TestDuplicateConflictingAndAmbiguousRecordsFailClosed(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	run := validRun(cfg, 1, "completed", new("success"))
	job := validJob(cfg, run, 1, 501, "linux", "completed", new("success"))
	cases := map[string]struct {
		runs []workflowRun
		jobs []workflowJob
		want string
	}{
		"duplicate run attempt": {
			runs: []workflowRun{run, run},
			jobs: []workflowJob{job},
			want: "duplicate workflow run record",
		},
		"ambiguous run IDs": {
			runs: []workflowRun{run, func() workflowRun {
				other := run
				other.ID++
				return other
			}()},
			jobs: []workflowJob{job},
			want: "ambiguous exact workflow runs",
		},
		"duplicate job name and attempt": {
			runs: []workflowRun{run},
			jobs: []workflowJob{job, func() workflowJob {
				duplicate := job
				duplicate.ID++
				return duplicate
			}()},
			want: "duplicate workflow job name",
		},
		"duplicate job ID": {
			runs: []workflowRun{run},
			jobs: []workflowJob{job, func() workflowJob {
				conflict := job
				conflict.Name = "other"
				return conflict
			}()},
			want: "duplicate workflow job ID",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newStaticAPI(
				cfg,
				[]workflowRecord{validWorkflow(cfg)},
				testCase.runs,
				testCase.jobs,
			)
			_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

func TestExactSHAIsValidatedOnRunsAndJobs(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	run := validRun(cfg, 1, "completed", new("success"))
	job := validJob(cfg, run, 1, 501, "linux", "completed", new("success"))
	t.Run("run", func(t *testing.T) {
		t.Parallel()
		badRun := run
		badRun.HeadSHA = strings.Repeat("b", 40)
		api := newStaticAPI(
			cfg,
			[]workflowRecord{validWorkflow(cfg)},
			[]workflowRun{badRun},
			[]workflowJob{job},
		)
		_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "head_sha") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("job", func(t *testing.T) {
		t.Parallel()
		badJob := job
		badJob.HeadSHA = strings.Repeat("b", 40)
		api := newStaticAPI(
			cfg,
			[]workflowRecord{validWorkflow(cfg)},
			[]workflowRun{run},
			[]workflowJob{badJob},
		)
		_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "head_sha") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunTargetFieldsAreValidatedExactly(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	run := validRun(cfg, 1, "completed", new("success"))
	job := validJob(cfg, run, 1, 501, "linux", "completed", new("success"))
	cases := map[string]struct {
		mutate func(*workflowRun)
		want   string
	}{
		"branch": {
			mutate: func(run *workflowRun) { run.HeadBranch = "other" },
			want:   "head_branch",
		},
		"event": {
			mutate: func(run *workflowRun) { run.Event = "workflow_dispatch" },
			want:   "event=",
		},
		"workflow ID": {
			mutate: func(run *workflowRun) { run.WorkflowID++ },
			want:   "workflow_id",
		},
		"workflow path": {
			mutate: func(run *workflowRun) { run.Path = ".github/workflows/other.yml@main" },
			want:   "path=",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			badRun := run
			testCase.mutate(&badRun)
			api := newStaticAPI(
				cfg,
				[]workflowRecord{validWorkflow(cfg)},
				[]workflowRun{badRun},
				[]workflowJob{job},
			)
			_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

func TestWorkflowResolutionRequiresExactActivePathAndName(t *testing.T) {
	t.Parallel()
	cfg := testConfig("linux")
	run := validRun(cfg, 1, "completed", new("success"))
	job := validJob(cfg, run, 1, 501, "linux", "completed", new("success"))
	cases := map[string]struct {
		workflows []workflowRecord
		want      string
	}{
		"missing path": {
			workflows: nil,
			want:      "expected exactly one workflow",
		},
		"duplicate path": {
			workflows: []workflowRecord{validWorkflow(cfg), func() workflowRecord {
				duplicate := validWorkflow(cfg)
				duplicate.ID++
				return duplicate
			}()},
			want: "expected exactly one workflow",
		},
		"wrong name": {
			workflows: []workflowRecord{func() workflowRecord {
				workflow := validWorkflow(cfg)
				workflow.Name = "CI"
				return workflow
			}()},
			want: "want exact",
		},
		"inactive": {
			workflows: []workflowRecord{func() workflowRecord {
				workflow := validWorkflow(cfg)
				workflow.State = "disabled_manually"
				return workflow
			}()},
			want: "want active",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newStaticAPI(cfg, testCase.workflows, []workflowRun{run}, []workflowJob{job})
			_, err := waitForSuccess(context.Background(), cfg, api, newFakeClock(), io.Discard)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

type apiCall struct {
	endpoint string
	query    url.Values
}

type fakeAPI struct {
	mu     sync.Mutex
	calls  []apiCall
	handle func(string, url.Values) ([]byte, error)
}

func (api *fakeAPI) get(_ context.Context, endpoint string, query url.Values) ([]byte, error) {
	query = cloneValues(query)
	api.mu.Lock()
	api.calls = append(api.calls, apiCall{endpoint: endpoint, query: query})
	handler := api.handle
	api.mu.Unlock()
	if handler == nil {
		return nil, errors.New("fake API has no handler")
	}
	return handler(endpoint, query)
}

func (api *fakeAPI) callsSnapshot() []apiCall {
	api.mu.Lock()
	defer api.mu.Unlock()
	result := make([]apiCall, len(api.calls))
	copy(result, api.calls)
	return result
}

func newStaticAPI(
	cfg config,
	workflows []workflowRecord,
	runs []workflowRun,
	jobs []workflowJob,
) *fakeAPI {
	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		if query.Get("page") != "1" || query.Get("per_page") != strconv.Itoa(apiPageSize) {
			return nil, fmt.Errorf("unexpected pagination query: %v", query)
		}
		switch endpoint {
		case workflowEndpoint(cfg):
			return pagePayload("workflows", workflows), nil
		case runEndpoint(cfg, testWorkflowID):
			if query.Get("branch") != cfg.branch ||
				query.Get("event") != cfg.event ||
				query.Get("head_sha") != cfg.sha {
				return nil, fmt.Errorf("missing exact run filters: %v", query)
			}
			return pagePayload("workflow_runs", runs), nil
		case jobEndpoint(cfg, testRunID):
			if query.Get("filter") != "all" {
				return nil, fmt.Errorf("jobs filter = %q, want all", query.Get("filter"))
			}
			return pagePayload("jobs", jobs), nil
		default:
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
	}
	return api
}

type fakeClock struct {
	mu      sync.Mutex
	current time.Time
	sleeps  []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{current: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)}
}

func (clock *fakeClock) now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.current
}

func (clock *fakeClock) sleep(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.sleeps = append(clock.sleeps, duration)
	clock.current = clock.current.Add(duration)
	return nil
}

func (clock *fakeClock) sleepCount() int {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return len(clock.sleeps)
}

func testConfig(jobs ...string) config {
	return config{
		repository:   "osauer/canary",
		workflow:     "ci.yml",
		workflowName: "ci",
		sha:          testSHA,
		branch:       "main",
		event:        "push",
		requiredJobs: append([]string(nil), jobs...),
		poll:         time.Second,
		timeout:      10 * time.Second,
	}
}

func validWorkflow(cfg config) workflowRecord {
	return workflowRecord{
		ID:    testWorkflowID,
		Name:  cfg.workflowName,
		Path:  ".github/workflows/" + cfg.workflow,
		State: "active",
	}
}

func validRun(cfg config, attempt int, status string, conclusion *string) workflowRun {
	return workflowRun{
		ID:         testRunID,
		Name:       cfg.workflowName,
		HeadBranch: cfg.branch,
		HeadSHA:    cfg.sha,
		Path:       ".github/workflows/" + cfg.workflow + "@" + cfg.branch,
		Event:      cfg.event,
		Status:     status,
		Conclusion: conclusion,
		WorkflowID: testWorkflowID,
		RunAttempt: attempt,
	}
}

func validJob(
	cfg config,
	run workflowRun,
	attempt int,
	id int64,
	name string,
	status string,
	conclusion *string,
) workflowJob {
	return workflowJob{
		ID:           id,
		RunID:        run.ID,
		RunAttempt:   attempt,
		HeadSHA:      cfg.sha,
		HeadBranch:   cfg.branch,
		WorkflowName: cfg.workflowName,
		Name:         name,
		Status:       status,
		Conclusion:   conclusion,
	}
}

func workflowEndpoint(cfg config) string {
	return fmt.Sprintf("repos/%s/actions/workflows", cfg.repository)
}

func runEndpoint(cfg config, workflowID int64) string {
	return fmt.Sprintf("repos/%s/actions/workflows/%d/runs", cfg.repository, workflowID)
}

func jobEndpoint(cfg config, runID int64) string {
	return fmt.Sprintf("repos/%s/actions/runs/%d/jobs", cfg.repository, runID)
}

func pagePayload(key string, items any) []byte {
	value := reflect.ValueOf(items)
	if value.Kind() == reflect.Slice && value.IsNil() {
		value = reflect.MakeSlice(value.Type(), 0, 0)
		items = value.Interface()
	}
	return explicitPagePayload(key, value.Len(), items)
}

func explicitPagePayload(key string, total int, items any) []byte {
	raw, err := jsonMarshal(map[string]any{
		"total_count": total,
		key:           items,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func paginatedPayload[T any](key string, items []T, query url.Values) ([]byte, error) {
	page, err := strconv.Atoi(query.Get("page"))
	if err != nil || page < 1 {
		return nil, fmt.Errorf("invalid page %q", query.Get("page"))
	}
	if query.Get("per_page") != strconv.Itoa(apiPageSize) {
		return nil, fmt.Errorf("per_page = %q", query.Get("per_page"))
	}
	start := (page - 1) * apiPageSize
	if start > len(items) {
		return nil, fmt.Errorf("page %d starts beyond %d items", page, len(items))
	}
	end := min(start+apiPageSize, len(items))
	return explicitPagePayload(key, len(items), items[start:end]), nil
}

// Kept behind a helper so payload construction remains concise and any future
// fixture encoding failure is loud.
func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}

func assertEndpointCalls(t *testing.T, calls []apiCall, endpoint string, want int) {
	t.Helper()
	got := 0
	for _, call := range calls {
		if call.endpoint == endpoint {
			got++
		}
	}
	if got != want {
		t.Fatalf("calls to %s = %d, want %d; calls=%v", endpoint, got, want, calls)
	}
}

func assertEndpointPageCalls(
	t *testing.T,
	calls []apiCall,
	endpoint string,
	page string,
	want int,
) {
	t.Helper()
	got := 0
	for _, call := range calls {
		if call.endpoint == endpoint && call.query.Get("page") == page {
			got++
		}
	}
	if got != want {
		t.Fatalf("calls to %s page %s = %d, want %d", endpoint, page, got, want)
	}
}
