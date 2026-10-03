package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func watchlistServer(t *testing.T) (*Server, *corestore.Store, string) {
	t.Helper()
	path := filepath.Join(privateTestDir(t), "daemon.db")
	core, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	return &Server{coreStore: core}, core, path
}

func mutateWatchlist(t *testing.T, s *Server, method string, params any) (*rpc.Watchlist, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return s.handleWatchlistMutation(t.Context(), &rpc.Request{Method: method, Params: raw})
}

func replaceWatchlist(t *testing.T, s *Server, symbols []rpc.WatchlistContract, rev int64, id string) (*rpc.Watchlist, error) {
	t.Helper()
	return mutateWatchlist(t, s, rpc.MethodWatchlistReplace, rpc.WatchlistReplaceRequest{Symbols: symbols, ExpectedRevision: rev, RequestID: id})
}

func TestWatchlistPristineEmptyOwnershipAndOfflineRestart(t *testing.T) {
	s, core, path := watchlistServer(t)
	legacy := privateTestDir(t)
	t.Setenv("XDG_DATA_HOME", legacy)
	if err := os.MkdirAll(filepath.Join(legacy, "ibkr"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "ibkr", "watchlist.json"), []byte(`{"symbols":["IGNORED"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	head, _ := core.AuthorityHead(t.Context())
	out, err := s.handleWatchlistList(t.Context())
	if err != nil || out.Version != 1 || out.Revision != 0 || out.Symbols == nil || len(out.Symbols) != 0 || out.AsOf.IsZero() {
		t.Fatal(out, err)
	}
	after, _ := core.AuthorityHead(t.Context())
	if head != after {
		t.Fatal("reading pristine list wrote state")
	}
	first, err := replaceWatchlist(t, s, []rpc.WatchlistContract{}, 0, "empty-intent")
	if err != nil || first.Revision != 1 || first.SavedRevision != 1 {
		t.Fatal(first, err)
	}
	if _, err = replaceWatchlist(t, s, []rpc.WatchlistContract{{Symbol: "SYNTH"}}, 0, "old-migration"); err == nil {
		t.Fatal("migration overwrote intentional empty list")
	}
	if err = core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = &Server{coreStore: reopened}
	out, err = replaceWatchlist(t, s, []rpc.WatchlistContract{}, 0, "empty-intent")
	if err != nil || !out.Replay || out.Revision != 1 || len(out.Symbols) != 0 {
		t.Fatal(out, err)
	}
	if s.watchlistSubsystemHealth().Status != "ready" {
		t.Fatal("offline list unavailable")
	}
}

func TestWatchlistImmutableReceiptCurrentReplayAndNoopFence(t *testing.T) {
	s, core, _ := watchlistServer(t)
	firstTerms := []rpc.WatchlistContract{{Symbol: " synth ", ConID: 17}}
	first, err := replaceWatchlist(t, s, firstTerms, 0, "first")
	if err != nil || first.Revision != 1 || first.Symbols[0].Symbol != "SYNTH" || first.Symbols[0].Exchange != "SMART" {
		t.Fatal(first, err)
	}
	first.Symbols[0].Symbol = "ALTERED"
	if _, err = replaceWatchlist(t, s, firstTerms, 1, "first"); err == nil {
		t.Fatal("receipt ignored changed revision term")
	}
	if _, err = mutateWatchlist(t, s, rpc.MethodWatchlistRemove, rpc.WatchlistRemoveRequest{Symbol: "SYNTH", ExpectedRevision: 0, RequestID: "first"}); err == nil {
		t.Fatal("receipt ignored changed operation")
	}
	second, err := replaceWatchlist(t, s, []rpc.WatchlistContract{{Symbol: "OTHER", ConID: 18}}, 1, "second")
	if err != nil || second.Revision != 2 {
		t.Fatal(second, err)
	}
	head, _ := core.AuthorityHead(t.Context())
	replay, err := replaceWatchlist(t, s, firstTerms, 0, "first")
	if err != nil || !replay.Replay || replay.Revision != 2 || replay.SavedRevision != 1 || replay.Symbols[0].Symbol != "OTHER" {
		t.Fatal(replay, err)
	}
	after, _ := core.AuthorityHead(t.Context())
	if head != after {
		t.Fatal("replay appended event")
	}
	noop, err := replaceWatchlist(t, s, second.Symbols, 2, "noop")
	if err != nil || noop.Revision != 2 || noop.SavedRevision != 2 {
		t.Fatal(noop, err)
	}
	if _, err = replaceWatchlist(t, s, second.Symbols, 1, "stale-noop"); err == nil {
		t.Fatal("stale noop not fenced")
	}
	events, err := core.LoadEvents(t.Context(), corestore.EventQuery{ScopeKey: daemonStateScope, Type: watchlistReceiptType})
	if err != nil || len(events) != 3 {
		t.Fatal(len(events), err)
	}
	for _, event := range events {
		if event.Origin != rpc.OrderOriginAgent {
			t.Fatal("invented human authority")
		}
	}
}

func TestWatchlistAddRemoveIdentityAndConcurrentCAS(t *testing.T) {
	s, core, _ := watchlistServer(t)
	add := rpc.WatchlistAddRequest{Contract: rpc.WatchlistContract{Symbol: "SYNTH", ConID: 17}, ExpectedRevision: 0, RequestID: "add"}
	one, err := mutateWatchlist(t, s, rpc.MethodWatchlistAdd, add)
	if err != nil || one.Revision != 1 {
		t.Fatal(one, err)
	}
	add.ExpectedRevision, add.RequestID = 1, "add-noop"
	if out, err := mutateWatchlist(t, s, rpc.MethodWatchlistAdd, add); err != nil || out.Revision != 1 {
		t.Fatal(out, err)
	}
	add.Contract.ConID, add.RequestID = 18, "changed-identity"
	if _, err = mutateWatchlist(t, s, rpc.MethodWatchlistAdd, add); err == nil {
		t.Fatal("add silently rebound identity")
	}
	add.Contract.Symbol, add.Contract.ConID, add.RequestID = "OTHER", 17, "duplicate-contract"
	if _, err = mutateWatchlist(t, s, rpc.MethodWatchlistAdd, add); err == nil {
		t.Fatal("duplicate contract accepted")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"race-one", "race-two"} {
		wg.Go(func() {
			other := &Server{coreStore: core}
			_, err := replaceWatchlist(t, other, []rpc.WatchlistContract{{Symbol: strings.ReplaceAll(strings.ToUpper(id), "-", "")}}, 1, id)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			var rpcErr *rpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeWatchlistConflict {
				t.Fatal(err)
			}
		}
	}
	if wins != 1 {
		t.Fatal("CAS winners", wins)
	}
	current, _ := s.handleWatchlistList(t.Context())
	out, err := mutateWatchlist(t, s, rpc.MethodWatchlistRemove, rpc.WatchlistRemoveRequest{Symbol: current.Symbols[0].Symbol, ExpectedRevision: 2, RequestID: "remove"})
	if err != nil || out.Revision != 3 || out.Symbols == nil || len(out.Symbols) != 0 {
		t.Fatal(out, err)
	}
}

func TestWatchlistStrictInputUnavailableAndConflictWire(t *testing.T) {
	s, core, _ := watchlistServer(t)
	for _, raw := range []string{`{}`, `{"symbols":[],"request_id":"x"}`, `{"symbols":null,"expected_revision":0,"request_id":"x"}`, `{"symbols":[],"expected_revision":-1,"request_id":"x"}`, `{"symbols":[],"expected_revision":0,"request_id":"x","origin":"human_tty"}`, `{"symbols":[{"symbol":"SYNTH","strike":100}],"expected_revision":0,"request_id":"x"}`} {
		if _, err := s.handleWatchlistMutation(t.Context(), &rpc.Request{Method: rpc.MethodWatchlistReplace, Params: []byte(raw)}); err == nil {
			t.Fatal("accepted invalid request", raw)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.handleWatchlistList(ctx); err == nil {
		t.Fatal("cancelled read became empty")
	}
	if _, err := (&Server{}).handleWatchlistList(t.Context()); err == nil {
		t.Fatal("missing store became empty")
	}
	_, err := replaceWatchlist(t, s, []rpc.WatchlistContract{}, 0, "initial")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	s.dispatch(t.Context(), &rpc.Request{ID: "test", Method: rpc.MethodWatchlistReplace, Params: []byte(`{"symbols":[],"expected_revision":0,"request_id":"stale"}`)}, json.NewEncoder(&output), bufio.NewReader(strings.NewReader("")))
	var response rpc.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Ok || response.Error == nil || response.Error.Code != rpc.CodeWatchlistConflict {
		t.Fatal(output.String(), err)
	}
	if _, err = core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindWatchlist, ExpectedRevision: 1, JSON: []byte(`{"version":1,"symbols":null}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.handleWatchlistList(t.Context()); err == nil {
		t.Fatal("invalid document became accepted empty")
	}
}

func TestWatchlistLostAcknowledgementCannotReapplyAfterRecovery(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	fail := false
	core, err := corestore.Open(t.Context(), corestore.Options{Path: path, CommitObserver: func(corestore.AuthorityHead) error {
		if fail {
			return errors.New("synthetic watermark failure")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	s := &Server{coreStore: core}
	fail = true
	terms := []rpc.WatchlistContract{{Symbol: "SYNTH"}}
	if _, err = replaceWatchlist(t, s, terms, 0, "lost-ack"); err == nil {
		t.Fatal("unaccepted write confirmed")
	}
	if _, err = s.handleWatchlistList(t.Context()); err == nil {
		t.Fatal("latched authority advertised list")
	}
	if _, err = replaceWatchlist(t, s, terms, 0, "lost-ack"); err == nil {
		t.Fatal("latched replay confirmed")
	}
	if err = core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = &Server{coreStore: reopened}
	if _, err = replaceWatchlist(t, s, []rpc.WatchlistContract{}, 1, "later-owner-clear"); err != nil {
		t.Fatal(err)
	}
	replay, err := replaceWatchlist(t, s, terms, 0, "lost-ack")
	if err != nil || !replay.Replay || replay.Revision != 2 || replay.SavedRevision != 1 || len(replay.Symbols) != 0 {
		t.Fatal(replay, err)
	}
}
