-- +goose Up
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

-- +goose Down
DROP TABLE monitor_configs;
