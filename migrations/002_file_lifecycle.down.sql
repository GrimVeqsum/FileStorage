BEGIN;

-- Dropping lifecycle information would resurrect incomplete/deleted files.
-- Deliberately refuse rollback until the operator resolves these rows explicitly.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM files WHERE state <> 'ready') THEN
        RAISE EXCEPTION 'cannot roll back lifecycle migration while non-ready file records exist';
    END IF;
END $$;

DROP INDEX idx_files_cleanup;
DROP INDEX idx_files_ready_created_at;
ALTER TABLE files
    DROP CONSTRAINT files_state_check,
    DROP COLUMN state,
    DROP COLUMN updated_at,
    DROP COLUMN next_retry_at;

COMMIT;
