package daemon

import (
	"fmt"
)

// DefaultPolicyTOML renders Canary's default file for a policy name
// ("rulebook", "protection", "opportunity" or "constitution"): the same
// commented template Canary writes when the file is missing. It backs
// `canary policy default <name>`, so the printed file cannot drift from the
// code the daemon runs. The constitution template carries placeholders only.
func DefaultPolicyTOML(name string) ([]byte, error) {
	switch name {
	case "protection":
		return ProtectionPolicyTemplate(rulebookEditRelease), nil
	case "opportunity":
		return OpportunityPolicyTemplate(rulebookEditRelease), nil
	case "rulebook":
		return RulebookPolicyTemplate(rulebookEditRelease), nil
	case "constitution", "risk":
		return ConstitutionPolicyTemplate(rulebookEditRelease), nil
	default:
		return nil, fmt.Errorf("unknown policy %q (expected rulebook, protection, opportunity or constitution)", name)
	}
}
