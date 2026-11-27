-- Session-to-route affinity. One row per session: the route it was last pinned
-- to and the three timestamps that make the pin auditable.
--
-- expires_at is the whole lifetime: a pin that is past it is not read as
-- affinity at all, so an abandoned session loses its route without any cleanup
-- having to run first. created_at survives a re-pin (the store's upsert keeps
-- it), so updated_at is what actually moves.
--
-- This is routing state, not history: rows past expires_at are removed by every
-- retention run rather than by a configured age, because their TTL already says
-- when they stopped meaning anything.

-- +goose Up
CREATE TABLE session_affinity (
    session_id TEXT PRIMARY KEY,
    alias      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX session_affinity_expires_at ON session_affinity (expires_at);

-- +goose Down
DROP TABLE session_affinity;
