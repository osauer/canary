// Command commit-check verifies the exact staged Git tree with a conservative,
// impact-aware subset of make check. It is an intermediate checkpoint gate,
// never final handoff, CI, or release evidence.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const plannerVersion = "commit-check-v1"

type cacheRecord struct {
	Key       string    `json:"key"`
	Tree      string    `json:"tree"`
	Targets   []string  `json:"targets"`
	PassedAt  time.Time `json:"passed_at"`
	ElapsedMS int64     `json:"elapsed_ms"`
}

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) int {
	flags := flag.NewFlagSet("commit-check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	planOnly := flags.Bool("plan-only", false, "print the staged-tree plan without running it")
	noCache := flags.Bool("no-cache", false, "ignore and do not update the exact-tree success cache")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "commit-check: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}

	repo, err := gitOutput("", "rev-parse", "--show-toplevel")
	if err != nil {
		return reportError("locate repository", err)
	}
	head, err := gitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		return reportError("resolve HEAD", err)
	}
	if output, code, err := runCommand(repo, nil, "git", "diff", "--cached", "--quiet", "--exit-code"); err != nil && code != 1 {
		return reportError("inspect staged changes", commandError(output, code, err))
	} else if err == nil {
		fmt.Fprintln(os.Stderr, "commit-check: no staged changes; stage the intended checkpoint first")
		return 2
	}
	if conflicts, err := gitOutput(repo, "ls-files", "-u"); err != nil {
		return reportError("inspect index conflicts", err)
	} else if strings.TrimSpace(conflicts) != "" {
		fmt.Fprintln(os.Stderr, "commit-check: unresolved index conflicts require the full gate after resolution")
		return 2
	}
	tree, err := gitOutput(repo, "write-tree")
	if err != nil {
		return reportError("capture staged tree", err)
	}
	raw, err := gitBytes(repo, "diff-tree", "--raw", "-z", "--no-commit-id", "-r", "-M", head, tree)
	if err != nil {
		return reportError("read staged tree diff", err)
	}
	changes, err := parseRawDiff(raw)
	if err != nil {
		return reportError("parse staged tree diff", err)
	}
	plan := planChecks(changes)
	printPlan(head, tree, plan)
	if *planOnly {
		return 0
	}

	cachePath := ""
	cacheKey := ""
	if !plan.full && !*noCache {
		cacheKey, err = exactTreeCacheKey(head, tree, plan)
		if err != nil {
			return reportError("build cache key", err)
		}
		cachePath, err = successCachePath(cacheKey)
		if err != nil {
			return reportError("locate success cache", err)
		}
		if cacheHit(cachePath, cacheKey) {
			fmt.Printf("commit-check: cache hit tree=%s; exact staged plan already passed\n", shortOID(tree))
			return 0
		}
		fmt.Printf("commit-check: cache miss key=%s\n", cacheKey[:12])
	} else if plan.full {
		fmt.Printf("commit-check: cache disabled for full fallback\n")
	} else {
		fmt.Printf("commit-check: cache disabled by caller\n")
	}

	worktree, cleanup, err := stagedWorktree(repo, head, tree)
	if err != nil {
		return reportError("materialize isolated staged tree", err)
	}
	defer cleanup()

	started := time.Now()
	makeArgs := []string{"--no-print-directory", "-j" + strconv.Itoa(commitCheckWorkers())}
	makeArgs = append(makeArgs, plan.targets...)
	env := os.Environ()
	if plan.hasGo {
		env = replaceEnv(env, "GOVULN_FORCE", "1")
	}
	fmt.Printf("commit-check: running in isolated worktree %s\n", worktree)
	output, code, runErr := runCommandStreaming(worktree, env, "make", makeArgs...)
	elapsed := time.Since(started)
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "commit-check: FAIL tree=%s elapsed=%s exit=%d\n", shortOID(tree), elapsed.Round(time.Millisecond), code)
		if strings.TrimSpace(output) != "" {
			fmt.Fprintln(os.Stderr, output)
		}
		return normalizedExitCode(code)
	}
	fmt.Printf("commit-check: PASS tree=%s elapsed=%s\n", shortOID(tree), elapsed.Round(time.Millisecond))

	if cachePath != "" {
		record := cacheRecord{
			Key: cacheKey, Tree: tree, Targets: plan.targets,
			PassedAt: time.Now().UTC(), ElapsedMS: elapsed.Milliseconds(),
		}
		if err := writeCacheRecord(cachePath, record); err != nil {
			return reportError("record exact-tree success", err)
		}
	}
	return 0
}

func printPlan(head, tree string, plan checkPlan) {
	fmt.Printf("commit-check: base=%s staged_tree=%s\n", shortOID(head), shortOID(tree))
	fmt.Printf("commit-check: classes=%s targets=%s\n", strings.Join(plan.classes, ","), strings.Join(plan.targets, ","))
	if plan.reason != "" {
		fmt.Printf("commit-check: full fallback: %s\n", plan.reason)
	}
	for _, candidate := range plan.changed {
		fmt.Printf("commit-check: changed %q\n", candidate)
	}
}

func stagedWorktree(repo, head, tree string) (string, func(), error) {
	root, err := os.MkdirTemp("", "canary-commit-check-")
	if err != nil {
		return "", nil, err
	}
	candidate := filepath.Join(root, "candidate")
	added := false
	cleanup := func() {
		if added {
			_, _, _ = runCommand(repo, nil, "git", "worktree", "remove", "--force", candidate)
		}
		_ = os.RemoveAll(root)
	}
	if output, code, err := runCommand(repo, nil, "git", "worktree", "add", "--detach", "--no-checkout", candidate, head); err != nil {
		cleanup()
		return "", nil, commandError(output, code, err)
	}
	added = true
	if output, code, err := runCommand(candidate, nil, "git", "read-tree", tree); err != nil {
		cleanup()
		return "", nil, commandError(output, code, err)
	}
	if output, code, err := runCommand(candidate, nil, "git", "checkout-index", "-a", "-f"); err != nil {
		cleanup()
		return "", nil, commandError(output, code, err)
	}
	return candidate, cleanup, nil
}

func exactTreeCacheKey(head, tree string, plan checkPlan) (string, error) {
	goVersion, err := commandVersion("go", "version")
	if err != nil {
		return "", err
	}
	nodeVersion, err := commandVersion("node", "--version")
	if err != nil {
		nodeVersion = "unavailable"
	}
	material := strings.Join([]string{
		plannerVersion,
		head,
		tree,
		strings.Join(plan.targets, ","),
		time.Now().UTC().Format("2006-01-02"),
		runtime.GOOS + "/" + runtime.GOARCH,
		goVersion,
		nodeVersion,
	}, "\n")
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:]), nil
}

func successCachePath(key string) (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "canary", "commit-check")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, key+".json"), nil
}

func cacheHit(path, key string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var record cacheRecord
	return json.Unmarshal(data, &record) == nil && record.Key == key
}

func writeCacheRecord(path string, record cacheRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".commit-check-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func commandVersion(name string, args ...string) (string, error) {
	output, code, err := runCommand("", nil, name, args...)
	if err != nil {
		return "", commandError(output, code, err)
	}
	return strings.TrimSpace(output), nil
}

func gitOutput(repo string, args ...string) (string, error) {
	output, err := gitBytes(repo, args...)
	return strings.TrimSpace(string(output)), err
}

func gitBytes(repo string, args ...string) ([]byte, error) {
	commandArgs := append([]string{}, args...)
	output, code, err := runCommand(repo, nil, "git", commandArgs...)
	if err != nil {
		return nil, commandError(output, code, err)
	}
	return []byte(output), nil
}

func runCommand(dir string, env []string, name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), 0, nil
	}
	return string(output), commandExitCode(err), err
}

func runCommandStreaming(dir string, env []string, name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return "", 0, nil
	}
	return "", commandExitCode(err), err
}

func commandExitCode(err error) int {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
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

func commandError(output string, code int, err error) error {
	detail := strings.TrimSpace(output)
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("exit %d: %s", code, detail)
}

func reportError(stage string, err error) int {
	fmt.Fprintf(os.Stderr, "commit-check: %s: %v\n", stage, err)
	return 2
}

func shortOID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

func commitCheckWorkers() int {
	workers := runtime.NumCPU()
	if workers < 1 {
		return 1
	}
	if workers > 8 {
		return 8
	}
	return workers
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}
