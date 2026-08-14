package limit

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestBurstThenRefill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New(60, 3) // 1 per second sustained, 3 at once
		for i := range 3 {
			if !l.Allow("a") {
				t.Fatalf("event %d within burst refused", i)
			}
		}
		if l.Allow("a") {
			t.Fatal("event past burst allowed")
		}
		if !l.Allow("b") {
			t.Fatal("keys share a bucket")
		}
		time.Sleep(999 * time.Millisecond)
		if l.Allow("a") {
			t.Fatal("refilled too early")
		}
		time.Sleep(time.Millisecond)
		if !l.Allow("a") {
			t.Fatal("did not refill after one second")
		}
		time.Sleep(time.Hour)
		for range 3 {
			if !l.Allow("a") {
				t.Fatal("refill exceeded burst? or did not refill")
			}
		}
		if l.Allow("a") {
			t.Fatal("bucket grew past burst while idle")
		}
	})
}

func TestIdleKeysAreForgotten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New(60, 3)
		for _, k := range []string{"a", "b", "c"} {
			l.Allow(k)
		}
		time.Sleep(2 * time.Minute)
		l.Allow("d") // triggers a sweep
		if n := l.size(); n != 1 {
			t.Errorf("buckets = %d, want only the active key", n)
		}
	})
}
