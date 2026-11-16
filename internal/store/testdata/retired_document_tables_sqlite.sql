-- The retired document tables and cursor columns the test seeds, copied from
-- the parent branch's schema.sql, schema_sqlite.sql, and dialect_sqlite.go.

-- Immutable hosted-extraction policy. A policy change creates a new profile;
-- mixed profiles may serve while an incremental rebuild is in progress.
CREATE TABLE IF NOT EXISTS document_extraction_profiles (
    id                    TEXT PRIMARY KEY,
    fingerprint           TEXT NOT NULL UNIQUE,
    provider              TEXT NOT NULL,
    endpoint              TEXT NOT NULL,
    region                TEXT NOT NULL,
    model                 TEXT NOT NULL,
    retention_posture     TEXT NOT NULL,
    training_posture      TEXT NOT NULL,
    allowed_media_types   JSON NOT NULL,
    include_inline        BOOLEAN NOT NULL DEFAULT FALSE,
    policy_json           JSON NOT NULL,
    enabled               BOOLEAN NOT NULL DEFAULT FALSE,
    created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    retired_at            DATETIME
);

CREATE TABLE IF NOT EXISTS document_extraction_rebuilds (
    id                    TEXT PRIMARY KEY,
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    extraction_input_key  TEXT NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('building', 'completed')),
    created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at          DATETIME
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
    lease_fence           INTEGER NOT NULL DEFAULT 0,
    lease_until           DATETIME,
    attempt_count         INTEGER NOT NULL DEFAULT 0,
    request_count         INTEGER NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    retry_count           INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0 AND retry_count <= request_count),
    provider_latency_ms   INTEGER NOT NULL DEFAULT 0 CHECK (provider_latency_ms >= 0),
    next_retry_at         DATETIME,
    local_bytes           INTEGER NOT NULL,
    provider_bytes        INTEGER,
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
    source_sequence       INTEGER NOT NULL DEFAULT 0,
    created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    published_at          DATETIME,
    CHECK (local_bytes >= 0),
    CHECK (provider_bytes IS NULL OR provider_bytes >= 0),
    CHECK (units_processed IS NULL OR units_processed >= 0)
);

-- The only serving pointer. Candidate revisions are unreachable until this
-- row switches in the same transaction that publishes their derivatives.
CREATE TABLE IF NOT EXISTS document_extraction_heads (
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    canonical_blob_hash   TEXT NOT NULL,
    extraction_input_key  TEXT NOT NULL,
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    source_sequence       INTEGER NOT NULL DEFAULT 0,
    switched_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (profile_id, canonical_blob_hash, extraction_input_key)
);

CREATE TABLE IF NOT EXISTS document_chunks (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    chunk_key             TEXT NOT NULL,
    ordinal               INTEGER NOT NULL,
    text                  TEXT NOT NULL,
    heading_path          JSON NOT NULL,
    first_unit_index      INTEGER NOT NULL,
    last_unit_index       INTEGER NOT NULL,
    synthetic_prefix_len  INTEGER NOT NULL DEFAULT 0,
    checksum              TEXT NOT NULL,
    char_count            INTEGER NOT NULL,
    table_chunk           BOOLEAN NOT NULL DEFAULT FALSE,
    code_chunk            BOOLEAN NOT NULL DEFAULT FALSE,
    truncated             BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (extraction_id, chunk_key),
    UNIQUE (extraction_id, ordinal),
    CHECK (ordinal >= 0),
    CHECK (first_unit_index >= 0 AND last_unit_index >= first_unit_index),
    CHECK (synthetic_prefix_len >= 0 AND char_count >= 0)
);

-- Occurrence identity is source-part based when authoritative. attachment_id
-- is only the current representative and may change after provider resync.
CREATE TABLE IF NOT EXISTS document_occurrences (
    occurrence_key        TEXT PRIMARY KEY,
    attachment_id         INTEGER NOT NULL UNIQUE REFERENCES attachments(id) ON DELETE CASCADE,
    message_id            INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    source_id             INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    source_part_key       TEXT,
    stable_source_part    BOOLEAN NOT NULL DEFAULT FALSE,
    canonical_blob_hash   TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    filename              TEXT,
    mime_type             TEXT,
    attachment_role       TEXT NOT NULL,
    role_source           TEXT NOT NULL,
    source_sequence       INTEGER NOT NULL DEFAULT 0,
    reconciled_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Monotonic search snapshot fence. Publication and occurrence reconciliation
-- increment it in their transaction; cursors bind to its observed value.
CREATE TABLE IF NOT EXISTS document_index_state (
    singleton             INTEGER PRIMARY KEY CHECK (singleton = 1),
    revision              INTEGER NOT NULL DEFAULT 0,
    target_profile_id     TEXT,
    updated_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT OR IGNORE INTO document_index_state(singleton, revision) VALUES (1, 0);

-- Document vectors are a corpus separate from message embeddings. The main
-- archive database owns generation and publication authority; vector backends
-- only store the opaque token below.
CREATE TABLE IF NOT EXISTS document_vector_generations (
    id                           INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint                  TEXT NOT NULL,
    target_extraction_profile_id TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE RESTRICT,
    embedding_profile            TEXT NOT NULL,
    model                        TEXT NOT NULL,
    dimension                    INTEGER NOT NULL CHECK (dimension > 0),
    state                        TEXT NOT NULL CHECK (state IN ('building', 'active', 'retired')),
    created_at                   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    activated_at                 DATETIME,
    retired_at                   DATETIME
);

-- Foreign-key cascades can remove occurrences before asynchronous attachment
-- reconciliation observes the deletion. Invalidate search cursors at the
-- authoritative row mutation so every deletion path is covered.
CREATE TRIGGER IF NOT EXISTS trg_document_occurrence_delete_revision
AFTER DELETE ON document_occurrences FOR EACH ROW
BEGIN
    UPDATE document_index_state
    SET revision = revision + 1, updated_at = CURRENT_TIMESTAMP
    WHERE singleton = 1;
END;

-- Message type is a document-search filter. Invalidate cursors when an indexed
-- occurrence moves between filter scopes, regardless of which importer made
-- the authoritative message update.
CREATE TRIGGER IF NOT EXISTS trg_document_message_type_revision
AFTER UPDATE OF message_type ON messages FOR EACH ROW
WHEN OLD.message_type IS NOT NEW.message_type
 AND EXISTS (
     SELECT 1
     FROM document_occurrences o
     JOIN document_extraction_heads h
       ON h.canonical_blob_hash = o.canonical_blob_hash
     WHERE o.message_id = NEW.id
 )
BEGIN
    UPDATE document_index_state
    SET revision = revision + 1, updated_at = CURRENT_TIMESTAMP
    WHERE singleton = 1;
END;

-- Document chunks keep canonical text in document_chunks. This external-
-- content index is a rebuildable derivative and never owns excerpts.
CREATE VIRTUAL TABLE IF NOT EXISTS document_chunks_fts USING fts5(
    text,
    content='document_chunks',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 1'
);

CREATE TRIGGER IF NOT EXISTS trg_document_chunks_fts_insert
AFTER INSERT ON document_chunks BEGIN
    INSERT INTO document_chunks_fts(rowid, text) VALUES (new.id, new.text);
END;

CREATE TRIGGER IF NOT EXISTS trg_document_chunks_fts_delete
AFTER DELETE ON document_chunks BEGIN
    INSERT INTO document_chunks_fts(document_chunks_fts, rowid, text)
    VALUES ('delete', old.id, old.text);
END;

CREATE TRIGGER IF NOT EXISTS trg_document_chunks_fts_update
AFTER UPDATE OF text ON document_chunks BEGIN
    INSERT INTO document_chunks_fts(document_chunks_fts, rowid, text)
    VALUES ('delete', old.id, old.text);
    INSERT INTO document_chunks_fts(rowid, text) VALUES (new.id, new.text);
END;

ALTER TABLE person_sweep_cursors ADD COLUMN optimistic_document_key TEXT NOT NULL DEFAULT '';
ALTER TABLE person_sweep_cursors ADD COLUMN reconcile_document_key TEXT NOT NULL DEFAULT '';
ALTER TABLE person_sweep_cursors ADD COLUMN backstop_document_key TEXT NOT NULL DEFAULT '';
