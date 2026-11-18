-- name: MonitorCursor :one
SELECT cursor, baselined_at FROM monitors WHERE name = $1;

-- name: SaveMonitorDescription :exec
INSERT INTO monitors (name, display_name, version)
VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET display_name = EXCLUDED.display_name, version = EXCLUDED.version;

-- name: SaveMonitorStatus :exec
UPDATE monitors
SET state = $2,
    message = $3,
    last_check_started_at = $4,
    last_success_at = $5,
    next_check_at = $6,
    status_seen_at = $7
WHERE name = $1;

-- name: AdvanceMonitorCursor :execrows
UPDATE monitors SET cursor = sqlc.arg(next) WHERE name = sqlc.arg(name) AND cursor = sqlc.arg(after);

-- name: CompleteMonitorBaseline :exec
UPDATE monitors SET baselined_at = now() WHERE name = $1 AND baselined_at IS NULL;

-- name: ListMonitors :many
SELECT name, display_name, version, state, message, last_check_started_at, last_success_at, next_check_at, status_seen_at
FROM monitors
ORDER BY name;
