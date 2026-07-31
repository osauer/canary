package daemon

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	contractAuthorityScope = "market/contracts"
	contractStateKind      = "contract_cache.current.v3"
)

type coreContractCacheAuthority struct {
	store *corestore.Store
}

func (a coreContractCacheAuthority) LoadContractCache() ([]byte, bool, error) {
	return loadMarketState(a.store, contractAuthorityScope, contractStateKind)
}

// SaveContractCache publishes the cache as current state only. It deliberately
// appends no observation: the contract cache is a local copy of contract
// details IBKR re-serves on request, nothing but the next boot reads it, and no
// decision rests on it. Writing the whole cache into the immutable ledger once
// a minute put 5.1 GB of unread snapshots in daemon.db, and because every boot
// re-hashes the ledger before opening the socket, that cost was paid as startup
// latency. See internal-docs/design/authority-contract-cache-bloat.md.
func (a coreContractCacheAuthority) SaveContractCache(payload []byte, _ time.Time) error {
	return saveMarketDocument(context.Background(), a.store, contractAuthorityScope, contractStateKind, payload)
}
