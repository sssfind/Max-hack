-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS processed_events (
    chat_id BIGINT NOT NULL,
    timestamp BIGINT NOT NULL,
    update_type VARCHAR(50) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (chat_id, timestamp, update_type)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS processed_events;
-- +goose StatementEnd