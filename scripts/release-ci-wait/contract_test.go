package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestParseConfigLoadsStrictAggregateContract(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/contract.json"
	raw := `{
		"repository":"osauer/canary",
		"workflows":[
			{"file":"ci.yml","name":"ci","jobs":["linux"]},
			{"file":"pages-check.yml","name":"pages check","jobs":["pages"]}
		]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write contract: %v", err)
	}
	cfg, err := parseConfig([]string{
		"-contract", path,
		"-historical",
		"-sha", testSHA,
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if !cfg.historical || cfg.repository != "osauer/canary" || len(cfg.workflows) != 2 {
		t.Fatalf("config = %+v", cfg)
	}

	_, err = parseConfig([]string{
		"-contract", path,
		"-workflow", "ci.yml",
		"-sha", testSHA,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with -contract") {
		t.Fatalf("error = %v, want mutually exclusive workflow authority", err)
	}
}

func TestContractSnapshotRechecksAllRunsAfterAllJobReads(t *testing.T) {
	t.Parallel()
	cfg := aggregateTestConfig(false)
	ciCfg := contractWorkflowConfig(cfg, cfg.workflows[0])
	pagesCfg := contractWorkflowConfig(cfg, cfg.workflows[1])
	ciWorkflow, ciRun, ciJobs := aggregateFixture(ciCfg, 17, 4101, 500)
	pagesWorkflow, pagesRun, pagesJobs := aggregateFixture(pagesCfg, 18, 4102, 600)

	ciRerun := ciRun
	ciRerun.RunAttempt = 2
	ciRerun.Status = "in_progress"
	ciRerun.Conclusion = nil
	var ciRunCalls int
	var events []string
	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		events = append(events, endpoint)
		switch endpoint {
		case workflowEndpoint(ciCfg):
			return pagePayload("workflows", []workflowRecord{ciWorkflow, pagesWorkflow}), nil
		case runEndpoint(ciCfg, ciWorkflow.ID):
			ciRunCalls++
			if ciRunCalls == 1 {
				return pagePayload("workflow_runs", []workflowRun{ciRun}), nil
			}
			return pagePayload("workflow_runs", []workflowRun{ciRerun}), nil
		case runEndpoint(pagesCfg, pagesWorkflow.ID):
			return pagePayload("workflow_runs", []workflowRun{pagesRun}), nil
		case jobEndpoint(ciCfg, ciRun.ID):
			return pagePayload("jobs", ciJobs), nil
		case jobEndpoint(pagesCfg, pagesRun.ID):
			return pagePayload("jobs", pagesJobs), nil
		default:
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
	}

	observation, err := inspectContract(context.Background(), cfg, api)
	if err != nil {
		t.Fatalf("inspectContract: %v", err)
	}
	if !strings.Contains(observation.pending, `"ci.yml" changed during inspection`) {
		t.Fatalf("pending = %q, want aggregate rerun drift", observation.pending)
	}
	ciSecondRunIndex := nthIndex(events, runEndpoint(ciCfg, ciWorkflow.ID), 2)
	pagesJobsIndex := nthIndex(events, jobEndpoint(pagesCfg, pagesRun.ID), 1)
	if ciSecondRunIndex < 0 || pagesJobsIndex < 0 || pagesJobsIndex >= ciSecondRunIndex {
		t.Fatalf("events do not show all job reads before global recheck: %v", events)
	}
}

func TestHistoricalContractDoesNotDependOnCurrentWorkflowCatalog(t *testing.T) {
	t.Parallel()
	cfg := aggregateTestConfig(true)
	ciCfg := contractWorkflowConfig(cfg, cfg.workflows[0])
	pagesCfg := contractWorkflowConfig(cfg, cfg.workflows[1])
	_, ciRun, ciJobs := aggregateFixture(ciCfg, 17, 4101, 500)
	_, pagesRun, pagesJobs := aggregateFixture(pagesCfg, 18, 4102, 600)
	historicalEndpoint := fmt.Sprintf("repos/%s/actions/runs", cfg.repository)

	api := &fakeAPI{}
	api.handle = func(endpoint string, query url.Values) ([]byte, error) {
		switch endpoint {
		case historicalEndpoint:
			if query.Get("head_sha") != cfg.sha ||
				query.Get("branch") != cfg.branch ||
				query.Get("event") != cfg.event {
				return nil, fmt.Errorf("historical query lost exact filters: %v", query)
			}
			return pagePayload("workflow_runs", []workflowRun{ciRun, pagesRun}), nil
		case jobEndpoint(ciCfg, ciRun.ID):
			return pagePayload("jobs", ciJobs), nil
		case jobEndpoint(pagesCfg, pagesRun.ID):
			return pagePayload("jobs", pagesJobs), nil
		case workflowEndpoint(cfg):
			return nil, fmt.Errorf("current workflow catalog must not be read during historical verification")
		default:
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
	}

	result, err := waitForContractSuccess(
		context.Background(),
		cfg,
		api,
		newFakeClock(),
		io.Discard,
	)
	if err != nil {
		t.Fatalf("waitForContractSuccess: %v", err)
	}
	if len(result.workflows) != 2 || result.jobCount != 2 {
		t.Fatalf("result = %+v, want two historical workflows/jobs", result)
	}
	assertEndpointCalls(t, api.callsSnapshot(), historicalEndpoint, 4)
	assertEndpointCalls(t, api.callsSnapshot(), workflowEndpoint(cfg), 0)
}

func TestHistoricalContractRejectsMissingWorkflowIdentity(t *testing.T) {
	t.Parallel()
	cfg := aggregateTestConfig(true)
	perWorkflow := contractWorkflowConfig(cfg, cfg.workflows[0])
	_, run, _ := aggregateFixture(perWorkflow, 17, 4101, 500)
	run.WorkflowID = 0
	historicalEndpoint := fmt.Sprintf("repos/%s/actions/runs", cfg.repository)
	api := &fakeAPI{}
	api.handle = func(endpoint string, _ url.Values) ([]byte, error) {
		if endpoint != historicalEndpoint {
			return nil, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
		return pagePayload("workflow_runs", []workflowRun{run}), nil
	}

	_, _, _, err := resolveHistoricalRun(context.Background(), perWorkflow, api)
	if err == nil || !strings.Contains(err.Error(), "workflow_id must be positive") {
		t.Fatalf("error = %v, want malformed workflow identity", err)
	}
}

func aggregateTestConfig(historical bool) config {
	cfg := testConfig()
	cfg.contractPath = "scripts/release-ci-contract.json"
	cfg.historical = historical
	cfg.workflows = []workflowContract{
		{File: "ci.yml", Name: "ci", Jobs: []string{"ci job"}},
		{File: "pages-check.yml", Name: "pages check", Jobs: []string{"pages job"}},
	}
	return cfg
}

func aggregateFixture(
	cfg config,
	workflowID int64,
	runID int64,
	jobID int64,
) (workflowRecord, workflowRun, []workflowJob) {
	workflow := validWorkflow(cfg)
	workflow.ID = workflowID
	run := validRun(cfg, 1, "completed", new("success"))
	run.ID = runID
	run.WorkflowID = workflowID
	jobs := []workflowJob{
		validJob(cfg, run, 1, jobID, cfg.requiredJobs[0], "completed", new("success")),
	}
	return workflow, run, jobs
}

func nthIndex(values []string, target string, occurrence int) int {
	seen := 0
	for index, value := range values {
		if value != target {
			continue
		}
		seen++
		if seen == occurrence {
			return index
		}
	}
	return -1
}
