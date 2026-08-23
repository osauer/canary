package apphttp

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	nethttp "net/http"
	"slices"
	"time"

	appalerts "github.com/osauer/canary/v2/internal/app/alerts"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	// AlertSchemaVersion identifies the browser-facing active alert contract.
	// The separate version field identifies the durable ledger projection.
	AlertSchemaVersion       = "alerts-v1"
	alertPollInterval        = 250 * time.Millisecond
	alertHealthUninitialized = "not_initialized"
)

// AlertDTO is the sole public projection of the app-owned alert ledger. Keep
// this mapping explicit: adding an internal state field must never publish it
// accidentally.
type AlertDTO struct {
	SchemaVersion  string                  `json:"schema_version"`
	Version        string                  `json:"version"`
	Initialized    bool                    `json:"initialized"`
	Generation     uint64                  `json:"generation"`
	AsOf           *time.Time              `json:"as_of"`
	CurrentState   *rpc.AlertSnapshotState `json:"current_state"`
	Coverage       *AlertCoverageDTO       `json:"coverage"`
	Sources        []AlertSourceDTO        `json:"sources"`
	Occurrences    []AlertOccurrenceDTO    `json:"occurrences"`
	Attention      AlertAttentionDTO       `json:"attention"`
	DeliveryHealth AlertDeliveryHealthDTO  `json:"delivery_health"`
}

// AlertCoverageDTO exposes only aggregate source coverage and freshness.
type AlertCoverageDTO struct {
	State           rpc.AlertCoverageState     `json:"state"`
	Freshness       rpc.AlertCoverageFreshness `json:"freshness"`
	AsOf            time.Time                  `json:"as_of"`
	ExpectedSources []rpc.AlertSource          `json:"expected_sources"`
	CoveredSources  []rpc.AlertSource          `json:"covered_sources"`
}

// AlertSourceDTO preserves the exact typed source reason, health, and timing
// evidence. Nil times mean that source has not yet been observed.
type AlertSourceDTO struct {
	Source         rpc.AlertSource         `json:"source"`
	Status         string                  `json:"status"`
	Reason         string                  `json:"reason"`
	EvidenceHealth rpc.AlertEvidenceHealth `json:"evidence_health"`
	InputAsOf      *time.Time              `json:"input_as_of"`
	ObservedAt     *time.Time              `json:"observed_at"`
	EvidenceAsOf   *time.Time              `json:"evidence_as_of"`
	FreshUntil     *time.Time              `json:"fresh_until"`
	Covered        bool                    `json:"covered"`
}

// AlertOccurrenceDTO exposes one redacted occurrence. DisplayID is the only
// public identity; producer keys, fingerprints, and delivery attempt identity
// remain private.
type AlertOccurrenceDTO struct {
	DisplayID        string                    `json:"display_id"`
	Source           rpc.AlertSource           `json:"source"`
	Kind             rpc.AlertKind             `json:"kind"`
	PresentationCode rpc.AlertPresentationCode `json:"presentation_code"`
	Title            string                    `json:"title"`
	Body             string                    `json:"body"`
	State            rpc.AlertEpisodeState     `json:"state"`
	Severity         rpc.AlertSeverity         `json:"severity"`
	EvidenceHealth   rpc.AlertEvidenceHealth   `json:"evidence_health"`
	Destination      rpc.AlertDestination      `json:"destination"`
	EvidenceAsOf     time.Time                 `json:"evidence_as_of"`
	StateChangedAt   time.Time                 `json:"state_changed_at"`
	FirstSeenAt      time.Time                 `json:"first_seen_at"`
	LastSeenAt       time.Time                 `json:"last_seen_at"`
	EndedAt          *time.Time                `json:"ended_at"`
	EndReason        *string                   `json:"end_reason"`
	AttentionSeq     uint64                    `json:"attention_seq"`
	Disposition      string                    `json:"disposition"`
}

// AlertAttentionRefDTO identifies one redacted unread occurrence.
type AlertAttentionRefDTO struct {
	DisplayID string          `json:"display_id"`
	Source    rpc.AlertSource `json:"source"`
	Kind      rpc.AlertKind   `json:"kind"`
}

// AlertAttentionDTO is a durable render cursor, not proof of human attention.
type AlertAttentionDTO struct {
	UnreadCount    int                    `json:"unread_count"`
	HighWaterSeq   uint64                 `json:"high_water_seq"`
	ReadThroughSeq uint64                 `json:"read_through_seq"`
	UnreadRefs     []AlertAttentionRefDTO `json:"unread_refs"`
}

// AlertDeliveryHealthDTO exposes only classified app delivery health. It
// deliberately omits targets, attempts, receipts, and raw transport errors.
type AlertDeliveryHealthDTO struct {
	State                       string     `json:"state"`
	Class                       string     `json:"class"`
	UpdatedAt                   *time.Time `json:"updated_at"`
	LastPushServiceAcceptanceAt *time.Time `json:"last_push_service_acceptance_at"`
}

func (h *handler) alertDTO() AlertDTO {
	now := time.Now().UTC()
	return newAlertDTO(h.deps.Store.AlertDelivery(now), now)
}

// alertDeliveryHealthDTO maps the ledger's delivery health for HTTP, with the
// uninitialized-store fallback shared by the alert and governance surfaces.
func alertDeliveryHealthDTO(view state.AlertDeliveryView) AlertDeliveryHealthDTO {
	health := AlertDeliveryHealthDTO{
		State:     view.DeliveryHealth.State,
		Class:     view.DeliveryHealth.Class,
		UpdatedAt: alertTime(view.DeliveryHealth.UpdatedAt),
		// Push-service acceptance is transport evidence only. It is not proof
		// that a device displayed the alert or that a person read it.
		LastPushServiceAcceptanceAt: alertTime(view.DeliveryHealth.LastAcceptedAt),
	}
	if !view.Initialized && health.State == "" {
		health.State = state.AlertDeliveryHealthUnavailable
		health.Class = alertHealthUninitialized
	}
	return health
}

func newAlertDTO(view state.AlertDeliveryView, now time.Time) AlertDTO {
	quarantined := view.DeliveryHealth.State == state.AlertDeliveryHealthUnavailable &&
		view.DeliveryHealth.Class == state.AlertDeliveryHealthClassInvalidPersistedState
	health := alertDeliveryHealthDTO(view)
	dto := AlertDTO{
		SchemaVersion:  AlertSchemaVersion,
		Version:        view.Version,
		Initialized:    view.Initialized && !quarantined,
		Generation:     view.Generation,
		Sources:        []AlertSourceDTO{},
		Occurrences:    []AlertOccurrenceDTO{},
		Attention:      newAlertAttentionDTO(view.Attention, nil),
		DeliveryHealth: health,
	}
	if !dto.Initialized {
		return dto
	}

	dto.AsOf = alertTime(view.AsOf)
	currentState := view.CurrentState
	if currentState == rpc.AlertSnapshotClear && !alertViewCanBeClear(view, now) {
		currentState = rpc.AlertSnapshotUnknown
	}
	dto.CurrentState = &currentState
	dto.Coverage = &AlertCoverageDTO{
		State:           view.Coverage.State,
		Freshness:       view.Coverage.Freshness,
		AsOf:            view.Coverage.AsOf.UTC(),
		ExpectedSources: append([]rpc.AlertSource{}, view.Coverage.ExpectedSources...),
		CoveredSources:  append([]rpc.AlertSource{}, view.Coverage.CoveredSources...),
	}
	for _, source := range view.Sources {
		dto.Sources = append(dto.Sources, AlertSourceDTO{
			Source:         source.Source,
			Status:         source.Status,
			Reason:         source.Reason,
			EvidenceHealth: source.EvidenceHealth,
			InputAsOf:      alertTime(source.InputAsOf),
			ObservedAt:     alertTime(source.ObservedAt),
			EvidenceAsOf:   alertTime(source.EvidenceAsOf),
			FreshUntil:     alertTime(source.FreshUntil),
			Covered:        source.Covered,
		})
	}
	dto.Occurrences = newAlertOccurrenceDTOs(view.Occurrences)
	dto.Attention = newAlertAttentionDTO(view.Attention, dto.Occurrences)
	return dto
}

func alertViewCanBeClear(view state.AlertDeliveryView, now time.Time) bool {
	if view.Coverage.State != rpc.AlertCoverageComplete || view.Coverage.Freshness != rpc.AlertCoverageCurrent ||
		len(view.Coverage.ExpectedSources) == 0 || len(view.Coverage.ExpectedSources) != len(view.Coverage.CoveredSources) ||
		len(view.Sources) != len(view.Coverage.ExpectedSources) {
		return false
	}
	expected := make(map[rpc.AlertSource]struct{}, len(view.Coverage.ExpectedSources))
	for _, source := range view.Coverage.ExpectedSources {
		expected[source] = struct{}{}
	}
	covered := make(map[rpc.AlertSource]struct{}, len(view.Coverage.CoveredSources))
	for _, source := range view.Coverage.CoveredSources {
		covered[source] = struct{}{}
	}
	if len(expected) != len(view.Coverage.ExpectedSources) || len(covered) != len(expected) {
		return false
	}
	for source := range expected {
		if _, ok := covered[source]; !ok {
			return false
		}
	}
	seen := make(map[rpc.AlertSource]struct{}, len(view.Sources))
	for _, source := range view.Sources {
		if _, ok := expected[source.Source]; !ok || !source.Covered || source.EvidenceHealth != rpc.AlertEvidenceCurrent ||
			source.FreshUntil.IsZero() || now.After(source.FreshUntil) {
			return false
		}
		if _, duplicate := seen[source.Source]; duplicate {
			return false
		}
		seen[source.Source] = struct{}{}
	}
	return len(seen) == len(expected)
}

func newAlertOccurrenceDTOs(occurrences []state.AlertDeliveryOccurrenceView) []AlertOccurrenceDTO {
	active := make([]state.AlertDeliveryOccurrenceView, 0, len(occurrences))
	for _, occurrence := range occurrences {
		// A durable episode may be held open while its producer evidence is
		// incomplete. That retained safety state is not a current user alert.
		if occurrence.EndedAt.IsZero() && occurrence.EvidenceHealth == rpc.AlertEvidenceCurrent {
			active = append(active, occurrence)
		}
	}
	slices.SortFunc(active, func(a, b state.AlertDeliveryOccurrenceView) int {
		if byTime := b.StateChangedAt.Compare(a.StateChangedAt); byTime != 0 {
			return byTime
		}
		return cmp.Compare(a.DisplayID, b.DisplayID)
	})
	// The durable ledger keeps terminal rows for delivery deduplication and
	// audit, but v3 has no user-facing alert history. Only current conditions
	// cross the public HTTP boundary.
	out := make([]AlertOccurrenceDTO, 0, len(active))
	for _, occurrence := range active {
		presentation, ok := appalerts.PresentationFor(occurrence.PresentationCode, occurrence.State)
		if !ok {
			presentation = appalerts.Presentation{
				Title: "Alert unavailable",
				Body:  "This alert cannot be displayed safely.",
			}
		}
		item := AlertOccurrenceDTO{
			DisplayID:        occurrence.DisplayID,
			Source:           occurrence.Source,
			Kind:             occurrence.Kind,
			PresentationCode: occurrence.PresentationCode,
			Title:            presentation.Title,
			Body:             presentation.Body,
			State:            occurrence.State,
			Severity:         occurrence.Severity,
			EvidenceHealth:   occurrence.EvidenceHealth,
			Destination:      occurrence.Destination,
			EvidenceAsOf:     occurrence.EvidenceAsOf.UTC(),
			StateChangedAt:   occurrence.StateChangedAt.UTC(),
			FirstSeenAt:      occurrence.FirstSeenAt.UTC(),
			LastSeenAt:       occurrence.LastSeenAt.UTC(),
			EndedAt:          alertTime(occurrence.EndedAt),
			AttentionSeq:     occurrence.AttentionSeq,
			Disposition:      occurrence.Disposition,
		}
		if occurrence.EndReason != "" {
			reason := occurrence.EndReason
			item.EndReason = &reason
		}
		out = append(out, item)
	}
	return out
}

func newAlertAttentionDTO(attention state.AlertDeliveryAttention, occurrences []AlertOccurrenceDTO) AlertAttentionDTO {
	active := make(map[string]AlertOccurrenceDTO, len(occurrences))
	for _, occurrence := range occurrences {
		active[occurrence.DisplayID] = occurrence
	}
	dto := AlertAttentionDTO{
		HighWaterSeq:   attention.HighWaterSeq,
		ReadThroughSeq: attention.ReadThroughSeq,
		UnreadRefs:     make([]AlertAttentionRefDTO, 0, len(attention.UnreadRefs)),
	}
	for _, ref := range attention.UnreadRefs {
		occurrence, ok := active[ref.DisplayID]
		if !ok || occurrence.Source != ref.Source || occurrence.Kind != ref.Kind {
			continue
		}
		dto.UnreadRefs = append(dto.UnreadRefs, AlertAttentionRefDTO{
			DisplayID: ref.DisplayID,
			Source:    ref.Source,
			Kind:      ref.Kind,
		})
	}
	dto.UnreadCount = len(dto.UnreadRefs)
	return dto
}

func alertTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func (h *handler) handleAlerts(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.alertDTO())
}

func (h *handler) handleAlertAttention(w nethttp.ResponseWriter, _ *nethttp.Request) {
	dto := h.alertDTO()
	if !dto.Initialized {
		writeError(w, nethttp.StatusServiceUnavailable, "alerts state is unavailable")
		return
	}
	writeJSON(w, dto.Attention)
}

func (h *handler) handleAlertAttentionRead(w nethttp.ResponseWriter, r *nethttp.Request) {
	throughSeq, err := decodeAttentionReadRequest(w, r)
	if err != nil {
		writeJSONRequestError(w, err, "invalid alerts attention read request")
		return
	}
	current := h.alertDTO()
	if !current.Initialized {
		writeError(w, nethttp.StatusConflict, "alerts state is unavailable")
		return
	}
	if _, err := h.deps.Store.MarkAlertDeliveryAttentionRead(throughSeq); err != nil {
		if errors.Is(err, state.ErrAlertDeliveryAttentionRead) {
			writeError(w, nethttp.StatusConflict, "attention cursor conflicts with current alerts state")
			return
		}
		if errors.Is(err, state.ErrAlertDeliveryUnavailable) {
			writeError(w, nethttp.StatusConflict, "alerts state is unavailable")
			return
		}
		writeError(w, nethttp.StatusInternalServerError, "persist alerts attention read cursor")
		return
	}
	writeJSON(w, h.alertDTO())
}

type alertStreamCursor [sha256.Size]byte

func newAlertStreamCursor(dto AlertDTO) alertStreamCursor {
	// Freshness can age at FreshUntil without a ledger write or generation
	// change. Hash the complete redacted DTO so SSE emits that public change.
	raw, err := json.Marshal(dto)
	if err != nil {
		return alertStreamCursor{}
	}
	return sha256.Sum256(raw)
}
