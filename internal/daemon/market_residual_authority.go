package daemon

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	contractAuthorityScope    = "market/contracts"
	contractStateKind         = "contract_cache.current.v3"
	contractObservationKind   = "contract_cache.snapshot.v3"
	contractObservationSource = "ibkr.tws.contract_details"
)

type coreContractCacheAuthority struct {
	store *corestore.Store
}

func (a coreContractCacheAuthority) LoadContractCache() ([]byte, bool, error) {
	return loadMarketState(a.store, contractAuthorityScope, contractStateKind)
}

func (a coreContractCacheAuthority) SaveContractCache(payload []byte, observedAt time.Time) error {
	var envelope struct {
		Version     int                        `json:"version"`
		MembersHash string                     `json:"members_hash"`
		Contracts   map[string]json.RawMessage `json:"contracts"`
		Options     map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode contract cache metadata: %w", err)
	}
	metadata, err := json.Marshal(struct {
		Version       int    `json:"version"`
		MembersHash   string `json:"members_hash,omitempty"`
		ContractCount int    `json:"contract_count"`
		OptionCount   int    `json:"option_count"`
		Method        string `json:"method"`
	}{envelope.Version, envelope.MembersHash, len(envelope.Contracts), len(envelope.Options), "IBKR contract details"})
	if err != nil {
		return fmt.Errorf("encode contract cache metadata: %w", err)
	}
	return saveMarketState(a.store, contractAuthorityScope, contractStateKind, corestore.ObservationInput{
		ScopeKey: contractAuthorityScope, Source: contractObservationSource,
		Kind: contractObservationKind, ObservedAt: observedAt,
		ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
	})
}
