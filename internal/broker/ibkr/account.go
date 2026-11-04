package ibkr

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Provenance maps the client's summary provenance onto the seam's; the two
// spell the same values.
func Provenance(p ibkrlib.AccountSummaryProvenance) broker.Provenance {
	return broker.Provenance(p)
}

// Ledger maps the per-currency rollup. The two row types are field for
// field the same, so each row converts directly; a nil map stays nil.
func Ledger(in map[string]ibkrlib.CurrencyLedger) map[string]broker.CurrencyLedger {
	if in == nil {
		return nil
	}
	out := make(map[string]broker.CurrencyLedger, len(in))
	for ccy, row := range in {
		out[ccy] = broker.CurrencyLedger(row)
	}
	return out
}

// Settlement maps the sanitized settlement callback provenance; nil stays
// nil and the rows are copied, never shared.
func Settlement(in *ibkrlib.AccountSettlementObservation) *broker.SettlementObservation {
	if in == nil {
		return nil
	}
	out := &broker.SettlementObservation{AsOf: in.AsOf, Callbacks: in.Callbacks}
	if in.Rows != nil {
		out.Rows = make([]broker.SettlementRow, 0, len(in.Rows))
		for _, row := range in.Rows {
			out.Rows = append(out.Rows, broker.SettlementRow{Currency: row.Currency, Source: row.Source, Finite: row.Finite})
		}
	}
	return out
}

// Summary maps one raw account summary. Money fields are carried as the
// pointers the client set: nil stays nil (the broker did not send the
// field) and is never read as zero. The pointers are shared with the raw
// value; the daemon clones what it keeps.
func Summary(raw *ibkrlib.RawAccountSummary) *broker.AccountSummary {
	if raw == nil {
		return nil
	}
	return &broker.AccountSummary{
		AccountID:              raw.AccountID,
		AccountType:            raw.AccountType,
		NetLiquidation:         raw.NetLiquidation,
		BuyingPower:            raw.BuyingPower,
		AvailableFunds:         raw.AvailableFunds,
		ExcessLiquidity:        raw.ExcessLiquidity,
		TotalCashValue:         raw.TotalCashValue,
		MaintenanceMargin:      raw.MaintenanceMargin,
		InitMarginReq:          raw.InitMarginReq,
		GrossPositionValue:     raw.GrossPositionValue,
		UnrealizedPnL:          raw.UnrealizedPnL,
		RealizedPnL:            raw.RealizedPnL,
		Cushion:                raw.Cushion,
		LookAheadInitMargin:    raw.LookAheadInitMargin,
		LookAheadMaintMargin:   raw.LookAheadMaintMargin,
		LookAheadAvailable:     raw.LookAheadAvailable,
		LookAheadExcess:        raw.LookAheadExcess,
		Currency:               raw.Currency,
		BaseCurrency:           raw.BaseCurrency,
		BaseCurrencyProvenance: broker.BaseCurrencyProvenance(raw.BaseCurrencyProvenance),
		CurrencyLedger:         Ledger(raw.CurrencyLedger),
		Settlement:             Settlement(raw.SettlementObservation),
		AsOf:                   raw.AsOf,
		Raw:                    raw.Raw,
	}
}

// Summary performs one bounded one-shot summary read of the pinned account,
// as the daemon's account snapshot does today. The provenance says whether
// the request supplied the rows or the client's unstamped stream cache did.
func (a *Adapter) Summary(ctx context.Context, timeout time.Duration) (*broker.AccountSummary, broker.Provenance, error) {
	if a == nil || a.conn == nil {
		return nil, "", ibkrlib.ErrIBKRUnavailable
	}
	raw, provenance, err := a.conn.RequestAccountSummaryWithProvenance(ctx, timeout)
	return Summary(raw), Provenance(provenance), err
}
