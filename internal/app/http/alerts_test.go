package apphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/live"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestAlertRoutesRequireAuthAndBootstrapMatchesGET(t *testing.T) {
	t.Parallel()
	srv, store, _ := newGovernanceTestHandlerWithoutPoll(t, routeFakeClient{})
	handler := srv.Handler()

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/alerts", nil),
		httptest.NewRequest(http.MethodGet, "/api/alerts/attention", nil),
		httptest.NewRequest(http.MethodPost, "/api/alerts/attention/read", strings.NewReader(`{"through_seq":0}`)),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s status=%d", request.Method, request.URL.Path, response.Code)
		}
	}

	at := time.Now().UTC().Add(-time.Minute)
	privateEpisode, privateOccurrence := alertHTTPObserve(t, store, at)
	privateScope := alertHTTPAuthorityScope(t)
	cookie := routeSessionCookie(t, handler)

	getRequest := httptest.NewRequest(http.MethodGet, "/api/alerts", nil)
	getRequest.AddCookie(cookie)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	var getDTO AlertDTO
	if err := json.Unmarshal(getResponse.Body.Bytes(), &getDTO); err != nil {
		t.Fatal(err)
	}
	if getDTO.SchemaVersion != AlertSchemaVersion || getDTO.Version != state.AlertDeliveryVersion || !getDTO.Initialized || getDTO.Generation != 2 {
		t.Fatalf("GET envelope=%+v", getDTO)
	}
	if getDTO.Coverage == nil || len(getDTO.Sources) != 1 || getDTO.Sources[0].Reason != "source_current" || getDTO.Sources[0].FreshUntil == nil {
		t.Fatalf("GET source evidence=%+v", getDTO)
	}
	if len(getDTO.Occurrences) != 1 || getDTO.Occurrences[0].DisplayID == "" ||
		getDTO.Occurrences[0].PresentationCode != rpc.AlertPresentationPortfolioStress ||
		getDTO.Occurrences[0].Title != "Portfolio stress" || getDTO.Occurrences[0].Body != "Canary reports portfolio stress." ||
		getDTO.Attention.UnreadCount != 1 || len(getDTO.Attention.UnreadRefs) != 1 {
		t.Fatalf("GET occurrences/attention=%+v", getDTO)
	}

	bootstrapRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootstrapRequest.AddCookie(cookie)
	bootstrapResponse := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapResponse, bootstrapRequest)
	if bootstrapResponse.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", bootstrapResponse.Code, bootstrapResponse.Body.String())
	}
	var bootstrap map[string]json.RawMessage
	if err := json.Unmarshal(bootstrapResponse.Body.Bytes(), &bootstrap); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"attention", "alert_inbox_v2", "last_push"} {
		if _, exists := bootstrap[removed]; exists {
			t.Fatalf("bootstrap retained removed alert authority %q: %s", removed, bootstrapResponse.Body.String())
		}
	}
	var getValue, bootstrapValue any
	if err := json.Unmarshal(getResponse.Body.Bytes(), &getValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bootstrap["alerts"], &bootstrapValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(getValue, bootstrapValue) {
		t.Fatalf("bootstrap projection differs from GET\nGET=%s\nbootstrap=%s", getResponse.Body.Bytes(), bootstrap["alerts"])
	}

	raw := getResponse.Body.String()
	for _, forbidden := range []string{
		`"source_watermarks"`, `"attempt_totals"`,
		`"episode_key"`, `"occurrence_key"`, `"evidence_fingerprint"`, `"attempt_id"`, `"receipt_key"`,
		`"target_ref"`, `"device_id"`, `"subscription_id"`, `"authority_scope"`, `"raw_error"`,
		privateScope, privateEpisode, privateOccurrence,
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("public alerts leaked %q: %s", forbidden, raw)
		}
	}
	assertAlertExactJSONKeys(t, getResponse.Body.Bytes())
	if getDTO.DeliveryHealth.LastPushServiceAcceptanceAt != nil {
		t.Fatalf("never-accepted alert ledger invented push acceptance: %+v", getDTO.DeliveryHealth)
	}
	if getDTO.DeliveryHealth.State != state.AlertDeliveryHealthUnavailable || getDTO.DeliveryHealth.Class != state.AlertDeliveryHealthClassNoSubscription {
		t.Fatalf("active mode without a subscription rendered healthy: %+v", getDTO.DeliveryHealth)
	}
}

func TestRemovedAlertRoutesAndMethodsStayRemoved(t *testing.T) {
	t.Parallel()
	srv, _, _ := newGovernanceTestHandlerWithoutPoll(t, routeFakeClient{})
	handler := srv.Handler()
	cookie := routeSessionCookie(t, handler)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/attention"},
		{http.MethodPost, "/api/attention/read"},
		{http.MethodGet, "/api/alert-inbox-v2"},
		{http.MethodGet, "/api/alert-inbox-v2/attention"},
		{http.MethodPost, "/api/alert-inbox-v2/attention/read"},
		{http.MethodDelete, "/api/alerts"},
	}
	for _, tc := range tests {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Errorf("removed %s %s status=%d body=%s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
}

func TestAlertAttentionFailsLoudBeforeInitialization(t *testing.T) {
	t.Parallel()
	srv, _, _ := newGovernanceTestHandlerWithoutPoll(t, routeFakeClient{})
	handler := srv.Handler()
	cookie := routeSessionCookie(t, handler)

	get := httptest.NewRequest(http.MethodGet, "/api/alerts/attention", nil)
	get.AddCookie(cookie)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("uninitialized attention GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}

	read := httptest.NewRequest(http.MethodPost, "/api/alerts/attention/read", strings.NewReader(`{"through_seq":0}`))
	read.AddCookie(cookie)
	readResponse := httptest.NewRecorder()
	handler.ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusConflict {
		t.Fatalf("uninitialized attention read status=%d body=%s", readResponse.Code, readResponse.Body.String())
	}
}

func TestAlertAttentionReadReturnsFullGenerationAndRejectsConflicts(t *testing.T) {
	t.Parallel()
	srv, store, _ := newGovernanceTestHandlerWithoutPoll(t, routeFakeClient{})
	handler := srv.Handler()
	alertHTTPObserve(t, store, time.Now().UTC().Add(-time.Minute))
	cookie := routeSessionCookie(t, handler)

	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"through_seq":null}`, `{"other":1}`, `{"through_seq":1,"other":2}`,
		`{"through_seq":-1}`, `{"through_seq":1.5}`, `{"through_seq":"1"}`, `{"through_seq":1,"through_seq":1}`,
		`{"through_seq":1}{"through_seq":1}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/alerts/attention/read", strings.NewReader(body))
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body=%q status=%d response=%s", body, response.Code, response.Body.String())
		}
	}

	future := httptest.NewRequest(http.MethodPost, "/api/alerts/attention/read", strings.NewReader(`{"through_seq":2}`))
	future.AddCookie(cookie)
	futureResponse := httptest.NewRecorder()
	handler.ServeHTTP(futureResponse, future)
	if futureResponse.Code != http.StatusConflict || strings.Contains(futureResponse.Body.String(), "alert delivery") {
		t.Fatalf("future cursor status=%d body=%s", futureResponse.Code, futureResponse.Body.String())
	}

	valid := httptest.NewRequest(http.MethodPost, "/api/alerts/attention/read", strings.NewReader(`{"through_seq":1}`))
	valid.AddCookie(cookie)
	validResponse := httptest.NewRecorder()
	handler.ServeHTTP(validResponse, valid)
	var dto AlertDTO
	if validResponse.Code != http.StatusOK || json.Unmarshal(validResponse.Body.Bytes(), &dto) != nil {
		t.Fatalf("valid cursor status=%d body=%s", validResponse.Code, validResponse.Body.String())
	}
	if dto.Generation != 3 || dto.Attention.HighWaterSeq != 1 || dto.Attention.ReadThroughSeq != 1 || dto.Attention.UnreadCount != 0 || len(dto.Attention.UnreadRefs) != 0 {
		t.Fatalf("read returned incoherent generation: %+v", dto)
	}

	regression := httptest.NewRequest(http.MethodPost, "/api/alerts/attention/read", strings.NewReader(`{"through_seq":0}`))
	regression.AddCookie(cookie)
	regressionResponse := httptest.NewRecorder()
	handler.ServeHTTP(regressionResponse, regression)
	if regressionResponse.Code != http.StatusConflict {
		t.Fatalf("regression status=%d body=%s", regressionResponse.Code, regressionResponse.Body.String())
	}
}

func TestAlertDTOOrdersActiveFirstAndBoundsEndedHistory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	view := alertHTTPView(now, rpc.AlertSnapshotActive, rpc.AlertCoverageCurrent, rpc.AlertEvidenceCurrent, now.Add(time.Hour))
	view.Occurrences = append(view.Occurrences,
		alertHTTPOccurrence("active-old", now.Add(-2*time.Minute), time.Time{}),
		alertHTTPOccurrence("active-new", now.Add(-time.Minute), time.Time{}),
	)
	for i := range alertEndedHistoryLimit + 2 {
		endedAt := now.Add(-time.Duration(i) * time.Minute)
		view.Occurrences = append(view.Occurrences, alertHTTPOccurrence(fmt.Sprintf("ended-%03d", i), endedAt.Add(-time.Minute), endedAt))
	}

	dto := newAlertDTO(view, now)
	if got, want := len(dto.Occurrences), 2+alertEndedHistoryLimit; got != want {
		t.Fatalf("occurrence count=%d want=%d", got, want)
	}
	if dto.Occurrences[0].DisplayID != "active-new" || dto.Occurrences[1].DisplayID != "active-old" || dto.Occurrences[2].DisplayID != "ended-000" {
		t.Fatalf("active/ended order=%v", alertDisplayIDs(dto.Occurrences[:3]))
	}
	if dto.Occurrences[len(dto.Occurrences)-1].DisplayID != "ended-099" {
		t.Fatalf("ended history bound last=%s", dto.Occurrences[len(dto.Occurrences)-1].DisplayID)
	}
}

func TestAlertDTOStaleSourceCannotRenderAllClear(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	view := alertHTTPView(now, rpc.AlertSnapshotClear, rpc.AlertCoverageCurrent, rpc.AlertEvidenceCurrent, now.Add(-time.Second))
	dto := newAlertDTO(view, now)
	if dto.CurrentState == nil || *dto.CurrentState != rpc.AlertSnapshotUnknown {
		t.Fatalf("expired source rendered clear: %+v", dto)
	}
	if len(dto.Sources) != 1 || dto.Sources[0].Reason != "source_current" || dto.Sources[0].FreshUntil == nil || !dto.Sources[0].FreshUntil.Equal(now.Add(-time.Second)) {
		t.Fatalf("source evidence was not preserved exactly: %+v", dto.Sources)
	}

	view.Coverage.Freshness = rpc.AlertCoverageStale
	view.Sources[0].Status = "stale"
	view.Sources[0].Reason = "freshness_expired"
	view.Sources[0].EvidenceHealth = rpc.AlertEvidenceStale
	dto = newAlertDTO(view, now)
	if dto.Sources[0].Status != "stale" || dto.Sources[0].Reason != "freshness_expired" || dto.Sources[0].EvidenceHealth != rpc.AlertEvidenceStale {
		t.Fatalf("stale source collapsed: %+v", dto.Sources[0])
	}
}

func TestAlertDTOUsesFixedPresentationAndRedactedAcceptanceTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	view := alertHTTPView(now, rpc.AlertSnapshotActive, rpc.AlertCoverageCurrent, rpc.AlertEvidenceCurrent, now.Add(time.Hour))
	view.DeliveryHealth.LastAcceptedAt = now.Add(-time.Minute)
	known := alertHTTPOccurrence("known", now, time.Time{})
	unknown := alertHTTPOccurrence("unknown", now.Add(-time.Second), time.Time{})
	unknown.PresentationCode = rpc.AlertPresentationCode("unknown_code")
	view.Occurrences = []state.AlertDeliveryOccurrenceView{known, unknown}

	dto := newAlertDTO(view, now)
	if dto.Occurrences[0].Title != "Portfolio stress" || dto.Occurrences[0].Body != "Canary reports portfolio stress." {
		t.Fatalf("known fixed presentation=%+v", dto.Occurrences[0])
	}
	if dto.Occurrences[1].Title != "Alert unavailable" || dto.Occurrences[1].Body != "This alert cannot be displayed safely." {
		t.Fatalf("unknown presentation did not fail closed: %+v", dto.Occurrences[1])
	}
	if dto.DeliveryHealth.LastPushServiceAcceptanceAt == nil || !dto.DeliveryHealth.LastPushServiceAcceptanceAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("redacted push-service acceptance time=%+v", dto.DeliveryHealth)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"last_push_service_acceptance_at"`) || strings.Contains(string(raw), "device_displayed") || strings.Contains(string(raw), "human_read") {
		t.Fatalf("acceptance contract is ambiguous or missing: %s", raw)
	}
}

func TestAlertInvalidPersistedStateIsRedactedAndAttentionReadRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	privateValue := "private-quarantined-ledger-value"
	persisted := `{"alert_settings":{"mode":"watch_and_act"},"alert_history":[{"id":"legacy-still-usable"}],"alert_delivery":{"version":17,"generation":9,"private_marker":"` + privateValue + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(persisted), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("Open quarantined state: %v", err)
	}
	h := &handler{deps: Dependencies{Store: store}}
	dto := h.alertDTO()
	if dto.Initialized || dto.Generation == 0 || dto.AsOf != nil || dto.CurrentState != nil || dto.Coverage != nil ||
		len(dto.Sources) != 0 || len(dto.Occurrences) != 0 || dto.Attention.UnreadCount != 0 {
		t.Fatalf("quarantine invented public state: %+v", dto)
	}
	if dto.DeliveryHealth.State != state.AlertDeliveryHealthUnavailable || dto.DeliveryHealth.Class != state.AlertDeliveryHealthClassInvalidPersistedState || dto.DeliveryHealth.UpdatedAt == nil {
		t.Fatalf("quarantine health=%+v", dto.DeliveryHealth)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{privateValue, "private_marker", "unsupported-private-version", "alert-delivery-quarantine-sha256-", "artifact", "path"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("public quarantine DTO leaked %q: %s", forbidden, raw)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/api/alerts/attention/read", strings.NewReader(`{"through_seq":0}`))
	response := httptest.NewRecorder()
	h.handleAlertAttentionRead(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "state is unavailable") {
		t.Fatalf("quarantine attention read status=%d body=%s", response.Code, response.Body.String())
	}
	getResponse := httptest.NewRecorder()
	h.handleAlertAttention(getResponse, httptest.NewRequest(http.MethodGet, "/api/alerts/attention", nil))
	if getResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("quarantine attention GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
}

func TestAlertSSESendsInitialAndGenerationUpdate(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	liveService := live.New(routeFakeClient{}, time.Minute, time.Minute)
	h := &handler{deps: Dependencies{Store: store, Live: liveService}}
	recorder := newAlertSSERecorder()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handleEvents(recorder, request)
	}()

	alertSSEWait(t, recorder, func(body string) bool {
		return strings.Count(body, "event: alerts") >= 1 && strings.Contains(body, `"initialized":false`) && strings.Contains(body, `"generation":0`)
	})
	alertHTTPObserve(t, store, time.Now().UTC().Add(-time.Minute))
	alertSSEWait(t, recorder, func(body string) bool {
		return strings.Count(body, "event: alerts") >= 2 && strings.Contains(body, `"initialized":true`) && strings.Contains(body, `"generation":1`)
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not stop after cancellation")
	}
}

func TestAlertSSEEmitsFreshnessExpiryWithoutStoreWrite(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	alertHTTPClearObserve(t, store, at, at.Add(time.Second))
	if generation := store.AlertDelivery(at).Generation; generation != 1 {
		t.Fatalf("initial generation=%d", generation)
	}
	liveService := live.New(routeFakeClient{}, time.Minute, time.Minute)
	h := &handler{deps: Dependencies{Store: store, Live: liveService}}
	recorder := newAlertSSERecorder()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handleEvents(recorder, request)
	}()

	alertSSEWait(t, recorder, func(body string) bool {
		return strings.Count(body, "event: alerts") >= 1 && strings.Contains(body, `"current_state":"clear"`)
	})
	alertSSEWait(t, recorder, func(body string) bool {
		return strings.Count(body, "event: alerts") >= 2 && strings.Contains(body, `"current_state":"unknown"`) && strings.Contains(body, `"freshness":"stale"`)
	})
	if generation := store.AlertDelivery(time.Now().UTC()).Generation; generation != 1 {
		t.Fatalf("freshness expiry wrote generation=%d", generation)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not stop after cancellation")
	}
}

func alertHTTPObserve(t *testing.T, store *state.Store, at time.Time) (string, string) {
	t.Helper()
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "private-account-and-symbol")
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := rpc.BuildAlertOccurrenceKey(episode, "private-opening")
	if err != nil {
		t.Fatal(err)
	}
	candidate := rpc.AlertCandidate{
		EpisodeKey:          episode,
		OccurrenceKey:       occurrence,
		EvidenceFingerprint: "sha256:" + strings.Repeat("a", 64),
		Source:              rpc.AlertSourceStress,
		Kind:                rpc.AlertKindPortfolioRisk,
		PresentationCode:    rpc.AlertPresentationPortfolioStress,
		State:               rpc.AlertEpisodeOpen,
		Severity:            rpc.AlertSeverityWatch,
		EvidenceHealth:      rpc.AlertEvidenceCurrent,
		Destination:         rpc.AlertDestinationAlerts,
		EvidenceAsOf:        at,
		StateChangedAt:      at,
		ObservedAt:          at,
	}
	snapshot := rpc.AlertCandidateSnapshot{
		SchemaVersion:  rpc.AlertCandidateSnapshotVersion,
		AuthorityScope: alertHTTPAuthorityScope(t),
		AsOf:           at,
		CurrentState:   rpc.AlertSnapshotActive,
		Coverage: rpc.AlertCoverage{
			State:           rpc.AlertCoverageComplete,
			Freshness:       rpc.AlertCoverageCurrent,
			AsOf:            at,
			ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress},
			CoveredSources:  []rpc.AlertSource{rpc.AlertSourceStress},
		},
		Sources: []rpc.AlertSourceCoverage{{
			Source:         rpc.AlertSourceStress,
			Status:         "current",
			Reason:         "source_current",
			EvidenceHealth: rpc.AlertEvidenceCurrent,
			InputAsOf:      at,
			ObservedAt:     at,
			EvidenceAsOf:   at,
			FreshUntil:     at.Add(time.Hour),
			Covered:        true,
		}},
		Candidates: []rpc.AlertCandidate{candidate},
	}
	if _, err := store.ObserveAlertSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	return episode, occurrence
}

func alertHTTPClearObserve(t *testing.T, store *state.Store, at, freshUntil time.Time) {
	t.Helper()
	snapshot := rpc.AlertCandidateSnapshot{
		SchemaVersion:  rpc.AlertCandidateSnapshotVersion,
		AuthorityScope: alertHTTPAuthorityScope(t),
		AsOf:           at,
		CurrentState:   rpc.AlertSnapshotClear,
		Coverage: rpc.AlertCoverage{
			State:           rpc.AlertCoverageComplete,
			Freshness:       rpc.AlertCoverageCurrent,
			AsOf:            at,
			ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress},
			CoveredSources:  []rpc.AlertSource{rpc.AlertSourceStress},
		},
		Sources: []rpc.AlertSourceCoverage{{
			Source:         rpc.AlertSourceStress,
			Status:         "current",
			Reason:         "source_current",
			EvidenceHealth: rpc.AlertEvidenceCurrent,
			InputAsOf:      at,
			ObservedAt:     at,
			EvidenceAsOf:   at,
			FreshUntil:     freshUntil,
			Covered:        true,
		}},
		Candidates: []rpc.AlertCandidate{},
	}
	if _, err := store.ObserveAlertSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
}

func alertHTTPAuthorityScope(t *testing.T) string {
	t.Helper()
	scope, err := rpc.BuildAlertAuthorityScope("HTTP-PRIVATE-ACCOUNT", "paper")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func alertHTTPView(now time.Time, current rpc.AlertSnapshotState, freshness rpc.AlertCoverageFreshness, health rpc.AlertEvidenceHealth, freshUntil time.Time) state.AlertDeliveryView {
	return state.AlertDeliveryView{
		Initialized:  true,
		Version:      state.AlertDeliveryVersion,
		Generation:   1,
		AsOf:         now,
		CurrentState: current,
		Coverage: rpc.AlertCoverage{
			State:           rpc.AlertCoverageComplete,
			Freshness:       freshness,
			AsOf:            now,
			ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress},
			CoveredSources:  []rpc.AlertSource{rpc.AlertSourceStress},
		},
		Sources: []rpc.AlertSourceCoverage{{
			Source:         rpc.AlertSourceStress,
			Status:         "current",
			Reason:         "source_current",
			EvidenceHealth: health,
			InputAsOf:      now,
			ObservedAt:     now,
			EvidenceAsOf:   now,
			FreshUntil:     freshUntil,
			Covered:        true,
		}},
		Occurrences:    []state.AlertDeliveryOccurrenceView{},
		Attention:      state.AlertDeliveryAttention{UnreadRefs: []state.AlertDeliveryAttentionRef{}},
		DeliveryHealth: state.AlertDeliveryHealth{State: state.AlertDeliveryHealthHealthy, UpdatedAt: now},
	}
}

func alertHTTPOccurrence(displayID string, stateChangedAt, endedAt time.Time) state.AlertDeliveryOccurrenceView {
	return state.AlertDeliveryOccurrenceView{
		DisplayID:        displayID,
		Source:           rpc.AlertSourceStress,
		Kind:             rpc.AlertKindPortfolioRisk,
		PresentationCode: rpc.AlertPresentationPortfolioStress,
		State:            rpc.AlertEpisodeOpen,
		Severity:         rpc.AlertSeverityWatch,
		EvidenceHealth:   rpc.AlertEvidenceCurrent,
		Destination:      rpc.AlertDestinationAlerts,
		EvidenceAsOf:     stateChangedAt,
		StateChangedAt:   stateChangedAt,
		FirstSeenAt:      stateChangedAt,
		LastSeenAt:       stateChangedAt,
		EndedAt:          endedAt,
		EndReason: func() string {
			if endedAt.IsZero() {
				return ""
			}
			return state.AlertDeliveryEndRecovered
		}(),
		AttentionSeq: 1,
		Disposition:  state.AlertDispositionEligible,
	}
}

func alertDisplayIDs(items []AlertOccurrenceDTO) []string {
	out := make([]string, len(items))
	for i := range items {
		out[i] = items[i].DisplayID
	}
	return out
}

func assertAlertExactJSONKeys(t *testing.T, raw []byte) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, top, "schema_version", "version", "initialized", "generation", "as_of", "current_state", "coverage", "sources", "occurrences", "attention", "delivery_health")
	var health map[string]json.RawMessage
	if err := json.Unmarshal(top["delivery_health"], &health); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, health, "state", "class", "updated_at", "last_push_service_acceptance_at")
	var coverage map[string]json.RawMessage
	if err := json.Unmarshal(top["coverage"], &coverage); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, coverage, "state", "freshness", "as_of", "expected_sources", "covered_sources")
	var sources []map[string]json.RawMessage
	if err := json.Unmarshal(top["sources"], &sources); err != nil || len(sources) != 1 {
		t.Fatalf("sources decode=%v len=%d", err, len(sources))
	}
	assertJSONKeys(t, sources[0], "source", "status", "reason", "evidence_health", "input_as_of", "observed_at", "evidence_as_of", "fresh_until", "covered")
	var attention map[string]json.RawMessage
	if err := json.Unmarshal(top["attention"], &attention); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, attention, "unread_count", "high_water_seq", "read_through_seq", "unread_refs")
	var unreadRefs []map[string]json.RawMessage
	if err := json.Unmarshal(attention["unread_refs"], &unreadRefs); err != nil || len(unreadRefs) != 1 {
		t.Fatalf("unread_refs decode=%v len=%d", err, len(unreadRefs))
	}
	assertJSONKeys(t, unreadRefs[0], "display_id", "source", "kind")
	var occurrences []map[string]json.RawMessage
	if err := json.Unmarshal(top["occurrences"], &occurrences); err != nil || len(occurrences) != 1 {
		t.Fatalf("occurrences decode=%v len=%d", err, len(occurrences))
	}
	assertJSONKeys(t, occurrences[0], "display_id", "source", "kind", "presentation_code", "title", "body", "state", "severity", "evidence_health", "destination", "evidence_as_of", "state_changed_at", "first_seen_at", "last_seen_at", "ended_at", "end_reason", "attention_seq", "disposition")
}

func assertJSONKeys(t *testing.T, value map[string]json.RawMessage, expected ...string) {
	t.Helper()
	if len(value) != len(expected) {
		t.Fatalf("JSON keys=%v want=%v", reflect.ValueOf(value).MapKeys(), expected)
	}
	for _, key := range expected {
		if _, ok := value[key]; !ok {
			t.Fatalf("JSON missing key %q in %v", key, reflect.ValueOf(value).MapKeys())
		}
	}
}

type alertSSERecorder struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	status int
}

func newAlertSSERecorder() *alertSSERecorder    { return &alertSSERecorder{header: make(http.Header)} }
func (r *alertSSERecorder) Header() http.Header { return r.header }
func (r *alertSSERecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}
func (r *alertSSERecorder) Write(value []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(value)
}
func (r *alertSSERecorder) Flush()         {}
func (r *alertSSERecorder) String() string { r.mu.Lock(); defer r.mu.Unlock(); return r.body.String() }

func alertSSEWait(t *testing.T, recorder *alertSSERecorder, ready func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready(recorder.String()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for SSE state: %s", recorder.String())
}

var _ http.Flusher = (*alertSSERecorder)(nil)
var _ io.Writer = (*alertSSERecorder)(nil)
