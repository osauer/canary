package daemon

import (
	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
)

func accountSettlementObservation(in *broker.SettlementObservation, provenance broker.Provenance) *rpc.AccountSettlementObservation {
	if in == nil || provenance != broker.ProvenanceRequest {
		return nil
	}
	out := &rpc.AccountSettlementObservation{AsOf: in.AsOf, Callbacks: in.Callbacks, Rows: make([]rpc.AccountSettlementRow, 0, len(in.Rows))}
	for _, row := range in.Rows {
		out.Rows = append(out.Rows, rpc.AccountSettlementRow{Currency: row.Currency, Source: row.Source, Finite: row.Finite})
	}
	return out
}
