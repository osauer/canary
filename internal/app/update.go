package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	apphttp "github.com/osauer/canary/v2/internal/app/http"
	selfupdate "github.com/osauer/canary/v2/internal/update"
)

const (
	updateCheckTTL       = 6 * time.Hour
	updateFailureRetry   = 15 * time.Minute
	updateRequestTimeout = 70 * time.Second
)

type updateReleaseFetcher func(context.Context, string) (*selfupdate.Release, error)
type updateProcessStarter func(string) (<-chan error, error)

// updateCoordinator is app-local, in-memory process state. The daemon remains
// uninvolved; installation and ordered stack restart stay in `canary update`.
type updateCoordinator struct {
	version    string
	executable string
	fetch      updateReleaseFetcher
	start      updateProcessStarter
	now        func() time.Time

	mu          sync.Mutex
	status      apphttp.UpdateStatusDTO
	lastAttempt time.Time
}

func newUpdateCoordinator(version string) *updateCoordinator {
	executable, _ := os.Executable()
	return newUpdateCoordinatorWithDeps(version, executable, selfupdate.FetchLatestRelease, startSelfUpdateProcess, time.Now)
}

func newUpdateCoordinatorWithDeps(version, executable string, fetch updateReleaseFetcher, start updateProcessStarter, now func() time.Time) *updateCoordinator {
	c := &updateCoordinator{
		version:    strings.TrimSpace(version),
		executable: strings.TrimSpace(executable),
		fetch:      fetch,
		start:      start,
		now:        now,
		status: apphttp.UpdateStatusDTO{
			SchemaVersion:  apphttp.UpdateStatusSchemaVersion,
			State:          apphttp.UpdateStateChecking,
			CurrentVersion: strings.TrimSpace(version),
		},
	}
	classification := selfupdate.EvaluateAvailability(version, version)
	switch classification.State {
	case selfupdate.AvailabilityDevelopmentBuild:
		c.status.State = apphttp.UpdateStateDevelopmentBuild
		c.status.Checking = false
		c.status.Message = "Development build; stable updates are not offered automatically."
	case selfupdate.AvailabilityInvalidVersion:
		c.status.State = apphttp.UpdateStateUnavailable
		c.status.Checking = false
		c.status.Message = "Installed version cannot be ordered against stable releases."
	}
	return c
}

// Status returns immediately. A stale read starts one detached metadata check;
// the SPA can poll this small resource without delaying bootstrap.
func (c *updateCoordinator) Status() apphttp.UpdateStatusDTO {
	c.mu.Lock()
	if c.shouldCheckLocked(c.now()) {
		c.status.Checking = true
		if c.status.CheckedAt.IsZero() {
			c.status.State = apphttp.UpdateStateChecking
		}
		c.lastAttempt = c.now().UTC()
		go c.refresh()
	}
	status := c.status
	c.mu.Unlock()
	return status
}

func (c *updateCoordinator) shouldCheckLocked(now time.Time) bool {
	if c.status.Checking || c.status.State == apphttp.UpdateStateUpdating || c.status.State == apphttp.UpdateStateDevelopmentBuild {
		return false
	}
	if c.lastAttempt.IsZero() {
		return true
	}
	ttl := updateCheckTTL
	if c.status.State == apphttp.UpdateStateUnavailable || c.status.State == apphttp.UpdateStateFailed {
		ttl = updateFailureRetry
	}
	return now.Sub(c.lastAttempt) >= ttl
}

func (c *updateCoordinator) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), updateRequestTimeout)
	defer cancel()
	release, err := c.fetch(ctx, c.version)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Checking = false
	c.status.CheckedAt = c.now().UTC()
	if err != nil {
		c.status.State = apphttp.UpdateStateUnavailable
		c.status.Available = false
		c.status.Message = "Update check unavailable; Canary will retry later."
		return
	}
	availability := selfupdate.EvaluateAvailability(c.version, release.TagName)
	c.status.LatestVersion = availability.LatestVersion
	c.status.Available = availability.Available
	c.status.Message = ""
	switch availability.State {
	case selfupdate.AvailabilityAvailable:
		c.status.State = apphttp.UpdateStateAvailable
	case selfupdate.AvailabilityDevelopmentBuild:
		c.status.State = apphttp.UpdateStateDevelopmentBuild
		c.status.Message = "Development build; stable updates are not offered automatically."
	case selfupdate.AvailabilityInvalidVersion:
		c.status.State = apphttp.UpdateStateUnavailable
		c.status.Message = "Installed version cannot be ordered against stable releases."
	case selfupdate.AvailabilityNewerInstalled:
		c.status.State = apphttp.UpdateStateCurrent
		c.status.Message = "Installed version is newer than the latest stable release."
	case selfupdate.AvailabilityDifferentMajor:
		c.status.State = apphttp.UpdateStateCurrent
		c.status.Message = "Stable updates do not cross major versions."
	default:
		c.status.State = apphttp.UpdateStateCurrent
	}
}

// Start accepts only the exact target currently displayed by the app. The
// updater still fetches release metadata again and applies its signed,
// same-major decision immediately before installation.
func (c *updateCoordinator) Start(targetVersion string) (apphttp.UpdateStatusDTO, error) {
	targetVersion = strings.TrimSpace(targetVersion)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.State == apphttp.UpdateStateUpdating {
		return c.status, fmt.Errorf("%w: an update is already running", apphttp.ErrUpdateConflict)
	}
	retryableFailure := c.status.State == apphttp.UpdateStateFailed && c.status.LatestVersion != ""
	if c.status.State != apphttp.UpdateStateAvailable && !retryableFailure {
		return c.status, fmt.Errorf("%w: no verified update is available", apphttp.ErrUpdateConflict)
	}
	if targetVersion == "" || targetVersion != c.status.LatestVersion {
		return c.status, fmt.Errorf("%w: update target changed; refresh and try again", apphttp.ErrUpdateConflict)
	}
	if c.executable == "" || c.start == nil {
		return c.status, errors.New("update process unavailable")
	}
	done, err := c.start(c.executable)
	if err != nil {
		return c.status, err
	}
	c.status.State = apphttp.UpdateStateUpdating
	c.status.Available = false
	c.status.TargetVersion = targetVersion
	c.status.Message = "Downloading, verifying, and restarting Canary."
	go c.awaitUpdate(done)
	return c.status, nil
}

func (c *updateCoordinator) awaitUpdate(done <-chan error) {
	err, ok := <-done
	if !ok {
		err = nil
	}
	// On the successful path the updater normally stops this app while it
	// restarts the stack, so this branch is primarily the pre-restart failure
	// surface. Keep raw process detail in the local log only.
	if err != nil {
		slog.Error("canary app update failed", "error", err)
		c.mu.Lock()
		c.status.State = apphttp.UpdateStateFailed
		c.status.Available = true
		c.status.Message = "Update failed before reconnect; retry here or run `canary update --restart` on the Mac."
		c.lastAttempt = c.now().UTC()
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.status.State = apphttp.UpdateStateChecking
	c.status.Checking = false
	c.status.Available = false
	c.status.TargetVersion = ""
	c.status.Message = ""
	c.lastAttempt = time.Time{}
	c.mu.Unlock()
	_ = c.Status()
}

func startSelfUpdateProcess(executable string) (<-chan error, error) {
	cmd := exec.Command(executable, "update", "--restart")
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start canary update: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	return done, nil
}
