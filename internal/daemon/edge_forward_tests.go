package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
)

const (
	edgeForwardTestsStateKind = "edge_forward_tests"
	edgeForwardTestsVersion   = 1
)

// edgeForwardTestRegistry records when each forward-test rule was first
// registered, per account scope. It is the one Edge document that is not
// regenerable: the instants are the evidence that a rule was written before
// the results it is read on, so nothing rebuilds, resets or rewrites them.
// It lives outside the Edge publication, which every analytical version bump
// throws away, and outside the bar cache, which starts afresh on a scope
// switch. A record is only ever added: a rule whose text changed gets a new
// record with its own instant beside the old one, and another account or
// mode gets its own records without touching these.
type edgeForwardTestRegistry struct {
	Version int                                `json:"version"`
	Scopes  map[string][]edgeForwardTestRecord `json:"scopes"`
}

type edgeForwardTestRecord struct {
	ID           string    `json:"id"`
	RuleHash     string    `json:"rule_hash"`
	RegisteredAt time.Time `json:"registered_at"`
}

// edgeForwardTestRules is the shipped rule set. Tests replace it to change a
// rule's text.
var edgeForwardTestRules = edgecore.ForwardTests

func (s *Server) loadEdgeForwardTestRegistry(ctx context.Context) (edgeForwardTestRegistry, int64, error) {
	out := edgeForwardTestRegistry{Version: edgeForwardTestsVersion, Scopes: map[string][]edgeForwardTestRecord{}}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, edgeForwardTestsStateKind)
	if err != nil || !ok {
		return out, 0, err
	}
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return out, 0, fmt.Errorf("decode Edge forward-test registry: %w", err)
	}
	// A registry written by a later build is left alone: rewriting it in this
	// build's shape could drop what that build recorded.
	if out.Version > edgeForwardTestsVersion {
		return out, 0, fmt.Errorf("unsupported future Edge forward-test registry version %d", out.Version)
	}
	if out.Scopes == nil {
		out.Scopes = map[string][]edgeForwardTestRecord{}
	}
	return out, doc.Revision, nil
}

// registerEdgeForwardTests records, once, the instant each shipped rule was
// first seen for this account scope. It is safe to call on every rebuild: a
// rule already recorded with the same text keeps its instant.
func (s *Server) registerEdgeForwardTests(ctx context.Context, scopeFingerprint string) error {
	if s == nil || s.coreStore == nil || scopeFingerprint == "" {
		return nil
	}
	for range 4 {
		registry, revision, err := s.loadEdgeForwardTestRegistry(ctx)
		if err != nil {
			return err
		}
		records := registry.Scopes[scopeFingerprint]
		known := make(map[string]bool, len(records))
		for _, record := range records {
			known[record.ID+"\x00"+record.RuleHash] = true
		}
		now := s.edgeNow()
		added := false
		for _, rule := range edgeForwardTestRules() {
			hash := rule.RuleHash()
			if known[rule.ID+"\x00"+hash] {
				continue
			}
			records = append(records, edgeForwardTestRecord{ID: rule.ID, RuleHash: hash, RegisteredAt: now})
			added = true
		}
		if !added {
			return nil
		}
		registry.Version = edgeForwardTestsVersion
		registry.Scopes[scopeFingerprint] = records
		raw, err := json.Marshal(registry)
		if err != nil {
			return err
		}
		_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: edgeForwardTestsStateKind, ExpectedRevision: revision, JSON: raw})
		if err == nil {
			return nil
		}
		// Another writer registered meanwhile: read again, so its instants
		// are kept and nothing is overwritten.
		if _, conflict := err.(*corestore.RevisionConflictError); !conflict {
			return err
		}
	}
	return fmt.Errorf("edge forward-test registration conflicted repeatedly")
}

// edgeForwardTestsView serves the shipped rules with the instant each was
// registered for this scope. A rule not yet registered has no instant.
func (s *Server) edgeForwardTestsView(ctx context.Context, scopeFingerprint string) []rpc.EdgeForwardTest {
	out := []rpc.EdgeForwardTest{}
	if s == nil || s.coreStore == nil {
		return out
	}
	registry, _, err := s.loadEdgeForwardTestRegistry(ctx)
	if err != nil {
		return out
	}
	registered := map[string]time.Time{}
	for _, record := range registry.Scopes[scopeFingerprint] {
		registered[record.ID+"\x00"+record.RuleHash] = record.RegisteredAt
	}
	for _, rule := range edgeForwardTestRules() {
		hash := rule.RuleHash()
		out = append(out, rpc.EdgeForwardTest{ID: rule.ID, Title: rule.Title, Rule: rule.Rule, RuleHash: hash, RegisteredAt: registered[rule.ID+"\x00"+hash]})
	}
	return out
}

// edgeForwardSampleIDs names the rules whose sample a decision executed at
// executedAt can belong to: those registered strictly before it. Whether the
// decision meets a rule's own conditions is the reader's question.
func edgeForwardSampleIDs(tests []rpc.EdgeForwardTest, executedAt time.Time) []string {
	var out []string
	for _, test := range tests {
		if edgecore.InForwardSample(test.RegisteredAt, executedAt) {
			out = append(out, test.ID)
		}
	}
	return out
}
