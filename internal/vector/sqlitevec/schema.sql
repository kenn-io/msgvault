-- vectors.db schema. See spec §5.2.

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY
);
INSERT OR IGNORE INTO schema_version VALUES (1);

CREATE TABLE IF NOT EXISTS index_generations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    model         TEXT NOT NULL,
    dimension     INTEGER NOT NULL,
    fingerprint   TEXT NOT NULL,
    started_at    INTEGER NOT NULL,
    -- seeded_at marks when the initial pending_embeddings seed pass
    -- finished. NULL means "row inserted but seed never committed"
    -- (e.g. crash between insert and seed) — the resume path re-runs
    -- seedPending in that case rather than activating an empty
    -- generation. Columns added after release ship via the ALTER
    -- TABLE migrations in migrate.go for already-initialized databases.
    seeded_at     INTEGER,
    completed_at  INTEGER,
    activated_at  INTEGER,
    state         TEXT NOT NULL,
    message_count INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_generations_active
    ON index_generations(state) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS idx_generations_building
    ON index_generations(state) WHERE state = 'building';

-- embeddings.embedding_id is the synthetic rowid that joins to the
-- dimension-specific vec0 virtual table (vectors_vec_dN). One row per
-- chunk: long messages produce multiple rows distinguished by
-- chunk_index (0-based, dense), short messages keep a single row with
-- chunk_index = 0. The UNIQUE constraint on (generation_id, message_id,
-- chunk_index) preserves idempotent re-upsert.
--
-- chunk_char_start / chunk_char_end record the rune-space offsets into
-- the preprocessed text for that chunk. They are debugging metadata
-- today (search returns one Hit per message; "which chunk matched" is
-- not yet plumbed to the UI) but ship with the schema so retro-fitting
-- chunk highlighting later does not require another migration.
CREATE TABLE IF NOT EXISTS embeddings (
    embedding_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    generation_id    INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
    message_id       INTEGER NOT NULL,
    chunk_index      INTEGER NOT NULL DEFAULT 0,
    embedded_at      INTEGER NOT NULL,
    source_char_len  INTEGER NOT NULL,
    chunk_char_start INTEGER NOT NULL DEFAULT 0,
    chunk_char_end   INTEGER NOT NULL DEFAULT 0,
    truncated        INTEGER NOT NULL DEFAULT 0,
    UNIQUE (generation_id, message_id, chunk_index)
);
CREATE INDEX IF NOT EXISTS idx_embeddings_msg ON embeddings(message_id);
CREATE INDEX IF NOT EXISTS idx_embeddings_gen_msg ON embeddings(generation_id, message_id);

CREATE TABLE IF NOT EXISTS pending_embeddings (
    generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
    message_id    INTEGER NOT NULL,
    enqueued_at   INTEGER NOT NULL,
    claimed_at    INTEGER,
    claim_token   TEXT,
    PRIMARY KEY (generation_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_pending_available
    ON pending_embeddings(generation_id, message_id) WHERE claimed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_pending_claims
    ON pending_embeddings(claimed_at) WHERE claimed_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS embed_runs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    generation_id INTEGER NOT NULL REFERENCES index_generations(id),
    started_at    INTEGER NOT NULL,
    ended_at      INTEGER,
    claimed       INTEGER NOT NULL DEFAULT 0,
    succeeded     INTEGER NOT NULL DEFAULT 0,
    failed        INTEGER NOT NULL DEFAULT 0,
    truncated     INTEGER NOT NULL DEFAULT 0,
    error         TEXT
);
