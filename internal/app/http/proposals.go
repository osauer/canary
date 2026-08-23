package apphttp

import (
	"errors"
	nethttp "net/http"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (h *handler) handleProposalsSnapshot(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.TradeProposalsSnapshot(r.Context(), rpc.TradeProposalSnapshotParams{Show: r.URL.Query().Get("show") == "1"})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsRefresh(w nethttp.ResponseWriter, r *nethttp.Request) {
	res, err := h.deps.Daemon.TradeProposalsRefresh(r.Context(), rpc.TradeProposalRefreshParams{Show: true})
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsPreview(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.TradeProposalPreviewParams
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	res, err := h.deps.Daemon.TradeProposalsPreview(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsSubmit(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		rpc.TradeProposalSubmitParams
		BrokerWriteConfirmation
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	if _, err := h.requireBrokerWriteConfirmation(r.Context(), req.BrokerWriteConfirmation); err != nil {
		writeBrokerWriteConfirmationError(w, err)
		return
	}
	// Origin is server-assigned, never client-claimed: every authenticated
	// app caller is a paired device regardless of what the body says.
	req.TradeProposalSubmitParams.Origin = rpc.OrderOriginPairedDevice
	res, err := h.deps.Daemon.TradeProposalsSubmit(r.Context(), req.TradeProposalSubmitParams)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsReducePreview(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.TradeProposalReduceParams
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	res, err := h.deps.Daemon.TradeProposalsReducePreview(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsReduceSubmit(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		rpc.TradeProposalReduceParams
		BrokerWriteConfirmation
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	if _, err := h.requireBrokerWriteConfirmation(r.Context(), req.BrokerWriteConfirmation); err != nil {
		writeBrokerWriteConfirmationError(w, err)
		return
	}
	// Origin is server-assigned, never client-claimed: every authenticated
	// app caller is a paired device regardless of what the body says.
	req.TradeProposalReduceParams.Origin = rpc.OrderOriginPairedDevice
	res, err := h.deps.Daemon.TradeProposalsReduceSubmit(r.Context(), req.TradeProposalReduceParams)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsReducePortfolioPreview(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.TradeProposalReducePortfolioParams
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	res, err := h.deps.Daemon.TradeProposalsReducePortfolioPreview(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func (h *handler) handleProposalsReducePortfolioSubmit(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		rpc.TradeProposalReducePortfolioParams
		BrokerWriteConfirmation
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	if _, err := h.requireBrokerWriteConfirmation(r.Context(), req.BrokerWriteConfirmation); err != nil {
		writeBrokerWriteConfirmationError(w, err)
		return
	}
	// Origin is server-assigned, never client-claimed: every authenticated app
	// caller is a paired device regardless of what the body says.
	req.TradeProposalReducePortfolioParams.Origin = rpc.OrderOriginPairedDevice
	res, err := h.deps.Daemon.TradeProposalsReducePortfolioSubmit(r.Context(), req.TradeProposalReducePortfolioParams)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

// handleProposalsRequestStop asks the daemon to generate a protective
// trailing-stop proposal for one named position. Generation only — no broker
// write happens here — but the verb exists to lead to one, so it carries the
// same confirm_account/confirm_mode affirmation and write-enabled requirement
// as the proposals submit routes. Preview and submit remain separately gated.
func (h *handler) handleProposalsRequestStop(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		rpc.TradeProposalRequestStopParams
		BrokerWriteConfirmation
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	if _, err := h.requireBrokerWriteConfirmation(r.Context(), req.BrokerWriteConfirmation); err != nil {
		writeBrokerWriteConfirmationError(w, err)
		return
	}
	req.TradeProposalRequestStopParams.Show = true
	res, err := h.deps.Daemon.TradeProposalsRequestStop(r.Context(), req.TradeProposalRequestStopParams)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}

func writeBrokerWriteConfirmationError(w nethttp.ResponseWriter, err error) {
	if rpcErr, ok := errors.AsType[*rpc.Error](err); ok {
		writeError(w, nethttp.StatusBadRequest, rpcErr.Message)
		return
	}
	writeError(w, nethttp.StatusBadGateway, err.Error())
}

func (h *handler) handleProposalsIgnore(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req rpc.TradeProposalIgnoreParams
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	res, err := h.deps.Daemon.TradeProposalsIgnore(r.Context(), req)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, res)
}
