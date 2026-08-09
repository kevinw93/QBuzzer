#!/usr/bin/env bash
# Runs a command against a throwaway PostgreSQL: a fresh data directory,
# listening on 127.0.0.1 only, with a random password. Everything is deleted
# afterwards. Usage: scripts/with-testdb.sh go test -race ./...
set -euo pipefail

dir=$(mktemp -d)
port=${QBUZZER_TESTDB_PORT:-55432}
pass=$(openssl rand -hex 16)
printf '%s' "$pass" >"$dir/pw"

cleanup() {
  pg_ctl -D "$dir/data" -m immediate stop >/dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT

initdb -D "$dir/data" -U qbuzzer --auth=scram-sha-256 --pwfile="$dir/pw" >/dev/null
pg_ctl -D "$dir/data" -l "$dir/log" -w \
  -o "-c listen_addresses=127.0.0.1 -p $port -k $dir" start >/dev/null

export DATABASE_URL="postgres://qbuzzer:$pass@127.0.0.1:$port/postgres?sslmode=disable"
export QBUZZER_REQUIRE_DB=1
"$@"
