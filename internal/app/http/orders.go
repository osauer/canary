package apphttp

import (
	"fmt"
	nethttp "net/http"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

type orderModifyPreviewRequest struct {
	Action string `json:"action,omitempty"`
	// Contract is accepted for wire compatibility with older paired-app
	// drafts, but modify previews always bind to the current OrderView.
	Contract   *rpc.ContractParams `json:"contract,omitempty"`
	Quantity   int                 `json:"quantity"`
	OrderType  string              `json:"order_type,omitempty"`
	LimitPrice *float64            `json:"limit_price,omitempty"`
	Trail      *rpc.OrderTrailSpec `json:"trail,omitempty"`
	Strategy   string              `json:"strategy,omitempty"`
	TIF        string              `json:"tif,omitempty"`
	OutsideRTH *bool               `json:"outside_rth,omitempty"`
}

type orderModifyRequest struct {
	PreviewToken string `json:"preview_token"`
	BrokerWriteConfirmation
}

type orderCancelRequest struct {
	BrokerWriteConfirmation
}

func (h *handler) handleOrdersOpen(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.OrdersOpen(r.Context(), rpc.OrdersOpenParams{})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOrderStatus(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.OrderStatus(r.Context(), rpc.OrderStatusParams{ID: r.PathValue("id")})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	if !res.Found {
		writeError(w, nethttp.StatusNotFound, "order not found")
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOrderCancel(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req orderCancelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	if _, err := h.requireBrokerCancelConfirmation(r.Context(), req.BrokerWriteConfirmation); err != nil {
		writeBrokerWriteConfirmationError(w, err)
		return
	}
	res, err := h.deps.Daemon.OrderCancel(r.Context(), rpc.OrderCancelParams{ID: r.PathValue("id"), TimeoutMs: 10000, Origin: rpc.OrderOriginPairedDevice})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOrderPreviewModify(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req orderModifyPreviewRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	status, err := h.deps.Daemon.OrderStatus(r.Context(), rpc.OrderStatusParams{ID: r.PathValue("id")})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	if !status.Found {
		writeError(w, nethttp.StatusNotFound, "order not found")
		return
	}
	params, err := modifyPreviewParamsFromRequest(status.Order, req)
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	res, err := h.deps.Daemon.OrderPreview(r.Context(), params)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOrderModify(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req orderModifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	if strings.TrimSpace(req.PreviewToken) == "" {
		writeError(w, nethttp.StatusBadRequest, "preview_token required")
		return
	}
	if _, err := h.requireBrokerWriteConfirmation(r.Context(), req.BrokerWriteConfirmation); err != nil {
		writeBrokerWriteConfirmationError(w, err)
		return
	}
	res, err := h.deps.Daemon.OrderModify(r.Context(), rpc.OrderModifyParams{
		ID:           r.PathValue("id"),
		PreviewToken: strings.TrimSpace(req.PreviewToken),
		TimeoutMs:    10000,
		Origin:       rpc.OrderOriginPairedDevice,
	})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func modifyPreviewParamsFromRequest(order rpc.OrderView, req orderModifyPreviewRequest) (rpc.OrderPreviewParams, error) {
	if req.Quantity <= 0 {
		return rpc.OrderPreviewParams{}, fmt.Errorf("quantity must be positive")
	}
	contract := orderViewContractParams(order)
	if strings.TrimSpace(contract.Symbol) == "" {
		return rpc.OrderPreviewParams{}, fmt.Errorf("contract symbol required")
	}
	if strings.TrimSpace(contract.SecType) == "" {
		contract.SecType = "STK"
	}
	if strings.TrimSpace(contract.Currency) == "" && strings.TrimSpace(contract.Market) == "" &&
		strings.TrimSpace(contract.Exchange) == "" && strings.TrimSpace(contract.PrimaryExch) == "" {
		contract.Currency = "USD"
	}
	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = order.Action
	}
	if strings.TrimSpace(action) == "" {
		return rpc.OrderPreviewParams{}, fmt.Errorf("action required")
	}
	orderType := defaultString(req.OrderType, order.OrderType)
	orderType = defaultString(orderType, rpc.OrderTypeLMT)
	tif := defaultString(req.TIF, order.TIF)
	tif = defaultString(tif, rpc.OrderTIFDay)
	limit := req.LimitPrice
	trail := req.Trail
	strategy := req.Strategy
	if strings.EqualFold(orderType, rpc.OrderTypeTRAIL) || strings.EqualFold(orderType, rpc.OrderTypeTRAILLIMIT) {
		// Trail previews reject explicit limit prices; a quantity-only change
		// re-sends the order's current trail intent.
		limit = nil
		if trail == nil {
			trail = order.Trail
		}
		strategy = defaultString(strategy, rpc.OrderStrategyBrokerTrail)
	} else {
		if limit == nil && order.LimitPrice > 0 {
			v := order.LimitPrice
			limit = &v
		}
		strategy = defaultString(strategy, rpc.OrderStrategyPatientLimit)
	}
	outsideRTH := false
	if req.OutsideRTH != nil {
		outsideRTH = *req.OutsideRTH
	}
	return rpc.OrderPreviewParams{
		Action:     action,
		Contract:   contract,
		Quantity:   req.Quantity,
		OrderType:  orderType,
		LimitPrice: limit,
		Trail:      trail,
		Strategy:   strategy,
		TIF:        tif,
		OutsideRTH: outsideRTH,
		ReplaceID:  order.OrderRef,
		TimeoutMs:  5000,
	}, nil
}

func orderViewContractParams(order rpc.OrderView) rpc.ContractParams {
	return rpc.ContractParams{
		ConID:        order.ConID,
		Symbol:       strings.TrimSpace(order.Symbol),
		SecType:      strings.TrimSpace(order.SecType),
		Exchange:     strings.TrimSpace(order.Exchange),
		PrimaryExch:  strings.TrimSpace(order.PrimaryExch),
		Currency:     strings.TrimSpace(order.Currency),
		LocalSymbol:  strings.TrimSpace(order.LocalSymbol),
		TradingClass: strings.TrimSpace(order.TradingClass),
		Expiry:       strings.TrimSpace(order.Expiry),
		Strike:       order.Strike,
		Right:        strings.TrimSpace(order.Right),
		Multiplier:   order.Multiplier,
	}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
