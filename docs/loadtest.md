# Load test: measured

200 rooms × (8 players + moderator) = 1,800 WebSockets, a tossup every ~3 s
per room, persistence on. Load generator and server shared one laptop's CPU
(Apple M-series), so the numbers are a lower bound.

Reproduce: `scripts/loadtest.sh OUTDIR -rooms 200 -players 8 -network none`
(or `-network mixed`), which also saves CPU and heap profiles.

- **none**: no injected delay; the window shrinks to ~10 ms, exposing
  server cost. Profiles come from this run.
- **mixed**: per-client network profiles from `docs/fairness.md`, injected
  client-side in both directions.

## Results

| | none, before | none, after | mixed, before | mixed, after |
|---|---|---|---|---|
| Buzz-to-lock p50 (client-measured) | 16.2 ms | 16.2 ms | 147.7 ms | 148.3 ms |
| Buzz-to-lock p99 | 17.8 ms | 18.1 ms | 453.7 ms | 454.4 ms |
| Server: lock lateness p50 / p99 | 0.04 / 0.51 ms | 0.04 / 0.53 ms | 0.04 / 0.34 ms | 0.04 / 0.32 ms |
| Server: inbox delay p99 | 0.11 ms | 0.09 ms | 0.04 ms | 0.03 ms |
| Tossups/s, all rooms | 81.8 | 82.4 | 72.0 | 72.8 |
| Messages written/s | 9,111 | 6,147 (−33%) | 8,956 | 6,330 (−29%) |
| Server CPU per 30 s measured | 19.0 s | 16.6 s (−13%) | 19.6 s | 16.6 s (−15%) |
| Heap in use per room | ~400 KiB | ~355 KiB | ~384 KiB | ~357 KiB |
| Goroutines per room | 37 | 37 | 37 | 37 |

Buzz-to-lock is the collection window plus network delay; server overhead
is under 0.5 ms at p99.

37 goroutines per room = room loop + 9 connections × (reader, writer, pinger,
keepalive).

## Profile and fix

CPU (none, before): ~64% write syscalls (`websocket.(*Conn).Write` →
`bufio.(*Writer).Flush` 38% cumulative), scheduler wakeups ~22%, room/game/JSON
under 5%. coder/websocket flushes every message, so cost scales with message
count.

Messages before: state 55%, score_changed 17%, buzz_locked 8%, ruled 8%,
ping 10%, error 2%. Each effect was followed by a state for the same version.

**Fix:** effects ride inside their state (`RoomState.effects`). An effect is
now lost only with its state, when a slow client's mailbox supersedes it.

Remaining writes: one state per change per connection, one ping per
connection every 2 s.
