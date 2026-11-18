-- name: InsertEvent :one
INSERT INTO events (monitor, event_id, monitor_seq, kind, severity, occurred_at, title, body, url, payload, baseline)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (monitor, event_id) DO NOTHING
RETURNING seq;

-- name: ListEvents :many
SELECT e.seq, e.monitor, p.display_name, e.event_id, e.kind, e.severity, e.occurred_at, e.detected_at, e.title, e.body, e.url, e.payload
FROM events e
JOIN monitors p ON p.name = e.monitor
WHERE (sqlc.narg(monitor)::text IS NULL OR e.monitor = sqlc.narg(monitor)::text)
  AND (sqlc.narg(kind)::text IS NULL OR e.kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(before)::bigint IS NULL OR e.seq < sqlc.narg(before)::bigint)
ORDER BY e.seq DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: LockEventOrder :exec
SELECT pg_advisory_xact_lock(1836019304);

-- name: LatestEventSeq :one
SELECT COALESCE(MAX(seq), 0)::bigint FROM events;

-- name: NotificationsAfter :many
SELECT e.seq, e.monitor, p.display_name, e.kind, e.severity, e.occurred_at, e.detected_at, e.title, e.body, e.url
FROM events e
JOIN monitors p ON p.name = e.monitor
WHERE e.seq > sqlc.arg(after)::bigint AND NOT e.baseline
ORDER BY e.seq
LIMIT sqlc.arg(max_rows)::bigint;
