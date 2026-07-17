package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Spec registers one message type. It is the single source for the Go
// decoder and the generated TypeScript unions, so the two cannot drift.
type Spec struct {
	Type    MsgType
	Payload any  // zero value of the payload struct
	Seq     bool // the envelope must carry a seq
}

var clientSpecs = []Spec{
	{MsgJoin, Join{}, false},
	{MsgSetName, SetName{}, true},
	{MsgBuzz, Buzz{}, true},
	{MsgPong, Pong{}, true},
	{MsgStartTossup, StartTossup{}, true},
	{MsgMarkPower, MarkPower{}, true},
	{MsgReadingFinished, ReadingFinished{}, true},
	{MsgRule, Rule{}, true},
	{MsgUndo, Undo{}, true},
	{MsgNext, Next{}, true},
	{MsgProtest, Protest{}, true},
}

var serverSpecs = []Spec{
	{MsgWelcome, Welcome{}, false},
	{MsgState, RoomState{}, false},
	{MsgError, ErrorMessage{}, false},
	{MsgPing, Ping{}, false},
	{MsgBuzzLocked, BuzzLocked{}, false},
	{MsgRuled, Ruled{}, false},
	{MsgScoreChanged, ScoreChanged{}, false},
}

var (
	clientIndex = index(clientSpecs)
	serverIndex = index(serverSpecs)
)

func index(specs []Spec) map[MsgType]Spec {
	m := make(map[MsgType]Spec, len(specs))
	for _, s := range specs {
		if _, dup := m[s.Type]; dup {
			panic("protocol: duplicate message type " + string(s.Type))
		}
		m[s.Type] = s
	}
	return m
}

func ClientSpecs() []Spec { return slices.Clone(clientSpecs) }
func ServerSpecs() []Spec { return slices.Clone(serverSpecs) }

// Encode builds a server -> client envelope. It returns an error if payload
// is not the registered type for typ.
func Encode(typ MsgType, payload any) ([]byte, error) {
	return encode(serverIndex, typ, 0, payload)
}

// EncodeClient builds a client -> server envelope (used by tests and tools).
func EncodeClient(typ MsgType, seq uint64, payload any) ([]byte, error) {
	return encode(clientIndex, typ, seq, payload)
}

func encode(idx map[MsgType]Spec, typ MsgType, seq uint64, payload any) ([]byte, error) {
	spec, ok := idx[typ]
	if !ok {
		return nil, fmt.Errorf("protocol: unregistered message type %q", typ)
	}
	if got, want := reflect.TypeOf(payload), reflect.TypeOf(spec.Payload); got != want {
		return nil, fmt.Errorf("protocol: %s payload must be %v, got %v", typ, want, got)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Type: typ, Seq: seq, Payload: data})
}

type validator interface{ Validate() error }

// DecodeClient decodes and validates a client message payload. The result is
// the payload struct by value (e.g. Buzz), suitable for a type switch.
// Unknown JSON fields are ignored so optional fields can be added without a
// protocol version bump.
func DecodeClient(env Envelope) (any, error) {
	spec, ok := clientIndex[env.Type]
	if !ok {
		return nil, fmt.Errorf("unknown message type %q", env.Type)
	}
	if spec.Seq && env.Seq == 0 {
		return nil, fmt.Errorf("%s requires seq", env.Type)
	}
	if len(env.Payload) == 0 {
		return nil, fmt.Errorf("%s requires a payload", env.Type)
	}
	ptr := reflect.New(reflect.TypeOf(spec.Payload))
	if err := json.Unmarshal(env.Payload, ptr.Interface()); err != nil {
		return nil, fmt.Errorf("invalid %s payload: %w", env.Type, err)
	}
	v := ptr.Elem().Interface()
	if val, ok := v.(validator); ok {
		if err := val.Validate(); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", env.Type, err)
		}
	}
	return v, nil
}

// ---- Validation ----

var ErrInvalidName = errors.New("name must be 1-32 printable characters")

// NormalizeName trims s and rejects empty, overlong, or non-printable names.
func NormalizeName(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrInvalidName
	}
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n == 0 || n > MaxNameLen {
		return "", ErrInvalidName
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", ErrInvalidName
		}
	}
	return s, nil
}

const maxTokenLen = 64

func (j Join) Validate() error {
	switch j.Role {
	case RolePlayer, RoleModerator:
	default:
		return fmt.Errorf("unknown role %q", j.Role)
	}
	if len(j.Token) > maxTokenLen {
		return errors.New("token too long")
	}
	if j.Role == RoleModerator && j.Token == "" {
		return errors.New("moderators must send a token")
	}
	if j.Role == RolePlayer && j.Token == "" && j.Name == "" {
		return errors.New("new players must send a name")
	}
	if j.Name != "" {
		if _, err := NormalizeName(j.Name); err != nil {
			return err
		}
	}
	return nil
}

func (m SetName) Validate() error {
	_, err := NormalizeName(m.Name)
	return err
}

func (m Buzz) Validate() error {
	if m.ClientTimeMs < 0 {
		return errors.New("clientTimeMs must be non-negative")
	}
	return requireID("tossupId", m.TossupID)
}

func (m Pong) Validate() error {
	if m.PingID == 0 {
		return errors.New("pingId is required")
	}
	if m.ClientRecvMs < 0 || m.ClientSendMs < m.ClientRecvMs {
		return errors.New("need 0 <= clientRecvMs <= clientSendMs")
	}
	return nil
}

func (m StartTossup) Validate() error     { return requireID("tossupId", m.TossupID) }
func (m MarkPower) Validate() error       { return requireID("tossupId", m.TossupID) }
func (m ReadingFinished) Validate() error { return requireID("tossupId", m.TossupID) }
func (m Next) Validate() error            { return requireID("tossupId", m.TossupID) }
func (m Undo) Validate() error            { return requireID("actionId", m.ActionID) }

func (m Rule) Validate() error {
	switch m.Result {
	case RulingCorrect, RulingIncorrect, RulingNoPenalty:
	default:
		return fmt.Errorf("unknown result %q", m.Result)
	}
	return requireID("buzzId", m.BuzzID)
}

func (m Protest) Validate() error {
	if !utf8.ValidString(m.Note) || utf8.RuneCountInString(m.Note) > MaxProtestNoteLen {
		return fmt.Errorf("note must be valid text of at most %d characters", MaxProtestNoteLen)
	}
	return requireID("tossupId", m.TossupID)
}

const maxIDLen = 32

func requireID(field, id string) error {
	if id == "" || len(id) > maxIDLen {
		return fmt.Errorf("%s must be 1-%d bytes", field, maxIDLen)
	}
	return nil
}

// Effect is implemented by the effect events (BuzzLocked, Ruled,
// ScoreChanged). WithVersion returns a copy referring to the state version
// that contains the effect's change.
type Effect interface {
	EffectType() MsgType
	WithVersion(v uint64) Effect
}

func (e BuzzLocked) EffectType() MsgType   { return MsgBuzzLocked }
func (e Ruled) EffectType() MsgType        { return MsgRuled }
func (e ScoreChanged) EffectType() MsgType { return MsgScoreChanged }

func (e BuzzLocked) WithVersion(v uint64) Effect   { e.Version = v; return e }
func (e Ruled) WithVersion(v uint64) Effect        { e.Version = v; return e }
func (e ScoreChanged) WithVersion(v uint64) Effect { e.Version = v; return e }
