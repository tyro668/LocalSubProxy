package client

// SetHandshakeHook installs a hook run between hello and welcome (tests only).
func SetHandshakeHook(f func()) { handshakeHook = f }

// Concurrency reports (declared, effective) limits.
func (c *Client) Concurrency() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wantConc, c.maxConc
}

// SetAnnounceHook installs a hook run between building and sending a models.update.
func SetAnnounceHook(f func([]byte)) { announceHook = f }

// OutboxWaiting reports sends queued for flow credit.
func (c *Client) OutboxWaiting() int {
	c.mu.Lock()
	o := c.out
	c.mu.Unlock()
	return o.Waiting()
}
