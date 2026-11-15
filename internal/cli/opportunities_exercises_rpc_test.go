package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/dial"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Use the same Unix socket boundary as the CLI, with synthetic daemon replies.
// This complements handler tests without starting a daemon or reaching IBKR.
func TestExerciseReconciliationRPCPreservesTermsAndRefusesFailedReply(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost_reply_%t", lost), func(t *testing.T) {
			socketPath := filepath.Join("/tmp", fmt.Sprintf("canary-exercise-%d.sock", time.Now().UnixNano()))
			ln, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(socketPath) })
			requests := make(chan rpc.Request, 1)
			serverErrors := make(chan error, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					serverErrors <- err
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				var req rpc.Request
				if err := json.NewDecoder(c).Decode(&req); err != nil {
					serverErrors <- err
					return
				}
				requests <- req
				if lost {
					serverErrors <- nil
					return
				}
				serverErrors <- json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: false, Error: &rpc.Error{Code: rpc.CodeBadRequest, Message: "evidence is missing, reused, partial, ambiguous, or does not exactly match this exercise; reservation remains pending"}})
			}()
			conn, err := dial.Connect(socketPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			var stdout, stderr bytes.Buffer
			args := []string{"reconcile-exercise", "synthetic-request", "revision-one", "--evidence", "synthetic-evidence", "--confirm-complete", "--json"}
			code := Run(t.Context(), &Env{Conn: conn, Stdout: &stdout, Stderr: &stderr, Origin: rpc.OrderOriginHumanTTY}, "opportunities", args)
			if err := <-serverErrors; err != nil {
				t.Fatal(err)
			}
			req := <-requests
			var got rpc.ExerciseReconcileParams
			if err := json.Unmarshal(req.Params, &got); err != nil {
				t.Fatal(err)
			}
			want := rpc.ExerciseReconcileParams{RequestRef: "synthetic-request", Revision: "revision-one", EvidenceID: "synthetic-evidence", ConfirmComplete: true, Origin: rpc.OrderOriginHumanTTY}
			if req.ID == "" || req.Method != rpc.MethodOpportunitiesReconcileExercise || got != want {
				t.Fatalf("wire request lost reviewed terms: method=%s id=%s params=%+v", req.Method, req.ID, got)
			}
			if code == 0 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("failed reply reported success: exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
			if !lost && !strings.Contains(stderr.String(), "reservation remains pending") {
				t.Fatalf("daemon refusal lost: %s", &stderr)
			}
		})
	}
}
