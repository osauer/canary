// Command release-ci-wait waits for one active GitHub Actions workflow and its
// exact required job set to succeed for a specific commit, branch, and event.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	apiPageSize   = 100
	defaultPoll   = 15 * time.Second
	defaultWait   = 30 * time.Minute
	maxRunResults = 1000
)

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	workflowPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.(yml|yaml)$`)
	shaPattern        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	eventPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	transientHTTP     = regexp.MustCompile(`\bhttp[[:space:]]+5[0-9]{2}\b`)
)

type config struct {
	repository   string
	workflow     string
	workflowName string
	sha          string
	branch       string
	event        string
	requiredJobs []string
	poll         time.Duration
	timeout      time.Duration
	contractPath string
	historical   bool
	workflows    []workflowContract
}

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(raw string) error {
	for part := range strings.SplitSeq(raw, ",") {
		value := strings.TrimSpace(part)
		if value == "" {
			return errors.New("job name must not be empty")
		}
		*values = append(*values, value)
	}
	return nil
}

type apiClient interface {
	get(context.Context, string, url.Values) ([]byte, error)
}

type ghAPI struct{}

type retryableAPIError struct {
	err error
}

func (err *retryableAPIError) Error() string {
	return err.err.Error()
}

func (err *retryableAPIError) Unwrap() error {
	return err.err
}

func (ghAPI) get(ctx context.Context, endpoint string, query url.Values) ([]byte, error) {
	args := ghAPIArgs(endpoint, query)
	command := exec.CommandContext(ctx, "gh", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		apiErr := fmt.Errorf("gh api GET %s: %s", endpoint, quoteExternalDetail(detail))
		if isTransientGHFailure(detail) {
			return nil, &retryableAPIError{err: apiErr}
		}
		return nil, apiErr
	}
	return stdout.Bytes(), nil
}

func isTransientGHFailure(detail string) bool {
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "http 429") || transientHTTP.MatchString(lower) {
		return true
	}
	for _, marker := range []string{
		"rate limit",
		"timeout",
		"timed out",
		"temporarily unavailable",
		"connection reset",
		"connection refused",
		"error connecting to api.github.com",
		"proxyconnect tcp",
		"dial tcp",
		"tls handshake",
		"unexpected eof",
		"network is unreachable",
		"no such host",
		"could not resolve host",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func quoteExternalDetail(detail string) string {
	if len(detail) > 4096 {
		detail = detail[:4096] + "..."
	}
	return strconv.QuoteToASCII(detail)
}

func quoteStrings(values []string) string {
	var rendered strings.Builder
	rendered.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			rendered.WriteByte(' ')
		}
		rendered.WriteString(strconv.QuoteToASCII(value))
	}
	rendered.WriteByte(']')
	return rendered.String()
}

func contractRunEvidence(workflows map[string]waitResult) string {
	names := make([]string, 0, len(workflows))
	for name := range workflows {
		names = append(names, name)
	}
	sort.Strings(names)

	var rendered strings.Builder
	rendered.WriteByte('[')
	for index, name := range names {
		if index > 0 {
			rendered.WriteString("; ")
		}
		result := workflows[name]
		fmt.Fprintf(
			&rendered,
			"workflow=%s,run_id=%d,attempt=%d",
			strconv.QuoteToASCII(name),
			result.RunID,
			result.RunAttempt,
		)
	}
	rendered.WriteByte(']')
	return rendered.String()
}

func ghAPIArgs(endpoint string, query url.Values) []string {
	args := []string{
		"api",
		"--method", "GET",
		"--hostname", "github.com",
		"-H", "Accept: application/vnd.github+json",
		"-H", "X-GitHub-Api-Version: 2022-11-28",
		endpoint,
	}
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, value := range query[key] {
			args = append(args, "-f", key+"="+value)
		}
	}
	return args
}

type clock interface {
	now() time.Time
	sleep(context.Context, time.Duration) error
}

type wallClock struct{}

func (wallClock) now() time.Time {
	return time.Now()
}

func (wallClock) sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type workflowRecord struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

type workflowRun struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	HeadBranch  string  `json:"head_branch"`
	HeadSHA     string  `json:"head_sha"`
	Path        string  `json:"path"`
	Event       string  `json:"event"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	WorkflowID  int64   `json:"workflow_id"`
	RunAttempt  int     `json:"run_attempt"`
	RunNumber   int64   `json:"run_number"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	DisplayName string  `json:"display_title"`
}

type workflowJob struct {
	ID           int64   `json:"id"`
	RunID        int64   `json:"run_id"`
	RunAttempt   int     `json:"run_attempt"`
	HeadSHA      string  `json:"head_sha"`
	HeadBranch   string  `json:"head_branch"`
	WorkflowName string  `json:"workflow_name"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	Conclusion   *string `json:"conclusion"`
}

type waitResult struct {
	WorkflowID int64
	RunID      int64
	RunAttempt int
	JobAttempt map[string]int
}

type observation struct {
	result  waitResult
	pending string
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "release-ci-wait: %v\n", err)
		return 2
	}

	if len(cfg.workflows) > 0 {
		result, err := waitForContractSuccess(context.Background(), cfg, ghAPI{}, wallClock{}, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "release-ci-wait: %v\n", err)
			return 1
		}
		fmt.Fprintf(
			stdout,
			"release-ci-wait: success contract=%s sha=%s workflows=%d jobs=%d historical=%t runs=%s\n",
			cfg.contractPath,
			cfg.sha,
			len(result.workflows),
			result.jobCount,
			cfg.historical,
			contractRunEvidence(result.workflows),
		)
	} else {
		result, err := waitForSuccess(context.Background(), cfg, ghAPI{}, wallClock{}, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "release-ci-wait: %v\n", err)
			return 1
		}
		fmt.Fprintf(
			stdout,
			"release-ci-wait: success workflow=%s sha=%s run_id=%d attempt=%d jobs=%d\n",
			cfg.workflow,
			cfg.sha,
			result.RunID,
			result.RunAttempt,
			len(result.JobAttempt),
		)
	}
	return 0
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	cfg := config{
		repository: "osauer/canary",
		workflow:   "ci.yml",
		branch:     "main",
		event:      "push",
		poll:       defaultPoll,
		timeout:    defaultWait,
	}
	var jobs stringList
	var contractPath string
	var historical bool
	flags := flag.NewFlagSet("release-ci-wait", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.repository, "repo", cfg.repository, "GitHub repository as owner/name")
	flags.StringVar(&cfg.workflow, "workflow", cfg.workflow, "workflow filename under .github/workflows")
	flags.StringVar(&cfg.workflowName, "workflow-name", "", "exact workflow display name")
	flags.StringVar(&cfg.sha, "sha", "", "exact 40-character commit SHA")
	flags.StringVar(&cfg.branch, "branch", cfg.branch, "exact workflow head branch")
	flags.StringVar(&cfg.event, "event", cfg.event, "exact workflow event")
	flags.Var(&jobs, "job", "required exact job name; repeat or provide a comma-separated list")
	flags.DurationVar(&cfg.poll, "poll", cfg.poll, "poll interval")
	flags.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "maximum wait")
	flags.StringVar(&contractPath, "contract", "", "strict JSON workflow/job contract")
	flags.BoolVar(&historical, "historical", false, "resolve historical runs without current workflow activation")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected positional arguments: %s", quoteStrings(flags.Args()))
	}
	visited := make(map[string]struct{})
	flags.Visit(func(current *flag.Flag) {
		visited[current.Name] = struct{}{}
	})
	if contractPath != "" {
		for _, incompatible := range []string{"repo", "workflow", "workflow-name", "job"} {
			if _, exists := visited[incompatible]; exists {
				return config{}, fmt.Errorf("-%s cannot be combined with -contract", incompatible)
			}
		}
		cfg.contractPath = contractPath
		cfg.historical = historical
		loaded, err := loadContractConfig(cfg)
		if err != nil {
			return config{}, err
		}
		return loaded, nil
	}
	if historical {
		return config{}, errors.New("-historical requires -contract")
	}
	cfg.requiredJobs = append([]string(nil), jobs...)
	if err := validateConfig(cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg config) error {
	if !repositoryPattern.MatchString(cfg.repository) {
		return fmt.Errorf("-repo must be an exact owner/name (got %q)", cfg.repository)
	}
	repositoryParts := strings.Split(cfg.repository, "/")
	if slices.Contains(repositoryParts, ".") || slices.Contains(repositoryParts, "..") {
		return fmt.Errorf("-repo contains an invalid path component")
	}
	if !workflowPattern.MatchString(cfg.workflow) {
		return fmt.Errorf("-workflow must be a .yml or .yaml filename (got %q)", cfg.workflow)
	}
	if err := validateText("-workflow-name", cfg.workflowName); err != nil {
		return err
	}
	if !shaPattern.MatchString(cfg.sha) {
		return fmt.Errorf("-sha must be an exact lowercase 40-character hexadecimal commit SHA")
	}
	if err := validateText("-branch", cfg.branch); err != nil {
		return err
	}
	if !eventPattern.MatchString(cfg.event) {
		return fmt.Errorf("-event contains unsupported characters (got %q)", cfg.event)
	}
	if len(cfg.requiredJobs) == 0 {
		return errors.New("at least one -job is required")
	}
	seen := make(map[string]struct{}, len(cfg.requiredJobs))
	for _, job := range cfg.requiredJobs {
		if err := validateText("-job", job); err != nil {
			return err
		}
		if _, exists := seen[job]; exists {
			return fmt.Errorf("duplicate required job %q", job)
		}
		seen[job] = struct{}{}
	}
	if cfg.poll <= 0 {
		return errors.New("-poll must be positive")
	}
	if cfg.timeout <= 0 {
		return errors.New("-timeout must be positive")
	}
	return nil
}

func validateText(flagName, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", flagName)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not have leading or trailing whitespace", flagName)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%s contains a control character", flagName)
		}
	}
	return nil
}

func waitForSuccess(
	ctx context.Context,
	cfg config,
	api apiClient,
	timer clock,
	progress io.Writer,
) (waitResult, error) {
	deadline := timer.now().Add(cfg.timeout)
	lastPending := ""
	for {
		remaining := deadline.Sub(timer.now())
		if remaining <= 0 {
			return waitResult{}, timeoutError(cfg, lastPending)
		}

		callCtx, cancel := context.WithTimeout(ctx, remaining)
		current, err := inspect(callCtx, cfg, api)
		callContextErr := callCtx.Err()
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return waitResult{}, ctx.Err()
			}
			if callContextErr != nil {
				return waitResult{}, timeoutError(cfg, lastPending)
			}
			if _, ok := errors.AsType[*retryableAPIError](err); ok {
				currentPending := "transient GitHub API failure; retrying: " + err.Error()
				if currentPending != lastPending {
					fmt.Fprintf(
						progress,
						"release-ci-wait: pending workflow=%s sha=%s: %s\n",
						cfg.workflow,
						cfg.sha,
						currentPending,
					)
					lastPending = currentPending
				}
				remaining = deadline.Sub(timer.now())
				if remaining <= 0 {
					return waitResult{}, timeoutError(cfg, lastPending)
				}
				if err := timer.sleep(ctx, min(cfg.poll, remaining)); err != nil {
					return waitResult{}, err
				}
				continue
			}
			return waitResult{}, err
		}
		if current.pending == "" {
			return current.result, nil
		}

		if current.pending != lastPending {
			fmt.Fprintf(progress, "release-ci-wait: pending workflow=%s sha=%s: %s\n", cfg.workflow, cfg.sha, current.pending)
			lastPending = current.pending
		}

		remaining = deadline.Sub(timer.now())
		if remaining <= 0 {
			return waitResult{}, timeoutError(cfg, lastPending)
		}
		sleepFor := min(cfg.poll, remaining)
		if err := timer.sleep(ctx, sleepFor); err != nil {
			return waitResult{}, err
		}
	}
}

func timeoutError(cfg config, pending string) error {
	if pending == "" {
		pending = "no successful observation"
	}
	return fmt.Errorf(
		"timed out after %s waiting for workflow %s at %s (%s)",
		cfg.timeout,
		cfg.workflow,
		cfg.sha,
		pending,
	)
}

func inspect(ctx context.Context, cfg config, api apiClient) (observation, error) {
	firstWorkflow, err := resolveWorkflow(ctx, cfg, api)
	if err != nil {
		return observation{}, err
	}
	firstRun, pending, err := resolveRun(ctx, cfg, firstWorkflow, api)
	if err != nil {
		return observation{}, err
	}
	if pending != "" {
		return observation{pending: pending}, nil
	}

	jobs, err := listJobs(ctx, cfg, firstRun, api)
	if err != nil {
		return observation{}, err
	}

	// Re-resolve the workflow and run after collecting every jobs page. This
	// prevents a partial rerun from producing a mixed-attempt success snapshot.
	secondWorkflow, err := resolveWorkflow(ctx, cfg, api)
	if err != nil {
		return observation{}, err
	}
	if secondWorkflow.ID != firstWorkflow.ID {
		return observation{}, fmt.Errorf(
			"workflow identity changed during inspection: %d -> %d",
			firstWorkflow.ID,
			secondWorkflow.ID,
		)
	}
	secondRun, pending, err := resolveRun(ctx, cfg, secondWorkflow, api)
	if err != nil {
		return observation{}, err
	}
	if pending != "" {
		return observation{pending: "run changed during inspection: " + pending}, nil
	}
	if secondRun.ID != firstRun.ID {
		return observation{}, fmt.Errorf(
			"exact target changed run ID during inspection: %d -> %d",
			firstRun.ID,
			secondRun.ID,
		)
	}
	if secondRun.RunAttempt != firstRun.RunAttempt ||
		secondRun.Status != firstRun.Status ||
		!equalOptionalString(secondRun.Conclusion, firstRun.Conclusion) {
		return observation{
			pending: fmt.Sprintf(
				"run %d changed during inspection (attempt %d -> %d, status %s -> %s)",
				firstRun.ID,
				firstRun.RunAttempt,
				secondRun.RunAttempt,
				firstRun.Status,
				secondRun.Status,
			),
		}, nil
	}

	jobAttempts, pending, err := evaluateJobs(cfg, secondRun, jobs)
	if err != nil {
		return observation{}, err
	}
	if pending != "" {
		return observation{pending: pending}, nil
	}
	return observation{
		result: waitResult{
			WorkflowID: secondWorkflow.ID,
			RunID:      secondRun.ID,
			RunAttempt: secondRun.RunAttempt,
			JobAttempt: jobAttempts,
		},
	}, nil
}

func resolveWorkflow(ctx context.Context, cfg config, api apiClient) (workflowRecord, error) {
	endpoint := fmt.Sprintf("repos/%s/actions/workflows", cfg.repository)
	workflows, err := collectPages(
		ctx,
		api,
		endpoint,
		nil,
		"workflows",
		0,
		func(raw []byte) (*int, *[]workflowRecord, error) {
			var payload struct {
				TotalCount *int              `json:"total_count"`
				Workflows  *[]workflowRecord `json:"workflows"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return nil, nil, err
			}
			return payload.TotalCount, payload.Workflows, nil
		},
	)
	if err != nil {
		return workflowRecord{}, err
	}

	expectedPath := ".github/workflows/" + cfg.workflow
	seenIDs := make(map[int64]struct{}, len(workflows))
	var matches []workflowRecord
	for _, workflow := range workflows {
		if workflow.ID <= 0 || workflow.Name == "" || workflow.Path == "" || workflow.State == "" {
			return workflowRecord{}, fmt.Errorf("malformed workflow record: required field is missing")
		}
		if _, exists := seenIDs[workflow.ID]; exists {
			return workflowRecord{}, fmt.Errorf("duplicate workflow ID %d", workflow.ID)
		}
		seenIDs[workflow.ID] = struct{}{}
		if workflow.Path == expectedPath {
			matches = append(matches, workflow)
		}
	}
	if len(matches) != 1 {
		return workflowRecord{}, fmt.Errorf(
			"expected exactly one workflow at %s, found %d",
			expectedPath,
			len(matches),
		)
	}
	selected := matches[0]
	if selected.Name != cfg.workflowName {
		return workflowRecord{}, fmt.Errorf(
			"workflow %s name is %q, want exact %q",
			expectedPath,
			selected.Name,
			cfg.workflowName,
		)
	}
	if selected.State != "active" {
		return workflowRecord{}, fmt.Errorf(
			"workflow %s state is %q, want active",
			expectedPath,
			selected.State,
		)
	}
	return selected, nil
}

func resolveRun(
	ctx context.Context,
	cfg config,
	workflow workflowRecord,
	api apiClient,
) (workflowRun, string, error) {
	endpoint := fmt.Sprintf(
		"repos/%s/actions/workflows/%d/runs",
		cfg.repository,
		workflow.ID,
	)
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
		"workflow runs",
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
		return workflowRun{}, "", err
	}
	if len(runs) == 0 {
		return workflowRun{}, "exact workflow run has not appeared", nil
	}

	byID := make(map[int64]map[int]workflowRun)
	for _, run := range runs {
		if err := validateRunRecord(cfg, workflow, run); err != nil {
			return workflowRun{}, "", err
		}
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
			"run %d attempt %d is %s",
			selected.ID,
			selected.RunAttempt,
			selected.Status,
		), nil
	}
	return selected, "", nil
}

func validateRunRecord(cfg config, workflow workflowRecord, run workflowRun) error {
	if run.ID <= 0 || run.RunAttempt <= 0 {
		return errors.New("malformed workflow run: ID and run_attempt must be positive")
	}
	if run.WorkflowID != workflow.ID {
		return fmt.Errorf(
			"workflow run %d workflow_id=%d, want %d",
			run.ID,
			run.WorkflowID,
			workflow.ID,
		)
	}
	if run.Name != cfg.workflowName {
		return fmt.Errorf(
			"workflow run %d name=%q, want exact %q",
			run.ID,
			run.Name,
			cfg.workflowName,
		)
	}
	if !matchesWorkflowPath(run.Path, cfg.workflow) {
		return fmt.Errorf(
			"workflow run %d path=%q, want exact .github/workflows/%s",
			run.ID,
			run.Path,
			cfg.workflow,
		)
	}
	if run.HeadSHA != cfg.sha {
		return fmt.Errorf(
			"workflow run %d head_sha=%q, want exact %q",
			run.ID,
			run.HeadSHA,
			cfg.sha,
		)
	}
	if run.HeadBranch != cfg.branch {
		return fmt.Errorf(
			"workflow run %d head_branch=%q, want exact %q",
			run.ID,
			run.HeadBranch,
			cfg.branch,
		)
	}
	if run.Event != cfg.event {
		return fmt.Errorf(
			"workflow run %d event=%q, want exact %q",
			run.ID,
			run.Event,
			cfg.event,
		)
	}
	if err := validateStateSyntax("workflow run", run.Status, run.Conclusion); err != nil {
		return fmt.Errorf("workflow run %d: %w", run.ID, err)
	}
	return nil
}

func matchesWorkflowPath(rawPath, workflow string) bool {
	basePath, _, _ := strings.Cut(rawPath, "@")
	return basePath == ".github/workflows/"+workflow
}

func listJobs(
	ctx context.Context,
	cfg config,
	run workflowRun,
	api apiClient,
) ([]workflowJob, error) {
	endpoint := fmt.Sprintf(
		"repos/%s/actions/runs/%d/jobs",
		cfg.repository,
		run.ID,
	)
	query := url.Values{"filter": {"all"}}
	return collectPages(
		ctx,
		api,
		endpoint,
		query,
		"workflow jobs",
		0,
		func(raw []byte) (*int, *[]workflowJob, error) {
			var payload struct {
				TotalCount *int           `json:"total_count"`
				Jobs       *[]workflowJob `json:"jobs"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return nil, nil, err
			}
			return payload.TotalCount, payload.Jobs, nil
		},
	)
}

func evaluateJobs(
	cfg config,
	run workflowRun,
	jobs []workflowJob,
) (map[string]int, string, error) {
	seenIDs := make(map[int64]struct{}, len(jobs))
	byName := make(map[string]map[int]workflowJob)
	for _, job := range jobs {
		if job.ID <= 0 || job.RunID <= 0 || job.RunAttempt <= 0 || job.Name == "" {
			return nil, "", errors.New("malformed workflow job: required field is missing")
		}
		if job.RunID != run.ID {
			return nil, "", fmt.Errorf(
				"workflow job %d run_id=%d, want %d",
				job.ID,
				job.RunID,
				run.ID,
			)
		}
		if job.RunAttempt > run.RunAttempt {
			return nil, "", fmt.Errorf(
				"workflow job %d attempt=%d is newer than stable run attempt %d",
				job.ID,
				job.RunAttempt,
				run.RunAttempt,
			)
		}
		if job.HeadSHA != cfg.sha {
			return nil, "", fmt.Errorf(
				"workflow job %d head_sha=%q, want exact %q",
				job.ID,
				job.HeadSHA,
				cfg.sha,
			)
		}
		if job.HeadBranch != cfg.branch {
			return nil, "", fmt.Errorf(
				"workflow job %d head_branch=%q, want exact %q",
				job.ID,
				job.HeadBranch,
				cfg.branch,
			)
		}
		if job.WorkflowName != cfg.workflowName {
			return nil, "", fmt.Errorf(
				"workflow job %d workflow_name=%q, want exact %q",
				job.ID,
				job.WorkflowName,
				cfg.workflowName,
			)
		}
		if err := validateStateSyntax("workflow job", job.Status, job.Conclusion); err != nil {
			return nil, "", fmt.Errorf("workflow job %d: %w", job.ID, err)
		}
		if _, exists := seenIDs[job.ID]; exists {
			return nil, "", fmt.Errorf("duplicate workflow job ID %d", job.ID)
		}
		seenIDs[job.ID] = struct{}{}

		attempts := byName[job.Name]
		if attempts == nil {
			attempts = make(map[int]workflowJob)
			byName[job.Name] = attempts
		}
		if _, exists := attempts[job.RunAttempt]; exists {
			return nil, "", fmt.Errorf(
				"duplicate workflow job name %q at attempt %d",
				job.Name,
				job.RunAttempt,
			)
		}
		attempts[job.RunAttempt] = job
	}

	selected := make(map[string]workflowJob, len(byName))
	for name, attempts := range byName {
		var latest workflowJob
		for attempt, job := range attempts {
			if attempt > latest.RunAttempt {
				latest = job
			}
		}
		selected[name] = latest
	}

	required := make(map[string]struct{}, len(cfg.requiredJobs))
	for _, name := range cfg.requiredJobs {
		required[name] = struct{}{}
	}
	var missing []string
	for name := range required {
		if _, exists := selected[name]; !exists {
			missing = append(missing, name)
		}
	}
	var extra []string
	for name := range selected {
		if _, exists := required[name]; !exists {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		return nil, "", fmt.Errorf(
			"exact workflow job set mismatch: missing=%s extra=%s",
			quoteStrings(missing),
			quoteStrings(extra),
		)
	}

	jobAttempts := make(map[string]int, len(selected))
	var pending []string
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		job := selected[name]
		state, err := classifyState("workflow job "+strconv.Quote(name), job.Status, job.Conclusion)
		if err != nil {
			return nil, "", err
		}
		if state == statePending {
			pending = append(
				pending,
				fmt.Sprintf(
					"%s(attempt=%d,status=%q)",
					strconv.QuoteToASCII(name),
					job.RunAttempt,
					job.Status,
				),
			)
		}
		jobAttempts[name] = job.RunAttempt
	}
	if len(pending) > 0 {
		return nil, "required jobs pending: " + strings.Join(pending, ", "), nil
	}
	return jobAttempts, "", nil
}

type stateClass int

const (
	statePending stateClass = iota
	stateSuccess
)

func validateStateSyntax(label, status string, conclusion *string) error {
	switch status {
	case "queued", "in_progress", "pending", "requested", "waiting":
		if conclusion != nil {
			return fmt.Errorf("%s status=%q has non-null conclusion=%q", label, status, *conclusion)
		}
		return nil
	case "completed":
		if conclusion == nil || *conclusion == "" {
			return fmt.Errorf("%s completed without a conclusion", label)
		}
		switch *conclusion {
		case "action_required", "cancelled", "failure", "neutral", "skipped",
			"stale", "startup_failure", "success", "timed_out":
			return nil
		default:
			return fmt.Errorf("%s has unsupported conclusion=%q", label, *conclusion)
		}
	default:
		return fmt.Errorf("%s has unsupported status=%q", label, status)
	}
}

func classifyState(label, status string, conclusion *string) (stateClass, error) {
	if err := validateStateSyntax(label, status, conclusion); err != nil {
		return statePending, err
	}
	if status != "completed" {
		return statePending, nil
	}
	if *conclusion != "success" {
		return statePending, fmt.Errorf(
			"%s completed with terminal conclusion=%q",
			label,
			*conclusion,
		)
	}
	return stateSuccess, nil
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func collectPages[T any](
	ctx context.Context,
	api apiClient,
	endpoint string,
	baseQuery url.Values,
	label string,
	maxTotal int,
	decode func([]byte) (*int, *[]T, error),
) ([]T, error) {
	expectedTotal := -1
	var collected []T
	for page := 1; ; page++ {
		query := cloneValues(baseQuery)
		query.Set("per_page", strconv.Itoa(apiPageSize))
		query.Set("page", strconv.Itoa(page))
		raw, err := api.get(ctx, endpoint, query)
		if err != nil {
			return nil, fmt.Errorf("list %s page %d: %w", label, page, err)
		}
		total, items, err := decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s page %d: %w", label, page, err)
		}
		if total == nil || items == nil {
			return nil, fmt.Errorf("decode %s page %d: total_count or item array is missing", label, page)
		}
		if *total < 0 {
			return nil, fmt.Errorf("decode %s page %d: negative total_count", label, page)
		}
		if maxTotal > 0 && *total > maxTotal {
			return nil, fmt.Errorf(
				"list %s: total_count=%d exceeds complete-search limit %d",
				label,
				*total,
				maxTotal,
			)
		}
		if expectedTotal == -1 {
			expectedTotal = *total
		} else if *total != expectedTotal {
			return nil, fmt.Errorf(
				"list %s: total_count changed across pages from %d to %d",
				label,
				expectedTotal,
				*total,
			)
		}
		if len(*items) > apiPageSize {
			return nil, fmt.Errorf(
				"decode %s page %d: got %d items with per_page=%d",
				label,
				page,
				len(*items),
				apiPageSize,
			)
		}
		collected = append(collected, (*items)...)
		if len(collected) > expectedTotal {
			return nil, fmt.Errorf(
				"list %s: received %d items, more than total_count=%d",
				label,
				len(collected),
				expectedTotal,
			)
		}
		if len(collected) == expectedTotal {
			return collected, nil
		}
		if len(*items) == 0 || len(*items) < apiPageSize {
			return nil, fmt.Errorf(
				"list %s: pagination ended at %d of total_count=%d",
				label,
				len(collected),
				expectedTotal,
			)
		}
	}
}

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, entries := range values {
		cloned[key] = append([]string(nil), entries...)
	}
	return cloned
}
