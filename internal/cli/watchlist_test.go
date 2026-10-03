package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type watchlistCLIConn struct {
	methods []string
	params  any
	err     error
}

func (c *watchlistCLIConn) Call(_ context.Context, method string, params, out any) error {
	c.methods = append(c.methods, method)
	c.params = params
	if method != rpc.MethodWatchlistList && c.err != nil {
		return c.err
	}
	*out.(*rpc.Watchlist) = rpc.Watchlist{Version: 1, Revision: 7, Symbols: []rpc.WatchlistContract{}}
	return nil
}
func (*watchlistCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestWatchlistCLIReplaceStrictStdinAndSingleCAS(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &watchlistCLIConn{}
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: c, Stdin: strings.NewReader(`{"symbols":[],"expected_revision":0,"request_id":"initial-empty"}`)}
	if code := Run(t.Context(), env, "watchlist", []string{"replace", "--spec", "-", "--json"}); code != 0 {
		t.Fatal(code, stderr.String())
	}
	in, ok := c.params.(rpc.WatchlistReplaceRequest)
	if !ok || in.ExpectedRevision != 0 || in.Symbols == nil || in.RequestID != "initial-empty" || !reflect.DeepEqual(c.methods, []string{rpc.MethodWatchlistReplace}) {
		t.Fatal(c)
	}
	if !strings.Contains(stdout.String(), `"symbols": []`) {
		t.Fatal(stdout.String())
	}
	for _, raw := range []string{`{}`, `{"symbols":[],"request_id":"no-revision"}`, `{"symbols":null,"expected_revision":0,"request_id":"x"}`, `{"symbols":[],"expected_revision":0,"request_id":"x","origin":"human_tty"}`, `{"symbols":[],"expected_revision":0,"request_id":"x"} {}`, strings.Repeat(" ", 16385)} {
		c.methods = nil
		env.Stdin = strings.NewReader(raw)
		if Run(t.Context(), env, "watchlist", []string{"replace", "--spec", "-", "--json"}) == 0 || len(c.methods) != 0 {
			t.Fatal("invalid spec reached daemon", raw)
		}
	}
}

func TestWatchlistCLIConvenienceDoesNotRetryWrites(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &watchlistCLIConn{err: errors.New("synthetic lost acknowledgement")}
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: c}
	if Run(t.Context(), env, "watchlist", []string{"add", "synth", "--con-id", "17", "--json"}) == 0 {
		t.Fatal("write failure hidden")
	}
	if !reflect.DeepEqual(c.methods, []string{rpc.MethodWatchlistList, rpc.MethodWatchlistAdd}) || stdout.Len() != 0 {
		t.Fatal(c.methods, stdout.String())
	}
	in := c.params.(rpc.WatchlistAddRequest)
	if in.Contract.Symbol != "SYNTH" || in.Contract.ConID != 17 || in.ExpectedRevision != 7 || in.RequestID == "" || !strings.Contains(stderr.String(), in.RequestID) {
		t.Fatal(in, stderr.String())
	}
	c.methods = nil
	c.err = nil
	stdout.Reset()
	if Run(t.Context(), env, "watchlist", []string{"remove", "SYNTH", "--expected-revision", "7", "--request-id", "remove-id", "--json"}) != 0 {
		t.Fatal(stderr.String())
	}
	if !reflect.DeepEqual(c.methods, []string{rpc.MethodWatchlistRemove}) {
		t.Fatal(c.methods)
	}
}

func TestWatchlistCLIOnlyExplicitRPCConflictGetsErrorEnvelope(t *testing.T) {
	for _, failure := range []error{&rpc.Error{Code: rpc.CodeWatchlistConflict, Message: "untrusted detail"}, errors.New("watchlist_conflict"), &rpc.Error{Code: rpc.CodeInternal, Message: "unavailable"}} {
		var stdout, stderr bytes.Buffer
		c := &watchlistCLIConn{err: failure}
		env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: c, Stdin: strings.NewReader(`{"symbols":[],"expected_revision":0,"request_id":"id"}`)}
		if Run(t.Context(), env, "watchlist", []string{"replace", "--spec", "-", "--json"}) == 0 {
			t.Fatal("failed mutation succeeded")
		}
		var typed *rpc.Error
		if errors.As(failure, &typed) && typed.Code == rpc.CodeWatchlistConflict {
			var wire struct {
				Error *rpc.Error `json:"error"`
			}
			if json.Unmarshal(stdout.Bytes(), &wire) != nil || wire.Error == nil || wire.Error.Code != rpc.CodeWatchlistConflict || strings.Contains(stdout.String(), "untrusted") {
				t.Fatal(stdout.String())
			}
		} else if stdout.Len() != 0 {
			t.Fatal("unknown outcome invented conflict", stdout.String())
		}
	}
}
