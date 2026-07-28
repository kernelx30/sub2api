-- Store the user-selected second OpenAI group for API-key-level failover.
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS openai_availability_fallback_group_id BIGINT REFERENCES groups(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_api_keys_openai_availability_fallback_group_id
    ON api_keys(openai_availability_fallback_group_id)
    WHERE deleted_at IS NULL;
