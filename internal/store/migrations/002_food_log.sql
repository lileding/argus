CREATE TABLE IF NOT EXISTS food_log (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    description TEXT NOT NULL,
    calories    INTEGER,
    meal_type   TEXT,
    logged_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_food_log_chat_date
    ON food_log (chat_id, logged_at);
