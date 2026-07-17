package room

import (
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

// Game is a room's rules engine. The room calls it only from its own
// goroutine, after checking the sender's role, so implementations need no
// locking and must not block.
type Game interface {
	Handle(cmd Command) Result
}

// Command is a game message from one seat.
type Command struct {
	PlayerID   string
	Role       protocol.Role
	Msg        any // protocol.Buzz, protocol.Rule, ...
	ReceivedAt time.Time
}

// Result tells the room what to send after a Command.
type Result struct {
	Reject  *protocol.ErrorMessage // sent only to the sender; the room fills in Seq
	Changed bool                   // game state changed: broadcast a new RoomState
	Effects []protocol.Effect      // broadcast after the state, stamped with its version
}

// unimplementedGame rejects every command. It is the default until the game
// state machine exists.
type unimplementedGame struct{}

func (unimplementedGame) Handle(Command) Result {
	return Result{Reject: &protocol.ErrorMessage{
		Code:    protocol.ErrCodeNotImplemented,
		Message: "game actions arrive with the game state machine",
	}}
}
