package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// syntheticBorrowFeeFile renders the 2026 provider layout: CRLF lines, a FIGI
// column, a trailing separator and a #EOF data-row count.
func syntheticBorrowFeeFile(bof string, eofCount int, rows ...string) string {
	lines := []string{"#BOF|" + bof, "#SYM|CUR|NAME|CON|ISIN|REBATERATE|FEERATE|AVAILABLE|FIGI|"}
	lines = append(lines, rows...)
	lines = append(lines, fmt.Sprintf("#EOF|%d", eofCount))
	return strings.Join(lines, "\r\n") + "\r\n"
}

var syntheticBorrowFeeRows = []string{
	"SYNA|USD|SYNTHETIC ALPHA INC|900000001|XX0000000001|3.6300|0.2500|300000|BBG000000001|",
	`SYNQ|USD|SYNTHETIC TR DEB 7%"95"|900000002|XX0000000002|2.7343|1.1457|4000|BBG000000002|`,
	"SYNL|USD|SYNTHETIC LARGE CO|900000003|XX0000000003|3.8800|0.2500|>10000000|BBG000000003|",
	"SYNN|USD|SYNTHETIC UNPUBLISHED CO|900000004|XX0000000004|NA|NA|2000||",
	"SYNX|EUR|SYNTHETIC EXTREME AG|900000005|XX0000000005|-45.1000|65.0000|>10000000|BBG000000005|",
	"SYNM|USD|SYNTHETIC MALFORMED|900000006|XX0000000006|abc|1.0000|100|BBG000000006|",
}

func syntheticBorrowFeeBody(bof string) string {
	return syntheticBorrowFeeFile(bof, len(syntheticBorrowFeeRows), syntheticBorrowFeeRows...)
}

func requireBorrowFeeFailure(t *testing.T, err error, code, stage string) {
	t.Helper()
	sourceErr, ok := errors.AsType[*borrowFeeFetchError](err)
	if !ok || sourceErr.code != code || sourceErr.stage != stage {
		t.Fatalf("failure = %v, want %s at %s", err, code, stage)
	}
}

func TestParseIBKRBorrowFeesProviderFormat2026(t *testing.T) {
	const url = "ftp://ftp2.interactivebrokers.com/usa.txt"
	entry, err := parseIBKRBorrowFeeDownload(syntheticBorrowFeeBody("2026.09.24|07:02:49"), url)
	if err != nil {
		t.Fatalf("provider file rejected: %v", err)
	}
	if want := time.Date(2026, 9, 24, 11, 2, 49, 0, time.UTC); !entry.AsOf.Equal(want) || entry.SourceURL != url {
		t.Fatalf("envelope as_of=%s source=%q", entry.AsOf, entry.SourceURL)
	}
	for _, sym := range []string{"SYNA", "SYNQ", "SYNL", "SYNN", "SYNX"} {
		if _, ok := entry.Symbols[sym]; !ok {
			t.Fatalf("published symbol %s dropped; have %d symbols", sym, len(entry.Symbols))
		}
	}
	if _, ok := entry.Symbols["SYNM"]; ok || len(entry.Symbols) != 5 || entry.SkippedRows != 1 {
		t.Fatalf("malformed row not skipped and counted: symbols=%d skipped=%d", len(entry.Symbols), entry.SkippedRows)
	}
	if got := entry.Symbols["SYNQ"].Name; got != `SYNTHETIC TR DEB 7%"95"` {
		t.Fatalf("bare quote altered the name: %q", got)
	}
	large := entry.Symbols["SYNL"]
	if large.Available != 10_000_000 || !large.AvailableLowerBound || large.FeeRate != 0.25 {
		t.Fatalf("lower-bound availability lost: %+v", large)
	}
	unpublished := entry.Symbols["SYNN"]
	if !unpublished.FeeRateUnpublished || !unpublished.RebateRateUnpublished || unpublished.FeeRate != 0 || unpublished.Available != 2000 {
		t.Fatalf("NA rates not marked unpublished: %+v", unpublished)
	}

	// A lower bound never reads as scarcity, and an unpublished fee never
	// flags; only a published extreme fee does, with the bound disclosed.
	now := entry.AsOf.Add(time.Minute)
	for _, sym := range []string{"SYNL", "SYNN"} {
		if flag, ok := marketEventBorrowFeeFlag(sym, entry.Symbols[sym], entry, now); ok {
			t.Fatalf("%s produced flag %+v", sym, flag)
		}
	}
	flag, ok := marketEventBorrowFeeFlag("SYNX", entry.Symbols["SYNX"], entry, now)
	if !ok || flag.ID != rpc.MarketEventBorrowFeeExtreme || !slices.Contains(flag.Details, "available>=10000000") {
		t.Fatalf("extreme fee flag lost its lower-bound disclosure: ok=%v %+v", ok, flag)
	}

	for name, body := range map[string]string{
		"eof_count_mismatch": syntheticBorrowFeeFile("2026.09.24|07:02:49", len(syntheticBorrowFeeRows)-1, syntheticBorrowFeeRows...),
		"row_after_eof":      syntheticBorrowFeeBody("2026.09.24|07:02:49") + syntheticBorrowFeeRows[0] + "\r\n",
		"no_valid_rows":      syntheticBorrowFeeFile("2026.09.24|07:02:49", 1, syntheticBorrowFeeRows[5]),
		"row_before_header":  syntheticBorrowFeeRows[0] + "\r\n" + syntheticBorrowFeeBody("2026.09.24|07:02:49"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseIBKRBorrowFeeDownload(body, url)
			requireBorrowFeeFailure(t, err, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse)
		})
	}
}

func TestBorrowFeeUnpublishedRateIsNotAnObservedZero(t *testing.T) {
	entry, err := parseIBKRBorrowFeeDownload(syntheticBorrowFeeBody("2026.09.24|07:02:49"), "ftp://ftp2.interactivebrokers.com/usa.txt")
	if err != nil {
		t.Fatal(err)
	}
	entry.FetchedAt = entry.AsOf
	health := rpc.SourceHealth{Source: "borrow_fee", Status: rpc.SourceStatusOK, RefreshState: rpc.SourceRefreshCurrent}
	rows, aggregate := bulkBorrowFeeCoverage([]string{"SYNA", "SYNN"}, entry, health)
	if len(rows) != 2 || rows[0].Status != rpc.BorrowFeeCoverageObserved || rows[0].FeeRate == nil || !rows[0].PolicyEligible {
		t.Fatalf("published fee row = %+v", rows)
	}
	na := rows[1]
	if na.Status != rpc.BorrowFeeCoverageUnavailable || na.Reason != "bulk_fee_rate_unpublished" || na.FeeRate != nil || na.PolicyEligible {
		t.Fatalf("NA fee collapsed into an observation: %+v", na)
	}
	if aggregate.Status != rpc.SourceStatusPartial {
		t.Fatalf("unpublished fee counted as coverage: %+v", aggregate)
	}
}

func TestBorrowFeeAuthorityLoadsVersion2AndRoundTripsVersion3(t *testing.T) {
	ctx := t.Context()
	store := openMarketTestCoreStore(t)
	v2 := `{"version":2,"last_good":{"fetched_at":"2026-09-23T15:00:00Z","as_of":"2026-09-23T14:58:00Z","source_url":"ftp://ftp3.interactivebrokers.com/usa.txt","symbols":{"SYNA":{"symbol":"SYNA","rebate_rate":3.63,"fee_rate":0.25,"available":300000}}},"last_attempt":{"outcome":"success","attempted_at":"2026-09-23T14:59:00Z","completed_at":"2026-09-23T15:00:00Z"}}`
	if _, err := store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: marketEventBorrowFeesScope, Kind: marketEventBorrowFeesStateKind, JSON: []byte(v2),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatalf("version 2 authority rejected: %v", err)
	}
	if cache.borrowFees.Symbols["SYNA"].FeeRate != 0.25 || cache.borrowFeesLastAttempt == nil {
		t.Fatalf("version 2 authority not restored: %+v", cache.borrowFees)
	}

	entry, err := parseIBKRBorrowFeeDownload(syntheticBorrowFeeBody("2026.09.24|11:58:00"), "ftp://ftp2.interactivebrokers.com/usa.txt")
	if err != nil {
		t.Fatal(err)
	}
	entry.FetchedAt = now
	if err := cache.persistBorrowFeeSuccess(ctx, entry, now, now); err != nil {
		t.Fatal(err)
	}
	restarted := newMarketEventCache(func() time.Time { return now })
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatalf("version 3 authority rejected: %v", err)
	}
	got := restarted.borrowFees
	if !got.Symbols["SYNL"].AvailableLowerBound || !got.Symbols["SYNN"].FeeRateUnpublished || got.SkippedRows != 1 {
		t.Fatalf("version 3 fields lost across restart: %+v", got)
	}

	forged := strings.Replace(v2, `"available":300000`, `"available":300000,"available_lower_bound":true`, 1)
	other := openMarketTestCoreStore(t)
	if _, err := other.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: marketEventBorrowFeesScope, Kind: marketEventBorrowFeesStateKind, JSON: []byte(forged),
	}); err != nil {
		t.Fatal(err)
	}
	if err := newMarketEventCache(func() time.Time { return now }).UseCoreStore(other); err == nil {
		t.Fatal("version 2 authority carrying version 3 fields was accepted")
	}
}

// fakeBorrowFTP is a loopback FTP server speaking the subset Canary uses.
type fakeBorrowFTP struct {
	body        string
	chunks      int           // transfer in this many writes
	chunkGap    time.Duration // pause before every write after the first
	stallPasses int32         // the first n sessions get no reply to PASS
	rejectLogin bool

	sessions   atomic.Int32
	firstChunk chan struct{}
	chunkOnce  sync.Once
	peerClosed chan string
	mu         sync.Mutex
	conns      []net.Conn
}

func (f *fakeBorrowFTP) start(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.firstChunk = make(chan struct{})
	f.peerClosed = make(chan string, 64)
	t.Cleanup(func() {
		_ = listener.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, conn := range f.conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.track(conn)
			go f.serve(conn)
		}
	}()
	return listener.Addr().String()
}

func (f *fakeBorrowFTP) track(conn net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns = append(f.conns, conn)
}

func (f *fakeBorrowFTP) report(what string) {
	select {
	case f.peerClosed <- what:
	default:
	}
}

func (f *fakeBorrowFTP) serve(conn net.Conn) {
	defer conn.Close()
	session := f.sessions.Add(1)
	reader := bufio.NewReader(conn)
	reply := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }
	reply("220 synthetic borrow-fee server")
	var data net.Listener
	defer func() {
		if data != nil {
			_ = data.Close()
		}
	}()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			f.report("control")
			return
		}
		switch cmd, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " "); cmd {
		case "USER":
			reply("331 password required")
		case "PASS":
			switch {
			case session <= f.stallPasses:
				// Stay silent; the client must give up and reconnect.
			case f.rejectLogin:
				reply("530 login rejected")
			default:
				reply("230 logged in")
			}
		case "TYPE":
			reply("200 binary")
		case "PASV":
			if data, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
				reply("425 no data port")
				continue
			}
			port := data.Addr().(*net.TCPAddr).Port
			reply(fmt.Sprintf("227 Entering Passive Mode (127,0,0,1,%d,%d)", port/256, port%256))
		case "RETR":
			reply("150 opening data connection")
			dc, err := data.Accept()
			if err != nil {
				return
			}
			f.track(dc)
			f.transfer(dc)
			reply("226 transfer complete")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}

func (f *fakeBorrowFTP) transfer(conn net.Conn) {
	defer conn.Close()
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
		f.report("data")
	}()
	chunks := max(f.chunks, 1)
	size := (len(f.body) + chunks - 1) / chunks
	for i := range chunks {
		if i > 0 {
			select {
			case <-closed:
				return
			case <-time.After(f.chunkGap):
			}
		}
		lo := min(i*size, len(f.body))
		if _, err := io.WriteString(conn, f.body[lo:min(lo+size, len(f.body))]); err != nil {
			return
		}
		f.chunkOnce.Do(func() { close(f.firstChunk) })
	}
}

type borrowFTPDialLog struct {
	mu    sync.Mutex
	hosts []string
}

func (l *borrowFTPDialLog) add(host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hosts = append(l.hosts, host)
}

func (l *borrowFTPDialLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.hosts)
}

// loopbackBorrowFTPSource keeps the production login and file but routes each
// endpoint to a loopback address; an empty route is a blackholed host whose
// connect times out. Any other non-loopback dial fails the test, so nothing
// reaches the network.
func loopbackBorrowFTPSource(t *testing.T, endpoints []string, routes map[string]string) (borrowFeeFTPSource, *borrowFTPDialLog) {
	dials := &borrowFTPDialLog{}
	src := ibkrBorrowFeeFTP
	src.endpoints = endpoints
	src.controlTimeout = 2 * time.Second
	src.transferTimeout = 5 * time.Second
	src.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var d net.Dialer
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return d.DialContext(ctx, network, addr)
		}
		target, ok := routes[host]
		if !ok || port != "21" {
			t.Errorf("unexpected borrow-fee dial to %s", addr)
			return nil, errors.New("unexpected dial")
		}
		dials.add(host)
		if target == "" {
			return nil, &net.OpError{Op: "dial", Net: network, Err: os.ErrDeadlineExceeded}
		}
		return d.DialContext(ctx, network, target)
	}
	return src, dials
}

// useBorrowFTPSource installs src as the production source for one test.
func useBorrowFTPSource(t *testing.T, src borrowFeeFTPSource) {
	orig, origFetch := ibkrBorrowFeeFTP, fetchIBKRBorrowFees
	ibkrBorrowFeeFTP, fetchIBKRBorrowFees = src, fetchIBKRBorrowFeesFTP
	t.Cleanup(func() { ibkrBorrowFeeFTP, fetchIBKRBorrowFees = orig, origFetch })
}

const (
	borrowFTPDocumented = "ftp3.interactivebrokers.com"
	borrowFTPMirror     = "ftp2.interactivebrokers.com"
)

func TestBorrowFeeFTPFailsOverToSecondEndpoint(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC) // regular session
	mirror := &fakeBorrowFTP{body: syntheticBorrowFeeBody("2026.09.24|11:58:00")}
	src, dials := loopbackBorrowFTPSource(t, ibkrBorrowFeeFTP.endpoints, map[string]string{
		borrowFTPDocumented: "", borrowFTPMirror: mirror.start(t),
	})
	useBorrowFTPSource(t, src)

	store := openMarketTestCoreStore(t)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	failedAt := now.Add(-time.Hour)
	next := failedAt.Add(marketEventsBorrowFeeRetryAfter)
	if err := cache.persistBorrowFeeFailure(ctx, marketEventBorrowFeeEntry{}, marketEventBorrowFeeAttempt{
		Outcome: marketEventBorrowFeeOutcomeFailure, AttemptedAt: failedAt, CompletedAt: failedAt, NextAttempt: &next,
		Failure: &rpc.SourceFailure{Code: rpc.SourceFailureDNSFailed, Stage: rpc.SourceFailureStageFTPControlConnect, FailedAt: failedAt, Retryable: true},
	}); err != nil {
		t.Fatal(err)
	}

	entry, health, err := cache.loadBorrowFees(ctx)
	if err != nil || health.Status != rpc.SourceStatusOK || health.LastFailure != nil {
		t.Fatalf("failover did not recover: err=%v health=%+v", err, health)
	}
	if entry.SourceURL != "ftp://"+borrowFTPMirror+"/usa.txt" || !slices.Equal(dials.snapshot(), []string{borrowFTPDocumented, borrowFTPMirror}) {
		t.Fatalf("serving endpoint source=%q dials=%v", entry.SourceURL, dials.snapshot())
	}
	if !slices.ContainsFunc(health.Notes, func(note string) bool { return strings.HasPrefix(note, "1 malformed") }) {
		t.Fatalf("skipped provider row not disclosed: %v", health.Notes)
	}
	doc, ok, err := store.GetStateDocument(ctx, marketEventBorrowFeesScope, marketEventBorrowFeesStateKind)
	if err != nil || !ok {
		t.Fatalf("authority ok=%v err=%v", ok, err)
	}
	var state marketEventBorrowFeesState
	if err := decodeStrictMarketEventJSON(doc.JSON, &state); err != nil {
		t.Fatal(err)
	}
	if state.LastAttempt == nil || state.LastAttempt.Outcome != marketEventBorrowFeeOutcomeSuccess || state.LastAttempt.Failure != nil ||
		state.LastGood == nil || state.LastGood.SourceURL != entry.SourceURL {
		t.Fatalf("success did not replace the dns_failed attempt: %+v", state)
	}

	restarted := newMarketEventCache(func() time.Time { return now.Add(time.Minute) })
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	served, health, err := restarted.loadBorrowFees(ctx)
	if err != nil || health.LastFailure != nil || served.SourceURL != entry.SourceURL ||
		!served.Symbols["SYNL"].AvailableLowerBound || len(dials.snapshot()) != 2 {
		t.Fatalf("restart lost the receipt: err=%v health=%+v dials=%v", err, health, dials.snapshot())
	}

	// Once the retained file ages out, the endpoint that served it goes first.
	restarted.now = func() time.Time { return now.Add(20 * time.Minute) }
	if _, _, err := restarted.loadBorrowFees(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dials.snapshot(); !slices.Equal(got[2:], []string{borrowFTPMirror}) {
		t.Fatalf("refresh did not start with the last serving endpoint: %v", got)
	}
}

func TestBorrowFeeFTPBothEndpointsFailKeepsTypedBackoff(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)
	rejecting := &fakeBorrowFTP{rejectLogin: true}
	// The documented host rejects the login and the mirror is dark: the login
	// rejection is the more informative failure even though it came first.
	src, dials := loopbackBorrowFTPSource(t, ibkrBorrowFeeFTP.endpoints, map[string]string{
		borrowFTPDocumented: rejecting.start(t), borrowFTPMirror: "",
	})
	useBorrowFTPSource(t, src)
	store := openMarketTestCoreStore(t)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	_, health, err := cache.loadBorrowFees(ctx)
	if err == nil || health.LastFailure == nil || health.LastFailure.Code != rpc.SourceFailureAuthenticationRejected ||
		health.LastFailure.Stage != rpc.SourceFailureStageFTPAuthenticate || health.RefreshState != rpc.SourceRefreshFetchFailed {
		t.Fatalf("typed failure lost: err=%v health=%+v", err, health)
	}
	if want := now.Add(marketEventsBorrowFeeRetryAfter); health.NextAttempt == nil || !health.NextAttempt.Equal(want) {
		t.Fatalf("backoff next attempt = %v, want %s", health.NextAttempt, want)
	}
	cache.now = func() time.Time { return now.Add(5 * time.Minute) }
	_, health, _ = cache.loadBorrowFees(ctx)
	if health.RefreshState != rpc.SourceRefreshFetchFailedBackoff || len(dials.snapshot()) != 2 {
		t.Fatalf("backoff not honoured: health=%+v dials=%v", health, dials.snapshot())
	}
}

func TestBorrowFeeFTPRetriesStalledPassOnce(t *testing.T) {
	body := syntheticBorrowFeeBody("2026.09.24|11:58:00")
	t.Run("reconnect_succeeds", func(t *testing.T) {
		server := &fakeBorrowFTP{body: body, stallPasses: 1}
		src, _ := loopbackBorrowFTPSource(t, []string{"ftp-a.test"}, map[string]string{"ftp-a.test": server.start(t)})
		src.controlTimeout = 250 * time.Millisecond
		entry, err := src.fetch(t.Context(), "")
		if err != nil || len(entry.Symbols) != 5 || server.sessions.Load() != 2 {
			t.Fatalf("stalled PASS was not retried: err=%v sessions=%d", err, server.sessions.Load())
		}
	})
	t.Run("second_stall_fails_over", func(t *testing.T) {
		stalled := &fakeBorrowFTP{body: body, stallPasses: 99}
		healthy := &fakeBorrowFTP{body: body}
		src, _ := loopbackBorrowFTPSource(t, []string{"ftp-a.test", "ftp-b.test"}, map[string]string{
			"ftp-a.test": stalled.start(t), "ftp-b.test": healthy.start(t),
		})
		src.controlTimeout = 250 * time.Millisecond
		entry, err := src.fetch(t.Context(), "")
		if err != nil || entry.SourceURL != "ftp://ftp-b.test/usa.txt" || stalled.sessions.Load() != 2 {
			t.Fatalf("expected one reconnect then failover: err=%v source=%q sessions=%d", err, entry.SourceURL, stalled.sessions.Load())
		}
	})
}

func TestBorrowFeeFTPSlowTransferWithinBudget(t *testing.T) {
	body := syntheticBorrowFeeBody("2026.09.24|11:58:00")
	slow := func(t *testing.T, transfer time.Duration) (marketEventBorrowFeeEntry, error) {
		server := &fakeBorrowFTP{body: body, chunks: 8, chunkGap: 100 * time.Millisecond}
		src, _ := loopbackBorrowFTPSource(t, []string{"ftp-a.test"}, map[string]string{"ftp-a.test": server.start(t)})
		src.controlTimeout = 250 * time.Millisecond
		src.transferTimeout = transfer
		return src.fetch(t.Context(), "")
	}
	// The transfer outlasts the control deadline several times over.
	if entry, err := slow(t, 5*time.Second); err != nil || len(entry.Symbols) != 5 {
		t.Fatalf("slow transfer failed under its own budget: %v", err)
	}
	_, err := slow(t, 300*time.Millisecond)
	requireBorrowFeeFailure(t, err, rpc.SourceFailureTimeout, rpc.SourceFailureStageFTPRetrieve)
}

func TestBorrowFeeFTPCancellationMidTransferPersistsNothing(t *testing.T) {
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)
	server := &fakeBorrowFTP{body: syntheticBorrowFeeBody("2026.09.24|11:58:00"), chunks: 50, chunkGap: 200 * time.Millisecond}
	src, _ := loopbackBorrowFTPSource(t, []string{borrowFTPMirror}, map[string]string{borrowFTPMirror: server.start(t)})
	useBorrowFTPSource(t, src)
	store := openMarketTestCoreStore(t)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := cache.loadBorrowFees(ctx)
		done <- err
	}()
	select {
	case <-server.firstChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled refresh returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt the transfer")
	}
	closed := map[string]bool{}
	for deadline := time.After(2 * time.Second); !closed["control"] || !closed["data"]; {
		select {
		case what := <-server.peerClosed:
			closed[what] = true
		case <-deadline:
			t.Fatalf("sockets left open after cancellation: %v", closed)
		}
	}
	if _, ok, err := store.GetStateDocument(t.Context(), marketEventBorrowFeesScope, marketEventBorrowFeesStateKind); err != nil || ok || cache.borrowFeesLastAttempt != nil {
		t.Fatalf("canceled transfer persisted an attempt: ok=%v err=%v attempt=%+v", ok, err, cache.borrowFeesLastAttempt)
	}
}

func TestBorrowFeeNotDueCarriesNextOpen(t *testing.T) {
	failedAt := time.Date(2026, 9, 23, 19, 48, 36, 0, time.UTC)
	backoff := failedAt.Add(marketEventsBorrowFeeRetryAfter)
	failure := &marketEventBorrowFeeAttempt{
		Outcome: marketEventBorrowFeeOutcomeFailure, AttemptedAt: failedAt, CompletedAt: failedAt, NextAttempt: &backoff,
		Failure: &rpc.SourceFailure{Code: rpc.SourceFailureDNSFailed, Stage: rpc.SourceFailureStageFTPControlConnect, FailedAt: failedAt, Retryable: true},
	}
	lastGood := marketEventBorrowFeeEntry{
		FetchedAt: time.Date(2026, 9, 24, 19, 55, 0, 0, time.UTC), AsOf: time.Date(2026, 9, 24, 19, 50, 0, 0, time.UTC),
		SourceURL: "ftp://" + borrowFTPMirror + "/usa.txt", Symbols: map[string]marketEventBorrowFeeRecord{"SYNA": {Symbol: "SYNA", FeeRate: 0.25}},
	}
	for _, tc := range []struct {
		name    string
		now     time.Time
		cached  marketEventBorrowFeeEntry
		attempt *marketEventBorrowFeeAttempt
		next    time.Time
	}{
		{"premarket_retained_failure", time.Date(2026, 9, 24, 11, 15, 0, 0, time.UTC), marketEventBorrowFeeEntry{}, failure, time.Date(2026, 9, 24, 13, 30, 0, 0, time.UTC)},
		{"after_close_last_good", time.Date(2026, 9, 24, 22, 0, 0, 0, time.UTC), lastGood, nil, time.Date(2026, 9, 25, 13, 30, 0, 0, time.UTC)},
		{"weekend", time.Date(2026, 9, 26, 15, 0, 0, 0, time.UTC), lastGood, failure, time.Date(2026, 9, 28, 13, 30, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := newMarketEventCache(func() time.Time { return tc.now })
			cache.borrowFees, cache.borrowFeesLastAttempt = tc.cached, tc.attempt
			_, health, err := cache.loadBorrowFees(t.Context())
			if err != nil || health.RefreshState != rpc.SourceRefreshNotDue {
				t.Fatalf("not-due read err=%v health=%+v", err, health)
			}
			if health.NextAttempt == nil || !health.NextAttempt.Equal(tc.next) {
				t.Fatalf("next attempt = %v, want next regular open %s", health.NextAttempt, tc.next)
			}
			if (tc.attempt != nil) != (health.LastFailure != nil) {
				t.Fatalf("retained failure changed: %+v", health.LastFailure)
			}
		})
	}
}

func TestBorrowFeeLogsFailureClassChangeAndRecovery(t *testing.T) {
	now := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	var buf bytes.Buffer
	cache.logger = &Logger{l: slog.New(slog.NewTextHandler(&buf, nil))}
	orig := fetchIBKRBorrowFees
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })
	outcomes := []error{
		newBorrowFeeFetchError(rpc.SourceFailureTimeout, rpc.SourceFailureStageFTPControlConnect, true),
		newBorrowFeeFetchError(rpc.SourceFailureTimeout, rpc.SourceFailureStageFTPControlConnect, true),
		newBorrowFeeFetchError(rpc.SourceFailureAuthenticationRejected, rpc.SourceFailureStageFTPAuthenticate, true),
		nil,
		nil,
	}
	want := []struct{ warns, recoveries int }{{1, 0}, {1, 0}, {2, 0}, {2, 1}, {2, 1}}
	for i, outcome := range outcomes {
		fetchIBKRBorrowFees = func(context.Context, string) (marketEventBorrowFeeEntry, error) {
			if outcome != nil {
				return marketEventBorrowFeeEntry{}, outcome
			}
			return marketEventBorrowFeeEntry{AsOf: now, SourceURL: "ftp://" + borrowFTPMirror + "/usa.txt",
				Symbols: map[string]marketEventBorrowFeeRecord{"SYNA": {Symbol: "SYNA", FeeRate: 0.25}}}, nil
		}
		_, _, _ = cache.loadBorrowFees(t.Context())
		logged := buf.String()
		if warns, recoveries := strings.Count(logged, "level=WARN"), strings.Count(logged, "recovered"); warns != want[i].warns || recoveries != want[i].recoveries {
			t.Fatalf("after outcome %d: warns=%d recoveries=%d, want %+v\n%s", i, warns, recoveries, want[i], logged)
		}
		now = now.Add(marketEventsBorrowFeeRetryAfter + time.Minute)
	}
}
