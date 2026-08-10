# QBuzzer

Live quiz bowl buzzer: a moderator reads over voice chat, players buzz in the
browser, and the server decides who was first.

Requires Go 1.27+, Node 22 (`mise install` reads `mise.toml`), and
golangci-lint v2 for `make lint`.

```sh
make dev    # Go server + Vite dev server at http://localhost:5173
make test   # go test -race ./...
cd web && npm test && npm run e2e   # Vitest; Playwright smoke test against the real binary
make lint   # golangci-lint + eslint
make gen    # regenerate web/src/protocol.ts from internal/protocol
make build  # frontend build, then a single binary at bin/qbuzzer
```

The server listens on `127.0.0.1:8080`; set `QBUZZER_ADDR` to override.

## Using it

1. **Moderator:** open the site, choose a moderator password, and create a room.
   Read the room code to players over voice chat. To hand the room to another
   moderator, tell them the password; they open `/r/CODE/mod` and take over.
2. **Players:** enter the room code and a name, pick a team, and buzz with the
   big button or the spacebar.
3. **Rulings** are one key each: `P` power, `C` 10, `N` neg, `Z` 0. `Space`
   starts the next tossup or moves on, `D` kills a tossup nobody got, `1`–`9`
   rule bonus parts (`Shift` for a miss), `U` undoes. The console lists buzzes
   in judged order with their network correction.
4. **Scoresheet:** "Export scoresheet" downloads a CSV (draft format).

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

## Persistence

Set `DATABASE_URL` to a PostgreSQL connection string to keep games across
restarts; without it, games live in memory. On startup the server migrates the
schema (goose), restores every unfinished game active in the last two hours,
and players reconnect to their seats automatically.

Rooms never wait on the database: they hand writes to a bounded in-memory
queue that a single writer flushes every 100 ms. If the database falls behind
long enough to fill the queue, the game keeps running and the moderator sees
that it is no longer being saved.

Moderator passwords are stored as argon2id hashes and session tokens as
SHA-256 hashes; neither is stored in plain text.

```sh
make test-db   # all Go tests, including Postgres ones, against a throwaway database
```

## Performance

200 rooms × 9 connections on one laptop: the lock fires within 0.5 ms of its
deadline at p99, and the server uses about half a core
([details, method, and before/after profiles](docs/loadtest.md)).
Profiling is available on a loopback-only debug listener:

```sh
QBUZZER_DEBUG_ADDR=127.0.0.1:6060 ./bin/qbuzzer
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
scripts/loadtest.sh loadtest/run -rooms 200 -players 8 -network mixed
```
