DROP TABLE notification_attempts;
DROP TABLE notification_outbox;
DROP TABLE incident_events;
DROP TABLE incidents;
DROP TABLE alert_rules;
UPDATE monitor_reliability_states SET
    consecutive_failures=LEAST(consecutive_failures,3),
    consecutive_successes=LEAST(consecutive_successes,2);
ALTER TABLE monitor_reliability_states
    DROP CONSTRAINT monitor_reliability_states_consecutive_failures_check,
    DROP CONSTRAINT monitor_reliability_states_consecutive_successes_check,
    ADD CONSTRAINT monitor_reliability_states_consecutive_failures_check
      CHECK (consecutive_failures BETWEEN 0 AND 3),
    ADD CONSTRAINT monitor_reliability_states_consecutive_successes_check
      CHECK (consecutive_successes BETWEEN 0 AND 2);
