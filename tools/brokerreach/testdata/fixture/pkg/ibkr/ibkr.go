// Package ibkr is a synthetic stand-in for Canary's connector: a send that
// runs a wire guard the order-write path installs.
package ibkr

// Connection sends broker frames.
type Connection struct{ sent int }

// Send is a route's send with no guard.
func (c *Connection) Send() error { return c.sendBoundFrames(nil) }

// SendGuarded is the order-write send with its wire guard.
func (c *Connection) SendGuarded(guard func() error) error { return c.sendBoundFrames(guard) }

func (c *Connection) sendBoundFrames(guard func() error) error {
	write := func() error {
		if guard != nil {
			if err := guard(); err != nil {
				return err
			}
		}
		c.sent++
		return nil
	}
	return write()
}
