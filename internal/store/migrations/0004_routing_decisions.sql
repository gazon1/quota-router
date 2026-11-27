-- Routing decision audit trail. Each row records one selection decision returned
-- by the controller. The payload is the machine-readable decision JSON; it is
-- written after the decision is made and must not influence selection.

-- +goose Up
CREATE TABLE routing_decisions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    decision_id    TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    selected_alias TEXT,
    selected_provider TEXT,
    payload        TEXT NOT NULL,
    created_at     TEXT NOT NULL
);

CREATE INDEX routing_decisions_session_created_at ON routing_decisions (session_id, created_at);
CREATE INDEX routing_decisions_decision_id ON routing_decisions (decision_id);

-- +goose Down
DROP TABLE routing_decisions;
