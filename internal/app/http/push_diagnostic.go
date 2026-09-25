package apphttp

import (
	nethttp "net/http"
)

// PushDiagnosticPath is local-Mac control: the relay refuses to forward it
// (see relay.forwardableAppPath) because relay-forwarded requests arrive from
// loopback and would pass the local gate.
const PushDiagnosticPath = "/api/push/diagnostic"

// handleLocalPushDiagnostic backs `canary app push-test`: one diagnostic push
// to every active subscription, journaled and acknowledged like an alert but
// never counted as one.
func (h *handler) handleLocalPushDiagnostic(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !isLocalMac(r.RemoteAddr) {
		writeError(w, nethttp.StatusForbidden, "diagnostic push is local-Mac only")
		return
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeRequiredEmptyJSONObject(w, r); err != nil {
			writeJSONRequestError(w, err, "diagnostic push body must be an empty JSON object")
			return
		}
	}
	result, err := h.deps.AlertController.SendDiagnostic(r.Context())
	if err != nil {
		writeError(w, nethttp.StatusInternalServerError, "diagnostic dispatch failed")
		return
	}
	writeJSON(w, result)
}
