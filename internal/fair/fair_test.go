package fair

import (
	"testing"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/protocol"
)

func TestPressTime(t *testing.T) {
	negative := clock.Estimate{Offset: -9000, RTT: 40, Bound: 20}
	if p := PressTime(5000, 5000-18-9000, negative, true, 10); p.Ms != 4982 || p.Clamped {
		t.Errorf("client clock behind the server: %+v", p)
	}
	// Client clock is 1000 ms ahead; RTT 40 ms and bound 20 ms, so the cap is
	// 70 ms with a 10 ms margin.
	est := clock.Estimate{Offset: 1000, RTT: 40, Bound: 20}
	tests := []struct {
		name        string
		clientMs    float64
		ok          bool
		wantMs      float64
		wantCorr    float64
		wantClamped bool
	}{
		{"honest: judged at the press", 5000 - 18 + 1000, true, 4982, 18, false},
		{"no estimate: judged at arrival", 4000, false, 5000, 0, false},
		{"no client time: judged at arrival", 0, true, 5000, 0, false},
		{"claims a press after arrival: clamped to arrival", 5000 + 30 + 1000, true, 5000, 0, true},
		{"claims a huge head start: clamped to the cap", 5000 - 400 + 1000, true, 4930, 70, true},
		{"exactly at the cap: not clamped", 5000 - 70 + 1000, true, 4930, 70, false},
		{"asymmetric link: uplink plus offset error, not clamped", 5000 - 55 + 1000, true, 4945, 55, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := PressTime(5000, tt.clientMs, est, tt.ok, 10)
			if p.Ms != tt.wantMs || p.CorrectionMs != tt.wantCorr || p.Clamped != tt.wantClamped {
				t.Errorf("got %+v, want ms %v corr %v clamped %v", p, tt.wantMs, tt.wantCorr, tt.wantClamped)
			}
		})
	}
}

func TestWindow(t *testing.T) {
	if w := Window(nil, 250); w != 0 {
		t.Errorf("no estimates: %v", w)
	}
	if w := Window([]float64{20, 140, 60}, 250); w != 140 {
		t.Errorf("largest cap: %v", w)
	}
	if w := Window([]float64{20, 900}, 250); w != 250 {
		t.Errorf("ceiling: %v", w)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		press float64
		want  protocol.Hint
	}{
		{999, protocol.HintYes},    // before the mark
		{1000, protocol.HintClose}, // exactly at it: the player may have heard it late
		{1300, protocol.HintClose}, // at the audio delay limit
		{1301, protocol.HintNo},
	}
	for _, tt := range tests {
		if got := Classify(tt.press, 1000, 300); got != tt.want {
			t.Errorf("press %v: %q, want %q", tt.press, got, tt.want)
		}
	}
}
