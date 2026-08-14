# Pilot: a practice game with the club

One moderator, two teams, about 20 tossups, real networks. Question: is it
fair, fast, and easy enough to replace what the club uses now?

## Checklist

**The week before**
- [ ] Deployed, and nothing merged since (no deploy may happen during the game).
- [ ] `fly status`: one machine in `ord`; `/healthz` is "ok".
- [ ] Ran one tossup yourself from a laptop and a phone on cellular data.
- [ ] Moderator has done a dry run with the keyboard shortcuts (P / C / N / Z,
      Space, D, 1–3, U).
- [ ] Feedback form ready (below), link ready to paste.

**At the start (10 minutes early)**
- [ ] Moderator creates the room on the device they'll moderate from and
      keeps the password somewhere they can find it.
- [ ] Read the room code over voice chat; everyone joins and picks a team.
- [ ] Check every player shows as connected and their latency badge isn't red.
- [ ] Note who is on Wi-Fi, wired, or cellular, and who uses Bluetooth audio
      (it adds voice delay QBuzzer can't see).
- [ ] Two practice buzzes so everyone hears the lock sound.

**During**
- [ ] Moderator notes the tossup number of any ruling that felt wrong, any
      "who buzzed first?" disagreement, and any time someone said their buzz
      didn't register.
- [ ] Try one deliberate disconnect (a player closes the tab and rejoins) and
      one undo.
- [ ] Watch for the "not being saved" banner (should never appear).

**After**
- [ ] Export the scoresheet CSV and compare it with the moderator's notes.
- [ ] Send the feedback form while it's fresh.
- [ ] Save the logs and run the queries below.

## What to capture

| Source | What | Why |
|---|---|---|
| `fly logs` (JSON) | joins, disconnect/reconnect, writer errors, rate limiting | Reconnects per player; any persistence trouble |
| Metrics | `qbuzzer_lock_lateness_ms`, `qbuzzer_inbox_delay_ms`, `qbuzzer_writer_queue_ops`, `qbuzzer_rate_limited_total` | Server overhead stays sub-millisecond; nothing backed up; no one blocked |
| Database | every buzz with arrival and judged times, correction, RTT, clamped flag | Fairness questions answered from data |
| Moderator's notes | disputed tossups, missed buzzes | Ground truth to compare with the data |
| Feedback form | perceived fairness and lag | What players actually experienced |

### Queries

Run with `fly mpg connect qbuzzer-db` (or `psql "$DATABASE_URL"` through
`fly proxy`). Replace `:game` with the game's id
(`SELECT id, code, created_at FROM games ORDER BY created_at DESC LIMIT 5`).

```sql
-- Per-player stats.
-- (The app's sqlc query PlayerStats returns the same.)
SELECT p.name, count(*) FILTER (WHERE r.result = 'power') AS powers,
       count(*) FILTER (WHERE r.result = 'correct') AS tens,
       count(*) FILTER (WHERE r.result = 'neg') AS negs, coalesce(sum(r.points), 0) AS points
FROM players p
LEFT JOIN buzzes b ON b.game_id = p.game_id AND b.player_id = p.player_id
LEFT JOIN rulings r USING (game_id, tossup_number, buzz_id)
WHERE p.game_id = :game AND p.role = 'player'
GROUP BY p.name ORDER BY points DESC;

-- Close races: tossups where the top two judged presses were under 25 ms apart,
-- and whether correction changed the order versus arrival.
WITH ranked AS (
  SELECT tossup_number, player_id, judged_ms, arrived_ms,
         rank() OVER (PARTITION BY tossup_number ORDER BY judged_ms)  AS by_judged,
         rank() OVER (PARTITION BY tossup_number ORDER BY arrived_ms) AS by_arrival
  FROM buzzes WHERE game_id = :game
)
SELECT a.tossup_number, a.player_id AS awarded_to, b.player_id AS runner_up,
       round((b.judged_ms - a.judged_ms)::numeric, 1) AS gap_ms,
       a.by_arrival > 1 AS correction_changed_winner
FROM ranked a JOIN ranked b USING (tossup_number)
WHERE a.by_judged = 1 AND b.by_judged = 2 AND b.judged_ms - a.judged_ms < 25
ORDER BY a.tossup_number;

-- Per-player network: corrections, RTT, and clamped buzzes (a clamp on an
-- honest player means the cap was too tight for their network).
SELECT p.name, count(*) AS buzzes, round(avg(b.arrived_ms - b.judged_ms)::numeric, 1) AS avg_correction_ms,
       round(avg(b.rtt_ms)::numeric, 1) AS avg_rtt_ms, count(*) FILTER (WHERE b.clamped) AS clamped
FROM buzzes b JOIN players p USING (game_id, player_id)
WHERE b.game_id = :game GROUP BY p.name ORDER BY clamped DESC, avg_rtt_ms DESC;

-- Protests filed.
SELECT tossup_id, filed_by, note FROM protests WHERE game_id = :game ORDER BY ordinal;
```

## Feedback form (5 minutes)

Players:
1. How fair did buzzing feel? (1 = often wrong, 5 = always right) Any tossup
   where you think you buzzed first but didn't get it?
2. Did any buzz not register, or register late? Which tossup?
3. How did your connection feel? (badge colour, reconnects)
4. Device and network: laptop/phone, Wi-Fi/wired/cellular, headphones wired or
   Bluetooth?
5. Compared with what we use now: better / same / worse, and why?

Moderator:
6. Were the controls fast enough to keep reading pace? Which key did you
   reach for and not find?
7. How confident were you in power and neg calls? Did knowing the buzz order
   and timing help?
8. Anything you needed during the game that wasn't there?

Tournament directors (if one watches):
9. What would you need before running a tournament on this? (Scoresheet
   format, multiple rooms, brackets, stats)
10. Paste or attach the scoresheet format you use.

## After the pilot

Prioritize fixes: (1) unfairness or lost data, (2) anything that slowed the
moderator, (3) everything else. Answers 9 and 10 replace the draft scoresheet
format.
