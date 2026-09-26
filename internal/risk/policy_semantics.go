package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ConstitutionSemantics names accounting and advisory features, never trading
// permission. Schema 1 encoded these features in document revisions; schema 2
// fixes their meaning independently of the owner's revision counter.
type ConstitutionSemantics struct {
	StatementReconciliation bool
	ProcessReminders        bool
}

// Semantics is the only interpreter of historical constitution revisions.
// Call Validate before adopting a file; unknown formats have no features.
func (c Constitution) Semantics() ConstitutionSemantics {
	switch c.SchemaVersion {
	case 2:
		return ConstitutionSemantics{StatementReconciliation: true, ProcessReminders: true}
	case 0, 1: // zero also supports in-memory legacy fixtures; Validate rejects it.
		return ConstitutionSemantics{StatementReconciliation: c.PolicyVersion >= 3, ProcessReminders: c.PolicyVersion >= 4}
	default:
		return ConstitutionSemantics{}
	}
}

// EffectiveFingerprintKey identifies settings used by live evaluations. The
// historical FingerprintKey remains unchanged for provenance and stored records.
func (c Constitution) EffectiveFingerprintKey() string {
	semantics := c.Semantics()
	c.Kind, c.SchemaVersion, c.PolicyVersion = ConstitutionKind, 0, 0
	c.Drawdown.BlockEnforcement = c.EffectiveBlockEnforcement()
	c.Drawdown.Release = c.EffectiveDrawdownRelease()
	raw, err := json.Marshal(struct {
		Semantics ConstitutionSemantics
		Policy    Constitution
	}{semantics, c})
	if err != nil {
		return "" // invalid non-finite inputs cannot establish equality
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
