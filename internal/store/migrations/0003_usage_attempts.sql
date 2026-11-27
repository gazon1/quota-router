-- Request-attempt usage and outcomes.
--
-- One row is one attempt of one request, so a retry gets its own row instead of
-- a second copy of the successful usage. (request_id, attempt) is the identity
-- ingestion deduplicates on; SQLite treats NULLs as distinct, so a report with
-- no request id cannot be deduplicated and is stored as sent.
--
-- Token counts stay NULL when the reporter did not supply them: a missing
-- number is never stored as zero. outcome is UNKNOWN for a row that predates
-- this column, because nothing established how it ended.

-- +goose Up
CREATE TABLE usage_records_new (
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
        ('SUCCESS', 'FAILED_BEFORE_FIRST_TOKEN', 'PARTIAL_STREAM', 'CANCELLED', 'UNKNOWN')),
    -- Routing decision this attempt was routed by; NULL when unreported.
    decision_id   TEXT,
    observed_at   TEXT NOT NULL,
    UNIQUE (request_id, attempt)
);

INSERT INTO usage_records_new
    (id, request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source, outcome, observed_at)
SELECT id, request_id, 1, provider, model, input_tokens, output_tokens, provenance, 'legacy', 'UNKNOWN', observed_at
FROM usage_records;

DROP TABLE usage_records;
ALTER TABLE usage_records_new RENAME TO usage_records;

CREATE INDEX usage_records_observed_at ON usage_records (observed_at);
CREATE INDEX usage_records_provider_model_observed_at ON usage_records (provider, model, observed_at);

-- +goose Down
-- Going back to the pre-attempt shape keeps the first attempt of each request:
-- an older build cannot represent a retry, and dropping the table would lose
-- usage the operator may still need.
CREATE TABLE usage_records_v1 (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id    TEXT UNIQUE,
    provider      TEXT NOT NULL,
    model         TEXT,
    input_tokens  INTEGER,
    output_tokens INTEGER,
    provenance    TEXT NOT NULL CHECK (provenance IN ('reported', 'estimated')),
    observed_at   TEXT NOT NULL
);

INSERT INTO usage_records_v1 (id, request_id, provider, model, input_tokens, output_tokens, provenance, observed_at)
SELECT id, request_id, provider, model, input_tokens, output_tokens, provenance, observed_at
FROM usage_records WHERE attempt = 1;

DROP TABLE usage_records;
ALTER TABLE usage_records_v1 RENAME TO usage_records;

CREATE INDEX usage_records_observed_at ON usage_records (observed_at);
