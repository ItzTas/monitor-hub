-- +goose Up
CREATE TABLE monitors (
    name TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    version TEXT NOT NULL,
    cursor BIGINT NOT NULL DEFAULT 0,
    baselined_at TIMESTAMPTZ,
    state SMALLINT NOT NULL DEFAULT 0,
    message TEXT NOT NULL DEFAULT '',
    last_check_started_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    next_check_at TIMESTAMPTZ,
    status_seen_at TIMESTAMPTZ
);

CREATE TABLE events (
    seq BIGSERIAL PRIMARY KEY,
    monitor TEXT NOT NULL REFERENCES monitors (name),
    event_id TEXT NOT NULL,
    monitor_seq BIGINT NOT NULL,
    kind TEXT NOT NULL,
    severity SMALLINT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    url TEXT NOT NULL,
    payload JSONB,
    baseline BOOLEAN NOT NULL DEFAULT false,
    UNIQUE (monitor, event_id)
);

CREATE TABLE deliveries (
    event_seq BIGINT NOT NULL REFERENCES events (seq) ON DELETE CASCADE,
    channel TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (event_seq, channel)
);
CREATE INDEX deliveries_due ON deliveries (channel, next_attempt_at) WHERE status = 'pending';

CREATE TABLE delivery_recipients (
    event_seq BIGINT NOT NULL,
    channel TEXT NOT NULL,
    recipient TEXT NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_seq, channel, recipient),
    FOREIGN KEY (event_seq, channel) REFERENCES deliveries (event_seq, channel) ON DELETE CASCADE
);

CREATE TABLE consumers (
    name       TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    position   BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    acked_at   TIMESTAMPTZ,
    seen_at    TIMESTAMPTZ
);

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

-- No foreign key to monitors: the configuration of a monitor that leaves the image stays
-- stored and applies again if it returns.
CREATE TABLE monitor_configs (
    monitor TEXT PRIMARY KEY,
    document JSONB NOT NULL,
    secrets JSONB NOT NULL,
    ignored JSONB NOT NULL,
    variables JSONB NOT NULL,
    revision BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
