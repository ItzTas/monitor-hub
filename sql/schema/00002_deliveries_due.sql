-- +goose Up
CREATE INDEX deliveries_due ON deliveries (channel, next_attempt_at) WHERE status = 'pending';

-- +goose Down
DROP INDEX deliveries_due;
