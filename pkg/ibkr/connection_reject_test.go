package ibkr

import (
	"errors"
	"net"
	"testing"
	"time"
)

// serveRejecting accepts loopback connections and ends each one with behave
// before reading a byte, until the test ends.
func serveRejecting(t *testing.T, behave func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			behave(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func rejectingConnection(port int) *Connection {
	return NewConnection(&ConnectionConfig{
		Host: "127.0.0.1", Port: port, ClientID: 97,
		ConnectTimeout: 2 * time.Second, MaxClientIDRetries: 1,
		AutoReconnect: false, EnableTLSFallback: false,
	})
}

// A listener that accepts the TCP connection and ends it before the API
// handshake — by FIN or by RST, whichever client step the reset lands on —
// is reported as ErrRejectedBeforeHandshake. A refused dial is not.
func TestConnectReportsAListenerThatEndsTheConnectionBeforeTheHandshake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behave func(net.Conn)
	}{
		{"closes before reading", func(c net.Conn) { _ = c.Close() }},
		{"resets before reading", func(c net.Conn) {
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := serveRejecting(t, tc.behave)
			for i := range 5 {
				conn := rejectingConnection(port)
				err := conn.Connect(t.Context())
				conn.rateLimiter.Stop()
				if !errors.Is(err, ErrRejectedBeforeHandshake) {
					t.Fatalf("attempt %d: %v, want ErrRejectedBeforeHandshake", i+1, err)
				}
				if conn.IsConnected() {
					t.Fatalf("attempt %d reported connected", i+1)
				}
			}
		})
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	conn := rejectingConnection(port)
	err = conn.Connect(t.Context())
	conn.rateLimiter.Stop()
	if err == nil || errors.Is(err, ErrRejectedBeforeHandshake) {
		t.Fatalf("refused dial classified as a rejection: %v", err)
	}
}
