-- SQLite-specific extensions (FTS5 for full-text search)

CREATE TABLE IF NOT EXISTS archive_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Full-text search index for messages
-- This is a standalone FTS table (not contentless) that stores its own copy
-- of searchable text. Updates are managed via Store.upsert_fts().
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    message_id UNINDEXED,
    subject,
    body,
    from_addr,
    to_addr,
    cc_addr,
    tokenize='unicode61 remove_diacritics 1'
);

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
