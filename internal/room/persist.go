package room

import (
	"encoding/json"
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

// Sink receives durable writes from rooms. Enqueue is called on the room
// goroutine and must never block: it returns false when it can't accept the
// op, and the room then stops persisting and reports itself unsaved.
type Sink interface {
	Enqueue(op Op) bool
}

// Op is one write. Ops for a game arrive in the order the room produced them.
type Op interface{ gameID() string }

type (
	OpCreateGame struct {
		GameID, Code, Epoch, PasswordHash string
		Rules                             json.RawMessage
		At                                time.Time
	}
	OpEpoch struct {
		GameID, Epoch string
	}
	// OpEvents appends to the game log. Seqs continue from the last OpEvents.
	OpEvents struct {
		GameID string
		Events []GameEvent
		At     time.Time
	}
	// OpSeat is the latest state of one seat; later ops for the same seat
	// supersede earlier ones.
	OpSeat struct {
		GameID string
		Seat   SeatRecord
	}
	// OpProjection is the latest query-friendly view of the game; later ones
	// supersede earlier ones.
	OpProjection struct {
		GameID string
		View   Projection
	}
	OpFinish struct {
		GameID string
		At     time.Time
	}
)

func (o OpCreateGame) gameID() string { return o.GameID }
func (o OpEpoch) gameID() string      { return o.GameID }
func (o OpEvents) gameID() string     { return o.GameID }
func (o OpSeat) gameID() string       { return o.GameID }
func (o OpProjection) gameID() string { return o.GameID }
func (o OpFinish) gameID() string     { return o.GameID }

// GameID reports which game an op belongs to.
func GameID(o Op) string { return o.gameID() }

// GameEvent is one entry of a game's durable log, opaque to the room. Seq is
// contiguous from 1.
type GameEvent struct {
	Seq  int
	Kind string
	At   time.Time
	Data json.RawMessage
}

// SeatRecord is a seat as stored. TokenHash is empty for the moderator.
type SeatRecord struct {
	PlayerID  string
	Role      protocol.Role
	Name      string
	TokenHash string
	LastSeq   uint64
}

// Projection is the part of a game that scoresheets and stats query.
type Projection struct {
	Teams           []ProjectedTeam         `json:"teams"`
	PlayerTeams     map[string]string       `json:"playerTeams"`
	History         []protocol.TossupRecord `json:"history"`
	Protests        []protocol.ProtestView  `json:"protests"`
	BonusPartPoints int                     `json:"bonusPartPoints"`
}

type ProjectedTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func projection(v protocol.GameView) Projection {
	p := Projection{
		Teams:           []ProjectedTeam{},
		PlayerTeams:     map[string]string{},
		History:         v.History,
		Protests:        v.Protests,
		BonusPartPoints: v.Rules.BonusPartPoints,
	}
	for _, t := range v.Teams {
		if !v.Rules.Individual {
			p.Teams = append(p.Teams, ProjectedTeam{ID: t.ID, Name: t.Name})
		}
		for _, pl := range t.PlayerIDs {
			p.PlayerTeams[pl] = t.ID
		}
	}
	return p
}

// Restored is a room loaded from storage.
type Restored struct {
	GameID, Code, PasswordHash string
	Seats                      []SeatRecord
	Game                       Game
}
