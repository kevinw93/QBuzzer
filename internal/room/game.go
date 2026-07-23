package room

import (
	"time"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/protocol"
)

// Game is a room's rules engine. The room calls it only from its own
// goroutine, after checking the sender's role, so implementations need no
// locking and must not block.
type Game interface {
	Handle(cmd Command) Result
	// Tick is called when the most recent Result's WakeAt is reached.
	Tick(now time.Time) Result
	// PlayerJoined is called once per new player seat, before the state that
	// includes it is broadcast.
	PlayerJoined(playerID string)
	View() protocol.GameView
}

// Command is a game message from one seat.
type Command struct {
	PlayerID   string
	Role       protocol.Role
	Msg        any // protocol.Buzz, protocol.Rule, ...
	ReceivedAt time.Time
	Clock      clock.Estimate // sender's clock estimate; valid only if ClockOK
	ClockOK    bool
}

// Result tells the room what to send after a Command or Tick.
type Result struct {
	Reject  *protocol.ErrorMessage // sent only to the sender; the room fills in Seq
	Changed bool                   // game state changed: broadcast a new RoomState
	Effects []protocol.Effect      // broadcast before the state, stamped with its version
	WakeAt  time.Time              // when to call Tick; zero cancels any pending wakeup
}

// unimplementedGame rejects every command. It is the default when
// Config.NewGame is nil.
type unimplementedGame struct{}

func (unimplementedGame) Handle(Command) Result {
	return Result{Reject: &protocol.ErrorMessage{
		Code:    protocol.ErrCodeNotImplemented,
		Message: "this room has no game",
	}}
}

func (unimplementedGame) Tick(time.Time) Result   { return Result{} }
func (unimplementedGame) PlayerJoined(string)     {}
func (unimplementedGame) View() protocol.GameView { return protocol.GameView{} }
