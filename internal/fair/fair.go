// Package fair converts client timestamps into server-time judgments for
// buzzes and moderator marks. All times are milliseconds on the clock.Server
// scale.
package fair

import (
	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/protocol"
)

// Press is a message's judged time.
type Press struct {
	Ms           float64 // judged server time
	CorrectionMs float64 // how much earlier than arrival it was judged
	Clamped      bool    // the client's claim fell outside [0, Cap] and was clamped
}

// Cap is the largest correction a message from a connection with estimate est
// may receive. An honest message's estimated correction is its uplink delay
// (under its RTT unless a spike hit that message) plus the offset estimate's
// error (within ±Bound), so the cap is RTT + Bound + margin.
func Cap(est clock.Estimate, marginMs float64) float64 { return est.RTT + est.Bound + marginMs }

// PressTime judges a message that arrived at arrivalMs carrying the client's
// timestamp clientMs (0 means the message had none). Without a clock estimate
// or a client timestamp it is judged at arrival. The correction is clamped to
// [0, Cap]: no message is judged later than it arrived, and no client can
// claim more than Cap of head start.
func PressTime(arrivalMs, clientMs float64, est clock.Estimate, ok bool, marginMs float64) Press {
	if !ok || clientMs == 0 {
		return Press{Ms: arrivalMs}
	}
	corr := arrivalMs - est.ToServer(clientMs)
	maxCorr := Cap(est, marginMs)
	p := Press{CorrectionMs: corr}
	switch {
	case corr < 0:
		p.CorrectionMs, p.Clamped = 0, true
	case corr > maxCorr:
		p.CorrectionMs, p.Clamped = maxCorr, true
	}
	p.Ms = arrivalMs - p.CorrectionMs
	return p
}

// Window is the collection window opened by the first arriving buzz: the
// largest cap among players who could still buzz, at most maxMs. A buzz
// pressed before the first arrival arrives at most its own uplink delay
// later, so a window covering every eligible cap lets it compete.
func Window(caps []float64, maxMs float64) float64 {
	w := 0.0
	for _, c := range caps {
		w = max(w, c)
	}
	return min(w, maxMs)
}

// Classify compares a press with a moderator mark (the power mark, or the end
// of reading). Players hear the moderator after an unknown audio delay of up
// to audioDelayMaxMs, so a press up to that long after the mark may still
// have come before the player heard it: that zone is a close call.
func Classify(pressMs, markMs, audioDelayMaxMs float64) protocol.Hint {
	switch gap := pressMs - markMs; {
	case gap < 0:
		return protocol.HintYes
	case gap <= audioDelayMaxMs:
		return protocol.HintClose
	default:
		return protocol.HintNo
	}
}
