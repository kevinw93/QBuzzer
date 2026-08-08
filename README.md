# QBuzzer

Live quiz bowl buzzer: a moderator reads over voice chat, players buzz in the
browser, and the server decides who was first.

Requires Go 1.27+, Node 22 (`mise install` reads `mise.toml`), and
golangci-lint v2 for `make lint`.

```sh
make dev    # Go server + Vite dev server at http://localhost:5173
make test   # go test -race ./...
make lint   # golangci-lint + eslint
make gen    # regenerate web/src/protocol.ts from internal/protocol
make build  # frontend build, then a single binary at bin/qbuzzer
```

The server listens on `127.0.0.1:8080`; set `QBUZZER_ADDR` to override.

## Layout

| Path | Contents |
|---|---|
| `cmd/server` | entrypoint, config, graceful shutdown |
| `internal/protocol` | wire types; source for the generated TypeScript |
| `internal/room` | hub and per-room event loop |
| `internal/httpapi` | HTTP routes and WebSocket pumps |
| `internal/clock` | clock offset and latency estimation |
| `internal/game` | tossup/bonus state machine and scoring |
| `internal/store` | PostgreSQL persistence |
| `web` | React + TypeScript frontend, embedded into the binary |

## Fairness

Players hear the moderator over voice chat and buzz over their own networks,
so "first to arrive" is often not "first to press". QBuzzer judges each buzz
by the client's press timestamp converted to server time with a per-connection
clock estimate (NTP-style ping exchanges), caps how far any client can move its
own buzz (RTT + estimate bound + 10 ms), and waits a short adaptive window
(the largest cap among eligible players, at most 100 ms) before locking in the
earliest press.

Simulated across mixed home, mobile, and asymmetric networks
([full results and assumptions](docs/fairness.md), regenerate with
`make fairness`):

| | First presser wins (all tossups) | First presser wins (presses < 25 ms apart) | Lock delay |
|---|---|---|---|
| Arrival order | 84.2% | 62.0% | 0 ms |
| QBuzzer | 93.7% | 83.3% | 105 ms |

What's not fixed: voice-chat delay. In about half of close races, the player
who *reacted* fastest after hearing the clue isn't the one who pressed first,
because their audio arrived later. Measuring that needs the moderator's audio
to go through QBuzzer (WebRTC playout timestamps), a planned stretch goal.
Powers and negs are the moderator's call (Power / 10 / Neg / 0 buttons): without
the audio, QBuzzer can't know which word a player heard.
