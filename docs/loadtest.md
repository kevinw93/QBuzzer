# Load test: measured

200 rooms × 8 players + 1 moderator each (1,800 WebSocket connections), every
room playing tossups as fast as a real game allows (~1 every 3 s), with
persistence on (Postgres). `cmd/loadgen` and the server ran as separate
processes on one laptop (Apple M-series), so they shared CPU; numbers are a
lower bound on what a dedicated server would do.

Reproduce: `scripts/loadtest.sh OUTDIR -rooms 200 -players 8 -network none`
(or `-network mixed`), which also saves CPU and heap profiles.

Two network settings:
- **none**: no injected delay. The collection window shrinks to ~10 ms, so
  this isolates the server's own cost; profiles come from this run.
- **mixed**: each client gets a network profile (LAN, Wi-Fi, asymmetric
  uplink, mobile; same shapes as `docs/fairness.md`), injected client-side in
  both directions.

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

Buzz-to-lock is dominated by the deliberate collection window and the
injected network delay, not by the server: the lock fires within 0.5 ms of
its deadline at p99, and messages wait under 0.1 ms in a room's inbox.

37 goroutines per room = 1 room loop + 9 connections × 4 (reader, writer,
pinger, keepalive).

## What the profile showed, and the fix

CPU (none, before): ~64% in `syscall` (writes), with
`websocket.(*Conn).Write` → `bufio.(*Writer).Flush` at 38% cumulative and the
scheduler waking goroutines at ~22%. QBuzzer's own logic (room loop, game,
JSON encoding) was under 5%. Every WebSocket message is one write system
call, and coder/websocket flushes after every message, so the lever is the
number of messages.

Messages written by type, before: state 55%, score_changed 17%,
buzz_locked 8%, ruled 8%, ping 10%, error 2%. The three effect events
(a third of all writes) were each followed immediately by a state message
for the same version.

**Fix:** effects ride inside the state they describe (`RoomState.effects`).
One message instead of two to four per change, and the room no longer needs
the "effects before state" ordering logic. Losing an effect was already
harmless by contract; now it can only be lost together with its state, when
a slow client's mailbox replaces that state with a newer one.

Remaining cost is the floor for this design: state messages (one per change
per connection) and clock-sync pings (one per connection every 2 s).
