package agentconfig

import (
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
