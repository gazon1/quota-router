-- Canonical request/outcome events and routing telemetry (issue #23).
--
-- usage_records becomes the canonical request/outcome event: one row is one
-- attempt of one request. The new columns record what the reporter observed
-- about the attempt — the route it used, the task class, the normalized error
-- class, the stream state, the measured latency — and every one of them stays
-- NULL when the reporter did not supply it. A missing value is never zero.
--
-- Token provenance becomes per count: input and output carry the channel that
-- produced them, so an estimate is never merged with a reported count. The
-- single row-level provenance column is gone; legacy rows keep the ambiguous
-- "reported" value, because their exact channel was never recorded.
--
-- observed_at_reported distinguishes a source-supplied observation timestamp
-- from a server-stamped arrival time, and received_at records when the report
-- was stored. The two clocks are never reconciled: the gap between them is
-- the report delay.
--
-- routing_decisions gains the task class and whether the selected route was an
-- explicit manual override, so a route decision can be read without parsing
-- the payload JSON.

-- +goose Up
CREATE TABLE usage_records_v2 (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id    TEXT,
    -- 1-based; a retry is attempt 2, not a duplicate of attempt 1.
    attempt       INTEGER NOT NULL CHECK (attempt >= 1),
    -- Stable id of this event. Derived from (request_id, attempt) when the
    -- reporter supplies none; never a metric label.
    event_id      TEXT,
    schema_version INTEGER NOT NULL DEFAULT 1,
    provider      TEXT NOT NULL,
    model         TEXT,
    -- Bounded class of work, when the reporter named one.
    task_class    TEXT,
    -- controller: the controller selected the route. manual: the agent used
    -- its manual fallback because the controller could not answer.
    route_source  TEXT CHECK (route_source IS NULL OR route_source IN ('controller', 'manual')),
    input_tokens  INTEGER,
    input_provenance TEXT CHECK (input_provenance IS NULL OR input_provenance IN
        ('provider_reported', 'proxy_reported', 'agent_reported', 'estimated', 'reported')),
    output_tokens INTEGER,
    output_provenance TEXT CHECK (output_provenance IS NULL OR output_provenance IN
        ('provider_reported', 'proxy_reported', 'agent_reported', 'estimated', 'reported')),
    -- Channel that reported the row (a LiteLLM callback, an agent report, ...).
    source        TEXT NOT NULL,
    outcome       TEXT NOT NULL CHECK (outcome IN
        ('SUCCESS', 'FAILED_BEFORE_FIRST_TOKEN', 'PARTIAL_STREAM', 'CANCELLED', 'AMBIGUOUS', 'UNKNOWN')),
    -- Normalized failure reason; a bounded class, never a provider message.
    error_class   TEXT CHECK (error_class IS NULL OR error_class IN
        ('timeout', 'rate_limited', 'auth', 'invalid_request', 'provider_error', 'network', 'unknown')),
    -- not_streamed, complete or interrupted.
    stream_state  TEXT CHECK (stream_state IS NULL OR stream_state IN
        ('not_streamed', 'complete', 'interrupted')),
    -- Measured latency in milliseconds. NULL means "not measured".
    ttft_ms       INTEGER,
    latency_ms    INTEGER,
    -- Routing decision this attempt was routed by; NULL when unreported.
    decision_id   TEXT,
    -- When the source observed the attempt.
    observed_at   TEXT NOT NULL,
    -- 1 when the reporter supplied observed_at, 0 when the server stamped it.
    observed_at_reported INTEGER NOT NULL DEFAULT 0 CHECK (observed_at_reported IN (0, 1)),
    -- When this installation stored the report.
    received_at   TEXT NOT NULL,
    UNIQUE (request_id, attempt)
);

INSERT INTO usage_records_v2
    (id, request_id, attempt, event_id, schema_version, provider, model, task_class, route_source,
     input_tokens, input_provenance, output_tokens, output_provenance, source, outcome, error_class,
     stream_state, ttft_ms, latency_ms, decision_id, observed_at, observed_at_reported, received_at)
SELECT id, request_id, attempt, NULL, 1, provider, model, NULL, NULL,
       input_tokens,
       CASE WHEN input_tokens IS NULL THEN NULL
            WHEN provenance = 'estimated' THEN 'estimated' ELSE 'reported' END,
       output_tokens,
       CASE WHEN output_tokens IS NULL THEN NULL
            WHEN provenance = 'estimated' THEN 'estimated' ELSE 'reported' END,
       source, outcome, NULL, NULL, NULL, NULL, decision_id, observed_at, 0, observed_at
FROM usage_records;

DROP TABLE usage_records;
ALTER TABLE usage_records_v2 RENAME TO usage_records;

CREATE INDEX usage_records_observed_at ON usage_records (observed_at);
CREATE INDEX usage_records_provider_model_observed_at ON usage_records (provider, model, observed_at);
CREATE INDEX usage_records_decision_id ON usage_records (decision_id);

ALTER TABLE routing_decisions ADD COLUMN task_class TEXT;
ALTER TABLE routing_decisions ADD COLUMN explicit INTEGER NOT NULL DEFAULT 0 CHECK (explicit IN (0, 1));

-- +goose Down
-- Back to the shape of issue #14: one row per attempt with a single row-level
-- provenance. The per-count provenance is collapsed back to one value, which
-- is all the old shape could hold, and the routing telemetry columns go away.
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

INSERT INTO usage_records_v1
    (id, request_id, attempt, provider, model, input_tokens, output_tokens, provenance, source, outcome, decision_id, observed_at)
SELECT id, request_id, attempt, provider, model, input_tokens, output_tokens,
       CASE WHEN COALESCE(input_provenance, output_provenance) = 'estimated' THEN 'estimated' ELSE 'reported' END,
       source,
       -- The old shape cannot say "ambiguous"; an ambiguous attempt is an
       -- unknown outcome there, never a success.
       CASE WHEN outcome = 'AMBIGUOUS' THEN 'UNKNOWN' ELSE outcome END,
       decision_id, observed_at
FROM usage_records;

DROP TABLE usage_records;
ALTER TABLE usage_records_v1 RENAME TO usage_records;

CREATE INDEX usage_records_observed_at ON usage_records (observed_at);
CREATE INDEX usage_records_provider_model_observed_at ON usage_records (provider, model, observed_at);

-- The routing telemetry columns go away again. The table is rebuilt rather
-- than altered, because a column added with a CHECK constraint cannot be
-- dropped from the old SQLite versions this build still supports.
CREATE TABLE routing_decisions_v1 (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    decision_id    TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    selected_alias TEXT,
    selected_provider TEXT,
    payload        TEXT NOT NULL,
    created_at     TEXT NOT NULL
);

INSERT INTO routing_decisions_v1
    (id, decision_id, policy_version, session_id, selected_alias, selected_provider, payload, created_at)
SELECT id, decision_id, policy_version, session_id, selected_alias, selected_provider, payload, created_at
FROM routing_decisions;

DROP TABLE routing_decisions;
ALTER TABLE routing_decisions_v1 RENAME TO routing_decisions;

CREATE INDEX routing_decisions_session_created_at ON routing_decisions (session_id, created_at);
CREATE INDEX routing_decisions_decision_id ON routing_decisions (decision_id);
