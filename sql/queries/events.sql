-- name: InsertEvent :one
INSERT INTO events (plugin, event_id, plugin_seq, kind, severity, occurred_at, title, body, url, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (plugin, event_id) DO NOTHING
RETURNING seq;

-- name: ListEvents :many
SELECT e.seq, e.plugin, p.display_name, e.event_id, e.kind, e.severity, e.occurred_at, e.detected_at, e.title, e.body, e.url, e.payload
FROM events e
JOIN plugins p ON p.name = e.plugin
WHERE (sqlc.narg(plugin)::text IS NULL OR e.plugin = sqlc.narg(plugin)::text)
  AND (sqlc.narg(kind)::text IS NULL OR e.kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(before)::bigint IS NULL OR e.seq < sqlc.narg(before)::bigint)
ORDER BY e.seq DESC
LIMIT sqlc.arg(max_rows)::bigint;
