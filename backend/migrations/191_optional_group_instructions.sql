ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS optional_instructions_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS optional_instructions TEXT NOT NULL DEFAULT '';

ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS optional_instructions_enabled BOOLEAN NOT NULL DEFAULT FALSE;
