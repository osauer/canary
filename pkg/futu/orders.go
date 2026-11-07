package futu

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
	trdgethistoryorderfilllist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_gethistoryorderfilllist"
	trdgethistoryorderlist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_gethistoryorderlist"
	trdgetorderfilllist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getorderfilllist"
	trdgetorderlist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getorderlist"
	trdsubaccpush "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_subaccpush"
	trdupdateorder "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_updateorder"
	trdupdateorderfill "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_updateorderfill"
)

// Order is one order of the pinned account as OpenD reports it. Times are
// OpenD's own strings.
type Order struct {
	ID           uint64
	Code         string
	Name         string
	Side         string
	Type         string
	Status       string
	Quantity     float64
	Price        *float64
	FilledQty    *float64
	AveragePrice *float64
	Currency     string
	CreatedAt    string
	UpdatedAt    string
	LastError    string
}

// Fill is one execution of the pinned account.
type Fill struct {
	ID        uint64
	OrderID   uint64
	Code      string
	Name      string
	Side      string
	Quantity  float64
	Price     float64
	CreatedAt string
}

func sideName(side int32) string {
	return strings.TrimPrefix(trdcommon.TrdSide(side).String(), "TrdSide_")
}

func orderFromRow(row *trdcommon.Order) Order {
	return Order{
		ID:           row.GetOrderID(),
		Code:         tradeCode(row.GetCode(), row.GetSecMarket()),
		Name:         row.GetName(),
		Side:         sideName(row.GetTrdSide()),
		Type:         strings.TrimPrefix(trdcommon.OrderType(row.GetOrderType()).String(), "OrderType_"),
		Status:       strings.TrimPrefix(trdcommon.OrderStatus(row.GetOrderStatus()).String(), "OrderStatus_"),
		Quantity:     row.GetQty(),
		Price:        row.Price,
		FilledQty:    row.FillQty,
		AveragePrice: row.FillAvgPrice,
		Currency:     currencyName(row.GetCurrency()),
		CreatedAt:    row.GetCreateTime(),
		UpdatedAt:    row.GetUpdateTime(),
		LastError:    row.GetLastErrMsg(),
	}
}

func fillFromRow(row *trdcommon.OrderFill) Fill {
	return Fill{
		ID:        row.GetFillID(),
		OrderID:   row.GetOrderID(),
		Code:      tradeCode(row.GetCode(), row.GetSecMarket()),
		Name:      row.GetName(),
		Side:      sideName(row.GetTrdSide()),
		Quantity:  row.GetQty(),
		Price:     row.GetPrice(),
		CreatedAt: row.GetCreateTime(),
	}
}

func orderListRequest(header *trdcommon.TrdHeader, refresh bool) *trdgetorderlist.Request {
	return &trdgetorderlist.Request{C2S: &trdgetorderlist.C2S{Header: header, RefreshCache: new(refresh)}}
}

func fillListRequest(header *trdcommon.TrdHeader, refresh bool) *trdgetorderfilllist.Request {
	return &trdgetorderfilllist.Request{C2S: &trdgetorderfilllist.C2S{Header: header, RefreshCache: new(refresh)}}
}

func readOrders(ctx context.Context, conn *connection, header *trdcommon.TrdHeader, refresh bool) ([]Order, error) {
	var resp trdgetorderlist.Response
	req := orderListRequest(header, refresh)
	var err error
	if refresh {
		err = conn.request(ctx, ProtoTrdGetOrderList, req, &resp)
	} else {
		err = conn.requestUnpaced(ctx, ProtoTrdGetOrderList, req, &resp)
	}
	if err != nil {
		return nil, err
	}
	rows := resp.GetS2C().GetOrderList()
	out := make([]Order, 0, len(rows))
	for _, row := range rows {
		out = append(out, orderFromRow(row))
	}
	return out, nil
}

func readFills(ctx context.Context, conn *connection, header *trdcommon.TrdHeader, refresh bool) ([]Fill, error) {
	var resp trdgetorderfilllist.Response
	req := fillListRequest(header, refresh)
	var err error
	if refresh {
		err = conn.request(ctx, ProtoTrdGetOrderFillList, req, &resp)
	} else {
		err = conn.requestUnpaced(ctx, ProtoTrdGetOrderFillList, req, &resp)
	}
	if err != nil {
		return nil, err
	}
	rows := resp.GetS2C().GetOrderFillList()
	out := make([]Fill, 0, len(rows))
	for _, row := range rows {
		out = append(out, fillFromRow(row))
	}
	return out, nil
}

// Orders reads today's orders across every authorised market.
func (c *Connector) Orders(ctx context.Context, refresh bool) ([]Order, error) {
	conn, headers, err := c.tradeHeaders()
	if err != nil {
		return nil, err
	}
	var out []Order
	seen := make(map[uint64]bool)
	for _, header := range headers {
		rows, err := readOrders(ctx, conn, header, refresh)
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

// Fills reads today's fills across every authorised market.
func (c *Connector) Fills(ctx context.Context, refresh bool) ([]Fill, error) {
	conn, headers, err := c.tradeHeaders()
	if err != nil {
		return nil, err
	}
	var out []Fill
	seen := make(map[uint64]bool)
	for _, header := range headers {
		rows, err := readFills(ctx, conn, header, refresh)
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

// HistoryOrders reads past orders between two times, in OpenD's own
// "yyyy-MM-dd HH:mm:ss" form, across every authorised market.
func (c *Connector) HistoryOrders(ctx context.Context, from, to string) ([]Order, error) {
	conn, headers, err := c.tradeHeaders()
	if err != nil {
		return nil, err
	}
	var out []Order
	seen := make(map[uint64]bool)
	for _, header := range headers {
		var resp trdgethistoryorderlist.Response
		req := &trdgethistoryorderlist.Request{C2S: &trdgethistoryorderlist.C2S{
			Header:           header,
			FilterConditions: &trdcommon.TrdFilterConditions{BeginTime: new(from), EndTime: new(to)},
		}}
		if err := conn.request(ctx, ProtoTrdGetHistoryOrderList, req, &resp); err != nil {
			return nil, err
		}
		for _, row := range resp.GetS2C().GetOrderList() {
			if order := orderFromRow(row); !seen[order.ID] {
				seen[order.ID] = true
				out = append(out, order)
			}
		}
	}
	return out, nil
}

// HistoryFills reads past fills between two times, as [Connector.HistoryOrders]
// does orders.
func (c *Connector) HistoryFills(ctx context.Context, from, to string) ([]Fill, error) {
	conn, headers, err := c.tradeHeaders()
	if err != nil {
		return nil, err
	}
	var out []Fill
	seen := make(map[uint64]bool)
	for _, header := range headers {
		var resp trdgethistoryorderfilllist.Response
		req := &trdgethistoryorderfilllist.Request{C2S: &trdgethistoryorderfilllist.C2S{
			Header:           header,
			FilterConditions: &trdcommon.TrdFilterConditions{BeginTime: new(from), EndTime: new(to)},
		}}
		if err := conn.request(ctx, ProtoTrdGetHistoryFillList, req, &resp); err != nil {
			return nil, err
		}
		for _, row := range resp.GetS2C().GetOrderFillList() {
			if fill := fillFromRow(row); !seen[fill.ID] {
				seen[fill.ID] = true
				out = append(out, fill)
			}
		}
	}
	return out, nil
}

// subscribeAccountPush asks OpenD for the pinned account's order and fill
// pushes on this connection.
func subscribeAccountPush(ctx context.Context, conn *connection, account uint64) error {
	var resp trdsubaccpush.Response
	req := &trdsubaccpush.Request{C2S: &trdsubaccpush.C2S{AccIDList: []uint64{account}}}
	return conn.request(ctx, ProtoTrdSubAccPush, req, &resp)
}

// handleTradePush takes one order or fill update from the read loop; a push
// for any account but the pin is dropped.
func (c *Connector) handleTradePush(conn *connection, frame Frame) {
	switch frame.ProtoID {
	case ProtoTrdUpdateOrder:
		var resp trdupdateorder.Response
		if err := proto.Unmarshal(frame.Body, &resp); err != nil {
			conn.close(fmt.Errorf("%w: Trd_UpdateOrder body: %v", ErrFrame, err))
			return
		}
		if s2c := resp.GetS2C(); s2c != nil && s2c.GetHeader().GetAccID() == c.pinned.Load() && c.cfg.OnOrder != nil {
			c.cfg.OnOrder(orderFromRow(s2c.GetOrder()))
		}
	case ProtoTrdUpdateOrderFill:
		var resp trdupdateorderfill.Response
		if err := proto.Unmarshal(frame.Body, &resp); err != nil {
			conn.close(fmt.Errorf("%w: Trd_UpdateOrderFill body: %v", ErrFrame, err))
			return
		}
		if s2c := resp.GetS2C(); s2c != nil && s2c.GetHeader().GetAccID() == c.pinned.Load() && c.cfg.OnFill != nil {
			c.cfg.OnFill(fillFromRow(s2c.GetOrderFill()))
		}
	}
}
