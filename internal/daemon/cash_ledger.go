package daemon

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/ibkrledger"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type cashLedgerAuthority struct {
	mu      sync.Mutex
	options ibkrledger.Options
	client  *ibkrledger.Client
	source  accountSnapshotSource
	readAt  time.Time
	rows    map[string]ibkrledger.Cash
	err     error
}

func cashLedgerPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// Reads are shared briefly within the exact connector session. Every caller
// still validates the original broker timestamps and newest confirmed fills.
func (a *cashLedgerAuthority) read(ctx context.Context, options ibkrledger.Options, source accountSnapshotSource, clock func() time.Time) (map[string]ibkrledger.Cash, error) {
	now := clock().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, errors.New("cash ledger read deadline elapsed")
	}
	if options != a.options {
		a.options, a.client, a.rows, a.readAt = options, nil, nil, time.Time{}
	}
	if a.source.same(source) && !a.readAt.IsZero() && now.Sub(a.readAt) >= 0 && now.Sub(a.readAt) < accountSnapshotFreshFor {
		return maps.Clone(a.rows), a.err
	}
	a.source, a.readAt, a.rows, a.err = source, now, nil, nil
	if a.client == nil {
		a.client, a.err = ibkrledger.New(options)
	}
	if a.err == nil {
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		a.rows, a.err = a.client.Read(bounded, source.scope.Account, clock)
	}
	return maps.Clone(a.rows), a.err
}

func (s *Server) annotateWebCash(ctx context.Context, res *rpc.AccountResult, connector *ibkrlib.Connector, session ibkrlib.ConnectorSessionBinding, scope brokerStateScope, authority accountSummaryAuthority) {
	if s.cfg == nil || s.cfg.CashLedger.URL == "" || res == nil {
		return
	}
	res.CashLedger = &rpc.CashLedgerHealth{Source: "ibkr-web-api", Status: "unavailable"}
	fail := func(reason string) { res.CashLedger.Reason = reason }
	if connector == nil || !connector.SessionCurrent(session) || !brokerScopeConcrete(scope) ||
		authority.Provenance != ibkrlib.AccountSummaryProvenanceRequest || authority.AsOf.IsZero() {
		fail("cash ledger requires a current request-authored TWS account and concrete session")
		return
	}
	op := s.cfg.CashLedger
	rows, err := s.cashLedger.read(ctx, ibkrledger.Options{URL: op.URL, BearerTokenFile: cashLedgerPath(op.BearerTokenFile), CACertFile: cashLedgerPath(op.CACertFile)}, accountSnapshotSource{connector: connector, session: session, scope: scope}, s.nowUTC)
	if err != nil {
		fail(err.Error())
		return
	}
	if !connector.SessionCurrent(session) || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		fail("broker account or connector session changed during the cash ledger read")
		return
	}
	cutoff, err := s.cashLedgerFillCutoff(scope)
	if err != nil {
		fail("cash ledger cannot validate cash-affecting fill continuity")
		return
	}
	if !cutoff.IsZero() && authority.AsOf.Before(cutoff) {
		fail("TWS cash snapshot predates a confirmed fill; wait for post-fill broker balances")
		return
	}
	matched, expected, oldest := 0, 0, time.Time{}
	apply := func(row *rpc.CurrencyExposure) {
		if row == nil || !row.CashObserved {
			return
		}
		expected++
		cash, ok := rows[normCcy(row.Currency)]
		now := s.nowUTC()
		if !ok || !cash.AsOf.After(time.Time{}) || cash.AsOf.After(now) || now.Sub(cash.AsOf) > ibkrledger.MaxAge ||
			!cutoff.IsZero() && cash.AsOf.Before(cutoff) {
			return
		}
		row.WebCash = &rpc.WebCashObservation{Currency: normCcy(row.Currency), Scope: accountDataScope(scope), CashBalance: cash.Balance, SettledCash: cash.Settled, AsOf: cash.AsOf}
		matched++
		if oldest.IsZero() || cash.AsOf.Before(oldest) {
			oldest = cash.AsOf
		}
	}
	apply(res.BaseCurrencyLedger)
	for i := range res.CurrencyExposure {
		apply(&res.CurrencyExposure[i])
	}
	if matched == 0 {
		fail("cash ledger has no current matching currency balances after the latest confirmed fill")
		return
	}
	res.CashLedger.Status, res.CashLedger.AsOf = "ok", oldest
	if matched < expected {
		res.CashLedger.Status, res.CashLedger.Reason = "partial", "some currency rows are missing, stale or predate a confirmed fill"
	}
}
