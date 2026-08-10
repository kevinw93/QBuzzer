package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:6060": true, "localhost:6060": true, "[::1]:6060": true,
		"0.0.0.0:6060": false, ":6060": false, "192.168.1.5:6060": false, "[::]:6060": false, "example.com:6060": false,
	} {
		err := CheckLoopback(addr)
		if (err == nil) != ok {
			t.Errorf("CheckLoopback(%q) = %v", addr, err)
		}
		if !ok && err != nil && addr != "nonsense" && !errors.Is(err, ErrNotLoopback) {
			t.Errorf("CheckLoopback(%q): unexpected error %v", addr, err)
		}
	}
}

func TestDebugStats(t *testing.T) {
	e := newEnv(t, nil)
	e.createRoom(t)
	ts := httptest.NewServer(NewDebug(e.hub))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/debug/stats?gc")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s Stats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || s.Rooms != 1 || s.Goroutines == 0 || s.HeapInuseBytes == 0 {
		t.Fatalf("stats = %+v, %v", s, err)
	}
	if _, ok := s.Histograms["broadcastMs"]; !ok {
		t.Errorf("histograms = %v", s.Histograms)
	}
	resp2, err := http.Get(ts.URL + "/debug/pprof/goroutine?debug=1")
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("pprof: %v %v", resp2, err)
	}
	_ = resp2.Body.Close()
}
