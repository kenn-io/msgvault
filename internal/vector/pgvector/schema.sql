-- pgvector backend schema. Parallel to internal/vector/sqlitevec/schema.sql
-- (spec §5.2), translated to PostgreSQL with the pgvector extension.
--
-- Unlike sqlitevec, the pgvector backend stores embeddings in the same
-- database as messages — there is no separate vectors.db. The CREATE
-- EXTENSION call is run by Migrate() prior to this file so that the
-- vector type below is resolvable.

CREATE TABLE IF NOT EXISTS index_generations (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    model         TEXT NOT NULL,
    dimension     INTEGER NOT NULL,
    fingerprint   TEXT NOT NULL,
    started_at    BIGINT NOT NULL,
    -- seeded_at marks when the initial pending_embeddings seed pass
    -- finished. NULL means "row inserted but seed never committed"
    -- (e.g. crash between insert and seed) — the resume path re-runs
    -- seedPending in that case rather than activating an empty
    -- generation.
    seeded_at     BIGINT,
    completed_at  BIGINT,
    activated_at  BIGINT,
    state         TEXT NOT NULL,
    message_count BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_generations_active
    ON index_generations(state) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS idx_generations_building
    ON index_generations(state) WHERE state = 'building';

-- Embedding storage. The vector column has no fixed dimension at the
-- table level so the same table can hold embeddings produced by
-- different models with different dimensions across generations.
-- Dimension is enforced at the application layer against
-- index_generations.dimension on Upsert and Search.
--
-- The dimension column lets HNSW indexes scope themselves to rows of a
-- known dim via `WHERE dimension = N`. Without the partial-index
-- guard, a 4-dim row would trip the (embedding::vector(768)) cast in
-- a 768-dim index and pgvector would reject the insert.
--
-- One row per chunk: long messages produce multiple rows distinguished
-- by chunk_index (0-based, dense), short messages keep a single row
-- with chunk_index = 0. The (generation_id, message_id, chunk_index)
-- primary key mirrors sqlitevec's UNIQUE constraint and preserves
-- idempotent re-upsert. chunk_char_start / chunk_char_end record the
-- rune-space offsets of the chunk in the preprocessed text — debugging
-- metadata today (search returns one Hit per message) that ships now so
-- chunk highlighting can be retro-fitted without another migration.
CREATE TABLE IF NOT EXISTS embeddings (
    generation_id    BIGINT NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
    message_id       BIGINT NOT NULL,
    chunk_index      INTEGER NOT NULL DEFAULT 0,
    embedded_at      BIGINT NOT NULL,
    source_char_len  INTEGER NOT NULL,
    chunk_char_start INTEGER NOT NULL DEFAULT 0,
    chunk_char_end   INTEGER NOT NULL DEFAULT 0,
    truncated        BOOLEAN NOT NULL DEFAULT FALSE,
    dimension        INTEGER NOT NULL,
    embedding        vector NOT NULL,
    PRIMARY KEY (generation_id, message_id, chunk_index)
);
CREATE INDEX IF NOT EXISTS idx_embeddings_msg ON embeddings(message_id);
CREATE INDEX IF NOT EXISTS idx_embeddings_dim ON embeddings(dimension);

CREATE TABLE IF NOT EXISTS pending_embeddings (
    generation_id BIGINT NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
    message_id    BIGINT NOT NULL,
    enqueued_at   BIGINT NOT NULL,
    claimed_at    BIGINT,
    claim_token   TEXT,
    PRIMARY KEY (generation_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_pending_available
    ON pending_embeddings(generation_id, message_id) WHERE claimed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_pending_claims
    ON pending_embeddings(claimed_at) WHERE claimed_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS embed_runs (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    generation_id BIGINT NOT NULL REFERENCES index_generations(id),
    started_at    BIGINT NOT NULL,
    ended_at      BIGINT,
    claimed       INTEGER NOT NULL DEFAULT 0,
    succeeded     INTEGER NOT NULL DEFAULT 0,
    failed        INTEGER NOT NULL DEFAULT 0,
    truncated     INTEGER NOT NULL DEFAULT 0,
    error         TEXT
);
