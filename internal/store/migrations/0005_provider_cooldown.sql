-- Provider cooldown is a routing-policy exclusion, independent of health and
-- quota. It is persisted so routing decisions can enforce it across restarts.

-- +goose Up
ALTER TABLE provider_health ADD COLUMN cooldown_until TEXT;

-- +goose Down
-- Rebuild provider_health without cooldown_until so migration history matches schema.
CREATE TABLE provider_health_old (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    provider    TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('HEALTHY', 'DEGRADED', 'UNAVAILABLE', 'UNKNOWN')),
    observed_at TEXT NOT NULL,
    error       TEXT
);
INSERT INTO provider_health_old (id, provider, state, observed_at, error)
SELECT id, provider, state, observed_at, error FROM provider_health;
DROP TABLE provider_health;
ALTER TABLE provider_health_old RENAME TO provider_health;
CREATE INDEX provider_health_provider_observed_at ON provider_health (provider, observed_at);
