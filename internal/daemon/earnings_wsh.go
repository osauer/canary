package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// wshEarningsClient is the narrow read-only seam between the daemon's
// earnings authority and the broker connector. Production supplies the live
// Connector; tests use a deterministic implementation without a Gateway.
type wshEarningsClient interface {
	FetchWSHEarnings(context.Context, string) (string, error)
}

type wshStockIdentityClient interface {
	ResolveWSHStockIdentity(context.Context, string, int) (*ibkrlib.ContractDetailsLite, error)
}

var (
	errWSHEarningsGatewayUnavailable = errors.New("IBKR WSH gateway unavailable")
	errWSHEarningsRequestFailed      = errors.New("IBKR WSH request failed")
	errWSHEarningsPayloadInvalid     = errors.New("IBKR WSH earnings payload invalid")
	errEarningsIdentityRequestFailed = errors.New("IBKR earnings identity request failed")
)

// fetchEarningsIdentity is an exact contract-details read, deliberately
// independent from WSH metadata/event entitlement and provider backoff.
func (s *Server) fetchEarningsIdentity(ctx context.Context, sym string, conID int) (earningsIdentityFetchResult, error) {
	now := time.Now().UTC()
	if s != nil && s.now != nil {
		now = s.now().UTC()
	}
	if s == nil {
		return earningsIdentityFailure(rpc.SourceFailureGatewayUnavailable, now), errEarningsIdentityRequestFailed
	}
	return fetchEarningsIdentityFrom(ctx, sym, conID, now, s.gatewayConnector())
}

func fetchEarningsIdentityFrom(ctx context.Context, sym string, conID int, now time.Time, client wshStockIdentityClient) (earningsIdentityFetchResult, error) {
	if client == nil || conID <= 0 {
		return earningsIdentityFailure(rpc.SourceFailureGatewayUnavailable, now), errEarningsIdentityRequestFailed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, earningsFetchTimeout)
	defer cancel()
	detail, err := client.ResolveWSHStockIdentity(ctx, sym, conID)
	if err != nil {
		code := rpc.SourceFailureContractUnavailable
		if errors.Is(err, context.DeadlineExceeded) {
			code = rpc.SourceFailureTimeout
		}
		var wshErr *ibkrlib.WSHError
		if errors.As(err, &wshErr) && wshErr != nil {
			switch wshErr.Kind {
			case ibkrlib.WSHErrorTransport:
				code = rpc.SourceFailureGatewayUnavailable
			case ibkrlib.WSHErrorTimeout:
				code = rpc.SourceFailureTimeout
			}
		}
		return earningsIdentityFailure(code, now), sanitizedEarningsIdentityError(err)
	}
	if detail == nil || detail.ConID != conID || detail.SecType != "STK" {
		result := earningsIdentityFailure(rpc.SourceFailureContractUnavailable, now)
		result.RetainProof = false
		return result, errEarningsIdentityRequestFailed
	}
	switch detail.StockType {
	case "ETF":
		return earningsIdentityFetchResult{Outcome: earningsIdentityNotApplicable}, nil
	case "COMMON":
		return earningsIdentityFetchResult{Outcome: earningsIdentityIssuer}, nil
	default:
		return earningsIdentityFetchResult{Outcome: earningsIdentityUnknown}, nil
	}
}

func earningsIdentityFailure(code string, now time.Time) earningsIdentityFetchResult {
	return earningsIdentityFetchResult{
		Outcome:     earningsIdentityUnknown,
		Failure:     &rpc.SourceFailure{Code: code, Stage: rpc.SourceFailureStageWSHContractResolve, FailedAt: now.UTC(), Retryable: true},
		RetainProof: true,
	}
}

func sanitizedEarningsIdentityError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(errEarningsIdentityRequestFailed, context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(errEarningsIdentityRequestFailed, context.Canceled)
	}
	return errEarningsIdentityRequestFailed
}

// fetchWSHEarningsProvider is the approved secondary-provider hook for the
// earnings cache. It deliberately projects raw broker JSON and errors into a
// small typed result before either can reach persistence or RPC.
func (s *Server) fetchWSHEarningsProvider(ctx context.Context, sym string) (earningsProviderFetchResult, error) {
	now := time.Now().UTC()
	if s != nil && s.now != nil {
		now = s.now().UTC()
	}
	if s == nil {
		return transportFailureResult(rpc.SourceFailureGatewayUnavailable, rpc.SourceFailureStageWSHMetadata, true, now), errWSHEarningsGatewayUnavailable
	}
	return fetchWSHEarningsProviderFrom(ctx, sym, now, s.gatewayConnector())
}

func fetchWSHEarningsProviderFrom(ctx context.Context, sym string, now time.Time, client wshEarningsClient) (earningsProviderFetchResult, error) {
	now = now.UTC()
	if client == nil {
		return transportFailureResult(rpc.SourceFailureGatewayUnavailable, rpc.SourceFailureStageWSHMetadata, true, now), errWSHEarningsGatewayUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, earningsFetchTimeout)
	defer cancel()
	raw, err := client.FetchWSHEarnings(ctx, sym)
	if err != nil {
		result := classifyWSHEarningsError(err, now)
		return result, sanitizedWSHEarningsError(err)
	}
	return parseWSHEarningsPayload([]byte(raw), now)
}

func classifyWSHEarningsError(err error, now time.Time) earningsProviderFetchResult {
	stage := rpc.SourceFailureStageWSHEvent
	code := rpc.SourceFailureTransportFailed
	retryable := true
	status := rpc.EarningsStatusTransportFailure

	if errors.Is(err, context.DeadlineExceeded) {
		code = rpc.SourceFailureTimeout
	}

	var wshErr *ibkrlib.WSHError
	if !errors.As(err, &wshErr) || wshErr == nil {
		return transportFailureResult(code, stage, retryable, now)
	}
	stage = wshFailureStage(wshErr.Operation)
	switch wshErr.Kind {
	case ibkrlib.WSHErrorConnectorInactive:
		code = rpc.SourceFailureContractUnavailable
	case ibkrlib.WSHErrorUnsupportedSecurity:
		return earningsProviderFetchResult{Status: rpc.EarningsStatusUnsupportedSecurity}
	case ibkrlib.WSHErrorMalformedResponse:
		status = rpc.EarningsStatusFormatChange
		code = rpc.SourceFailureInvalidPayload
		retryable = false
		if wshErr.Operation == "event_data" {
			stage = rpc.SourceFailureStageWSHDecode
		}
	case ibkrlib.WSHErrorEventTypeUnavailable:
		status = rpc.EarningsStatusFormatChange
		code = rpc.SourceFailureInvalidPayload
		stage = rpc.SourceFailureStageWSHMetadata
		retryable = false
	case ibkrlib.WSHErrorTimeout:
		code = rpc.SourceFailureTimeout
	case ibkrlib.WSHErrorTransport:
		code = rpc.SourceFailureGatewayUnavailable
	case ibkrlib.WSHErrorUnsupportedProtocol:
		code = rpc.SourceFailureProtocolRejected
		retryable = false
	case ibkrlib.WSHErrorEntitlementRequired:
		code = rpc.SourceFailureNotEntitled
		retryable = false
	case ibkrlib.WSHErrorDuplicateRequest:
		code = rpc.SourceFailureProtocolRejected
	case ibkrlib.WSHErrorMetadataRequired:
		code = rpc.SourceFailureProtocolRejected
		stage = rpc.SourceFailureStageWSHMetadata
	case ibkrlib.WSHErrorCanceled, ibkrlib.WSHErrorContractResolution, ibkrlib.WSHErrorProviderFailure:
		code = rpc.SourceFailureTransportFailed
	default:
		code = rpc.SourceFailureTransportFailed
	}
	return earningsProviderFetchResult{
		Status: status,
		Failure: &rpc.SourceFailure{
			Code:      code,
			Stage:     stage,
			FailedAt:  now,
			Retryable: retryable,
		},
	}
}

func wshFailureStage(operation string) string {
	switch strings.TrimSpace(operation) {
	case "resolve_contract":
		return rpc.SourceFailureStageWSHContractResolve
	case "acquire", "metadata":
		return rpc.SourceFailureStageWSHMetadata
	case "event_data":
		return rpc.SourceFailureStageWSHEvent
	default:
		return rpc.SourceFailureStageWSHEvent
	}
}

func sanitizedWSHEarningsError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(errWSHEarningsRequestFailed, context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(errWSHEarningsRequestFailed, context.Canceled)
	}
	return errWSHEarningsRequestFailed
}

type wshEarningsEvent struct {
	Date      string
	TimeOfDay string
	Estimated bool
}

// parseWSHEarningsPayload accepts the two WSH callback envelopes observed at
// the API boundary: a direct event array, or an object containing that array
// under "events". An empty array is an explicit successful publication with
// no next date. Any non-empty record must satisfy the allowlisted earnings
// schema; parser drift never becomes a guessed date.
func parseWSHEarningsPayload(body []byte, now time.Time) (earningsProviderFetchResult, error) {
	events, err := decodeWSHEarningsEvents(body)
	if err != nil {
		return wshFormatChangeResult(now), errWSHEarningsPayloadInvalid
	}
	if len(events) == 0 {
		return earningsProviderFetchResult{Status: rpc.EarningsStatusNoDatePublished}, nil
	}

	today := earningsCalendarDate(now)
	byDate := make(map[string]wshEarningsEvent)
	for _, raw := range events {
		event, err := decodeWSHEarningsEvent(raw)
		if err != nil {
			return wshFormatChangeResult(now), errWSHEarningsPayloadInvalid
		}
		if event.Date < today {
			continue
		}
		previous, exists := byDate[event.Date]
		if !exists {
			byDate[event.Date] = event
			continue
		}
		if previous.TimeOfDay != "" && event.TimeOfDay != "" && previous.TimeOfDay != event.TimeOfDay {
			return wshFormatChangeResult(now), errWSHEarningsPayloadInvalid
		}
		if previous.TimeOfDay == "" {
			previous.TimeOfDay = event.TimeOfDay
		}
		previous.Estimated = previous.Estimated || event.Estimated
		byDate[event.Date] = previous
	}
	if len(byDate) == 0 {
		return earningsProviderFetchResult{Status: rpc.EarningsStatusNoDatePublished}, nil
	}

	next := ""
	for date := range byDate {
		if next == "" || date < next {
			next = date
		}
	}
	event := byDate[next]
	return earningsProviderFetchResult{
		Status: rpc.EarningsStatusDate,
		Entry: earningsEntry{
			Date:       event.Date,
			TimeOfDay:  event.TimeOfDay,
			Estimated:  event.Estimated,
			ObservedAt: now.UTC(),
		},
	}, nil
}

func wshFormatChangeResult(now time.Time) earningsProviderFetchResult {
	return earningsProviderFetchResult{
		Status: rpc.EarningsStatusFormatChange,
		Failure: &rpc.SourceFailure{
			Code:      rpc.SourceFailureInvalidPayload,
			Stage:     rpc.SourceFailureStageWSHDecode,
			FailedAt:  now.UTC(),
			Retryable: false,
		},
	}
}

func decodeWSHEarningsEvents(body []byte) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if err := requireWSHJSONEOF(decoder); err != nil {
		return nil, err
	}
	root = bytes.TrimSpace(root)
	if len(root) == 0 {
		return nil, errors.New("empty WSH payload")
	}
	if root[0] == '[' {
		var events []json.RawMessage
		if err := json.Unmarshal(root, &events); err != nil {
			return nil, err
		}
		return events, nil
	}
	if root[0] != '{' {
		return nil, errors.New("WSH payload is not an event envelope")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(root, &envelope); err != nil {
		return nil, err
	}
	eventsRaw, ok := envelope["events"]
	if !ok || bytes.Equal(bytes.TrimSpace(eventsRaw), []byte("null")) {
		return nil, errors.New("WSH payload has no events array")
	}
	var events []json.RawMessage
	if err := json.Unmarshal(eventsRaw, &events); err != nil {
		return nil, err
	}
	return events, nil
}

func requireWSHJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("WSH payload contains multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeWSHEarningsEvent(raw json.RawMessage) (wshEarningsEvent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return wshEarningsEvent{}, errors.New("WSH event is not an object")
	}
	if eventType, ok, err := optionalWSHString(fields, "event_type"); err != nil {
		return wshEarningsEvent{}, err
	} else if ok && eventType != "" && !isWSHEarningsEventType(eventType) {
		return wshEarningsEvent{}, errors.New("WSH event is not an earnings-date event")
	}
	if eventType, ok, err := optionalWSHString(fields, "event_code"); err != nil {
		return wshEarningsEvent{}, err
	} else if ok && eventType != "" && !isWSHEarningsEventType(eventType) {
		return wshEarningsEvent{}, errors.New("WSH event code is not earnings-date")
	}

	// IBKR's documented WSH event record keeps identity fields on the outer
	// object and event-specific values beneath "data". Accept the historical
	// flattened callback shape only when "data" is absent; never merge the two
	// namespaces or guess across conflicting provider formats.
	eventFields := fields
	if rawData, ok := fields["data"]; ok {
		if bytes.Equal(bytes.TrimSpace(rawData), []byte("null")) {
			return wshEarningsEvent{}, errors.New("WSH event data is null")
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(rawData, &nested); err != nil || nested == nil {
			return wshEarningsEvent{}, errors.New("WSH event data is not an object")
		}
		eventFields = nested
	}

	date, err := coalescedNormalizedWSHString(eventFields, normalizeWSHEarningsDate, "earnings_date", "date")
	if err != nil || date == "" {
		return wshEarningsEvent{}, errors.New("WSH event has no valid earnings date")
	}

	timeOfDay, err := coalescedNormalizedWSHString(eventFields, normalizeWSHEarningsTime, "time_of_day", "time")
	if err != nil {
		return wshEarningsEvent{}, err
	}

	estimated := false
	if rawEstimated, ok := eventFields["estimated"]; ok {
		if err := json.Unmarshal(rawEstimated, &estimated); err != nil {
			return wshEarningsEvent{}, errors.New("WSH estimated field is not boolean")
		}
	}
	if status, ok, err := optionalWSHString(eventFields, "wshe_earnings_date_status"); err != nil {
		return wshEarningsEvent{}, err
	} else if ok && status != "" {
		switch strings.ToUpper(strings.TrimSpace(status)) {
		case "CONFIRMED":
		case "UNCONFIRMED", "INFERRED":
			estimated = true
		default:
			return wshEarningsEvent{}, errors.New("WSH earnings status changed")
		}
	}
	return wshEarningsEvent{Date: date, TimeOfDay: timeOfDay, Estimated: estimated}, nil
}

func optionalWSHString(fields map[string]json.RawMessage, key string) (string, bool, error) {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", ok, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, errors.New("WSH field is not a string")
	}
	return strings.TrimSpace(value), true, nil
}

func coalescedNormalizedWSHString(fields map[string]json.RawMessage, normalize func(string) (string, error), keys ...string) (string, error) {
	value := ""
	for _, key := range keys {
		candidate, ok, err := optionalWSHString(fields, key)
		if err != nil {
			return "", err
		}
		if !ok || candidate == "" {
			continue
		}
		candidate, err = normalize(candidate)
		if err != nil {
			return "", err
		}
		if candidate == "" {
			continue
		}
		if value != "" && value != candidate {
			return "", errors.New("WSH aliases disagree")
		}
		value = candidate
	}
	return value, nil
}

func isWSHEarningsEventType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "wsh_ed", "wshe_ed", "earnings date", "earnings_date":
		return true
	default:
		return false
	}
}

func normalizeWSHEarningsDate(value string) (string, error) {
	value = strings.TrimSpace(value)
	var layout string
	switch len(value) {
	case len("20060102"):
		layout = "20060102"
	case len(time.DateOnly):
		layout = time.DateOnly
	default:
		return "", errors.New("WSH earnings date format changed")
	}
	parsed, err := time.Parse(layout, value)
	if err != nil {
		return "", errors.New("WSH earnings date is invalid")
	}
	return parsed.Format(time.DateOnly), nil
}

func normalizeWSHEarningsTime(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.NewReplacer("_", " ", "-", " ").Replace(value)
	value = strings.Join(strings.Fields(value), " ")
	switch value {
	case "", "UNSPECIFIED", "DURING MARKET":
		return "", nil
	case "BMO", "BEFORE MARKET", "BEFORE MARKET OPEN", "BEFORE MARKET OPENS", "PRE MARKET":
		return "bmo", nil
	case "AMC", "AFTER MARKET", "AFTER MARKET CLOSE", "AFTER MARKET CLOSES", "POST MARKET":
		return "amc", nil
	default:
		return "", errors.New("WSH time-of-day format changed")
	}
}
