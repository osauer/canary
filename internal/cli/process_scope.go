package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/procinfo"
)

// processScope contains only path identity, never credentials or broker data.
// Compare effective paths so an explicit default and an omitted override agree.
type processScope struct {
	Home        string `json:"home"`
	Socket      string `json:"socket"`
	Config      string `json:"config"`
	State       string `json:"state"`
	Cache       string `json:"cache"`
	Log         string `json:"log"`
	AppState    string `json:"app_state"`
	AppDefaults string `json:"app_defaults"`
}

func scopeFromEnvironment(environ []string) (processScope, error) {
	env := map[string]string{}
	for _, item := range environ {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	home := env["HOME"]
	if !filepath.IsAbs(home) {
		return processScope{}, errors.New("cannot verify process HOME")
	}
	fallback := func(key, def string) string {
		if v := env[key]; v != "" {
			return v
		}
		return def
	}
	state := fallback("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runtime := fallback("XDG_RUNTIME_DIR", filepath.Join(home, ".cache"))
	config := fallback("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	scope := processScope{Home: home, Socket: fallback("CANARY_SOCKET", filepath.Join(runtime, "ibkr", "ibkr.sock")), Config: fallback("CANARY_CONFIG", filepath.Join(config, "ibkr", "config.toml")), State: filepath.Join(state, "ibkr", "daemon.db"), Cache: fallback("XDG_CACHE_HOME", filepath.Join(home, ".cache")), Log: fallback("CANARY_LOG", filepath.Join(state, "ibkr", "ibkr-daemon.log"))}
	scope.AppState = fallback("CANARY_APP_STATE_DIR", filepath.Join(state, "ibkr", "app"))
	// Hash app defaults so private relay URLs are not copied into recovery records.
	var defaults strings.Builder
	for _, key := range []string{"CANARY_APP_ADDR", "CANARY_APP_PUBLIC_URL", "CANARY_APP_REMOTE", "CANARY_APP_REMOTE_URL", "CANARY_APP_LOG_LEVEL"} {
		fmt.Fprintf(&defaults, "%s=%s\x00", key, env[key])
	}
	digest := sha256.Sum256([]byte(defaults.String()))
	scope.AppDefaults = hex.EncodeToString(digest[:])
	for _, path := range []*string{&scope.Home, &scope.Socket, &scope.Config, &scope.State, &scope.Cache, &scope.Log, &scope.AppState} {
		if !filepath.IsAbs(*path) {
			return processScope{}, errors.New("relative profile paths cannot be attributed safely; use absolute HOME/XDG/Canary paths")
		}
		*path = canonicalScopePath(*path)
	}
	return scope, nil
}

func verifyAppScope(ctx context.Context, paths map[string]struct{}) error {
	current, err := scopeFromEnvironment(os.Environ())
	if err != nil {
		return err
	}

	proc, err := findAppProcessForExecutables(ctx, paths)
	if err == nil {
		if err := verifyAppProcessScope(proc.PID, procinfo.Inspect); err != nil {
			return err
		}
	} else if !errors.Is(err, errAppNotRunning) {
		return err
	}
	if sup, ok := findAppLaunchAgent(ctx); ok {
		if err := verifySupervisorDefinition(sup); err != nil {
			return err
		}
		if sup.PID <= 0 {
			return errors.New("loaded app supervisor has no process whose profile can be verified; manage it with `canary setup app` from its original profile")
		}
		if sup.PID != proc.PID {
			if err := verifyAppProcessScope(sup.PID, procinfo.Inspect); err != nil {
				return err
			}
		}
	}
	// The supervisor may be unloaded after a previous failed restart. Its
	// durable record must bind recovery to the profile that quiesced it.
	marker, exists, err := readRestartQuiesceMarker(restartQuiesceMarkerPath())
	if err != nil {
		return fmt.Errorf("cannot verify app recovery record: %w", err)
	}
	if exists && marker.Scope != current {
		return errors.New("app recovery record belongs to another or unknown profile; leave it intact and recover from the original profile (older records need `canary setup app`)")
	}
	if exists {
		if _, err := validateRestartQuiesceMarker(marker, paths, true); err != nil {
			return fmt.Errorf("app recovery record cannot be resumed safely: %w; record left intact, recover with canary setup app from the original profile", err)
		}
	}
	return nil
}

func verifyAppPathArgs(args []string) error {
	for i, arg := range args {
		key, value, equals := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "-") || (key != "state-dir" && key != "log") {
			continue
		}
		if !equals {
			if i+1 >= len(args) {
				return errors.New("missing app path argument")
			}
			value = args[i+1]
		}
		if value == "" || (key == "log" && value == "stderr") {
			continue
		}
		if !filepath.IsAbs(value) {
			return fmt.Errorf("app uses relative -%s; restart it from its original working directory with an absolute path", key)
		}
	}
	return nil
}

// Custom launchd environment/working-directory dictionaries need explicit
// supervisor management; process-name discovery cannot prove what launchd will
// inherit on its next launch. Refuse before unload, never rewrite such a job.
func verifySupervisorDefinition(sup appSupervisor) error {
	data, err := os.ReadFile(sup.PlistPath)
	if err != nil {
		return fmt.Errorf("read app supervisor definition: %w", err)
	}
	program, err := parseLaunchAgentPlist(data)
	if err != nil {
		return err
	}
	if !slices.Equal(program.args, sup.Args) {
		return errors.New("on-disk app arguments differ from selected supervisor; leave it untouched and manage it explicitly")
	}
	if program.executable != sup.Executable && !executablePathMatches(program.executable, currentExecutablePaths()) {
		return errors.New("on-disk app executable differs from selected supervisor")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "key" {
			var key string
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return err
			}
			if key == "EnvironmentVariables" || key == "WorkingDirectory" {
				return errors.New("app supervisor defines a custom environment or working directory; manage this job explicitly with launchctl from its original profile")
			}
		}
	}
	return verifyAppPathArgs(sup.Args)
}

// Resolve existing ancestors too: creating a previously absent state file must
// not change identity just because /tmp or /var is a symlink on macOS.
func canonicalScopePath(path string) string {
	path = filepath.Clean(path)
	candidate := path
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			for _, part := range slices.Backward(suffix) {
				resolved = filepath.Join(resolved, part)
			}
			return resolved
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return path
		}
		suffix = append(suffix, filepath.Base(candidate))
		candidate = parent
	}
}

func verifyAppProcessScope(pid int, inspect func(int) (procinfo.Invocation, error)) error {
	current, err := scopeFromEnvironment(os.Environ())
	if err != nil {
		return err
	}
	inv, err := inspect(pid)
	if err != nil {
		return fmt.Errorf("cannot verify app profile for pid %d: %w", pid, err)
	}
	if _, _, ok := appArgvMatch(inv.Args, nil); !ok {
		return errors.New("selected process is no longer a Canary app")
	}
	target, err := scopeFromEnvironment(inv.Environment)
	if err != nil {
		return err
	}
	if target != current {
		return fmt.Errorf("app pid %d belongs to a different HOME, socket, config, state or app-default profile; run the command from that app's original profile", pid)
	}
	return verifyAppPathArgs(inv.Args)
}

func signalVerifiedApp(pid int, timeout time.Duration, inspect func(int) (procinfo.Invocation, error), signal func(int, time.Duration) error) error {
	if err := verifyAppProcessScope(pid, inspect); err != nil {
		return err
	}
	return signal(pid, timeout)
}

func verifySelectedSupervisor(_ context.Context, sup appSupervisor) error {
	if err := verifySupervisorDefinition(sup); err != nil {
		return err
	}
	if sup.PID <= 0 {
		return errors.New("supervisor has no process whose profile can be verified; manage it explicitly")
	}
	return verifyAppProcessScope(sup.PID, procinfo.Inspect)
}
