package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type flexEvidenceSelection struct {
	ActiveQueryFingerprint string
	IncludeAll             bool
}

func flexQueryFingerprint(queryID string) string {
	queryID = strings.TrimSpace(queryID)
	if queryID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("canary.flex-query.v1\x00" + queryID))
	return "query_" + hex.EncodeToString(digest[:16])
}

func validFlexQueryFingerprint(value string) bool {
	if len(value) != len("query_")+32 || !strings.HasPrefix(value, "query_") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "query_"))
	return err == nil
}

func (s *Server) configuredFlexQueryFingerprint() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return flexQueryFingerprint(s.cfg.Flex.QueryID)
}

// flexEvidenceSelection derives authority directly from the configured Query
// ID. Untagged pre-generation XML remains immutable broker evidence, but a
// configured query must refetch it under its opaque tag before it can certify
// Reporting, Recon, or Edge.
func (s *Server) flexEvidenceSelection() flexEvidenceSelection {
	return flexEvidenceSelection{ActiveQueryFingerprint: s.configuredFlexQueryFingerprint()}
}

func statementProjectionScopeForSelection(selection flexEvidenceSelection) string {
	if validFlexQueryFingerprint(selection.ActiveQueryFingerprint) {
		return statementProjectionScope + ":" + selection.ActiveQueryFingerprint
	}
	return statementProjectionScope
}

func (s *Server) activeStatementProjectionScope() string {
	return statementProjectionScopeForSelection(s.flexEvidenceSelection())
}

// retainedFlexFileQueryFingerprint returns tagged=false for pre-generation
// filenames. A filename that claims the tagged namespace but is malformed is
// rejected instead of being silently treated as legacy evidence.
func retainedFlexFileQueryFingerprint(name string) (fingerprint string, tagged bool, err error) {
	if !strings.HasPrefix(name, "flex-query_") {
		return "", false, nil
	}
	rest := strings.TrimPrefix(name, "flex-")
	before, _, ok := strings.Cut(rest, "-")
	if !ok {
		return "", true, fmt.Errorf("malformed generated Flex evidence filename")
	}
	fingerprint = before
	if !validFlexQueryFingerprint(fingerprint) {
		return "", true, fmt.Errorf("malformed generated Flex evidence fingerprint")
	}
	return fingerprint, true, nil
}

func (selection flexEvidenceSelection) includesRetainedFlexFile(name string) (bool, error) {
	fingerprint, tagged, err := retainedFlexFileQueryFingerprint(name)
	if err != nil {
		return false, err
	}
	if selection.IncludeAll {
		return true, nil
	}
	if tagged {
		return fingerprint == selection.ActiveQueryFingerprint, nil
	}
	if selection.ActiveQueryFingerprint == "" {
		return true, nil
	}
	return false, nil
}
