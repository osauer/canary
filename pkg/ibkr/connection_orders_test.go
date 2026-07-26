//go:build trading

package ibkr

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPlaceOrderDoesNotSendDoubleMaxSentinels pins the v0.7-era regression
// where unset numeric fields ended up as `MAX_FLOAT` strings on the wire.
// The bundled `canary` binary refuses every order verb at the daemon
// dispatcher, so this is the one wire-shape contract worth keeping for
// library callers (downstream forks, clean-room ports) who may genuinely
// drive Connection.PlaceOrder.
func TestPlaceOrderDoesNotSendDoubleMaxSentinels(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(1)

	var buf bytes.Buffer
	conn.writer = bufio.NewWriter(&buf)

	order := &IBKROrder{
		Symbol:    "MSFT",
		SecType:   "STK",
		Exchange:  "SMART",
		Currency:  "USD",
		Action:    "BUY",
		TotalQty:  50,
		OrderType: "LMT",
		LmtPrice:  330.10,
		TIF:       "DAY",
	}

	require.NoError(t, conn.PlaceOrder(order))
	payload := extractRawPayload(t, &buf)

	if strings.Contains(payload, "1.7976931348623157e+308") {
		t.Fatalf("placeOrder payload should not contain double max sentinel, payload=%q", payload)
	}
}

func extractRawPayload(t *testing.T, buf *bytes.Buffer) string {
	data := buf.Bytes()
	require.GreaterOrEqual(t, len(data), 4)
	msgLen := binary.BigEndian.Uint32(data[:4])
	require.GreaterOrEqual(t, uint32(len(data[4:])), msgLen)
	payload := data[4 : 4+msgLen]
	return string(payload)
}
