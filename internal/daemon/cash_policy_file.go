package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// CashPolicyFile answers the cash policy methods (policy.cash.get, check and
// apply) for one protection policy file without a broker, through the
// daemon's own handlers: the read, the loader's validation, the write with
// its backup and provenance, the reload and the receipt are the daemon's.
// canarytest/policyfile serves it so a client's tests drive the real methods
// over a temporary file. Facts that need the broker say it is not connected.
type CashPolicyFile struct {
	s    *Server
	core *corestore.Store
}

// OpenCashPolicyFile reads the policy file at policyPath, as the daemon's
// protection policy manager does, and keeps receipts in a state store at
// statePath.
func OpenCashPolicyFile(ctx context.Context, policyPath, statePath string) (*CashPolicyFile, error) {
	core, err := corestore.Open(ctx, corestore.Options{Path: statePath})
	if err != nil {
		return nil, err
	}
	m := newProtectionPolicyManager(policyPath, false, 0, nil)
	m.reload()
	s := &Server{protectionPolicies: m, coreStore: core}
	s.cashPolicyBookForTest = func() cashPolicyBook {
		const why = "no broker is connected"
		return cashPolicyBook{at: time.Now().UTC(), ledgerReason: why, orderCapReason: why, ratesReason: why}
	}
	return &CashPolicyFile{s: s, core: core}, nil
}

// Call answers one cash policy method with its JSON result; a refusal is an
// *rpc.Error carrying the daemon's code.
func (f *CashPolicyFile) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	req := &rpc.Request{Method: method, Params: params}
	var out any
	var err error
	switch method {
	case rpc.MethodCashPolicyGet:
		out, err = f.s.handleCashPolicyGet(ctx, req)
	case rpc.MethodCashPolicyCheck:
		out, err = f.s.handleCashPolicyCheck(ctx, req)
	case rpc.MethodCashPolicyApply:
		out, err = f.s.handleCashPolicyApply(ctx, req)
	default:
		return nil, &rpc.Error{Code: rpc.CodeUnknownMethod, Message: fmt.Sprintf("unknown method: %s", method)}
	}
	if err != nil {
		code, msg := classifyError(err)
		return nil, &rpc.Error{Code: code, Message: msg}
	}
	return json.Marshal(out)
}

// Close closes the state store.
func (f *CashPolicyFile) Close() error {
	return f.core.Close()
}
