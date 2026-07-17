package room

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func next(t *testing.T, c *Client) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	msg, err := c.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return string(msg)
}

func hasPending(c *Client) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue) > 0 || c.state != nil
}

func TestMailboxQueueBeforeStateAndStatesConflate(t *testing.T) {
	c := newClient(8)
	c.setState([]byte("s1"))
	c.enqueue([]byte("welcome"))
	c.setState([]byte("s2"))
	c.enqueue([]byte("error"))
	c.setState([]byte("s3"))

	for _, want := range []string{"welcome", "error", "s3"} {
		if got := next(t, c); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	if hasPending(c) {
		t.Error("superseded states were delivered or kept")
	}
}

func TestMailboxQueueIsBounded(t *testing.T) {
	c := newClient(2)
	if !c.enqueue([]byte("a")) || !c.enqueue([]byte("b")) {
		t.Fatal("enqueue within limit failed")
	}
	if c.enqueue([]byte("c")) {
		t.Error("enqueue past limit succeeded")
	}
	c.setState([]byte("s")) // states never count toward the limit
	next(t, c)
	if !c.enqueue([]byte("c")) {
		t.Error("enqueue failed after the queue drained")
	}
}

func TestMailboxCloseDiscardsPending(t *testing.T) {
	c := newClient(4)
	c.enqueue([]byte("a"))
	c.setState([]byte("s"))
	c.close(ReasonSlow)
	if _, err := c.Next(context.Background()); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("err = %v, want ErrClientClosed", err)
	}
	if c.CloseReason() != ReasonSlow {
		t.Errorf("reason = %v", c.CloseReason())
	}
}

func TestMailboxNextBlocksUntilSignaledOrCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newClient(4)
		got := make(chan string, 1)
		go func() {
			msg, _ := c.Next(context.Background())
			got <- string(msg)
		}()
		synctest.Wait()
		select {
		case m := <-got:
			t.Fatalf("Next returned %q with nothing pending", m)
		default:
		}
		c.setState([]byte("s"))
		if m := <-got; m != "s" {
			t.Errorf("got %q", m)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := c.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want deadline exceeded", err)
		}
	})
}

// A consumer racing a producer sees strictly increasing states and always
// ends on the last one.
func TestMailboxConcurrentProducerConsumer(t *testing.T) {
	const n = 10000
	c := newClient(4)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 1; i <= n; i++ {
			c.setState([]byte(strconv.Itoa(i)))
		}
		c.close(ReasonLeft)
	})

	last := 0
	for {
		msg, err := c.Next(context.Background())
		if errors.Is(err, ErrClientClosed) {
			break
		}
		v, _ := strconv.Atoi(string(msg))
		if v <= last {
			t.Fatalf("state %d after %d", v, last)
		}
		last = v
	}
	wg.Wait()
	// Close discards pending, so the consumer may stop short of n; it must
	// never go backwards, which the loop checked.
	if last == 0 {
		t.Error("consumer saw no states")
	}
}
