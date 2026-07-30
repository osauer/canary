package main

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

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
		race:        true,
		tags:        "trading",
		timeout:     240 * time.Second,
	}, driver, &stdout, &stderr)
	if code != 23 {
		t.Fatalf("execute exit = %d, want child exit 23; stderr=%s", code, stderr.String())
	}
	if len(driver.patterns) != 2 {
		t.Fatalf("executed %d shards, want stop after shard 2", len(driver.patterns))
	}
}

func TestExecutePropagatesInventoryFailure(t *testing.T) {
	t.Parallel()
	driver := &fakeDriver{listCode: 19, listErr: errors.New("compile failed")}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(config{packagePath: "./internal/daemon", shards: 4}, driver, &stdout, &stderr)
	if code != 19 {
		t.Fatalf("execute exit = %d, want inventory exit 19", code)
	}
	if len(driver.patterns) != 0 {
		t.Fatalf("executed shard patterns after inventory failure: %v", driver.patterns)
	}
}

type fakeDriver struct {
	inventory []byte
	listCode  int
	listErr   error
	failAt    int
	failCode  int
	patterns  []string
}

func (f *fakeDriver) list(config) ([]byte, int, error) {
	return f.inventory, f.listCode, f.listErr
}

func (f *fakeDriver) run(_ config, pattern string, _, _ io.Writer) (int, error) {
	f.patterns = append(f.patterns, pattern)
	if f.failAt > 0 && len(f.patterns) == f.failAt {
		return f.failCode, errors.New("fixture shard failure")
	}
	return 0, nil
}
