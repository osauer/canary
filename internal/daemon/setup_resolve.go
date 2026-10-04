package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// setupResolver is the broker surface for exact underlying identity.
type setupResolver interface {
	HistoricalSessionCurrent(ibkrlib.HistoricalSessionBinding) bool
	ResolveOrderContractForSession(context.Context, ibkrlib.ConnectorSessionBinding, ibkrlib.Contract, time.Duration) (ibkrlib.ResolvedOrderContract, error)
}

// setupResolutionLimit bounds remembered resolutions; the least recently
// used goes first.
const setupResolutionLimit = 64

// setupResolutionCache remembers exact underlying resolutions per broker
// session, so a watched contract costs one contract-details request per
// broker session instead of one per evaluation. A resolution is bound to the
// session binding it was made in: a reconnect, or a binding that is no longer
// current, resolves again. Failures are not remembered.
type setupResolutionCache struct {
	mu   sync.Mutex
	rows map[string]*setupResolution
	tick uint64
}

type setupResolution struct {
	binding  ibkrlib.HistoricalSessionBinding
	resolved ibkrlib.ResolvedOrderContract
	used     uint64
}

// resolveSetupContract returns the exact contract for the requested one in
// binding's broker session, reading the broker only on a miss.
func (s *Server) resolveSetupContract(ctx context.Context, c setupResolver, binding ibkrlib.HistoricalSessionBinding, requested ibkrlib.Contract) (ibkrlib.ResolvedOrderContract, error) {
	raw, _ := json.Marshal(requested)
	key := string(raw)
	p := &s.setupResolutions
	current := c.HistoricalSessionCurrent(binding)
	p.mu.Lock()
	if row := p.rows[key]; row != nil {
		if current && row.binding == binding {
			p.tick++
			row.used = p.tick
			out := row.resolved
			p.mu.Unlock()
			return out, nil
		}
		delete(p.rows, key)
	}
	p.mu.Unlock()
	resolved, err := c.ResolveOrderContractForSession(ctx, binding, requested, 10*time.Second)
	if err != nil || !c.HistoricalSessionCurrent(binding) {
		return resolved, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[string]*setupResolution{}
	}
	for k, row := range p.rows {
		if row.binding != binding {
			delete(p.rows, k)
		}
	}
	if _, ok := p.rows[key]; !ok && len(p.rows) >= setupResolutionLimit {
		victim := ""
		for k, row := range p.rows {
			if victim == "" || row.used < p.rows[victim].used {
				victim = k
			}
		}
		delete(p.rows, victim)
	}
	p.tick++
	p.rows[key] = &setupResolution{binding: binding, resolved: resolved, used: p.tick}
	return resolved, nil
}
