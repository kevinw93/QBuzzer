package room

// CloseReason is why a room closed a client's Outbound channel.
type CloseReason int

const (
	ReasonLeft       CloseReason = iota
	ReasonSlow                   // outbound buffer was full; the client should reconnect
	ReasonRoomClosed             // idle timeout or server shutdown
	ReasonReplaced               // the same session joined on another connection
)

func (r CloseReason) String() string {
	switch r {
	case ReasonLeft:
		return "left"
	case ReasonSlow:
		return "slow"
	case ReasonRoomClosed:
		return "room closed"
	case ReasonReplaced:
		return "replaced"
	default:
		return "unknown"
	}
}

// Client is a room's outbound mailbox for one connection. The room is the only
// sender on send and the only one that closes it.
type Client struct {
	send   chan []byte
	reason CloseReason
}

// Outbound yields encoded messages and is closed when the room drops the client.
func (c *Client) Outbound() <-chan []byte { return c.send }

// CloseReason is valid only after a receive from Outbound has observed the
// channel closed.
func (c *Client) CloseReason() CloseReason { return c.reason }
