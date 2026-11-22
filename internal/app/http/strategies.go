package apphttp

import (
	nethttp "net/http"
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
)

type strategySubmitRequest struct {
	PreviewToken string `json:"preview_token"`
	BrokerWriteConfirmation
}

func (h *handler) handleStrategyPreview(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.StrategyPreviewParams
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	// The paired app can identify a daemon-authored strategy, operation, and
	// price. It cannot claim another source or author combo legs.
	req.Source = "strategy_app"
	// The app closes and reduces. Adding units opens risk and belongs to the
	// gated CLI alone, so no other operation is passed on to the daemon.
	switch req.Operation = strings.ToLower(strings.TrimSpace(req.Operation)); req.Operation {
	case rpc.StrategyOperationClose, rpc.StrategyOperationReduce:
	default:
		writeError(w, nethttp.StatusBadRequest, "the app closes or reduces a strategy; operation must be close or reduce")
		return
	}
	res, err := h.deps.Daemon.StrategyPreview(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleStrategySubmit(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req strategySubmitRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	req.PreviewToken = strings.TrimSpace(req.PreviewToken)
	if req.PreviewToken == "" {
		writeError(w, nethttp.StatusBadRequest, "preview_token required")
		return
	}
	if _, err := h.requireBrokerWriteConfirmation(r.Context(), req.BrokerWriteConfirmation); err != nil {
		writeBrokerWriteConfirmationError(w, err)
		return
	}
	res, err := h.deps.Daemon.OrderPlace(r.Context(), rpc.OrderPlaceParams{
		PreviewToken: req.PreviewToken,
		TimeoutMs:    10000,
		Origin:       rpc.OrderOriginPairedDevice,
	})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}
