package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const hookPath = "../../hooks/canary-pre-tool-use.sh"

func TestCanaryPreToolHookLiveReadyAllowsBrokerWrite(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","can_write":true,"blocked":false,"live_override":"ready","gateway_port":4001,"account":"U1234567","endpoint":"127.0.0.1:4001"}`
	res := runCanaryHook(t, hookRun{status: status, command: "canary order place --preview-token TOKEN --json"})
	if res.code != 0 {
		t.Fatalf("hook exit=%d stderr=%s", res.code, res.stderr)
	}
}

func TestCanaryPreToolHookBlocksChainedReadThenWrite(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","can_write":true,"blocked":false,"live_override":"ready","gateway_port":4001,"account":"U1234567","endpoint":"127.0.0.1:4001"}`
	res := runCanaryHook(t, hookRun{status: status, command: "canary order status abc; canary order place --preview-token TOKEN --json"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "without shell composition") {
		t.Fatalf("stderr missing composition block: %s", res.stderr)
	}
}

func TestCanaryPreToolHookGatesOpportunitiesExercise(t *testing.T) {
	t.Parallel()
	status := `{"mode":"paper","can_write":false,"blocked":false,"live_override":"blocked","gateway_port":4002,"account":"DU1234567","endpoint":"127.0.0.1:4002","write_blockers":[{"code":"trading_frozen"}]}`
	res := runCanaryHook(t, hookRun{status: status, command: "canary opportunities exercise option_exercise:a sha256:rev --json"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "Broker-adjacent") {
		t.Fatalf("stderr missing broker-write gate: %s", res.stderr)
	}
}

func TestCanaryPreToolHookGatesProposalReduceSubmit(t *testing.T) {
	t.Parallel()
	status := `{"mode":"disabled","can_write":false,"blocked":true,"live_override":"blocked","write_blockers":[{"code":"trading_disabled"}]}`
	res := runCanaryHook(t, hookRun{status: status, command: "canary proposals reduce BB --percent 25 --submit --json"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "Broker-adjacent") {
		t.Fatalf("stderr missing broker-write gate: %s", res.stderr)
	}
}

func TestCanaryPreToolHookBlocksComposedProposalReduceSubmit(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","can_write":true,"blocked":false,"live_override":"ready","gateway_port":4001,"account":"U1234567"}`
	res := runCanaryHook(t, hookRun{status: status, command: "canary proposals reduce BB --percent 25 --submit --json; echo done"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "without shell composition") {
		t.Fatalf("stderr missing composition block: %s", res.stderr)
	}
}

func TestCanaryPreToolHookDoesNotFreezeExemptFutureClose(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","blocked":false,"live_override":"ready","can_write":false,"account":"U1234567","gateway_port":7496,"write_blockers":[{"code":"trading_frozen"}]}`
	res := runCanaryHook(t, hookRun{status: status, command: "canary order close 42"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
}

func TestCanaryPreToolHookAllowsReadOnlyPipe(t *testing.T) {
	t.Parallel()
	status := `{"mode":"paper","can_write":true,"blocked":false,"live_override":"blocked","gateway_port":4002,"account":"DU1234567","endpoint":"127.0.0.1:4002"}`
	res := runCanaryHook(t, hookRun{status: status, command: "canary opportunities status --json | jq ."})
	if res.code != 0 {
		t.Fatalf("hook exit=%d stderr=%s", res.code, res.stderr)
	}
}

func TestCanaryPreToolHookAllowsHelpPipeForWriteShapedCommand(t *testing.T) {
	t.Parallel()
	res := runCanaryHook(t, hookRun{command: "canary settings set --help | sed -n 1,80p"})
	if res.code != 0 {
		t.Fatalf("hook exit=%d stderr=%s", res.code, res.stderr)
	}
}

func TestCanaryPreToolHookDoesNotLetHelpHideSecondCanaryWrite(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","can_write":true,"blocked":false,"live_override":"ready","gateway_port":4001,"account":"U1234567","endpoint":"127.0.0.1:4001"}`
	res := runCanaryHook(t, hookRun{status: status, command: "canary settings set --help; canary order place --preview-token TOKEN"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "without shell composition") {
		t.Fatalf("stderr missing composition block: %s", res.stderr)
	}
}

func TestCanaryPreToolHookMissingJQOnlyBlocksCanary(t *testing.T) {
	t.Parallel()
	path := pathWithoutJQ(t)
	if res := runCanaryHook(t, hookRun{command: "echo hi", path: path}); res.code != 0 {
		t.Fatalf("non-canary without jq exit=%d stderr=%s", res.code, res.stderr)
	}
	res := runCanaryHook(t, hookRun{command: "canary order place --preview-token TOKEN", path: path})
	if res.code != 2 {
		t.Fatalf("canary without jq exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "jq is required") {
		t.Fatalf("stderr missing jq requirement: %s", res.stderr)
	}
}

func TestCanaryPreToolHookRejectsRetiredIBKRExecutable(t *testing.T) {
	t.Parallel()
	res := runCanaryHook(t, hookRun{command: "ibkr status --json"})
	if res.code != 2 || !strings.Contains(res.stderr, "retired ibkr executable") {
		t.Fatalf("retired executable exit=%d stderr=%s", res.code, res.stderr)
	}
}

type hookRun struct {
	status  string
	command string
	path    string
}

type hookResult struct {
	code   int
	stderr string
}

func runCanaryHook(t *testing.T, in hookRun) hookResult {
	t.Helper()
	temp := t.TempDir()
	fakeCanary := filepath.Join(temp, "canary")
	script := `#!/bin/sh
if [ "$1" = "trading" ] && [ "$2" = "status" ] && [ "$3" = "--json" ]; then
  printf '%s\n' "$CANARY_FAKE_STATUS"
  exit 0
fi
printf 'unexpected fake canary call: %s\n' "$*" >&2
exit 44
`
	if err := os.WriteFile(fakeCanary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake canary: %v", err)
	}
	payload := `{"tool_input":{"command":` + strconvQuote(in.command) + `}}`
	cmd := exec.Command("/bin/bash", hookPath)
	cmd.Stdin = strings.NewReader(payload)
	path := temp + string(os.PathListSeparator) + os.Getenv("PATH")
	if in.path != "" {
		path = in.path
	}
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"CANARY_FAKE_STATUS="+in.status,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run hook: %v", err)
		}
	}
	return hookResult{code: code, stderr: string(out)}
}

func pathWithoutJQ(t *testing.T) string {
	t.Helper()
	temp := t.TempDir()
	links := map[string]string{
		"cat":  "/bin/cat",
		"grep": "/usr/bin/grep",
	}
	if runtime.GOOS == "linux" {
		links["grep"] = "/bin/grep"
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(temp, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	return temp
}

func strconvQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
