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
    -- seeded_at is stamped at CreateGeneration as harmless vestigial
    -- metadata. Under the scan-and-fill design there is no separate seed
    -- pass, and activation no longer gates on it (coverage — missing==0 —
    -- is the real gate). Retained only so the column stays populated for
    -- legacy display; no destructive migration drops it.
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

-- embed_watermark tracks the highest message id the scan-and-fill embed
-- worker has already swept for a generation, so each RunOnce resumes the
-- forward scan instead of re-scanning the whole messages table. It is a
-- pure optimization: losing it only makes the next scan start at id 0,
-- which is harmless (the scan predicate + idempotent upsert make
-- re-sweeping covered rows a no-op). The full-scan backstop ignores it.
-- See internal/vector/sqlitevec/schema.sql for the full contract.
CREATE TABLE IF NOT EXISTS embed_watermark (
    generation_id BIGINT PRIMARY KEY,
    watermark_id  BIGINT NOT NULL DEFAULT 0
);
