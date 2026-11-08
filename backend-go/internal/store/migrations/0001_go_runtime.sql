-- 0001_go_runtime.sql — additive schema for the Go backend (MIGRATION_PLAN 5.2, 5.3, 7.4, 8.2).
--
-- REQUIRES HUMAN REVIEW against the real Neon catalog before any production run
-- (plan 8.1: column types, existing constraints/indexes, duplicates, row counts).
-- Every statement is additive and idempotent: no DROP, no DELETE, no rewrite of
-- existing rows, no change to existing columns. Legacy Java rows keep epoch NULL
-- and are ignored by the Go backend (they must be drained, plan 8.3).
-- Applied only through `cmd/admin migrate -confirm`, never at server start-up.

-- Room runtime state (replaces the Java in-memory maps; no parallel checkpoint table).
ALTER TABLE game_session ADD COLUMN IF NOT EXISTS epoch uuid;
ALTER TABLE game_session ADD COLUMN IF NOT EXISTS state_version bigint NOT NULL DEFAULT 0;
ALTER TABLE game_session ADD COLUMN IF NOT EXISTS state_schema_version integer NOT NULL DEFAULT 1;
ALTER TABLE game_session ADD COLUMN IF NOT EXISTS runtime_state jsonb;
ALTER TABLE game_session ADD COLUMN IF NOT EXISTS next_deadline_at timestamptz;
ALTER TABLE game_session ADD COLUMN IF NOT EXISTS last_activity_at timestamptz;
ALTER TABLE game_session ADD COLUMN IF NOT EXISTS closed_at timestamptz;
ALTER TABLE game_session ADD COLUMN IF NOT EXISTS host_user_id bigint;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'game_session_host_user_id_fkey') THEN
        ALTER TABLE game_session
            ADD CONSTRAINT game_session_host_user_id_fkey
            FOREIGN KEY (host_user_id) REFERENCES users (id) ON DELETE SET NULL;
    END IF;
END $$;

-- One active (open, Go-managed) room per public code; closed rooms and legacy
-- rows keep their codes without blocking reuse. Epoch distinguishes generations.
CREATE UNIQUE INDEX IF NOT EXISTS game_session_active_code_key
    ON game_session (session_code) WHERE closed_at IS NULL AND epoch IS NOT NULL;

-- Authentication (SHA-256 of a 32-byte random bearer; the token itself is never stored)
-- and multi-connection presence leases.
ALTER TABLE users ADD COLUMN IF NOT EXISTS token_hash bytea;
ALTER TABLE users ADD COLUMN IF NOT EXISTS token_expires_at timestamptz;
ALTER TABLE users ADD COLUMN IF NOT EXISTS created_at timestamptz;
ALTER TABLE users ADD COLUMN IF NOT EXISTS connection_leases jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE users ADD COLUMN IF NOT EXISTS disconnected_at timestamptz;

CREATE UNIQUE INDEX IF NOT EXISTS users_token_hash_key ON users (token_hash) WHERE token_hash IS NOT NULL;

-- Relation/query indexes (plan 8.2). Non-unique. Created only when no index
-- with the same leading columns exists: the real database (inspected
-- 2026-11-08) already has idx_users_game_session, idx_question_session_round
-- and idx_shown_questions_session, so no duplicates are added there.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index i
        WHERE i.indrelid = 'users'::regclass
          AND i.indkey[0] = (SELECT attnum FROM pg_attribute WHERE attrelid = 'users'::regclass AND attname = 'game_session_id')
    ) THEN
        CREATE INDEX users_game_session_id_idx ON users (game_session_id);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_index i
        WHERE i.indrelid = 'question'::regclass
          AND i.indkey[0] = (SELECT attnum FROM pg_attribute WHERE attrelid = 'question'::regclass AND attname = 'game_session_id')
          AND i.indkey[1] = (SELECT attnum FROM pg_attribute WHERE attrelid = 'question'::regclass AND attname = 'round_id')
    ) THEN
        CREATE INDEX question_session_round_idx ON question (game_session_id, round_id, id);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_index i
        WHERE i.indrelid = 'shown_questions'::regclass
          AND i.indkey[0] = (SELECT attnum FROM pg_attribute WHERE attrelid = 'shown_questions'::regclass AND attname = 'session_id')
    ) THEN
        CREATE INDEX shown_questions_session_idx ON shown_questions (session_id, question_id);
    END IF;
END $$;

-- Idempotency receipts: the only new infrastructure table (plan 5.3). No FK to
-- users so a receipt survives the actor being kicked/removed.
CREATE TABLE IF NOT EXISTS operation_receipt (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope             text        NOT NULL,
    actor_id          bigint      NOT NULL,
    key_hash          bytea       NOT NULL,
    request_hash      bytea       NOT NULL,
    operation         text        NOT NULL,
    committed_version bigint,
    result            jsonb       NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    CONSTRAINT operation_receipt_scope_actor_key UNIQUE (scope, actor_id, key_hash)
);
CREATE INDEX IF NOT EXISTS operation_receipt_expires_idx ON operation_receipt (expires_at);
