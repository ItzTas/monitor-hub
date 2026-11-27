-- +goose Up
-- In every grant table a NULL monitor means monitor:* (every monitor); a marker
-- value could not satisfy the foreign key.
CREATE TABLE users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    login TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    admin BOOLEAN NOT NULL DEFAULT false,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_grants (
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    monitor TEXT REFERENCES monitors (name) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action IN ('view', 'check', 'hook', 'configure'))
);
CREATE UNIQUE INDEX user_grants_unique ON user_grants (user_id, COALESCE(monitor, ''), action);

CREATE TABLE tokens (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    label TEXT NOT NULL,
    hash TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ
);

CREATE TABLE token_grants (
    token_id BIGINT NOT NULL REFERENCES tokens (id) ON DELETE CASCADE,
    monitor TEXT REFERENCES monitors (name) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action IN ('view', 'check', 'hook'))
);
CREATE UNIQUE INDEX token_grants_unique ON token_grants (token_id, COALESCE(monitor, ''), action);

CREATE TABLE credentials (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    label TEXT NOT NULL,
    username TEXT NOT NULL UNIQUE,
    secret_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ
);

CREATE TABLE credential_grants (
    credential_id BIGINT NOT NULL REFERENCES credentials (id) ON DELETE CASCADE,
    monitor TEXT REFERENCES monitors (name) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action = 'hook')
);
CREATE UNIQUE INDEX credential_grants_unique ON credential_grants (credential_id, COALESCE(monitor, ''), action);

CREATE TABLE sessions (
    id_hash TEXT PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE sessions;
DROP TABLE credential_grants;
DROP TABLE credentials;
DROP TABLE token_grants;
DROP TABLE tokens;
DROP TABLE user_grants;
DROP TABLE users;
