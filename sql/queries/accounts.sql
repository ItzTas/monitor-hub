-- name: InsertUser :one
INSERT INTO users (login, password_hash, admin, created_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (login) DO NOTHING
RETURNING id;

-- name: UserByLogin :one
SELECT id, login, admin, disabled_at, created_at FROM users WHERE login = $1;

-- name: SetUserPassword :one
UPDATE users SET password_hash = $2 WHERE login = $1 RETURNING id;

-- name: SetUserAdmin :exec
UPDATE users SET admin = $2 WHERE id = $1;

-- name: SetUserDisabled :exec
UPDATE users
SET disabled_at = CASE WHEN sqlc.arg(disabled)::boolean THEN COALESCE(disabled_at, sqlc.arg(now)::timestamptz) END
WHERE id = sqlc.arg(id);

-- name: DeleteUser :execrows
DELETE FROM users WHERE login = $1;

-- name: ListUsers :many
SELECT id, login, admin, disabled_at, created_at FROM users ORDER BY login;

-- name: UserGrants :many
SELECT user_id, monitor, action FROM user_grants
WHERE user_id = ANY(sqlc.arg(users)::bigint[])
ORDER BY user_id, monitor NULLS FIRST, action;

-- name: LockAccess :exec
SELECT pg_advisory_xact_lock(1836019305);

-- name: CountAdmins :one
SELECT count(*) FROM users WHERE admin AND disabled_at IS NULL;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = $1;
