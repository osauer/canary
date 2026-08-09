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
