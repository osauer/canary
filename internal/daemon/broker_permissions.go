package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// IBKR offers no read of an account's trading permissions, so Canary learns
// them from the broker's answer to a preview (owner decision 2026-10-29
// 07:14 CET, "Daily preview probe"): a WhatIf the broker accepts shows the
// class enabled, one it refuses for want of permission shows it not
// enabled. The daemon checks futures itself, the index hedge's class, at
// each start and then daily, with a WhatIf of one Micro S&P contract that
// sends no order; every other class is learned from the trader's own
// previews. An order or hedge in a class shown not enabled is refused before
// the broker, and the trading status says so.

const (
	// brokerPermissionsKind is the daemon.db state document of the states.
	brokerPermissionsKind = "broker_permissions"
	// brokerPermissionCheckEvery is how often the daemon checks futures
	// again: daily, the owner's choice of 2026-10-29 07:14 CET.
	brokerPermissionCheckEvery = 24 * time.Hour
	// brokerPermissionLoopEvery is how often the loop looks whether a check
	// is due or was missed for want of a gateway.
	brokerPermissionLoopEvery = 10 * time.Minute

	brokerPermissionSourceCheck   = "Canary's daily check"
	brokerPermissionSourcePreview = "your preview"
	brokerPermissionCheckOrderRef = "canary-permission-check"
)

type brokerPermissionRecord struct {
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checked_at"`
	Source    string    `json:"source"`
	Detail    string    `json:"detail,omitempty"`
}

// brokerPermissionStore keeps the states per broker scope and class in
// daemon.db, so a restart remembers them; the checks of this process are
// kept in memory, so every start checks futures once.
type brokerPermissionStore struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	records  map[string]brokerPermissionRecord
	checked  map[string]time.Time
}

func brokerPermissionKey(scope brokerStateScope, class string) string {
	return scope.Account + "|" + scope.Mode + "|" + class
}

func (p *brokerPermissionStore) bindCore(ctx context.Context, core *corestore.Store) error {
	if core == nil {
		return errors.New("broker permission store is not attached")
	}
	records, revision, err := loadBrokerPermissions(ctx, core)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.core, p.revision, p.records = core, revision, records
	p.mu.Unlock()
	return nil
}

func loadBrokerPermissions(ctx context.Context, core *corestore.Store) (map[string]brokerPermissionRecord, int64, error) {
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, brokerPermissionsKind)
	if err != nil || !ok {
		return map[string]brokerPermissionRecord{}, 0, err
	}
	records := map[string]brokerPermissionRecord{}
	if err := json.Unmarshal(doc.JSON, &records); err != nil {
		return nil, 0, fmt.Errorf("decode broker permissions: %w", err)
	}
	return records, doc.Revision, nil
}

// brokerPermissionClass names a contract's permission class, or "" for a
// contract Canary does not track.
func brokerPermissionClass(c rpc.ContractParams) string {
	switch strings.ToUpper(strings.TrimSpace(c.SecType)) {
	case "FUT", rpc.SecTypeFuture:
		return rpc.BrokerPermissionFutures
	case "OPT", rpc.SecTypeOption:
		return rpc.BrokerPermissionOptions
	case "STK", "ETF":
		return rpc.BrokerPermissionStocks
	case "BOND", "BILL":
		return rpc.BrokerPermissionBonds
	}
	return ""
}

// brokerPermissionRefusal reports whether a broker refusal is for want of
// trading permission. IBKR words these several ways; a market-data
// permission is not a trading one.
func brokerPermissionRefusal(message string) bool {
	m := strings.ToUpper(message)
	if strings.Contains(m, "MARKET DATA") {
		return false
	}
	for _, s := range []string{"PERMISSION", "NOT PERMITTED", "NOT ALLOWED TO TRADE", "NOT ELIGIBLE TO TRADE"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

func brokerPermissionDetail(message string) string {
	m := strings.Join(strings.Fields(message), " ")
	if r := []rune(m); len(r) > 240 {
		m = string(r[:240]) + "…"
	}
	return m
}

// observe records what one WhatIf showed. An acceptance shows the class
// enabled and a permission refusal not enabled. Any other answer from the
// daily check leaves a known state as it was and, while none is known, says
// why; from the trader's preview it shows nothing.
func (p *brokerPermissionStore) observe(ctx context.Context, scope brokerStateScope, class, source string, w rpc.OrderWhatIfResult, at time.Time) error {
	if class == "" {
		return nil
	}
	rec := brokerPermissionRecord{CheckedAt: at.UTC(), Source: source}
	switch {
	case w.Status == rpc.OrderWhatIfStatusAccepted:
		rec.State = rpc.BrokerPermissionEnabled
	case w.Status == rpc.OrderWhatIfStatusRejected && brokerPermissionRefusal(w.Message):
		rec.State, rec.Detail = rpc.BrokerPermissionNotEnabled, brokerPermissionDetail(w.Message)
	case source != brokerPermissionSourceCheck:
		return nil
	default:
		rec.State, rec.Detail = rpc.BrokerPermissionUnknown, "the check got no answer on permission: "+brokerPermissionDetail(nonEmptyString(w.Message, "no broker answer"))
	}
	key := brokerPermissionKey(scope, class)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records == nil {
		p.records = map[string]brokerPermissionRecord{}
	}
	if prev, ok := p.records[key]; ok && rec.State == rpc.BrokerPermissionUnknown && prev.State != rpc.BrokerPermissionUnknown {
		return nil
	}
	p.records[key] = rec
	if p.core == nil {
		return nil
	}
	for range 3 {
		raw, err := json.Marshal(p.records)
		if err != nil {
			return err
		}
		doc, err := p.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: brokerPermissionsKind, ExpectedRevision: p.revision, JSON: raw})
		if err == nil {
			p.revision = doc.Revision
			return nil
		}
		if _, conflict := errors.AsType[*corestore.RevisionConflictError](err); !conflict {
			return err
		}
		stored, revision, err := loadBrokerPermissions(ctx, p.core)
		if err != nil {
			return err
		}
		for k, r := range stored {
			if mine, ok := p.records[k]; !ok || r.CheckedAt.After(mine.CheckedAt) {
				p.records[k] = r
			}
		}
		p.revision = revision
	}
	return errors.New("broker permissions kept changing during the write")
}

// view lists the scope's states: futures always, another class once seen.
func (p *brokerPermissionStore) view(scope brokerStateScope) []rpc.BrokerPermission {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []rpc.BrokerPermission
	for _, class := range []string{rpc.BrokerPermissionFutures, rpc.BrokerPermissionOptions, rpc.BrokerPermissionStocks, rpc.BrokerPermissionBonds} {
		rec, ok := p.records[brokerPermissionKey(scope, class)]
		if !ok && class != rpc.BrokerPermissionFutures {
			continue
		}
		out = append(out, brokerPermissionView(class, rec, ok))
	}
	return out
}

func brokerPermissionView(class string, rec brokerPermissionRecord, ok bool) rpc.BrokerPermission {
	v := rpc.BrokerPermission{Class: class, State: rpc.BrokerPermissionUnknown}
	if ok {
		v.State, v.CheckedAt, v.Source, v.Detail = rec.State, rec.CheckedAt, rec.Source, rec.Detail
	}
	switch {
	case v.State == rpc.BrokerPermissionNotEnabled:
		v.Action = "Enable " + class + " under Settings › Trading Permissions in IBKR's account settings; Canary checks again at its next start or within a day."
	case v.State == rpc.BrokerPermissionUnknown && class == rpc.BrokerPermissionFutures:
		v.Action = "Canary checks with a preview once the gateway is connected and trading is on."
	}
	return v
}

// state is one class's state in the scope.
func (p *brokerPermissionStore) state(scope brokerStateScope, class string) rpc.BrokerPermission {
	p.mu.Lock()
	rec, ok := p.records[brokerPermissionKey(scope, class)]
	p.mu.Unlock()
	return brokerPermissionView(class, rec, ok)
}

// due reports whether the daemon should check class now: not yet in this
// process, or a day after its last check.
func (p *brokerPermissionStore) due(scope brokerStateScope, class string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	last, ok := p.checked[brokerPermissionKey(scope, class)]
	return !ok || now.Sub(last) >= brokerPermissionCheckEvery
}

func (p *brokerPermissionStore) markChecked(scope brokerStateScope, class string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.checked == nil {
		p.checked = map[string]time.Time{}
	}
	p.checked[brokerPermissionKey(scope, class)] = now
}

// brokerPermissionBlocker refuses an order in a class IBKR refused for want
// of permission; nil otherwise, unknown included, since a preview is how the
// state is learned.
func (s *Server) brokerPermissionBlocker(contract rpc.ContractParams) *rpc.TradingBlocker {
	class := brokerPermissionClass(contract)
	if class == "" {
		return nil
	}
	st := s.brokerPermissions.state(s.currentBrokerStateScope(), class)
	if st.State != rpc.BrokerPermissionNotEnabled {
		return nil
	}
	msg := fmt.Sprintf("%s trading is not enabled at IBKR (%s, %s)", strings.ToUpper(class[:1])+class[1:], st.Source, st.CheckedAt.Format(time.RFC3339))
	if st.Detail != "" {
		msg += ": " + st.Detail
	}
	return &rpc.TradingBlocker{Code: rpc.BrokerPermissionNotEnabledCode, Message: msg, Action: st.Action}
}

// noteBrokerPermissionPreview records the class evidence of the trader's own
// preview.
func (s *Server) noteBrokerPermissionPreview(ctx context.Context, draft rpc.OrderDraft, w rpc.OrderWhatIfResult) {
	if err := s.brokerPermissions.observe(ctx, s.currentBrokerStateScope(), brokerPermissionClass(draft.Contract), brokerPermissionSourcePreview, w, s.nowUTC()); err != nil {
		s.warnf("broker permissions: %v", err)
	}
}

// startBrokerPermissionChecks runs the futures check at start and daily.
func (s *Server) startBrokerPermissionChecks(ctx context.Context) {
	if !s.productionStateDatabase {
		return
	}
	s.macroLoopWG.Go(func() {
		ticker := time.NewTicker(brokerPermissionLoopEvery)
		defer ticker.Stop()
		for {
			if status := s.currentTradingStatus(); status.CanPreview {
				s.checkBrokerPermissions(ctx, status)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// checkBrokerPermissions checks futures when due: a WhatIf to buy one Micro
// S&P contract at half its price, a limit that cannot fill, that the broker
// evaluates and never works. It reports whether it checked.
func (s *Server) checkBrokerPermissions(ctx context.Context, status rpc.TradingStatus) bool {
	scope, now := s.currentBrokerStateScope(), s.nowUTC()
	class := rpc.BrokerPermissionFutures
	if !s.brokerPermissions.due(scope, class, now) {
		return false
	}
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	contract, quote, err := s.reduceHedgeFuture(readCtx)
	if err != nil || contract.ConID == 0 {
		s.warnf("broker permissions: futures check skipped, no Micro S&P contract: %v", err)
		return false
	}
	draft := rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: contract, Quantity: 1, OrderType: rpc.OrderTypeLMT,
		LimitPrice: brokerPermissionCheckPrice(hedgeQuotePrice(quote)), TIF: rpc.OrderTIFDay, OrderRef: brokerPermissionCheckOrderRef}
	w, err := s.fetchPreviewWhatIfBound(readCtx, status, draft, orderPreviewDefaultWait, nil)
	if err != nil {
		s.warnf("broker permissions: futures check failed: %v", err)
		return false
	}
	if w.Status != rpc.OrderWhatIfStatusAccepted && w.Status != rpc.OrderWhatIfStatusRejected {
		return false
	}
	s.brokerPermissions.markChecked(scope, class, now)
	if err := s.brokerPermissions.observe(ctx, scope, class, brokerPermissionSourceCheck, w, now); err != nil {
		s.warnf("broker permissions: %v", err)
	}
	return true
}

// brokerPermissionCheckPrice is half the future's price on its 0.25 tick,
// at least one tick: a buy limit far below the market.
func brokerPermissionCheckPrice(price float64) float64 {
	return max(0.25, math.Floor(price/2/0.25)*0.25)
}
