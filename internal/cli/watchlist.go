package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runWatchlist(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "watchlist")
	spec := fs.String("spec", "", "replace request JSON file, or - for stdin")
	conID := fs.Int("con-id", 0, "known exact underlying ID; zero is unresolved")
	revision := fs.Int64("expected-revision", -1, "revision fence; add/remove otherwise read once")
	id := fs.String("request-id", "", "immutable request ID; add/remove otherwise generate one")
	jsonOut := fs.Bool("json", false, "emit accepted list or explicit conflict JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	op := "list"
	if fs.NArg() > 0 {
		op = fs.Arg(0)
	}
	var out rpc.Watchlist
	var method string
	var params any
	switch op {
	case "list":
		if fs.NArg() > 1 || *spec != "" || *conID != 0 || *revision != -1 || *id != "" {
			return fail(env, "usage: canary watchlist list [--json]")
		}
		method = rpc.MethodWatchlistList
	case "replace":
		if fs.NArg() != 1 || *spec == "" || *conID != 0 || *revision != -1 || *id != "" {
			return fail(env, "usage: canary watchlist replace --spec PATH|- [--json]")
		}
		var reader io.Reader = env.Stdin
		if *spec != "-" {
			file, err := os.Open(*spec)
			if err != nil {
				return fail(env, "watchlist: %v", err)
			}
			defer file.Close()
			reader = file
		}
		if reader == nil {
			return fail(env, "watchlist: spec input unavailable")
		}
		raw, err := io.ReadAll(io.LimitReader(reader, 16385))
		if err != nil || len(raw) > 16384 {
			return fail(env, "watchlist: spec must be at most 16 KiB")
		}
		var wire struct {
			Symbols          []rpc.WatchlistContract `json:"symbols"`
			ExpectedRevision *int64                  `json:"expected_revision"`
			RequestID        string                  `json:"request_id"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err = d.Decode(&wire); err != nil || d.Decode(new(any)) != io.EOF || wire.ExpectedRevision == nil {
			return fail(env, "watchlist: invalid replace request")
		}
		wire.Symbols, err = rpc.NormalizeWatchlistSymbols(wire.Symbols)
		if err != nil {
			return fail(env, "watchlist: %v", err)
		}
		*revision, *id = *wire.ExpectedRevision, wire.RequestID
		method, params = rpc.MethodWatchlistReplace, rpc.WatchlistReplaceRequest{Symbols: wire.Symbols, ExpectedRevision: *revision, RequestID: *id}
	case "add", "remove":
		if fs.NArg() != 2 || *spec != "" || (op == "remove" && *conID != 0) {
			return fail(env, "usage: canary watchlist add|remove SYMBOL [--con-id ID] [--expected-revision N] [--request-id ID] [--json]")
		}
		contract, err := rpc.NormalizeWatchlistContract(rpc.WatchlistContract{Symbol: fs.Arg(1), ConID: *conID})
		if err != nil {
			return fail(env, "watchlist: %v", err)
		}
		if *revision == -1 {
			var current rpc.Watchlist
			if err = env.Conn.Call(ctx, rpc.MethodWatchlistList, nil, &current); err != nil {
				return fail(env, "watchlist: read revision: %v", err)
			}
			*revision = current.Revision
		}
		if *id == "" {
			*id = rand.Text()
		}
		if op == "add" {
			method, params = rpc.MethodWatchlistAdd, rpc.WatchlistAddRequest{Contract: contract, ExpectedRevision: *revision, RequestID: *id}
		} else {
			method, params = rpc.MethodWatchlistRemove, rpc.WatchlistRemoveRequest{Symbol: contract.Symbol, ExpectedRevision: *revision, RequestID: *id}
		}
	default:
		return fail(env, "usage: canary watchlist list|add|remove|replace [--json]")
	}
	if op != "list" {
		if err := rpc.ValidateWatchlistMutation(*revision, *id); err != nil {
			return fail(env, "watchlist: %v", err)
		}
	}
	if err := env.Conn.Call(ctx, method, params, &out); err != nil {
		var rpcErr *rpc.Error
		if *jsonOut && errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeWatchlistConflict {
			_ = printJSON(env, struct {
				Error *rpc.Error `json:"error"`
			}{&rpc.Error{Code: rpc.CodeWatchlistConflict, Message: "Watchlist terms or revision changed; refresh before saving"}})
		}
		if op != "list" {
			return fail(env, "watchlist: %v; retry unchanged terms with expected_revision=%d request_id=%s", err, *revision, *id)
		}
		return fail(env, "watchlist: %v", err)
	}
	if *jsonOut {
		return printJSON(env, out)
	}
	fmt.Fprintf(env.Stdout, "Watchlist · revision %d · %d symbols\n", out.Revision, len(out.Symbols))
	for _, c := range out.Symbols {
		fmt.Fprintf(env.Stdout, "%s\tcontract %d\n", c.Symbol, c.ConID)
	}
	return 0
}
