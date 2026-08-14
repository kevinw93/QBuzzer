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

A buzz is judged by the client's press timestamp converted to server time
(per-connection NTP-style clock estimate). The correction is capped at
RTT + estimate bound + 10 ms, and the lock waits an adaptive window (largest
cap among eligible players, at most 100 ms).

Simulated on mixed networks ([results and assumptions](docs/fairness.md),
`make fairness`):

| | First presser wins (all tossups) | First presser wins (presses < 25 ms apart) | Lock delay |
|---|---|---|---|
| Arrival order | 84.2% | 62.0% | 0 ms |
| QBuzzer | 93.7% | 83.3% | 105 ms |

Not corrected: voice-chat delay, which decides about half of close races.
That needs the audio to go through QBuzzer. For the same reason, powers and
negs are the moderator's call.

## Deploying

[docs/deploy.md](docs/deploy.md) (one Fly.io machine, private Managed
Postgres; CI deploys `main` after approval). Pilot plan:
[docs/pilot.md](docs/pilot.md).

## Persistence

Without `DATABASE_URL`, games live in memory only. With it, startup migrates
the schema and restores unfinished games active in the last two hours; players
reconnect to their seats.

Rooms never wait on the database. Writes go to a bounded queue flushed every
100 ms; a crash loses at most that interval. If the queue fills, the game
continues unsaved and the moderator is told.

Stored: argon2id password hashes and SHA-256 session-token hashes, never the
secrets.

```sh
make test-db   # all Go tests, including Postgres ones, against a throwaway database
```

## Performance

200 rooms × 9 connections on one laptop: lock within 0.5 ms of its deadline
at p99, about half a core ([docs/loadtest.md](docs/loadtest.md)). Profiling
(loopback only):

```sh
QBUZZER_DEBUG_ADDR=127.0.0.1:6060 ./bin/qbuzzer
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
scripts/loadtest.sh loadtest/run -rooms 200 -players 8 -network mixed
```
