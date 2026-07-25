package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/cli"
	"github.com/osauer/ibkr/v2/internal/mcp"
)

func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	first := render(cli.Catalog())
	second := render(cli.Catalog())
	if first != second {
		t.Fatal("render is not byte-deterministic across runs")
	}
}

func TestRenderCoversEveryCommand(t *testing.T) {
	t.Parallel()
	body := render(cli.Catalog())
	for _, spec := range cli.Catalog() {
		heading := fmt.Sprintf("## `ibkr %s`\n", spec.Name)
		if !strings.Contains(body, heading) {
			t.Errorf("command %q has no section in the generated page", spec.Name)
		}
		if !strings.Contains(body, fmt.Sprintf("(#ibkr-%s)", spec.Name)) {
			t.Errorf("command %q is missing from the summary table", spec.Name)
		}
	}
}

// Registry order is what the help table shows and what keeps `status` first,
// so the page must not silently sort itself.
func TestRenderKeepsRegistryOrder(t *testing.T) {
	t.Parallel()
	body := render(cli.Catalog())
	rest := body
	for _, spec := range cli.Catalog() {
		heading := fmt.Sprintf("## `ibkr %s`\n", spec.Name)
		index := strings.Index(rest, heading)
		if index < 0 {
			t.Fatalf("command %q is out of registry order in the generated page", spec.Name)
		}
		rest = rest[index+len(heading):]
	}
}

func TestRenderReportsMCPCounterpartFromExclusions(t *testing.T) {
	t.Parallel()
	body := render(cli.Catalog())
	for _, spec := range cli.Catalog() {
		reason, cliOnly := mcp.ExcludedCLI[spec.Name]
		if !cliOnly {
			continue
		}
		if !strings.Contains(body, strings.TrimRight(reason, ".")) {
			t.Errorf("command %q is CLI-only but its recorded reason is not rendered", spec.Name)
		}
	}
}

// `ibkr order` reaches the broker. The page must say so rather than listing it
// as an ordinary command.
func TestRenderFlagsGatedBrokerWrites(t *testing.T) {
	t.Parallel()
	body := render(cli.Catalog())
	for _, want := range []string{
		"Broker writes are gated.",
		"Guard `read-only`, with `confirm` subcommands.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generated page is missing %q", want)
		}
	}
}

// strictestGuard must never under-report: a read-only command that owns a
// confirm subcommand is summarized as confirm.
func TestStrictestGuardTakesTheSubcommandMaximum(t *testing.T) {
	t.Parallel()
	spec := cli.CommandSpec{
		Guard: cli.GuardReadOnly,
		Subcommands: []cli.SubcommandSpec{
			{Name: "show", Guard: cli.GuardReadOnly},
			{Name: "place", Guard: cli.GuardConfirm},
		},
	}
	if got := strictestGuard(spec); got != cli.GuardConfirm {
		t.Fatalf("strictestGuard = %q, want %q", got, cli.GuardConfirm)
	}
	local := cli.CommandSpec{Guard: cli.GuardLocal, Subcommands: []cli.SubcommandSpec{{Name: "list", Guard: cli.GuardReadOnly}}}
	if got := strictestGuard(local); got != cli.GuardLocal {
		t.Fatalf("strictestGuard = %q, want %q", got, cli.GuardLocal)
	}
}

// The checked-in page is the artifact `make docs-check` diffs against. This
// catches a registry change committed without a regeneration.
func TestCheckedInPageMatchesGenerator(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "..", defaultOutput)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != render(cli.Catalog()) {
		t.Fatalf("%s is out of date; run `make docs-regen`", defaultOutput)
	}
}
