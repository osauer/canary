// Package ibkrledger reads per-currency cash from an existing authenticated
// IBKR Web API session. It never creates a brokerage session or sends orders.
package ibkrledger

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// MaxAge bounds the original broker timestamp, independently of fetch time.
const MaxAge = time.Minute

// Options configures an operator-authenticated, read-only Web API connection.
type Options struct {
	URL, BearerTokenFile, CACertFile string
}

// Cash is one explicitly observed currency row; BASE aggregates are excluded.
type Cash struct {
	Balance, Settled float64
	AsOf             time.Time
}

// Client permits only the two portfolio GETs needed for cash evidence.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New validates transport settings; credentials are read privately per request.
func New(o Options) (*Client, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
		strings.TrimRight(u.Path, "/") != "/v1/api" || u.RawPath != "" {
		return nil, errors.New("cash ledger requires a plain Web API URL ending /v1/api")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := host == "localhost" || ip != nil && ip.IsLoopback()
	if !loopback && !(u.Scheme == "https" && host == "api.ibkr.com" && (u.Port() == "" || u.Port() == "443")) {
		return nil, errors.New("cash ledger endpoint must be loopback or https://api.ibkr.com/v1/api")
	}
	if u.Scheme != "https" && !(loopback && u.Scheme == "http") {
		return nil, errors.New("cash ledger requires verified HTTPS or loopback HTTP")
	}
	if !loopback && o.BearerTokenFile == "" {
		return nil, errors.New("direct Web API access requires an operator-provided SSO bearer token")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A system HTTP proxy must never receive account paths or credentials.
	transport.Proxy = nil
	if o.CACertFile != "" {
		pem, readErr := os.ReadFile(o.CACertFile)
		pool, poolErr := x509.SystemCertPool()
		if poolErr != nil {
			pool = x509.NewCertPool()
		}
		if readErr != nil || !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("cash ledger CA certificate is unavailable or invalid")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{base: strings.TrimRight(u.String(), "/"), token: o.BearerTokenFile,
		http: &http.Client{Transport: transport, Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("cash ledger redirects are forbidden") }}}, nil
}

// Read admits only exact-account, explicit finite currency rows with current
// original timestamps. Inventory lookup is required by the broker endpoint.
func (c *Client) Read(ctx context.Context, account string, clock func() time.Time) (map[string]Cash, error) {
	if account == "" || strings.IndexFunc(account, func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) >= 0 {
		return nil, errors.New("cash ledger requires one concrete broker account")
	}
	var inventory []struct {
		AccountID string `json:"accountId"`
	}
	if err := c.get(ctx, "/portfolio/accounts", &inventory); err != nil {
		return nil, err
	}
	count := 0
	for _, a := range inventory {
		if a.AccountID == account {
			count++
		}
	}
	if count != 1 {
		return nil, errors.New("cash ledger session does not identify the selected broker account exactly once")
	}
	var rows map[string]struct {
		Account   string   `json:"acctcode"`
		Currency  string   `json:"currency"`
		Balance   *float64 `json:"cashbalance"`
		Settled   *float64 `json:"settledcash"`
		Timestamp *int64   `json:"timestamp"`
	}
	if err := c.get(ctx, "/portfolio/"+account+"/ledger", &rows); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	now := clock().UTC()
	out := make(map[string]Cash)
	for currency, row := range rows {
		if currency == "BASE" {
			continue
		}
		if len(currency) != 3 || strings.IndexFunc(currency, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 ||
			row.Currency != currency || row.Account != account || row.Balance == nil || row.Settled == nil || row.Timestamp == nil ||
			math.IsNaN(*row.Balance) || math.IsInf(*row.Balance, 0) || math.IsNaN(*row.Settled) || math.IsInf(*row.Settled, 0) {
			return nil, errors.New("cash ledger has missing, ambiguous or mismatched currency evidence")
		}
		asOf := time.Unix(*row.Timestamp, 0).UTC()
		if *row.Timestamp <= 0 || asOf.After(now) || now.Sub(asOf) > MaxAge {
			return nil, errors.New("cash ledger original broker timestamp is missing, stale or in the future")
		}
		out[currency] = Cash{Balance: *row.Balance, Settled: *row.Settled, AsOf: asOf}
	}
	if len(out) == 0 {
		return nil, errors.New("cash ledger contains no explicit currency cash evidence")
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return errors.New("cash ledger request is invalid")
	}
	req.Header.Set("User-Agent", "Canary-cash-ledger/1")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		info, e := os.Lstat(c.token)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
			return errors.New("cash ledger token must be a private regular file")
		}
		file, e := os.Open(c.token)
		if e != nil {
			return errors.New("cash ledger token is unavailable")
		}
		actual, e := file.Stat()
		if e != nil || !os.SameFile(info, actual) || !actual.Mode().IsRegular() || actual.Mode().Perm()&0077 != 0 {
			file.Close()
			return errors.New("cash ledger token changed while being opened")
		}
		data, e := io.ReadAll(io.LimitReader(file, 16385))
		file.Close()
		token := strings.TrimSpace(string(data))
		if e != nil || len(data) > 16384 || token == "" || strings.ContainsAny(token, "\r\n\x00") {
			return errors.New("cash ledger token is unavailable or invalid")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return errors.New("cash ledger connection failed; check endpoint, authenticated read-only session and trusted certificate")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("cash ledger read unavailable (HTTP %d); keep the sweep held", response.StatusCode)
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(data) > limit {
		return errors.New("cash ledger response is unavailable or exceeds its size bound")
	}
	if err := uniqueJSON(data); err != nil {
		return errors.New("cash ledger response is malformed or has duplicate fields")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return errors.New("cash ledger response has invalid field types")
	}
	return nil
}

// Reject duplicate object keys before typed decoding can silently retain the
// last cash figure, currency identity or account. Bound nesting as well.
func uniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 24 {
			return errors.New("depth")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				// The documented API keys are ASCII. Reject Unicode aliases
				// (encoding/json also folds long-s and Kelvin sign to ASCII).
				for _, ch := range key {
					if ch > 127 {
						return errors.New("non-ASCII key")
					}
				}
				// Distinct case spellings must not replace one cash/identity field.
				key = strings.ToLower(key)
				if !ok || seen[key] {
					return errors.New("duplicate")
				}
				seen[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}
