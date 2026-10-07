// Package policyfile serves the daemon's own cash policy methods on a
// canarytest fake daemon, over a temporary protection policy file: a client's
// end-to-end tests read, check and write the file exactly as the daemon does,
// and reread it afterwards. Facts that need a broker say none is connected.
package policyfile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/osauer/canary/v2/canarytest"
	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Serve answers policy.cash.get, policy.cash.check and policy.cash.apply on
// s with the daemon's handlers over the policy file at path. Receipts go to a
// private state store that lives as long as the test.
func Serve(t testing.TB, s *canarytest.Server, path string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "policyfile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := daemon.OpenCashPolicyFile(context.Background(), path, filepath.Join(dir, "daemon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	for _, method := range []string{rpc.MethodCashPolicyGet, rpc.MethodCashPolicyCheck, rpc.MethodCashPolicyApply} {
		s.Handle(method, func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
			return f.Call(ctx, method, params)
		})
	}
}
