-- Routing decision candidates, and the outcome detail the decision history
-- correlates.
--
-- routing_decision_candidates holds the evaluated candidate set of one decision:
-- one row per candidate the selector looked at, in evaluation order, with the
-- stable reason code that decided its fate and the quota/health values the hard
-- checks read. The rows cascade with their decision, so pruning history never
-- leaves orphan candidates behind and a decision never keeps candidates without
-- its row.
--
-- usage_records gains the two outcome fields the decision history needs: the
-- latency of the attempt and its normalized error class. Both stay NULL when
-- nobody reported them — a missing latency is not zero. The outcome enum gains
-- TIMEOUT, which the previous CHECK constraint could not express, so the table
-- is rebuilt the same way migration 0003 rebuilt it.

-- +goose Up
CREATE TABLE routing_decision_candidates (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The decision row this candidate belongs to. Cascading is what keeps the
    -- candidate set and its decision from ever disagreeing.
    decision_row_id      INTEGER NOT NULL REFERENCES routing_decisions (id) ON DELETE CASCADE,
    -- The stable decision id, copied for the correlation queries that do not
    -- want to join.
    decision_id          TEXT NOT NULL,
    -- Evaluation order, 1-based. One candidate per position.
    ordinal              INTEGER NOT NULL CHECK (ordinal > 0),
    alias                TEXT NOT NULL,
    provider             TEXT NOT NULL,
    selected             INTEGER NOT NULL CHECK (selected IN (0, 1)),
    -- Whether the candidate satisfied every hard constraint.
    eligible             INTEGER NOT NULL CHECK (eligible IN (0, 1)),
    -- The stage that eliminated the candidate; NULL when it was not eliminated.
    eliminated_stage     TEXT,
    -- The stable reason code; the prose beside it may be reworded.
    reason_code          TEXT NOT NULL CHECK (reason_code <> ''),
    reason               TEXT,
    transient            INTEGER NOT NULL DEFAULT 0 CHECK (transient IN (0, 1)),
    -- The quota values the decision acted on. NULL means the provider carried
    -- no observation, not zero and not available.
    quota_state          TEXT,
    quota_unknown_reason TEXT,
    quota_observed_at    TEXT,
    quota_reset_at       TEXT,
    -- The health values the decision acted on, with the same NULL meaning.
    health_state         TEXT,
    health_reason        TEXT,
    health_observed_at   TEXT,
    cooldown_until       TEXT,
    created_at           TEXT NOT NULL,
    UNIQUE (decision_row_id, alias),
    UNIQUE (decision_row_id, ordinal)
);

CREATE INDEX routing_decision_candidates_decision_id ON routing_decision_candidates (decision_id, ordinal);

-- The decision row gains the structured fields the decision history is queried
-- by. They are nullable: a decision made before this migration has none.
ALTER TABLE routing_decisions ADD COLUMN task_class TEXT;
ALTER TABLE routing_decisions ADD COLUMN requested_alias TEXT;
ALTER TABLE routing_decisions ADD COLUMN requested_provider TEXT;
ALTER TABLE routing_decisions ADD COLUMN selected_reason_code TEXT;
ALTER TABLE routing_decisions ADD COLUMN selected_reason TEXT;
ALTER TABLE routing_decisions ADD COLUMN affinity_expires_at TEXT;

CREATE INDEX routing_decisions_created_at ON routing_decisions (created_at);
CREATE INDEX routing_decisions_selected_provider_created_at ON routing_decisions (selected_provider, created_at);

CREATE TABLE usage_records_v2 (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id    TEXT,
    -- 1-based; a retry is attempt 2, not a duplicate of attempt 1.
    attempt       INTEGER NOT NULL CHECK (attempt >= 1),
    provider      TEXT NOT NULL,
    model         TEXT,
    input_tokens  INTEGER,
    output_tokens INTEGER,
    -- Quality of the token counts: the provider's own accounting or this
    -- installation's estimate. Never promoted from one to the other.
    provenance    TEXT NOT NULL CHECK (provenance IN ('reported', 'estimated')),
    -- Channel that reported the row (a LiteLLM callback, an agent report, ...).
    source        TEXT NOT NULL,
    outcome       TEXT NOT NULL CHECK (outcome IN
        ('SUCCESS', 'FAILED_BEFORE_FIRST_TOKEN', 'PARTIAL_STREAM', 'CANCELLED', 'TIMEOUT', 'UNKNOWN')),
    -- Routing decision this attempt was routed by; NULL when unreported.
    decision_id   TEXT,
    -- Attempt latency in milliseconds. NULL when nobody measured it.
    latency_ms    INTEGER,
    -- Normalized failure class. NULL when the attempt did not fail. Never raw
    -- error text: a provider message can carry a credential.
    error_class   TEXT,
    observed_at   TEXT NOT NULL,
    UNIQUE (request_id, attempt)
);

INSERT INTO usage_records_v2
    (id, request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source, outcome, decision_id, latency_ms, error_class, observed_at)
SELECT id, request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source, outcome, decision_id, NULL, NULL, observed_at
FROM usage_records;

DROP TABLE usage_records;
ALTER TABLE usage_records_v2 RENAME TO usage_records;

CREATE INDEX usage_records_observed_at ON usage_records (observed_at);
CREATE INDEX usage_records_provider_model_observed_at ON usage_records (provider, model, observed_at);
-- Outcome lookup by decision: the correlation query of the decision history.
CREATE INDEX usage_records_decision_id ON usage_records (decision_id);

-- +goose Down
DROP INDEX usage_records_decision_id;

-- The two outcome columns and the TIMEOUT outcome did not exist before, so the
-- table goes back to the shape migration 0003 left it in. The rows themselves
-- are kept: an older build cannot represent a timeout, and dropping usage the
-- operator may still need is worse than a row this build cannot read.
CREATE TABLE usage_records_v1 (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id    TEXT,
    attempt       INTEGER NOT NULL CHECK (attempt >= 1),
    provider      TEXT NOT NULL,
    model         TEXT,
    input_tokens  INTEGER,
    output_tokens INTEGER,
    provenance    TEXT NOT NULL CHECK (provenance IN ('reported', 'estimated')),
    source        TEXT NOT NULL,
    outcome       TEXT NOT NULL CHECK (outcome IN
        ('SUCCESS', 'FAILED_BEFORE_FIRST_TOKEN', 'PARTIAL_STREAM', 'CANCELLED', 'UNKNOWN')),
    decision_id   TEXT,
    observed_at   TEXT NOT NULL,
    UNIQUE (request_id, attempt)
);

INSERT INTO usage_records_v1 (id, request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source, outcome, decision_id, observed_at)
SELECT id, request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source,
       CASE WHEN outcome = 'TIMEOUT' THEN 'FAILED_BEFORE_FIRST_TOKEN' ELSE outcome END,
       decision_id, observed_at
FROM usage_records;

DROP TABLE usage_records;
ALTER TABLE usage_records_v1 RENAME TO usage_records;

CREATE INDEX usage_records_observed_at ON usage_records (observed_at);
CREATE INDEX usage_records_provider_model_observed_at ON usage_records (provider, model, observed_at);

DROP INDEX routing_decisions_selected_provider_created_at;
DROP INDEX routing_decisions_created_at;
ALTER TABLE routing_decisions DROP COLUMN affinity_expires_at;
ALTER TABLE routing_decisions DROP COLUMN selected_reason;
ALTER TABLE routing_decisions DROP COLUMN selected_reason_code;
ALTER TABLE routing_decisions DROP COLUMN requested_provider;
ALTER TABLE routing_decisions DROP COLUMN requested_alias;
ALTER TABLE routing_decisions DROP COLUMN task_class;

DROP TABLE routing_decision_candidates;
