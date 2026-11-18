-- +goose Up
ALTER TABLE events ADD COLUMN baseline BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE consumers (
    name       TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    position   BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    acked_at   TIMESTAMPTZ,
    seen_at    TIMESTAMPTZ
);

-- +goose Down
DROP TABLE consumers;
ALTER TABLE events DROP COLUMN baseline;
