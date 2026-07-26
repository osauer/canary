package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestParseWSHEarningsPayloadSelectsNextDateAndNormalizesFields(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 21, 12, 34, 56, 0, time.UTC)
	tests := []struct {
		name      string
		payload   string
		date      string
		timeOfDay string
		estimated bool
	}{
		{
			name:      "documented nested WSH event data",
			payload:   `[{"event_type":"wshe_ed","data":{"earnings_date":"20260731","time_of_day":"AFTER MARKET","wshe_earnings_date_status":"CONFIRMED"}}]`,
			date:      "2026-07-31",
			timeOfDay: "amc",
		},
		{
			name:      "legacy flattened callback fields",
			payload:   `[{"event_type":"wshe_ed","earnings_date":"20260731","time_of_day":"AFTER MARKET","wshe_earnings_date_status":"CONFIRMED"}]`,
			date:      "2026-07-31",
			timeOfDay: "amc",
		},
		{
			name:      "generic callback aliases",
			payload:   `{"events":[{"event_code":"wsh_ed","date":"2026-07-22","time":"before_market_open","wshe_earnings_date_status":"INFERRED"}]}`,
			date:      "2026-07-22",
			timeOfDay: "bmo",
			estimated: true,
		},
		{
			name: "earliest current or future wins",
			payload: `{"events":[
				{"earnings_date":"20260720","time_of_day":"AFTER MARKET"},
				{"earnings_date":"20260815","time_of_day":"BEFORE MARKET"},
				{"earnings_date":"20260721","time_of_day":"DURING MARKET"}
			]}`,
			date: "2026-07-21",
		},
		{
			name: "same date compatible records merge conservatively",
			payload: `[
				{"earnings_date":"20260730","time_of_day":"UNSPECIFIED","wshe_earnings_date_status":"CONFIRMED"},
				{"earnings_date":"2026-07-30","time_of_day":"BMO","wshe_earnings_date_status":"UNCONFIRMED"}
			]`,
			date:      "2026-07-30",
			timeOfDay: "bmo",
			estimated: true,
		},
		{
			name:      "recognized post market alias",
			payload:   `[{"date":"2026-07-30","time":"post-market","estimated":true}]`,
			date:      "2026-07-30",
			timeOfDay: "amc",
			estimated: true,
		},
		{
			name:      "equivalent aliases agree after normalization",
			payload:   `[{"earnings_date":"20260730","date":"2026-07-30","time_of_day":"AFTER MARKET","time":"AMC"}]`,
			date:      "2026-07-30",
			timeOfDay: "amc",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseWSHEarningsPayload([]byte(tc.payload), now)
			if err != nil {
				t.Fatalf("parseWSHEarningsPayload: %v", err)
			}
			if got.Status != rpc.EarningsStatusDate || got.Failure != nil {
				t.Fatalf("result = %+v, want clean date", got)
			}
			if got.Entry.Date != tc.date || got.Entry.TimeOfDay != tc.timeOfDay || got.Entry.Estimated != tc.estimated {
				t.Fatalf("entry = %+v, want date=%s time=%q estimated=%v", got.Entry, tc.date, tc.timeOfDay, tc.estimated)
			}
			if !got.Entry.ObservedAt.Equal(now) {
				t.Fatalf("observed_at = %v, want %v", got.Entry.ObservedAt, now)
			}
		})
	}
}

func TestParseWSHEarningsPayloadNoDatePublished(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 21, 23, 0, 0, 0, time.UTC)
	for _, payload := range []string{
		`[]`,
		`{"events":[]}`,
		`[{"earnings_date":"20260720","time_of_day":"AFTER MARKET"}]`,
	} {
		got, err := parseWSHEarningsPayload([]byte(payload), now)
		if err != nil {
			t.Fatalf("payload %s: %v", payload, err)
		}
		if got.Status != rpc.EarningsStatusNoDatePublished || got.Failure != nil || got.Entry != (earningsEntry{}) {
			t.Fatalf("payload %s result = %+v, want explicit no-date", payload, got)
		}
	}
}

func TestParseWSHEarningsPayloadSchemaFailuresStayTypedAndRedacted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	secret := "PRIVATE-UPSTREAM-CONTENT"
	tests := []struct {
		name    string
		payload string
	}{
		{"invalid JSON", `{` + secret},
		{"trailing JSON", `[] {}`},
		{"missing events envelope", `{"data":[]}`},
		{"null events", `{"events":null}`},
		{"events not array", `{"events":{}}`},
		{"null event", `[null]`},
		{"null event data", `[{"event_type":"wshe_ed","data":null}]`},
		{"event data not object", `[{"event_type":"wshe_ed","data":[]}]`},
		{"nested data does not fall back to outer fields", `[{"event_type":"wshe_ed","earnings_date":"20260730","data":{}}]`},
		{"missing date", `[{"time_of_day":"AFTER MARKET"}]`},
		{"date is not string", `[{"earnings_date":20260730}]`},
		{"invalid date", `[{"earnings_date":"20260230"}]`},
		{"unsupported date shape", `[{"earnings_date":"07/30/2026"}]`},
		{"date aliases conflict", `[{"earnings_date":"20260730","date":"2026-08-01"}]`},
		{"time is not string", `[{"earnings_date":"20260730","time_of_day":1}]`},
		{"unknown time", `[{"earnings_date":"20260730","time_of_day":"at lunch"}]`},
		{"unknown status", `[{"earnings_date":"20260730","wshe_earnings_date_status":"MAYBE"}]`},
		{"estimated is not boolean", `[{"earnings_date":"20260730","estimated":"yes"}]`},
		{"wrong event type", `[{"event_type":"wshe_div","earnings_date":"20260730"}]`},
		{"same date time conflict", `[{"earnings_date":"20260730","time_of_day":"BEFORE MARKET"},{"earnings_date":"2026-07-30","time_of_day":"AFTER MARKET"}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseWSHEarningsPayload([]byte(tc.payload), now)
			if !errors.Is(err, errWSHEarningsPayloadInvalid) {
				t.Fatalf("error = %v, want sanitized payload error", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked raw payload: %v", err)
			}
			assertWSHFormatChangeResult(t, got, now)
		})
	}
}

func TestClassifyWSHEarningsError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		err       error
		status    string
		code      string
		stage     string
		retryable bool
	}{
		{"unsupported security", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorUnsupportedSecurity, Operation: "resolve_contract"}, rpc.EarningsStatusUnsupportedSecurity, "", "", false},
		{"connector inactive", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorConnectorInactive, Operation: "resolve_contract"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureContractUnavailable, rpc.SourceFailureStageWSHContractResolve, true},
		{"malformed event", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorMalformedResponse, Operation: "event_data"}, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageWSHDecode, false},
		{"malformed metadata", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorMalformedResponse, Operation: "metadata"}, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageWSHMetadata, false},
		{"event type unavailable", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorEventTypeUnavailable, Operation: "metadata"}, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageWSHMetadata, false},
		{"resolve timeout", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorTimeout, Operation: "resolve_contract"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureTimeout, rpc.SourceFailureStageWSHContractResolve, true},
		{"gateway transport", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorTransport, Operation: "metadata"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureGatewayUnavailable, rpc.SourceFailureStageWSHMetadata, true},
		{"unsupported protocol", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorUnsupportedProtocol, Operation: "event_data"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageWSHEvent, false},
		{"entitlement", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorEntitlementRequired, Operation: "event_data"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false},
		{"duplicate", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorDuplicateRequest, Operation: "event_data"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageWSHEvent, true},
		{"metadata required", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorMetadataRequired, Operation: "event_data"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageWSHMetadata, true},
		{"contract lookup", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorContractResolution, Operation: "resolve_contract"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureTransportFailed, rpc.SourceFailureStageWSHContractResolve, true},
		{"provider failure", &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorProviderFailure, Operation: "metadata"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureTransportFailed, rpc.SourceFailureStageWSHMetadata, true},
		{"plain deadline", context.DeadlineExceeded, rpc.EarningsStatusTransportFailure, rpc.SourceFailureTimeout, rpc.SourceFailureStageWSHEvent, true},
		{"plain error", errors.New("raw provider text"), rpc.EarningsStatusTransportFailure, rpc.SourceFailureTransportFailed, rpc.SourceFailureStageWSHEvent, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyWSHEarningsError(tc.err, now)
			if got.Status != tc.status {
				t.Fatalf("status = %q, want %q", got.Status, tc.status)
			}
			if tc.code == "" {
				if got.Failure != nil {
					t.Fatalf("failure = %+v, want nil", got.Failure)
				}
				return
			}
			if got.Failure == nil {
				t.Fatal("missing typed failure")
			}
			if got.Failure.Code != tc.code || got.Failure.Stage != tc.stage || got.Failure.Retryable != tc.retryable || !got.Failure.FailedAt.Equal(now) {
				t.Fatalf("failure = %+v, want code=%s stage=%s retryable=%v at=%v", got.Failure, tc.code, tc.stage, tc.retryable, now)
			}
			if !rpc.ValidSourceFailure(got.Failure) {
				t.Fatalf("failure is outside RPC allowlist: %+v", got.Failure)
			}
		})
	}
}

func TestFetchWSHEarningsProviderFromProjectsSuccessAndSanitizesFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.FixedZone("test", 2*60*60))
	client := &fakeWSHEarningsClient{payload: `[{"earnings_date":"20260731","time_of_day":"AFTER MARKET"}]`}
	got, err := fetchWSHEarningsProviderFrom(context.Background(), "AAPL", now, client)
	if err != nil {
		t.Fatalf("fetchWSHEarningsProviderFrom: %v", err)
	}
	if client.symbol != "AAPL" || got.Status != rpc.EarningsStatusDate || got.Entry.Date != "2026-07-31" || got.Entry.TimeOfDay != "amc" {
		t.Fatalf("symbol=%q result=%+v", client.symbol, got)
	}
	if got.Entry.ObservedAt.Location() != time.UTC || !got.Entry.ObservedAt.Equal(now) {
		t.Fatalf("observed_at = %v, want UTC instant %v", got.Entry.ObservedAt, now)
	}

	const secret = "account U123 token PRIVATE"
	client = &fakeWSHEarningsClient{err: errors.New(secret)}
	got, err = fetchWSHEarningsProviderFrom(context.Background(), "AAPL", now, client)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("raw provider error escaped sanitization: %v", err)
	}
	if got.Status != rpc.EarningsStatusTransportFailure || got.Failure == nil || got.Failure.Code != rpc.SourceFailureTransportFailed {
		t.Fatalf("failure result = %+v", got)
	}

	client = &fakeWSHEarningsClient{err: &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorKind(secret), Operation: secret}}
	_, err = fetchWSHEarningsProviderFrom(context.Background(), "AAPL", now, client)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unrecognized typed provider error escaped sanitization: %v", err)
	}
}

func TestFetchWSHEarningsProviderFromNilClientIsGatewayFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	got, err := fetchWSHEarningsProviderFrom(context.Background(), "AAPL", now, nil)
	if !errors.Is(err, errWSHEarningsGatewayUnavailable) {
		t.Fatalf("error = %v, want gateway unavailable", err)
	}
	if got.Status != rpc.EarningsStatusTransportFailure || got.Failure == nil || got.Failure.Code != rpc.SourceFailureGatewayUnavailable || got.Failure.Stage != rpc.SourceFailureStageWSHMetadata || !got.Failure.Retryable {
		t.Fatalf("result = %+v", got)
	}
}

type fakeWSHEarningsClient struct {
	payload string
	err     error
	symbol  string
}

func (c *fakeWSHEarningsClient) FetchWSHEarnings(_ context.Context, symbol string) (string, error) {
	c.symbol = symbol
	return c.payload, c.err
}

func assertWSHFormatChangeResult(t *testing.T, got earningsProviderFetchResult, now time.Time) {
	t.Helper()
	if got.Status != rpc.EarningsStatusFormatChange || got.Entry != (earningsEntry{}) {
		t.Fatalf("result = %+v, want format change with no entry", got)
	}
	if got.Failure == nil || got.Failure.Code != rpc.SourceFailureInvalidPayload || got.Failure.Stage != rpc.SourceFailureStageWSHDecode || got.Failure.Retryable || !got.Failure.FailedAt.Equal(now) {
		t.Fatalf("failure = %+v, want non-retryable WSH decode failure at %v", got.Failure, now)
	}
	if !rpc.ValidSourceFailure(got.Failure) {
		t.Fatalf("failure is outside RPC allowlist: %+v", got.Failure)
	}
}
