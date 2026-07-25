package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/osauer/ibkr/v2/internal/risk"
)

// TestRiskPolicyTemplateLoads gates template drift for the one policy that
// ships as a file: examples/risk-policy.toml must decode with no unknown
// keys and pass Constitution validation, mirroring what riskPolicyManager
// does with a user copy — a template the loader rejects would hand every
// user who follows internal-docs/design/risk-policy.md a dead policy file.
func TestRiskPolicyTemplateLoads(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "risk-policy.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	var c risk.Constitution
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		t.Fatalf("shipped template must decode: %v", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		t.Fatalf("shipped template has unknown keys: %v", undecoded)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("shipped template must validate: %v", err)
	}
}
