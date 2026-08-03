package agentconfig

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func TestProjectCodexDefaultsStayBounded(t *testing.T) {
	var cfg struct {
		SandboxMode    string `toml:"sandbox_mode"`
		ApprovalPolicy string `toml:"approval_policy"`
		Agents         struct {
			MaxThreads int `toml:"max_threads"`
			MaxDepth   int `toml:"max_depth"`
		} `toml:"agents"`
	}
	if _, err := toml.DecodeFile(repoPath(".codex", "config.toml"), &cfg); err != nil {
		t.Fatalf("decode project Codex config: %v", err)
	}
	if cfg.SandboxMode != "workspace-write" {
		t.Fatalf("sandbox_mode=%q, want workspace-write; host-wide access must be a bounded user choice", cfg.SandboxMode)
	}
	if cfg.ApprovalPolicy != "on-request" {
		t.Fatalf("approval_policy=%q, want on-request", cfg.ApprovalPolicy)
	}
	if cfg.Agents.MaxThreads < 1 || cfg.Agents.MaxThreads > 4 {
		t.Fatalf("agents.max_threads=%d, want 1..4", cfg.Agents.MaxThreads)
	}
	if cfg.Agents.MaxDepth != 1 {
		t.Fatalf("agents.max_depth=%d, want 1", cfg.Agents.MaxDepth)
	}
}

func TestReviewerAgentsStayReadOnly(t *testing.T) {
	paths, err := filepath.Glob(repoPath(".codex", "agents", "*.toml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob reviewer agents: paths=%v err=%v", paths, err)
	}
	for _, path := range paths {
		var cfg struct {
			Name        string `toml:"name"`
			SandboxMode string `toml:"sandbox_mode"`
		}
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			t.Errorf("decode %s: %v", path, err)
			continue
		}
		if cfg.Name == "" || cfg.SandboxMode != "read-only" {
			t.Errorf("%s: name=%q sandbox_mode=%q, want named read-only reviewer", path, cfg.Name, cfg.SandboxMode)
		}
	}
}

func TestCodexHookAndBrowserPolicyAreWired(t *testing.T) {
	data, err := os.ReadFile(repoPath(".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode .codex/hooks.json: %v", err)
	}
	if !strings.Contains(string(data), ".codex/hooks/canary-pre-tool-use.sh") || !strings.Contains(string(data), "exec_command") {
		t.Fatal(".codex/hooks.json does not wire the project shell hook to exec_command")
	}
	for _, path := range []string{
		repoPath(".codex", "hooks", "canary-pre-tool-use.sh"),
		repoPath("hooks", "canary-pre-tool-use.sh"),
		repoPath("hooks", "exec-policy-parity_test.sh"),
	} {
		if info, err := os.Stat(path); err != nil || info.Mode()&0o111 == 0 {
			t.Errorf("hook/test %s missing or not executable: info=%v err=%v", path, info, err)
		}
	}
	browserRules, err := os.ReadFile(repoPath("web", "app", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	normalizedRules := strings.Join(strings.Fields(string(browserRules)), " ")
	for _, required := range []string{"Browser QA is read-only", "human-paired-device", "gated CLI path"} {
		if !strings.Contains(normalizedRules, required) {
			t.Errorf("web/app/AGENTS.md missing browser safety phrase %q", required)
		}
	}
}

func TestCanaryExecPolicyAndRetiredIBKRBoundaries(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex unavailable")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	cmd := exec.Command("/bin/bash", repoPath("hooks", "exec-policy-parity_test.sh"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("execpolicy parity: %v\n%s", err, out)
	}
}

func TestSessionStartInvokesOnlyCanaryAndDetectsRetiredIBKR(t *testing.T) {
	data, err := os.ReadFile(repoPath("hooks", "session-start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, required := range []string{
		"command -v canary",
		"command -v ibkr",
		"hooks will not invoke it",
		`cli_bin="canary"`,
		`bin_raw=$("$cli_bin" version --json`,
	} {
		if !strings.Contains(source, required) {
			t.Errorf("session-start boundary missing %q", required)
		}
	}
	if strings.Contains(source, `cli_bin="ibkr"`) {
		t.Fatal("session-start still invokes the retired ibkr executable")
	}
}

func TestRepoCanaryHarnessSkillIdentity(t *testing.T) {
	canonical, err := os.ReadFile(repoPath("skills", "canary", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	repoSkill, err := os.ReadFile(repoPath(".agents", "skills", "canary-harness", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), "\nname: canary\n") {
		t.Fatal("canonical installed skill must use name canary")
	}
	if !strings.Contains(string(repoSkill), "\nname: canary-harness\n") {
		t.Fatal("repo development skill must use unique name canary-harness")
	}
}

func TestTrackedClaudeConfigStaysPortable(t *testing.T) {
	cmd := exec.Command("git", "ls-files", "-z", ".claude")
	cmd.Dir = repoPath()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files .claude: %v", err)
	}
	files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(files) == 0 || files[0] == "" {
		t.Fatal("no tracked files under .claude")
	}
	for _, rel := range files {
		data, err := os.ReadFile(repoPath(rel))
		if err != nil {
			t.Fatal(err)
		}
		// This is a public repo cloned to arbitrary checkouts: binaries and
		// state must resolve via env/PATH, never a checkout-specific home.
		if strings.Contains(string(data), "/Users/") {
			t.Errorf("%s embeds a checkout-specific /Users path", rel)
		}
	}
}

func TestClaudeSkillAndAgentFrontmatterParses(t *testing.T) {
	skillPaths, err := filepath.Glob(repoPath(".claude", "skills", "*", "SKILL.md"))
	if err != nil || len(skillPaths) == 0 {
		t.Fatalf("glob .claude skills: paths=%v err=%v", skillPaths, err)
	}
	agentPaths, err := filepath.Glob(repoPath(".claude", "agents", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range append(skillPaths, agentPaths...) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rest, ok := strings.CutPrefix(string(data), "---\n")
		if !ok {
			t.Errorf("%s: missing frontmatter open", path)
			continue
		}
		header, _, ok := strings.Cut(rest, "\n---\n")
		if !ok {
			t.Errorf("%s: missing frontmatter close", path)
			continue
		}
		for _, field := range []string{"name: ", "description: "} {
			if !strings.HasPrefix(header, field) && !strings.Contains(header, "\n"+field) {
				t.Errorf("%s: frontmatter missing %q", path, strings.TrimSpace(field))
			}
		}
	}
}
