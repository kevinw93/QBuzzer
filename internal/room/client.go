package room

import (
	"context"
	"errors"
	"sync"
)

// CloseReason is why a room closed a client.
type CloseReason int

const (
	ReasonLeft       CloseReason = iota
	ReasonSlow                   // message queue overflowed; the client should reconnect
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

// ErrClientClosed is returned by Client.Next once the room has closed the
// client.
var ErrClientClosed = errors.New("client closed")

// Client is a room's outbound mailbox for one connection. The room goroutine
// is the only producer; the connection's writer is the only consumer.
//
// States are conflated: a pending RoomState is replaced by a newer one, since
// each is a complete snapshot. Other messages (welcome, errors, effect events)
// are queued in order, and the queue is bounded: overflowing it is how the
// room detects a client that cannot keep up. Producers never wait on I/O; the
// mutex is only held for slice operations.
type Client struct {
	notify chan struct{} // capacity 1; signaled whenever something becomes pending
	limit  int

	mu     sync.Mutex
	queue  [][]byte
	state  []byte // latest undelivered state, nil if none
	closed bool
	reason CloseReason
}

func newClient(limit int) *Client {
	return &Client{notify: make(chan struct{}, 1), limit: limit}
}

// Next blocks until a message is pending, the client is closed
// (ErrClientClosed), or ctx is done. Queued messages are returned before the
// pending state, so an effect event always precedes the state it refers to.
// Closing discards anything still pending.
func (c *Client) Next(ctx context.Context) ([]byte, error) {
	for {
		c.mu.Lock()
		switch {
		case c.closed:
			c.mu.Unlock()
			return nil, ErrClientClosed
		case len(c.queue) > 0:
			msg := c.queue[0]
			c.queue[0] = nil
			c.queue = c.queue[1:]
			c.mu.Unlock()
			return msg, nil
		case c.state != nil:
			msg := c.state
			c.state = nil
			c.mu.Unlock()
			return msg, nil
		}
		c.mu.Unlock()

		select {
		case <-c.notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// CloseReason is valid once Next has returned ErrClientClosed.
func (c *Client) CloseReason() CloseReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

// enqueue appends msg, or returns false without queuing if the queue is full.
func (c *Client) enqueue(msg []byte) bool {
	c.mu.Lock()
	if len(c.queue) >= c.limit {
		c.mu.Unlock()
		return false
	}
	c.queue = append(c.queue, msg)
	c.mu.Unlock()
	c.signal()
	return true
}

// setState replaces any undelivered state with msg.
func (c *Client) setState(msg []byte) {
	c.mu.Lock()
	c.state = msg
	c.mu.Unlock()
	c.signal()
}

func (c *Client) close(reason CloseReason) {
	c.mu.Lock()
	c.closed, c.reason = true, reason
	c.queue, c.state = nil, nil
	c.mu.Unlock()
	c.signal()
}

func (c *Client) signal() {
	select {
	case c.notify <- struct{}{}:
	default: // a wakeup is already pending; Next rechecks everything
	}
}
