package apphttp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/app/relay"
)

// net.Pipe has no receive buffer: a peer that stops reading stalls the actual
// net/http writer, including writes through HyperServe's logging middleware.
func TestEventsStreamBoundsStalledWrite(t *testing.T) {
	t.Parallel()
	peer, started, done, cancel, _ := eventsPipe(t)
	defer peer.Close()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("event stream did not begin writing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("stalled SSE write retained the connection after request cancellation and the five-second budget")
	}
}

func TestEventsStreamSurvivesIdleWriteBudget(t *testing.T) {
	t.Parallel()
	peer, _, done, cancel, svc := eventsPipe(t)
	defer peer.Close()
	if err := peer.SetReadDeadline(time.Now().Add(9 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(peer), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("authenticated event stream unavailable")
	}
	scan := bufio.NewScanner(response.Body)
	readEvent := func(want string) {
		t.Helper()
		seen := false
		for scan.Scan() {
			if scan.Text() == "event: "+want {
				seen = true
			}
			if seen && scan.Text() == "" {
				return
			}
		}
		t.Fatalf("missing %s event: %v", want, scan.Err())
	}
	readEvent("snapshot")
	readEvent("alerts")
	// Idle time must not consume the next send's budget or end the stream.
	select {
	case <-done:
		t.Fatal("healthy stream ended before the next update")
	case <-time.After(5100 * time.Millisecond):
	}
	svc.PollOnce(t.Context())
	readEvent("snapshot")
	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("idle stream did not close its connection when the peer stopped reading before cancellation")
	}
}

func eventsPipe(t *testing.T) (net.Conn, <-chan struct{}, <-chan struct{}, context.CancelFunc, *live.Service) {
	t.Helper()
	var svc *live.Service
	h := newTestHandlerWithDependencies(t, routeFakeClient{}, relay.Noop{PublicURL: "https://relay.example"}, func(d *Dependencies) { svc = d.Live }).Handler()
	cookie := routeSessionCookie(t, h)
	serverSide, peer := net.Pipe()
	tracked := &eventsObservedConn{Conn: serverSide, started: make(chan struct{})}
	ln := &eventsPipeListener{conn: tracked, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	srv := &http.Server{
		WriteTimeout: 50 * time.Millisecond,
		BaseContext:  func(net.Listener) context.Context { return ctx },
		Handler:      h,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				close(done)
			}
		},
	}
	t.Cleanup(func() { cancel(); _ = peer.Close(); _ = srv.Close() })
	go func() { _ = srv.Serve(ln) }()
	if _, err := fmt.Fprintf(peer, "GET /api/events HTTP/1.1\r\nHost: stream.test\r\nCookie: %s\r\n\r\n", cookie.String()); err != nil {
		t.Fatal(err)
	}
	return peer, tracked.started, done, cancel, svc
}

type eventsObservedConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *eventsObservedConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

type eventsPipeListener struct {
	conn   net.Conn
	handed bool
	closed chan struct{}
	once   sync.Once
}

func (l *eventsPipeListener) Accept() (net.Conn, error) {
	if !l.handed {
		l.handed = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *eventsPipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *eventsPipeListener) Addr() net.Addr { return l.conn.LocalAddr() }
