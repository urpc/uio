package uws

// UIO connection tasks serialize the complete protocol and application
// callback path for one connection. UWS therefore invokes handlers directly:
// no second mailbox, payload copy, or runner state is required here.
func (c *Conn) notifyOpen() {
	state := c.handshake.Load()
	defer c.releaseHandshakeState(state)
	if handler := c.handler; handler != nil {
		handler.OnOpen(c)
	}
}

func (c *Conn) notifyMessage(message Message) error {
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	if handler := c.handler; handler != nil {
		handler.OnMessage(c, message)
	}
	return nil
}

func (c *Conn) notifyClose(info CloseEvent) {
	if handler := c.handler; handler != nil {
		handler.OnClose(c, info)
	}
}
