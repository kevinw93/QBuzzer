#!/usr/bin/env bash
# Load-tests a freshly built server with persistence on, against a throwaway
# Postgres. Usage: scripts/loadtest.sh OUTDIR [loadgen flags...]
# Writes OUTDIR/report.md, cpu.pprof, heap.pprof, server.log.
set -euo pipefail
out=$1
shift
mkdir -p "$out"
go build -o bin/qbuzzer ./cmd/server
go build -o bin/loadgen ./cmd/loadgen
export QBUZZER_LOADTEST_OUT=$out QBUZZER_LOADTEST_ARGS="$*"
exec scripts/with-testdb.sh bash -c '
  ulimit -n 20000
  export QBUZZER_ADDR=127.0.0.1:18180 QBUZZER_DEBUG_ADDR=127.0.0.1:6061
  ./bin/qbuzzer >"$QBUZZER_LOADTEST_OUT/server.log" 2>&1 &
  srv=$!
  trap "kill -INT $srv 2>/dev/null; wait $srv" EXIT
  for _ in $(seq 50); do curl -sf http://$QBUZZER_ADDR/healthz >/dev/null && break; sleep 0.1; done
  # shellcheck disable=SC2086
  ./bin/loadgen -url http://$QBUZZER_ADDR -debug http://$QBUZZER_DEBUG_ADDR \
    -cpuprofile "$QBUZZER_LOADTEST_OUT/cpu.pprof" -heapprofile "$QBUZZER_LOADTEST_OUT/heap.pprof" \
    $QBUZZER_LOADTEST_ARGS | tee "$QBUZZER_LOADTEST_OUT/report.md"
'
