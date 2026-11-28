-- name: GetMonitorConfig :one
SELECT document, secrets, ignored, variables, revision, updated_at
FROM monitor_configs
WHERE monitor = sqlc.arg(monitor);

-- A row is created only when previous is 0; any other previous writes only over that revision.
-- The SELECT must yield a row for ON CONFLICT to run, so it also yields one when the monitor has a row.
-- name: PutMonitorConfig :execrows
INSERT INTO monitor_configs (monitor, document, secrets, ignored, variables, revision, updated_at)
SELECT sqlc.arg(monitor)::text, sqlc.arg(document)::jsonb, sqlc.arg(secrets)::jsonb, sqlc.arg(ignored)::jsonb,
       sqlc.arg(variables)::jsonb, 1, sqlc.arg(updated_at)::timestamptz
WHERE sqlc.arg(previous)::bigint = 0
   OR EXISTS (SELECT 1 FROM monitor_configs WHERE monitor = sqlc.arg(monitor)::text)
ON CONFLICT (monitor) DO UPDATE
SET document = EXCLUDED.document,
    secrets = EXCLUDED.secrets,
    ignored = EXCLUDED.ignored,
    variables = EXCLUDED.variables,
    revision = monitor_configs.revision + 1,
    updated_at = EXCLUDED.updated_at
WHERE monitor_configs.revision = sqlc.arg(previous)::bigint;
