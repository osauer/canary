package daemon

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

var errCashLedgerFillUnknown = errors.New("cash ledger fill evidence is unavailable")

// cashLedgerFillCutoff bounds cash receipts by the latest locally confirmed
// fill. It certifies neither settlement nor complete account-wide activity:
// offline and unobserved other-client executions remain outside this journal.
// An absent journal is unknown, including in a build without trading support.
func (s *Server) cashLedgerFillCutoff(scope brokerStateScope) (time.Time, error) {
	if !brokerScopeConcrete(scope) {
		return time.Time{}, errCashLedgerFillUnknown
	}
	if s == nil {
		return time.Time{}, fmt.Errorf("%w: %w", errCashLedgerFillUnknown, ErrTradingDisabled)
	}
	failures := s.orderLifecyclePersistenceFailures.Load()
	if s.orderLifecyclePersistenceUncertain.Load() {
		return time.Time{}, errCashLedgerFillUnknown
	}
	views, events, err := s.loadOrderViews()
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", errCashLedgerFillUnknown, err)
	}
	cutoff, err := cashLedgerFillCutoffFrom(views, events, scope, s.orderNow())
	if s.orderLifecyclePersistenceUncertain.Load() || s.orderLifecyclePersistenceFailures.Load() != failures {
		return time.Time{}, errCashLedgerFillUnknown
	}
	return cutoff, err
}

func cashLedgerFillCutoffFrom(views []rpc.OrderView, events map[string][]rpc.OrderEvent, scope brokerStateScope, now time.Time) (time.Time, error) {
	if !brokerScopeConcrete(scope) || now.IsZero() {
		return time.Time{}, errCashLedgerFillUnknown
	}
	var cutoff time.Time
	for _, view := range views {
		// A positively different account or mode cannot affect this scope. An
		// unstamped identity with fill or uncertain-send evidence cannot be ignored.
		if brokerScopeAccountConcrete(view.Account) && !strings.EqualFold(strings.TrimSpace(view.Account), strings.TrimSpace(scope.Account)) ||
			(view.Mode == rpc.AccountModePaper || view.Mode == rpc.AccountModeLive) && !strings.EqualFold(strings.TrimSpace(view.Mode), strings.TrimSpace(scope.Mode)) {
			continue
		}
		uncertain := view.SendState == orderSendStateUncertainSend || view.SendState == orderSendStateSendAttempted ||
			view.LifecycleStatus == rpc.OrderLifecycleUnknownReconcileRequired
		if uncertain || math.IsNaN(view.Filled) || math.IsInf(view.Filled, 0) || view.Filled < 0 {
			return time.Time{}, errCashLedgerFillUnknown
		}
		filled := 0.0
		var receipt time.Time
		executions := map[string]bool{}
		for _, event := range events[orderViewKey(view)] {
			if math.IsNaN(event.Filled) || math.IsInf(event.Filled, 0) || event.Filled < 0 {
				return time.Time{}, errCashLedgerFillUnknown
			}
			if event.Filled == 0 {
				continue
			}
			if event.Type != orderJournalEventStatusUpdated && event.Type != orderJournalEventBrokerAcknowledged {
				// Bookkeeping can repeat a known cumulative quantity, never confirm
				// a new fill or supply its receipt time.
				if event.Filled > filled {
					return time.Time{}, errCashLedgerFillUnknown
				}
				continue
			}
			if !brokerScopeConcrete(brokerStateScope{Account: view.Account, Mode: view.Mode}) ||
				!strings.EqualFold(strings.TrimSpace(event.Account), strings.TrimSpace(scope.Account)) ||
				!strings.EqualFold(strings.TrimSpace(event.Mode), strings.TrimSpace(scope.Mode)) ||
				event.At.IsZero() || event.At.After(now) || event.Filled < filled {
				return time.Time{}, errCashLedgerFillUnknown
			}
			if event.Filled > filled || event.ExecID != "" && !executions[event.ExecID] {
				if event.At.Before(receipt) {
					return time.Time{}, errCashLedgerFillUnknown
				}
				// At is the local durable callback receipt. ExecTime is broker text
				// and cannot move this safety frontier backwards.
				if event.At.After(cutoff) {
					cutoff = event.At.UTC()
				}
				filled = event.Filled
				receipt = event.At.UTC()
				if event.ExecID != "" {
					executions[event.ExecID] = true
				}
			}
		}
		if view.Filled != filled {
			return time.Time{}, errCashLedgerFillUnknown
		}
	}
	return cutoff, nil
}

// Proposal generation reads account cash before positions and working orders.
// Recheck at consumption, after those reads, so a newly filled buy cannot lose
// its commitment while retained pre-fill supplemental cash still funds a sweep.
func (s *Server) cashLedgerValidatePlanning(acct *rpc.AccountResult, scope brokerStateScope, in *cashSweepInput, now time.Time) {
	if acct == nil || in == nil || in.LedgerReason != "" {
		return
	}
	hasWeb := acct.BaseCurrencyLedger != nil && acct.BaseCurrencyLedger.WebCash != nil
	for _, row := range acct.CurrencyExposure {
		hasWeb = hasWeb || row.WebCash != nil
	}
	if !hasWeb {
		return // Native broker settlement observations have their own authority.
	}
	if acct.Authority == nil || acct.Authority.Scope != accountDataScope(scope) {
		in.LedgerReason = "supplemental cash belongs to a different proposal account/mode"
		return
	}
	cutoff, err := s.cashLedgerFillCutoff(scope)
	if err == nil && !cutoff.IsZero() && (acct.Authority.AsOf.IsZero() || acct.Authority.AsOf.Before(cutoff)) {
		in.LedgerReason = "TWS cash snapshot predates a confirmed fill observed during proposal generation; wait for post-fill broker balances"
		return
	}
	apply := func(row rpc.CurrencyExposure) {
		if row.WebCash == nil || err == nil && (cutoff.IsZero() || !row.WebCash.AsOf.Before(cutoff)) {
			return
		}
		ccy := normCcy(row.Currency)
		if _, ok := in.Ledger[ccy]; !ok {
			return
		}
		// Removing an unusable supplement cannot manufacture native settlement
		// evidence. A separately observed native value can still be used.
		row.WebCash = nil
		native := cashSweepCashObservation(row, acct, now)
		if native.Settled == nil {
			native.SettledReason = "supplemental cash predates a confirmed fill observed during proposal generation"
			if err != nil {
				native.SettledReason = "supplemental cash cannot validate cash-affecting fill continuity during proposal generation"
			}
		}
		in.Ledger[ccy] = native
	}
	for _, row := range acct.CurrencyExposure {
		if normCcy(row.Currency) != normCcy(acct.BaseCurrency) {
			apply(row)
		}
	}
	if row := acct.BaseCurrencyLedger; row != nil {
		apply(*row)
	}
}
