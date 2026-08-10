// Command loadgen drives a running QBuzzer server with simulated rooms over
// real WebSockets: a moderator and players per room, with per-player network
// delays injected client-side, playing tossups as fast as a real game would.
// It reports buzz-to-lock latency measured by the buzzing client, plus the
// server's own timings and memory from its debug endpoint.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/kevinw93/QBuzzer/internal/httpapi"
	"github.com/kevinw93/QBuzzer/internal/metrics"
	"github.com/kevinw93/QBuzzer/internal/protocol"
)

type delay struct{ base, jitter, spikeProb, spike float64 }

func (d delay) sample(rng *rand.Rand) time.Duration {
	v := d.base + rng.ExpFloat64()*d.jitter
	if rng.Float64() < d.spikeProb {
		v += rng.Float64() * d.spike
	}
	return time.Duration(v * float64(time.Millisecond))
}

type profile struct {
	name     string
	weight   float64
	up, down delay
}

// Same shapes as cmd/fairsim.
var profiles = []profile{
	{"lan", 0.10, delay{0.3, 0.2, 0, 0}, delay{0.3, 0.2, 0, 0}},
	{"wifi", 0.45, delay{8, 6, 0.05, 80}, delay{8, 6, 0.05, 80}},
	{"asymmetric", 0.15, delay{40, 4, 0, 0}, delay{8, 2, 0, 0}},
	{"mobile", 0.30, delay{30, 25, 0.15, 300}, delay{25, 20, 0.15, 300}},
}

var noDelay = profile{name: "none"}

func pick(rng *rand.Rand, mix string) profile {
	if mix == "none" {
		return noDelay
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

type stats struct {
	errMu       sync.Mutex
	errors      map[string]int     // code: message -> count
	buzzToLock  *metrics.Histogram // ms, measured by the winning buzzer
	tossups     atomic.Int64
	serverErrs  atomic.Int64
	disconnects atomic.Int64
}

// conn is one simulated client. Outgoing messages are delayed by the uplink
// and incoming ones by the downlink, each in FIFO order like a TCP stream.
type conn struct {
	ws     *websocket.Conn
	prof   profile
	offset float64 // client clock minus wall clock, ms

	mu  sync.Mutex
	rng *rand.Rand
	seq uint64

	out    chan []byte
	states chan protocol.RoomState // signals a new state; latest-wins
	latest protocol.RoomState      // guarded by mu
	handle func(env protocol.Envelope)
}

func (c *conn) now() float64 { return float64(time.Now().UnixNano())/1e6 + c.offset }

func (c *conn) sample(d delay) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return d.sample(c.rng)
}

func dial(ctx context.Context, base, code string, prof profile, seed uint64, st *stats) (*conn, error) {
	url := "ws" + strings.TrimPrefix(base, "http") + "/api/rooms/" + code + "/ws"
	ws, _, err := websocket.Dial(ctx, url, nil) //nolint:bodyclose // coder/websocket: the handshake response body needs no closing
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(1 << 22)
	c := &conn{
		ws: ws, prof: prof, rng: rand.New(rand.NewPCG(seed, seed*31+7)), //nolint:gosec // simulation, not security
		out: make(chan []byte, 256), states: make(chan protocol.RoomState, 1),
	}
	c.offset = (c.rng.Float64() - 0.5) * 100
	go c.sendLoop(ctx)
	go c.readLoop(ctx, st)
	return c, nil
}

func (c *conn) sendLoop(ctx context.Context) {
	var lastDue time.Time
	for msg := range c.out {
		due := time.Now().Add(c.sample(c.prof.up))
		if due.Before(lastDue) {
			due = lastDue // FIFO: a message can't overtake an earlier one
		}
		lastDue = due
		time.Sleep(time.Until(due))
		if err := c.ws.Write(ctx, websocket.MessageText, msg); err != nil {
			return
		}
	}
}

type delivery struct {
	data []byte
	due  time.Time
}

func (c *conn) readLoop(ctx context.Context, st *stats) {
	deliveries := make(chan delivery, 1024)
	go func() {
		for d := range deliveries {
			time.Sleep(time.Until(d.due))
			c.deliver(d.data)
		}
	}()
	defer close(deliveries)
	var lastDue time.Time
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				st.disconnects.Add(1)
			}
			return
		}
		due := time.Now().Add(c.sample(c.prof.down))
		if due.Before(lastDue) {
			due = lastDue
		}
		lastDue = due
		deliveries <- delivery{data, due}
	}
}

func (c *conn) deliver(data []byte) {
	var env protocol.Envelope
	if json.Unmarshal(data, &env) != nil {
		return
	}
	switch env.Type {
	case protocol.MsgPing:
		var p protocol.Ping
		_ = json.Unmarshal(env.Payload, &p)
		recv := c.now()
		b, _ := protocol.EncodeClient(protocol.MsgPong, 0, protocol.Pong{PingID: p.ID, ClientRecvMs: recv, ClientSendMs: c.now()})
		c.out <- b
	case protocol.MsgState:
		var s protocol.RoomState
		_ = json.Unmarshal(env.Payload, &s)
		c.mu.Lock()
		c.latest = s
		c.mu.Unlock()
		select {
		case <-c.states:
		default:
		}
		c.states <- s
	}
	if c.handle != nil {
		c.handle(env)
	}
}

func (c *conn) send(typ protocol.MsgType, payload any) {
	c.mu.Lock()
	seq := uint64(0)
	if typ != protocol.MsgJoin {
		c.seq++
		seq = c.seq
	}
	c.mu.Unlock()
	b, err := protocol.EncodeClient(typ, seq, payload)
	if err != nil {
		panic(err)
	}
	c.out <- b
}

// waitState returns the current state if it satisfies ok, else the first new
// one that does, or false on timeout.
func (c *conn) waitState(ctx context.Context, d time.Duration, ok func(protocol.RoomState) bool) (protocol.RoomState, bool) {
	c.mu.Lock()
	cur := c.latest
	c.mu.Unlock()
	if cur.Version > 0 && ok(cur) {
		return cur, true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case s := <-c.states:
			if ok(s) {
				return s, true
			}
		case <-t.C:
			return protocol.RoomState{}, false
		case <-ctx.Done():
			return protocol.RoomState{}, false
		}
	}
}

type player struct {
	*conn
	id string

	mu      sync.Mutex
	pressed map[string]time.Time // tossup ID -> wall time of this player's last press
}

func runRoom(ctx context.Context, base string, nPlayers int, mix string, seed uint64, st *stats, ready *sync.WaitGroup) error {
	rng := rand.New(rand.NewPCG(seed, seed^0xabcdef)) //nolint:gosec // simulation
	body, _ := json.Marshal(protocol.CreateRoomRequest{ModeratorPassword: "loadtest-password"})
	resp, err := http.Post(base+"/api/rooms", "application/json", bytes.NewReader(body)) //nolint:noctx // one-off setup request
	if err != nil {
		return err
	}
	var room protocol.CreateRoomResponse
	err = json.NewDecoder(resp.Body).Decode(&room)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}

	mod, err := dial(ctx, base, room.Code, pick(rng, mix), rng.Uint64(), st)
	if err != nil {
		return err
	}
	mod.handle = func(env protocol.Envelope) {
		if env.Type == protocol.MsgError {
			st.recordErr(env)
		}
	}
	mod.send(protocol.MsgJoin, protocol.Join{ProtocolVersion: protocol.ProtocolVersion, Role: protocol.RoleModerator, Password: "loadtest-password"})
	mod.send(protocol.MsgCreateTeam, protocol.CreateTeam{Name: "Alpha"})
	mod.send(protocol.MsgCreateTeam, protocol.CreateTeam{Name: "Beta"})

	players := make([]*player, nPlayers)
	for i := range players {
		c, err := dial(ctx, base, room.Code, pick(rng, mix), rng.Uint64(), st)
		if err != nil {
			return err
		}
		p := &player{conn: c, pressed: map[string]time.Time{}}
		welcome := make(chan string, 1)
		c.handle = func(env protocol.Envelope) {
			switch env.Type {
			case protocol.MsgWelcome:
				var w protocol.Welcome
				_ = json.Unmarshal(env.Payload, &w)
				welcome <- w.PlayerID
			case protocol.MsgState: // the lock arrives as an effect inside the state
				var s protocol.RoomState
				_ = json.Unmarshal(env.Payload, &s)
				for _, e := range s.Effects {
					var l protocol.BuzzLocked
					if e.Type != protocol.MsgBuzzLocked || json.Unmarshal(e.Payload, &l) != nil || l.PlayerID != p.id {
						continue
					}
					p.mu.Lock()
					t, ok := p.pressed[l.TossupID]
					p.mu.Unlock()
					if ok {
						st.buzzToLock.Observe(float64(time.Since(t).Nanoseconds()) / 1e6)
					}
				}
			case protocol.MsgError:
				st.recordErr(env)
			}
		}
		c.send(protocol.MsgJoin, protocol.Join{ProtocolVersion: protocol.ProtocolVersion, Role: protocol.RolePlayer, Name: fmt.Sprint("P", i)})
		select {
		case p.id = <-welcome:
		case <-time.After(10 * time.Second):
			return errors.New("no welcome")
		}
		players[i] = p
		team := "T1"
		if i%2 == 1 {
			team = "T2"
		}
		mod.send(protocol.MsgAssignTeam, protocol.AssignTeam{PlayerID: p.id, TeamID: team})
	}
	if _, ok := mod.waitState(ctx, 15*time.Second, func(s protocol.RoomState) bool {
		return len(s.Game.Teams) == 2 && len(s.Game.Teams[0].PlayerIDs)+len(s.Game.Teams[1].PlayerIDs) == nPlayers
	}); !ok {
		return errors.New("room setup timed out")
	}
	ready.Done()

	for ctx.Err() == nil {
		s, ok := mod.waitState(ctx, 10*time.Second, func(s protocol.RoomState) bool { return s.Game.Phase == protocol.PhaseIdle })
		if !ok {
			return nil
		}
		tossup := s.Game.TossupID
		mod.send(protocol.MsgStartTossup, protocol.StartTossup{TossupID: tossup})

		// The deciding clue comes 0.8–1.8 s in; 2–4 players know it and press
		// after their voice-chat delay plus a reaction time.
		clue := time.Duration(800+rng.IntN(1000)) * time.Millisecond
		knowers := rng.Perm(nPlayers)[:2+rng.IntN(3)]
		for _, k := range knowers {
			p := players[k]
			press := clue + time.Duration(80+rng.IntN(170))*time.Millisecond +
				time.Duration(math.Exp(math.Log(300)+0.25*rng.NormFloat64()))*time.Millisecond
			time.AfterFunc(press, func() {
				p.mu.Lock()
				p.pressed[tossup] = time.Now()
				p.mu.Unlock()
				p.send(protocol.MsgBuzz, protocol.Buzz{TossupID: tossup, ClientTimeMs: p.now()})
			})
		}

		s, ok = mod.waitState(ctx, 10*time.Second, func(s protocol.RoomState) bool { return s.Game.Phase == protocol.PhaseAwaitingRuling })
		if !ok {
			continue
		}
		var buzzID string
		for _, b := range s.Game.Buzzes {
			if b.Status == protocol.BuzzStatusAwaiting {
				buzzID = b.ID
			}
		}
		time.Sleep(400 * time.Millisecond) // the player answers aloud
		result := []protocol.Ruling{protocol.RulingPower, protocol.RulingCorrect, protocol.RulingCorrect, protocol.RulingNeg}[rng.IntN(4)]
		mod.send(protocol.MsgRule, protocol.Rule{BuzzID: buzzID, Result: result})
		if result == protocol.RulingNeg {
			mod.send(protocol.MsgNext, protocol.Next{TossupID: tossup})
		} else if s, ok := mod.waitState(ctx, 10*time.Second, func(s protocol.RoomState) bool { return s.Game.Phase == protocol.PhaseBonus }); ok {
			for part := range 3 {
				time.Sleep(150 * time.Millisecond)
				mod.send(protocol.MsgRuleBonus, protocol.RuleBonus{BonusID: s.Game.Bonus.ID, Part: part, Correct: rng.IntN(2) == 0})
			}
			mod.send(protocol.MsgNext, protocol.Next{TossupID: tossup})
		}
		st.tossups.Add(1)
	}
	return nil
}

func (st *stats) recordErr(env protocol.Envelope) {
	var e protocol.ErrorMessage
	_ = json.Unmarshal(env.Payload, &e)
	st.serverErrs.Add(1)
	st.errMu.Lock()
	defer st.errMu.Unlock()
	st.errors[string(e.Code)+": "+e.Message]++
}

func fetch(url, path string) {
	resp, err := http.Get(url) //nolint:noctx,gosec // local debug endpoint
	if err != nil {
		fmt.Fprintln(os.Stderr, "profile:", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err == nil {
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "profile:", err)
		}
	}
}

func getStats(debug, query string) (httpapi.Stats, error) {
	var s httpapi.Stats
	resp, err := http.Get(debug + "/debug/stats" + query) //nolint:noctx // local debug endpoint
	if err != nil {
		return s, err
	}
	defer func() { _ = resp.Body.Close() }()
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

func main() {
	base := flag.String("url", "http://127.0.0.1:18080", "server URL")
	debug := flag.String("debug", "http://127.0.0.1:6060", "server debug URL (QBUZZER_DEBUG_ADDR)")
	rooms := flag.Int("rooms", 200, "rooms")
	nPlayers := flag.Int("players", 8, "players per room")
	mix := flag.String("network", "mixed", "mixed (B6 network profiles) or none")
	warmup := flag.Duration("warmup", 10*time.Second, "excluded from measurements")
	dur := flag.Duration("duration", 60*time.Second, "measured run length")
	seed := flag.Uint64("seed", 1, "random seed")
	cpuProfile := flag.String("cpuprofile", "", "save the server's CPU profile for the measured phase here")
	heapProfile := flag.String("heapprofile", "", "save the server's heap profile at the end here")
	flag.Parse()

	baseline, err := getStats(*debug, "?gc")
	if err != nil {
		fmt.Fprintln(os.Stderr, "debug endpoint:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := &stats{buzzToLock: metrics.NewHistogram(0.01, 60_000, 1.1), errors: map[string]int{}}
	var ready, done sync.WaitGroup
	ready.Add(*rooms)
	sem := make(chan struct{}, 16) // room setup concurrency
	var failed atomic.Int64
	start := time.Now()
	for i := range *rooms {
		done.Go(func() {
			sem <- struct{}{}
			go func() { time.Sleep(200 * time.Millisecond); <-sem }()
			if err := runRoom(ctx, *base, *nPlayers, *mix, *seed*1000+uint64(i), st, &ready); err != nil { //nolint:gosec // i is small
				failed.Add(1)
				fmt.Fprintln(os.Stderr, "room:", err)
				ready.Done()
			}
		})
	}
	ready.Wait()
	setup := time.Since(start)
	time.Sleep(*warmup)
	st.buzzToLock.Reset()
	if _, err := getStats(*debug, "?reset"); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	tossups0 := st.tossups.Load()
	var profiled sync.WaitGroup
	if *cpuProfile != "" {
		profiled.Go(func() { fetch(*debug+fmt.Sprintf("/debug/pprof/profile?seconds=%d", int(dur.Seconds())), *cpuProfile) })
	}
	time.Sleep(*dur)
	profiled.Wait()
	if *heapProfile != "" {
		fetch(*debug+"/debug/pprof/heap?gc=1", *heapProfile)
	}
	tossups := st.tossups.Load() - tossups0
	end, err := getStats(*debug, "?gc")
	cancel()
	done.Wait()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	live := float64(*rooms - int(failed.Load()))
	b := st.buzzToLock.Summary()
	fmt.Printf("## Load test: %d rooms × %d players, network %q\n\n", *rooms, *nPlayers, *mix)
	fmt.Printf("Setup %.1fs, warm-up %v, measured %v. Rooms failed: %d. Server error replies: %d. Unexpected disconnects: %d.\n\n",
		setup.Seconds(), *warmup, *dur, failed.Load(), st.serverErrs.Load(), st.disconnects.Load())
	fmt.Printf("| Metric | p50 | p99 | max |\n|---|---|---|---|\n")
	fmt.Printf("| Buzz-to-lock, client-measured (ms) | %.1f | %.1f | %.1f |\n", b.P50, b.P99, b.Max)
	for _, k := range []string{"inboxDelayMs", "lockLatenessMs", "broadcastMs", "stateBytes"} {
		h := end.Histograms[k]
		fmt.Printf("| Server %s | %.3f | %.3f | %.3f |\n", k, h.P50, h.P99, h.Max)
	}
	fmt.Printf("\n| | |\n|---|---|\n")
	fmt.Printf("| Tossups/s (all rooms) | %.1f |\n", float64(tossups)/dur.Seconds())
	fmt.Printf("| Locks measured | %d |\n", b.Count)
	fmt.Printf("| Goroutines per room | %.1f |\n", float64(end.Goroutines-baseline.Goroutines)/live)
	fmt.Printf("| Heap in use per room | %.1f KiB |\n", float64(end.HeapInuseBytes-baseline.HeapInuseBytes)/live/1024)
	fmt.Printf("| Server goroutines total | %d |\n", end.Goroutines)
	var total uint64
	for _, n := range end.Outbound {
		total += n
	}
	fmt.Printf("| Messages written/s | %.0f |\n", float64(total)/dur.Seconds())
	for _, k := range []string{"state", "ping", "error", "buzz_locked", "ruled", "score_changed"} {
		fmt.Printf("| … %s | %.0f/s (%.0f%%) |\n", k, float64(end.Outbound[k])/dur.Seconds(), 100*float64(end.Outbound[k])/float64(max(total, 1)))
	}
	if len(st.errors) > 0 {
		fmt.Printf("\nServer error replies by kind:\n\n")
		for k, n := range st.errors {
			fmt.Printf("- %d × %s (late buzzes after the lock are expected)\n", n, k)
		}
	}
}
