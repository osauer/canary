package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func accountSettlementObservation(raw *ibkrlib.RawAccountSummary, provenance ibkrlib.AccountSummaryProvenance) *rpc.AccountSettlementObservation {
	if raw == nil || provenance != ibkrlib.AccountSummaryProvenanceRequest || raw.SettlementObservation == nil {
		return nil
	}
	in := raw.SettlementObservation
	out := &rpc.AccountSettlementObservation{AsOf: in.AsOf, Callbacks: in.Callbacks, Rows: make([]rpc.AccountSettlementRow, 0, len(in.Rows))}
	for _, row := range in.Rows {
		out.Rows = append(out.Rows, rpc.AccountSettlementRow{Currency: row.Currency, Source: row.Source, Finite: row.Finite})
	}
	return out
}
