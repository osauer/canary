package futu

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTripsThroughTheHeader(t *testing.T) {
	body := []byte("not a protobuf, but any bytes round-trip")
	raw := EncodeFrame(Frame{ProtoID: ProtoQotGetBasicQot, Serial: 42, Body: body})
	if len(raw) != HeaderSize+len(body) {
		t.Fatalf("encoded length %d, want %d", len(raw), HeaderSize+len(body))
	}
	if string(raw[:2]) != "FT" || raw[6] != byte(BodyProtobuf) || raw[7] != 0 {
		t.Fatalf("header prefix %q format %d version %d", raw[:2], raw[6], raw[7])
	}
	if got := binary.LittleEndian.Uint32(raw[2:]); got != ProtoQotGetBasicQot {
		t.Fatalf("protocol id %d", got)
	}
	if got := binary.LittleEndian.Uint32(raw[12:]); got != uint32(len(body)) {
		t.Fatalf("body length %d", got)
	}
	if sum := sha1.Sum(body); !bytes.Equal(raw[16:36], sum[:]) {
		t.Fatal("header SHA-1 is not the body's")
	}
	if !bytes.Equal(raw[36:44], make([]byte, 8)) {
		t.Fatal("reserved bytes are not zero")
	}

	got, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtoID != ProtoQotGetBasicQot || got.Serial != 42 || !bytes.Equal(got.Body, body) {
		t.Fatalf("decoded %+v", got)
	}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{ProtoID: ProtoKeepAlive, Serial: 7}); err != nil {
		t.Fatal(err)
	}
	empty, err := ReadFrame(&buf)
	if err != nil || empty.ProtoID != ProtoKeepAlive || empty.Serial != 7 || len(empty.Body) != 0 {
		t.Fatalf("empty body frame: %+v %v", empty, err)
	}
}

// Every malformed header is an ErrFrame; a short read is an unexpected EOF
// and a clean close before any byte is a plain EOF.
func TestReadFrameRefusesEveryMalformedHeader(t *testing.T) {
	good := EncodeFrame(Frame{ProtoID: ProtoNotify, Serial: 1, Body: []byte("body")})
	mutate := func(f func(raw []byte)) []byte {
		raw := bytes.Clone(good)
		f(raw)
		return raw
	}
	cases := []struct {
		name string
		raw  []byte
		want error
		text string
	}{
		{"bad flag", mutate(func(r []byte) { r[0], r[1] = 'T', 'F' }), ErrFrame, "header flag"},
		{"json body format", mutate(func(r []byte) { r[6] = byte(BodyJSON) }), ErrFrame, "protobuf (0) only"},
		{"length longer than the body", mutate(func(r []byte) { binary.LittleEndian.PutUint32(r[12:], 5) }), ErrFrame, "declares 5 body bytes"},
		{"length above the cap", mutate(func(r []byte) { binary.LittleEndian.PutUint32(r[12:], MaxBodySize+1) }), ErrFrame, "above"},
		{"sha-1 mismatch", mutate(func(r []byte) { r[HeaderSize] ^= 0xff }), ErrFrame, "SHA-1"},
		{"short header", good[:HeaderSize-1], io.ErrUnexpectedEOF, ""},
		{"clean close", nil, io.EOF, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tc.raw))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
			if tc.text != "" && !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error %q does not name %q", err, tc.text)
			}
		})
	}
	// A length shorter than the body reads the declared bytes and refuses
	// on the SHA-1: the remainder is the next frame's problem, not silently
	// swallowed.
	short := mutate(func(r []byte) { binary.LittleEndian.PutUint32(r[12:], 2) })
	if _, err := ReadFrame(bytes.NewReader(short)); !errors.Is(err, ErrFrame) || !strings.Contains(err.Error(), "SHA-1") {
		t.Fatalf("short declared length: %v", err)
	}
}

func TestWriteFrameRefusesABodyAboveTheCap(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, Frame{ProtoID: ProtoQotSub, Body: make([]byte, MaxBodySize+1)})
	if !errors.Is(err, ErrFrame) || buf.Len() != 0 {
		t.Fatalf("oversized body: err %v, %d bytes written", err, buf.Len())
	}
}
