package cli

import (
	"slices"
	"testing"
)

func TestCatalogCoversCommands(t *testing.T) {
	t.Parallel()
	cmds := Commands()
	catalog := Catalog()
	if len(catalog) != len(cmds) {
		t.Fatalf("Catalog len=%d, Commands len=%d", len(catalog), len(cmds))
	}
	seen := map[string]bool{}
	for i, cmd := range cmds {
		spec := catalog[i]
		if seen[spec.Name] {
			t.Fatalf("duplicate catalog entry %q", spec.Name)
		}
		seen[spec.Name] = true
		if spec.Name != cmd.Name {
			t.Fatalf("catalog[%d].Name=%q, Commands[%d].Name=%q", i, spec.Name, i, cmd.Name)
		}
		if spec.Summary != cmd.Summary {
			t.Fatalf("%s summary drift", cmd.Name)
		}
		if spec.Usage != cmd.Usage {
			t.Fatalf("%s usage drift", cmd.Name)
		}
		if spec.Guard == "" {
			t.Fatalf("%s missing guard class", cmd.Name)
		}
		if spec.TUI == "" {
			t.Fatalf("%s missing TUI support", cmd.Name)
		}
		if spec.Group == "" {
			t.Fatalf("%s missing help group", cmd.Name)
		}
		if !slices.ContainsFunc(HelpGroups(), func(g HelpGroupSpec) bool { return g.Group == spec.Group }) {
			t.Fatalf("%s has help group %q, which HelpGroups does not render", cmd.Name, spec.Group)
		}
	}
}

func TestCatalogValueFlagsDriveHoisting(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"expiry", "width", "side", "rate", "timeout", "limit", "symbol",
		"type", "sort", "days", "by", "lookback-days", "benchmark",
		"entry", "stop", "target", "risk-pct", "lot", "fx",
		"only", "scale", "market", "exchange", "primary", "currency", "instrument", "log",
		"date", "next", "input", "min-price", "min-volume", "min-dollar-volume",
		"min-dte", "max-dte", "target-dte", "class",
		"account", "preview-token", "strategy", "tif", "replace-order",
		"order-type", "trail-percent", "trail-amount", "initial-stop", "limit-offset", "trigger-method",
		"addr", "public-url", "state-dir", "config", "socket",
		"profile", "view", "wait",
	} {
		if !isValueFlag(name) {
			t.Fatalf("isValueFlag(%q)=false, want true", name)
		}
	}
	for _, name := range []string{"json", "watch", "force", "details", "all", "save", "record", "execute", "require-live", "exclude-penny", "profiles"} {
		if isValueFlag(name) {
			t.Fatalf("isValueFlag(%q)=true, want false", name)
		}
	}
}

func TestOrderCatalogFlagsMatchHandlers(t *testing.T) {
	t.Parallel()
	specs := map[string]CommandSpec{}
	for _, spec := range Catalog() {
		specs[spec.Name] = spec
	}
	hasFlag := func(command, flag string) bool {
		for _, f := range specs[command].Flags {
			if f.Name == flag {
				return true
			}
		}
		return false
	}
	if hasFlag("orders", "account") {
		t.Fatal("orders catalog advertises --account, but orders handlers do not parse it")
	}
	if !hasFlag("order", "trigger-method") {
		t.Fatal("order catalog missing --trigger-method")
	}
}

// The subcommands() helper stamps every name read-only, which is wrong for any
// subcommand that writes. These two were wrong in exactly that way, and the
// generated CLI reference now publishes the guard, so the mistake would be
// visible to readers.
func TestSubcommandGuardsMatchWhatTheyDo(t *testing.T) {
	t.Parallel()
	want := map[string]map[string]GuardClass{
		"watch":    {"add": GuardLocal, "remove": GuardLocal, "clear": GuardLocal, "list": GuardReadOnly},
		"settings": {"show": GuardReadOnly, "set": GuardConfirm},
		"recon":    {"show": GuardReadOnly, "backtest": GuardReadOnly, "equity": GuardReadOnly, "dismiss": GuardConfirm},
	}
	for _, spec := range Catalog() {
		expected, checked := want[spec.Name]
		if !checked {
			continue
		}
		got := map[string]GuardClass{}
		for _, sub := range spec.Subcommands {
			got[sub.Name] = sub.Guard
		}
		for name, guard := range expected {
			if got[name] != guard {
				t.Errorf("%s %s guard=%q, want %q", spec.Name, name, got[name], guard)
			}
		}
	}
}
