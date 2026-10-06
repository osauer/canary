//go:build trading

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// LevelingBundleContract is the trading rig's two-conversion repayment as
// Desk reaches it: through the canary CLI, answered by the daemon's own
// request dispatch. It exists for the Desk-shaped contract test in package
// daemon_test, which may import the CLI.
type LevelingBundleContract struct {
	rig    *automaticTestRig
	broker *brokerCallLog
	// BundleID and Revision name the served repayment.
	BundleID, Revision string
}

// NewLevelingBundleContract serves the repayment of levelingBundleRig.
func NewLevelingBundleContract(t *testing.T) *LevelingBundleContract {
	rig, _, bundle, broker, _ := levelingBundleRig(t)
	return &LevelingBundleContract{rig: rig, broker: broker, BundleID: bundle.ID, Revision: bundle.Revision}
}

// Call answers one request through the daemon's dispatch, as the socket
// does: the params are encoded, the response envelope decoded.
func (c *LevelingBundleContract) Call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	c.rig.server.dispatch(ctx, &rpc.Request{ID: "contract", Method: method, Params: raw}, json.NewEncoder(&buf), bufio.NewReader(strings.NewReader("")))
	var resp rpc.Response
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		return err
	}
	if !resp.Ok {
		if resp.Error == nil {
			return errors.New("daemon refused without an error")
		}
		return resp.Error
	}
	return json.Unmarshal(resp.Result, out)
}

// Stream is never used by the bundle commands.
func (c *LevelingBundleContract) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("unexpected stream")
}

// Orders counts the orders the fake broker received.
func (c *LevelingBundleContract) Orders() int { return c.broker.count() }
