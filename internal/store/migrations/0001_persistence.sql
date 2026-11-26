-- Subscription quota, local usage, provider health and controller state.
--
-- Timestamps are UTC text in the fixed-width layout the store writes
-- (2006-01-02T15:04:05.000Z), so they sort lexicographically and stay readable
-- in a backup. Subscription quota, local usage and provider health are separate
-- tables on purpose: none of them may be derived from another.

-- +goose Up
CREATE TABLE quota_snapshots (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    provider       TEXT NOT NULL,
    state          TEXT NOT NULL CHECK (state IN ('AVAILABLE', 'LOW_QUOTA', 'EXHAUSTED', 'UNKNOWN')),
    -- Why the quota is UNKNOWN, and NULL for every other state. The reason is
    -- enumerated, so a typo cannot pass as a documented reason.
    unknown_reason TEXT CHECK ((state = 'UNKNOWN') = (unknown_reason IS NOT NULL))
        CHECK (unknown_reason IS NULL OR unknown_reason IN
            ('unsupported', 'fetch_failed', 'stale', 'malformed', 'auth', 'not_observed')),
    source         TEXT NOT NULL,
    confidence     TEXT NOT NULL CHECK (confidence IN ('reported', 'estimated')),
    observed_at    TEXT NOT NULL,
    -- Safe description of a fetch failure; never a raw provider response.
    error          TEXT
);

CREATE INDEX quota_snapshots_provider_observed_at ON quota_snapshots (provider, observed_at);

-- One row per quota window ("5h", "weekly", ...) known for a snapshot. Values
-- stay NULL when the provider does not report them.
CREATE TABLE quota_windows (
    snapshot_id     INTEGER NOT NULL REFERENCES quota_snapshots (id) ON DELETE CASCADE,
    window          TEXT NOT NULL,
    unit            TEXT,
    limit_value     REAL,
    used_value      REAL,
    remaining_value REAL,
    reset_at        TEXT,
    PRIMARY KEY (snapshot_id, window)
);

CREATE TABLE usage_records (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    -- Deduplicates retried or repeated reports; NULL when the source has no id.
    request_id    TEXT UNIQUE,
    provider      TEXT NOT NULL,
    model         TEXT,
    input_tokens  INTEGER,
    output_tokens INTEGER,
    provenance    TEXT NOT NULL CHECK (provenance IN ('reported', 'estimated')),
    observed_at   TEXT NOT NULL
);

CREATE INDEX usage_records_observed_at ON usage_records (observed_at);

CREATE TABLE provider_health (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    provider    TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('HEALTHY', 'DEGRADED', 'UNAVAILABLE', 'UNKNOWN')),
    observed_at TEXT NOT NULL,
    error       TEXT
);

CREATE INDEX provider_health_provider_observed_at ON provider_health (provider, observed_at);

CREATE TABLE controller_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE controller_state;
DROP TABLE provider_health;
DROP TABLE usage_records;
DROP TABLE quota_windows;
DROP TABLE quota_snapshots;
