package ibkr

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

// wireTHandshakeConn is a bare connection on one end of a synchronous pipe,
// the shape TestHandshakeContract uses; the test scripts the other end.
func wireTHandshakeConn(t *testing.T) (*Connection, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn := &Connection{
		config: &ConnectionConfig{ClientID: 15},
		conn:   client, reader: bufio.NewReader(client), writer: bufio.NewWriter(client),
	}
	return conn, server
}

// wireTReadDescriptor reads one "API\0" + length-prefixed version descriptor
// from the client and returns the descriptor text.
func wireTReadDescriptor(server net.Conn) (string, error) {
	head := make([]byte, 8)
	if _, err := io.ReadFull(server, head); err != nil {
		return "", err
	}
	if string(head[:4]) != "API\x00" {
		return "", fmt.Errorf("descriptor prefix %q", head[:4])
	}
	n := int(head[4])<<24 | int(head[5])<<16 | int(head[6])<<8 | int(head[7])
	body := make([]byte, n)
	if _, err := io.ReadFull(server, body); err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(body), "\x00"), nil
}

// wireTServeHandshake answers each descriptor the client sends with the
// matching ack and closes the server side afterwards when asked. The
// returned channel carries the descriptors seen, or an error.
func wireTServeHandshake(server net.Conn, acks [][]byte, closeAfterLast bool) <-chan []string {
	done := make(chan []string, 1)
	go func() {
		var seen []string
		for i, ack := range acks {
			descriptor, err := wireTReadDescriptor(server)
			if err != nil {
				seen = append(seen, "error: "+err.Error())
				break
			}
			seen = append(seen, descriptor)
			if len(ack) > 0 {
				if _, err := server.Write(ack); err != nil {
					seen = append(seen, "error: "+err.Error())
					break
				}
			}
			if closeAfterLast && i == len(acks)-1 {
				_ = server.Close()
			}
		}
		done <- seen
	}()
	return done
}

// The v100 (length-prefixed) handshake reply decides the session: the
// negotiated version and connection time land on the connection, and every
// refusal names its reason without touching the version.
func TestLengthPrefixedHandshakeVariants(t *testing.T) {
	for _, tc := range []struct {
		name            string
		ack             []byte
		wantErr         string
		wantVersion     int
		wantTime        string
		wantNoData      bool
		wantDescriptors []string
	}{
		{"version and time", buildHandshakeAck("176", "20261103 10:00:00 CET"), "", 176, "20261103 10:00:00 CET", false, []string{"v100..203"}},
		{"version without time", buildHandshakeAck("176"), "", 176, "", false, []string{"v100..203"}},
		{"below minimum", buildHandshakeAck("123", "20261103 10:00:00 CET"), "server version 123 is too old (minimum: 124)", 0, "", false, []string{"v100..203"}},
		{"redirect with target", buildHandshakeAck("-1", "gateway.example.test:4001"), "handshake redirect requested: gateway.example.test:4001", 0, "", false, []string{"v100..203"}},
		{"redirect without target", buildHandshakeAck("-1"), "handshake redirect requested but no target provided", 0, "", false, []string{"v100..203"}},
		{"length above 4096", []byte{0, 0, 0x10, 0x01}, "handshake frame length out of bounds: 4097", 0, "", false, []string{"v100..203"}},
		{"non-numeric version", buildHandshakeAck("abc", "x"), `invalid server version string "abc"`, 0, "", false, []string{"v100..203"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, server := wireTHandshakeConn(t)
			done := wireTServeHandshake(server, [][]byte{tc.ack}, false)
			err := conn.handshake()
			seen := <-done
			if strings.Join(seen, ",") != strings.Join(tc.wantDescriptors, ",") {
				t.Fatalf("descriptors = %q, want %q", seen, tc.wantDescriptors)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("handshake: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("handshake error = %v, want %q", err, tc.wantErr)
			}
			if errors.Is(err, errHandshakeNoData) != tc.wantNoData {
				t.Fatalf("no-data verdict = %t, want %t (err=%v)", !tc.wantNoData, tc.wantNoData, err)
			}
			if conn.serverVersion != tc.wantVersion || conn.connTime != tc.wantTime {
				t.Fatalf("serverVersion=%d connTime=%q, want %d %q", conn.serverVersion, conn.connTime, tc.wantVersion, tc.wantTime)
			}
		})
	}
}

// A zero-length frame or an empty version field is "no data": the client
// retries once with the single-version descriptor and accepts that answer.
// Two silences in a row end as the no-data verdict, not a rejection.
func TestLengthPrefixedHandshakeNoDataRetriesWithSingleVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first []byte
	}{
		{"zero-length frame", []byte{0, 0, 0, 0}},
		{"empty version field", buildHandshakeAck("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, server := wireTHandshakeConn(t)
			done := wireTServeHandshake(server, [][]byte{tc.first, buildHandshakeAck("176", "20261103 10:00:00 CET")}, false)
			if err := conn.handshake(); err != nil {
				t.Fatalf("handshake after retry: %v", err)
			}
			if seen := <-done; strings.Join(seen, ",") != "v100..203,v203" {
				t.Fatalf("descriptors = %q, want the range then the single version", seen)
			}
			if conn.serverVersion != 176 {
				t.Fatalf("serverVersion = %d, want 176", conn.serverVersion)
			}
		})
	}
	t.Run("silence twice", func(t *testing.T) {
		conn, server := wireTHandshakeConn(t)
		done := wireTServeHandshake(server, [][]byte{{0, 0, 0, 0}, {0, 0, 0, 0}}, false)
		err := conn.handshake()
		<-done
		if !errors.Is(err, errHandshakeNoData) || errors.Is(err, ErrRejectedBeforeHandshake) {
			t.Fatalf("err = %v, want no-data without a rejection", err)
		}
		if conn.serverVersion != 0 {
			t.Fatalf("serverVersion = %d after silence", conn.serverVersion)
		}
	})
}

// The peer hanging up in place of the reply is a rejection before the
// handshake: whichever attempt sees the hang-up (an EOF or reset on the read,
// or a broken pipe on the retry's send), the chain says the listener ended
// the connection and no version is negotiated.
func TestHandshakePeerCloseIsRejectionBeforeHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server := <-accepted
	conn := &Connection{
		config: &ConnectionConfig{ClientID: 15},
		conn:   client, reader: bufio.NewReader(client), writer: bufio.NewWriter(client),
	}
	done := wireTServeHandshake(server, [][]byte{nil}, true)
	err = conn.handshake()
	<-done
	if !errors.Is(err, ErrRejectedBeforeHandshake) {
		t.Fatalf("err = %v, want the rejection before the handshake", err)
	}
	if conn.serverVersion != 0 {
		t.Fatalf("serverVersion = %d after a hang-up", conn.serverVersion)
	}
}

// The ASCII reply (version and time as NUL-terminated strings, no length
// prefix) negotiates the same way; a reply that ends after the version is
// accepted with an empty connection time.
func TestAsciiHandshakeVariants(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ack         []byte
		closeAfter  bool
		wantErr     string
		wantVersion int
		wantTime    string
	}{
		{"version and time", []byte("176\x0020261103 10:00:00 CET\x00"), false, "", 176, "20261103 10:00:00 CET"},
		{"version then close", []byte("176\x00"), true, "", 176, ""},
		{"below minimum", []byte("123\x0020261103 10:00:00 CET\x00"), false, "server version 123 is too old (minimum: 124)", 0, ""},
		{"redirect", []byte("-1\x00gateway.example.test:4001\x00"), false, "handshake redirect requested: gateway.example.test:4001", 0, ""},
		{"non-numeric version", []byte("9x\x00t\x00"), false, `invalid server version string "9x"`, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, server := wireTHandshakeConn(t)
			done := wireTServeHandshake(server, [][]byte{tc.ack}, tc.closeAfter)
			err := conn.handshake()
			if seen := <-done; len(seen) != 1 || seen[0] != "v100..203" {
				t.Fatalf("descriptors = %q", seen)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("handshake: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("handshake error = %v, want %q", err, tc.wantErr)
			}
			if conn.serverVersion != tc.wantVersion || conn.connTime != tc.wantTime {
				t.Fatalf("serverVersion=%d connTime=%q, want %d %q", conn.serverVersion, conn.connTime, tc.wantVersion, tc.wantTime)
			}
		})
	}
}

// The client's descriptor frame is "API\0", a 4-byte big-endian length, then
// the NUL-terminated version range; the retry carries the single version.
func TestHandshakeDescriptorFrameLayout(t *testing.T) {
	conn, server := wireTHandshakeConn(t)
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4+4+len("v100..203")+1)
		_, err := io.ReadFull(server, buf)
		if err != nil {
			buf = nil
		}
		got <- buf
		_ = server.Close()
	}()
	_ = conn.handshake()
	frame := <-got
	if !bytes.Equal(frame, buildHandshakeFrame("v100..203")) {
		t.Fatalf("descriptor frame = % x, want % x", frame, buildHandshakeFrame("v100..203"))
	}
	if frame[7] != byte(len("v100..203")+1) || string(frame[:4]) != "API\x00" {
		t.Fatalf("frame prefix = % x", frame[:8])
	}
}
