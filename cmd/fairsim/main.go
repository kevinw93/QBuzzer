// Command fairsim simulates tossups with realistic network and voice-chat
// delays and measures how often each adjudication policy awards the buzz to
// the player who truly pressed first. It drives the real clock estimator and
// fairness code. Output is Markdown (make fairness writes docs/fairness.md).
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/fair"
)

type delay struct{ base, jitter, spikeProb, spike float64 }

func (d delay) sample(rng *rand.Rand) float64 {
	v := d.base + rng.ExpFloat64()*d.jitter
	if rng.Float64() < d.spikeProb {
		v += rng.Float64() * d.spike
	}
	return v
}

type profile struct {
	name     string
	weight   float64
	up, down delay
	proc     float64 // max client processing between ping receipt and pong
}

var profiles = []profile{
	{"LAN", 0.10, delay{0.3, 0.2, 0, 0}, delay{0.3, 0.2, 0, 0}, 0.5},
	{"home Wi-Fi", 0.45, delay{8, 6, 0.05, 80}, delay{8, 6, 0.05, 80}, 2},
	{"asymmetric uplink", 0.15, delay{40, 4, 0, 0}, delay{8, 2, 0, 0}, 2},
	{"mobile", 0.30, delay{30, 25, 0.15, 300}, delay{25, 20, 0.15, 300}, 5},
}

const (
	marginMs      = 10
	graceMs       = 5 // game.tickGrace
	closeRaceMs   = 25
	knowProb      = 0.6
	rtMedianMs    = 300
	rtSigma       = 0.25 // lognormal shape
	audioMinMs    = 80
	audioMaxMs    = 250
	audioJitterMs = 10
	warmupMs      = 30_000
)

// Correction caps. capRTTBound is fair.Cap; capRTT (the original spec) omits
// the offset estimate's own error, which is up to Bound in either direction.
type capFn func(clock.Estimate) float64

func capRTT(e clock.Estimate) float64      { return e.RTT + marginMs }
func capRTTBound(e clock.Estimate) float64 { return fair.Cap(e, marginMs) }

type player struct {
	prof   profile
	offset float64 // client clock minus server
	audio  float64 // voice-chat delay, per game
	est    clock.Estimate
}

type buzz struct {
	player                int
	heard, press, arrival float64 // server time
	claimed               float64 // the client's timestamp converted to server time, unclamped
	est                   clock.Estimate
}

// judged applies cap c (nil: no cap) to b's claimed press time.
func (b buzz) judged(c capFn) (ms float64, clamped bool) {
	if c == nil {
		return b.claimed, false
	}
	corr := b.arrival - b.claimed
	switch limit := c(b.est); {
	case corr < 0:
		return b.arrival, true
	case corr > limit:
		return b.arrival - limit, true
	}
	return b.claimed, false
}

func pickProfile(rng *rand.Rand, only int) profile {
	if only >= 0 {
		return profiles[only]
	}
	x := rng.Float64()
	for _, p := range profiles {
		if x < p.weight {
			return p
		}
		x -= p.weight
	}
	return profiles[len(profiles)-1]
}

// warm runs real ping exchanges for warmupMs, giving the estimate a player
// would have after 30 s in the room.
func warm(rng *rand.Rand, p *player) {
	s := clock.NewSession(clock.DefaultConfig())
	now := 0.0
	for now < warmupMs {
		if id, ok := s.Ping(now); ok {
			at := now + p.prof.up.sample(rng)
			reply := at + rng.Float64()*p.prof.proc
			_ = s.Pong(id, at+p.offset, reply+p.offset, reply+p.prof.down.sample(rng))
		}
		now += s.NextInterval()
	}
	p.est, _ = s.Estimate(now)
}

func newGame(rng *rand.Rand, only int) []player {
	ps := make([]player, 4+rng.IntN(5))
	for i := range ps {
		ps[i] = player{
			prof: pickProfile(rng, only),
			// performance.timeOrigin + now() is Unix-epoch ms; the server
			// clock counts from process start.
			offset: 1.76e12 + rng.Float64()*1e6,
			audio:  audioMinMs + rng.Float64()*(audioMaxMs-audioMinMs),
		}
		warm(rng, &ps[i])
	}
	return ps
}

// tossup returns the buzzes of every player who knows the answer at the
// deciding clue, spoken at server time 0. Player 0 claims to have pressed
// cheatMs earlier than they did.
func tossup(rng *rand.Rand, ps []player, cheatMs float64) []buzz {
	var bs []buzz
	for len(bs) < 2 {
		bs = bs[:0]
		for i, p := range ps {
			if rng.Float64() >= knowProb {
				continue
			}
			heard := max(20, p.audio+rng.NormFloat64()*audioJitterMs)
			press := heard + math.Exp(math.Log(rtMedianMs)+rtSigma*rng.NormFloat64())
			client := math.Round((press+p.offset)*10) / 10 // 0.1 ms timer resolution
			if i == 0 {
				client -= cheatMs
			}
			bs = append(bs, buzz{
				player: i, heard: heard, press: press,
				arrival: press + p.prof.up.sample(rng),
				claimed: p.est.ToServer(client), est: p.est,
			})
		}
	}
	return bs
}

type policy struct {
	name   string
	window func(ps []player) float64 // < 0: arrival order, no window
	cap    capFn
}

func fixed(w float64) func([]player) float64 { return func([]player) float64 { return w } }

func adaptive(ceiling float64, c capFn) func([]player) float64 {
	return func(ps []player) float64 {
		caps := make([]float64, len(ps))
		for i, p := range ps {
			caps[i] = c(p.est)
		}
		return fair.Window(caps, ceiling)
	}
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// winner returns who gets the floor, the lock delay after the first arrival,
// and how many buzzes had their claim clamped.
func winner(pol policy, ps []player, bs []buzz) (who int, lag float64, clamped int) {
	first := slices.MinFunc(bs, func(a, b buzz) int { return cmpF(a.arrival, b.arrival) })
	w := pol.window(ps)
	if w < 0 {
		return first.player, 0, 0
	}
	best, bestT := -1, math.Inf(1)
	for _, b := range bs {
		t, c := b.judged(pol.cap)
		if c {
			clamped++
		}
		if b.arrival > first.arrival+w {
			continue
		}
		if t < bestT {
			best, bestT = b.player, t
		}
	}
	return best, w + graceMs, clamped
}

type stats struct {
	n, pressRight, heardRight, close, closeRight, buzzes, clamped int
	lags                                                          []float64
}

func pct(a, b int) string { return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(max(b, 1))) }

func (s *stats) lag(q float64) string {
	slices.Sort(s.lags)
	return fmt.Sprintf("%.0f ms", s.lags[int(q*float64(len(s.lags)-1))])
}

type truth struct {
	pressFirst, heardFirst int
	close, audioFlipped    bool
}

func judgeTruth(bs []buzz) truth {
	byPress := slices.Clone(bs)
	slices.SortFunc(byPress, func(a, b buzz) int { return cmpF(a.press, b.press) })
	t := truth{
		pressFirst: byPress[0].player,
		heardFirst: slices.MinFunc(bs, func(a, b buzz) int { return cmpF(a.press-a.heard, b.press-b.heard) }).player,
		close:      byPress[1].press-byPress[0].press < closeRaceMs,
	}
	t.audioFlipped = t.close && t.pressFirst != t.heardFirst
	return t
}

func run(rng *rand.Rand, games, perGame, only int, policies []policy) (res []stats, closeN, flipped int) {
	res = make([]stats, len(policies))
	for range games {
		ps := newGame(rng, only)
		for range perGame {
			bs := tossup(rng, ps, 0)
			tr := judgeTruth(bs)
			if tr.close {
				closeN++
				if tr.audioFlipped {
					flipped++
				}
			}
			for i, pol := range policies {
				w, lag, cl := winner(pol, ps, bs)
				s := &res[i]
				s.n++
				s.lags = append(s.lags, lag)
				s.buzzes += len(bs)
				s.clamped += cl
				if w == tr.pressFirst {
					s.pressRight++
				}
				if w == tr.heardFirst {
					s.heardRight++
				}
				if tr.close {
					s.close++
					if w == tr.pressFirst {
						s.closeRight++
					}
				}
			}
		}
	}
	return res, closeN, flipped
}

func cheatWinRate(rng *rand.Rand, games, perGame int, cheatMs float64, c capFn) string {
	wins, n := 0, 0
	pol := policy{window: adaptive(100, capRTTBound), cap: c}
	for range games {
		ps := newGame(rng, -1)
		for range perGame {
			bs := tossup(rng, ps, cheatMs)
			if !slices.ContainsFunc(bs, func(b buzz) bool { return b.player == 0 }) {
				continue
			}
			n++
			if w, _, _ := winner(pol, ps, bs); w == 0 {
				wins++
			}
		}
	}
	return pct(wins, n)
}

func main() {
	games := flag.Int("games", 2000, "simulated games")
	perGame := flag.Int("tossups", 20, "tossups per game")
	seed := flag.Uint64("seed", 1, "random seed")
	flag.Parse()
	rng := rand.New(rand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15)) //nolint:gosec // simulation needs a seeded, reproducible PRNG

	policies := []policy{
		{name: "Arrival order (naive)", window: fixed(-1)},
		{name: "Corrected, cap RTT + 10 ms, adaptive ≤ 100 ms", window: adaptive(100, capRTT), cap: capRTT},
		{name: "**Corrected, cap RTT + bound + 10 ms, adaptive ≤ 100 ms (default)**", window: adaptive(100, capRTTBound), cap: capRTTBound},
		{name: "Corrected, cap RTT + bound + 10 ms, adaptive ≤ 150 ms", window: adaptive(150, capRTTBound), cap: capRTTBound},
		{name: "Corrected, cap RTT + bound + 10 ms, adaptive ≤ 250 ms", window: adaptive(250, capRTTBound), cap: capRTTBound},
		{name: "Corrected, cap RTT + bound + 10 ms, adaptive ≤ 400 ms", window: adaptive(400, capRTTBound), cap: capRTTBound},
		{name: "Corrected, cap RTT + bound + 10 ms, fixed 25 ms", window: fixed(25), cap: capRTTBound},
		{name: "Corrected, cap RTT + bound + 10 ms, fixed 50 ms", window: fixed(50), cap: capRTTBound},
		{name: "Corrected, cap RTT + bound + 10 ms, fixed 100 ms", window: fixed(100), cap: capRTTBound},
		{name: "Corrected, no cap, adaptive ≤ 100 ms (unsafe: see cheating)", window: adaptive(100, capRTTBound), cap: nil},
	}

	var b strings.Builder
	mixed, closeN, flipped := run(rng, *games, *perGame, -1, policies)
	fmt.Fprintf(&b, "# Buzz fairness: measured\n\n")
	fmt.Fprintf(&b, "Generated by `go run ./cmd/fairsim -games %d -tossups %d -seed %d` (`make fairness`): %d simulated tossups, 4–8 players each. ", *games, *perGame, *seed, mixed[0].n)
	b.WriteString("The simulator drives the real clock estimator (`internal/clock`) and adjudication code (`internal/fair`); only the network, voice chat, and players are simulated.\n\n")
	b.WriteString("- **Press-fair**: the player who physically pressed first wins. This is what network correction can fix.\n")
	b.WriteString("- **Heard-fair**: the player who reacted fastest after *hearing* the clue wins. This also needs each player's voice-chat delay, which QBuzzer can't see while audio goes through Discord or Zoom.\n")
	fmt.Fprintf(&b, "- **Close race**: the two earliest presses were under %d ms apart.\n", closeRaceMs)
	b.WriteString("- **Clamped**: share of honest buzzes whose claimed press time fell outside the allowed correction.\n")
	b.WriteString("- **Lock delay**: from the first buzz arriving to the winner being locked in (window + 5 ms grace).\n\n")

	b.WriteString("## Mixed networks\n\n")
	for _, p := range profiles {
		fmt.Fprintf(&b, "%s %.0f%%, ", p.name, 100*p.weight)
	}
	b.WriteString("assigned per player.\n\n")
	b.WriteString("| Policy | Press-fair | Press-fair, close races | Heard-fair | Clamped | Lock delay p50 | p95 |\n|---|---|---|---|---|---|---|\n")
	for i, pol := range policies {
		s := &mixed[i]
		cl := "—"
		if pol.cap != nil {
			cl = pct(s.clamped, s.buzzes)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", pol.name, pct(s.pressRight, s.n), pct(s.closeRight, s.close), pct(s.heardRight, s.n), cl, s.lag(0.5), s.lag(0.95))
	}
	fmt.Fprintf(&b, "\nClose races were %s of tossups. In %s of them, voice-chat delay alone made the fastest *reactor* differ from the first *presser*; no network correction can fix those.\n\n", pct(closeN, mixed[0].n), pct(flipped, closeN))

	b.WriteString("## One network type per room\n\nPress-fair in close races.\n\n| Network | Arrival order | Cap RTT + 10 ms | Cap RTT + bound + 10 ms | Clamped (RTT + 10 / RTT + bound + 10) |\n|---|---|---|---|---|\n")
	compare := []policy{policies[0], policies[1], policies[2]}
	for i, p := range profiles {
		r, _, _ := run(rng, *games/4, *perGame, i, compare)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s / %s |\n", p.name, pct(r[0].closeRight, r[0].close), pct(r[1].closeRight, r[1].close), pct(r[2].closeRight, r[2].close),
			pct(r[1].clamped, r[1].buzzes), pct(r[2].clamped, r[2].buzzes))
	}

	b.WriteString("\n## Cheating\n\nPlayer 0 claims to have pressed earlier than they did. Mixed networks, adaptive window ≤ 100 ms. Win rate is over tossups where player 0 knew the answer.\n\n")
	b.WriteString("| Claimed head start | Cap RTT + 10 ms | Cap RTT + bound + 10 ms | No cap |\n|---|---|---|---|\n")
	for _, lie := range []float64{0, 25, 50, 100, 300} {
		fmt.Fprintf(&b, "| %.0f ms | %s | %s | %s |\n", lie,
			cheatWinRate(rng, *games/4, *perGame, lie, capRTT),
			cheatWinRate(rng, *games/4, *perGame, lie, capRTTBound),
			cheatWinRate(rng, *games/4, *perGame, lie, nil))
	}

	b.WriteString("\n## Assumptions\n\n| Parameter | Value |\n|---|---|\n")
	for _, p := range profiles {
		fmt.Fprintf(&b, "| Network: %s | uplink %.1f + exp(%.1f) ms, downlink %.1f + exp(%.1f) ms, %.0f%% spikes up to %.0f ms |\n",
			p.name, p.up.base, p.up.jitter, p.down.base, p.down.jitter, 100*p.up.spikeProb, p.up.spike)
	}
	fmt.Fprintf(&b, "| Voice-chat delay | uniform %d–%d ms per player per game, ±%d ms per tossup |\n", audioMinMs, audioMaxMs, audioJitterMs)
	fmt.Fprintf(&b, "| Reaction time after hearing the clue | lognormal, median %d ms, σ = %.2f |\n", rtMedianMs, rtSigma)
	fmt.Fprintf(&b, "| Players who know the answer at the deciding clue | each with p = %.1f, at least 2 |\n", knowProb)
	fmt.Fprintf(&b, "| Clock estimate | after %d s of real ping exchanges (burst, then every 2 s) |\n", warmupMs/1000)
	b.WriteString("\nThese are modeling assumptions, not measurements of real voice chat or networks; replace them with pilot data.\n")

	if _, err := os.Stdout.WriteString(b.String()); err != nil {
		os.Exit(1)
	}
}
