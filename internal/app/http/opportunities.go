package apphttp

import (
	nethttp "net/http"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (h *handler) handleOpportunitiesSnapshot(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.OpportunitiesSnapshot(r.Context(), rpc.OpportunitySnapshotParams{Show: r.URL.Query().Get("show") == "1"})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOpportunitiesRefresh(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.OpportunitiesRefresh(r.Context(), rpc.OpportunityRefreshParams{Show: true})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOpportunitiesPreviewExercise(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.OpportunityExercisePreviewParams
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	req.Origin = rpc.OrderOriginPairedDevice
	res, err := h.deps.Daemon.OpportunitiesPreviewExercise(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOpportunitiesSubmitExercise(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		rpc.OpportunityExerciseSubmitParams
		BrokerWriteConfirmation
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	if _, err := h.requireBrokerSessionConfirmation(r.Context(), req.BrokerWriteConfirmation); err != nil {
		writeBrokerWriteConfirmationError(w, err)
		return
	}
	req.OpportunityExerciseSubmitParams.Origin = rpc.OrderOriginPairedDevice
	res, err := h.deps.Daemon.OpportunitiesSubmitExercise(r.Context(), req.OpportunityExerciseSubmitParams)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleOpportunitiesIgnore(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.OpportunityIgnoreParams
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	res, err := h.deps.Daemon.OpportunitiesIgnore(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}
