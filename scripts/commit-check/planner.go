package main

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

type stagedChange struct {
	status  byte
	oldMode string
	newMode string
	paths   []string
}

type checkPlan struct {
	classes []string
	targets []string
	full    bool
	reason  string
	hasGo   bool
	changed []string
}

var fastTargetOrder = []string{
	"account-data-check",
	"product-identity-check",
	"gofmt-check",
	"go-doc-check",
	"vet-check",
	"staticcheck-check",
	"modernize-check",
	"govulncheck-check",
	"parity-check",
	"docs-check",
	"docs-html-check",
	"app-check",
	"remote-relay-check",
	"changelog-check",
}

func planChecks(changes []stagedChange) checkPlan {
	plan := checkPlan{}
	if len(changes) == 0 {
		plan.full = true
		plan.reason = "no staged changes were found"
		plan.targets = []string{"check"}
		return plan
	}

	classSet := map[string]bool{}
	targetSet := map[string]bool{
		"account-data-check":     true,
		"product-identity-check": true,
	}
	pathSet := map[string]bool{}
	for _, change := range changes {
		if change.oldMode == "120000" || change.newMode == "120000" ||
			change.oldMode == "160000" || change.newMode == "160000" {
			return fullPlan(changes, "symlink or submodule change requires the full gate")
		}
		for _, candidate := range change.paths {
			cleaned, err := cleanRepoPath(candidate)
			if err != nil {
				return fullPlan(changes, err.Error())
			}
			pathSet[cleaned] = true
			classes, targets, known, sensitiveReason := classifyPath(cleaned)
			if sensitiveReason != "" {
				return fullPlan(changes, sensitiveReason)
			}
			if !known {
				return fullPlan(changes, fmt.Sprintf("unclassified path %q", cleaned))
			}
			for _, class := range classes {
				classSet[class] = true
				if class == "go" {
					plan.hasGo = true
				}
			}
			for _, target := range targets {
				targetSet[target] = true
			}
		}
	}

	for _, class := range []string{"go", "generated-authority", "app", "relay", "docs", "changelog"} {
		if classSet[class] {
			plan.classes = append(plan.classes, class)
		}
	}
	for _, target := range fastTargetOrder {
		if targetSet[target] {
			plan.targets = append(plan.targets, target)
		}
	}
	for candidate := range pathSet {
		plan.changed = append(plan.changed, candidate)
	}
	sort.Strings(plan.changed)
	return plan
}

func fullPlan(changes []stagedChange, reason string) checkPlan {
	pathSet := map[string]bool{}
	for _, change := range changes {
		for _, candidate := range change.paths {
			pathSet[candidate] = true
		}
	}
	var changed []string
	for candidate := range pathSet {
		changed = append(changed, candidate)
	}
	sort.Strings(changed)
	return checkPlan{
		classes: []string{"full-fallback"},
		targets: []string{"check"},
		full:    true,
		reason:  reason,
		changed: changed,
	}
}

func cleanRepoPath(candidate string) (string, error) {
	if candidate == "" || strings.ContainsRune(candidate, '\x00') {
		return "", fmt.Errorf("invalid staged path %q", candidate)
	}
	if strings.HasPrefix(candidate, "/") || candidate == "." || candidate == ".." {
		return "", fmt.Errorf("path escapes repository scope: %q", candidate)
	}
	cleaned := path.Clean(candidate)
	if cleaned != candidate || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("non-canonical staged path %q", candidate)
	}
	return cleaned, nil
}

func classifyPath(candidate string) (classes, targets []string, known bool, sensitiveReason string) {
	if reason := authoritySensitiveReason(candidate); reason != "" {
		return nil, nil, false, reason
	}

	add := func(class string, classTargets ...string) {
		classes = append(classes, class)
		targets = append(targets, classTargets...)
		known = true
	}

	if candidate == "CHANGELOG.md" {
		add("changelog", "changelog-check")
		return
	}
	if strings.HasPrefix(candidate, "web/app/") || isAllowlistedAppScript(candidate) {
		add("app", "app-check")
	}
	if strings.HasPrefix(candidate, "cloudflare/remote-relay/") {
		add("relay", "remote-relay-check")
	}
	if strings.HasSuffix(candidate, ".go") {
		add("go",
			"gofmt-check",
			"go-doc-check",
			"vet-check",
			"staticcheck-check",
			"modernize-check",
			"govulncheck-check",
		)
		if strings.HasPrefix(candidate, "cmd/") ||
			strings.HasPrefix(candidate, "internal/mcp/") ||
			strings.HasPrefix(candidate, "internal/config/") ||
			strings.HasPrefix(candidate, "scripts/docgen/") {
			add("generated-authority", "parity-check", "docs-check", "docs-html-check")
		}
	}
	if isPublicDocumentation(candidate) {
		add("docs", "docs-html-check")
		if strings.HasPrefix(candidate, "docs/docs/reference/") ||
			strings.HasPrefix(candidate, "docs/reference/") {
			targets = append(targets, "docs-check")
		}
	}
	return
}

func authoritySensitiveReason(candidate string) string {
	switch {
	case candidate == "Makefile":
		return "Makefile changed"
	case candidate == "go.mod" || candidate == "go.sum":
		return "Go dependency or toolchain authority changed"
	case candidate == "AGENTS.md":
		return "repository instructions changed"
	case strings.HasPrefix(candidate, ".github/workflows/"):
		return "CI workflow authority changed"
	case strings.HasPrefix(candidate, ".codex/") ||
		strings.HasPrefix(candidate, ".claude/") ||
		strings.HasPrefix(candidate, ".agents/"):
		return "agent or release authority changed"
	case strings.HasPrefix(candidate, ".claude-plugin/") ||
		candidate == "server.json":
		return "plugin or registry metadata changed"
	case strings.HasPrefix(candidate, "scripts/") && !isAllowlistedAppScript(candidate):
		return "verification or release script changed"
	}
	return ""
}

func isAllowlistedAppScript(candidate string) bool {
	switch candidate {
	case "scripts/app-browser-smoke.mjs",
		"scripts/app-screenshots.mjs",
		"scripts/check-app-icons.mjs":
		return true
	}
	return false
}

func isPublicDocumentation(candidate string) bool {
	if strings.HasPrefix(candidate, "docs/") {
		return true
	}
	switch candidate {
	case "README.md", "CONTRIBUTING.md", "SECURITY.md", "PRIVACY.md":
		return true
	}
	return false
}
