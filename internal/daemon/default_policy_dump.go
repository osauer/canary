package daemon

import (
	"bytes"
	"fmt"

	"github.com/BurntSushi/toml"
)

// DefaultPolicyTOML renders the embedded default for a policy name
// ("protection" or "opportunity") as activation-ready TOML. It backs
// `canary policy default <name>`: no template file ships for these policies,
// so the printable embedded default is the single source and cannot drift
// from the code the daemon actually runs.
func DefaultPolicyTOML(name string) ([]byte, error) {
	var policy any
	switch name {
	case "protection":
		policy = defaultProtectionPolicy()
	case "opportunity":
		policy = defaultOpportunityPolicy()
	default:
		return nil, fmt.Errorf("unknown policy %q (expected protection or opportunity)", name)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(policy); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
