-- Test-only fixture for the store DB tests: the tables the SDK reads, as
-- pen-gpt's CI fixture (db/sql/apen_ci_fixture.sql) and stream_runs migration
-- (db/migrations/apen/0001_stream_runs.sql) create them. {schema} is the
-- product schema under test.
DROP TABLE IF EXISTS {schema}.stream_runs, {schema}.share_links, {schema}.response_feedback,
    {schema}.thread_pin, {schema}.mastra_messages, {schema}.mastra_threads;
CREATE SCHEMA IF NOT EXISTS {schema};
CREATE TABLE IF NOT EXISTS {schema}.mastra_threads (
    id TEXT PRIMARY KEY,
    "resourceId" TEXT NOT NULL,
    title TEXT,
    metadata TEXT,
    "createdAt" TIMESTAMP NOT NULL,
    "updatedAt" TIMESTAMP NOT NULL,
    "createdAtZ" TIMESTAMPTZ DEFAULT NOW(),
    "updatedAtZ" TIMESTAMPTZ DEFAULT NOW(),
    "deletedAt" TIMESTAMP
);
CREATE TABLE IF NOT EXISTS {schema}.mastra_messages (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL,
    content TEXT NOT NULL,
    role TEXT NOT NULL,
    type TEXT NOT NULL,
    "createdAt" TIMESTAMP NOT NULL,
    "resourceId" TEXT,
    "createdAtZ" TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS {schema}.thread_pin (
    user_id VARCHAR NOT NULL,
    thread_id VARCHAR NOT NULL,
    is_deleted BOOLEAN NOT NULL DEFAULT FALSE,
    pinned_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, thread_id)
);
CREATE TABLE IF NOT EXISTS {schema}.response_feedback (
    thread_id VARCHAR NOT NULL,
    message_id VARCHAR NOT NULL,
    user_id VARCHAR NOT NULL,
    feedback_type VARCHAR(10) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (message_id, thread_id, user_id)
);
CREATE TABLE IF NOT EXISTS {schema}.share_links (
    id VARCHAR PRIMARY KEY,
    reference_id VARCHAR NOT NULL,
    user_id VARCHAR NOT NULL,
    short_code VARCHAR,
    created_at TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    type VARCHAR NOT NULL
);
CREATE TABLE {schema}.stream_runs (
    run_id                UUID PRIMARY KEY,
    user_id               VARCHAR(255) NOT NULL,
    -- No foreign key: mastra_threads belongs to Mastra, threads are only
    -- soft-deleted, and the thread may not be stored yet when the run starts.
    thread_id             VARCHAR(255) NOT NULL,
    user_message_id       UUID NOT NULL,
    status                TEXT NOT NULL,
    cleanup_status        TEXT NOT NULL,
    started_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Refreshed by the owning instance while the run is active; reconciliation
    -- fails an active run whose heartbeat went stale.
    heartbeat_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    stop_requested_at     TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    terminal_at           TIMESTAMPTZ,
    cleanup_attempts      INTEGER NOT NULL DEFAULT 0,
    cleanup_next_retry_at TIMESTAMPTZ,

    CONSTRAINT stream_runs_status
        CHECK (status IN ('running', 'completing', 'completed', 'stopped', 'superseded', 'failed')),
    CONSTRAINT stream_runs_cleanup_status
        CHECK (cleanup_status IN ('not_needed', 'pending', 'done', 'failed')),
    -- Each status carries exactly its own timestamp. cleanup_status says whether
    -- the turn's assistant messages are (to be) removed: never for running,
    -- completing or completed; always for stopped and superseded. A failed turn
    -- has nothing to remove when its anchor was never written or when it failed
    -- in completing, where the finished answer is kept.
    CONSTRAINT stream_runs_status_fields
        CHECK (
            (status IN ('running', 'completing')
                AND cleanup_status = 'not_needed'
                AND stop_requested_at IS NULL AND completed_at IS NULL AND terminal_at IS NULL)
            OR (status = 'completed'
                AND cleanup_status = 'not_needed'
                AND completed_at IS NOT NULL AND stop_requested_at IS NULL AND terminal_at IS NULL)
            OR (status = 'stopped'
                AND cleanup_status IN ('pending', 'done', 'failed')
                AND stop_requested_at IS NOT NULL AND completed_at IS NULL AND terminal_at IS NULL)
            OR (status = 'superseded'
                AND cleanup_status IN ('pending', 'done', 'failed')
                AND terminal_at IS NOT NULL AND stop_requested_at IS NULL AND completed_at IS NULL)
            OR (status = 'failed'
                AND terminal_at IS NOT NULL AND stop_requested_at IS NULL AND completed_at IS NULL)
        ),
    CONSTRAINT stream_runs_cleanup_fields
        CHECK (
            cleanup_attempts >= 0
            AND (cleanup_status <> 'not_needed'
                OR (cleanup_attempts = 0 AND cleanup_next_retry_at IS NULL))
            AND (cleanup_status <> 'done' OR cleanup_next_retry_at IS NULL)
        ),
    -- Each turn anchors its own server-generated user message.
    CONSTRAINT stream_runs_user_message UNIQUE (user_message_id)
);
