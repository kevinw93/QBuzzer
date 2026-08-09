package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

// stored is one durable log entry: an event as recorded (with every decision
// made when it was handled, such as a buzz's judged time and window), or an
// undo. Replaying entries in order through the live code reproduces the game.
type stored struct {
	ID         int             `json:"id,omitempty"` // event ID; 0 for undo
	Player     string          `json:"player,omitempty"`
	Msg        json.RawMessage `json:"msg,omitempty"`
	Arrived    time.Time       `json:"arrived,omitzero"`
	Correction float64         `json:"correction,omitempty"`
	Clamped    bool            `json:"clamped,omitempty"`
	Window     time.Duration   `json:"window,omitempty"`
	RTT        float64         `json:"rtt,omitempty"`
	ActionID   string          `json:"actionId,omitempty"` // undo only
}

const (
	kindPlayerJoined = "player_joined"
	kindWindowClosed = "window_closed"
	kindUndo         = "undo"
)

var msgKinds = func() map[reflect.Type]protocol.MsgType {
	m := map[reflect.Type]protocol.MsgType{}
	for _, s := range protocol.ClientSpecs() {
		m[reflect.TypeOf(s.Payload)] = s.Type
	}
	return m
}()

func kindOf(msg any) string {
	switch msg.(type) {
	case playerJoined:
		return kindPlayerJoined
	case windowClosed:
		return kindWindowClosed
	}
	k, ok := msgKinds[reflect.TypeOf(msg)]
	if !ok {
		panic(fmt.Sprintf("game: no kind for %T", msg))
	}
	return string(k)
}

func (g *Game) appendStored(kind string, at time.Time, s stored) {
	data, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("game: encoding %s: %v", kind, err))
	}
	g.persistSeq++
	g.pending = append(g.pending, room.GameEvent{Seq: g.persistSeq, Kind: kind, At: at, Data: data})
}

func (g *Game) logEvent(e event) {
	s := stored{ID: e.id, Player: e.player, Arrived: e.arrived, Correction: e.correction, Clamped: e.clamped, Window: e.window, RTT: e.rtt}
	switch e.msg.(type) {
	case playerJoined, windowClosed:
	default:
		msg, err := json.Marshal(e.msg)
		if err != nil {
			panic(fmt.Sprintf("game: encoding %T: %v", e.msg, err))
		}
		s.Msg = msg
	}
	g.appendStored(kindOf(e.msg), e.at, s)
}

func (g *Game) Drain() []room.GameEvent {
	p := g.pending
	g.pending = nil
	return p
}

func (g *Game) RulesJSON() json.RawMessage {
	b, err := json.Marshal(g.rules)
	if err != nil {
		panic(err)
	}
	return b
}

var (
	ErrLogGap     = errors.New("game: log has a gap")
	ErrLogCorrupt = errors.New("game: log does not replay")
)

// Restore rebuilds a game from its ruleset and full log. It runs every entry
// through the same code that handled it live, so any divergence (a gap,
// a different event ID, a rejected event) is reported rather than papered
// over.
func Restore(rulesJSON []byte, log []room.GameEvent) (*Game, error) {
	var r Ruleset
	if err := json.Unmarshal(rulesJSON, &r); err != nil {
		return nil, fmt.Errorf("game: rules: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("game: rules: %w", err)
	}
	g := New(r)
	for i, ge := range log {
		if ge.Seq != i+1 {
			return nil, fmt.Errorf("%w: entry %d has seq %d", ErrLogGap, i+1, ge.Seq)
		}
		if err := g.replayEntry(ge); err != nil {
			return nil, fmt.Errorf("%w: seq %d (%s): %w", ErrLogCorrupt, ge.Seq, ge.Kind, err)
		}
	}
	g.pending = nil
	g.persistSeq = len(log)
	return g, nil
}

func (g *Game) replayEntry(ge room.GameEvent) error {
	var s stored
	if err := json.Unmarshal(ge.Data, &s); err != nil {
		return err
	}
	if ge.Kind == kindUndo {
		return g.undo(s.ActionID, ge.At)
	}
	e := event{at: ge.At, player: s.Player, arrived: s.Arrived, correction: s.Correction, clamped: s.Clamped, window: s.Window, rtt: s.RTT}
	switch ge.Kind {
	case kindPlayerJoined:
		e.msg = playerJoined{}
	case kindWindowClosed:
		e.msg = windowClosed{}
	default:
		msg, err := protocol.DecodeClient(protocol.Envelope{Type: protocol.MsgType(ge.Kind), Seq: 1, Payload: s.Msg})
		if err != nil {
			return err
		}
		e.msg = msg
	}
	if _, err := g.record(e); err != nil {
		return err
	}
	if g.nextID != s.ID {
		return fmt.Errorf("event ID %d replayed as %d", s.ID, g.nextID)
	}
	return nil
}
