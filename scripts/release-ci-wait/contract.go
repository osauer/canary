package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
)

const maxContractBytes = 64 * 1024

type workflowContract struct {
	File string   `json:"file"`
	Name string   `json:"name"`
	Jobs []string `json:"jobs"`
}

type contractDocument struct {
	Repository string             `json:"repository"`
	Workflows  []workflowContract `json:"workflows"`
}

type contractWaitResult struct {
	workflows map[string]waitResult
	jobCount  int
}

type contractObservation struct {
	result  contractWaitResult
	pending string
}

type targetSnapshot struct {
	cfg      config
	workflow workflowRecord
	run      workflowRun
	jobs     []workflowJob
}

func loadContractConfig(cfg config) (config, error) {
	if err := validateText("-contract", cfg.contractPath); err != nil {
		return config{}, err
	}
	raw, err := os.ReadFile(cfg.contractPath)
	if err != nil {
		return config{}, fmt.Errorf("read -contract: %w", err)
	}
	if len(raw) > maxContractBytes {
		return config{}, fmt.Errorf("-contract exceeds %d bytes", maxContractBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document contractDocument
	if err := decoder.Decode(&document); err != nil {
		return config{}, fmt.Errorf("decode -contract: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return config{}, fmt.Errorf("decode -contract: %w", err)
	}
	if len(document.Workflows) == 0 {
		return config{}, errors.New("-contract must contain at least one workflow")
	}
	if len(document.Workflows) > 32 {
		return config{}, errors.New("-contract contains more than 32 workflows")
	}

	cfg.repository = document.Repository
	cfg.workflows = slices.Clone(document.Workflows)
	seenFiles := make(map[string]struct{}, len(cfg.workflows))
	seenNames := make(map[string]struct{}, len(cfg.workflows))
	for index, workflow := range cfg.workflows {
		if _, exists := seenFiles[workflow.File]; exists {
			return config{}, fmt.Errorf("-contract contains duplicate workflow file %q", workflow.File)
		}
		seenFiles[workflow.File] = struct{}{}
		if _, exists := seenNames[workflow.Name]; exists {
			return config{}, fmt.Errorf("-contract contains duplicate workflow name %q", workflow.Name)
		}
		seenNames[workflow.Name] = struct{}{}

		perWorkflow := contractWorkflowConfig(cfg, workflow)
		if err := validateConfig(perWorkflow); err != nil {
			return config{}, fmt.Errorf("-contract workflows[%d]: %w", index, err)
		}
	}
	return cfg, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}

func contractWorkflowConfig(parent config, workflow workflowContract) config {
	cfg := parent
	cfg.workflow = workflow.File
	cfg.workflowName = workflow.Name
	cfg.requiredJobs = slices.Clone(workflow.Jobs)
	cfg.workflows = nil
	return cfg
}

func waitForContractSuccess(
	ctx context.Context,
	cfg config,
	api apiClient,
	timer clock,
	progress io.Writer,
) (contractWaitResult, error) {
	deadline := timer.now().Add(cfg.timeout)
	lastPending := ""
	for {
		remaining := deadline.Sub(timer.now())
		if remaining <= 0 {
			return contractWaitResult{}, contractTimeoutError(cfg, lastPending)
		}

		callCtx, cancel := context.WithTimeout(ctx, remaining)
		current, err := inspectContract(callCtx, cfg, api)
		callContextErr := callCtx.Err()
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return contractWaitResult{}, ctx.Err()
			}
			if callContextErr != nil {
				return contractWaitResult{}, contractTimeoutError(cfg, lastPending)
			}
			if _, ok := errors.AsType[*retryableAPIError](err); ok {
				currentPending := "transient GitHub API failure; retrying: " + err.Error()
				if currentPending != lastPending {
					fmt.Fprintf(
						progress,
						"release-ci-wait: pending contract=%s sha=%s: %s\n",
						cfg.contractPath,
						cfg.sha,
						currentPending,
					)
					lastPending = currentPending
				}
				remaining = deadline.Sub(timer.now())
				if remaining <= 0 {
					return contractWaitResult{}, contractTimeoutError(cfg, lastPending)
				}
				if err := timer.sleep(ctx, min(cfg.poll, remaining)); err != nil {
					return contractWaitResult{}, err
				}
				continue
			}
			return contractWaitResult{}, err
		}
		if current.pending == "" {
			return current.result, nil
		}
		if current.pending != lastPending {
			fmt.Fprintf(
				progress,
				"release-ci-wait: pending contract=%s sha=%s: %s\n",
				cfg.contractPath,
				cfg.sha,
				current.pending,
			)
			lastPending = current.pending
		}

		remaining = deadline.Sub(timer.now())
		if remaining <= 0 {
			return contractWaitResult{}, contractTimeoutError(cfg, lastPending)
		}
		if err := timer.sleep(ctx, min(cfg.poll, remaining)); err != nil {
			return contractWaitResult{}, err
		}
	}
}

func contractTimeoutError(cfg config, pending string) error {
	if pending == "" {
		pending = "no successful observation"
	}
	return fmt.Errorf(
		"timed out after %s waiting for workflow contract %s at %s (%s)",
		cfg.timeout,
		cfg.contractPath,
		cfg.sha,
		pending,
	)
}

func inspectContract(
	ctx context.Context,
	cfg config,
	api apiClient,
) (contractObservation, error) {
	first := make([]targetSnapshot, 0, len(cfg.workflows))
	var pending []string
	for _, workflow := range cfg.workflows {
		perWorkflow := contractWorkflowConfig(cfg, workflow)
		target, targetPending, err := resolveTarget(ctx, perWorkflow, api, cfg.historical)
		if err != nil {
			return contractObservation{}, fmt.Errorf("workflow %q: %w", workflow.File, err)
		}
		if targetPending != "" {
			pending = append(
				pending,
				fmt.Sprintf("%s: %s", strconvQuote(workflow.File), targetPending),
			)
			continue
		}
		first = append(first, target)
	}
	if len(pending) > 0 {
		return contractObservation{pending: strings.Join(pending, "; ")}, nil
	}

	for index := range first {
		jobs, err := listJobs(ctx, first[index].cfg, first[index].run, api)
		if err != nil {
			return contractObservation{}, fmt.Errorf(
				"workflow %q jobs: %w",
				first[index].cfg.workflow,
				err,
			)
		}
		first[index].jobs = jobs
	}

	second := make([]targetSnapshot, 0, len(first))
	pending = pending[:0]
	for _, previous := range first {
		current, targetPending, err := resolveTarget(ctx, previous.cfg, api, cfg.historical)
		if err != nil {
			return contractObservation{}, fmt.Errorf(
				"workflow %q recheck: %w",
				previous.cfg.workflow,
				err,
			)
		}
		if targetPending != "" {
			pending = append(
				pending,
				fmt.Sprintf(
					"%s changed during inspection: %s",
					strconvQuote(previous.cfg.workflow),
					targetPending,
				),
			)
			continue
		}
		if current.workflow.ID != previous.workflow.ID {
			return contractObservation{}, fmt.Errorf(
				"workflow %q identity changed during inspection: %d -> %d",
				previous.cfg.workflow,
				previous.workflow.ID,
				current.workflow.ID,
			)
		}
		if current.run.ID != previous.run.ID {
			return contractObservation{}, fmt.Errorf(
				"workflow %q changed run ID during inspection: %d -> %d",
				previous.cfg.workflow,
				previous.run.ID,
				current.run.ID,
			)
		}
		if current.run.RunAttempt != previous.run.RunAttempt ||
			current.run.Status != previous.run.Status ||
			!equalOptionalString(current.run.Conclusion, previous.run.Conclusion) {
			pending = append(
				pending,
				fmt.Sprintf(
					"%s run %d changed during inspection (attempt %d -> %d, status %q -> %q)",
					strconvQuote(previous.cfg.workflow),
					previous.run.ID,
					previous.run.RunAttempt,
					current.run.RunAttempt,
					previous.run.Status,
					current.run.Status,
				),
			)
			continue
		}
		current.jobs = previous.jobs
		second = append(second, current)
	}
	if len(pending) > 0 {
		return contractObservation{pending: strings.Join(pending, "; ")}, nil
	}

	result := contractWaitResult{
		workflows: make(map[string]waitResult, len(second)),
	}
	pending = pending[:0]
	for _, target := range second {
		jobAttempts, jobPending, err := evaluateJobs(target.cfg, target.run, target.jobs)
		if err != nil {
			return contractObservation{}, fmt.Errorf(
				"workflow %q: %w",
				target.cfg.workflow,
				err,
			)
		}
		if jobPending != "" {
			pending = append(
				pending,
				fmt.Sprintf("%s: %s", strconvQuote(target.cfg.workflow), jobPending),
			)
			continue
		}
		result.workflows[target.cfg.workflow] = waitResult{
			WorkflowID: target.workflow.ID,
			RunID:      target.run.ID,
			RunAttempt: target.run.RunAttempt,
			JobAttempt: jobAttempts,
		}
		result.jobCount += len(jobAttempts)
	}
	if len(pending) > 0 {
		return contractObservation{pending: strings.Join(pending, "; ")}, nil
	}
	return contractObservation{result: result}, nil
}

func strconvQuote(value string) string {
	return fmt.Sprintf("%q", value)
}

func resolveTarget(
	ctx context.Context,
	cfg config,
	api apiClient,
	historical bool,
) (targetSnapshot, string, error) {
	if historical {
		workflow, run, pending, err := resolveHistoricalRun(ctx, cfg, api)
		return targetSnapshot{cfg: cfg, workflow: workflow, run: run}, pending, err
	}
	workflow, err := resolveWorkflow(ctx, cfg, api)
	if err != nil {
		return targetSnapshot{}, "", err
	}
	run, pending, err := resolveRun(ctx, cfg, workflow, api)
	return targetSnapshot{cfg: cfg, workflow: workflow, run: run}, pending, err
}

func resolveHistoricalRun(
	ctx context.Context,
	cfg config,
	api apiClient,
) (workflowRecord, workflowRun, string, error) {
	endpoint := fmt.Sprintf("repos/%s/actions/runs", cfg.repository)
	query := url.Values{
		"branch":   {cfg.branch},
		"event":    {cfg.event},
		"head_sha": {cfg.sha},
	}
	runs, err := collectPages(
		ctx,
		api,
		endpoint,
		query,
		"historical workflow runs",
		maxRunResults,
		func(raw []byte) (*int, *[]workflowRun, error) {
			var payload struct {
				TotalCount   *int           `json:"total_count"`
				WorkflowRuns *[]workflowRun `json:"workflow_runs"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return nil, nil, err
			}
			return payload.TotalCount, payload.WorkflowRuns, nil
		},
	)
	if err != nil {
		return workflowRecord{}, workflowRun{}, "", err
	}

	var matching []workflowRun
	var workflow workflowRecord
	for _, run := range runs {
		if !matchesWorkflowPath(run.Path, cfg.workflow) {
			continue
		}
		if run.WorkflowID <= 0 {
			return workflowRecord{}, workflowRun{}, "", errors.New(
				"malformed historical workflow run: workflow_id must be positive",
			)
		}
		if workflow.ID == 0 {
			workflow = workflowRecord{
				ID:    run.WorkflowID,
				Name:  run.Name,
				Path:  ".github/workflows/" + cfg.workflow,
				State: "historical",
			}
		}
		if run.WorkflowID != workflow.ID {
			return workflowRecord{}, workflowRun{}, "", fmt.Errorf(
				"historical workflow %q has multiple workflow IDs: %d and %d",
				cfg.workflow,
				workflow.ID,
				run.WorkflowID,
			)
		}
		if err := validateRunRecord(cfg, workflow, run); err != nil {
			return workflowRecord{}, workflowRun{}, "", err
		}
		matching = append(matching, run)
	}
	if len(matching) == 0 {
		return workflowRecord{}, workflowRun{}, "exact historical workflow run has not appeared", nil
	}
	selected, pending, err := selectExactRun(matching)
	if err != nil {
		return workflowRecord{}, workflowRun{}, "", err
	}
	return workflow, selected, pending, nil
}

func selectExactRun(runs []workflowRun) (workflowRun, string, error) {
	byID := make(map[int64]map[int]workflowRun)
	for _, run := range runs {
		attempts := byID[run.ID]
		if attempts == nil {
			attempts = make(map[int]workflowRun)
			byID[run.ID] = attempts
		}
		if _, exists := attempts[run.RunAttempt]; exists {
			return workflowRun{}, "", fmt.Errorf(
				"duplicate workflow run record for ID %d attempt %d",
				run.ID,
				run.RunAttempt,
			)
		}
		attempts[run.RunAttempt] = run
	}
	if len(byID) != 1 {
		ids := make([]int64, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return workflowRun{}, "", fmt.Errorf(
			"ambiguous exact workflow runs: distinct run IDs %v",
			ids,
		)
	}

	var selected workflowRun
	for _, attempts := range byID {
		for attempt, run := range attempts {
			if attempt > selected.RunAttempt {
				selected = run
			}
		}
	}
	state, err := classifyState("workflow run", selected.Status, selected.Conclusion)
	if err != nil {
		return workflowRun{}, "", err
	}
	if state == statePending {
		return workflowRun{}, fmt.Sprintf(
			"run %d attempt %d is %q",
			selected.ID,
			selected.RunAttempt,
			selected.Status,
		), nil
	}
	return selected, "", nil
}
