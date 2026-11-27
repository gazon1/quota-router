-- Per-provider poll bookkeeping.
--
-- Last attempt and last success are separate columns on purpose: a provider
-- that keeps failing must still show when it was last polled, and a failure must
-- not erase when quota was last known good. NULL means "never", which is not the
-- same as a successful poll at the zero time.

-- +goose Up
CREATE TABLE poll_state (
    provider        TEXT PRIMARY KEY,
    last_attempt_at TEXT NOT NULL,
    -- NULL until an attempt establishes quota; a later failure leaves it alone.
    last_success_at TEXT
);

-- +goose Down
DROP TABLE poll_state;
