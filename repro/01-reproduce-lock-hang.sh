#!/usr/bin/env bash
#
# Reproduces the "subscriptions stop updating, process and port stay alive,
# nothing in the logs, restart temporarily fixes it" failure.
#
# Root cause under test:
#   1. A background worker runs a database query that blocks on a row lock held
#      by another transaction. Without statement_timeout/lock_timeout the query
#      waits forever and the worker goroutine never returns.
#   2. The job queue is unbuffered, so pool.Push() blocks until a worker
#      receives the job. Once every worker is stuck, the single scheduler
#      goroutine freezes inside Push() and no new batch is ever produced.
#
# The fix adds lock_timeout/statement_timeout (steps 3 and 4) so the blocked
# query fails fast and the worker is released. The buffered queue with a
# non-blocking Push (demonstrated by the Go unit test TestPushNeverBlocks*)
# guarantees the scheduler itself can never freeze.
#
# Requirements: docker (postgres:18 image, as in .devcontainer), psql.
#
# Usage:
#   ./repro/01-reproduce-lock-hang.sh
#
set -euo pipefail

CONTAINER="miniflux-repro-db"
PGPORT=55432
export PGPASSWORD=postgres

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

host_psql() {
  psql -h 127.0.0.1 -p "$PGPORT" -U postgres -d repro "$@"
}

# Run SQL inside a long-lived detached session whose stdin stays open, so the
# transaction (and its row lock) remains held until we close the pipe.
hold_lock() {
  local pipe=$1
  docker exec -i "$CONTAINER" psql -U postgres -d repro -v ON_ERROR_STOP=1 <"$pipe" &
}

echo "==> Starting an isolated PostgreSQL instance on port ${PGPORT}"
cleanup
docker run -d --name "$CONTAINER" \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=repro \
  -p "${PGPORT}:5432" \
  postgres:18 >/dev/null

echo "==> Waiting for PostgreSQL to accept connections"
for _ in $(seq 1 30); do
  if host_psql -tAc 'select 1' >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

host_psql -c '
  CREATE TABLE feeds (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL,
    feed_url text NOT NULL,
    next_check_at timestamptz NOT NULL DEFAULT now()
  );
  INSERT INTO feeds (id, user_id, feed_url)
  SELECT g, 1, '"'"'https://example.invalid/feed-'"'"' || g
  FROM generate_series(1, 50) g;
' >/dev/null

echo
echo "==> Step 1: an unlocked UPDATE returns immediately"
start=$(date +%s)
host_psql -c "UPDATE feeds SET next_check_at = now() WHERE id = 1;" >/dev/null
echo "    unlocked update took $(( $(date +%s) - start ))s"

echo
echo "==> Step 2: WITHOUT the fix, a locked UPDATE blocks indefinitely"
echo "    Opening a background session A that takes a row lock and stays idle in transaction..."
PIPE=$(mktemp -u)
mkfifo "$PIPE"
exec 9<>"$PIPE"
hold_lock 9
printf 'BEGIN;\nUPDATE feeds SET next_check_at = now() WHERE id = 1;\n' >&9
sleep 2

LOCK_PID=$(host_psql -tAc \
  "SELECT pid FROM pg_locks WHERE relation = 'feeds'::regclass AND granted AND mode = 'RowExclusiveLock' LIMIT 1;")
echo "    lock holder backend pid: ${LOCK_PID}"
if [ -z "$LOCK_PID" ]; then
  echo "    UNEXPECTED: no row lock was acquired by session A"
  exit 1
fi

echo "    Attempting a competing UPDATE with no timeout (script aborts it after 5s)..."
set +e
timeout 5 host_psql -c "UPDATE feeds SET next_check_at = now() WHERE id = 1;"
rc=$?
set -e
if [ "$rc" -eq 124 ]; then
  echo "    RESULT: the query was still blocked after 5s -> this is what hangs a worker forever"
else
  echo "    UNEXPECTED: competing query did not block (rc=$rc)"
fi

echo
echo "==> Step 3: WITH the fix, lock_timeout aborts the blocked query"
start=$(date +%s)
set +e
host_psql -c "SET lock_timeout = '3s'; UPDATE feeds SET next_check_at = now() WHERE id = 1;"
rc=$?
set -e
elapsed=$(( $(date +%s) - start ))
if [ "$rc" -ne 0 ] && [ "$elapsed" -le 4 ]; then
  echo "    RESULT: query failed fast after ~${elapsed}s with a lock timeout -> worker is freed"
else
  echo "    UNEXPECTED: rc=$rc elapsed=${elapsed}s"
fi

echo
echo "==> Step 4: statement_timeout also caps a stalled statement"
start=$(date +%s)
set +e
host_psql -c "SET statement_timeout = '3s'; SELECT pg_sleep(10);"
rc=$?
set -e
elapsed=$(( $(date +%s) - start ))
if [ "$rc" -ne 0 ] && [ "$elapsed" -le 4 ]; then
  echo "    RESULT: long statement aborted after ~${elapsed}s instead of running 10s+"
else
  echo "    UNEXPECTED: rc=$rc elapsed=${elapsed}s"
fi

echo
echo "==> Releasing the idle-in-transaction lock holder (session A)"
printf 'COMMIT;\n' >&9
exec 9>&-
wait 2>/dev/null || true
rm -f "$PIPE"

echo
echo "All checks finished. The container is removed automatically on exit."
