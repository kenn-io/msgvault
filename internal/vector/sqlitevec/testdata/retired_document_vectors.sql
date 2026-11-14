-- Independent attachment-document vectors. Publication tokens are globally
-- unique opaque identities; generation IDs belong to the main document-vector
-- ledger and intentionally do not reference message index_generations.
CREATE TABLE IF NOT EXISTS document_vector_embeddings (
    document_vector_id INTEGER PRIMARY KEY AUTOINCREMENT,
    token              TEXT NOT NULL UNIQUE,
    generation_id      INTEGER NOT NULL,
    dimension          INTEGER NOT NULL CHECK (dimension > 0)
);
CREATE INDEX IF NOT EXISTS idx_document_vector_embeddings_generation
    ON document_vector_embeddings(generation_id, dimension, token);
