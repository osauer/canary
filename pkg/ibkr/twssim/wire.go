package twssim

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// The TWS API frames every message with a four-byte big-endian length. From
// server version 100 the message id is a four-byte big-endian integer at the
// head of the body; a text message's fields follow it, each ended by a NUL,
// and a protocol-buffer message's encoded body follows it whole. A
// protocol-buffer message's id is the text message's id plus 200.
const protoOffset = 200

// Request ids the client sends (EClient's outgoing message ids).
const (
	reqMktData          = 1
	cancelMktData       = 2
	placeOrder          = 3
	cancelOrder         = 4
	reqOpenOrders       = 5
	reqAccountUpdates   = 6
	reqExecutions       = 7
	reqIDs              = 8
	reqContractDetails  = 9
	reqAutoOpenOrders   = 15
	reqAllOpenOrders    = 16
	reqManagedAccts     = 17
	reqHistoricalData   = 20
	cancelHistorical    = 25
	reqCurrentTime      = 49
	reqGlobalCancel     = 58
	reqMarketDataType   = 59
	reqPositions        = 61
	reqAccountSummary   = 62
	cancelAccountSumm   = 63
	cancelPositions     = 64
	startAPI            = 71
	reqSecDefOptParams  = 78
	reqMatchingSymbols  = 81
	reqHeadTimestamp    = 87
	cancelHeadTimestamp = 90
	reqMarketRule       = 91
	reqPnL              = 92
	cancelPnL           = 93
	reqPnLSingle        = 94
	cancelPnLSingle     = 95
	reqCompletedOrders  = 99
	reqWSHMetaData      = 100
	cancelWSHMetaData   = 101
	reqWSHEventData     = 102
	cancelWSHEventData  = 103
)

// Message ids the simulator sends (EWrapper's incoming message ids).
const (
	msgTickPrice          = 1
	msgTickSize           = 2
	msgOrderStatus        = 3
	msgError              = 4
	msgOpenOrder          = 5
	msgAccountValue       = 6
	msgPortfolioValue     = 7
	msgAccountTime        = 8
	msgNextValidID        = 9
	msgContractDetails    = 10
	msgExecDetails        = 11
	msgManagedAccounts    = 15
	msgHistoricalData     = 17
	msgCurrentTime        = 49
	msgContractDetailsEnd = 52
	msgOpenOrderEnd       = 53
	msgAccountDownloadEnd = 54
	msgExecDetailsEnd     = 55
	msgTickSnapshotEnd    = 57
	msgMarketDataType     = 58
	msgCommissionReport   = 59
	msgPosition           = 61
	msgPositionEnd        = 62
	msgAccountSummary     = 63
	msgAccountSummaryEnd  = 64
	msgSecDefOptEnd       = 76
	msgSymbolSamples      = 79
	msgHeadTimestamp      = 88
	msgMarketRule         = 93
	msgPnL                = 94
	msgPnLSingle          = 95
	msgCompletedOrdersEnd = 102
	msgHistoricalDataEnd  = 108
)

func readFrame(r *bufio.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > 16<<20 {
		return nil, fmt.Errorf("twssim: frame of %d bytes", size)
	}
	body := make([]byte, size)
	_, err := io.ReadFull(r, body)
	return body, err
}

func appendFrame(dst, body []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(body)))
	return append(dst, body...)
}

// textMessage encodes a text message: the binary id, then every field with
// its NUL.
func textMessage(id int, fields ...string) []byte {
	body := binary.BigEndian.AppendUint32(nil, uint32(id))
	for _, f := range fields {
		body = append(body, f...)
		body = append(body, 0)
	}
	return body
}

// protoMessage encodes a protocol-buffer message under its wire id.
func protoMessage(textID int, body []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(textID+protoOffset)), body...)
}

// splitFields splits a text message's body (after the id) at its NULs.
func splitFields(body []byte) []string {
	s := string(body)
	s = strings.TrimSuffix(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func itoa(v int) string { return strconv.Itoa(v) }

// Protocol-buffer encoding, as far as the messages the simulator exchanges
// need it: varints, 64-bit doubles and length-delimited bytes.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

type protoWriter []byte

func (w protoWriter) key(field, wireType int) protoWriter {
	return binary.AppendUvarint(w, uint64(field<<3|wireType))
}

func (w protoWriter) varint(field int, v int64) protoWriter {
	return binary.AppendUvarint(w.key(field, wireVarint), uint64(v))
}

func (w protoWriter) boolean(field int, v bool) protoWriter {
	if !v {
		return w
	}
	return w.varint(field, 1)
}

func (w protoWriter) double(field int, v float64) protoWriter {
	return binary.LittleEndian.AppendUint64(w.key(field, wireFixed64), math.Float64bits(v))
}

func (w protoWriter) bytes(field int, v []byte) protoWriter {
	return append(binary.AppendUvarint(w.key(field, wireBytes), uint64(len(v))), v...)
}

func (w protoWriter) str(field int, v string) protoWriter {
	if v == "" {
		return w
	}
	return w.bytes(field, []byte(v))
}

// protoField is one decoded field: its number and wire type, the varint or
// fixed value, and the bytes of a length-delimited field.
type protoField struct {
	num, wire int
	u         uint64
	b         []byte
}

func (f protoField) double() float64 { return math.Float64frombits(f.u) }

func decodeProto(body []byte) ([]protoField, error) {
	var out []protoField
	for len(body) > 0 {
		key, n := binary.Uvarint(body)
		if n <= 0 {
			return nil, errors.New("twssim: bad protobuf key")
		}
		body = body[n:]
		f := protoField{num: int(key >> 3), wire: int(key & 7)}
		switch f.wire {
		case wireVarint:
			f.u, n = binary.Uvarint(body)
			if n <= 0 {
				return nil, errors.New("twssim: bad protobuf varint")
			}
			body = body[n:]
		case wireFixed64:
			if len(body) < 8 {
				return nil, errors.New("twssim: short protobuf fixed64")
			}
			f.u, body = binary.LittleEndian.Uint64(body), body[8:]
		case wireFixed32:
			if len(body) < 4 {
				return nil, errors.New("twssim: short protobuf fixed32")
			}
			f.u, body = uint64(binary.LittleEndian.Uint32(body)), body[4:]
		case wireBytes:
			size, n := binary.Uvarint(body)
			if n <= 0 || uint64(len(body)-n) < size {
				return nil, errors.New("twssim: bad protobuf length")
			}
			f.b, body = body[n:n+int(size)], body[n+int(size):]
		default:
			return nil, fmt.Errorf("twssim: protobuf wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out, nil
}
