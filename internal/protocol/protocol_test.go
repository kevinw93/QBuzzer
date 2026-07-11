package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// One valid, fully populated sample per registered type. Tests fail if a
// message is registered without a sample, so new messages get round-trip
// coverage automatically.
var samples = map[MsgType]any{
	MsgJoin:            Join{ProtocolVersion: ProtocolVersion, Role: RolePlayer, Name: "Alice", Token: "tok"},
	MsgSetName:         SetName{Name: "Bob"},
	MsgBuzz:            Buzz{TossupID: "t3", ClientTimeMs: 1234.5},
	MsgPong:            Pong{PingID: 9, ClientRecvMs: 100.25, ClientSendMs: 100.75},
	MsgStartTossup:     StartTossup{TossupID: "t3"},
	MsgMarkPower:       MarkPower{TossupID: "t3"},
	MsgReadingFinished: ReadingFinished{TossupID: "t3"},
	MsgRule:            Rule{BuzzID: "b7", Result: RulingIncorrect},
	MsgUndo:            Undo{ActionID: "a12"},
	MsgNext:            Next{TossupID: "t3"},
	MsgProtest:         Protest{TossupID: "t3", Note: "answer was acceptable: “Gödel”"},

	MsgWelcome: Welcome{PlayerID: "p1", Role: RolePlayer, SessionToken: "tok", LastSeq: 41},
	MsgState: RoomState{
		Epoch: "e1", Version: 5, Code: "ABC234", ModeratorConnected: true,
		Players: []PlayerView{{ID: "p1", Name: "Alice", Connected: true}, {ID: "p2", Name: "Bob"}},
	},
	MsgError:        ErrorMessage{Code: ErrCodeStale, Message: "buzz b6 is no longer pending", Seq: 12},
	MsgPing:         Ping{ID: 9},
	MsgBuzzLocked:   BuzzLocked{Version: 6, TossupID: "t3", BuzzID: "b7", PlayerID: "p2"},
	MsgRuled:        Ruled{Version: 7, BuzzID: "b7", PlayerID: "p2", Result: RulingCorrect, Points: 15},
	MsgScoreChanged: ScoreChanged{Version: 7, PlayerID: "p2", Delta: -5},
}

func TestEverySpecHasASample(t *testing.T) {
	for _, s := range append(ClientSpecs(), ServerSpecs()...) {
		sample, ok := samples[s.Type]
		if !ok {
			t.Errorf("no sample for %q", s.Type)
			continue
		}
		if reflect.TypeOf(sample) != reflect.TypeOf(s.Payload) {
			t.Errorf("sample for %q is %T, registry says %T", s.Type, sample, s.Payload)
		}
	}
}

func TestClientMessagesRoundTrip(t *testing.T) {
	for _, s := range ClientSpecs() {
		t.Run(string(s.Type), func(t *testing.T) {
			want := samples[s.Type]
			b, err := EncodeClient(s.Type, 17, want)
			if err != nil {
				t.Fatal(err)
			}
			var env Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatal(err)
			}
			if env.Type != s.Type || env.Seq != 17 {
				t.Errorf("envelope = %+v", env)
			}
			got, err := DecodeClient(env)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip:\n got %#v\nwant %#v", got, want)
			}
		})
	}
}

func TestServerMessagesRoundTrip(t *testing.T) {
	for _, s := range ServerSpecs() {
		t.Run(string(s.Type), func(t *testing.T) {
			want := samples[s.Type]
			b, err := Encode(s.Type, want)
			if err != nil {
				t.Fatal(err)
			}
			var env Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatal(err)
			}
			if env.Type != s.Type || env.Seq != 0 {
				t.Errorf("envelope = %+v", env)
			}
			ptr := reflect.New(reflect.TypeOf(want))
			if err := json.Unmarshal(env.Payload, ptr.Interface()); err != nil {
				t.Fatal(err)
			}
			if got := ptr.Elem().Interface(); !reflect.DeepEqual(got, want) {
				t.Errorf("round trip:\n got %#v\nwant %#v", got, want)
			}
		})
	}
}

// Exact bytes for representative messages. The TypeScript client depends on
// these field names, so a changed JSON tag must fail loudly here.
func TestWireFormat(t *testing.T) {
	tests := []struct {
		name string
		got  func() ([]byte, error)
		want string
	}{
		{
			"join omits seq and empty token",
			func() ([]byte, error) {
				return EncodeClient(MsgJoin, 0, Join{ProtocolVersion: 1, Role: RolePlayer, Name: "Alice"})
			},
			`{"type":"join","payload":{"protocolVersion":1,"role":"player","name":"Alice"}}`,
		},
		{
			"buzz",
			func() ([]byte, error) { return EncodeClient(MsgBuzz, 7, Buzz{TossupID: "t3", ClientTimeMs: 1500.5}) },
			`{"type":"buzz","seq":7,"payload":{"tossupId":"t3","clientTimeMs":1500.5}}`,
		},
		{
			"state with no players encodes []",
			func() ([]byte, error) {
				return Encode(MsgState, RoomState{Epoch: "e", Version: 1, Code: "ABC234", Players: []PlayerView{}})
			},
			`{"type":"state","payload":{"epoch":"e","version":1,"code":"ABC234","moderatorConnected":false,"players":[]}}`,
		},
		{
			"error",
			func() ([]byte, error) {
				return Encode(MsgError, ErrorMessage{Code: ErrCodeForbidden, Message: "moderators only", Seq: 3})
			},
			`{"type":"error","payload":{"code":"forbidden","message":"moderators only","seq":3}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := tt.got()
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Errorf("\n got %s\nwant %s", b, tt.want)
			}
		})
	}
}

func TestEncodeRejectsMismatches(t *testing.T) {
	if _, err := Encode(MsgState, Welcome{}); err == nil {
		t.Error("wrong payload type accepted")
	}
	if _, err := Encode(MsgBuzz, Buzz{}); err == nil {
		t.Error("server encoder accepted a client message type")
	}
	if _, err := EncodeClient(MsgState, 1, RoomState{}); err == nil {
		t.Error("client encoder accepted a server message type")
	}
	if _, err := Encode("bogus", Ping{}); err == nil {
		t.Error("unregistered type accepted")
	}
}

func TestDecodeClientRejects(t *testing.T) {
	long := strings.Repeat("x", 33)
	tests := []struct {
		name    string
		env     string
		wantErr string
	}{
		{"unknown type", `{"type":"launch","seq":1,"payload":{}}`, "unknown message type"},
		{"server type", `{"type":"state","seq":1,"payload":{}}`, "unknown message type"},
		{"missing seq", `{"type":"buzz","payload":{"tossupId":"t1"}}`, "requires seq"},
		{"missing payload", `{"type":"buzz","seq":1}`, "requires a payload"},
		{"null payload", `{"type":"buzz","seq":1,"payload":null}`, "tossupId"},
		{"payload not an object", `{"type":"buzz","seq":1,"payload":[1]}`, "invalid buzz payload"},
		{"wrong field type", `{"type":"buzz","seq":1,"payload":{"tossupId":3}}`, "invalid buzz payload"},
		{"empty id", `{"type":"buzz","seq":1,"payload":{"tossupId":""}}`, "tossupId"},
		{"long id", `{"type":"next","seq":1,"payload":{"tossupId":"` + long + `"}}`, "tossupId"},
		{"negative time", `{"type":"buzz","seq":1,"payload":{"tossupId":"t1","clientTimeMs":-1}}`, "non-negative"},
		{"pong without id", `{"type":"pong","seq":1,"payload":{"clientRecvMs":1,"clientSendMs":2}}`, "pingId"},
		{"pong sent before received", `{"type":"pong","seq":1,"payload":{"pingId":1,"clientRecvMs":5,"clientSendMs":4}}`, "clientRecvMs"},
		{"unknown ruling", `{"type":"rule","seq":1,"payload":{"buzzId":"b1","result":"maybe"}}`, "unknown result"},
		{"undo without id", `{"type":"undo","seq":1,"payload":{}}`, "actionId"},
		{"long protest", `{"type":"protest","seq":1,"payload":{"tossupId":"t1","note":"` + strings.Repeat("n", MaxProtestNoteLen+1) + `"}}`, "note"},
		{"blank name", `{"type":"set_name","seq":1,"payload":{"name":"  "}}`, "name must be"},
		{"unknown role", `{"type":"join","payload":{"protocolVersion":1,"role":"admin","name":"A"}}`, "unknown role"},
		{"moderator without token", `{"type":"join","payload":{"protocolVersion":1,"role":"moderator"}}`, "token"},
		{"new player without name", `{"type":"join","payload":{"protocolVersion":1,"role":"player"}}`, "name"},
		{"oversized token", `{"type":"join","payload":{"protocolVersion":1,"role":"player","token":"` + strings.Repeat("t", 65) + `"}}`, "token too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var env Envelope
			if err := json.Unmarshal([]byte(tt.env), &env); err != nil {
				t.Fatal(err)
			}
			_, err := DecodeClient(env)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeClientAccepts(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want any
	}{
		{"join needs no seq", `{"type":"join","payload":{"protocolVersion":1,"role":"player","name":"A"}}`,
			Join{ProtocolVersion: 1, Role: RolePlayer, Name: "A"}},
		{"returning player needs no name", `{"type":"join","payload":{"protocolVersion":1,"role":"player","token":"tok"}}`,
			Join{ProtocolVersion: 1, Role: RolePlayer, Token: "tok"}},
		{"unknown fields ignored", `{"type":"buzz","seq":4,"payload":{"tossupId":"t1","clientTimeMs":2,"future":true}}`,
			Buzz{TossupID: "t1", ClientTimeMs: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var env Envelope
			if err := json.Unmarshal([]byte(tt.env), &env); err != nil {
				t.Fatal(err)
			}
			got, err := DecodeClient(env)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"  Alice  ", "Alice", true},
		{"Gödel", "Gödel", true},
		{strings.Repeat("é", MaxNameLen), strings.Repeat("é", MaxNameLen), true},
		{strings.Repeat("é", MaxNameLen+1), "", false},
		{"", "", false},
		{"   ", "", false},
		{"bad\x00name", "", false},
		{"bad\nname", "", false},
		{"\xff", "", false},
	}
	for _, tt := range tests {
		got, err := NormalizeName(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("NormalizeName(%q) = %q, %v", tt.in, got, err)
		}
	}
}
