package apphttp

import (
	"errors"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

// PushAckPath is the paired-device receipt route. It is relay-forwardable:
// the phone's service worker reaches it through the relay.
const PushAckPath = "/api/push/ack"

// pushAckRequest is a device receipt. DeviceID is optional and, when sent,
// must name the authenticated session's own device; the recorded device is
// always the session's.
type pushAckRequest struct {
	NoticeID string `json:"notice_id"`
	Event    string `json:"event"`
	At       string `json:"at,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
}

// PushAckResult reports a stored or already-known receipt.
type PushAckResult struct {
	NoticeID   string    `json:"notice_id"`
	Kind       string    `json:"kind"`
	Event      string    `json:"event"`
	Recorded   bool      `json:"recorded"`
	ReceivedAt time.Time `json:"received_at"`
}

func (h *handler) handlePushAck(w nethttp.ResponseWriter, r *nethttp.Request) {
	sess, ok := h.session(r)
	if !ok {
		writeError(w, nethttp.StatusUnauthorized, "unauthorized")
		return
	}
	var req pushAckRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "invalid push acknowledgement")
		return
	}
	noticeID := strings.TrimSpace(req.NoticeID)
	if rpc.ValidatePushNoticeID(noticeID) != nil || (req.Event != rpc.PushAckDisplayed && req.Event != rpc.PushAckOpened) {
		writeError(w, nethttp.StatusBadRequest, "invalid push acknowledgement")
		return
	}
	if claimed := strings.TrimSpace(req.DeviceID); claimed != "" && claimed != sess.DeviceID {
		writeError(w, nethttp.StatusForbidden, "acknowledgement names another device")
		return
	}
	var deviceAt time.Time
	if raw := strings.TrimSpace(req.At); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, nethttp.StatusBadRequest, "invalid push acknowledgement time")
			return
		}
		deviceAt = parsed.UTC()
	}
	outcome, err := h.deps.AlertController.RecordPushAck(noticeID, sess.DeviceID, req.Event, deviceAt)
	switch {
	case errors.Is(err, state.ErrPushNoticeUnknown):
		writeError(w, nethttp.StatusNotFound, "unknown push notice")
		return
	case errors.Is(err, state.ErrPushAckDeviceGone):
		writeError(w, nethttp.StatusUnauthorized, "unauthorized")
		return
	case errors.Is(err, state.ErrPushAckInvalid):
		writeError(w, nethttp.StatusConflict, "push acknowledgement conflicts with the journal")
		return
	case err != nil:
		writeError(w, nethttp.StatusInternalServerError, "persist push acknowledgement")
		return
	}
	writeJSON(w, PushAckResult{
		NoticeID: outcome.NoticeID, Kind: outcome.Kind, Event: outcome.Event,
		Recorded: outcome.Recorded, ReceivedAt: outcome.ReceivedAt,
	})
}
