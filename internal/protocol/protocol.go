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
	MsgJoin        MsgType = "join"
	MsgSetName     MsgType = "set_name"
	MsgBuzz        MsgType = "buzz"
	MsgPong        MsgType = "pong"
	MsgStartTossup MsgType = "start_tossup"
	MsgRule        MsgType = "rule"
	MsgUndo        MsgType = "undo"
	MsgNext        MsgType = "next"
	MsgProtest     MsgType = "protest"
	MsgRuleBonus   MsgType = "rule_bonus"
	MsgCreateTeam  MsgType = "create_team"
	MsgAssignTeam  MsgType = "assign_team"
	MsgChooseTeam  MsgType = "choose_team"

	// Server -> client.
	MsgWelcome MsgType = "welcome"
	MsgState   MsgType = "state"
	MsgError   MsgType = "error"
	MsgPing    MsgType = "ping"

	// Effects, inside RoomState.Effects.
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
	CloseCodeUnauthorized   CloseCode = 4001 // wrong moderator password
	CloseCodeUnknownSession CloseCode = 4002 // session token unknown to this room; rejoin without it
	CloseCodeReplaced       CloseCode = 4003 // the same session connected elsewhere; do not reconnect
	CloseCodeTooSlow        CloseCode = 4004 // outbound buffer overflowed; reconnect
	CloseCodeJoinRequired   CloseCode = 4005 // first message was not a valid join
	CloseCodeRateLimited    CloseCode = 4006 // too many wrong moderator passwords; wait a minute
)

// ---- Client -> server payloads ----

// Join must be the first message on a connection. Players resume a session
// with Token; moderators authenticate with the room's Password. Joining as
// moderator replaces any existing moderator connection, which is how a room
// is handed to another moderator.
type Join struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Role            Role   `json:"role"`
	Name            string `json:"name,omitempty"` // required for new players
	Token           string `json:"token,omitempty"`
	Password        string `json:"password,omitempty"`
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

type Next struct {
	TossupID string `json:"tossupId"`
}

// Ruling is the moderator's verdict on a buzz. The moderator decides powers
// and negs; QBuzzer can't know which word a player heard.
type Ruling string

const (
	RulingPower     Ruling = "power"      // correct, before the power mark
	RulingCorrect   Ruling = "correct"    // correct, no power
	RulingNeg       Ruling = "neg"        // incorrect interrupt
	RulingNoPenalty Ruling = "no_penalty" // incorrect, no penalty (e.g. after the question ended)
)

// Rule is rejected unless BuzzID is the buzz awaiting a ruling, and if Result
// awards points the ruleset doesn't have (a power without powers, a neg
// without negs).
type Rule struct {
	BuzzID string `json:"buzzId"`
	Result Ruling `json:"result"`
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
	Unsaved            bool         `json:"unsaved,omitempty"` // storage fell behind; this game will not survive a restart
	Effects            []Envelope   `json:"effects,omitempty"` // effects of the change that produced this version
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

// Ping starts a clock-sync exchange; the client answers with Pong. RTTMs and
// BoundMs report the server's current estimate for this connection (0 until
// the first exchange completes), for display only.
type Ping struct {
	ID      uint64  `json:"id"`
	RTTMs   float64 `json:"rttMs,omitempty"`
	BoundMs float64 `json:"boundMs,omitempty"`
}

// BuzzLocked, Ruled, and ScoreChanged are effects: they trigger UI feedback
// (sounds, flashes) and ride inside the RoomState whose change they describe
// (RoomState.Effects). They never carry state the client should apply, and
// losing one (its state superseded before delivery) is harmless.
type BuzzLocked struct {
	TossupID string `json:"tossupId"`
	BuzzID   string `json:"buzzId"`
	PlayerID string `json:"playerId"`
}

type Ruled struct {
	BuzzID   string `json:"buzzId"`
	PlayerID string `json:"playerId"`
	Result   Ruling `json:"result"`
	Points   int    `json:"points"`
}

type ScoreChanged struct {
	TeamID string `json:"teamId"`
	Delta  int    `json:"delta"`
}

// ---- HTTP ----

// CreateRoomRequest is the body of POST /api/rooms. Whoever knows the
// password can moderate the room.
type CreateRoomRequest struct {
	ModeratorPassword string `json:"moderatorPassword"`
}

type CreateRoomResponse struct {
	Code string `json:"code"`
}

// Moderator password limits, in runes. Separate declarations keep tygo from
// treating them as an enum.
const MinPasswordLen = 6

const MaxPasswordLen = 64

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
	PhaseTossupDead     Phase = "tossup_dead" // every team locked out
)

type BuzzStatus string

const (
	BuzzStatusPending   BuzzStatus = "pending" // inside the collection window
	BuzzStatusLost      BuzzStatus = "lost"    // another buzz in the window was earlier
	BuzzStatusAwaiting  BuzzStatus = "awaiting"
	BuzzStatusCorrect   BuzzStatus = "correct"
	BuzzStatusIncorrect BuzzStatus = "incorrect" // ruled a neg
	BuzzStatusNoPenalty BuzzStatus = "no_penalty"
)

// BuzzView is one buzz on the current tossup. OffsetMs is when it was judged
// pressed, in milliseconds after the tossup started; CorrectionMs is how much
// earlier than its arrival that is.
type BuzzView struct {
	ID           string     `json:"id"`
	PlayerID     string     `json:"playerId"`
	TeamID       string     `json:"teamId"`
	OffsetMs     float64    `json:"offsetMs"`
	CorrectionMs float64    `json:"correctionMs"`
	Clamped      bool       `json:"clamped"` // the client's timestamp was implausible
	Status       BuzzStatus `json:"status"`
	Power        bool       `json:"power"`           // ruled a power
	RTTMs        float64    `json:"rttMs,omitempty"` // the buzzer's connection RTT estimate; 0 if none yet
	Points       int        `json:"points"`
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
	Phase        Phase          `json:"phase"`
	TossupID     string         `json:"tossupId"`
	TossupNumber int            `json:"tossupNumber"`
	Buzzes       []BuzzView     `json:"buzzes"`    // current tossup, earliest first
	LockedOut    []string       `json:"lockedOut"` // team IDs, or player IDs under player lockout
	Bonus        *BonusView     `json:"bonus,omitempty"`
	Teams        []TeamView     `json:"teams"`
	Protests     []ProtestView  `json:"protests"`
	LastActionID string         `json:"lastActionId,omitempty"` // target for Undo
	History      []TossupRecord `json:"history"`                // finished tossups, oldest first
	Rules        RulesView      `json:"rules"`
}

// TossupRecord is a finished tossup, for scoresheets.
type TossupRecord struct {
	TossupID string     `json:"tossupId"`
	Number   int        `json:"number"`
	Buzzes   []BuzzView `json:"buzzes"` // earliest first; none were correct if the tossup went dead
	Bonus    *BonusView `json:"bonus,omitempty"`
}

// RulesView is what the UI needs from the ruleset.
type RulesView struct {
	Individual      bool `json:"individual"`
	TossupPoints    int  `json:"tossupPoints"`
	PowerPoints     int  `json:"powerPoints"` // 0: no powers
	NegPoints       int  `json:"negPoints"`   // 0: no negs
	BonusParts      int  `json:"bonusParts"`
	BonusPartPoints int  `json:"bonusPartPoints"`
	Bounceback      bool `json:"bounceback"`
}
