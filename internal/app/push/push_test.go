package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestGovernancePayloadIsExactCanonicalAllowlist(t *testing.T) {
	t.Parallel()
	candidate := rpc.NudgeCandidate{
		Fingerprint: "sha256:" + strings.Repeat("a", 64),
		Kind:        rpc.NudgeKindPolicyDrift, State: rpc.NudgeStateOpen,
		Severity: "HOSTILE-BALANCE-123", Title: "HOSTILE account DU123", Body: "HOSTILE AAPL token",
		OccurredAt:  time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC),
		Destination: "https://evil.example/steal",
	}
	payload, err := GovernancePayload(candidate, "gov-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	want := `{"title":"Risk settings changed","body":"Review the changed settings before relying on reminders.","severity":"act","kind":"policy_drift","destination":"alerts","display_id":"gov-0123456789abcdef"}`
	if got != want {
		t.Fatalf("payload=%s\nwant=%s", got, want)
	}
	for _, sentinel := range []string{"HOSTILE", "DU123", "AAPL", "evil.example", candidate.Fingerprint, "url", "endpoint", "token"} {
		if strings.Contains(got, sentinel) {
			t.Fatalf("payload leaked %q: %s", sentinel, got)
		}
	}
}

type captureClient struct {
	req *http.Request
}

func (c *captureClient) Do(req *http.Request) (*http.Response, error) {
	c.req = req
	return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Body: io.NopCloser(strings.NewReader(""))}, nil
}

// Apple's push service rejects the whole JWT (403 BadJwtToken) when the sub
// claim is double-prefixed ("mailto:mailto:…", the webpush-go v1.4.0 behavior
// for values that already carry "mailto:") or names an @localhost contact.
func TestWebPushSenderVAPIDContactSurvivesLibraryPrefixing(t *testing.T) {
	t.Parallel()
	browserKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	vapidPrivate, vapidPublic, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	client := &captureClient{}
	sender := WebPushSender{Subscriber: Subscriber, Client: client}
	attempt := sender.Send(context.Background(), state.PushSubscription{
		ID:       "sub-vapid-contact",
		Endpoint: "https://web.push.apple.com/probe-token",
		P256DH:   base64.RawURLEncoding.EncodeToString(browserKey.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}, state.VAPIDKeys{PrivateKey: vapidPrivate, PublicKey: vapidPublic}, SafeDiagnosticPayload())
	if !attempt.OK {
		t.Fatalf("send attempt not OK: %+v", attempt)
	}
	if client.req == nil {
		t.Fatal("no outbound request captured")
	}
	authz := client.req.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(authz, "vapid t=")
	if !ok {
		t.Fatalf("authorization header shape unexpected: %q", authz)
	}
	jwt, _, ok := strings.Cut(rest, ",")
	if !ok {
		t.Fatalf("authorization header missing k= segment: %q", authz)
	}
	segments := strings.Split(jwt, ".")
	if len(segments) != 3 {
		t.Fatalf("JWT does not have three segments: %q", jwt)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Sub != Subscriber {
		t.Fatalf("JWT sub=%q, want %q", claims.Sub, Subscriber)
	}
	if strings.Contains(claims.Sub, "mailto:mailto:") {
		t.Fatalf("JWT sub double-prefixed: %q", claims.Sub)
	}
	if strings.Contains(claims.Sub, "@localhost") {
		t.Fatalf("JWT sub uses localhost contact: %q", claims.Sub)
	}
	if want := "https://web.push.apple.com"; claims.Aud != want {
		t.Fatalf("JWT aud=%q, want %q", claims.Aud, want)
	}
}

func TestSafeDiagnosticPayloadIsFixed(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(SafeDiagnosticPayload())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"title":"Canary notification test","body":"Safe test notification. No account data is included.","destination":"alerts","display_id":"diagnostic-safe-test"}`
	if string(raw) != want {
		t.Fatalf("payload=%s, want=%s", raw, want)
	}
}

func TestTransportClassificationIsAllowlistedAndTruthful(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{name: "deadline", err: context.DeadlineExceeded, want: state.GovernanceTransportDeadlineRetry},
		{name: "cancellation", err: context.Canceled, want: state.GovernanceTransportCanceledRetry},
		{name: "network", err: errors.New("dial sentinel raw endpoint"), want: state.GovernanceTransportNetworkRetry},
		{name: "accepted", status: http.StatusCreated, want: state.GovernanceTransportAccepted},
		{name: "request timeout", status: http.StatusRequestTimeout, want: state.GovernanceTransportHTTPRetry},
		{name: "rate limited", status: http.StatusTooManyRequests, want: state.GovernanceTransportHTTPRetry},
		{name: "server failure", status: http.StatusBadGateway, want: state.GovernanceTransportHTTPRetry},
		{name: "ordinary 400 is terminal rejection", status: http.StatusBadRequest, want: state.GovernanceTransportHTTPRejected},
		{name: "dead not found", status: http.StatusNotFound, want: state.GovernanceTransportDead},
		{name: "dead gone", status: http.StatusGone, want: state.GovernanceTransportDead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyTransport(tt.err, tt.status); got != tt.want {
				t.Fatalf("classifyTransport()=%q, want %q", got, tt.want)
			}
		})
	}
}
