-- SQLite-specific extensions (FTS5 for full-text search)

CREATE TABLE IF NOT EXISTS archive_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Role uniqueness is a SQLite partial index. CardDAV URL identity uniqueness
-- lives in schema.sql's carddav_address_book_urls table so canonical and alias
-- URLs cannot collide across columns.
CREATE UNIQUE INDEX IF NOT EXISTS idx_carddav_one_write_target
    ON carddav_address_books((1)) WHERE is_write_target = TRUE;
CREATE INDEX IF NOT EXISTS idx_carddav_resources_person
    ON carddav_resources(person_id) WHERE person_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_carddav_resources_remote_uid
    ON carddav_resources(remote_uid) WHERE remote_uid IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_carddav_publications_pending
    ON carddav_publications(pending_operation, person_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_carddav_one_unresolved_conflict
    ON carddav_conflicts(address_book_id, href) WHERE status = 'unresolved';
CREATE INDEX IF NOT EXISTS idx_carddav_conflicts_resolved_at
    ON carddav_conflicts(status, resolved_at);

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
