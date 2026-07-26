package apphttp

import (
	"errors"
	nethttp "net/http"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (h *handler) handleBriefSeen(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.BriefAckParams
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	// Origin is server-assigned, never client-claimed: every authenticated
	// app caller is a paired device regardless of what the body says.
	req.Origin = rpc.OrderOriginPairedDevice
	res, err := h.deps.Daemon.BriefAck(r.Context(), req)
	if err != nil {
		writeDaemonGovernanceError(w, err)
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleReconcileSignoff(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		ReportID string `json:"report_id"`
		Origin   string `json:"origin,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.ReportID) == "" {
		writeError(w, nethttp.StatusBadRequest, "report_id required")
		return
	}
	res, err := h.deps.Daemon.ReconcileSignoff(r.Context(), rpc.CapitalEventParams{
		Type:   "reconcile",
		Report: req.ReportID,
		Origin: rpc.OrderOriginPairedDevice,
	})
	if err != nil {
		writeDaemonGovernanceError(w, err)
		return
	}
	writeJSON(w, res)
}

func writeDaemonGovernanceError(w nethttp.ResponseWriter, err error) {
	if rpcErr, ok := errors.AsType[*rpc.Error](err); ok {
		writeError(w, nethttp.StatusBadGateway, rpcErr.Message)
		return
	}
	writeError(w, nethttp.StatusBadGateway, err.Error())
}
