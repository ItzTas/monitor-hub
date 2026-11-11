-- name: InsertEvent :one
INSERT INTO events (plugin, event_id, plugin_seq, kind, severity, occurred_at, title, body, url, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (plugin, event_id) DO NOTHING
RETURNING seq;
