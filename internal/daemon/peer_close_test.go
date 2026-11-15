package daemon

import (
	"bufio"
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A client that gives up on Add discovery closes its socket. The daemon's
// broker work for that request must stop instead of running on behind the
// next request.
func TestCancelOnPeerCloseStopsWorkWhenTheClientLeaves(t *testing.T) {
	server, client := unixPair(t)
	r := bufio.NewReader(server)
	ctx, stop := cancelOnPeerClose(context.Background(), server, r)
	defer stop()
	_ = client.Close()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("request context survived its client closing the connection")
	}
}

func TestCancelOnPeerCloseKeepsTheConnectionForTheNextRequest(t *testing.T) {
	server, client := unixPair(t)
	r := bufio.NewReader(server)
	ctx, stop := cancelOnPeerClose(context.Background(), server, r)
	time.Sleep(50 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("an open, quiet client cancelled its request")
	}
	stop()
	if _, err := client.Write([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := r.ReadString('\n')
	if err != nil || line != "next\n" {
		t.Fatalf("connection unusable after the request: %q %v", line, err)
	}
}

// Only read-only discovery is cancelled on disconnect. An order send must
// never be cut short because its caller went away.
func TestOnlyAddDiscoveryCancelsOnPeerClose(t *testing.T) {
	if len(cancelOnPeerCloseMethods) != 1 || !cancelOnPeerCloseMethods[rpc.MethodAddMarket] {
		t.Fatalf("cancel-on-close methods = %v, want only %s", cancelOnPeerCloseMethods, rpc.MethodAddMarket)
	}
}

func unixPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	// macOS limits socket paths to 104 bytes; t.TempDir is too long.
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := dir + "/s"
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return server, client
}
