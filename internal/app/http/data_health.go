package apphttp

import (
	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/rpc"
	nethttp "net/http"
	"strconv"
)

func (h *handler) handleDataHealth(w nethttp.ResponseWriter, r *nethttp.Request) {
	c, ok := h.deps.Daemon.(daemonclient.DataHealthClient)
	if !ok {
		writeError(w, nethttp.StatusServiceUnavailable, "Canary data health unavailable")
		return
	}
	var p rpc.DataHealthParams
	var err error
	if value := r.URL.Query().Get("offset"); value != "" {
		p.Offset, err = strconv.Atoi(value)
		if err != nil {
			writeError(w, 400, "invalid offset")
			return
		}
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		p.Limit, err = strconv.Atoi(value)
		if err != nil {
			writeError(w, 400, "invalid limit")
			return
		}
	}
	p.Revision = r.URL.Query().Get("revision")
	result, err := c.DataHealth(r.Context(), p)
	if err != nil {
		writeError(w, nethttp.StatusServiceUnavailable, "Canary data health unavailable; retry from the first page")
		return
	}
	writeJSON(w, result)
}
