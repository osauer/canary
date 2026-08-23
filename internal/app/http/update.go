package apphttp

import (
	"errors"
	nethttp "net/http"
	"strings"
	"time"
)

// UpdateStatusSchemaVersion identifies the browser-facing app update contract.
const UpdateStatusSchemaVersion = "app-update-v1"

// Update status states keep release discovery, action, and failure explicit.
const (
	UpdateStateChecking         = "checking"
	UpdateStateCurrent          = "current"
	UpdateStateAvailable        = "available"
	UpdateStateDevelopmentBuild = "development_build"
	UpdateStateUnavailable      = "unavailable"
	UpdateStateUpdating         = "updating"
	UpdateStateFailed           = "failed"
)

// UpdateStatusDTO is app-host process state, not daemon authority. It carries
// no downloaded bytes, paths, command output, or release free text.
type UpdateStatusDTO struct {
	SchemaVersion  string    `json:"schema_version"`
	State          string    `json:"state"`
	CurrentVersion string    `json:"current_version"`
	LatestVersion  string    `json:"latest_version,omitempty"`
	TargetVersion  string    `json:"target_version,omitempty"`
	Available      bool      `json:"available"`
	Checking       bool      `json:"checking,omitempty"`
	CheckedAt      time.Time `json:"checked_at,omitzero"`
	Message        string    `json:"message,omitempty"`
}

// UpdateController owns app-local release discovery and starts the existing
// self-update command. It does not transfer updater or process authority to
// the browser: the HTTP layer exposes only these two bounded operations.
type UpdateController interface {
	Status() UpdateStatusDTO
	Start(targetVersion string) (UpdateStatusDTO, error)
}

func (h *handler) handleUpdateStatus(w nethttp.ResponseWriter, _ *nethttp.Request) {
	if h.deps.UpdateController == nil {
		writeError(w, nethttp.StatusServiceUnavailable, "update status unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, h.deps.UpdateController.Status())
}

func (h *handler) handleUpdateStart(w nethttp.ResponseWriter, r *nethttp.Request) {
	if h.deps.UpdateController == nil {
		writeError(w, nethttp.StatusServiceUnavailable, "update action unavailable")
		return
	}
	var req struct {
		TargetVersion string `json:"target_version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSONRequestError(w, err, "")
		return
	}
	req.TargetVersion = strings.TrimSpace(req.TargetVersion)
	if req.TargetVersion == "" {
		writeError(w, nethttp.StatusBadRequest, "target_version required")
		return
	}
	status, err := h.deps.UpdateController.Start(req.TargetVersion)
	if err != nil {
		if errors.Is(err, ErrUpdateConflict) {
			writeError(w, nethttp.StatusConflict, err.Error())
			return
		}
		writeError(w, nethttp.StatusInternalServerError, "could not start update")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(nethttp.StatusAccepted)
	writeJSON(w, status)
}

// ErrUpdateConflict reports a stale target or an update that cannot start from
// the controller's current state.
var ErrUpdateConflict = errors.New("update state conflict")
