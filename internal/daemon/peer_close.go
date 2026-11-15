package daemon

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// cancelOnPeerCloseMethods are read-only requests whose broker work stops
// when the client leaves. serveConn reads nothing while a handler runs, so
// without a watch an abandoned discovery keeps the broker busy behind the
// next one. Order methods are never listed: a send is not cut short because
// its caller went away.
var cancelOnPeerCloseMethods = map[string]bool{rpc.MethodAddMarket: true}

// cancelOnPeerClose returns a context cancelled when the client closes the
// connection. stop ends the watch and leaves r and conn ready for the next
// request; it does not consume any bytes the client sends meanwhile.
func cancelOnPeerClose(parent context.Context, conn net.Conn, r *bufio.Reader) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.Peek(1); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			cancel()
		}
	}()
	return ctx, func() {
		_ = conn.SetReadDeadline(time.Now())
		<-done
		_ = conn.SetReadDeadline(time.Time{})
		cancel()
	}
}
