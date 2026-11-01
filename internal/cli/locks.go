package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// runLocks reads and edits the owner's keep list. A locked name is never
// selected by a bulk reduction or an automatic one, on any of its held
// contracts; it places no order.
func runLocks(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "locks")
	conID := fs.Int("con-id", 0, "contract ID the lock is placed from (optional; the lock covers every contract of the name)")
	role := fs.String("role", rpc.HoldingLockRoleCore, "hedge or core")
	revision := fs.Int64("expected-revision", -1, "revision fence; otherwise read once")
	id := fs.String("request-id", "", "immutable request ID; otherwise generated")
	jsonOut := fs.Bool("json", false, "emit the accepted list or an explicit conflict as JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	op := "list"
	if fs.NArg() > 0 {
		op = fs.Arg(0)
	}
	var out rpc.HoldingLocks
	method, params := rpc.MethodHoldingLocksList, any(nil)
	switch op {
	case "automatic-list":
		if fs.NArg() != 1 || !*jsonOut || *conID != 0 || *revision != -1 || *id != "" {
			return fail(env, "usage: canary locks automatic-list --json")
		}
		params = rpc.HoldingLocksListRequest{AutomaticOnly: true}
	case "automatic-update":
		if fs.NArg() != 1 || env.Stdin == nil || !*jsonOut {
			return fail(env, "usage: canary locks automatic-update --json < request.json")
		}
		raw, err := io.ReadAll(io.LimitReader(env.Stdin, 32769))
		if err != nil || len(raw) > 32768 {
			return fail(env, "locks: automatic update must be at most 32 KiB")
		}
		var in struct {
			Automatic        *rpc.AutomaticLocksUpdate `json:"automatic"`
			ExpectedRevision *int64                    `json:"expected_revision"`
			RequestID        string                    `json:"request_id"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || in.Automatic == nil || in.ExpectedRevision == nil {
			return fail(env, "locks: invalid automatic update")
		}
		if err := rpc.ValidateWatchlistMutation(*in.ExpectedRevision, in.RequestID); err != nil {
			return fail(env, "locks: %v", err)
		}
		*revision, *id = *in.ExpectedRevision, in.RequestID
		// Preserve required-field presence for the daemon's strict decoder.
		method, params = rpc.MethodHoldingLocksReplace, json.RawMessage(raw)
	case "list":
		if fs.NArg() > 1 || *conID != 0 || *revision != -1 || *id != "" {
			return fail(env, "usage: canary locks list [--json]")
		}
	case "lock", "unlock":
		if fs.NArg() != 2 || *conID < 0 || strings.TrimSpace(fs.Arg(1)) == "" {
			return fail(env, "usage: canary locks lock|unlock SYMBOL [--con-id ID] [--role hedge|core] [--expected-revision N] [--request-id ID] [--json]")
		}
		var current rpc.HoldingLocks
		if err := env.Conn.Call(ctx, rpc.MethodHoldingLocksList, nil, &current); err != nil {
			return fail(env, "locks: read current list: %v", err)
		}
		if *revision == -1 {
			*revision = current.Revision
		}
		symbol := strings.ToUpper(strings.TrimSpace(fs.Arg(1)))
		next := slices.DeleteFunc(slices.Clone(current.Locks), func(l rpc.HoldingLock) bool {
			return l.Symbol == symbol || *conID > 0 && l.ConID == *conID
		})
		if op == "lock" {
			next = append(next, rpc.HoldingLock{ConID: *conID, Symbol: fs.Arg(1), Role: *role})
		}
		next, err := rpc.NormalizeHoldingLocks(next)
		if err != nil {
			return fail(env, "locks: %v", err)
		}
		if *id == "" {
			*id = rand.Text()
		}
		if err = rpc.ValidateWatchlistMutation(*revision, *id); err != nil {
			return fail(env, "locks: %v", err)
		}
		method, params = rpc.MethodHoldingLocksReplace, rpc.HoldingLocksReplaceRequest{Locks: next, ExpectedRevision: *revision, RequestID: *id}
	default:
		return fail(env, "usage: canary locks list|lock|unlock [--json]; automatic-list|automatic-update --json")
	}
	if err := env.Conn.Call(ctx, method, params, &out); err != nil {
		var rpcErr *rpc.Error
		if *jsonOut && errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeHoldingLocksConflict {
			_ = printJSON(env, struct {
				Error *rpc.Error `json:"error"`
			}{rpcErr})
		}
		if op != "list" && op != "automatic-list" {
			return fail(env, "locks: %v; retry unchanged terms with expected_revision=%d request_id=%s", err, *revision, *id)
		}
		return fail(env, "locks: %v", err)
	}
	if *jsonOut {
		return printJSON(env, out)
	}
	renderHoldingLocks(env, out)
	return 0
}

func renderHoldingLocks(env *Env, out rpc.HoldingLocks) {
	if len(out.Locks) == 0 {
		fmt.Fprintln(env.Stdout, "Locked holdings · none · every line can be reduced in bulk")
		return
	}
	fmt.Fprintf(env.Stdout, "Locked holdings · %d · every held contract of the name, never selected by a bulk or automatic reduction\n", len(out.Locks))
	for _, l := range out.Locks {
		fmt.Fprintf(env.Stdout, "%s\t%s\t%s\n", l.Symbol, strings.ToLower(l.Role), formatHoldingLockCoverage(l))
	}
}

// formatHoldingLockCoverage says what the lock holds in the book now: the
// contracts it covers, or that none of the name is held, so a lock left
// behind by a roll or a sale is seen for what it is. Without a book read
// it names the contract the lock was placed from, if any.
func formatHoldingLockCoverage(l rpc.HoldingLock) string {
	switch {
	case l.Held == nil && l.ConID > 0:
		return fmt.Sprintf("placed from contract %d", l.ConID)
	case l.Held == nil:
		return "every contract of the name"
	case !*l.Held:
		return "not held · keeps nothing until a line of it is held again"
	}
	ids := make([]string, 0, len(l.Contracts))
	for _, id := range l.Contracts {
		ids = append(ids, fmt.Sprint(id))
	}
	return fmt.Sprintf("%d %s held (%s)", len(l.Contracts), pluralWord(len(l.Contracts), "contract", "contracts"), strings.Join(ids, ", "))
}
