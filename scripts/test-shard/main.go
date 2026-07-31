// Command test-shard runs one Go package's complete test inventory in a
// deterministic set of bounded parallel processes. It is intentionally
// package scoped: packages are already Go's normal unit of isolation, and only
// an oversized package should need another boundary.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/token"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const inventoryPattern = `^(Test|Example|Fuzz)`

type config struct {
	packagePath string
	shards      int
	workers     int
	race        bool
	tags        string
	timeout     time.Duration
}

type testDriver interface {
	list(config) ([]byte, int, error)
	run(config, string, io.Writer, io.Writer) (int, error)
}

type goTestDriver struct{}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "test-shard: %v\n", err)
		return 2
	}
	return execute(cfg, goTestDriver{}, stdout, stderr)
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("test-shard", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.packagePath, "package", "", "exact Go package to test")
	flags.IntVar(&cfg.shards, "shards", 4, "number of test processes")
	flags.IntVar(&cfg.workers, "workers", 1, "maximum test processes to run concurrently")
	flags.BoolVar(&cfg.race, "race", false, "enable the Go race detector")
	flags.StringVar(&cfg.tags, "tags", "", "comma-separated Go build tags")
	flags.DurationVar(&cfg.timeout, "timeout", 240*time.Second, "timeout for each shard")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(cfg.packagePath) == "" {
		return config{}, errors.New("-package is required")
	}
	if cfg.shards < 1 {
		return config{}, errors.New("-shards must be at least 1")
	}
	if cfg.workers < 1 {
		return config{}, errors.New("-workers must be at least 1")
	}
	if cfg.workers > cfg.shards {
		return config{}, errors.New("-workers must not exceed -shards")
	}
	if cfg.timeout <= 0 {
		return config{}, errors.New("-timeout must be positive")
	}
	cfg.tags = strings.TrimSpace(cfg.tags)
	return cfg, nil
}

func execute(cfg config, driver testDriver, stdout, stderr io.Writer) int {
	started := time.Now()
	rawInventory, code, err := driver.list(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "test-shard: inventory failed: %v\n", err)
		return normalizedExitCode(code)
	}
	if code != 0 {
		fmt.Fprintf(stderr, "test-shard: inventory exited with status %d\n", code)
		return normalizedExitCode(code)
	}

	inventory, err := parseInventory(rawInventory)
	if err != nil {
		fmt.Fprintf(stderr, "test-shard: invalid inventory: %v\n", err)
		return 2
	}
	plan, err := makeShardPlan(inventory, cfg.shards)
	if err != nil {
		fmt.Fprintf(stderr, "test-shard: invalid shard plan: %v\n", err)
		return 2
	}

	sizes := make([]string, 0, len(plan))
	for _, shard := range plan {
		sizes = append(sizes, fmt.Sprintf("%d", len(shard)))
	}
	fmt.Fprintf(
		stdout,
		"test-shard: %s inventory=%d shards=%d workers=%d sizes=%s\n",
		cfg.packagePath,
		len(inventory),
		len(plan),
		cfg.workers,
		strings.Join(sizes, ","),
	)

	type shardResult struct {
		index   int
		code    int
		err     error
		elapsed time.Duration
		stdout  bytes.Buffer
		stderr  bytes.Buffer
	}
	results := make(chan shardResult, cfg.workers)
	next := 0
	active := 0
	failed := false
	failureIndex := len(plan)
	failureCode := 0

	launch := func(index int) {
		shard := plan[index]
		pattern := shardPattern(shard)
		fmt.Fprintf(stdout, "test-shard: starting shard %d/%d (%d tests)\n", index+1, len(plan), len(shard))
		active++
		go func() {
			var result shardResult
			result.index = index
			shardStarted := time.Now()
			result.code, result.err = driver.run(cfg, pattern, &result.stdout, &result.stderr)
			result.elapsed = time.Since(shardStarted)
			results <- result
		}()
	}

	for next < len(plan) && active < cfg.workers {
		launch(next)
		next++
	}
	for active > 0 {
		result := <-results
		active--
		if result.stdout.Len() > 0 {
			fmt.Fprintf(stdout, "test-shard: stdout shard %d/%d\n", result.index+1, len(plan))
			_, _ = io.Copy(stdout, &result.stdout)
		}
		if result.stderr.Len() > 0 {
			fmt.Fprintf(stderr, "test-shard: stderr shard %d/%d\n", result.index+1, len(plan))
			_, _ = io.Copy(stderr, &result.stderr)
		}
		switch {
		case result.err != nil:
			fmt.Fprintf(stderr, "test-shard: shard %d/%d failed: %v\n", result.index+1, len(plan), result.err)
			if result.index < failureIndex {
				failureIndex = result.index
				failureCode = normalizedExitCode(result.code)
			}
			failed = true
		case result.code != 0:
			fmt.Fprintf(stderr, "test-shard: shard %d/%d exited with status %d\n", result.index+1, len(plan), result.code)
			if result.index < failureIndex {
				failureIndex = result.index
				failureCode = normalizedExitCode(result.code)
			}
			failed = true
		default:
			fmt.Fprintf(stdout, "test-shard: completed shard %d/%d elapsed=%s\n", result.index+1, len(plan), result.elapsed.Round(time.Millisecond))
		}
		if !failed && next < len(plan) {
			launch(next)
			next++
		}
	}
	if failed {
		fmt.Fprintf(stderr, "test-shard: failed elapsed=%s\n", time.Since(started).Round(time.Millisecond))
		return failureCode
	}
	fmt.Fprintf(stdout, "test-shard: PASS elapsed=%s\n", time.Since(started).Round(time.Millisecond))
	return 0
}

func parseInventory(output []byte) ([]string, error) {
	var names []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		candidate := fields[0]
		if !hasRunnablePrefix(candidate) {
			continue
		}
		if len(fields) != 1 || !token.IsIdentifier(candidate) {
			return nil, fmt.Errorf("malformed runnable name %q", line)
		}
		names = append(names, candidate)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read go test inventory: %w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("go test -list returned no tests, examples, or fuzz targets")
	}
	sort.Strings(names)
	for index := 1; index < len(names); index++ {
		if names[index] == names[index-1] {
			return nil, fmt.Errorf("duplicate runnable name %q", names[index])
		}
	}
	return names, nil
}

func hasRunnablePrefix(name string) bool {
	return strings.HasPrefix(name, "Test") ||
		strings.HasPrefix(name, "Example") ||
		strings.HasPrefix(name, "Fuzz")
}

func makeShardPlan(inventory []string, shardCount int) ([][]string, error) {
	if shardCount < 1 {
		return nil, errors.New("shard count must be at least 1")
	}
	if len(inventory) < shardCount {
		return nil, fmt.Errorf("inventory has %d entries for %d shards", len(inventory), shardCount)
	}

	sorted := append([]string(nil), inventory...)
	sort.Strings(sorted)
	plan := make([][]string, shardCount)
	for index, name := range sorted {
		plan[index%shardCount] = append(plan[index%shardCount], name)
	}
	if err := validateShardPlan(sorted, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func validateShardPlan(inventory []string, plan [][]string) error {
	if len(inventory) == 0 {
		return errors.New("inventory is empty")
	}
	expected := make(map[string]struct{}, len(inventory))
	for _, name := range inventory {
		if _, exists := expected[name]; exists {
			return fmt.Errorf("inventory contains duplicate %q", name)
		}
		expected[name] = struct{}{}
	}

	assigned := make(map[string]int, len(inventory))
	for index, shard := range plan {
		if len(shard) == 0 {
			return fmt.Errorf("shard %d is empty", index+1)
		}
		for _, name := range shard {
			if _, exists := expected[name]; !exists {
				return fmt.Errorf("shard %d contains unknown runnable %q", index+1, name)
			}
			assigned[name]++
			if assigned[name] > 1 {
				return fmt.Errorf("runnable %q is assigned more than once", name)
			}
		}
	}
	for _, name := range inventory {
		if assigned[name] != 1 {
			return fmt.Errorf("runnable %q is not assigned exactly once", name)
		}
	}
	return nil
}

func shardPattern(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, regexp.QuoteMeta(name))
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

func (goTestDriver) list(cfg config) ([]byte, int, error) {
	args := commonGoTestArgs(cfg)
	args = append(args, "-timeout="+cfg.timeout.String(), "-list="+inventoryPattern, cfg.packagePath)
	cmd := exec.Command("go", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return stdout.Bytes(), commandExitCode(err), errors.New(detail)
	}
	return stdout.Bytes(), 0, nil
}

func (goTestDriver) run(cfg config, pattern string, stdout, stderr io.Writer) (int, error) {
	args := commonGoTestArgs(cfg)
	args = append(args, "-json", "-timeout="+cfg.timeout.String(), "-run="+pattern, cfg.packagePath)
	cmd := exec.Command("go", args...)
	var rawStdout bytes.Buffer
	var rawStderr bytes.Buffer
	cmd.Stdout = &rawStdout
	cmd.Stderr = &rawStderr
	runErr := cmd.Run()
	summary, parseErr := parseGoTestSummary(rawStdout.Bytes())
	if runErr != nil || parseErr != nil {
		if summary.output != "" {
			_, _ = io.WriteString(stdout, summary.output)
		} else {
			_, _ = stdout.Write(rawStdout.Bytes())
		}
		_, _ = stderr.Write(rawStderr.Bytes())
		if runErr != nil {
			return commandExitCode(runErr), runErr
		}
		return 1, parseErr
	}
	if rawStderr.Len() > 0 {
		_, _ = stderr.Write(rawStderr.Bytes())
	}
	fmt.Fprintf(stdout, "go test: %s\n", summary.telemetry())
	if len(summary.skippedNames) > 0 {
		const maxReportedSkips = 12
		reported := summary.skippedNames
		if len(reported) > maxReportedSkips {
			reported = reported[:maxReportedSkips]
		}
		fmt.Fprintf(stdout, "go test: skipped=%s", strings.Join(reported, ","))
		if len(summary.skippedNames) > len(reported) {
			fmt.Fprintf(stdout, " (+%d more)", len(summary.skippedNames)-len(reported))
		}
		fmt.Fprintln(stdout)
	}
	return 0, nil
}

func commonGoTestArgs(cfg config) []string {
	args := []string{"test"}
	if cfg.race {
		args = append(args, "-race")
	}
	if cfg.tags != "" {
		args = append(args, "-tags="+cfg.tags)
	}
	return args
}

func commandExitCode(err error) int {
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return normalizedExitCode(exitError.ExitCode())
	}
	return 1
}

func normalizedExitCode(code int) int {
	if code > 0 {
		return code
	}
	return 1
}
