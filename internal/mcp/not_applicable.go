package mcp

import (
	"encoding/json"
	"errors"

	"github.com/osauer/canary/v2/internal/rpc"
)

// notApplicable reports a daemon's answer that this broker does not serve the
// tool's data.
func notApplicable(err error) bool {
	var rpcErr *rpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeNotApplicable
}

// notApplicableResult is a typed result: an AI host must never mistake "off for
// this broker" for "broken".
func notApplicableResult(tool string) json.RawMessage {
	out, _ := json.Marshal(map[string]string{
		"status": rpc.CodeNotApplicable,
		"tool":   tool,
		"reason": "IBKR only: this daemon serves OpenD",
	})
	return out
}
