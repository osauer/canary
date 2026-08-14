package app

import (
	"context"
	"errors"
	"testing"
	"time"

	apphttp "github.com/osauer/canary/v2/internal/app/http"
	selfupdate "github.com/osauer/canary/v2/internal/update"
)

func TestUpdateCoordinatorDoesNotCheckDevelopmentBuilds(t *testing.T) {
	t.Parallel()
	fetched := false
	c := newUpdateCoordinatorWithDeps(
		"v3.0.1-39-g7b62a7d4", "/tmp/canary",
		func(context.Context, string) (*selfupdate.Release, error) {
			fetched = true
			return &selfupdate.Release{TagName: "v3.0.1"}, nil
		},
		nil,
		time.Now,
	)
	status := c.Status()
	if fetched || status.State != apphttp.UpdateStateDevelopmentBuild || status.Available || status.Checking {
		t.Fatalf("development status=%+v fetched=%t", status, fetched)
	}
}

func TestUpdateCoordinatorChecksAndStartsOnlyDisplayedTarget(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	done := make(chan error, 1)
	started := ""
	c := newUpdateCoordinatorWithDeps(
		"v3.0.1", "/tmp/canary",
		func(context.Context, string) (*selfupdate.Release, error) {
			return &selfupdate.Release{TagName: "v3.0.2"}, nil
		},
		func(executable string) (<-chan error, error) {
			started = executable
			return done, nil
		},
		func() time.Time { return now },
	)
	if first := c.Status(); first.State != apphttp.UpdateStateChecking || !first.Checking {
		t.Fatalf("first status=%+v, want checking", first)
	}
	status := waitForUpdateState(t, c, apphttp.UpdateStateAvailable)
	if !status.Available || status.LatestVersion != "v3.0.2" {
		t.Fatalf("available status=%+v", status)
	}
	if _, err := c.Start("v3.0.3"); !errors.Is(err, apphttp.ErrUpdateConflict) {
		t.Fatalf("wrong target error=%v, want conflict", err)
	}
	status, err := c.Start("v3.0.2")
	if err != nil || status.State != apphttp.UpdateStateUpdating || started != "/tmp/canary" {
		t.Fatalf("start status=%+v executable=%q err=%v", status, started, err)
	}
	done <- errors.New("synthetic updater failure")
	close(done)
	status = waitForUpdateState(t, c, apphttp.UpdateStateFailed)
	if !status.Available || status.TargetVersion != "v3.0.2" {
		t.Fatalf("failed status=%+v", status)
	}
}

func waitForUpdateState(t *testing.T, c *updateCoordinator, want string) apphttp.UpdateStatusDTO {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		status := c.Status()
		if status.State == want && !status.Checking {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("update state=%+v, want %q", status, want)
		}
		time.Sleep(time.Millisecond)
	}
}
