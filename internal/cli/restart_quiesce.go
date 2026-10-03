package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/productidentity"
)

const (
	restartQuiesceMarkerVersion = 1
	// restartQuiesceMarkerName is the private record a stack restart leaves
	// in the app state directory while the launchd app job is booted out.
	restartQuiesceMarkerName = "restart-quiesced.json"
	// restartQuiesceMarkerOldAfter is a reporting threshold only: an old
	// record is still honored when its plist validates.
	restartQuiesceMarkerOldAfter = 14 * 24 * time.Hour
)

// restartQuiesceMarker records a launchd app supervisor that a stack restart
// booted out and could not resume because the daemon stage failed. A booted-
// out job is invisible to `launchctl print` and to process discovery, so
// without this record a rerun reports "no app was running" and the app stays
// down until a human bootstraps it by hand. The record carries the plist's
// own ProgramArguments and nothing else: no tokens, secret URLs or account
// data.
type restartQuiesceMarker struct {
	Version    int      `json:"version"`
	Target     string   `json:"target"`
	PlistPath  string   `json:"plist_path"`
	Args       []string `json:"args"`
	Executable string   `json:"executable"`
	QuiescedAt string   `json:"quiesced_at"`
}

// pendingSupervisorResume is a validated quiesce record: the launchd job to
// bootstrap again, rebuilt from its plist as it is on disk now.
type pendingSupervisorResume struct {
	supervisor appSupervisor
	quiescedAt time.Time
	migrate    bool // the plist still names the pre-upgrade executable
	path       string
}

func restartQuiesceMarkerPath() string {
	logPath := appRestartLogPath()
	if logPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(logPath), "app", restartQuiesceMarkerName)
}

func writeRestartQuiesceMarker(path string, sup appSupervisor, now time.Time) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("no app state directory for the quiesce record")
	}
	marker := restartQuiesceMarker{
		Version:    restartQuiesceMarkerVersion,
		Target:     sup.Target,
		PlistPath:  sup.PlistPath,
		Args:       slices.Clone(sup.Args),
		Executable: sup.Executable,
		QuiescedAt: now.UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quiesce record: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create app state directory: %w", err)
	}
	return writeFileAtomically(path, data, 0o600, "quiesce record", "app state directory")
}

// readRestartQuiesceMarker reports exists=false without an error when there
// is no record; exists=true with an error means a record that cannot be used.
func readRestartQuiesceMarker(path string) (marker restartQuiesceMarker, exists bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return restartQuiesceMarker{}, false, nil
		}
		return restartQuiesceMarker{}, false, err
	}
	if !info.Mode().IsRegular() {
		return restartQuiesceMarker{}, true, errors.New("quiesce record is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return restartQuiesceMarker{}, true, err
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return restartQuiesceMarker{}, true, fmt.Errorf("decode quiesce record: %w", err)
	}
	return marker, true, nil
}

func removeRestartQuiesceMarker(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// validateRestartQuiesceMarker rebuilds the supervisor from the plist as it
// is on disk, because the plist, not the record, is what launchd will run.
// It rejects a record whose plist is gone, is not the pinned app job, or
// names an executable this restart would refuse to supervise: an operator
// who booted the app out on purpose and removed its plist must not get it
// back.
func validateRestartQuiesceMarker(marker restartQuiesceMarker, currentPaths map[string]struct{}, canMigrate bool) (pendingSupervisorResume, error) {
	if marker.Version != restartQuiesceMarkerVersion {
		return pendingSupervisorResume{}, fmt.Errorf("unsupported record version %d", marker.Version)
	}
	sup := appSupervisor{Target: marker.Target, PlistPath: marker.PlistPath}
	if _, err := launchAgentDomain(sup); err != nil {
		return pendingSupervisorResume{}, err
	}
	quiescedAt, err := time.Parse(time.RFC3339, marker.QuiescedAt)
	if err != nil {
		return pendingSupervisorResume{}, fmt.Errorf("record has no valid quiesced_at: %w", err)
	}
	cleanPlist := filepath.Clean(sup.PlistPath)
	if filepath.Base(cleanPlist) != appLaunchAgentLabel+".plist" ||
		filepath.Base(filepath.Dir(cleanPlist)) != "LaunchAgents" ||
		filepath.Base(filepath.Dir(filepath.Dir(cleanPlist))) != "Library" {
		return pendingSupervisorResume{}, fmt.Errorf("plist path %q is not pinned to %s", sup.PlistPath, appLaunchAgentLabel)
	}
	info, err := os.Lstat(sup.PlistPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return pendingSupervisorResume{}, fmt.Errorf("plist %s no longer exists", sup.PlistPath)
		}
		return pendingSupervisorResume{}, fmt.Errorf("inspect launchd plist: %w", err)
	}
	if !info.Mode().IsRegular() {
		return pendingSupervisorResume{}, fmt.Errorf("launchd plist %q is not a regular file", sup.PlistPath)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return pendingSupervisorResume{}, fmt.Errorf("launchd plist %q is group/world writable", sup.PlistPath)
	}
	data, err := os.ReadFile(sup.PlistPath)
	if err != nil {
		return pendingSupervisorResume{}, fmt.Errorf("read launchd plist: %w", err)
	}
	program, err := parseLaunchAgentPlist(data)
	if err != nil {
		return pendingSupervisorResume{}, err
	}
	if !filepath.IsAbs(program.executable) {
		return pendingSupervisorResume{}, fmt.Errorf("plist executable %q is not an absolute path", program.executable)
	}
	if !isAppServerArgs(program.args) {
		return pendingSupervisorResume{}, errors.New("plist ProgramArguments is not a Canary app server command")
	}
	migrate := !executablePathMatches(program.executable, currentPaths)
	if migrate && (filepath.Base(program.executable) != productidentity.PreUpgradeExecutable || !canMigrate) {
		return pendingSupervisorResume{}, fmt.Errorf("plist points at %q, not the current installed Canary executable or the explicit pre-upgrade executable", program.executable)
	}
	sup.Executable = program.executable
	sup.Args = program.args
	return pendingSupervisorResume{supervisor: sup, quiescedAt: quiescedAt, migrate: migrate}, nil
}

// findPendingSupervisorResume reports a quiesce record whose launchd job can
// be bootstrapped again. A record that no longer validates is discarded with
// a one-line explanation. Without launchd adapters there is nothing to
// resume, so the record is left untouched.
func findPendingSupervisorResume(opts *restartOptions, deps appRestartDeps, prefix string) (pendingSupervisorResume, bool) {
	if deps.supervisor == nil || deps.load == nil {
		return pendingSupervisorResume{}, false
	}
	path := restartQuiesceMarkerPath()
	if path == "" {
		return pendingSupervisorResume{}, false
	}
	marker, exists, err := readRestartQuiesceMarker(path)
	if !exists {
		if err != nil {
			fmt.Fprintf(opts.err, "%s: inspect quiesce record %s: %v\n", prefix, path, err)
		}
		return pendingSupervisorResume{}, false
	}
	var pending pendingSupervisorResume
	if err == nil {
		pending, err = validateRestartQuiesceMarker(marker, restartExecutablePaths(deps), deps.rewrite != nil)
	}
	if err != nil {
		fmt.Fprintf(opts.err, "%s: discarding quiesce record %s: %v; the app stays stopped (reload its launchd job with `%s setup app` if it should run)\n", prefix, path, err, productidentity.Executable)
		if removeErr := removeRestartQuiesceMarker(path); removeErr != nil {
			fmt.Fprintf(opts.err, "%s: remove quiesce record: %v\n", prefix, removeErr)
		}
		return pendingSupervisorResume{}, false
	}
	pending.path = path
	when := pending.quiescedAt.Local().Format(time.RFC3339)
	if age := time.Since(pending.quiescedAt); age > restartQuiesceMarkerOldAfter {
		fmt.Fprintf(opts.err, "%s: quiesce record for %s is %d days old (quiesced %s); honoring it because its plist still validates\n", prefix, pending.supervisor.Target, int(age.Hours()/24), when)
	}
	if !opts.jsonOut {
		fmt.Fprintf(opts.out, "%s: app supervisor %s was quiesced by an earlier restart at %s and never resumed; resuming it\n", prefix, pending.supervisor.Target, when)
	}
	return pending, true
}

// discardStaleRestartQuiesceMarker removes a quiesce record while the launchd
// job it names is loaded. The record claims the job was booted out and never
// bootstrapped again; a loaded job contradicts that (a human loaded it by
// hand), and keeping the record would let a later restart revive a job the
// operator then unloaded on purpose.
func discardStaleRestartQuiesceMarker(opts *restartOptions, prefix string) {
	path := restartQuiesceMarkerPath()
	if path == "" {
		return
	}
	if _, err := os.Lstat(path); err != nil {
		return
	}
	if err := removeRestartQuiesceMarker(path); err != nil {
		fmt.Fprintf(opts.err, "%s: remove stale quiesce record %s: %v\n", prefix, path, err)
		return
	}
	if !opts.jsonOut {
		fmt.Fprintf(opts.out, "%s: discarded stale quiesce record %s (the app supervisor is loaded)\n", prefix, path)
	}
}

// planPendingSupervisorResume turns a validated quiesce record into a plan
// that ran with its supervisor set, so the daemon stage is followed by the
// same bootstrap that resumes a job quiesced in this run. A plist still on
// the pre-upgrade executable is rewritten first, as a loaded job would be.
func planPendingSupervisorResume(ctx context.Context, opts *restartOptions, deps appRestartDeps, prefix string, plan appStackRestartPlan, pending pendingSupervisorResume) (appStackRestartPlan, int) {
	sup := pending.supervisor
	plan.ran = true
	plan.pendingResume = true
	plan.quiescedAt = pending.quiescedAt
	plan.supervisor = &sup
	plan.currentPath = restartExecutablePaths(deps)
	plan.args = slices.Clone(sup.Args)
	plan.result.Action = "not_running"
	plan.result.Reason = "quiesced_by_earlier_restart"
	plan.result.Supervisor = sup.Target
	plan.result.Args = slices.Clone(sup.Args)
	plan.result.QuiescedAt = pending.quiescedAt.UTC().Format(time.RFC3339)
	plan.result.QuiesceRecord = pending.path
	if pending.migrate {
		if err := deps.rewrite(ctx, sup); err != nil {
			fmt.Fprintf(opts.err, "%s: rewrite pre-upgrade launchd app supervisor: %v\n", prefix, err)
			return plan, 1
		}
	}
	return plan, 0
}

// resumePendingSupervisorApp is `restart --app` for a launchd job that an
// earlier stack restart booted out and never bootstrapped again.
func resumePendingSupervisorApp(ctx context.Context, opts *restartOptions, deps appRestartDeps, prefix string, startedAt time.Time, pending pendingSupervisorResume) (appRestartResult, bool, int) {
	plan := appStackRestartPlan{result: appRestartResult{Target: "app", Supervisor: pending.supervisor.Target}, startedAt: startedAt}
	if opts.appAddrSet || opts.appPublicURLSet || opts.appRemoteSet || opts.appRemoteURLSet || opts.appStateDirSet {
		fmt.Fprintf(opts.err, "%s: app flag overrides do not apply to the launchd-supervised app (%s); rewrite and reload its plist with `%s setup app`, then rerun\n", prefix, pending.supervisor.Target, productidentity.Executable)
		return plan.result, true, 1
	}
	plan, exit := planPendingSupervisorResume(ctx, opts, deps, prefix, plan, pending)
	if exit != 0 {
		return plan.result, true, exit
	}
	res, exit := bootstrapQuiescedSupervisor(ctx, opts, deps, prefix, plan)
	return res, true, exit
}
