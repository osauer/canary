package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

type exerciseCLIConn struct {
	method   string
	params   json.RawMessage
	calls    int
	response any
	err      error
}

func (c *exerciseCLIConn) Call(_ context.Context, method string, in, out any) error {
	c.calls++
	c.method = method
	c.params, _ = json.Marshal(in)
	if c.err != nil {
		return c.err
	}
	data, err := json.Marshal(c.response)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
func (*exerciseCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("unexpected stream")
}

func TestExercisesCLIReadAndExactHumanReconciliation(t *testing.T) {
	for _, tc := range []struct {
		args           []string
		method, origin string
		response       any
	}{
		{[]string{"exercises", "--json"}, "opportunities.exercises", rpc.OrderOriginAgent, map[string]any{"requests": []any{}}},
		{[]string{"reconcile-exercise", "synthetic-request", "revision-one", "--evidence", "synthetic-evidence", "--confirm-complete", "--json"}, "opportunities.reconcile_exercise", rpc.OrderOriginHumanTTY, map[string]any{"accepted": true, "request_ref": "synthetic-request", "revision": "revision-one", "evidence_id": "synthetic-evidence", "provenance": "human-attested broker evidence"}},
	} {
		c := &exerciseCLIConn{response: tc.response}
		var out, stderr bytes.Buffer
		code := Run(t.Context(), &Env{Conn: c, Stdout: &out, Stderr: &stderr, Origin: tc.origin}, "opportunities", tc.args)
		if code != 0 || c.calls != 1 || c.method != tc.method {
			t.Fatalf("%v: exit=%d method=%s calls=%d stderr=%s", tc.args, code, c.method, c.calls, &stderr)
		}
		if tc.origin == rpc.OrderOriginHumanTTY {
			var got map[string]any
			if err := json.Unmarshal(c.params, &got); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]any{"request_ref": "synthetic-request", "revision": "revision-one", "evidence_id": "synthetic-evidence", "confirm_complete": true, "origin": rpc.OrderOriginHumanTTY} {
				if got[key] != want {
					t.Fatalf("%s=%v want %v", key, got[key], want)
				}
			}
		}
	}
}

func TestExerciseReconciliationCLIRejectsMissingAuthorityBeforeRPC(t *testing.T) {
	valid := []string{"reconcile-exercise", "synthetic-request", "revision-one", "--evidence", "synthetic-evidence", "--confirm-complete"}
	for _, origin := range []string{"", rpc.OrderOriginAgent, rpc.OrderOriginHumanTTY} {
		cases := [][]string{valid[:len(valid)-1], {"reconcile-exercise", "synthetic-request", "--evidence", "synthetic-evidence", "--confirm-complete"}, {"reconcile-exercise", "synthetic-request", "revision-one", "--confirm-complete"}, append(append([]string{}, valid...), "unexpected")}
		if origin != rpc.OrderOriginHumanTTY {
			cases = append(cases, valid)
		}
		for _, args := range cases {
			c := &exerciseCLIConn{}
			var out bytes.Buffer
			if code := Run(t.Context(), &Env{Conn: c, Stdout: &out, Stderr: &out, Origin: origin}, "opportunities", args); code == 0 || c.calls != 0 {
				t.Fatalf("origin %q args=%v exit=%d calls=%d", origin, args, code, c.calls)
			}
		}
	}
}

func TestExerciseReconciliationCLIRefusalDoesNotReportCompletion(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		c := &exerciseCLIConn{err: &rpc.Error{Code: "exercise_evidence_unavailable", Message: "No complete broker evidence; request remains pending"}}
		var out bytes.Buffer
		args := []string{"reconcile-exercise", "synthetic-request", "revision-one", "--evidence", "synthetic-evidence", "--confirm-complete"}
		if jsonOut {
			args = append(args, "--json")
		}
		if code := Run(t.Context(), &Env{Conn: c, Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginHumanTTY}, "opportunities", args); code == 0 || c.calls != 1 || !strings.Contains(out.String(), "remains pending") {
			t.Fatalf("exit=%d calls=%d output=%s", code, c.calls, &out)
		}
	}
}

func TestExerciseReconciliationCLIHelpNeedsNoConnectionOrAuthority(t *testing.T) {
	for _, sub := range []string{"exercises", "reconcile-exercise"} {
		var out bytes.Buffer
		if code := Run(t.Context(), &Env{Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginAgent}, "opportunities", []string{sub, "--help"}); code != 0 || !strings.Contains(out.String(), "canary opportunities "+sub) {
			t.Fatalf("%s: exit=%d output=%s", sub, code, &out)
		}
	}
}

func TestExerciseReconciliationCLIUnacceptedResultIsFailure(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		c := &exerciseCLIConn{response: rpc.ExerciseReconcileResult{Accepted: false, RequestRef: "synthetic-request"}}
		var out bytes.Buffer
		args := []string{"reconcile-exercise", "synthetic-request", "revision-one", "--evidence", "synthetic-evidence", "--confirm-complete"}
		if jsonOut {
			args = append(args, "--json")
		}
		code := Run(t.Context(), &Env{Conn: c, Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginHumanTTY}, "opportunities", args)
		if code == 0 || c.calls != 1 || strings.Contains(out.String(), "reconciliation recorded") {
			t.Fatalf("exit=%d calls=%d output=%s", code, c.calls, &out)
		}
		if jsonOut {
			var result rpc.ExerciseReconcileResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Accepted || result.RequestRef != "synthetic-request" {
				t.Fatalf("refusal response lost: %s", &out)
			}
		}
	}
}

func goldenPendingExercises() rpc.ExercisePendingResult {
	return rpc.ExercisePendingResult{AsOf: goldenAt, Requests: []rpc.ExercisePendingRequest{
		{RequestRef: "synthetic-request", Revision: "revision-one", State: "pending", Symbol: "SYNA", Quantity: 2,
			Option:     rpc.ContractParams{ConID: 4242, Symbol: "SYNA", SecType: "OPT", Expiry: "20261218", Strike: 40, Right: "P"},
			Underlying: rpc.ContractParams{ConID: 4243, Symbol: "SYNA", SecType: "STK"}, SubmittedAt: goldenAt,
			Evidence: []rpc.ExerciseEvidenceCandidate{{ID: "synthetic-evidence", At: goldenAt.Add(60 * time.Minute)}}},
		{RequestRef: "synthetic-legacy-request", Revision: "revision-two", State: "pending",
			Blockers: []rpc.TradingBlocker{{Code: "exercise_metadata_missing", Message: "Request metadata is unavailable; its outcome remains unresolved."}}},
	}}
}

func goldenExerciseResolution(replay bool) rpc.ExerciseReconcileResult {
	return rpc.ExerciseReconcileResult{Accepted: true, Replay: replay, RequestRef: "synthetic-request", Revision: "revision-one", EvidenceID: "synthetic-evidence", Provenance: "human-attested broker evidence", AsOf: goldenAt}
}
