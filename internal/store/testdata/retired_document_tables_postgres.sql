-- The retired document tables and cursor columns the test seeds, copied from
-- the parent branch's schema_pg.sql and dialect_pg.go.

CREATE TABLE IF NOT EXISTS document_extraction_profiles (
    id                    TEXT PRIMARY KEY,
    fingerprint           TEXT NOT NULL UNIQUE,
    provider              TEXT NOT NULL,
    endpoint              TEXT NOT NULL,
    region                TEXT NOT NULL,
    model                 TEXT NOT NULL,
    retention_posture     TEXT NOT NULL,
    training_posture      TEXT NOT NULL,
    allowed_media_types   JSONB NOT NULL,
    include_inline        BOOLEAN NOT NULL DEFAULT FALSE,
    policy_json           JSONB NOT NULL,
    enabled               BOOLEAN NOT NULL DEFAULT FALSE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    retired_at            TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS document_extraction_rebuilds (
    id                    TEXT PRIMARY KEY,
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    extraction_input_key  TEXT NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('building', 'completed')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at          TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS document_extractions (
    id                    TEXT PRIMARY KEY,
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    rebuild_id            TEXT REFERENCES document_extraction_rebuilds(id) ON DELETE SET NULL,
    canonical_blob_hash   TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    extraction_input_key  TEXT NOT NULL DEFAULT 'original',
    -- Media type of the occurrence the claim uploaded through. A head only
    -- covers its owner while the representative occurrence still selects
    -- this route; NULL predates route tracking and always matches.
    source_media_type     TEXT,
    state                 TEXT NOT NULL CHECK (state IN ('staging', 'ready', 'terminal', 'tombstoned')),
    lease_owner           TEXT,
    lease_fence           BIGINT NOT NULL DEFAULT 0,
    lease_until           TIMESTAMPTZ,
    attempt_count         INTEGER NOT NULL DEFAULT 0,
    request_count         INTEGER NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    retry_count           INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0 AND retry_count <= request_count),
    provider_latency_ms   BIGINT NOT NULL DEFAULT 0 CHECK (provider_latency_ms >= 0),
    next_retry_at         TIMESTAMPTZ,
    local_bytes           BIGINT NOT NULL,
    provider_bytes        BIGINT,
    units_processed       INTEGER,
    returned_model        TEXT,
    manifest_checksum     TEXT,
    normalization_version INTEGER,
    document_family       TEXT,
    unit_kind             TEXT,
    normalized_truncated  BOOLEAN NOT NULL DEFAULT FALSE,
    terminal_reason       TEXT,
    failure_reason        TEXT,
    failure_detail        TEXT,
    source_sequence       BIGINT NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    published_at          TIMESTAMPTZ,
    CHECK (local_bytes >= 0),
    CHECK (provider_bytes IS NULL OR provider_bytes >= 0),
    CHECK (units_processed IS NULL OR units_processed >= 0)
);

CREATE TABLE IF NOT EXISTS document_extraction_heads (
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    canonical_blob_hash   TEXT NOT NULL,
    extraction_input_key  TEXT NOT NULL,
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    source_sequence       BIGINT NOT NULL DEFAULT 0,
    switched_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (profile_id, canonical_blob_hash, extraction_input_key)
);

CREATE TABLE IF NOT EXISTS document_chunks (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    chunk_key             TEXT NOT NULL,
    ordinal               INTEGER NOT NULL,
    text                  TEXT NOT NULL,
    heading_path          JSONB NOT NULL,
    first_unit_index      INTEGER NOT NULL,
    last_unit_index       INTEGER NOT NULL,
    synthetic_prefix_len  INTEGER NOT NULL DEFAULT 0,
    checksum              TEXT NOT NULL,
    char_count            INTEGER NOT NULL,
    table_chunk           BOOLEAN NOT NULL DEFAULT FALSE,
    code_chunk            BOOLEAN NOT NULL DEFAULT FALSE,
    truncated             BOOLEAN NOT NULL DEFAULT FALSE,
    search_fts            TSVECTOR GENERATED ALWAYS AS (to_tsvector('simple', COALESCE(text, ''))) STORED,
    UNIQUE (extraction_id, chunk_key),
    UNIQUE (extraction_id, ordinal),
    CHECK (ordinal >= 0),
    CHECK (first_unit_index >= 0 AND last_unit_index >= first_unit_index),
    CHECK (synthetic_prefix_len >= 0 AND char_count >= 0)
);

CREATE TABLE IF NOT EXISTS document_occurrences (
    occurrence_key        TEXT PRIMARY KEY,
    attachment_id         BIGINT NOT NULL UNIQUE REFERENCES attachments(id) ON DELETE CASCADE,
    message_id            BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    source_id             BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    source_part_key       TEXT,
    stable_source_part    BOOLEAN NOT NULL DEFAULT FALSE,
    canonical_blob_hash   TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    filename              TEXT,
    mime_type             TEXT,
    attachment_role       TEXT NOT NULL,
    role_source           TEXT NOT NULL,
    source_sequence       BIGINT NOT NULL DEFAULT 0,
    reconciled_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS document_index_state (
    singleton             SMALLINT PRIMARY KEY CHECK (singleton = 1),
    revision              BIGINT NOT NULL DEFAULT 0,
    target_profile_id     TEXT,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO document_index_state(singleton, revision) VALUES (1, 0)
ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS document_vector_generations (
    id                           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    fingerprint                  TEXT NOT NULL,
    target_extraction_profile_id TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE RESTRICT,
    embedding_profile            TEXT NOT NULL,
    model                        TEXT NOT NULL,
    dimension                    INTEGER NOT NULL CHECK (dimension > 0),
    state                        TEXT NOT NULL CHECK (state IN ('building', 'active', 'retired')),
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    activated_at                 TIMESTAMPTZ,
    retired_at                   TIMESTAMPTZ
);

-- Foreign-key cascades can remove occurrences before asynchronous attachment
-- reconciliation observes the deletion. Invalidate search cursors at the
-- authoritative row mutation so every deletion path is covered.
CREATE OR REPLACE FUNCTION bump_document_index_revision_after_occurrence_delete()
RETURNS TRIGGER AS $$
BEGIN
    UPDATE document_index_state
    SET revision = revision + 1, updated_at = CURRENT_TIMESTAMP
    WHERE singleton = 1;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_document_occurrence_delete_revision ON document_occurrences;
CREATE TRIGGER trg_document_occurrence_delete_revision
AFTER DELETE ON document_occurrences
FOR EACH ROW EXECUTE FUNCTION bump_document_index_revision_after_occurrence_delete();

-- Message type is a document-search filter. Invalidate cursors when an indexed
-- occurrence moves between filter scopes, regardless of which importer made
-- the authoritative message update.
CREATE OR REPLACE FUNCTION bump_document_index_revision_after_message_type_update()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.message_type IS DISTINCT FROM NEW.message_type
       AND EXISTS (
           SELECT 1
           FROM document_occurrences o
           JOIN document_extraction_heads h
             ON h.canonical_blob_hash = o.canonical_blob_hash
           WHERE o.message_id = NEW.id
       ) THEN
        UPDATE document_index_state
        SET revision = revision + 1, updated_at = CURRENT_TIMESTAMP
        WHERE singleton = 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_document_message_type_revision ON messages;
CREATE TRIGGER trg_document_message_type_revision
AFTER UPDATE OF message_type ON messages
FOR EACH ROW EXECUTE FUNCTION bump_document_index_revision_after_message_type_update();

ALTER TABLE person_sweep_cursors ADD COLUMN IF NOT EXISTS optimistic_document_key TEXT NOT NULL DEFAULT '';
ALTER TABLE person_sweep_cursors ADD COLUMN IF NOT EXISTS reconcile_document_key TEXT NOT NULL DEFAULT '';
ALTER TABLE person_sweep_cursors ADD COLUMN IF NOT EXISTS backstop_document_key TEXT NOT NULL DEFAULT '';
