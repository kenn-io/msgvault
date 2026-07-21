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
