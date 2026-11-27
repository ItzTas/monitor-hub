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

-- name: UserForLogin :one
SELECT id, login, admin, disabled_at, password_hash FROM users WHERE login = $1;

-- name: InsertSession :exec
INSERT INTO sessions (id_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: SessionPerson :one
SELECT u.id, u.login, u.admin
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id_hash = sqlc.arg(id_hash) AND s.expires_at > sqlc.arg(now)::timestamptz AND u.disabled_at IS NULL;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id_hash = $1;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at <= $1;

-- name: DeleteUserGrants :exec
DELETE FROM user_grants WHERE user_id = $1;

-- name: InsertUserGrant :exec
INSERT INTO user_grants (user_id, monitor, action) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;
