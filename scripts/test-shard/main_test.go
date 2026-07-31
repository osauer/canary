package main

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseConfigValidatesWorkerBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name: "valid bounded pool",
			args: []string{"-package", "./internal/daemon", "-shards", "4", "-workers", "2"},
		},
		{
			name:    "zero workers",
			args:    []string{"-package", "./internal/daemon", "-shards", "4", "-workers", "0"},
			wantErr: "-workers must be at least 1",
		},
		{
			name:    "workers exceed shards",
			args:    []string{"-package", "./internal/daemon", "-shards", "4", "-workers", "5"},
			wantErr: "-workers must not exceed -shards",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			cfg, err := parseConfig(tc.args, &stderr)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parseConfig: %v", err)
				}
				if cfg.workers != 2 {
					t.Fatalf("workers = %d, want 2", cfg.workers)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parseConfig error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseInventorySortsRunnableNamesAndIgnoresGoStatus(t *testing.T) {
	t.Parallel()
	got, err := parseInventory([]byte(strings.Join([]string{
		"TestZulu",
		"ExampleAlpha",
		"FuzzBeta",
		"ok  example.test/package  0.01s",
		"",
	}, "\n")))
	if err != nil {
		t.Fatalf("parseInventory: %v", err)
	}
	want := []string{"ExampleAlpha", "FuzzBeta", "TestZulu"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory = %v, want %v", got, want)
	}
}

func TestParseInventoryFailsClosedOnEmptyMalformedOrDuplicateOutput(t *testing.T) {
	t.Parallel()
	for name, output := range map[string]string{
		"empty":     "ok  example.test/package  0.01s\n",
		"malformed": "TestBroken extra-field\n",
		"duplicate": "TestSame\nTestSame\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseInventory([]byte(output)); err == nil {
				t.Fatalf("parseInventory(%q) succeeded, want fail-closed error", output)
			}
		})
	}
}

func TestMakeShardPlanIsDeterministicBalancedAndExact(t *testing.T) {
	t.Parallel()
	inventory := []string{
		"TestHotel",
		"TestAlpha",
		"TestGolf",
		"TestBravo",
		"TestFoxtrot",
		"TestCharlie",
		"TestEcho",
		"TestDelta",
	}
	got, err := makeShardPlan(inventory, 3)
	if err != nil {
		t.Fatalf("makeShardPlan: %v", err)
	}
	want := [][]string{
		{"TestAlpha", "TestDelta", "TestGolf"},
		{"TestBravo", "TestEcho", "TestHotel"},
		{"TestCharlie", "TestFoxtrot"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	if err := validateShardPlan(inventory, got); err != nil {
		t.Fatalf("validate exact plan: %v", err)
	}
}

func TestValidateShardPlanRejectsGapsOverlapUnknownsAndEmptyShards(t *testing.T) {
	t.Parallel()
	inventory := []string{"TestAlpha", "TestBravo", "TestCharlie"}
	cases := map[string][][]string{
		"gap":         {{"TestAlpha"}, {"TestBravo"}},
		"overlap":     {{"TestAlpha", "TestBravo"}, {"TestBravo", "TestCharlie"}},
		"unknown":     {{"TestAlpha", "TestBravo"}, {"TestCharlie", "TestDelta"}},
		"empty shard": {{"TestAlpha", "TestBravo", "TestCharlie"}, {}},
	}
	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateShardPlan(inventory, plan); err == nil {
				t.Fatalf("validateShardPlan(%v) succeeded, want error", plan)
			}
		})
	}
}

func TestShardPatternSelectsOnlyTopLevelParents(t *testing.T) {
	t.Parallel()
	pattern := shardPattern([]string{"TestParent", "ExampleFlow"})
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("Compile(%q): %v", pattern, err)
	}
	if !compiled.MatchString("TestParent") || !compiled.MatchString("ExampleFlow") {
		t.Fatalf("pattern %q does not select its top-level inventory", pattern)
	}
	if compiled.MatchString("TestOther") || compiled.MatchString("TestParent/child") {
		t.Fatalf("pattern %q escaped its top-level shard", pattern)
	}
}

func TestExecuteStopsAfterFailureAndPreservesChildExitCode(t *testing.T) {
	t.Parallel()
	driver := &fakeDriver{
		inventory: []byte("TestAlpha\nTestBravo\nTestCharlie\nTestDelta\n"),
		failAt:    2,
		failCode:  23,
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(config{
		packagePath: "./internal/daemon",
		shards:      4,
		workers:     1,
		race:        true,
		tags:        "trading",
		timeout:     240 * time.Second,
	}, driver, &stdout, &stderr)
	if code != 23 {
		t.Fatalf("execute exit = %d, want child exit 23; stderr=%s", code, stderr.String())
	}
	if got := driver.patternSnapshot(); len(got) != 2 {
		t.Fatalf("executed %d shards, want stop after shard 2", len(got))
	}
}

func TestExecuteBoundsParallelismAndRunsEveryShardOnce(t *testing.T) {
	t.Parallel()
	driver := &fakeDriver{
		inventory: []byte("TestAlpha\nTestBravo\nTestCharlie\nTestDelta\nTestEcho\nTestFoxtrot\n"),
		delay:     20 * time.Millisecond,
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(config{
		packagePath: "./internal/daemon",
		shards:      6,
		workers:     2,
		timeout:     240 * time.Second,
	}, driver, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("execute exit = %d; stderr=%s", code, stderr.String())
	}
	patterns, maxActive := driver.snapshot()
	if maxActive != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", maxActive)
	}
	if len(patterns) != 6 {
		t.Fatalf("executed %d shards, want 6", len(patterns))
	}
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		if _, exists := seen[pattern]; exists {
			t.Fatalf("shard pattern ran more than once: %s", pattern)
		}
		seen[pattern] = struct{}{}
	}
}

func TestExecuteParallelFailureStopsDispatchAndUsesLowestShardExit(t *testing.T) {
	t.Parallel()
	driver := &fakeDriver{
		inventory: []byte("TestAlpha\nTestBravo\nTestCharlie\nTestDelta\n"),
		failures: map[string]int{
			"TestAlpha": 17,
			"TestBravo": 23,
		},
		delays: map[string]time.Duration{
			"TestAlpha": 40 * time.Millisecond,
			"TestBravo": 5 * time.Millisecond,
		},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(config{
		packagePath: "./internal/daemon",
		shards:      4,
		workers:     2,
		timeout:     240 * time.Second,
	}, driver, &stdout, &stderr)
	if code != 17 {
		t.Fatalf("execute exit = %d, want lowest-index shard exit 17; stderr=%s", code, stderr.String())
	}
	if got := driver.patternSnapshot(); len(got) != 2 {
		t.Fatalf("executed %d shards, want only the two already in flight: %v", len(got), got)
	}
}

func TestExecutePropagatesInventoryFailure(t *testing.T) {
	t.Parallel()
	driver := &fakeDriver{listCode: 19, listErr: errors.New("compile failed")}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(config{packagePath: "./internal/daemon", shards: 4, workers: 1}, driver, &stdout, &stderr)
	if code != 19 {
		t.Fatalf("execute exit = %d, want inventory exit 19", code)
	}
	if got := driver.patternSnapshot(); len(got) != 0 {
		t.Fatalf("executed shard patterns after inventory failure: %v", got)
	}
}

type fakeDriver struct {
	mu        sync.Mutex
	inventory []byte
	listCode  int
	listErr   error
	failAt    int
	failCode  int
	patterns  []string
	active    int
	maxActive int
	delay     time.Duration
	delays    map[string]time.Duration
	failures  map[string]int
}

func (f *fakeDriver) list(config) ([]byte, int, error) {
	return f.inventory, f.listCode, f.listErr
}

func (f *fakeDriver) run(_ config, pattern string, _, _ io.Writer) (int, error) {
	f.mu.Lock()
	f.patterns = append(f.patterns, pattern)
	runNumber := len(f.patterns)
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	delay := f.delay
	failCode := 0
	for name, candidateDelay := range f.delays {
		if strings.Contains(pattern, name) {
			delay = candidateDelay
		}
	}
	for name, candidateCode := range f.failures {
		if strings.Contains(pattern, name) {
			failCode = candidateCode
		}
	}
	if f.failAt > 0 && runNumber == f.failAt {
		failCode = f.failCode
	}
	f.mu.Unlock()

	time.Sleep(delay)

	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	if failCode != 0 {
		return failCode, errors.New("fixture shard failure")
	}
	return 0, nil
}

func (f *fakeDriver) patternSnapshot() []string {
	patterns, _ := f.snapshot()
	return patterns
}

func (f *fakeDriver) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.patterns...), f.maxActive
}
