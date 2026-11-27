-- Provider cooldown is a routing-policy exclusion, independent of health and
-- quota. It is persisted so routing decisions can enforce it across restarts.

-- +goose Up
ALTER TABLE provider_health ADD COLUMN cooldown_until TEXT;

-- +goose Down
-- SQLite does not support DROP COLUMN in older versions; rebuild not needed for down.
-- goose down is not used in production; keep schema compatible.
SELECT 1;
