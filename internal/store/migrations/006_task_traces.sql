-- Link traces to async task execution.

ALTER TABLE traces
    ADD COLUMN IF NOT EXISTS task_id BIGINT REFERENCES tasks(id),
    ADD COLUMN IF NOT EXISTS parent_task_id BIGINT REFERENCES tasks(id);

CREATE INDEX IF NOT EXISTS idx_traces_task
    ON traces (task_id)
    WHERE task_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_traces_parent_task
    ON traces (parent_task_id)
    WHERE parent_task_id IS NOT NULL;
