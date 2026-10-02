package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func accountStreamObservation(in *ibkrlib.AccountStreamObservation) *rpc.AccountStreamObservation {
	if in == nil {
		return nil
	}
	out := &rpc.AccountStreamObservation{
		Status: in.Status, ClockSource: in.ClockSource, SocketEpoch: in.SocketEpoch,
		ReadAt: in.ReadAt, RequestedAt: in.RequestedAt, DownloadEndAt: in.DownloadEndAt, CompletedAt: in.CompletedAt, LastCallbackAt: in.LastCallbackAt,
		AccountReady: in.AccountReady, AccountReadyAt: in.AccountReadyAt, TradingType: in.TradingType, TradingTypeObserved: in.TradingTypeObserved, TradingTypeAt: in.TradingTypeAt,
		Rows: make([]rpc.AccountStreamRow, 0, len(in.Rows)), RowsTruncated: in.RowsTruncated,
	}
	for _, row := range in.Rows {
		out.Rows = append(out.Rows, rpc.AccountStreamRow{Key: row.Key, Currency: row.Currency, Source: row.Source, ValueStatus: row.ValueStatus, Callbacks: row.Callbacks, FirstReceivedAt: row.FirstReceivedAt, LastReceivedAt: row.LastReceivedAt})
	}
	return out
}
