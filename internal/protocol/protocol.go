// Package protocol defines the HTTP and WebSocket wire types.
// web/src/protocol.ts is generated from this package by tygo.
package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Envelope wraps every WebSocket message in both directions. Type determines
// the shape of Data.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Server -> client message types.
const (
	MsgWelcome = "welcome" // Welcome; always the first message on a connection
	MsgState   = "state"   // RoomState; replaces the client's entire state
	MsgError   = "error"   // ErrorMessage; sent only to the client whose request failed
)

// Client -> server message types.
const (
	MsgSetName = "set_name" // SetName
)

type Welcome struct {
	PlayerID string `json:"playerId"`
}

type PlayerView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// RoomState is the complete state of a room. Version strictly increases within
// a room; clients discard any state whose version is not greater than the last
// one applied.
type RoomState struct {
	Version uint64       `json:"version"`
	Code    string       `json:"code"`
	Players []PlayerView `json:"players"`
}

const (
	ErrCodeBadMessage  = "bad_message"
	ErrCodeUnknownType = "unknown_type"
	ErrCodeInvalidName = "invalid_name"
)

type ErrorMessage struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type SetName struct {
	Name string `json:"name"`
}

// CreateRoomResponse is the body of a successful POST /api/rooms.
type CreateRoomResponse struct {
	Code string `json:"code"`
}

// MaxNameLen is measured in runes.
const MaxNameLen = 32

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

// Encode returns a complete Envelope for v.
func Encode(typ string, v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Type: typ, Data: data})
}
