-- name: QueueDeliveries :exec
INSERT INTO deliveries (event_seq, channel, status, next_attempt_at)
SELECT sqlc.arg(event_seq)::bigint, unnest(sqlc.arg(channels)::text[]), 'pending', sqlc.arg(next_attempt_at)::timestamptz;

-- name: DueDeliveries :many
SELECT d.event_seq, d.channel, d.status, d.attempts, d.next_attempt_at, d.sent_at, d.last_error,
       e.monitor, p.display_name, e.kind, e.severity, e.title, e.body, e.url, e.occurred_at
FROM deliveries d
JOIN events e ON e.seq = d.event_seq
JOIN monitors p ON p.name = e.monitor
WHERE d.channel = sqlc.arg(channel) AND d.status = 'pending' AND d.next_attempt_at <= sqlc.arg(now)::timestamptz
ORDER BY e.occurred_at, e.seq
LIMIT sqlc.arg(max_rows)::bigint;

-- name: UpdateDelivery :exec
UPDATE deliveries
SET status = $3, attempts = $4, next_attempt_at = $5, last_error = $6, sent_at = $7
WHERE event_seq = $1 AND channel = $2;

-- name: ReachedRecipients :many
SELECT event_seq, recipient FROM delivery_recipients
WHERE channel = sqlc.arg(channel) AND event_seq = ANY(sqlc.arg(seqs)::bigint[]);

-- name: RecordRecipient :exec
INSERT INTO delivery_recipients (event_seq, channel, recipient)
SELECT unnest(sqlc.arg(seqs)::bigint[]), sqlc.arg(channel)::text, sqlc.arg(recipient)::text
ON CONFLICT DO NOTHING;
