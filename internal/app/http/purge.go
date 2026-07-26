package apphttp

import (
	"errors"
	nethttp "net/http"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

type purgeActionRequest struct {
	PurgeID string   `json:"purge_id,omitempty"`
	All     bool     `json:"all,omitempty"`
	Symbols []string `json:"symbols,omitempty"`
	Scale   float64  `json:"scale,omitempty"`
	BrokerWriteConfirmation
}

func (h *handler) handlePurgeStatus(w nethttp.ResponseWriter, r *nethttp.Request) {
	limit := 50
	res, err := h.deps.Daemon.PurgeStatus(r.Context(), rpc.PurgeStatusParams{
		PurgeID: strings.TrimSpace(r.URL.Query().Get("purge_id")),
		Account: strings.TrimSpace(r.URL.Query().Get("account")),
		Limit:   limit,
	})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handlePurgeRestorePreview(w nethttp.ResponseWriter, r *nethttp.Request) {
	req, ok := h.decodePurgeActionRequest(w, r)
	if !ok {
		return
	}
	params, err := purgeRestoreParams(req)
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	res, err := h.deps.Daemon.PurgeRestorePreview(r.Context(), params)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handlePurgeExecute(w nethttp.ResponseWriter, r *nethttp.Request) {
	req, ok := h.decodePurgeActionRequest(w, r)
	if !ok {
		return
	}
	if err := h.requirePurgeWriteConfirmation(r, req); err != nil {
		writePurgeConfirmationError(w, err)
		return
	}
	symbols := purgeRequestSymbols(req.Symbols)
	if !req.All && len(symbols) == 0 {
		writeError(w, nethttp.StatusBadRequest, "purge target requires all=true or at least one symbol")
		return
	}
	bypassPreview := true
	res, err := h.deps.Daemon.PurgeExecute(r.Context(), rpc.PurgeExecuteParams{
		PurgeID:       strings.TrimSpace(req.PurgeID),
		All:           req.All,
		Symbols:       symbols,
		BypassPreview: &bypassPreview,
		WaitMs:        2000,
		Origin:        rpc.OrderOriginPairedDevice,
	})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handlePurgeRestoreExecute(w nethttp.ResponseWriter, r *nethttp.Request) {
	req, ok := h.decodePurgeActionRequest(w, r)
	if !ok {
		return
	}
	if err := h.requirePurgeWriteConfirmation(r, req); err != nil {
		writePurgeConfirmationError(w, err)
		return
	}
	params, err := purgeRestoreParams(req)
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	params.WaitMs = 2000
	res, err := h.deps.Daemon.PurgeRestoreExecute(r.Context(), params)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) decodePurgeActionRequest(w nethttp.ResponseWriter, r *nethttp.Request) (purgeActionRequest, bool) {
	var req purgeActionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return purgeActionRequest{}, false
	}
	return req, true
}

func (h *handler) requirePurgeWriteConfirmation(r *nethttp.Request, req purgeActionRequest) error {
	_, err := h.requireBrokerWriteConfirmation(r.Context(), req.BrokerWriteConfirmation)
	return err
}

func writePurgeConfirmationError(w nethttp.ResponseWriter, err error) {
	if rpcErr, ok := errors.AsType[*rpc.Error](err); ok {
		writeError(w, nethttp.StatusBadRequest, rpcErr.Message)
		return
	}
	writeError(w, nethttp.StatusBadGateway, err.Error())
}

func purgeRestoreParams(req purgeActionRequest) (rpc.PurgeRestoreParams, error) {
	symbols := purgeRequestSymbols(req.Symbols)
	if !req.All && len(symbols) == 0 {
		return rpc.PurgeRestoreParams{}, &rpc.Error{Code: rpc.CodeBadRequest, Message: "restore target requires all=true or at least one symbol"}
	}
	scale := req.Scale
	if scale == 0 {
		scale = 1
	}
	return rpc.PurgeRestoreParams{
		PurgeID: strings.TrimSpace(req.PurgeID),
		All:     req.All,
		Symbols: symbols,
		Scale:   scale,
		Origin:  rpc.OrderOriginPairedDevice,
	}, nil
}

func purgeRequestSymbols(symbols []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" || seen[symbol] {
			continue
		}
		seen[symbol] = true
		out = append(out, symbol)
	}
	return out
}
