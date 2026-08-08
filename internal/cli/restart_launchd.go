package cli

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/productidentity"
)

// appLaunchAgentLabel matches the pre-rename LaunchAgent. It is intentionally
// pinned so the executable rename cannot load a second app supervisor.
const appLaunchAgentLabel = productidentity.AppLaunchAgentLabel

// appSupervisor describes a loaded launchd job that owns the app process.
type appSupervisor struct {
	Target     string   // launchctl target, e.g. gui/501/com.osauer.ibkr-app
	PID        int      // supervised pid, 0 while launchd has no live process
	Executable string   // leading plist ProgramArguments executable
	Args       []string // plist ProgramArguments starting at "app"
	PlistPath  string   // pinned per-user plist path used for a one-shot migration
	ParseError string   // non-empty means the loaded job cannot be safely managed
}

var launchdPIDRe = regexp.MustCompile(`(?m)^\s*pid = (\d+)\b`)
var (
	plistProgramArgumentsRe = regexp.MustCompile(`(?s)<key>\s*ProgramArguments\s*</key>\s*<array>(.*?)</array>`)
	plistLabelRe            = regexp.MustCompile(`(?s)<key>\s*Label\s*</key>\s*<string>([^<]*)</string>`)
	plistProgramKeyRe       = regexp.MustCompile(`(?s)<key>\s*Program\s*</key>`)
	plistStringRe           = regexp.MustCompile(`(?s)<string>([^<]*)</string>`)
	launchdProgramRe        = regexp.MustCompile(`(?m)^\s*program = (.+?)\s*$`)
	launchdArgumentsRe      = regexp.MustCompile(`(?m)^\s*arguments = \{\s*$`)
)

// findAppLaunchAgent reports the loaded app LaunchAgent, if any. A restart
// must go through launchd when it owns the app: SIGTERM-plus-respawn races
// launchd's KeepAlive and strands an orphan that holds the app state lock
// while launchd crash-loops against it.
func findAppLaunchAgent(ctx context.Context) (appSupervisor, bool) {
	if runtime.GOOS != "darwin" {
		return appSupervisor{}, false
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), appLaunchAgentLabel)
	out, err := exec.CommandContext(ctx, "launchctl", "print", target).Output()
	if err != nil {
		return appSupervisor{}, false
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return appSupervisor{
			Target:     target,
			ParseError: "cannot resolve the current user's LaunchAgents directory",
		}, true
	}
	sup := appSupervisor{
		Target:    target,
		PlistPath: filepath.Join(home, "Library", "LaunchAgents", appLaunchAgentLabel+".plist"),
	}
	if m := launchdPIDRe.FindStringSubmatch(string(out)); m != nil {
		if pid, err := strconv.Atoi(m[1]); err == nil {
			sup.PID = pid
		}
	}
	sup.Executable, sup.Args, err = parseLaunchdProgramArguments(string(out))
	if err != nil {
		sup.ParseError = err.Error()
	}
	return sup, true
}

func parseLaunchdProgramArguments(out string) (string, []string, error) {
	if len(launchdArgumentsRe.FindAllStringIndex(out, -1)) != 1 {
		return "", nil, errors.New("launchd job must contain exactly one arguments block")
	}
	var args []string
	inBlock := false
	closedBlock := false
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if trimmed == "arguments = {" {
				inBlock = true
			}
			continue
		}
		if trimmed == "}" {
			closedBlock = true
			break
		}
		if trimmed == "" || strings.ContainsAny(trimmed, "{}") {
			return "", nil, errors.New("malformed launchd ProgramArguments")
		}
		args = append(args, trimmed)
	}
	if !closedBlock {
		return "", nil, errors.New("missing or unterminated launchd ProgramArguments")
	}
	if len(args) < 2 {
		return "", nil, errors.New("launchd ProgramArguments has no app subcommand")
	}
	executable := args[0]
	appArgs := append([]string(nil), args[1:]...)
	if !filepath.IsAbs(executable) {
		return "", nil, errors.New("launchd app executable is not an absolute path")
	}
	if !productidentity.IsManagedProcessExecutableBase(filepath.Base(executable)) {
		return "", nil, fmt.Errorf("launchd app executable %q is not managed by Canary", executable)
	}
	if !isAppServerArgs(appArgs) {
		return "", nil, errors.New("launchd ProgramArguments is not an app server command")
	}
	programs := launchdProgramRe.FindAllStringSubmatch(out, -1)
	if len(programs) > 1 {
		return "", nil, errors.New("multiple launchd program declarations")
	}
	if len(programs) == 1 && strings.TrimSpace(programs[0][1]) != executable {
		return "", nil, errors.New("launchd program does not match ProgramArguments executable")
	}
	return executable, appArgs, nil
}

func kickstartLaunchAgent(ctx context.Context, target string) error {
	out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart -k %s: %v: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// migrateAppLaunchAgent performs the only supported legacy-command transition:
// the pinned, already-loaded app label may move from an `ibkr app ...`
// ProgramArguments vector to the installed `canary app ...` executable. It
// never introduces a second label and never invokes the retired command.
func migrateAppLaunchAgent(ctx context.Context, sup appSupervisor, currentExecutable string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("launchd migration is only available on macOS")
	}
	return migrateAppLaunchAgentUsing(ctx, sup, currentExecutable, runLaunchctl)
}

type launchctlRunner func(context.Context, ...string) ([]byte, error)

func runLaunchctl(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
}

func migrateAppLaunchAgentUsing(ctx context.Context, sup appSupervisor, currentExecutable string, run launchctlRunner) error {
	if err := rewriteAppLaunchAgentForMigration(sup, currentExecutable); err != nil {
		return err
	}
	if out, err := run(ctx, "bootout", sup.Target); err != nil {
		return fmt.Errorf("launchctl bootout %s: %v: %s", sup.Target, err, strings.TrimSpace(string(out)))
	}
	domain, err := launchAgentDomain(sup)
	if err != nil {
		return err
	}
	if out, err := run(ctx, "bootstrap", domain, sup.PlistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap %s %s: %v: %s", domain, sup.PlistPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func rewriteAppLaunchAgentForMigration(sup appSupervisor, currentExecutable string) error {
	if sup.ParseError != "" ||
		sup.Target != fmt.Sprintf("gui/%d/%s", os.Getuid(), appLaunchAgentLabel) ||
		filepath.Base(sup.Executable) != productidentity.PreUpgradeExecutable ||
		!isAppServerArgs(sup.Args) {
		return errors.New("loaded supervisor is not the trusted pre-upgrade Canary app job")
	}
	if strings.TrimSpace(sup.PlistPath) == "" {
		return errors.New("loaded supervisor has no pinned plist path")
	}
	cleanPlist := filepath.Clean(sup.PlistPath)
	if filepath.Base(cleanPlist) != appLaunchAgentLabel+".plist" ||
		filepath.Base(filepath.Dir(cleanPlist)) != "LaunchAgents" ||
		filepath.Base(filepath.Dir(filepath.Dir(cleanPlist))) != "Library" {
		return fmt.Errorf("loaded supervisor plist path %q is not pinned to %s", sup.PlistPath, appLaunchAgentLabel)
	}
	currentExecutable = strings.TrimSpace(currentExecutable)
	if !filepath.IsAbs(currentExecutable) || filepath.Base(currentExecutable) != productidentity.Executable {
		return fmt.Errorf("canonical executable %q is not an absolute %s path", currentExecutable, productidentity.Executable)
	}
	info, err := os.Stat(currentExecutable)
	if err != nil {
		return fmt.Errorf("verify installed canonical executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("canonical executable %q is not an executable regular file", currentExecutable)
	}
	plistInfo, err := os.Lstat(sup.PlistPath)
	if err != nil {
		return fmt.Errorf("inspect launchd plist: %w", err)
	}
	if !plistInfo.Mode().IsRegular() {
		return fmt.Errorf("launchd plist %q is not a regular file", sup.PlistPath)
	}
	if plistInfo.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("launchd plist %q is group/world writable", sup.PlistPath)
	}
	data, err := os.ReadFile(sup.PlistPath)
	if err != nil {
		return fmt.Errorf("read launchd plist: %w", err)
	}
	rewritten, err := rewriteLaunchAgentExecutable(data, sup, currentExecutable)
	if err != nil {
		return err
	}
	if !bytes.Equal(rewritten, data) {
		if err := writeLaunchAgentAtomically(sup.PlistPath, rewritten, plistInfo.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func unloadAppLaunchAgent(ctx context.Context, sup appSupervisor) error {
	out, err := runLaunchctl(ctx, "bootout", sup.Target)
	if err != nil {
		return fmt.Errorf("launchctl bootout %s: %v: %s", sup.Target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func loadAppLaunchAgent(ctx context.Context, sup appSupervisor) error {
	domain, err := launchAgentDomain(sup)
	if err != nil {
		return err
	}
	out, err := runLaunchctl(ctx, "bootstrap", domain, sup.PlistPath)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap %s %s: %v: %s", domain, sup.PlistPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func launchAgentDomain(sup appSupervisor) (string, error) {
	domain, suffix, ok := strings.Cut(sup.Target, "/"+appLaunchAgentLabel)
	if !ok || domain == "" || suffix != "" {
		return "", fmt.Errorf("invalid launchd target %q", sup.Target)
	}
	if strings.TrimSpace(sup.PlistPath) == "" {
		return "", errors.New("launchd supervisor has no plist path")
	}
	return domain, nil
}

func rewriteLaunchAgentExecutable(data []byte, sup appSupervisor, currentExecutable string) ([]byte, error) {
	labelMatches := plistLabelRe.FindAllSubmatch(data, -1)
	if len(labelMatches) != 1 {
		return nil, errors.New("launchd plist must contain exactly one parseable Label")
	}
	labelMatch := labelMatches[0]
	label, err := decodePlistString(labelMatch[1])
	if err != nil || label != appLaunchAgentLabel {
		return nil, fmt.Errorf("launchd plist label is not %q", appLaunchAgentLabel)
	}
	if plistProgramKeyRe.Match(data) {
		return nil, errors.New("launchd plist has a separate Program override")
	}
	blocks := plistProgramArgumentsRe.FindAllSubmatchIndex(data, -1)
	if len(blocks) != 1 {
		return nil, errors.New("launchd plist must contain exactly one parseable ProgramArguments array")
	}
	block := blocks[0]
	bodyStart, bodyEnd := block[2], block[3]
	body := data[bodyStart:bodyEnd]
	stringMatches := plistStringRe.FindAllSubmatchIndex(body, -1)
	if len(stringMatches) < 2 {
		return nil, errors.New("launchd plist ProgramArguments has no app subcommand")
	}
	remaining := append([]byte(nil), body...)
	for i := range slices.Backward(stringMatches) {
		remaining = append(remaining[:stringMatches[i][0]], remaining[stringMatches[i][1]:]...)
	}
	if strings.TrimSpace(string(remaining)) != "" {
		return nil, errors.New("launchd plist ProgramArguments contains unsupported XML")
	}
	programArgs := make([]string, 0, len(stringMatches))
	for _, match := range stringMatches {
		value, err := decodePlistString(body[match[2]:match[3]])
		if err != nil {
			return nil, fmt.Errorf("decode launchd ProgramArguments: %w", err)
		}
		programArgs = append(programArgs, value)
	}
	if !slices.Equal(programArgs[1:], sup.Args) {
		return nil, errors.New("on-disk launchd arguments do not match the loaded supervisor")
	}
	onDiskExecutable := programArgs[0]
	if onDiskExecutable != sup.Executable && onDiskExecutable != currentExecutable {
		return nil, errors.New("on-disk launchd executable does not match the loaded or canonical supervisor")
	}
	if filepath.Base(onDiskExecutable) != productidentity.PreUpgradeExecutable && onDiskExecutable != currentExecutable {
		return nil, errors.New("on-disk launchd executable is not the explicit pre-upgrade command")
	}

	firstValueStart := bodyStart + stringMatches[0][2]
	firstValueEnd := bodyStart + stringMatches[0][3]
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(currentExecutable)); err != nil {
		return nil, fmt.Errorf("encode canonical executable path: %w", err)
	}
	rewritten := make([]byte, 0, len(data)-firstValueEnd+firstValueStart+escaped.Len())
	rewritten = append(rewritten, data[:firstValueStart]...)
	rewritten = append(rewritten, escaped.Bytes()...)
	rewritten = append(rewritten, data[firstValueEnd:]...)
	return rewritten, nil
}

func decodePlistString(raw []byte) (string, error) {
	var value string
	wrapped := append([]byte("<string>"), raw...)
	wrapped = append(wrapped, []byte("</string>")...)
	if err := xml.Unmarshal(wrapped, &value); err != nil {
		return "", err
	}
	return value, nil
}

func writeLaunchAgentAtomically(path string, data []byte, mode os.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".migrate-*")
	if err != nil {
		return fmt.Errorf("create launchd plist candidate: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if retErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return fmt.Errorf("set launchd plist candidate mode: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write launchd plist candidate: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync launchd plist candidate: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close launchd plist candidate: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish launchd plist candidate: %w", err)
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open LaunchAgents directory for sync: %w", err)
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync LaunchAgents directory: %w", err)
	}
	return nil
}
