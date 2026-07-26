package daemonclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestReconciliationClientUsesTypedMethodsAndExactParams(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 6, 30, 0, 0, time.UTC)
	current := reconciliationClientCurrentStatus(now)
	checking := rpc.ReconAutomationStatus{
		Report: rpc.ReconFetchStatus{
			Configured: true, State: rpc.ReconReportStateChecking, ExpectedCoverageTo: now.AddDate(0, 0, -1),
			LastAttempt: now, RetryAutomatic: true, Busy: true,
		},
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateChecking, Reason: rpc.ReconEvaluationReasonReportPending},
	}
	statusRaw, err := json.Marshal(rpc.ReconStatusResult{Status: current})
	if err != nil {
		t.Fatal(err)
	}
	checkRaw, err := json.Marshal(rpc.ReconCheckResult{Outcome: rpc.ReconCheckOutcomeStarted, Status: checking})
	if err != nil {
		t.Fatal(err)
	}
	path, done := serveReconciliationDaemon(t, []reconciliationDaemonReply{
		{method: rpc.MethodReconStatus, result: statusRaw},
		{method: rpc.MethodReconCheck, result: checkRaw},
	})
	client := Real{SocketPath: path}

	status, err := client.ReconcileStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status == nil || status.Status.Report.State != rpc.ReconReportStateCurrent || status.Status.Evaluation.State != rpc.ReconEvaluationStateComplete {
		t.Fatalf("status=%+v", status)
	}
	check, err := client.ReconcileCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if check == nil || check.Outcome != rpc.ReconCheckOutcomeStarted || !check.Status.Report.Busy {
		t.Fatalf("check=%+v", check)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationClientRejectsInvalidTypedResults(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 6, 30, 0, 0, time.UTC)
	validStatus := reconciliationClientCurrentStatus(now)
	validStatusRaw, err := json.Marshal(validStatus)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		method string
		result string
		call   func(Real) (bool, error)
	}{
		{
			name:   "status with unknown state",
			method: rpc.MethodReconStatus,
			result: `{"status":{"report":{"state":"private_daemon_state","busy":false,"retry_automatic":false,"can_check_now":false},"evaluation":{"state":"waiting","reason":"report_pending"}}}`,
			call: func(client Real) (bool, error) {
				result, err := client.ReconcileStatus(context.Background())
				return result != nil, err
			},
		},
		{
			name:   "check with unknown outcome",
			method: rpc.MethodReconCheck,
			result: fmt.Sprintf(`{"outcome":"started_and_signed_off","status":%s}`, validStatusRaw),
			call: func(client Real) (bool, error) {
				result, err := client.ReconcileCheck(context.Background())
				return result != nil, err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, done := serveReconciliationDaemon(t, []reconciliationDaemonReply{{method: tc.method, result: json.RawMessage(tc.result)}})
			gotResult, err := tc.call(Real{SocketPath: path})
			if err == nil || gotResult {
				t.Fatalf("has_result=%t err=%v, want rejected typed result", gotResult, err)
			}
			if !strings.Contains(err.Error(), "recon.") {
				t.Fatalf("error=%q, want method context", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

type reconciliationDaemonReply struct {
	method string
	result json.RawMessage
}

func serveReconciliationDaemon(t *testing.T, replies []reconciliationDaemonReply) (string, <-chan error) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ibkr-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		defer close(done)
		for _, reply := range replies {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			var request rpc.Request
			if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			if request.Method != reply.method {
				_ = conn.Close()
				done <- fmt.Errorf("method=%q, want %q", request.Method, reply.method)
				return
			}
			if !bytes.Equal(bytes.TrimSpace(request.Params), []byte("{}")) {
				_ = conn.Close()
				done <- fmt.Errorf("params=%s, want exact empty object", request.Params)
				return
			}
			response := rpc.Response{ID: request.ID, Ok: true, Result: reply.result}
			if err := json.NewEncoder(conn).Encode(response); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			if err := conn.Close(); err != nil {
				done <- err
				return
			}
		}
	}()
	return path, done
}

func reconciliationClientCurrentStatus(now time.Time) rpc.ReconAutomationStatus {
	return rpc.ReconAutomationStatus{
		Report: rpc.ReconFetchStatus{
			Configured: true, State: rpc.ReconReportStateCurrent,
			ExpectedCoverageTo: now.AddDate(0, 0, -1), CoverageTo: now.AddDate(0, 0, -1),
			LastSuccess: now, LastAttempt: now, RetryAutomatic: true, CanCheckNow: true,
		},
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateComplete},
	}
}
