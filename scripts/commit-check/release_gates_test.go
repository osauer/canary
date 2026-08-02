package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// CHECK_TARGETS is release authority: `make check`, `make test`, hosted CI and
// the release waiter all run exactly that list. `app-check` is an aggregate the
// staged planner selects for web/app changes, but CHECK_TARGETS enumerates its
// members one by one, so a member can be added to the aggregate and never
// reach the binding gate. That is not hypothetical — app-behavior-check ran 15
// production SPA module tests that no release SHA was ever gated on, while the
// sibling gate added beside it was wired correctly by luck. Enumeration is the
// mechanism; this is the assertion that makes it safe.

func repoMakefile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	return string(data)
}

// makefileWords returns the whitespace-separated words assigned to a variable
// or listed as a target's prerequisites, with $(REFERENCES) expanded from the
// same file and any trailing `##` help text dropped.
func makefileWords(makefile, name, separator string) []string {
	var line string
	for candidate := range strings.SplitSeq(makefile, "\n") {
		if after, ok := strings.CutPrefix(candidate, name+separator); ok {
			line = after
			break
		}
	}
	if line == "" {
		return nil
	}
	if body, _, found := strings.Cut(line, "##"); found {
		line = body
	}
	var words []string
	for word := range strings.FieldsSeq(line) {
		if reference, ok := strings.CutPrefix(word, "$("); ok {
			expanded := makefileWords(makefile, strings.TrimSuffix(reference, ")"), " ?=")
			if expanded == nil {
				expanded = makefileWords(makefile, strings.TrimSuffix(reference, ")"), " =")
			}
			words = append(words, expanded...)
			continue
		}
		words = append(words, word)
	}
	return words
}

// gatesMissingFromReleaseAuthority reports every aggregate prerequisite that
// CHECK_TARGETS does not run. The aggregate itself counts as coverage: listing
// `app-check` in CHECK_TARGETS would run all of its members.
func gatesMissingFromReleaseAuthority(makefile, aggregate string) []string {
	authority := makefileWords(makefile, "CHECK_TARGETS", " =")
	if slices.Contains(authority, aggregate) {
		return nil
	}
	var missing []string
	for _, prerequisite := range makefileWords(makefile, aggregate, ":") {
		if !slices.Contains(authority, prerequisite) {
			missing = append(missing, prerequisite)
		}
	}
	return missing
}

func TestEveryAppCheckPrerequisiteIsReleaseAuthority(t *testing.T) {
	makefile := repoMakefile(t)
	if missing := gatesMissingFromReleaseAuthority(makefile, "app-check"); len(missing) > 0 {
		t.Fatalf("app-check prerequisites absent from CHECK_TARGETS, so make check/CI/release never run them: %s",
			strings.Join(missing, " "))
	}
}

// The assertion above runs from inside the list it guards, so a gate dropping
// out of CHECK_TARGETS could take the guard with it.
func TestContractGateGuardsItself(t *testing.T) {
	authority := makefileWords(repoMakefile(t), "CHECK_TARGETS", " =")
	if !slices.Contains(authority, "commit-check-contract-check") {
		t.Fatal("commit-check-contract-check left CHECK_TARGETS; the gate-wiring assertions no longer run")
	}
}

// Proof the assertion bites: remove a gate from CHECK_TARGETS and it must be
// named. An assertion that only ever passes is indistinguishable from one that
// cannot fail.
func TestReleaseAuthorityAssertionCatchesARemovedGate(t *testing.T) {
	const makefile = `CHECK_DEPS ?= plugin-check parity-check
CHECK_TARGETS = $(CHECK_DEPS) app-syntax-check app-auth-check
app-check: app-syntax-check app-auth-check app-behavior-check ## Fast SPA gate
`
	missing := gatesMissingFromReleaseAuthority(makefile, "app-check")
	if !slices.Equal(missing, []string{"app-behavior-check"}) {
		t.Fatalf("mutated Makefile reported %q, want [app-behavior-check]", missing)
	}

	restored := strings.Replace(makefile, "app-auth-check\n", "app-auth-check app-behavior-check\n", 1)
	if missing := gatesMissingFromReleaseAuthority(restored, "app-check"); missing != nil {
		t.Fatalf("restored Makefile still reports %q", missing)
	}

	viaAggregate := strings.Replace(makefile, "CHECK_TARGETS = $(CHECK_DEPS)", "CHECK_TARGETS = $(CHECK_DEPS) app-check", 1)
	if missing := gatesMissingFromReleaseAuthority(viaAggregate, "app-check"); missing != nil {
		t.Fatalf("CHECK_TARGETS running the aggregate still reports %q", missing)
	}
}
