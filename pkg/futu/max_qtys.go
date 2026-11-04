package futu

import (
	"context"
	"fmt"
	"strings"

	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
	trdgetmaxtrdqtys "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getmaxtrdqtys"
)

// MaxQuantities is what OpenD says the pinned account could trade of one
// instrument at one price. It is a capacity read in every build, not a
// what-if. Nil means OpenD did not send the field.
type MaxQuantities struct {
	CashBuy          float64
	CashAndMarginBuy *float64
	PositionSell     float64
	SellShort        *float64
	BuyBack          *float64
	LongRequiredIM   *float64
	ShortRequiredIM  *float64
}

// tradeMarkets maps a code's prefix to the trade market that authorises it.
var tradeMarkets = map[string]trdcommon.TrdMarket{
	"HK": trdcommon.TrdMarket_TrdMarket_HK,
	"US": trdcommon.TrdMarket_TrdMarket_US,
	"SH": trdcommon.TrdMarket_TrdMarket_CN,
	"SZ": trdcommon.TrdMarket_TrdMarket_CN,
	"SG": trdcommon.TrdMarket_TrdMarket_SG,
	"JP": trdcommon.TrdMarket_TrdMarket_JP,
	"AU": trdcommon.TrdMarket_TrdMarket_AU,
	"MY": trdcommon.TrdMarket_TrdMarket_MY,
	"CA": trdcommon.TrdMarket_TrdMarket_CA,
}

// headerFor picks the pin's header for the instrument's market.
func headerFor(headers []*trdcommon.TrdHeader, code string) (*trdcommon.TrdHeader, string, error) {
	prefix, symbol, ok := strings.Cut(code, ".")
	market, known := tradeMarkets[prefix]
	if !ok || !known || symbol == "" {
		return nil, "", fmt.Errorf("%w: %q", ErrInvalidCode, code)
	}
	for _, header := range headers {
		if header.GetTrdMarket() == int32(market) {
			return header, symbol, nil
		}
	}
	return nil, "", fmt.Errorf("%w: %q", ErrMarketNotAuthorised, code)
}

func maxQuantitiesRequest(header *trdcommon.TrdHeader, symbol string, orderType trdcommon.OrderType, price float64) *trdgetmaxtrdqtys.Request {
	return &trdgetmaxtrdqtys.Request{C2S: &trdgetmaxtrdqtys.C2S{
		Header:    header,
		OrderType: new(int32(orderType)),
		Code:      new(symbol),
		Price:     new(price),
	}}
}

// MaxQuantities reads the capacity for a limit order at the price; a cash
// account is refused by OpenD, which reaches the caller as an [*Error].
func (c *Connector) MaxQuantities(ctx context.Context, code string, price float64) (MaxQuantities, error) {
	conn, headers, err := c.tradeHeaders()
	if err != nil {
		return MaxQuantities{}, err
	}
	header, symbol, err := headerFor(headers, code)
	if err != nil {
		return MaxQuantities{}, err
	}
	return readMaxQuantities(ctx, conn, header, symbol, price)
}

func readMaxQuantities(ctx context.Context, conn *connection, header *trdcommon.TrdHeader, symbol string, price float64) (MaxQuantities, error) {
	var resp trdgetmaxtrdqtys.Response
	req := maxQuantitiesRequest(header, symbol, trdcommon.OrderType_OrderType_Normal, price)
	if err := conn.request(ctx, ProtoTrdGetMaxTrdQtys, req, &resp); err != nil {
		return MaxQuantities{}, err
	}
	row := resp.GetS2C().GetMaxTrdQtys()
	if row == nil {
		return MaxQuantities{}, fmt.Errorf("%w: Trd_GetMaxTrdQtys reply carries no answer", ErrFrame)
	}
	return MaxQuantities{
		CashBuy:          row.GetMaxCashBuy(),
		CashAndMarginBuy: row.MaxCashAndMarginBuy,
		PositionSell:     row.GetMaxPositionSell(),
		SellShort:        row.MaxSellShort,
		BuyBack:          row.MaxBuyBack,
		LongRequiredIM:   row.LongRequiredIM,
		ShortRequiredIM:  row.ShortRequiredIM,
	}, nil
}
