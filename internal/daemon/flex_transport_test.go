package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFlexRawCallUsesDocumentedGET(t *testing.T) {
	t.Parallel()

	want := url.Values{
		"t":  {"private-token"},
		"q":  {"private-query"},
		"v":  {"3"},
		"fd": {"20250101"},
		"td": {"20251231"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.URL.Query(); got.Encode() != want.Encode() {
			t.Errorf("query = %q, want %q", got.Encode(), want.Encode())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("body = %q, want empty", body)
		}
		if got := r.Header.Get("User-Agent"); got != flexUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, flexUserAgent)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)

	got, err := flexRawCall(context.Background(), server.Client(), server.URL, want)
	if err != nil {
		t.Fatalf("flexRawCall: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
}

func TestFlexRawCallRefusesRedirectAndRedactsSecrets(t *testing.T) {
	t.Parallel()

	const token = "private-token-never-forward"
	const query = "private-query-never-forward"
	var secondHostCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secondHostCalls.Add(1)
	}))
	t.Cleanup(second.Close)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, second.URL+"?t="+token+"&q="+query, http.StatusFound)
	}))
	t.Cleanup(first.Close)

	_, err := flexRawCall(context.Background(), first.Client(), first.URL, url.Values{"t": {token}, "q": {query}, "v": {"3"}})
	if err == nil {
		t.Fatal("redirect returned no error")
	}
	if secondHostCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want zero", secondHostCalls.Load())
	}
	if text := err.Error(); strings.Contains(text, token) || strings.Contains(text, query) || strings.Contains(text, first.URL) || strings.Contains(text, second.URL) {
		t.Fatalf("error leaked request material: %q", text)
	}
}

func TestFlexRawCallRedactsRequestOnHTTPFailure(t *testing.T) {
	t.Parallel()

	const token = "private-token"
	const reference = "private-reference"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	_, err := flexRawCall(context.Background(), server.Client(), server.URL, url.Values{"t": {token}, "q": {reference}, "v": {"3"}})
	if err == nil {
		t.Fatal("HTTP failure returned no error")
	}
	if text := err.Error(); strings.Contains(text, token) || strings.Contains(text, reference) || strings.Contains(text, server.URL) {
		t.Fatalf("error leaked request material: %q", text)
	}
}

func TestFlexEnvelopeFailureRecognizesCurrentV3Codes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code      string
		reason    string
		retryable bool
	}{
		{code: "1017", reason: rpc.ReconReportReasonResponseInvalid, retryable: true},
		{code: "1020", reason: rpc.ReconReportReasonResponseInvalid, retryable: true},
		{code: "1021", reason: rpc.ReconReportReasonReportNotReady, retryable: true},
		{code: "1025", reason: rpc.ReconReportReasonResponseInvalid, retryable: true},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			failure, ok := flexEnvelopeFailure(test.code).(*flexFetchFailure)
			if !ok {
				t.Fatalf("failure type = %T", flexEnvelopeFailure(test.code))
			}
			if failure.reason != test.reason || failure.retryable != test.retryable {
				t.Fatalf("failure = reason %q retryable %t, want %q %t", failure.reason, failure.retryable, test.reason, test.retryable)
			}
			if failure.brokerCode != test.code {
				t.Fatalf("broker code = %q, want %q", failure.brokerCode, test.code)
			}
			if strings.Contains(failure.detail, "unrecognized") {
				t.Fatalf("documented code %s was classified as unrecognized", test.code)
			}
		})
	}
}

func TestFlexEnvelopeFailureSanitizesBrokerCode(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"", "123", "12345", "12a4", "１２３４", "1025 broker text"} {
		failure := flexEnvelopeFailure(code).(*flexFetchFailure)
		if failure.brokerCode != "" {
			t.Fatalf("code %q retained as %q", code, failure.brokerCode)
		}
	}
}
