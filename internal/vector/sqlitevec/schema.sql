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
    -- seeded_at is stamped at CreateGeneration as harmless vestigial
    -- metadata. Under the scan-and-fill design there is no separate seed
    -- pass, and activation no longer gates on it (coverage — missing==0 —
    -- is the real gate). Retained only so the column stays populated for
    -- legacy display; no destructive migration drops it.
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

-- embed_watermark tracks the highest message id the scan-and-fill embed
-- worker has already swept for a generation, so each RunOnce resumes the
-- forward scan from where the last one stopped instead of re-scanning the
-- whole messages B-tree. It is a pure optimization: losing it (or never
-- seeding it) only makes the next scan start at id 0, which is harmless
-- because the scan predicate (embed_gen IS NULL OR embed_gen <> gen) and
-- the idempotent upsert make re-sweeping covered rows a no-op. The
-- full-scan backstop ignores this watermark entirely. No FK to messages
-- (those live in the main DB on SQLite); it lives here with the
-- generations it watermarks.
CREATE TABLE IF NOT EXISTS embed_watermark (
    generation_id INTEGER PRIMARY KEY,
    watermark_id  INTEGER NOT NULL DEFAULT 0
);
