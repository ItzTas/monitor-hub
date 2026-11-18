-- +goose Up
-- Renaming a constraint renames its index too. The NOT NULL constraints Postgres 18
-- names after the old columns are left alone: older servers do not have them.
ALTER TABLE plugins RENAME TO monitors;
ALTER TABLE monitors RENAME CONSTRAINT plugins_pkey TO monitors_pkey;
ALTER TABLE events RENAME COLUMN plugin TO monitor;
ALTER TABLE events RENAME COLUMN plugin_seq TO monitor_seq;
ALTER TABLE events RENAME CONSTRAINT events_plugin_fkey TO events_monitor_fkey;
ALTER TABLE events RENAME CONSTRAINT events_plugin_event_id_key TO events_monitor_event_id_key;

-- +goose Down
ALTER TABLE events RENAME CONSTRAINT events_monitor_event_id_key TO events_plugin_event_id_key;
ALTER TABLE events RENAME CONSTRAINT events_monitor_fkey TO events_plugin_fkey;
ALTER TABLE events RENAME COLUMN monitor_seq TO plugin_seq;
ALTER TABLE events RENAME COLUMN monitor TO plugin;
ALTER TABLE monitors RENAME CONSTRAINT monitors_pkey TO plugins_pkey;
ALTER TABLE monitors RENAME TO plugins;
