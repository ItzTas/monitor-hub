-- name: InsertConsumer :one
INSERT INTO consumers (name, token_hash, position)
SELECT sqlc.arg(name)::text, sqlc.arg(token_hash)::text, COALESCE(MAX(seq), 0)::bigint FROM events
ON CONFLICT (name) DO NOTHING
RETURNING name;

-- name: ListConsumers :many
SELECT c.name, c.position, c.created_at, c.acked_at, c.seen_at,
  (SELECT count(*) FROM events e WHERE e.baseline = false AND e.seq > c.position)::bigint AS behind
FROM consumers c
ORDER BY c.name;

-- name: ConsumerByTokenHash :one
SELECT name, position FROM consumers WHERE token_hash = $1;

-- name: RotateConsumer :one
UPDATE consumers SET token_hash = $2 WHERE name = $1 RETURNING name;

-- name: DeleteConsumer :one
DELETE FROM consumers WHERE name = $1 RETURNING name;

-- name: TouchConsumer :one
UPDATE consumers SET seen_at = now() WHERE name = $1 AND token_hash = $2 RETURNING name;

-- name: AckConsumer :one
UPDATE consumers
SET position = GREATEST(position, sqlc.arg(seq)::bigint), acked_at = now(), seen_at = now()
WHERE name = sqlc.arg(name) AND token_hash = sqlc.arg(token_hash)
RETURNING position;

-- name: ClampConsumerPosition :exec
UPDATE consumers SET position = sqlc.arg(latest)::bigint
WHERE name = sqlc.arg(name) AND position > sqlc.arg(latest)::bigint;
