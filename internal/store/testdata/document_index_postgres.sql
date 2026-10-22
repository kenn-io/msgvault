-- Document schema before the Docbank processing migration.

CREATE TABLE IF NOT EXISTS document_extraction_runs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invocation_key TEXT NOT NULL UNIQUE CHECK (octet_length(invocation_key) BETWEEN 1 AND 128 AND btrim(invocation_key) = invocation_key),
    trigger TEXT NOT NULL CHECK (trigger IN ('manual', 'scheduled')),
    state TEXT NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'succeeded', 'partial', 'failed', 'cancelled')),
    started_at TIMESTAMPTZ NOT NULL, finished_at TIMESTAMPTZ,
    error_code TEXT CHECK (error_code IS NULL OR error_code IN ('invocation_cancelled', 'invocation_timeout', 'invocation_rate_limited', 'invocation_authentication_failed', 'invocation_upstream_failed', 'invocation_invalid_output', 'invocation_safety_limit', 'invocation_archive_drift', 'invocation_daemon_restarted', 'invocation_internal', 'invocation_unsafe_error_redacted')),
    attempted BIGINT NOT NULL DEFAULT 0 CHECK (attempted >= 0),
    succeeded BIGINT NOT NULL DEFAULT 0 CHECK (succeeded >= 0),
    failed BIGINT NOT NULL DEFAULT 0 CHECK (failed >= 0),
    CHECK ((state = 'running' AND finished_at IS NULL AND error_code IS NULL) OR (state <> 'running' AND finished_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_document_extraction_runs_operations_order ON document_extraction_runs(started_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_document_extraction_runs_active ON document_extraction_runs(state, id) WHERE state = 'running';

CREATE TABLE IF NOT EXISTS document_embedding_runs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invocation_key TEXT NOT NULL UNIQUE CHECK (octet_length(invocation_key) BETWEEN 1 AND 128 AND btrim(invocation_key) = invocation_key),
    trigger TEXT NOT NULL CHECK (trigger IN ('manual', 'scheduled')),
    state TEXT NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'succeeded', 'partial', 'failed', 'cancelled')),
    started_at TIMESTAMPTZ NOT NULL, finished_at TIMESTAMPTZ,
    error_code TEXT CHECK (error_code IS NULL OR error_code IN ('invocation_cancelled', 'invocation_timeout', 'invocation_rate_limited', 'invocation_authentication_failed', 'invocation_upstream_failed', 'invocation_invalid_output', 'invocation_safety_limit', 'invocation_archive_drift', 'invocation_daemon_restarted', 'invocation_internal', 'invocation_unsafe_error_redacted')),
    attempted BIGINT NOT NULL DEFAULT 0 CHECK (attempted >= 0),
    succeeded BIGINT NOT NULL DEFAULT 0 CHECK (succeeded >= 0),
    failed BIGINT NOT NULL DEFAULT 0 CHECK (failed >= 0),
    CHECK ((state = 'running' AND finished_at IS NULL AND error_code IS NULL) OR (state <> 'running' AND finished_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_document_embedding_runs_operations_order ON document_embedding_runs(started_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_document_embedding_runs_active ON document_embedding_runs(state, id) WHERE state = 'running';

DROP TRIGGER IF EXISTS trg_document_extraction_runs_terminal_immutable ON document_extraction_runs;

CREATE TRIGGER trg_document_extraction_runs_terminal_immutable BEFORE UPDATE ON document_extraction_runs FOR EACH ROW EXECUTE FUNCTION reject_terminal_operation_invocation_update();

DROP TRIGGER IF EXISTS trg_document_embedding_runs_terminal_immutable ON document_embedding_runs;

CREATE TRIGGER trg_document_embedding_runs_terminal_immutable BEFORE UPDATE ON document_embedding_runs FOR EACH ROW EXECUTE FUNCTION reject_terminal_operation_invocation_update();

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

CREATE TABLE IF NOT EXISTS document_provider_consents (
    profile_id            TEXT PRIMARY KEY REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    profile_fingerprint   TEXT NOT NULL,
    retention_posture     TEXT NOT NULL,
    training_posture      TEXT NOT NULL,
    consented_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS document_extraction_rebuilds (
    id                    TEXT PRIMARY KEY,
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    extraction_input_key  TEXT NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('building', 'completed')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at          TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_document_extraction_rebuilds_active
    ON document_extraction_rebuilds(profile_id, extraction_input_key)
    WHERE state = 'building';

CREATE TABLE IF NOT EXISTS document_extraction_rebuild_targets (
    rebuild_id            TEXT NOT NULL REFERENCES document_extraction_rebuilds(id) ON DELETE CASCADE,
    canonical_blob_hash   TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    PRIMARY KEY (rebuild_id, canonical_blob_hash)
);

CREATE TABLE IF NOT EXISTS document_extractions (
    id                    TEXT PRIMARY KEY,
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    rebuild_id            TEXT REFERENCES document_extraction_rebuilds(id) ON DELETE SET NULL,
    canonical_blob_hash   TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    extraction_input_key  TEXT NOT NULL DEFAULT 'original',



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

CREATE INDEX IF NOT EXISTS idx_document_extractions_owner
    ON document_extractions(profile_id, canonical_blob_hash, extraction_input_key, state);

CREATE INDEX IF NOT EXISTS idx_document_extractions_lease
    ON document_extractions(state, lease_until);

CREATE UNIQUE INDEX IF NOT EXISTS idx_document_extractions_vector_identity
    ON document_extractions(id, profile_id, canonical_blob_hash, extraction_input_key, source_sequence);

CREATE TABLE IF NOT EXISTS document_extraction_conversions (
    extraction_id        TEXT PRIMARY KEY REFERENCES document_extractions(id) ON DELETE CASCADE,
    provider_media_type TEXT NOT NULL,
    pdf_sha256           TEXT NOT NULL CHECK (length(pdf_sha256) = 64),
    pdf_bytes            BIGINT NOT NULL CHECK (pdf_bytes > 0),
    pages                INTEGER NOT NULL CHECK (pages > 0),
    policy_fingerprint   TEXT NOT NULL,
    converter_version    TEXT NOT NULL,
    spans                JSONB NOT NULL
);

CREATE TABLE IF NOT EXISTS document_extraction_claims (
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    canonical_blob_hash   TEXT NOT NULL,
    extraction_input_key  TEXT NOT NULL,
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    lease_owner           TEXT NOT NULL,
    lease_fence           BIGINT NOT NULL,
    lease_until           TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (profile_id, canonical_blob_hash, extraction_input_key)
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

CREATE TABLE IF NOT EXISTS document_units (
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    unit_index            INTEGER NOT NULL,
    unit_kind             TEXT NOT NULL,
    text                  TEXT NOT NULL,
    header_text           TEXT,
    footer_text           TEXT,
    width                 INTEGER,
    height                INTEGER,
    dpi                    INTEGER,
    checksum              TEXT NOT NULL,
    char_count            INTEGER NOT NULL,
    truncated             BOOLEAN NOT NULL DEFAULT FALSE,
    heading_marks         JSONB NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (extraction_id, unit_index),
    CHECK (unit_index >= 0),
    CHECK (char_count >= 0)
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

CREATE UNIQUE INDEX IF NOT EXISTS idx_document_chunks_vector_identity
    ON document_chunks(id, extraction_id, chunk_key, checksum);

CREATE INDEX IF NOT EXISTS idx_document_chunks_search_fts
    ON document_chunks USING GIN(search_fts);

CREATE TABLE IF NOT EXISTS document_chunk_spans (
    extraction_id         TEXT NOT NULL,
    chunk_key             TEXT NOT NULL,
    span_ordinal          INTEGER NOT NULL,
    unit_index            INTEGER NOT NULL,
    start_char            INTEGER NOT NULL,
    end_char              INTEGER NOT NULL,
    synthetic             BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (extraction_id, chunk_key, span_ordinal),
    FOREIGN KEY (extraction_id, chunk_key)
        REFERENCES document_chunks(extraction_id, chunk_key) ON DELETE CASCADE,
    FOREIGN KEY (extraction_id, unit_index)
        REFERENCES document_units(extraction_id, unit_index) ON DELETE CASCADE,
    CHECK (span_ordinal >= 0),
    CHECK (start_char >= 0 AND end_char >= start_char)
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

CREATE INDEX IF NOT EXISTS idx_document_occurrences_hash
    ON document_occurrences(canonical_blob_hash, message_id, occurrence_key);

CREATE INDEX IF NOT EXISTS idx_document_occurrences_source
    ON document_occurrences(source_id, message_id);

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

CREATE UNIQUE INDEX IF NOT EXISTS idx_document_vector_generations_building
    ON document_vector_generations(state) WHERE state = 'building';

CREATE UNIQUE INDEX IF NOT EXISTS idx_document_vector_generations_active
    ON document_vector_generations(state) WHERE state = 'active';

CREATE INDEX IF NOT EXISTS idx_document_vector_generations_live_fingerprint
    ON document_vector_generations(fingerprint) WHERE state <> 'retired';

CREATE TABLE IF NOT EXISTS document_vector_consents (
    egress_fingerprint           TEXT PRIMARY KEY,
    purpose                      TEXT NOT NULL CHECK (purpose IN ('document_embedding', 'query_embedding')),
    generation_fingerprint       TEXT NOT NULL,
    target_extraction_profile_id  TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE RESTRICT,
    embedding_profile            TEXT NOT NULL,
    model                        TEXT NOT NULL,
    dimension                    INTEGER NOT NULL CHECK (dimension > 0),
    consented_at                 TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS document_vector_provider_usage (
    fingerprint          TEXT PRIMARY KEY,
    provider_calls       BIGINT NOT NULL DEFAULT 0 CHECK (provider_calls >= 0),
    provider_documents   BIGINT NOT NULL DEFAULT 0 CHECK (provider_documents >= 0),
    provider_chunks      BIGINT NOT NULL DEFAULT 0 CHECK (provider_chunks >= 0),
    provider_input_chars BIGINT NOT NULL DEFAULT 0 CHECK (provider_input_chars >= 0),
    updated_at           TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS document_vector_build_progress (
    generation_id  BIGINT PRIMARY KEY REFERENCES document_vector_generations(id) ON DELETE CASCADE,
    after_chunk_id BIGINT NOT NULL CHECK (after_chunk_id > 0),
    updated_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS document_vector_publications (
    generation_id                BIGINT NOT NULL REFERENCES document_vector_generations(id) ON DELETE RESTRICT,
    extraction_id                TEXT NOT NULL,
    extraction_profile_id        TEXT NOT NULL,
    canonical_blob_hash          TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    extraction_input_key         TEXT NOT NULL,
    chunk_id                     BIGINT NOT NULL,
    chunk_key                    TEXT NOT NULL,
    chunk_checksum               TEXT NOT NULL,
    source_sequence              BIGINT NOT NULL,
    token                        TEXT NOT NULL UNIQUE,
    state                        TEXT NOT NULL CHECK (state IN ('pending', 'ready', 'failed')),
    lease_owner                  TEXT,
    lease_fence                  BIGINT NOT NULL DEFAULT 0,
    lease_until                  TIMESTAMPTZ,
    attempt_count                INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_retry_at                TIMESTAMPTZ,
    error_code                   TEXT,
    backend_cleaned_at           TIMESTAMPTZ,
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (generation_id, extraction_id, chunk_id)
);

CREATE INDEX IF NOT EXISTS idx_document_vector_publications_cleanup
    ON document_vector_publications(generation_id, backend_cleaned_at, token);

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

DROP TRIGGER IF EXISTS trg_operation_history_document_extraction_insert ON document_extraction_runs;

CREATE TRIGGER trg_operation_history_document_extraction_insert AFTER INSERT ON document_extraction_runs
FOR EACH ROW EXECUTE FUNCTION advance_operation_history_membership_revision();

DROP TRIGGER IF EXISTS trg_operation_history_document_extraction_delete ON document_extraction_runs;

CREATE TRIGGER trg_operation_history_document_extraction_delete AFTER DELETE ON document_extraction_runs
FOR EACH ROW EXECUTE FUNCTION advance_operation_history_membership_revision();

DROP TRIGGER IF EXISTS trg_operation_history_document_extraction_update ON document_extraction_runs;

CREATE TRIGGER trg_operation_history_document_extraction_update AFTER UPDATE ON document_extraction_runs
FOR EACH ROW WHEN (OLD.started_at IS DISTINCT FROM NEW.started_at OR OLD.state IS DISTINCT FROM NEW.state)
EXECUTE FUNCTION advance_operation_history_membership_revision();

DROP TRIGGER IF EXISTS trg_operation_history_document_embedding_insert ON document_embedding_runs;

CREATE TRIGGER trg_operation_history_document_embedding_insert AFTER INSERT ON document_embedding_runs
FOR EACH ROW EXECUTE FUNCTION advance_operation_history_membership_revision();

DROP TRIGGER IF EXISTS trg_operation_history_document_embedding_delete ON document_embedding_runs;

CREATE TRIGGER trg_operation_history_document_embedding_delete AFTER DELETE ON document_embedding_runs
FOR EACH ROW EXECUTE FUNCTION advance_operation_history_membership_revision();

DROP TRIGGER IF EXISTS trg_operation_history_document_embedding_update ON document_embedding_runs;

CREATE TRIGGER trg_operation_history_document_embedding_update AFTER UPDATE ON document_embedding_runs
FOR EACH ROW WHEN (OLD.started_at IS DISTINCT FROM NEW.started_at OR OLD.state IS DISTINCT FROM NEW.state)
EXECUTE FUNCTION advance_operation_history_membership_revision();
