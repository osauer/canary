package apphttp

import (
	nethttp "net/http"
	"strconv"

	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/rpc"
)

func (h *handler) handleMarketTape(w nethttp.ResponseWriter, r *nethttp.Request) {
	client, ok := h.deps.Daemon.(daemonclient.MarketTapeClient)
	if !ok {
		writeError(w, nethttp.StatusServiceUnavailable, "Market tape unavailable")
		return
	}
	var p rpc.MarketTapeParams
	if value := r.URL.Query().Get("sessions"); value != "" {
		var err error
		p.Sessions, err = strconv.Atoi(value)
		if err != nil {
			writeError(w, nethttp.StatusBadRequest, "Invalid tape session count")
			return
		}
	}
	p, err := rpc.NormalizeMarketTapeParams(p)
	if err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	result, err := client.MarketTape(r.Context(), p)
	if err != nil || result == nil {
		writeError(w, nethttp.StatusServiceUnavailable, "Market tape unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, result)
}
