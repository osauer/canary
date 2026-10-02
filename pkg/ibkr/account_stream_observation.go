package ibkr

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AccountStreamObservation is passive receipt metadata from the existing
// reqAccountUpdates subscription. No balance, account identifier or execution
// authority is exposed. All clocks are local receive/read clocks, not broker
// valuation timestamps; completion does not make individual values fresh.
type AccountStreamObservation struct {
	Status, ClockSource                     string
	SocketEpoch                             uint64
	RequestedAt, DownloadEndAt, CompletedAt time.Time
	ReadAt, LastCallbackAt                  time.Time
	AccountReady                            string
	AccountReadyAt                          time.Time
	TradingType                             string
	TradingTypeObserved                     bool
	TradingTypeAt                           time.Time
	Rows                                    []AccountStreamRow
	RowsTruncated                           bool
}

// AccountStreamRow describes an allowlisted exact wire key and currency.
// ValueStatus distinguishes an observed real zero from invalid/unset data
// without exposing the amount. Source labels are diagnostic, never authority.
type AccountStreamRow struct {
	Key, Currency, Source, ValueStatus string
	Callbacks                          int
	FirstReceivedAt, LastReceivedAt    time.Time
}

type accountStreamReceipt struct {
	account, accountReady, tradingType string
	epoch                              uint64
	requestedAt, downloadEndAt         time.Time
	lastCallbackAt, accountReadyAt     time.Time
	tradingTypeAt                      time.Time
	tradingTypeObserved                bool
	rows                               map[string]AccountStreamRow
	truncated                          bool
}

// Called with accountMu held by the existing account-value handler. This does
// not admit a row to any financial snapshot or change subscription behavior.
func (c *Connection) observeAccountStreamValueLocked(account, key, value, currency string, at time.Time) {
	r := &c.accountStreamReceipt
	if r.requestedAt.IsZero() || r.epoch != c.BrokerSessionEpoch() || !accountCodeConcrete(r.account) || !strings.EqualFold(account, r.account) {
		return
	}
	r.lastCallbackAt = at
	if key == "AccountReady" {
		r.accountReady = "invalid"
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			r.accountReady = "ready"
		case "false":
			r.accountReady = "not_ready"
		}
		r.accountReadyAt = at
		return
	}
	if key == "TradingType-S" {
		r.tradingTypeObserved, r.tradingTypeAt = true, at
		r.tradingType = classifyStreamTradingType(value)
		return
	}
	field, prefixed := splitGatewayLedgerTag(key)
	if !streamCashDiagnosticField(field) {
		return
	}
	// The allowlist rejects reserved raw namespaces and suffixed storage keys.
	// Invalid currencies retain only the fact of invalidity, never broker text.
	source := "unlabelled"
	if !concreteAccountSummaryLedgerCurrency(currency) && currency != "BASE" && currency != "" {
		currency, source = "", "invalid_currency"
	} else if prefixed && concreteAccountSummaryLedgerCurrency(currency) {
		source = "broker_ledger_label"
	} else if field == "SettledCash" || field == "TotalCashValue" || strings.HasSuffix(field, "-S") || strings.HasSuffix(field, "-C") {
		source = "account_total"
	} else if field == "CashBalance" && concreteAccountSummaryLedgerCurrency(currency) {
		source = "currency_cash_balance"
	}
	status := "invalid"
	if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
		status = "observed"
		if parsed == math.MaxFloat64 {
			status = "unset"
		}
	}
	if r.rows == nil {
		r.rows = map[string]AccountStreamRow{}
	}
	id := key + "\x00" + currency
	row, exists := r.rows[id]
	if !exists && len(r.rows) >= 128 {
		r.truncated = true
		return
	}
	if !exists {
		row = AccountStreamRow{Key: key, Currency: currency, FirstReceivedAt: at}
	}
	row.Source, row.ValueStatus, row.LastReceivedAt = source, status, at
	if row.Callbacks < 1_000_000 {
		row.Callbacks++
	}
	r.rows[id] = row
}

func streamCashDiagnosticField(field string) bool {
	switch field {
	case "SettledCash", "CashBalance", "TotalCashBalance", "TotalCashValue", "TotalCashValue-S", "TotalCashValue-C", "EquityWithLoanValue-S", "EquityWithLoanValue-C", "FuturesPNL":
		return true
	}
	return false
}

func classifyStreamTradingType(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "CASH":
		return "cash"
	case "MARGIN", "REGT", "PORTFOLIO", "REG-T-MARGIN", "REG T MARGIN", "REG-T MARGIN", "PORTFOLIO MARGIN", "PORTFOLIO-MARGIN":
		return "margin"
	default:
		// Recognize display labels only, never ownership labels or internal tokens.
		return "unknown"
	}
}

func (c *Connection) observeAccountStreamEnd(account string, at time.Time) {
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	r := &c.accountStreamReceipt
	if !r.requestedAt.IsZero() && r.epoch == c.BrokerSessionEpoch() && accountCodeConcrete(r.account) && strings.EqualFold(strings.TrimSpace(account), r.account) && r.downloadEndAt.IsZero() {
		r.downloadEndAt = at
	}
}

// CaptureAccountStreamObservationForSession inspects the already-running
// subscription within the caller's original binding. It sends no request and
// never samples a successor session as a substitute for that binding.
func (c *Connector) CaptureAccountStreamObservationForSession(binding ConnectorSessionBinding, expected string) *AccountStreamObservation {
	if c == nil || !accountCodeConcrete(expected) || binding.connector != c || binding.connection == nil {
		return nil
	}
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	binding.connection.inboundEpochMu.Lock()
	defer binding.connection.inboundEpochMu.Unlock()
	c.evidenceBarrier.Lock()
	defer c.evidenceBarrier.Unlock()
	if !c.SessionCurrent(binding) {
		return nil
	}
	conn := binding.connection
	conn.portfolioHealthMu.RLock()
	health := conn.portfolioHealth
	conn.portfolioHealthMu.RUnlock()
	conn.accountMu.RLock()
	defer conn.accountMu.RUnlock()
	r := conn.accountStreamReceipt
	out := &AccountStreamObservation{Status: "no_subscription", ClockSource: "local_receive", SocketEpoch: binding.epoch, ReadAt: time.Now().UTC(), AccountReady: "unknown", TradingType: "unknown", Rows: []AccountStreamRow{}}
	if r.requestedAt.IsZero() {
		return out
	}
	if r.epoch != binding.epoch || !strings.EqualFold(r.account, expected) || !strings.EqualFold(health.Account, expected) ||
		(!r.requestedAt.Equal(health.RequestedAt) && health.ScopeConflictAt.IsZero() && health.InvalidPayloadAt.IsZero()) {
		out.Status = "scope_or_generation_changed"
		return out
	}
	out.RequestedAt, out.DownloadEndAt, out.CompletedAt = r.requestedAt, r.downloadEndAt, health.InitialCompletedAt
	out.LastCallbackAt = r.lastCallbackAt
	out.AccountReadyAt, out.TradingTypeAt, out.TradingTypeObserved = r.accountReadyAt, r.tradingTypeAt, r.tradingTypeObserved
	if r.accountReady != "" {
		out.AccountReady = r.accountReady
	}
	if r.tradingType != "" {
		out.TradingType = r.tradingType
	}
	out.RowsTruncated = r.truncated
	for _, row := range r.rows {
		out.Rows = append(out.Rows, row)
	}
	slices.SortFunc(out.Rows, func(a, b AccountStreamRow) int {
		return strings.Compare(a.Key+"\x00"+a.Currency, b.Key+"\x00"+b.Currency)
	})
	out.Status = "initial_pending"
	switch {
	case !health.ScopeConflictAt.IsZero():
		out.Status = "scope_conflict"
	case !health.InvalidPayloadAt.IsZero():
		out.Status = "invalid_payload"
	case !health.DownloadShortAt.IsZero():
		out.Status = "download_short"
	case out.AccountReady == "not_ready" || out.AccountReady == "invalid":
		out.Status = "account_not_ready"
	case !health.InitialCompletedAt.IsZero():
		out.Status = "initial_complete"
	}
	return out
}
