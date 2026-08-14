package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/kevinw93/QBuzzer/internal/limit"
	"github.com/kevinw93/QBuzzer/internal/metrics"
)

func postAs(t *testing.T, e *env, ip string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.ts.URL+"/api/rooms", strings.NewReader(`{"moderatorPassword":"hunter22"}`))
	if ip != "" {
		req.Header.Set("Fly-Client-IP", ip)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	return resp.StatusCode
}

func TestCreateRoomRateLimit(t *testing.T) {
	t.Run("per trusted client IP", func(t *testing.T) {
		e := newEnvOpts(t, nil, Options{CreateLimit: limit.New(2, 2), ClientIPHeader: "Fly-Client-IP"})
		for i, want := range []int{201, 201, 429} {
			if got := postAs(t, e, "203.0.113.7"); got != want {
				t.Fatalf("request %d: %d, want %d", i, got, want)
			}
		}
		if got := postAs(t, e, "198.51.100.9"); got != 201 {
			t.Errorf("another client was limited: %d", got)
		}
	})
	t.Run("an untrusted header can't dodge the limit", func(t *testing.T) {
		e := newEnvOpts(t, nil, Options{CreateLimit: limit.New(2, 2)}) // no proxy header configured
		postAs(t, e, "1.1.1.1")
		postAs(t, e, "2.2.2.2")
		if got := postAs(t, e, "3.3.3.3"); got != 429 {
			t.Errorf("forged header got a fresh bucket: %d", got)
		}
	})
}

func TestJoinRateLimitAndConnectionGauge(t *testing.T) {
	e := newEnvOpts(t, nil, Options{JoinLimit: limit.New(1, 1)})
	code := e.createRoom(t).Code
	before := metrics.Connections.Load()
	conn, _ := e.join(t, code, player("Alice", ""))
	if got := metrics.Connections.Load(); got != before+1 {
		t.Errorf("connections = %d, want %d", got, before+1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c2, resp, err := websocket.Dial(ctx, e.wsURL(code), nil)
	if err == nil {
		c2.CloseNow()
		t.Fatal("second join within the limit window succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("resp = %v, want 429", resp)
	}
	conn.CloseNow()
}

func TestMetricsEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	conn, _ := e.join(t, e.createRoom(t).Code, player("Alice", ""))
	defer conn.CloseNow()
	rec := httptest.NewRecorder()
	NewMetrics(e.hub).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE qbuzzer_rooms gauge\nqbuzzer_rooms 1\n",
		"# TYPE qbuzzer_lock_lateness_ms summary\n",
		`qbuzzer_messages_written_total{type="state"}`,
		"qbuzzer_connections ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}
