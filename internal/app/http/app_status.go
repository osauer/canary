package apphttp

import (
	nethttp "net/http"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

// App status route and schema constants define the local operator contract.
const (
	// AppStatusPath is the local-Mac-only app-host health endpoint used by
	// `canary app status`. The relay explicitly refuses to forward it.
	AppStatusPath           = "/api/app-status"
	AppStatusSchemaVersion  = "app-status-v1"
	AppStatusStateReady     = "ready"
	AppStatusStateAttention = "attention"
)

// AlertProducerStatusDTO reports the app's last daemon-authored alert
// snapshot. It is producer evidence, separate from the app-owned dispatcher
// state below, and contains no account, candidate, or transport identity.
type AlertProducerStatusDTO struct {
	Initialized  bool                    `json:"initialized"`
	AsOf         *time.Time              `json:"as_of"`
	CurrentState *rpc.AlertSnapshotState `json:"current_state"`
	Coverage     *AlertCoverageDTO       `json:"coverage"`
	Sources      []AlertSourceDTO        `json:"sources"`
}

// AppStatusDTO is the redacted local operator health contract for the app
// host. A successful response itself proves HTTP liveness; State summarizes
// whether producer coverage and dispatcher readiness are both current.
type AppStatusDTO struct {
	SchemaVersion   string                 `json:"schema_version"`
	Version         string                 `json:"version"`
	State           string                 `json:"state"`
	AlertProducer   AlertProducerStatusDTO `json:"alert_producer"`
	AlertDispatcher AlertDeliveryHealthDTO `json:"alert_dispatcher"`
}

func (h *handler) handleAppStatus(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !isLocalMac(r.RemoteAddr) {
		writeError(w, nethttp.StatusForbidden, "app status is local-Mac only")
		return
	}
	writeJSON(w, h.appStatusDTO())
}

func (h *handler) appStatusDTO() AppStatusDTO {
	alerts := h.alertDTO()
	dto := AppStatusDTO{
		SchemaVersion: AppStatusSchemaVersion,
		Version:       h.deps.Version,
		State:         AppStatusStateAttention,
		AlertProducer: AlertProducerStatusDTO{
			Initialized:  alerts.Initialized,
			AsOf:         alerts.AsOf,
			CurrentState: alerts.CurrentState,
			Coverage:     alerts.Coverage,
			Sources:      alerts.Sources,
		},
		AlertDispatcher: alerts.DeliveryHealth,
	}
	if appStatusReady(dto) {
		dto.State = AppStatusStateReady
	}
	return dto
}

// AppStatusReady reports whether both halves of alerting are currently able
// to do their jobs. An active alert episode is not a health failure; only
// producer initialization/coverage/freshness and dispatcher health matter.
func AppStatusReady(dto AppStatusDTO) bool {
	return appStatusReady(dto)
}

func appStatusReady(dto AppStatusDTO) bool {
	coverage := dto.AlertProducer.Coverage
	return dto.AlertProducer.Initialized && coverage != nil &&
		coverage.State == rpc.AlertCoverageComplete && coverage.Freshness == rpc.AlertCoverageCurrent &&
		dto.AlertDispatcher.State == state.AlertDeliveryHealthHealthy
}
