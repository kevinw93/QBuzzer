// Package protocol defines the HTTP and WebSocket wire types.
//
// web/src/protocol.ts is generated from this file by tygo, and
// web/src/messages.ts (the discriminated unions) from the registry in
// codec.go by ./gen.
package protocol

import "encoding/json"

// ProtocolVersion must match between client and server. The server closes
// mismatched connections with CloseCodeReloadRequired.
const ProtocolVersion = 1

// Envelope wraps every WebSocket message. Seq is set by clients on every
// message except join and pong, and strictly increases for the lifetime of a session,
// across reconnects. Servers never set it.
type Envelope struct {
	Type    MsgType         `json:"type"`
	Seq     uint64          `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type MsgType string

const (
	// Client -> server.
	MsgJoin            MsgType = "join"
	MsgSetName         MsgType = "set_name"
	MsgBuzz            MsgType = "buzz"
	MsgPong            MsgType = "pong"
	MsgStartTossup     MsgType = "start_tossup"
	MsgMarkPower       MsgType = "mark_power"
	MsgReadingFinished MsgType = "reading_finished"
	MsgRule            MsgType = "rule"
	MsgUndo            MsgType = "undo"
	MsgNext            MsgType = "next"
	MsgProtest         MsgType = "protest"
	MsgRuleBonus       MsgType = "rule_bonus"
	MsgCreateTeam      MsgType = "create_team"
	MsgAssignTeam      MsgType = "assign_team"
	MsgChooseTeam      MsgType = "choose_team"

	// Server -> client.
	MsgWelcome      MsgType = "welcome"
	MsgState        MsgType = "state"
	MsgError        MsgType = "error"
	MsgPing         MsgType = "ping"
	MsgBuzzLocked   MsgType = "buzz_locked"
	MsgRuled        MsgType = "ruled"
	MsgScoreChanged MsgType = "score_changed"
)

type Role string

const (
	RolePlayer    Role = "player"
	RoleModerator Role = "moderator"
)

// CloseCode is an application WebSocket close code (4000-4999 are reserved
// for applications).
type CloseCode int

const (
	CloseCodeReloadRequired CloseCode = 4000 // protocol version mismatch; reload the page
	CloseCodeUnauthorized   CloseCode = 4001 // moderator token rejected
	CloseCodeUnknownSession CloseCode = 4002 // session token unknown to this room; rejoin without it
	CloseCodeReplaced       CloseCode = 4003 // the same session connected elsewhere; do not reconnect
	CloseCodeTooSlow        CloseCode = 4004 // outbound buffer overflowed; reconnect
	CloseCodeJoinRequired   CloseCode = 4005 // first message was not a valid join
)

// ---- Client -> server payloads ----

// Join must be the first message on a connection. Token resumes an existing
// session (players) or proves moderator rights (moderators).
type Join struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Role            Role   `json:"role"`
	Name            string `json:"name,omitempty"` // required for new players
	Token           string `json:"token,omitempty"`
}

type SetName struct {
	Name string `json:"name"`
}

// Buzz is ignored unless TossupID is the tossup currently being read.
type Buzz struct {
	TossupID     string  `json:"tossupId"`
	ClientTimeMs float64 `json:"clientTimeMs"` // client's clock; interpreted only via clock sync
}

// Pong answers Ping with the client's receive and send times on its clock.
type Pong struct {
	PingID       uint64  `json:"pingId"`
	ClientRecvMs float64 `json:"clientRecvMs"`
	ClientSendMs float64 `json:"clientSendMs"`
}

// Moderator tossup actions name the tossup they were issued against and are
// rejected if it is no longer current.
type StartTossup struct {
	TossupID string `json:"tossupId"`
}

// MarkPower and ReadingFinished carry the moderator's clock so they are
// judged on the same timeline as buzzes.
type MarkPower struct {
	TossupID     string  `json:"tossupId"`
	ClientTimeMs float64 `json:"clientTimeMs,omitempty"`
}

type ReadingFinished struct {
	TossupID     string  `json:"tossupId"`
	ClientTimeMs float64 `json:"clientTimeMs,omitempty"`
}

type Next struct {
	TossupID string `json:"tossupId"`
}

type Ruling string

const (
	RulingCorrect   Ruling = "correct"
	RulingIncorrect Ruling = "incorrect"  // neg if the buzz interrupted, else zero
	RulingNoPenalty Ruling = "no_penalty" // incorrect, never negged
)

// Rule is rejected unless BuzzID is the buzz awaiting a ruling. Power (for a
// correct answer) and Neg (for an incorrect one) override the timing hint,
// and are required when the hint is close.
type Rule struct {
	BuzzID string `json:"buzzId"`
	Result Ruling `json:"result"`
	Power  *bool  `json:"power,omitempty"`
	Neg    *bool  `json:"neg,omitempty"`
}

// Undo is rejected unless ActionID is the most recent undoable action.
type Undo struct {
	ActionID string `json:"actionId"`
}

const MaxProtestNoteLen = 500

type Protest struct {
	TossupID string `json:"tossupId"`
	Note     string `json:"note"`
}

// ---- Server -> client payloads ----

// Welcome is the first server message after a successful join. Clients store
// SessionToken to resume, and discard queued messages with seq <= LastSeq.
type Welcome struct {
	PlayerID     string `json:"playerId"`
	Role         Role   `json:"role"`
	SessionToken string `json:"sessionToken"`
	LastSeq      uint64 `json:"lastSeq"`
}

type PlayerView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

// RoomState is the complete state of a room and replaces whatever the client
// had. Within one Epoch, Version strictly increases; clients discard states
// with an equal or lower Version. A new Epoch (room recreated, e.g. after a
// restart) resets Version and must be accepted.
type RoomState struct {
	Epoch              string       `json:"epoch"`
	Version            uint64       `json:"version"`
	Code               string       `json:"code"`
	ModeratorConnected bool         `json:"moderatorConnected"`
	Players            []PlayerView `json:"players"`
	Game               GameView     `json:"game"`
}

type ErrCode string

const (
	ErrCodeBadMessage     ErrCode = "bad_message"
	ErrCodeInvalidName    ErrCode = "invalid_name"
	ErrCodeForbidden      ErrCode = "forbidden"
	ErrCodeStale          ErrCode = "stale" // precondition (tossupId, buzzId, actionId) no longer holds
	ErrCodeNotImplemented ErrCode = "not_implemented"
	ErrCodeNotAllowed     ErrCode = "not_allowed" // not valid in the current game phase or for this player
)

// ErrorMessage is sent only to the client whose message failed. Seq
// identifies that message.
type ErrorMessage struct {
	Code    ErrCode `json:"code"`
	Message string  `json:"message"`
	Seq     uint64  `json:"seq,omitempty"`
}

// Ping starts a clock-sync exchange; the client answers with Pong.
type Ping struct {
	ID uint64 `json:"id"`
}

// BuzzLocked, Ruled, and ScoreChanged trigger UI effects (sounds, flashes).
// They never carry state the client should apply: the RoomState at Version
// is authoritative, and losing one of these is harmless. An effect arrives
// before the RoomState with its Version, never after.
type BuzzLocked struct {
	Version  uint64 `json:"version"`
	TossupID string `json:"tossupId"`
	BuzzID   string `json:"buzzId"`
	PlayerID string `json:"playerId"`
}

type Ruled struct {
	Version  uint64 `json:"version"`
	BuzzID   string `json:"buzzId"`
	PlayerID string `json:"playerId"`
	Result   Ruling `json:"result"`
	Points   int    `json:"points"`
}

type ScoreChanged struct {
	Version uint64 `json:"version"`
	TeamID  string `json:"teamId"`
	Delta   int    `json:"delta"`
}

// ---- HTTP ----

// CreateRoomResponse is the body of POST /api/rooms. ModeratorToken is shown
// only once; whoever holds it can moderate the room.
type CreateRoomResponse struct {
	Code           string `json:"code"`
	ModeratorToken string `json:"moderatorToken"`
}

// MaxNameLen is measured in runes.
const MaxNameLen = 32

// ---- Game: client -> server ----

const MaxBonusParts = 10

// RuleBonus rules one bonus part. A part is ruled once; use Undo to change it.
// BouncebackTeamID awards a missed part to another team when the ruleset
// allows bouncebacks.
type RuleBonus struct {
	BonusID          string `json:"bonusId"`
	Part             int    `json:"part"` // 0-based
	Correct          bool   `json:"correct"`
	BouncebackTeamID string `json:"bouncebackTeamId,omitempty"`
}

type CreateTeam struct {
	Name string `json:"name"`
}

// AssignTeam moves a player to a team; an empty TeamID unassigns them.
type AssignTeam struct {
	PlayerID string `json:"playerId"`
	TeamID   string `json:"teamId,omitempty"`
}

// ChooseTeam lets a player assign themselves.
type ChooseTeam struct {
	TeamID string `json:"teamId"`
}

// ---- Game: server -> client (inside RoomState) ----

type Phase string

const (
	PhaseIdle           Phase = "idle"            // between tossups; TossupID is the next to start
	PhaseReading        Phase = "reading"         // buzzing open
	PhaseBuzzWindow     Phase = "buzz_window"     // collecting near-simultaneous buzzes
	PhaseAwaitingRuling Phase = "awaiting_ruling" // one buzz holds the floor
	PhaseBonus          Phase = "bonus"
	PhaseTossupDead     Phase = "tossup_dead" // every team locked out, or time expired
)

type BuzzStatus string

const (
	BuzzStatusPending   BuzzStatus = "pending" // inside the collection window
	BuzzStatusLost      BuzzStatus = "lost"    // another buzz in the window was earlier
	BuzzStatusAwaiting  BuzzStatus = "awaiting"
	BuzzStatusCorrect   BuzzStatus = "correct"
	BuzzStatusIncorrect BuzzStatus = "incorrect"
	BuzzStatusNoPenalty BuzzStatus = "no_penalty"
)

// Hint is the timing evidence for a power or an interrupt. Close means the
// press came within the possible audio delay after the moderator's mark, so
// only the moderator can judge it.
type Hint string

const (
	HintYes   Hint = "yes"
	HintNo    Hint = "no"
	HintClose Hint = "close"
)

// BuzzView is one buzz on the current tossup. OffsetMs is when it was judged
// pressed, in milliseconds after the tossup started; CorrectionMs is how much
// earlier than its arrival that is. Gaps are press minus mark, absent until
// the mark exists.
type BuzzView struct {
	ID             string     `json:"id"`
	PlayerID       string     `json:"playerId"`
	TeamID         string     `json:"teamId"`
	OffsetMs       float64    `json:"offsetMs"`
	CorrectionMs   float64    `json:"correctionMs"`
	Clamped        bool       `json:"clamped"` // the client's timestamp was implausible
	Status         BuzzStatus `json:"status"`
	PowerHint      Hint       `json:"powerHint,omitempty"` // empty when the rules have no powers
	PowerGapMs     *float64   `json:"powerGapMs,omitempty"`
	InterruptHint  Hint       `json:"interruptHint"`
	InterruptGapMs *float64   `json:"interruptGapMs,omitempty"`
	Power          bool       `json:"power"`     // as ruled; the hint until then
	Interrupt      bool       `json:"interrupt"` // as ruled; the hint until then
	Points         int        `json:"points"`
}

// TeamView is a team and its score. In individual play every player is a
// team whose ID is the player's ID and whose Name is empty.
type TeamView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Score     int      `json:"score"`
	PlayerIDs []string `json:"playerIds"`
}

type BonusPartView struct {
	Ruled            bool   `json:"ruled"`
	Correct          bool   `json:"correct"`
	BouncebackTeamID string `json:"bouncebackTeamId,omitempty"`
}

type BonusView struct {
	ID     string          `json:"id"`
	TeamID string          `json:"teamId"` // the team that earned the bonus
	Parts  []BonusPartView `json:"parts"`
}

type ProtestView struct {
	TossupID string `json:"tossupId"`
	PlayerID string `json:"playerId"` // who filed it ("mod" for the moderator)
	Note     string `json:"note"`
}

// GameView is the game portion of RoomState.
type GameView struct {
	Phase           Phase         `json:"phase"`
	TossupID        string        `json:"tossupId"`
	TossupNumber    int           `json:"tossupNumber"`
	PowerMarked     bool          `json:"powerMarked"`
	ReadingFinished bool          `json:"readingFinished"`
	Buzzes          []BuzzView    `json:"buzzes"`    // current tossup, earliest first
	LockedOut       []string      `json:"lockedOut"` // team IDs, or player IDs under player lockout
	Bonus           *BonusView    `json:"bonus,omitempty"`
	Teams           []TeamView    `json:"teams"`
	Protests        []ProtestView `json:"protests"`
	LastActionID    string        `json:"lastActionId,omitempty"` // target for Undo
	Individual      bool          `json:"individual"`
}
