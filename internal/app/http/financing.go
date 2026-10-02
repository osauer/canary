package apphttp

import (
	nethttp "net/http"
	"strconv"

	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/rpc"
)

func (h *handler) handleFinancingFees(w nethttp.ResponseWriter, r *nethttp.Request) {
	client, ok := h.deps.Daemon.(daemonclient.FinancingClient)
	if !ok {
		writeError(w, nethttp.StatusServiceUnavailable, "Lending fees unavailable")
		return
	}
	query := r.URL.Query()
	params := rpc.FinancingFeesParams{Window: query.Get("window"), From: query.Get("from"), To: query.Get("to"), Cursor: query.Get("cursor"), Fingerprint: query.Get("fingerprint")}
	for key, values := range query {
		if len(values) != 1 {
			writeError(w, nethttp.StatusBadRequest, "Ambiguous lending fee parameters")
			return
		}
		switch key {
		case "window", "from", "to", "cursor", "fingerprint", "con_id", "limit":
		default:
			writeError(w, nethttp.StatusBadRequest, "Unknown lending fee parameter")
			return
		}
	}
	if raw := query.Get("con_id"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, nethttp.StatusBadRequest, "Invalid lending contract")
			return
		}
		params.ConID = value
	}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, nethttp.StatusBadRequest, "Invalid lending fee limit")
			return
		}
		params.Limit = value
	}
	params, err := rpc.NormalizeFinancingFeesParams(params)
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	result, err := client.FinancingFees(r.Context(), params)
	if err != nil || result == nil {
		writeError(w, nethttp.StatusServiceUnavailable, "Lending fees unavailable or snapshot changed; refresh Edge and retry")
		return
	}
	if rpc.ValidateFinancingFeesResponse(*result, params) != nil {
		writeError(w, nethttp.StatusServiceUnavailable, "Lending fees unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, result)
}
