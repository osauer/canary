package agentconfig

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The hook lets an agent's settings write through only for the registry's
// collection keys (internal-docs/design/strategy-register.md, "Settings:
// classes the daemon enforces"). Its list is a copy, so it must name exactly
// the keys the registry classes as collection: a key added there without
// the hook stays refused for agents, and a key the hook names that the
// registry does not class as collection would pass the hook and be refused
// by the daemon.
func TestHookCollectionKeysMatchRegistry(t *testing.T) {
	raw, err := os.ReadFile(repoPath("hooks", "canary-pre-tool-use.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^collection_settings_keys=\(([^)]*)\)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("the hook carries no collection_settings_keys list")
	}
	hook := strings.Fields(string(m[1]))
	var registry []string
	for _, spec := range rpc.SettingsKeys() {
		if spec.Class == rpc.SettingsClassCollection {
			registry = append(registry, spec.Key)
		}
	}
	slices.Sort(hook)
	slices.Sort(registry)
	if !slices.Equal(hook, registry) {
		t.Fatalf("hook collection keys %v, registry collection class %v", hook, registry)
	}
}
