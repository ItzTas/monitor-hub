-- name: QueueDeliveries :exec
INSERT INTO deliveries (event_seq, channel, status, next_attempt_at)
SELECT sqlc.arg(event_seq)::bigint, unnest(sqlc.arg(channels)::text[]), 'pending', sqlc.arg(next_attempt_at)::timestamptz;
