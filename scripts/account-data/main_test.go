package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/canarytest"
	"github.com/osauer/canary/v2/internal/rpc"
)

func answer(raw string) canarytest.Handler {
	return func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(raw), nil }
}

const (
	availablePositions = `{"as_of":"2026-02-02T15:00:00Z","authority":{"availability":"available","scope":{"account_mode":"live"}},
		"stocks":[{"symbol":"ZQXNEW","sec_type":"STK"},{"symbol":"ZQXK B","sec_type":"STK"}],
		"options":[{"symbol":"ZQXOPT","local_symbol":"ZQXOPT1 260918C00005000","sec_type":"OPT"}]}`
	liveAccount = `{"as_of":"2026-02-02T15:00:00Z","net_liquidation":618034.27,
		"authority":{"availability":"available","scope":{"account_mode":"live"},"fields":{"net_liquidation":true}},
		"currency_exposure":[{"currency":"USD","cash_ccy":-271828.18,"cash_observed":true},{"currency":"GBP","cash_ccy":0}],
		"base_currency_ledger":{"currency":"EUR","cash_ccy":314159.26,"cash_observed":true,"settled_cash_ccy":298765.43}}`
)

func read(t *testing.T, positions, account string) (string, int) {
	t.Helper()
	daemon := canarytest.Serve(t)
	daemon.Handle(rpc.MethodPositionsList, answer(positions))
	daemon.Handle(rpc.MethodAccountSummary, answer(account))
	var out, errOut bytes.Buffer
	code := run(t.Context(), daemon.SocketPath(), &out, &errOut)
	return out.String(), code
}

func TestLiveBookRows(t *testing.T) {
	got, code := read(t, availablePositions, liveAccount)
	want := `holdings|available|2026-02-02
held|ZQXNEW
held|ZQXK
held|ZQXOPT
held|ZQXOPT1
money|available
nlv|618034.27|2026-02-02
cash|-271828.18|
cash|314159.26|
cash|298765.43|
`
	if code != 0 || got != want {
		t.Fatalf("exit %d, rows:\n%s\nwant:\n%s", code, got, want)
	}
}

// A disconnected broker answers with empty rows; they prove nothing, so the
// gate falls back instead of checking against an empty list.
func TestUnavailableBrokerDataIsNotAnEmptyBook(t *testing.T) {
	got, code := read(t, `{"authority":{"availability":"unavailable","reason":"not_connected"}}`,
		`{"net_liquidation":0,"authority":{"availability":"unavailable","reason":"not_connected"}}`)
	want := "holdings|unavailable|not_connected\nmoney|unavailable|not_connected\n"
	if code != 0 || got != want {
		t.Fatalf("exit %d, rows:\n%s", code, got)
	}
}

func TestPaperBookAndUnobservedNLV(t *testing.T) {
	paper := `{"net_liquidation":864197.53,"authority":{"availability":"available","scope":{"account_mode":"paper"},"fields":{"net_liquidation":true}}}`
	if got, _ := read(t, availablePositions, paper); !bytes.HasSuffix([]byte(got), []byte("money|paper|\n")) {
		t.Fatalf("paper book rows:\n%s", got)
	}
	unobserved := `{"as_of":"2026-02-02T15:00:00Z","authority":{"availability":"available","scope":{"account_mode":"live"},"fields":{}}}`
	if got, _ := read(t, availablePositions, unobserved); !bytes.Contains([]byte(got), []byte("nlv|missing|2026-02-02\n")) {
		t.Fatalf("unobserved NLV rows:\n%s", got)
	}
}

// No daemon: exit 2 and nothing on stdout. Connecting never starts one.
func TestNoDaemonReachable(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(t.Context(), filepath.Join(t.TempDir(), "absent.sock"), &out, &errOut); code != exitNoDaemon || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
}

func TestPaperOrUnknownHoldingsCannotReplaceLiveDenylist(t *testing.T) {
	for _, positions := range []string{
		strings.Replace(availablePositions, `"account_mode":"live"`, `"account_mode":"paper"`, 1),
		`{"authority":{"availability":"available","scope":{"account_mode":"paper"}},"stocks":[],"options":[]}`,
		`{"authority":{"availability":"available"},"stocks":[],"options":[]}`,
	} {
		got, code := read(t, positions, liveAccount)
		if code != 0 || !strings.HasPrefix(got, "holdings|unavailable|") || strings.Contains(got, "held|") {
			t.Fatalf("non-live positions supplied a replacement denylist: exit %d, rows:\n%s", code, got)
		}
	}
}
