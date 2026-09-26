package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// isolatePolicyHome points HOME and the config lookup at a fresh directory.
func isolatePolicyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CANARY_CONFIG", "")
	return home
}

// `canary policy ensure` works with no daemon: the dry run writes nothing,
// the real run writes all four files, and a rerun reports them unchanged.
func TestPolicyEnsureWritesMissingFilesWithoutADaemon(t *testing.T) {
	home := isolatePolicyHome(t)
	policies := filepath.Join(home, ".config", "ibkr", "policies")
	run := func(args ...string) (string, string, int) {
		var out, errb bytes.Buffer
		code := RunPolicyLocal(context.Background(), &Env{Stdout: &out, Stderr: &errb, Version: "v9.9.9"}, args)
		return out.String(), errb.String(), code
	}
	if !PolicyLocalSubcommand([]string{"ensure", "--dry-run"}) || PolicyLocalSubcommand([]string{"show"}) {
		t.Fatal("ensure must run without the daemon and show must not")
	}
	out, _, code := run("ensure", "--dry-run")
	if code != 0 || strings.Count(out, "would create") != 4 {
		t.Fatalf("dry run exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(policies); !os.IsNotExist(err) {
		t.Fatal("the dry run wrote the policy directory")
	}
	out, _, code = run("ensure")
	if code != 0 || strings.Count(out, "created") != 4 {
		t.Fatalf("ensure exit %d:\n%s", code, out)
	}
	for _, name := range []string{"rulebook-policy.toml", "protection-policy.toml", "opportunity-policy.toml", "risk-policy.toml"} {
		data, err := os.ReadFile(filepath.Join(policies, name))
		if err != nil || !strings.HasPrefix(string(data), daemon.PolicyUnreviewedMarker+"\n") || !strings.Contains(string(data), "Canary v9.9.9 wrote this") {
			t.Fatalf("%s: %v\n%s", name, err, data)
		}
	}
	out, _, code = run("ensure", "--json")
	var res struct {
		Actions []daemon.PolicyFileAction `json:"actions"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || len(res.Actions) != 4 {
		t.Fatalf("json rerun exit %d:\n%s", code, out)
	}
	for _, a := range res.Actions {
		if a.Action != daemon.PolicyFileUnchanged {
			t.Fatalf("rerun changed %+v", a)
		}
	}
}

// A config file that does not parse never stops the ensure step: it says so
// and uses the default policy paths.
func TestPolicyEnsureSurvivesABrokenConfig(t *testing.T) {
	home := isolatePolicyHome(t)
	cfg := filepath.Join(home, ".config", "ibkr", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[gateway\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := RunPolicyLocal(context.Background(), &Env{Stdout: &out, Stderr: &errb, Version: "v9.9.9"}, []string{"ensure"}); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "config unreadable") || strings.Count(out.String(), "created") != 4 {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out.String(), errb.String())
	}
	if data, _ := os.ReadFile(cfg); string(data) != "[gateway\n" {
		t.Fatal("the ensure step touched config.toml")
	}
}

// The Rulebook screen and the policy-file list say when a file is still
// Canary's unreviewed template and what waits for the owner's number.
func TestPolicyScreensMarkUnreviewedFilesAndMissingNumbers(t *testing.T) {
	var out bytes.Buffer
	env := &Env{Stdout: &out, Stderr: &bytes.Buffer{}}
	st := &rpc.RulebookPolicyStatus{Status: rpc.RulebookPolicyStatusActive, Source: "file", Review: rpc.PolicyReviewUnreviewed, Missing: []string{"takeover_gap_pct"}}
	renderRulesPolicy(env, st, risk.DefaultRulebookPolicy(), nil)
	if s := out.String(); !strings.Contains(s, "(default, unreviewed)") || !strings.Contains(s, "not in file  takeover_gap_pct") {
		t.Fatalf("rules policy screen:\n%s", s)
	}
	out.Reset()
	renderPolicyFiles(env, []rpc.PolicyFileStatus{
		{Policy: "protection", Path: "/p/protection-policy.toml", Status: "active", Review: rpc.PolicyReviewUnreviewed, PolicyID: "protection-mvp", PolicyVersion: "1",
			NeedsYourNumber: []string{"automatic submission: off until you list reduce-only buckets under [authority].pre_authorised"}, Notes: []string{"a note"}},
		{Policy: "rulebook", Path: "/p/rulebook-policy.toml", Status: "error", Review: rpc.PolicyReviewUnreviewed},
	}, false)
	s := out.String()
	if !strings.Contains(s, "default, unreviewed") || !strings.Contains(s, "needs your number: automatic submission") ||
		!strings.Contains(s, "error (default, unreviewed)") || strings.Contains(s, "a note") {
		t.Fatalf("policy files:\n%s", s)
	}
}
