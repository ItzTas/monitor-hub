-- +goose Up
CREATE TABLE plugins (
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
    plugin TEXT NOT NULL REFERENCES plugins (name),
    event_id TEXT NOT NULL,
    plugin_seq BIGINT NOT NULL,
    kind TEXT NOT NULL,
    severity SMALLINT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    url TEXT NOT NULL,
    payload JSONB,
    UNIQUE (plugin, event_id)
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

-- +goose Down
DROP TABLE deliveries;
DROP TABLE events;
DROP TABLE plugins;
