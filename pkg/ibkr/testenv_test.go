package ibkr

import (
	"bufio"
	"bytes"
	"sync"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func (s *safeBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

// setServerVersionReady configures a connection with the given server version
// and marks both the handshake and broker ID namespace ready so unit tests can
// exercise outbound encoding paths without recreating the async startup frames.
func setServerVersionReady(c *Connection, version int) {
	c.serverVersion = version
	c.signalHandshakeReady()
	c.observeNextValidOrderID(1)
}

func newBufferedSafeWriter(socket *safeBuffer) *bufio.Writer {
	return bufio.NewWriter(socket)
}
