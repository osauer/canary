package ibkr

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestEncodeMessageRejectsOpcodeOverflow(t *testing.T) {
	c := &Connection{serverVersion: 100}
	for _, opcode := range []any{int64(math.MaxInt32) + 1, int64(math.MinInt32) - 1, int64(1)<<32 + placeOrder} {
		if got := c.encodeMsg(opcode, "payload"); len(got) != 0 {
			t.Fatalf("out-of-range opcode %v encoded as %x", opcode, got)
		}
	}
	if int64(int(math.MaxInt32)) == math.MaxInt32 && int64(^uint(0)>>1) > math.MaxInt32 {
		wide := int64(math.MaxInt32) + 1
		opcode := int(wide)
		if got := c.encodeMsg(opcode, "payload"); len(got) != 0 {
			t.Fatalf("out-of-range native opcode encoded as %x", got)
		}
	}
	for _, opcode := range []int64{math.MinInt32, placeOrder, math.MaxInt32} {
		got := c.encodeMsg(opcode, int64(1)<<40)
		if len(got) < 4 || int64(int32(binary.BigEndian.Uint32(got))) != opcode || string(got[4:]) != "1099511627776\x00" {
			t.Fatalf("valid opcode or wider payload changed: %x", got)
		}
	}
}

func TestEmptyEncodedMessageNeverReachesBroker(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "bound"}[bound], func(t *testing.T) {
			conn, wire := newReadyWireTestConnection(t)
			var err error
			if bound {
				err = conn.sendMessageWithTypeContext(t.Context(), nil, RequestTypeOrder)
			} else {
				err = conn.sendMessage(nil)
			}
			if err == nil || wire.Len() != 0 || conn.writer.Buffered() != 0 || brokerSendMayHaveBeenWritten(err) {
				t.Fatalf("empty frame err=%v wire=%d buffered=%d", err, wire.Len(), conn.writer.Buffered())
			}
		})
	}
}
