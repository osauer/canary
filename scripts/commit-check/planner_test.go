package main

import (
	"reflect"
	"strings"
	"testing"
)

func regularChange(paths ...string) stagedChange {
	return stagedChange{status: 'M', oldMode: "100644", newMode: "100644", paths: paths}
}

func TestPlanChecksSelectsConservativeTargetFamilies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		changes     []stagedChange
		wantClasses []string
		wantTargets []string
		wantFull    bool
	}{
		{
			name:        "ordinary Go",
			changes:     []stagedChange{regularChange("internal/risk/check.go")},
			wantClasses: []string{"go"},
			wantTargets: []string{"account-data-check", "product-identity-check", "gofmt-check", "go-doc-check", "vet-check", "staticcheck-check", "modernize-check", "govulncheck-check"},
		},
		{
			name:        "CLI Go adds generated authority",
			changes:     []stagedChange{regularChange("cmd/canary/main.go")},
			wantClasses: []string{"go", "generated-authority"},
			wantTargets: []string{"account-data-check", "product-identity-check", "gofmt-check", "go-doc-check", "vet-check", "staticcheck-check", "modernize-check", "govulncheck-check", "parity-check", "docs-check", "docs-html-check"},
		},
		{
			name:        "SPA",
			changes:     []stagedChange{regularChange("web/app/auth.js")},
			wantClasses: []string{"app"},
			wantTargets: []string{"account-data-check", "product-identity-check", "app-check"},
		},
		{
			name:        "relay",
			changes:     []stagedChange{regularChange("cloudflare/remote-relay/src/index.js")},
			wantClasses: []string{"relay"},
			wantTargets: []string{"account-data-check", "product-identity-check", "remote-relay-check"},
		},
		{
			name:        "reference docs",
			changes:     []stagedChange{regularChange("docs/docs/reference/cli.md")},
			wantClasses: []string{"docs"},
			wantTargets: []string{"account-data-check", "product-identity-check", "docs-check", "docs-html-check"},
		},
		{
			name:        "changelog",
			changes:     []stagedChange{regularChange("CHANGELOG.md")},
			wantClasses: []string{"changelog"},
			wantTargets: []string{"account-data-check", "product-identity-check", "changelog-check"},
		},
		{
			name: "rename unions classes",
			changes: []stagedChange{{
				status: 'R', oldMode: "100644", newMode: "100644",
				paths: []string{"README.md", "web/app/readme.js"},
			}},
			wantClasses: []string{"app", "docs"},
			wantTargets: []string{"account-data-check", "product-identity-check", "docs-html-check", "app-check"},
		},
		{
			name:     "Makefile falls back",
			changes:  []stagedChange{regularChange("Makefile")},
			wantFull: true,
		},
		{
			name:     "release script falls back",
			changes:  []stagedChange{regularChange("scripts/release-smoke.sh")},
			wantFull: true,
		},
		{
			name:     "unknown path falls back",
			changes:  []stagedChange{regularChange("config/example.toml")},
			wantFull: true,
		},
		{
			name: "symlink falls back",
			changes: []stagedChange{{
				status: 'A', oldMode: "000000", newMode: "120000",
				paths: []string{"docs/link.md"},
			}},
			wantFull: true,
		},
		{
			name:        "spaces and newlines stay exact",
			changes:     []stagedChange{regularChange("docs/odd name\nstill-docs.md")},
			wantClasses: []string{"docs"},
			wantTargets: []string{"account-data-check", "product-identity-check", "docs-html-check"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := planChecks(tc.changes)
			if got.full != tc.wantFull {
				t.Fatalf("full = %v, want %v (plan=%+v)", got.full, tc.wantFull, got)
			}
			if tc.wantFull {
				if !reflect.DeepEqual(got.targets, []string{"check"}) || got.reason == "" {
					t.Fatalf("full fallback lacks target/reason: %+v", got)
				}
				return
			}
			if !reflect.DeepEqual(got.classes, tc.wantClasses) {
				t.Fatalf("classes = %v, want %v", got.classes, tc.wantClasses)
			}
			if !reflect.DeepEqual(got.targets, tc.wantTargets) {
				t.Fatalf("targets = %v, want %v", got.targets, tc.wantTargets)
			}
		})
	}
}

func TestPlanChecksUnionsMultipleRecognizedClassesWithoutDuplicates(t *testing.T) {
	t.Parallel()
	got := planChecks([]stagedChange{
		regularChange("internal/risk/check.go"),
		regularChange("web/app/auth.js"),
		regularChange("README.md"),
	})
	if got.full {
		t.Fatalf("recognized union fell back: %+v", got)
	}
	counts := make(map[string]int, len(got.targets))
	for _, target := range got.targets {
		counts[target]++
	}
	for target, count := range counts {
		if count != 1 {
			t.Fatalf("target %q appears %d times: %v", target, count, got.targets)
		}
	}
	if strings.Join(got.classes, ",") != "go,app,docs" {
		t.Fatalf("classes = %v, want go,app,docs", got.classes)
	}
}

func TestCleanRepoPathRejectsEscapesAndNonCanonicalForms(t *testing.T) {
	t.Parallel()
	for _, candidate := range []string{"", "/absolute", "../escape", "docs/../Makefile", "."} {
		if _, err := cleanRepoPath(candidate); err == nil {
			t.Fatalf("cleanRepoPath(%q) succeeded", candidate)
		}
	}
}
