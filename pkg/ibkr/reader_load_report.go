package ibkr

import "time"

// ReaderLoad is the current connection's reader load: its work over the
// running window and the finished one before it, the per-message time
// included. The zero value when no connection is up.
func (c *Connector) ReaderLoad() ReaderLoad {
	if c == nil {
		return ReaderLoad{}
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return ReaderLoad{}
	}
	return conn.readerLoad.snapshot(time.Now())
}
