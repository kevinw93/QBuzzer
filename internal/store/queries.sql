-- name: CreateTournament :one
INSERT INTO tournaments (name) VALUES ($1) RETURNING id;

-- name: CreateGame :exec
INSERT INTO games (id, tournament_id, code, epoch, moderator_password_hash, rules, created_at, last_active_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7);

-- name: TouchGame :exec
UPDATE games SET last_active_at = $2 WHERE id = $1;

-- name: SetGameEpoch :exec
UPDATE games SET epoch = $2 WHERE id = $1;

-- name: FinishGame :exec
UPDATE games SET finished_at = $2 WHERE id = $1 AND finished_at IS NULL;

-- name: FinishStaleGames :execrows
UPDATE games SET finished_at = now() WHERE finished_at IS NULL AND last_active_at < $1;

-- name: RecoverableGames :many
SELECT id, code, moderator_password_hash, rules FROM games
WHERE finished_at IS NULL AND last_active_at >= $1
ORDER BY created_at;

-- name: InsertEvents :copyfrom
INSERT INTO game_events (game_id, seq, kind, at, data) VALUES ($1, $2, $3, $4, $5);

-- name: GameEvents :many
SELECT seq, kind, at, data FROM game_events WHERE game_id = $1 ORDER BY seq;

-- name: UpsertPlayer :exec
INSERT INTO players (game_id, player_id, role, name, token_hash, last_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (game_id, player_id) DO UPDATE
SET name = excluded.name, token_hash = excluded.token_hash, last_seq = excluded.last_seq;

-- name: GamePlayers :many
SELECT player_id, role, name, token_hash, last_seq FROM players WHERE game_id = $1 ORDER BY player_id;

-- name: ClearPlayerTeams :exec
UPDATE players SET team_id = NULL WHERE game_id = $1;

-- name: SetPlayerTeam :exec
UPDATE players SET team_id = $3 WHERE game_id = $1 AND player_id = $2;

-- name: ClearProjection :exec
WITH t AS (DELETE FROM tossups WHERE tossups.game_id = $1),
     tm AS (DELETE FROM teams WHERE teams.game_id = $1)
DELETE FROM protests WHERE protests.game_id = $1;

-- name: InsertTeam :exec
INSERT INTO teams (game_id, team_id, name) VALUES ($1, $2, $3);

-- name: InsertTossup :exec
INSERT INTO tossups (game_id, number, tossup_id) VALUES ($1, $2, $3);

-- name: InsertBuzz :exec
INSERT INTO buzzes (game_id, tossup_number, buzz_id, player_id, team_id, arrived_ms, judged_ms, rtt_ms, clamped, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: InsertRuling :exec
INSERT INTO rulings (game_id, tossup_number, buzz_id, result, points) VALUES ($1, $2, $3, $4, $5);

-- name: InsertBonusPart :exec
INSERT INTO bonus_parts (game_id, tossup_number, part, team_id, correct, bounceback_team, points)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertProtest :exec
INSERT INTO protests (game_id, ordinal, tossup_id, filed_by, note) VALUES ($1, $2, $3, $4, $5);

-- name: PlayerStats :many
-- Per-player tossup stats for one game, for scoresheets and leaderboards.
SELECT p.player_id, p.name,
       count(*) FILTER (WHERE r.result = 'power')::int      AS powers,
       count(*) FILTER (WHERE r.result = 'correct')::int    AS tens,
       count(*) FILTER (WHERE r.result = 'neg')::int        AS negs,
       coalesce(sum(r.points), 0)::int                      AS points,
       coalesce(sum(b.judged_ms) FILTER (WHERE r.result IN ('power', 'correct')), 0)::double precision AS correct_buzz_ms_total -- average = total / (powers + tens)
FROM players p
LEFT JOIN buzzes b ON b.game_id = p.game_id AND b.player_id = p.player_id
LEFT JOIN rulings r ON r.game_id = b.game_id AND r.tossup_number = b.tossup_number AND r.buzz_id = b.buzz_id
WHERE p.game_id = $1 AND p.role = 'player'
GROUP BY p.player_id, p.name
ORDER BY points DESC, p.player_id;
