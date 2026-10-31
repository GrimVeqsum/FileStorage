BEGIN;

-- Existing files stay visible and keep their existing object keys.
ALTER TABLE files
    ADD COLUMN state TEXT NOT NULL DEFAULT 'ready',
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN next_retry_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD CONSTRAINT files_state_check CHECK (state IN ('uploading', 'ready', 'deleting', 'deleted'));

CREATE INDEX idx_files_cleanup ON files (next_retry_at, id) WHERE state <> 'ready';
CREATE INDEX idx_files_ready_created_at ON files (created_at DESC, id) WHERE state = 'ready';

COMMIT;
