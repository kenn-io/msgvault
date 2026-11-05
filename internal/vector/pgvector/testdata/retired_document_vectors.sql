-- Independent attachment-document vectors. Tokens are opaque globally unique
-- publication identities; generation IDs intentionally do not reference the
-- message-vector index_generations table.
CREATE TABLE IF NOT EXISTS document_vector_backend_generations (
    generation_id BIGINT PRIMARY KEY,
    dimension     INTEGER NOT NULL CHECK (dimension > 0),
    UNIQUE (generation_id, dimension)
);

CREATE TABLE IF NOT EXISTS document_vector_embeddings (
    token         TEXT PRIMARY KEY,
    generation_id BIGINT NOT NULL,
    dimension     INTEGER NOT NULL CHECK (dimension > 0),
    embedding     vector NOT NULL,
    CONSTRAINT document_vector_embeddings_generation_dimension_fkey
        FOREIGN KEY (generation_id, dimension)
        REFERENCES document_vector_backend_generations(generation_id, dimension)
);
CREATE INDEX IF NOT EXISTS idx_document_vector_embeddings_generation
    ON document_vector_embeddings(generation_id, dimension, token);
