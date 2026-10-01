package ibkrledger

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ledgerFixture(t *testing.T, inventory, ledger string, seen *[]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.Method+" "+r.URL.Path)
		}
		if r.Method != http.MethodGet || r.Header.Get("User-Agent") == "" {
			t.Error("read-only request contract")
		}
		switch r.URL.Path {
		case "/v1/api/portfolio/accounts":
			fmt.Fprint(w, inventory)
		case "/v1/api/portfolio/DU1234567/ledger":
			fmt.Fprint(w, ledger)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "no", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{URL: srv.URL + "/v1/api"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExactCurrencyLedgerAndZero(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{"BASE":{"settledcash":999999},"EUR":{"acctcode":"DU1234567","currency":"EUR","cashbalance":12000,"settledcash":9000,"timestamp":%d},"USD":{"acctcode":"DU1234567","currency":"USD","cashbalance":0,"settledcash":0,"timestamp":%d}}`, now.Unix(), now.Unix())
	var seen []string
	c := ledgerFixture(t, `[{"accountId":"DU1234567"}]`, body, &seen)
	rows, err := c.Read(context.Background(), "DU1234567", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows["EUR"].Settled != 9000 || rows["USD"].Settled != 0 || !rows["EUR"].AsOf.Equal(now) {
		t.Fatalf("currency receipt lost: %#v", rows)
	}
	if strings.Join(seen, ",") != "GET /v1/api/portfolio/accounts,GET /v1/api/portfolio/DU1234567/ledger" {
		t.Fatal(seen)
	}
}

func TestLedgerRefusesAmbiguousAndUnavailableEvidence(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	valid := map[string]any{"acctcode": "DU1234567", "currency": "USD", "cashbalance": 10000, "settledcash": 9000, "timestamp": now.Unix()}
	cases := map[string]func(map[string]any){
		"wrong account":  func(r map[string]any) { r["acctcode"] = "DU7654321" },
		"wrong currency": func(r map[string]any) { r["currency"] = "EUR" },
		"absent cash":    func(r map[string]any) { delete(r, "cashbalance") },
		"null settled":   func(r map[string]any) { r["settledcash"] = nil },
		"cash string":    func(r map[string]any) { r["cashbalance"] = "10000" },
		"absent clock":   func(r map[string]any) { delete(r, "timestamp") },
		"stale":          func(r map[string]any) { r["timestamp"] = now.Add(-MaxAge - time.Second).Unix() },
		"future":         func(r map[string]any) { r["timestamp"] = now.Add(time.Second).Unix() },
		"milliseconds":   func(r map[string]any) { r["timestamp"] = now.UnixMilli() },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := maps.Clone(valid)
			mutate(r)
			data, _ := json.Marshal(map[string]any{"USD": r})
			c := ledgerFixture(t, `[{"accountId":"DU1234567"}]`, string(data), nil)
			if _, err := c.Read(context.Background(), "DU1234567", func() time.Time { return now }); err == nil {
				t.Fatal("admitted uncertain cash")
			}
		})
	}
	for _, body := range []string{`{"BASE":{"cashbalance":10000,"settledcash":9000}}`, `{"USD":{"settledcash":1,"settledcash":9000}}`, `{"USD":{},"USD":{}}`, `{"USD":{"cashbalance":0,"CashBalance":60000,"settledcash":0,"SettledCash":60000,"acctcode":"DU1234567","currency":"USD","timestamp":1790848800}}`, `null`, `{} {}`} {
		c := ledgerFixture(t, `[{"accountId":"DU1234567"}]`, body, nil)
		if _, err := c.Read(context.Background(), "DU1234567", func() time.Time { return now }); err == nil {
			t.Fatal("admitted ambiguous/aggregate JSON")
		}
	}
}

func TestInventoryCannotSelectAnotherAccount(t *testing.T) {
	for _, inventory := range []string{`[]`, `[{"accountId":"DU7654321"}]`, `[{"accountId":"DU1234567"},{"accountId":"DU1234567"}]`} {
		var seen []string
		c := ledgerFixture(t, inventory, `{}`, &seen)
		if _, err := c.Read(context.Background(), "DU1234567", time.Now); err == nil {
			t.Fatal("ambiguous inventory admitted")
		}
		if len(seen) != 1 {
			t.Fatal("ledger was queried without an exact selected account")
		}
	}
}

func TestLedgerRefusesUnicodeFieldAliases(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	for _, key := range []string{`ca\u017fhbalance`, `\u017fettledcash`} {
		body := fmt.Sprintf(`{"USD":{"acctcode":"DU1234567","currency":"USD","cashbalance":0,"settledcash":0,"%s":60000,"timestamp":%d}}`, key, now.Unix())
		c := ledgerFixture(t, `[{"accountId":"DU1234567"}]`, body, nil)
		if _, err := c.Read(context.Background(), "DU1234567", func() time.Time { return now }); err == nil {
			t.Fatal("admitted Unicode alias of cash field")
		}
	}
}

func TestLedgerTransportBoundsAndRedaction(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/v1/api", "https://other.example/v1/api", "http://user:secret@localhost/v1/api", "http://localhost/v1/api?secret=1", "http://localhost/iserver", "http://localhost/v1/api#secret"} {
		if _, err := New(Options{URL: endpoint}); err == nil {
			t.Fatalf("unsafe endpoint admitted %s", endpoint)
		}
	}
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits++; fmt.Fprint(w, `[]`) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/iserver/auth/ssodh/init", http.StatusFound)
	}))
	defer srv.Close()
	c, _ := New(Options{URL: srv.URL + "/v1/api"})
	_, err := c.Read(context.Background(), "DU1234567", time.Now)
	if err == nil || targetHits != 0 || strings.Contains(err.Error(), "DU1234567") || strings.Contains(err.Error(), srv.URL) {
		t.Fatal("redirect or private transport data escaped")
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("synthetic-private-token"), 0644); err != nil {
		t.Fatal(err)
	}
	c, _ = New(Options{URL: srv.URL + "/v1/api", BearerTokenFile: token})
	if _, err = c.Read(context.Background(), "DU1234567", time.Now); err == nil || strings.Contains(err.Error(), "synthetic-private-token") {
		t.Fatal("nonprivate credential admitted")
	}
	if _, err := New(Options{URL: "https://api.ibkr.com/v1/api"}); err == nil {
		t.Fatal("direct API without authenticated bearer admitted")
	}
}

func TestLedgerTLSIsVerifiedAndPrivateBearerIsNotDisclosed(t *testing.T) {
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer synthetic-sso-token" {
			t.Error("private SSO credential not attached")
		}
		http.Error(w, "private-account-DU1234567 synthetic-sso-token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(token, []byte("synthetic-sso-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{URL: srv.URL + "/v1/api", BearerTokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Read(context.Background(), "DU1234567", time.Now); err == nil || requests != 0 {
		t.Fatal("untrusted TLS was used")
	}
	c, err = New(Options{URL: srv.URL + "/v1/api", BearerTokenFile: token, CACertFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Read(context.Background(), "DU1234567", time.Now); err == nil || requests != 1 || strings.Contains(err.Error(), "DU1234567") || strings.Contains(err.Error(), "synthetic-sso-token") {
		t.Fatal("auth error lost bounds or disclosed response", err)
	}
}

func TestLedgerBodyAndMoneyOverflowBounds(t *testing.T) {
	for _, body := range []string{strings.Repeat(" ", 1<<20) + "[]", `{"USD":{"acctcode":"DU1234567","currency":"USD","cashbalance":1e309,"settledcash":1,"timestamp":1790848800}}`} {
		c := ledgerFixture(t, `[{"accountId":"DU1234567"}]`, body, nil)
		if _, err := c.Read(context.Background(), "DU1234567", time.Now); err == nil {
			t.Fatal("unbounded body or money admitted")
		}
	}
}
