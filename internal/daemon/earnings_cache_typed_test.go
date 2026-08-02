package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

type earningsRoundTripFunc func(*http.Request) (*http.Response, error)

func (f earningsRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func nasdaqTestPayload(t testing.TB, data any, rCode int) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": data, "status": map[string]any{"rCode": rCode}})
	if err != nil {
		t.Fatal("marshal synthetic Nasdaq payload")
	}
	return body
}

func TestParseNasdaqEarningsTypedOutcomes(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	providerSymbol := "TESTQ"
	prefix := nasdaqAnnouncementPrefix(providerSymbol)
	tests := []struct {
		name           string
		body           []byte
		expectedSymbol string
		status         string
		code           string
		stage          string
	}{
		{"prefix only", nasdaqTestPayload(t, map[string]any{"announcement": prefix}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"observed exact no-date envelope", []byte(`{"data":{"announcement":"Earnings announcement* for TESTQ: "},"status":{"rCode":200}}`), providerSymbol, rpc.EarningsStatusNoDatePublished, "", ""},
		{"leading space", nasdaqTestPayload(t, map[string]any{"announcement": " " + prefix}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"leading tab", nasdaqTestPayload(t, map[string]any{"announcement": "\t" + prefix}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"leading newline", nasdaqTestPayload(t, map[string]any{"announcement": "\n" + prefix}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"multiple trailing spaces", nasdaqTestPayload(t, map[string]any{"announcement": prefix + "  "}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"trailing tab", nasdaqTestPayload(t, map[string]any{"announcement": prefix + "\t"}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"trailing newline", nasdaqTestPayload(t, map[string]any{"announcement": prefix + "\n"}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"multiple spaces before date", nasdaqTestPayload(t, map[string]any{"announcement": prefix + "  Jul 30, 2026"}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"date trailing space", nasdaqTestPayload(t, map[string]any{"announcement": prefix + " Jul 30, 2026 "}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"announcement absent", nasdaqTestPayload(t, map[string]any{}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"announcement null", nasdaqTestPayload(t, map[string]any{"announcement": nil}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"announcement empty", nasdaqTestPayload(t, map[string]any{"announcement": ""}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"wrong expected symbol", nasdaqTestPayload(t, map[string]any{"announcement": prefix + " Jul 30, 2026"}, http.StatusOK), "OTHERQ", rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"arbitrary prose ending in date", nasdaqTestPayload(t, map[string]any{"announcement": "untrusted text Jul 30, 2026"}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"extra content", nasdaqTestPayload(t, map[string]any{"announcement": prefix + " Jul 30, 2026 extra"}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"unstarred prefix", nasdaqTestPayload(t, map[string]any{"announcement": strings.Replace(prefix, "*", "", 1)}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"wrong type", nasdaqTestPayload(t, map[string]any{"announcement": 17}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"malformed date", nasdaqTestPayload(t, map[string]any{"announcement": prefix + " Jul 030, 2026"}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"malformed JSON", []byte{'{'}, providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqDecode},
		{"missing data", []byte(`{"status":{"rCode":200}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"missing status with no date", []byte(`{"data":{"announcement":null}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"missing status with date", []byte(`{"data":{"announcement":"Earnings announcement* for TESTQ: Jul 30, 2026"}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"null nested status", []byte(`{"data":{"announcement":null,"status":null}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"missing top-level status code", []byte(`{"data":{"announcement":"Earnings announcement* for TESTQ: Jul 30, 2026"},"status":{}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"null top-level status code", []byte(`{"data":{"announcement":"Earnings announcement* for TESTQ: Jul 30, 2026"},"status":{"rCode":null}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"invalid top-level status code type", []byte(`{"data":{"announcement":"Earnings announcement* for TESTQ: Jul 30, 2026"},"status":{"rCode":"200"}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"retired nested-only status date", []byte(`{"data":{"announcement":"Earnings announcement* for TESTQ: Jul 30, 2026","status":{"rCode":200}}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"conflicting nested status", []byte(`{"data":{"announcement":"Earnings announcement* for TESTQ: Jul 30, 2026","status":{"rCode":200}},"status":{"rCode":200}}`), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"bad request with data", nasdaqTestPayload(t, map[string]any{"announcement": prefix + " Jul 30, 2026"}, http.StatusBadRequest), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"not found with data", nasdaqTestPayload(t, map[string]any{"announcement": prefix + " Jul 30, 2026"}, http.StatusNotFound), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"server error with data", nasdaqTestPayload(t, map[string]any{"announcement": prefix + " Jul 30, 2026"}, http.StatusInternalServerError), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"unsupported bad request", nasdaqTestPayload(t, nil, http.StatusBadRequest), providerSymbol, rpc.EarningsStatusUnsupportedSecurity, "", ""},
		{"not found is not semantic unsupported", nasdaqTestPayload(t, nil, http.StatusNotFound), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"unsupported ignores prose", []byte(`{"data":null,"status":{"rCode":400,"bCodeMessage":"untrusted provider prose"}}`), providerSymbol, rpc.EarningsStatusUnsupportedSecurity, "", ""},
		{"elapsed date", nasdaqTestPayload(t, map[string]any{"announcement": prefix + " Jul 20, 2026"}, http.StatusOK), providerSymbol, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry, err := parseNasdaqEarnings(test.body, test.expectedSymbol, now)
			if err == nil {
				t.Fatal("typed unresolved payload returned nil error")
			}
			if entry != (earningsEntry{}) {
				t.Fatal("typed unresolved payload produced a usable entry")
			}
			var outcome *earningsProviderError
			if !errors.As(err, &outcome) {
				t.Fatalf("error type = %T, want *earningsProviderError", err)
			}
			if outcome.status != test.status {
				t.Fatalf("status = %q, want %q", outcome.status, test.status)
			}
			if test.code == "" {
				if outcome.failure != nil {
					t.Fatalf("semantic outcome leaked failure: %+v", outcome.failure)
				}
				return
			}
			if outcome.failure == nil || outcome.failure.Code != test.code || outcome.failure.Stage != test.stage {
				t.Fatalf("failure = %+v, want %s/%s", outcome.failure, test.code, test.stage)
			}
		})
	}
}

func requireNasdaqTypedOutcome(t testing.TB, body []byte, wantStatus, wantCode, wantStage string) {
	t.Helper()
	requireNasdaqTypedOutcomeFor(t, body, "TESTQ", wantStatus, wantCode, wantStage)
}

func requireNasdaqTypedOutcomeFor(t testing.TB, body []byte, providerSymbol, wantStatus, wantCode, wantStage string) {
	t.Helper()
	entry, err := parseNasdaqEarnings(body, providerSymbol, time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))
	if err == nil || entry != (earningsEntry{}) {
		t.Fatal("unresolved Nasdaq payload produced a usable entry")
	}
	var outcome *earningsProviderError
	if !errors.As(err, &outcome) {
		t.Fatalf("error type = %T, want *earningsProviderError", err)
	}
	if outcome.status != wantStatus {
		t.Fatalf("status = %q, want %q", outcome.status, wantStatus)
	}
	if wantCode == "" {
		if outcome.failure != nil {
			t.Fatalf("semantic outcome leaked failure: %+v", outcome.failure)
		}
		return
	}
	if outcome.failure == nil || outcome.failure.Code != wantCode || outcome.failure.Stage != wantStage {
		t.Fatalf("failure = %+v, want %s/%s", outcome.failure, wantCode, wantStage)
	}
}

func TestParseNasdaqEarningsRejectsAuthorityKeyAliases(t *testing.T) {
	announcement := nasdaqAnnouncementPrefix("TESTQ") + " Jul 30, 2026"
	validData := fmt.Sprintf(`{"announcement":%q,"status":{"rCode":200}}`, announcement)
	tests := []struct {
		name string
		body string
	}{
		{"top-level data case alias", fmt.Sprintf(`{"data":{%s},"Data":{%s}}`, validData[1:len(validData)-1], validData[1:len(validData)-1])},
		{"top-level status case alias", fmt.Sprintf(`{"data":%s,"Status":{"rCode":200}}`, validData)},
		{"announcement case alias", fmt.Sprintf(`{"data":{"announcement":%q,"Announcement":%q,"status":{"rCode":200}}}`, announcement, announcement)},
		{"nested status case alias", fmt.Sprintf(`{"data":{"announcement":%q,"status":{"rCode":200},"Status":{"rCode":200}}}`, announcement)},
		{"status code case alias", fmt.Sprintf(`{"data":{"announcement":%q,"status":{"rCode":200,"RCode":200}}}`, announcement)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireNasdaqTypedOutcome(t, []byte(test.body), rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema)
		})
	}
}

func TestParseNasdaqEarningsRejectsDuplicateAuthorityKeys(t *testing.T) {
	announcement := nasdaqAnnouncementPrefix("TESTQ") + " Jul 30, 2026"
	data := fmt.Sprintf(`{"announcement":%q,"status":{"rCode":200}}`, announcement)
	tests := []struct {
		name string
		body string
	}{
		{"top-level data", fmt.Sprintf(`{"data":%s,"data":%s}`, data, data)},
		{"top-level status", `{"data":null,"status":{"rCode":400},"status":{"rCode":400}}`},
		{"nested announcement", fmt.Sprintf(`{"data":{"announcement":%q,"announcement":%q,"status":{"rCode":200}}}`, announcement, announcement)},
		{"nested status", fmt.Sprintf(`{"data":{"announcement":%q,"status":{"rCode":200},"status":{"rCode":200}}}`, announcement)},
		{"status code", fmt.Sprintf(`{"data":{"announcement":%q,"status":{"rCode":200,"rCode":200}}}`, announcement)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireNasdaqTypedOutcome(t, []byte(test.body), rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema)
		})
	}
}

// The observed envelope carries the announcement under a status-free data
// object and one top-level status. Scheduled dates spell the day zero-padded
// and Zacks estimates do not, so both spellings must resolve.
func TestParseNasdaqEarningsObservedEnvelopeResolves(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	providerSymbol := "TESTQ"
	prefix := nasdaqAnnouncementPrefix(providerSymbol)
	for _, test := range []struct {
		name         string
		announcement string
		date         string
	}{
		{"unpadded day", prefix + " Aug 3, 2026", "2026-08-03"},
		{"zero-padded day", prefix + " Aug 03, 2026", "2026-08-03"},
		{"two-digit day", prefix + " Sep 24, 2026", "2026-09-24"},
		{"today is not elapsed", prefix + " Jul 21, 2026", "2026-07-21"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := nasdaqTestPayload(t, map[string]any{
				"announcement": test.announcement,
				"reportText":   "untrusted provider prose",
			}, http.StatusOK)
			entry, err := parseNasdaqEarnings(body, providerSymbol, now)
			if err != nil {
				t.Fatalf("parse observed envelope: %v", err)
			}
			if entry.Date != test.date || !entry.ObservedAt.Equal(now) || entry.TimeOfDay != "" || entry.Estimated {
				t.Fatalf("typed entry = %+v, want date %s", entry, test.date)
			}
		})
	}
}

// Nasdaq answers a dotted class-share request but echoes some issuers with a
// slash. It is per-issuer, not a rule about dotted symbols, so both spellings
// have to resolve while the announcement stays bound to the exact security
// requested.
func TestParseNasdaqEarningsAcceptsSlashEchoOnlyWhereRequestHadADot(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	lead := nasdaqAnnouncementLead
	for _, test := range []struct {
		name         string
		providerSym  string
		announcement string
		date         string
	}{
		{"slash echo with date", "BRK.B", lead + "BRK/B: Jul 30, 2026", "2026-07-30"},
		{"dot echo with date", "BF.B", lead + "BF.B: Jul 30, 2026", "2026-07-30"},
		{"multiple dots echoed as slashes", "A.B.C", lead + "A/B/C: Jul 30, 2026", "2026-07-30"},
		{"only the second dot echoed", "A.B.C", lead + "A.B/C: Jul 30, 2026", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := nasdaqTestPayload(t, map[string]any{"announcement": test.announcement}, http.StatusOK)
			entry, err := parseNasdaqEarnings(body, test.providerSym, now)
			if test.date == "" {
				if err == nil {
					t.Fatalf("mixed separator spelling resolved to %+v", entry)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if entry.Date != test.date {
				t.Fatalf("date = %q, want %q", entry.Date, test.date)
			}
		})
	}

	t.Run("slash echo with no date", func(t *testing.T) {
		body := nasdaqTestPayload(t, map[string]any{"announcement": lead + "BRK/B: "}, http.StatusOK)
		requireNasdaqTypedOutcomeFor(t, body, "BRK.B", rpc.EarningsStatusNoDatePublished, "", "")
	})

	// The prefix is what binds a payload to the security requested. Widening it
	// must not let a different one through.
	for _, test := range []struct{ name, providerSym, announcement string }{
		{"different class", "BRK.B", lead + "BRK/A: Jul 30, 2026"},
		{"separator dropped", "BRK.B", lead + "BRKB: Jul 30, 2026"},
		{"separator added where none was requested", "BRKB", lead + "BRK/B: Jul 30, 2026"},
		{"slash echoed for an undotted request", "AAPL", lead + "AAP/L: Jul 30, 2026"},
		{"unrelated symbol", "BRK.B", lead + "OTHERQ: Jul 30, 2026"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := nasdaqTestPayload(t, map[string]any{"announcement": test.announcement}, http.StatusOK)
			requireNasdaqTypedOutcomeFor(t, body, test.providerSym,
				rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema)
		})
	}
}

func TestParseNasdaqEarningsToleratesUnrelatedProseFields(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	announcement := nasdaqAnnouncementPrefix("TESTQ") + " Jul 30, 2026"
	body := fmt.Appendf(nil, `{"notice":"untrusted top-level prose","data":{"announcement":%q,"reportText":"untrusted data prose"},"status":{"rCode":200,"message":"untrusted status prose"}}`, announcement)
	entry, err := parseNasdaqEarnings(body, "TESTQ", now)
	if err != nil {
		t.Fatalf("parse payload with unrelated prose: %v", err)
	}
	if entry.Date != "2026-07-30" || !entry.ObservedAt.Equal(now) || entry.TimeOfDay != "" || entry.Estimated {
		t.Fatalf("typed entry = %+v", entry)
	}

	requireNasdaqTypedOutcome(t,
		[]byte(`{"notice":"untrusted top-level prose","data":null,"status":{"rCode":400,"bCodeMessage":"untrusted status prose"}}`),
		rpc.EarningsStatusUnsupportedSecurity, "", "")

	requireNasdaqTypedOutcome(t,
		[]byte(`{"notice":"untrusted top-level prose","data":{"announcement":"Earnings announcement* for TESTQ: ","reportText":"untrusted data prose"},"status":{"rCode":200,"message":"untrusted status prose"}}`),
		rpc.EarningsStatusNoDatePublished, "", "")
}

func TestParseNasdaqEarningsUnsupportedRequiresExplicitNullDataAndExactBadRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing data", `{"status":{"rCode":400}}`},
		{"not found", `{"data":null,"status":{"rCode":404}}`},
		{"ok", `{"data":null,"status":{"rCode":200}}`},
		{"server error", `{"data":null,"status":{"rCode":500}}`},
		{"string bad request", `{"data":null,"status":{"rCode":"400"}}`},
		{"fractional bad request", `{"data":null,"status":{"rCode":400.0}}`},
		{"nested bad request", `{"data":{"announcement":null,"status":{"rCode":400}}}`},
		{"no-date wrong root code", `{"data":{"announcement":"Earnings announcement* for TESTQ: "},"status":{"rCode":400}}`},
		{"no-date string root code", `{"data":{"announcement":"Earnings announcement* for TESTQ: "},"status":{"rCode":"200"}}`},
		{"no-date root plus nested status", `{"data":{"announcement":"Earnings announcement* for TESTQ: ","status":{"rCode":200}},"status":{"rCode":200}}`},
		{"no-date exact prefix without space", `{"data":{"announcement":"Earnings announcement* for TESTQ:"},"status":{"rCode":200}}`},
		{"no-date two trailing spaces", `{"data":{"announcement":"Earnings announcement* for TESTQ:  "},"status":{"rCode":200}}`},
		{"no-date wrong symbol", `{"data":{"announcement":"Earnings announcement* for OTHERQ: "},"status":{"rCode":200}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireNasdaqTypedOutcome(t, []byte(test.body), rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema)
		})
	}
}

func TestFetchNasdaqEarningsBareClientErrorsAreProtocolFailures(t *testing.T) {
	for _, statusCode := range []int{http.StatusBadRequest, http.StatusNotFound} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			cache := newEarningsCacheMemory(nil)
			cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: statusCode, Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			result, err := cache.fetchNasdaqProvider(t.Context(), "TESTQ")
			if err == nil {
				t.Fatal("bare HTTP client error returned nil error")
			}
			if result.Status != rpc.EarningsStatusTransportFailure || result.Entry != (earningsEntry{}) {
				t.Fatalf("fetch result = %+v", result)
			}
			if result.Failure == nil || result.Failure.Code != rpc.SourceFailureProtocolRejected ||
				result.Failure.Stage != rpc.SourceFailureStageNasdaqRequest || result.Failure.Retryable {
				t.Fatalf("failure = %+v", result.Failure)
			}
		})
	}
}

func TestEarningsNasdaqUserAgentUsesCanonicalProductIdentity(t *testing.T) {
	t.Parallel()
	if !strings.HasSuffix(earningsNasdaqUserAgent, " canary-earnings/1.0") {
		t.Fatalf("Nasdaq User-Agent = %q", earningsNasdaqUserAgent)
	}
}

func TestValidateEarningsNasdaqParserProvenance(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	formatChange := normalizeEarningsAttempt(earningsNasdaqProvider, "TESTQ", earningsProviderFetchResult{
		Status: rpc.EarningsStatusFormatChange,
		Failure: &rpc.SourceFailure{
			Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageNasdaqSchema,
		},
	}, now, now)
	noDate := normalizeEarningsAttempt(earningsNasdaqProvider, "TESTQ", earningsProviderFetchResult{
		Status: rpc.EarningsStatusNoDatePublished,
	}, now, now)
	tests := []struct {
		name     string
		provider string
		attempt  earningsProviderAttempt
		version  int
		valid    bool
	}{
		{"negative format change", earningsNasdaqProvider, formatChange, -1, false},
		{"v0 format change", earningsNasdaqProvider, formatChange, 0, false},
		{"v1 format change", earningsNasdaqProvider, formatChange, earningsNasdaqParserContractLegacy, true},
		{"first exact format change", earningsNasdaqProvider, formatChange, earningsNasdaqParserContractFirstExact, true},
		{"retired exact format change", earningsNasdaqProvider, formatChange, earningsNasdaqParserContract - 1, true},
		{"current format change", earningsNasdaqProvider, formatChange, earningsNasdaqParserContract, true},
		{"future format change", earningsNasdaqProvider, formatChange, earningsNasdaqParserContract + 1, false},
		{"v0 legacy non-format", earningsNasdaqProvider, noDate, 0, true},
		{"v1 non-format", earningsNasdaqProvider, noDate, earningsNasdaqParserContractLegacy, false},
		{"first exact no-date", earningsNasdaqProvider, noDate, earningsNasdaqParserContractFirstExact, true},
		{"retired exact no-date", earningsNasdaqProvider, noDate, earningsNasdaqParserContract - 1, true},
		{"current non-format", earningsNasdaqProvider, noDate, earningsNasdaqParserContract, true},
		{"WSH legacy", earningsWSHProvider, noDate, earningsNasdaqParserContractLegacy, false},
		{"WSH current", earningsWSHProvider, noDate, earningsNasdaqParserContract, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.attempt.ParserContractVersion = test.version
			err := validateEarningsProviderState("TESTQ", test.provider, earningsProviderState{LastAttempt: test.attempt}, now)
			if test.valid && err != nil {
				t.Fatalf("valid parser provenance rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid parser provenance accepted")
			}
		})
	}
}

func TestEarningsNasdaqParserContractDueRules(t *testing.T) {
	completed := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	formatChange := normalizeEarningsAttempt(earningsNasdaqProvider, "TESTQ", earningsProviderFetchResult{
		Status:  rpc.EarningsStatusFormatChange,
		Failure: &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageNasdaqSchema},
	}, completed, completed)
	noDate := normalizeEarningsAttempt(earningsNasdaqProvider, "TESTQ", earningsProviderFetchResult{
		Status: rpc.EarningsStatusNoDatePublished,
	}, completed, completed)
	entry := earningsEntry{Date: "2026-07-30", ObservedAt: completed}
	date := normalizeEarningsAttempt(earningsNasdaqProvider, "TESTQ", earningsProviderFetchResult{
		Status: rpc.EarningsStatusDate, Entry: entry,
	}, completed, completed)
	unsupported := normalizeEarningsAttempt(earningsNasdaqProvider, "TESTQ", earningsProviderFetchResult{
		Status: rpc.EarningsStatusUnsupportedSecurity,
	}, completed, completed)
	for _, test := range []struct {
		name    string
		attempt earningsProviderAttempt
		version int
		wantDue bool
	}{
		{"previous format change", formatChange, earningsNasdaqParserContractPrevious, true},
		{"previous no-date", noDate, earningsNasdaqParserContractPrevious, true},
		{"previous date", date, earningsNasdaqParserContractPrevious, false},
		{"previous unsupported", unsupported, earningsNasdaqParserContractPrevious, false},
		{"retired format change", formatChange, earningsNasdaqParserContract - 1, true},
		{"retired no-date", noDate, earningsNasdaqParserContract - 1, false},
		{"current format change", formatChange, earningsNasdaqParserContract, false},
		{"current no-date", noDate, earningsNasdaqParserContract, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.attempt.ParserContractVersion = test.version
			if test.attempt.NextAttempt == nil || !test.attempt.NextAttempt.After(completed) {
				t.Fatal("fixture has no stored future retry deadline")
			}
			got := earningsProviderDue(earningsNasdaqProvider, earningsProviderState{LastAttempt: test.attempt}, completed)
			if got != test.wantDue {
				t.Fatalf("due = %v, want %v", got, test.wantDue)
			}
		})
	}
}

func TestParseNasdaqEarningsExactDateIgnoresUntypedText(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	providerSymbol := "TESTQ"
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{
			"announcement": nasdaqAnnouncementPrefix(providerSymbol) + " Jul 30, 2026",
			"reportText":   "untrusted auxiliary text",
		},
		"status": map[string]any{
			"rCode":   http.StatusOK,
			"message": "untrusted status prose",
		},
	})
	if err != nil {
		t.Fatal("marshal typed Nasdaq fixture")
	}

	entry, err := parseNasdaqEarnings(body, providerSymbol, now)
	if err != nil {
		t.Fatalf("parse synthetic payload: %v", err)
	}
	if entry.Date != "2026-07-30" || !entry.ObservedAt.Equal(now) {
		t.Fatalf("typed entry date/time mismatch")
	}
	if entry.TimeOfDay != "" || entry.Estimated {
		t.Fatal("untyped provider text influenced the typed entry")
	}
}

func TestParseNasdaqEarningsErrorsDoNotEchoProviderText(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	const sentinel = "synthetic-private-response-marker"
	body := nasdaqTestPayload(t, map[string]any{"announcement": sentinel}, http.StatusOK)

	_, err := parseNasdaqEarnings(body, "TESTQ", now)
	if err == nil {
		t.Fatal("unrecognized provider text returned nil error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatal("provider error echoed untrusted response text")
	}
}

func TestParseNasdaqEarningsRejectsNonCanonicalProviderSymbol(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		symbol string
	}{
		{"empty", ""},
		{"lowercase", "testq"},
		{"broker space not normalized", "TEST Q"},
		{"leading punctuation", ".TESTQ"},
		{"trailing punctuation", "TESTQ-"},
		{"repeated punctuation", "TEST.-Q"},
		{"colon", "TEST:Q"},
		{"quote", `TEST"Q`},
		{"wildcard", "TEST*Q"},
		{"control", "TEST\tQ"},
		{"non ascii", "TESTÄ"},
		{"too long", strings.Repeat("A", nasdaqProviderSymbolMaxLen+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := nasdaqTestPayload(t, map[string]any{
				"announcement": nasdaqAnnouncementPrefix(test.symbol) + " Jul 30, 2026",
			}, http.StatusOK)
			entry, err := parseNasdaqEarnings(body, test.symbol, now)
			if err == nil || entry != (earningsEntry{}) {
				t.Fatal("non-canonical provider symbol produced a usable entry")
			}
			var outcome *earningsProviderError
			if !errors.As(err, &outcome) || outcome.status != rpc.EarningsStatusFormatChange {
				t.Fatal("non-canonical provider symbol did not fail closed")
			}
			if outcome.failure == nil || outcome.failure.Code != rpc.SourceFailureInvalidPayload || outcome.failure.Stage != rpc.SourceFailureStageNasdaqSchema {
				t.Fatal("non-canonical provider symbol did not retain typed failure")
			}
		})
	}
}

func TestResolveEarningsProvidersAgreementAndLastGood(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	entry := func(date, session string) earningsEntry {
		return earningsEntry{Date: date, TimeOfDay: session, ObservedAt: now}
	}
	state := func(status string, current, lastGood *earningsEntry) earningsProviderState {
		next := now.Add(earningsFreshWindow)
		return earningsProviderState{
			LastAttempt: earningsProviderAttempt{
				Status: status, Entry: cloneEarningsEntry(current), AttemptedAt: now,
				CompletedAt: now, NextAttempt: &next,
			},
			LastGood: cloneEarningsEntry(lastGood),
		}
	}
	aapl := entry("2026-07-30", "")
	aaplAMC := entry("2026-07-30", "amc")
	aaplBMO := entry("2026-07-30", "bmo")
	different := entry("2026-07-31", "amc")

	tests := []struct {
		name      string
		providers map[string]earningsProviderState
		status    string
		reason    string
		date      string
		stale     bool
	}{
		{
			name: "matching dates compatible sessions",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusDate, &aapl, &aapl),
				earningsWSHProvider:    state(rpc.EarningsStatusDate, &aaplAMC, &aaplAMC),
			}, status: rpc.EarningsStatusDate, reason: earningsReasonConsensus, date: aapl.Date,
		},
		{
			name: "different dates conflict",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusDate, &aaplAMC, &aaplAMC),
				earningsWSHProvider:    state(rpc.EarningsStatusDate, &different, &different),
			}, status: rpc.EarningsStatusConflictingSources, reason: earningsReasonConflicting,
		},
		{
			name: "explicit sessions conflict",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusDate, &aaplAMC, &aaplAMC),
				earningsWSHProvider:    state(rpc.EarningsStatusDate, &aaplBMO, &aaplBMO),
			}, status: rpc.EarningsStatusConflictingSources, reason: earningsReasonConflicting,
		},
		{
			name: "one date plus explicit no date",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusDate, &aaplAMC, &aaplAMC),
				earningsWSHProvider:    state(rpc.EarningsStatusNoDatePublished, nil, nil),
			}, status: rpc.EarningsStatusDate, reason: earningsReasonSingleSource, date: aapl.Date,
		},
		{
			name: "transport retains last good",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusTransportFailure, nil, &aaplAMC),
			}, status: rpc.EarningsStatusDate, reason: earningsReasonRetainedLastGood, date: aapl.Date, stale: true,
		},
		{
			name: "no date does not hide behind last good",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusNoDatePublished, nil, &aaplAMC),
			}, status: rpc.EarningsStatusNoDatePublished, reason: rpc.EarningsStatusNoDatePublished,
		},
		{
			name: "format change does not hide behind last good",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusFormatChange, nil, &aaplAMC),
			}, status: rpc.EarningsStatusFormatChange, reason: rpc.EarningsStatusFormatChange,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := resolveEarningsProviders(test.providers, now)
			if got.Status != test.status || got.Reason != test.reason || got.Stale != test.stale {
				t.Fatalf("resolution = %+v, want status=%s reason=%s stale=%v", got, test.status, test.reason, test.stale)
			}
			if test.date == "" {
				if got.Entry != nil {
					t.Fatalf("unknown/conflict exposed date %+v", got.Entry)
				}
			} else if got.Entry == nil || got.Entry.Date != test.date {
				t.Fatalf("date = %+v, want %s", got.Entry, test.date)
			}
		})
	}
}

func TestEarningsProviderOutcomesPersistAndRecoverWithoutRawError(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := openMarketTestCoreStore(t)
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body := []byte(`{"data":{"announcement":"Earnings announcement* for AAPL: "},"status":{"rCode":200}}`)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	if err := cache.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
		return transportFailureResult(rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false, now), errors.New("SECRET provider prose")
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	cache.refreshOne(context.Background(), "AAPL")

	view, ok := cache.resolution("AAPL")
	if !ok || view.Status != rpc.EarningsStatusNoDatePublished || len(view.Providers) != 2 {
		t.Fatalf("resolution = %+v ok=%v", view, ok)
	}
	doc, ok, err := store.GetStateDocument(context.Background(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok {
		t.Fatalf("state read: ok=%v err=%v", ok, err)
	}
	if bytesContain(doc.JSON, "SECRET") {
		t.Fatal("raw provider error entered state authority")
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(doc.JSON, &header); err != nil || header.Version != earningsPersistVersion {
		t.Fatalf("state version: header=%+v err=%v", header, err)
	}
	observations, err := store.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsProviderObservationKind,
	})
	if err != nil || len(observations) != 2 {
		t.Fatalf("provider observations=%d err=%v", len(observations), err)
	}
	for _, observation := range observations {
		if bytesContain(observation.Payload, "SECRET") {
			t.Fatal("raw provider error entered immutable observation")
		}
	}

	restarted := newEarningsCacheMemory(nil)
	restarted.clock = func() time.Time { return now.Add(time.Minute) }
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatalf("restart attach: %v", err)
	}
	recovered, ok := restarted.resolution("AAPL")
	if !ok || recovered.Status != rpc.EarningsStatusNoDatePublished || len(recovered.Providers) != 2 {
		t.Fatalf("recovered = %+v ok=%v", recovered, ok)
	}
	for _, provider := range recovered.Providers {
		if provider.NextAttempt == nil || !provider.NextAttempt.After(now) {
			t.Fatalf("provider retry state not recovered: %+v", provider)
		}
	}
}

func TestEarningsProviderBackoffPersistsAcrossRestart(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		result   earningsProviderFetchResult
		expected time.Duration
	}{
		{
			name:     "provider-confirmed unsupported security",
			result:   earningsProviderFetchResult{Status: rpc.EarningsStatusUnsupportedSecurity},
			expected: earningsTTL,
		},
		{
			name: "provider format",
			result: earningsProviderFetchResult{Status: rpc.EarningsStatusFormatChange, Failure: &rpc.SourceFailure{
				Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageWSHDecode, Retryable: false,
			}},
			expected: earningsNonRetryableFailureRetry,
		},
		{
			name:     "non-retryable provider failure",
			result:   transportFailureResult(rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false, base),
			expected: earningsNonRetryableFailureRetry,
		},
		{
			name:     "temporary connector inactive",
			result:   transportFailureResult(rpc.SourceFailureContractUnavailable, rpc.SourceFailureStageWSHContractResolve, true, base),
			expected: earningsContractResolutionRetry,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := openMarketTestCoreStore(t)
			var providerLogs []string
			initial := newEarningsCacheMemory(func(format string, args ...any) {
				providerLogs = append(providerLogs, fmt.Sprintf(format, args...))
			})
			initial.clock = func() time.Time { return base }
			initial.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
				body := nasdaqTestPayload(t, map[string]any{"announcement": nasdaqAnnouncementPrefix("TESTQ") + " "}, http.StatusOK)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}
			providerCalls := 0
			if err := initial.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
				providerCalls++
				return tc.result, errors.New("typed provider failure")
			}); err != nil {
				t.Fatal(err)
			}
			if err := initial.UseCoreStore(store); err != nil {
				t.Fatal(err)
			}
			initial.refreshOne(context.Background(), "TESTQ")
			if providerCalls != 1 {
				t.Fatalf("initial provider calls = %d, want 1", providerCalls)
			}
			for _, line := range providerLogs {
				if strings.Contains(line, "TESTQ") {
					t.Fatalf("provider log exposed the requested name: %q", line)
				}
			}

			view, ok := initial.resolution("TESTQ")
			if !ok {
				t.Fatal("missing committed provider outcome")
			}
			var nextAttempt *time.Time
			for _, provider := range view.Providers {
				if provider.Provider == earningsWSHProvider {
					nextAttempt = provider.NextAttempt
				}
			}
			wantNext := base.Add(tc.expected)
			if nextAttempt == nil || !nextAttempt.Equal(wantNext) {
				t.Fatalf("persisted next attempt = %v, want %v", nextAttempt, wantNext)
			}

			restarted := newEarningsCacheMemory(nil)
			restartNow := wantNext.Add(-time.Minute)
			restarted.clock = func() time.Time { return restartNow }
			restarted.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
				body := nasdaqTestPayload(t, map[string]any{"announcement": nasdaqAnnouncementPrefix("TESTQ") + " "}, http.StatusOK)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}
			providerCalls = 0
			if err := restarted.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
				providerCalls++
				return earningsProviderFetchResult{}, errors.New("unexpected early request")
			}); err != nil {
				t.Fatal(err)
			}
			if err := restarted.UseCoreStore(store); err != nil {
				t.Fatalf("restart attach: %v", err)
			}
			recovered, ok := restarted.resolution("TESTQ")
			if !ok {
				t.Fatal("restart lost the failed provider outcome")
			}
			recoveredStatus := ""
			for _, provider := range recovered.Providers {
				if provider.Provider == earningsWSHProvider {
					recoveredStatus = provider.Status
				}
			}
			if recoveredStatus != tc.result.Status {
				t.Fatalf("restarted provider status = %q, want visible failure %q", recoveredStatus, tc.result.Status)
			}
			restarted.refreshOne(context.Background(), "TESTQ")
			if providerCalls != 0 {
				t.Fatalf("provider calls before persisted retry = %d, want 0", providerCalls)
			}

			restartNow = wantNext
			restarted.refreshOne(context.Background(), "TESTQ")
			if providerCalls != 1 {
				t.Fatalf("provider calls at persisted retry = %d, want 1", providerCalls)
			}
		})
	}
}

func TestEarningsConflictPersistsAcrossRestart(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dir, "daemon.db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body := nasdaqTestPayload(t, map[string]any{"announcement": nasdaqAnnouncementPrefix("TESTQ") + " Jul 30, 2026"}, http.StatusOK)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	if err := cache.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
		return earningsProviderFetchResult{Status: rpc.EarningsStatusDate, Entry: earningsEntry{Date: "2026-07-31", ObservedAt: now}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	cache.refreshOne(context.Background(), "TESTQ")
	if _, _, ok := cache.get("TESTQ"); ok {
		t.Fatal("conflicting providers exposed a usable earnings date")
	}
	view, ok := cache.resolution("TESTQ")
	if !ok || view.Status != rpc.EarningsStatusConflictingSources {
		t.Fatalf("conflict = %+v ok=%v", view, ok)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = corestore.Open(context.Background(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatalf("reopen authority: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close reopened authority: %v", err)
		}
	}()

	restarted := newEarningsCacheMemory(nil)
	restarted.clock = func() time.Time { return now.Add(time.Minute) }
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	view, ok = restarted.resolution("TESTQ")
	if !ok || view.Status != rpc.EarningsStatusConflictingSources {
		t.Fatalf("restarted conflict = %+v ok=%v", view, ok)
	}
}

func TestEarningsFailedAuthorityCommitDoesNotPublishMemory(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := openMarketTestCoreStore(t)
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body := nasdaqTestPayload(t, map[string]any{"announcement": nasdaqAnnouncementPrefix("TESTQ") + " Jul 30, 2026"}, http.StatusOK)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cache.refreshOne(context.Background(), "TESTQ")
	if _, ok := cache.resolution("TESTQ"); ok {
		t.Fatal("failed SQLite commit published provider result into memory")
	}
}

func TestEarningsV1AuthorityMigratesInPlace(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := openMarketTestCoreStore(t)
	legacy := earningsPersistEnvelopeV1{Version: earningsLegacyVersion, Entries: map[string]earningsEntry{
		"AAPL": {Date: "2026-07-30", TimeOfDay: "amc", ObservedAt: now},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now.Add(time.Minute) }
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := store.GetStateDocument(context.Background(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok || doc.Revision != created.Revision+1 {
		t.Fatalf("migrated doc revision=%d ok=%v err=%v", doc.Revision, ok, err)
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(doc.JSON, &header); err != nil || header.Version != earningsPersistVersion {
		t.Fatalf("migrated header=%+v err=%v", header, err)
	}
	entry, _, ok := cache.get("AAPL")
	if !ok || entry.Date != "2026-07-30" {
		t.Fatalf("migrated entry=%+v ok=%v", entry, ok)
	}
	observations, err := store.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsProviderObservationKind,
	})
	if err != nil || len(observations) != 0 {
		t.Fatalf("migration invented observations: count=%d err=%v", len(observations), err)
	}
}

func TestEarningsNasdaqParserContractUpgradeRetriesOnlyOldFormatChange(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	now := base.Add(time.Hour)
	legacyNext := base.Add(earningsNonRetryableFailureRetry)
	legacyFailure := &rpc.SourceFailure{
		Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageNasdaqSchema,
		FailedAt: base, Retryable: false,
	}
	legacyProviders := map[string]earningsProviderStateLegacy{earningsNasdaqProvider: {
		LastAttempt: earningsProviderAttemptLegacy{
			Status: rpc.EarningsStatusFormatChange, AttemptedAt: base, CompletedAt: base,
			NextAttempt: &legacyNext, LastFailure: legacyFailure,
		},
	}}
	currentProviders := migrateEarningsProviderStates(legacyProviders)
	legacy := earningsPersistEnvelopeV3{Version: earningsIdentityPersistVersion, Symbols: map[string]earningsSymbolStateV3{
		"TESTQ": {
			Resolution: resolveEarningsProviders(currentProviders, base), Providers: legacyProviders, UpdatedAt: base,
		},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal("marshal legacy parser contract fixture")
	}
	var smuggled map[string]any
	if err := json.Unmarshal(raw, &smuggled); err != nil {
		t.Fatal("decode strict migration fixture")
	}
	smuggled["symbols"].(map[string]any)["TESTQ"].(map[string]any)["providers"].(map[string]any)[earningsNasdaqProvider].(map[string]any)["last_attempt"].(map[string]any)["parser_contract_version"] = earningsNasdaqParserContract
	smuggledRaw, err := json.Marshal(smuggled)
	if err != nil {
		t.Fatal("encode strict migration fixture")
	}
	if _, err := decodeEarningsEnvelopeV3(smuggledRaw, now); err == nil {
		t.Fatal("v3 authority accepted a v4 parser-contract field")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal("secure parser contract fixture directory")
	}
	databasePath := filepath.Join(dir, "daemon.db")
	store, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal("open parser contract fixture authority")
	}
	created, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw,
	})
	if err != nil {
		t.Fatal("seed legacy parser contract authority")
	}

	const providerProse = "synthetic-untrusted-parser-contract-prose"
	providerCalls := 0
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		body := nasdaqTestPayload(t, map[string]any{"announcement": providerProse}, http.StatusOK)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal("migrate legacy parser contract authority")
	}
	migrated, ok := cache.symbols["TESTQ"].Providers[earningsNasdaqProvider]
	if !ok || migrated.LastAttempt.ParserContractVersion != earningsNasdaqParserContractLegacy ||
		!earningsProviderDue(earningsNasdaqProvider, migrated, now) {
		t.Fatal("older Nasdaq format-change parser contract was not immediately due")
	}
	cache.refreshOne(t.Context(), "TESTQ")
	if providerCalls != 1 {
		t.Fatal("older Nasdaq parser contract did not trigger exactly one refresh")
	}
	doc, ok, err := store.GetStateDocument(t.Context(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok || doc.Revision != created.Revision+2 || strings.Contains(string(doc.JSON), providerProse) {
		t.Fatal("current parser outcome was not safely committed after migration")
	}
	var persisted earningsPersistEnvelope
	if err := json.Unmarshal(doc.JSON, &persisted); err != nil || persisted.Version != earningsPersistVersion {
		t.Fatal("decode current parser contract authority")
	}
	current := persisted.Symbols["TESTQ"].Providers[earningsNasdaqProvider]
	wantNext := now.Add(earningsNonRetryableFailureRetry)
	if current.LastAttempt.Status != rpc.EarningsStatusFormatChange ||
		current.LastAttempt.ParserContractVersion != earningsNasdaqParserContract ||
		current.LastAttempt.NextAttempt == nil || !current.LastAttempt.NextAttempt.Equal(wantNext) {
		t.Fatal("current parser contract and retry deadline were not persisted together")
	}
	if err := store.Close(); err != nil {
		t.Fatal("close parser contract authority")
	}

	store, err = corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal("reopen parser contract authority")
	}
	defer func() { _ = store.Close() }()
	restartNow := now.Add(time.Minute)
	restarted := newEarningsCacheMemory(nil)
	restarted.clock = func() time.Time { return restartNow }
	restarted.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected same-contract retry")
	})}
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal("load current parser contract after reopen")
	}
	restarted.refreshOne(t.Context(), "TESTQ")
	if providerCalls != 1 {
		t.Fatal("same parser contract retried before its persisted deadline")
	}
	recovered := restarted.symbols["TESTQ"].Providers[earningsNasdaqProvider].LastAttempt
	if recovered.ParserContractVersion != earningsNasdaqParserContract ||
		recovered.NextAttempt == nil || !recovered.NextAttempt.Equal(wantNext) {
		t.Fatal("same parser contract retry deadline did not survive close and reopen")
	}
}

func TestEarningsNasdaqParserContractV2NoDateRefreshesAndPersistsV3Deadline(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	loadNow := base.Add(time.Hour)
	storedNext := base.Add(earningsFreshWindow)
	previous := earningsProviderAttempt{
		Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: base, CompletedAt: base,
		NextAttempt: &storedNext, ParserContractVersion: earningsNasdaqParserContractPrevious,
	}
	providers := map[string]earningsProviderState{earningsNasdaqProvider: {LastAttempt: previous}}
	seed := earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: map[string]earningsSymbolState{
		"TESTQ": {Resolution: resolveEarningsState(providers, nil, base), Providers: providers, UpdatedAt: base},
	}}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal("marshal v2 no-date authority")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal("secure v2 no-date authority directory")
	}
	databasePath := filepath.Join(dir, "daemon.db")
	store, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal("open v2 no-date authority")
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw,
	}); err != nil {
		t.Fatal("seed v2 no-date authority")
	}

	providerCalls := 0
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return loadNow }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":{"announcement":"Earnings announcement* for TESTQ: "},"status":{"rCode":200}}`))}, nil
	})}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal("load v2 no-date authority")
	}
	loaded := cache.symbols["TESTQ"].Providers[earningsNasdaqProvider].LastAttempt
	if loaded.ParserContractVersion != earningsNasdaqParserContractPrevious ||
		!earningsProviderDue(earningsNasdaqProvider, earningsProviderState{LastAttempt: loaded}, loadNow) {
		t.Fatal("v2 no-date authority was not immediately due after load")
	}
	cache.refreshOne(t.Context(), "TESTQ")
	if providerCalls != 1 {
		t.Fatalf("provider calls = %d, want 1", providerCalls)
	}
	wantNext := loadNow.Add(earningsFreshWindow)
	doc, ok, err := store.GetStateDocument(t.Context(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok {
		t.Fatalf("read v3 no-date authority: ok=%v err=%v", ok, err)
	}
	var persisted earningsPersistEnvelope
	if err := json.Unmarshal(doc.JSON, &persisted); err != nil {
		t.Fatal("decode v3 no-date authority")
	}
	current := persisted.Symbols["TESTQ"].Providers[earningsNasdaqProvider].LastAttempt
	if current.Status != rpc.EarningsStatusNoDatePublished || current.ParserContractVersion != earningsNasdaqParserContract ||
		current.NextAttempt == nil || !current.NextAttempt.Equal(wantNext) {
		t.Fatalf("persisted v3 no-date attempt = %+v", current)
	}
	if err := store.Close(); err != nil {
		t.Fatal("close v3 no-date authority")
	}

	store, err = corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal("reopen v3 no-date authority")
	}
	defer func() { _ = store.Close() }()
	restartNow := loadNow.Add(time.Minute)
	restarted := newEarningsCacheMemory(nil)
	restarted.clock = func() time.Time { return restartNow }
	restarted.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected v3 no-date retry")
	})}
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal("reload v3 no-date authority")
	}
	restarted.refreshOne(t.Context(), "TESTQ")
	if providerCalls != 1 {
		t.Fatal("v3 no-date retried before its persisted deadline")
	}
	recovered := restarted.symbols["TESTQ"].Providers[earningsNasdaqProvider].LastAttempt
	if recovered.ParserContractVersion != earningsNasdaqParserContract ||
		recovered.NextAttempt == nil || !recovered.NextAttempt.Equal(wantNext) {
		t.Fatal("v3 no-date retry deadline did not survive restart")
	}
}

func TestDecodeEarningsV4UpgradesOnlyLegacyWSHNotEntitledAggregate(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	next := now.Add(earningsFreshWindow)
	providers := map[string]earningsProviderState{
		earningsNasdaqProvider: {LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, NextAttempt: &next, ParserContractVersion: earningsNasdaqParserContract}},
		earningsWSHProvider:    {LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusTransportFailure, AttemptedAt: now, CompletedAt: now, NextAttempt: &next, LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageWSHMetadata, Retryable: false, FailedAt: now}}},
	}
	legacy := earningsSymbolState{Resolution: earningsResolution{Status: rpc.EarningsStatusTransportFailure, Reason: rpc.EarningsStatusTransportFailure}, Providers: providers, UpdatedAt: now}
	decode := func(state earningsSymbolState) (map[string]earningsSymbolState, error) {
		raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: map[string]earningsSymbolState{"SYNTH1": state}})
		if err != nil {
			t.Fatal(err)
		}
		return decodeEarningsEnvelopeV4(raw, now)
	}
	loaded, err := decode(legacy)
	if err != nil || loaded["SYNTH1"].Resolution.Status != rpc.EarningsStatusNoDatePublished ||
		loaded["SYNTH1"].Providers[earningsWSHProvider].LastAttempt.NextAttempt == nil || !loaded["SYNTH1"].Providers[earningsWSHProvider].LastAttempt.NextAttempt.Equal(next) {
		t.Fatalf("legacy tuple upgrade failed: loaded=%+v err=%v", loaded, err)
	}
	unsupported := legacy
	unsupported.Providers = make(map[string]earningsProviderState, len(providers))
	maps.Copy(unsupported.Providers, providers)
	nasdaq := unsupported.Providers[earningsNasdaqProvider]
	nasdaq.LastAttempt.Status = rpc.EarningsStatusUnsupportedSecurity
	unsupported.Providers[earningsNasdaqProvider] = nasdaq
	loaded, err = decode(unsupported)
	if err != nil || loaded["SYNTH1"].Resolution.Status != rpc.EarningsStatusUnsupportedSecurity {
		t.Fatalf("legacy unsupported tuple upgrade failed: loaded=%+v err=%v", loaded, err)
	}
	for _, mutate := range []func(*rpc.SourceFailure){
		func(f *rpc.SourceFailure) { f.Stage = "other" },
		func(f *rpc.SourceFailure) { f.Retryable = true },
		func(f *rpc.SourceFailure) { f.Code = rpc.SourceFailureInvalidPayload },
	} {
		state := legacy
		state.Providers = map[string]earningsProviderState{}
		maps.Copy(state.Providers, providers)
		failure := *state.Providers[earningsWSHProvider].LastAttempt.LastFailure
		mutate(&failure)
		wsh := state.Providers[earningsWSHProvider]
		wsh.LastAttempt.LastFailure = &failure
		state.Providers[earningsWSHProvider] = wsh
		state.Resolution = earningsResolution{Status: rpc.EarningsStatusNoDatePublished, Reason: rpc.EarningsStatusNoDatePublished}
		if _, err := decode(state); err == nil {
			t.Fatal("non-exact legacy tuple was accepted")
		}
	}
}

func TestEarningsLegacyParserRefreshCommitFailureIsMemoryThrottled(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	now := base.Add(time.Hour)
	legacyFailure := &rpc.SourceFailure{
		Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageNasdaqSchema,
		FailedAt: base, Retryable: false,
	}
	legacyNext := base.Add(earningsNonRetryableFailureRetry)
	legacyAttempt := earningsProviderAttempt{
		Status: rpc.EarningsStatusFormatChange, AttemptedAt: base, CompletedAt: base,
		NextAttempt: &legacyNext, LastFailure: legacyFailure,
		ParserContractVersion: earningsNasdaqParserContractLegacy,
	}
	providers := map[string]earningsProviderState{earningsNasdaqProvider: {LastAttempt: legacyAttempt}}
	seed := earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: map[string]earningsSymbolState{
		"TESTQ": {
			Resolution: resolveEarningsProviders(providers, base), Providers: providers, UpdatedAt: base,
		},
	}}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal("marshal legacy parser refresh fixture")
	}
	store := openMarketTestCoreStore(t)
	created, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw,
	})
	if err != nil {
		t.Fatal("seed legacy parser refresh fixture")
	}

	currentNow := now
	requests := make(chan struct{}, 2)
	var providerCalls atomic.Int32
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return currentNow }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		requests <- struct{}{}
		body := nasdaqTestPayload(t, map[string]any{
			"announcement": nasdaqAnnouncementPrefix("TESTQ") + " Jul 30, 2026",
		}, http.StatusOK)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal("attach legacy parser refresh fixture")
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind,
		ExpectedRevision: created.Revision, JSON: raw,
	}); err != nil {
		t.Fatal("force stale earnings cache revision")
	}
	before, ok, err := store.GetStateDocument(t.Context(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok {
		t.Fatal("read stale-CAS authority fixture")
	}
	beforeObservations, err := store.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsProviderObservationKind,
	})
	if err != nil || len(beforeObservations) != 0 {
		t.Fatal("stale-CAS fixture unexpectedly contains provider observations")
	}

	waitForFailedCommit := func(wantRetry time.Time) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			cache.mu.Lock()
			inflight := cache.inflight["TESTQ"]
			retryAt, gated := cache.authorityRetryNotBefore["TESTQ"]
			cache.mu.Unlock()
			if !inflight && gated && retryAt.Equal(wantRetry) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("failed commit gate = %v present=%v inflight=%v, want %v", retryAt, gated, inflight, wantRetry)
			}
			time.Sleep(time.Millisecond)
		}
	}

	targets := []earningsRefreshTarget{{Symbol: "TESTQ"}}
	cache.kickRefreshTargets(t.Context(), targets)
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy parser refresh did not request the provider")
	}
	waitForFailedCommit(now.Add(earningsAuthorityCommitRetry))
	for range 5 {
		cache.kickRefreshTargets(t.Context(), targets)
	}
	if got := providerCalls.Load(); got != 1 {
		t.Fatalf("provider calls inside failed-commit gate = %d, want 1", got)
	}
	select {
	case <-requests:
		t.Fatal("failed-commit gate allowed another provider request")
	default:
	}

	currentNow = now.Add(earningsAuthorityCommitRetry)
	cache.kickRefreshTargets(t.Context(), targets)
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy parser refresh did not retry after failed-commit gate")
	}
	waitForFailedCommit(currentNow.Add(earningsAuthorityCommitRetry))
	if got := providerCalls.Load(); got != 2 {
		t.Fatalf("provider calls after failed-commit gate = %d, want 2", got)
	}

	after, ok, err := store.GetStateDocument(t.Context(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok || after.Revision != before.Revision || string(after.JSON) != string(before.JSON) {
		t.Fatal("failed receipt-bound CAS changed earnings state authority")
	}
	afterObservations, err := store.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsProviderObservationKind,
	})
	if err != nil || len(afterObservations) != len(beforeObservations) {
		t.Fatal("failed receipt-bound CAS retained provider observations")
	}
	cache.mu.Lock()
	retained := cache.symbols["TESTQ"].Providers[earningsNasdaqProvider].LastAttempt
	cache.mu.Unlock()
	if retained.Status != rpc.EarningsStatusFormatChange || retained.ParserContractVersion != earningsNasdaqParserContractLegacy {
		t.Fatal("failed receipt-bound CAS changed in-memory committed parser state")
	}
}

func TestEarningsStrictDecodersRejectDuplicatePrivateKeysWithoutEcho(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	const privateKey = "SYNTHETIC-PRIVATE-HELD-SYMBOL"
	tests := []struct {
		name   string
		decode func([]byte) error
	}{
		{"v1", func(raw []byte) error { _, err := decodeEarningsEnvelopeV1(raw, now, true); return err }},
		{"v2", func(raw []byte) error { _, err := decodeEarningsEnvelopeV2(raw, now); return err }},
		{"v3", func(raw []byte) error { _, err := decodeEarningsEnvelopeV3(raw, now); return err }},
		{"v4", func(raw []byte) error { _, err := decodeEarningsEnvelopeV4(raw, now); return err }},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version := index + earningsLegacyVersion
			mapKey := "symbols"
			if version == earningsLegacyVersion {
				mapKey = "entries"
			}
			raw := fmt.Appendf(nil, `{"version":%d,%q:{%q:{},%q:{}}}`, version, mapKey, privateKey, privateKey)
			err := test.decode(raw)
			if err == nil {
				t.Fatal("strict earnings decoder accepted a duplicate private key")
			}
			if strings.Contains(err.Error(), privateKey) {
				t.Fatal("strict earnings decoder echoed a private key")
			}
		})
	}
}

func TestEarningsStrictDecodersRejectCaseFoldedVersionAlias(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		decode func([]byte) error
	}{
		{"v1", func(raw []byte) error { _, err := decodeEarningsEnvelopeV1(raw, now, true); return err }},
		{"v2", func(raw []byte) error { _, err := decodeEarningsEnvelopeV2(raw, now); return err }},
		{"v3", func(raw []byte) error { _, err := decodeEarningsEnvelopeV3(raw, now); return err }},
		{"v4", func(raw []byte) error { _, err := decodeEarningsEnvelopeV4(raw, now); return err }},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version := index + earningsLegacyVersion
			mapKey := "symbols"
			if version == earningsLegacyVersion {
				mapKey = "entries"
			}
			raw := fmt.Appendf(nil, `{"version":%d,"Version":%d,%q:{}}`, version, version, mapKey)
			if err := test.decode(raw); err == nil {
				t.Fatal("strict earnings decoder accepted version/Version authority aliases")
			}
		})
	}
}

func TestEarningsV4StrictDecoderRejectsCaseFoldedParserContractAlias(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	attempt := normalizeEarningsAttempt(earningsNasdaqProvider, "TESTQ", earningsProviderFetchResult{
		Status: rpc.EarningsStatusNoDatePublished,
	}, now, now)
	providers := map[string]earningsProviderState{earningsNasdaqProvider: {LastAttempt: attempt}}
	state := earningsSymbolState{
		Resolution: resolveEarningsProviders(providers, now), Providers: providers, UpdatedAt: now,
	}
	raw, err := json.Marshal(earningsPersistEnvelope{
		Version: earningsPersistVersion, Symbols: map[string]earningsSymbolState{"TESTQ": state},
	})
	if err != nil {
		t.Fatal("marshal parser-contract alias fixture")
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal("decode parser-contract alias fixture")
	}
	lastAttempt := envelope["symbols"].(map[string]any)["TESTQ"].(map[string]any)["providers"].(map[string]any)[earningsNasdaqProvider].(map[string]any)["last_attempt"].(map[string]any)
	lastAttempt["Parser_Contract_Version"] = earningsNasdaqParserContract
	raw, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal("marshal parser-contract authority aliases")
	}
	if _, err := decodeEarningsEnvelopeV4(raw, now); err == nil {
		t.Fatal("strict v4 decoder accepted parser-contract authority aliases")
	}
}

func TestEarningsStrictDecodersRejectCaseFoldedPrivateMapKeysWithoutEcho(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	const privateUpper = "SYNTHETIC-PRIVATE-HELD-SYMBOL"
	const privateLower = "synthetic-private-held-symbol"
	tests := []struct {
		name   string
		decode func([]byte) error
	}{
		{"v1", func(raw []byte) error { _, err := decodeEarningsEnvelopeV1(raw, now, true); return err }},
		{"v2", func(raw []byte) error { _, err := decodeEarningsEnvelopeV2(raw, now); return err }},
		{"v3", func(raw []byte) error { _, err := decodeEarningsEnvelopeV3(raw, now); return err }},
		{"v4", func(raw []byte) error { _, err := decodeEarningsEnvelopeV4(raw, now); return err }},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version := index + earningsLegacyVersion
			mapKey := "symbols"
			if version == earningsLegacyVersion {
				mapKey = "entries"
			}
			raw := fmt.Appendf(nil, `{"version":%d,%q:{%q:{},%q:{}}}`, version, mapKey, privateUpper, privateLower)
			err := test.decode(raw)
			if err == nil {
				t.Fatal("strict earnings decoder accepted case-folded private map keys")
			}
			if strings.Contains(err.Error(), privateUpper) || strings.Contains(err.Error(), privateLower) {
				t.Fatal("strict earnings decoder echoed a private map key")
			}
		})
	}
}

func TestEarningsAuthorityRejectsUnknownFields(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if _, err := decodeEarningsEnvelopeV1([]byte(`{"version":1,"entries":{},"unexpected":true}`), now, true); err == nil {
		t.Fatal("strict v1 authority accepted an unknown field")
	}

	next := now.Add(earningsFreshWindow)
	entry := earningsEntry{Date: "2026-07-30", ObservedAt: now}
	providers := map[string]earningsProviderState{earningsNasdaqProvider: {
		LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusDate, Entry: &entry, AttemptedAt: now, CompletedAt: now, NextAttempt: &next},
		LastGood:    &entry,
	}}
	state := earningsSymbolState{Resolution: resolveEarningsProviders(providers, now), Providers: providers, UpdatedAt: now}
	raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: map[string]earningsSymbolState{"AAPL": state}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	symbols := envelope["symbols"].(map[string]any)
	symbols["AAPL"].(map[string]any)["unexpected"] = true
	raw, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEarningsEnvelopeV4(raw, now); err == nil {
		t.Fatal("v4 authority accepted an unknown nested field")
	}
}

func bytesContain(raw []byte, value string) bool {
	return strings.Contains(string(raw), value)
}
