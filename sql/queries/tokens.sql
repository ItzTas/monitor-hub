-- name: InsertToken :one
INSERT INTO tokens (user_id, label, hash, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: InsertTokenGrant :exec
INSERT INTO token_grants (token_id, monitor, action) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: ListTokens :many
SELECT t.id, u.login, t.label, t.created_at, t.expires_at, t.last_used_at
FROM tokens t
JOIN users u ON u.id = t.user_id
WHERE sqlc.arg(user_id)::bigint = 0 OR t.user_id = sqlc.arg(user_id)
ORDER BY t.created_at, t.id;

-- name: TokenGrants :many
SELECT token_id, monitor, action FROM token_grants
WHERE token_id = ANY(sqlc.arg(tokens)::bigint[])
ORDER BY token_id, monitor NULLS FIRST, action;

-- name: DeleteToken :execrows
DELETE FROM tokens WHERE id = sqlc.arg(id) AND (sqlc.arg(user_id)::bigint = 0 OR user_id = sqlc.arg(user_id));

-- name: TokenPerson :one
SELECT t.id, u.id AS user_id, u.login, u.admin
FROM tokens t
JOIN users u ON u.id = t.user_id
WHERE t.hash = sqlc.arg(hash) AND u.disabled_at IS NULL
    AND (t.expires_at IS NULL OR t.expires_at > sqlc.arg(now)::timestamptz);

-- name: TouchToken :exec
UPDATE tokens SET last_used_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id)
    AND (last_used_at IS NULL OR last_used_at < sqlc.arg(now)::timestamptz - interval '1 minute');

-- name: InsertCredential :one
INSERT INTO credentials (user_id, label, username, secret_hash, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- name: InsertCredentialGrant :exec
INSERT INTO credential_grants (credential_id, monitor, action) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: ListCredentials :many
SELECT c.id, u.login, c.label, c.username, c.created_at, c.expires_at, c.last_used_at
FROM credentials c
JOIN users u ON u.id = c.user_id
WHERE sqlc.arg(user_id)::bigint = 0 OR c.user_id = sqlc.arg(user_id)
ORDER BY c.created_at, c.id;

-- name: CredentialGrants :many
SELECT credential_id, monitor, action FROM credential_grants
WHERE credential_id = ANY(sqlc.arg(credentials)::bigint[])
ORDER BY credential_id, monitor NULLS FIRST, action;

-- name: DeleteCredential :execrows
DELETE FROM credentials WHERE id = sqlc.arg(id) AND (sqlc.arg(user_id)::bigint = 0 OR user_id = sqlc.arg(user_id));

-- name: CredentialPerson :one
SELECT c.id, c.secret_hash, u.id AS user_id, u.login, u.admin
FROM credentials c
JOIN users u ON u.id = c.user_id
WHERE c.username = sqlc.arg(username) AND u.disabled_at IS NULL
    AND (c.expires_at IS NULL OR c.expires_at > sqlc.arg(now)::timestamptz);

-- name: TouchCredential :exec
UPDATE credentials SET last_used_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id)
    AND (last_used_at IS NULL OR last_used_at < sqlc.arg(now)::timestamptz - interval '1 minute');
