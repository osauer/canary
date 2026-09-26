package rpc

// EffectivePolicyFingerprintVersion identifies live settings independently of
// descriptive metadata. Provenance fingerprints retain their existing meaning.
const EffectivePolicyFingerprintVersion = "effective-policy-v1"

// PolicyDiagnostic explains one file setting without implying trading authority.
type PolicyDiagnostic struct {
	Key     string `json:"key"`
	Feature string `json:"feature"`
	Message string `json:"message"`
}
