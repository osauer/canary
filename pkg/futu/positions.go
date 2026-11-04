package futu

import (
	"context"
	"strings"

	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
	trdgetpositionlist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getpositionlist"
)

// Position is one held line of the pinned account. A short line's quantity is
// negative.
type Position struct {
	ID               uint64
	Code             string
	Name             string
	Quantity         float64
	SellableQuantity float64
	Price            float64
	Value            float64
	AverageCost      *float64
	DilutedCost      *float64
	ProfitLoss       float64
	UnrealizedPnL    *float64
	RealizedPnL      *float64
	Currency         string
}

// secMarketPrefixes names a trade row's security market the way a code does.
var secMarketPrefixes = map[int32]string{
	int32(trdcommon.TrdSecMarket_TrdSecMarket_HK):    "HK",
	int32(trdcommon.TrdSecMarket_TrdSecMarket_US):    "US",
	int32(trdcommon.TrdSecMarket_TrdSecMarket_CN_SH): "SH",
	int32(trdcommon.TrdSecMarket_TrdSecMarket_CN_SZ): "SZ",
	int32(trdcommon.TrdSecMarket_TrdSecMarket_SG):    "SG",
	int32(trdcommon.TrdSecMarket_TrdSecMarket_JP):    "JP",
	int32(trdcommon.TrdSecMarket_TrdSecMarket_AU):    "AU",
	int32(trdcommon.TrdSecMarket_TrdSecMarket_MY):    "MY",
	int32(trdcommon.TrdSecMarket_TrdSecMarket_CA):    "CA",
}

// tradeCode joins a trade row's bare code and security market into the code a
// quote read takes. A row that already carries its prefix keeps it.
func tradeCode(code string, secMarket int32) string {
	if strings.Contains(code, ".") {
		return code
	}
	if prefix, ok := secMarketPrefixes[secMarket]; ok {
		return prefix + "." + code
	}
	return code
}

func positionsRequest(header *trdcommon.TrdHeader, refresh bool) *trdgetpositionlist.Request {
	return &trdgetpositionlist.Request{C2S: &trdgetpositionlist.C2S{Header: header, RefreshCache: new(refresh)}}
}

// Positions reads the held book across every market the pin is authorised
// for, one request per market. Refresh works as it does on [Connector.Funds].
func (c *Connector) Positions(ctx context.Context, refresh bool) ([]Position, error) {
	conn, headers, err := c.tradeHeaders()
	if err != nil {
		return nil, err
	}
	var out []Position
	seen := make(map[uint64]bool)
	for _, header := range headers {
		rows, err := readPositions(ctx, conn, header, refresh)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if !seen[row.ID] {
				seen[row.ID] = true
				out = append(out, row)
			}
		}
	}
	return out, nil
}

func readPositions(ctx context.Context, conn *connection, header *trdcommon.TrdHeader, refresh bool) ([]Position, error) {
	var resp trdgetpositionlist.Response
	req := positionsRequest(header, refresh)
	var err error
	if refresh {
		err = conn.request(ctx, ProtoTrdGetPositionList, req, &resp)
	} else {
		err = conn.requestUnpaced(ctx, ProtoTrdGetPositionList, req, &resp)
	}
	if err != nil {
		return nil, err
	}
	rows := resp.GetS2C().GetPositionList()
	out := make([]Position, 0, len(rows))
	for _, row := range rows {
		quantity := row.GetQty()
		if trdcommon.PositionSide(row.GetPositionSide()) == trdcommon.PositionSide_PositionSide_Short {
			quantity = -quantity
		}
		out = append(out, Position{
			ID:               row.GetPositionID(),
			Code:             tradeCode(row.GetCode(), row.GetSecMarket()),
			Name:             row.GetName(),
			Quantity:         quantity,
			SellableQuantity: row.GetCanSellQty(),
			Price:            row.GetPrice(),
			Value:            row.GetVal(),
			AverageCost:      row.AverageCostPrice,
			DilutedCost:      row.DilutedCostPrice,
			ProfitLoss:       row.GetPlVal(),
			UnrealizedPnL:    row.UnrealizedPL,
			RealizedPnL:      row.RealizedPL,
			Currency:         currencyName(row.GetCurrency()),
		})
	}
	return out, nil
}
