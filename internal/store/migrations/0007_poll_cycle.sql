-- Polling cycle identity: the snapshot, the health observation and the poll
-- state anchor of one cycle carry the same cycle id, so a reader can never pair
-- a snapshot with a health observation or a freshness anchor from different
-- polling cycles. The cycle id is assigned by SavePollResult when a cycle is
-- committed, and every statement of that cycle writes the same value.
--
-- +goose Up
ALTER TABLE quota_snapshots ADD COLUMN poll_cycle_id INTEGER;
ALTER TABLE provider_health ADD COLUMN poll_cycle_id INTEGER;
ALTER TABLE poll_state ADD COLUMN poll_cycle_id INTEGER;

-- +goose Down
ALTER TABLE quota_snapshots DROP COLUMN poll_cycle_id;
ALTER TABLE provider_health DROP COLUMN poll_cycle_id;
ALTER TABLE poll_state DROP COLUMN poll_cycle_id;
