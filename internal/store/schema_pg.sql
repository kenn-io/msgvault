-- msgvault PostgreSQL schema
-- Native PostgreSQL types and identity columns, parallel to schema.sql.

-- ============================================================================
-- SOURCES & IDENTITY
-- ============================================================================

CREATE TABLE IF NOT EXISTS sources (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_type TEXT NOT NULL,
    identifier TEXT NOT NULL,
    display_name TEXT,

    google_user_id TEXT UNIQUE,

    last_sync_at TIMESTAMPTZ,
    sync_cursor TEXT,
    sync_config JSONB,
    oauth_app TEXT,

    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_type, identifier)
);

CREATE TABLE IF NOT EXISTS participants (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email_address TEXT,
    phone_number TEXT,
    display_name TEXT,
    domain TEXT,

    canonical_id TEXT,

    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS participant_identifiers (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    identifier_type TEXT NOT NULL,
    identifier_value TEXT NOT NULL,
    display_value TEXT,

    is_primary BOOLEAN DEFAULT FALSE,

    UNIQUE(identifier_type, identifier_value)
);

-- ============================================================================
-- CONVERSATIONS & MESSAGES
-- ============================================================================

CREATE TABLE IF NOT EXISTS conversations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    source_conversation_id TEXT,

    conversation_type TEXT NOT NULL DEFAULT 'email_thread',
    title TEXT,

    participant_count INTEGER DEFAULT 0,
    message_count INTEGER DEFAULT 0,
    unread_count INTEGER DEFAULT 0,
    last_message_at TIMESTAMPTZ,
    last_message_preview TEXT,

    metadata JSONB,

    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_id, source_conversation_id)
);

CREATE TABLE IF NOT EXISTS conversation_participants (
    conversation_id BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    role TEXT DEFAULT 'member',
    joined_at TIMESTAMPTZ,
    left_at TIMESTAMPTZ,

    PRIMARY KEY (conversation_id, participant_id)
);

CREATE TABLE IF NOT EXISTS messages (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    conversation_id BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    source_message_id TEXT,
    rfc822_message_id TEXT,

    message_type TEXT NOT NULL DEFAULT 'email',

    sent_at TIMESTAMPTZ,
    received_at TIMESTAMPTZ,
    read_at TIMESTAMPTZ,
    delivered_at TIMESTAMPTZ,
    internal_date TIMESTAMPTZ,

    sender_id BIGINT REFERENCES participants(id),
    is_from_me BOOLEAN DEFAULT FALSE,

    subject TEXT,
    snippet TEXT,

    reply_to_message_id BIGINT REFERENCES messages(id),
    thread_position INTEGER,

    is_read BOOLEAN DEFAULT TRUE,
    is_delivered BOOLEAN,
    is_sent BOOLEAN DEFAULT TRUE,
    is_edited BOOLEAN DEFAULT FALSE,
    is_forwarded BOOLEAN DEFAULT FALSE,

    size_estimate BIGINT,
    has_attachments BOOLEAN DEFAULT FALSE,
    attachment_count INTEGER DEFAULT 0,

    deleted_at TIMESTAMPTZ,
    deleted_from_source_at TIMESTAMPTZ,
    delete_batch_id TEXT,

    archived_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    indexing_version INTEGER DEFAULT 1,

    metadata JSONB,

    -- Row-level last-modified watermark, maintained ENTIRELY by the
    -- database (triggers, created by EnsureTriggers), never by application
    -- write paths. Used by the embed worker as an optimistic-CAS token.
    -- See schema.sql for the full contract.
    last_modified TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,

    -- Full-text search column
    search_fts TSVECTOR,

    -- Vector-embedding watermark: the index generation this message's
    -- embeddings were last written for. NULL means "needs embedding"
    -- (new rows default to NULL). See schema.sql for the full contract.
    embed_gen BIGINT,

    UNIQUE(source_id, source_message_id)
);

CREATE TABLE IF NOT EXISTS message_recipients (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    recipient_type TEXT NOT NULL,
    display_name TEXT,

    UNIQUE(message_id, participant_id, recipient_type)
);

-- ============================================================================
-- REACTIONS
-- ============================================================================

CREATE TABLE IF NOT EXISTS reactions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    reaction_type TEXT NOT NULL,
    reaction_value TEXT NOT NULL,

    created_at TIMESTAMPTZ,
    removed_at TIMESTAMPTZ,

    UNIQUE(message_id, participant_id, reaction_type, reaction_value)
);

-- ============================================================================
-- ATTACHMENTS
-- ============================================================================

CREATE TABLE IF NOT EXISTS attachments (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,

    filename TEXT,
    mime_type TEXT,
    size BIGINT,

    content_hash TEXT,
    storage_path TEXT NOT NULL DEFAULT '',

    media_type TEXT,
    width INTEGER,
    height INTEGER,
    duration_ms INTEGER,

    thumbnail_hash TEXT,
    thumbnail_path TEXT,

    source_attachment_id TEXT,
    attachment_metadata JSONB,

    encryption_version INTEGER DEFAULT 0,

    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

-- ============================================================================
-- LABELS
-- ============================================================================

CREATE TABLE IF NOT EXISTS labels (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id BIGINT REFERENCES sources(id) ON DELETE CASCADE,

    source_label_id TEXT,
    name TEXT NOT NULL,
    label_type TEXT,
    color TEXT,

    UNIQUE(source_id, name)
);

CREATE TABLE IF NOT EXISTS message_labels (
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    label_id BIGINT NOT NULL REFERENCES labels(id) ON DELETE CASCADE,

    PRIMARY KEY (message_id, label_id)
);

-- ============================================================================
-- RAW DATA
-- ============================================================================

CREATE TABLE IF NOT EXISTS message_bodies (
    message_id BIGINT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    body_text TEXT,
    body_html TEXT
);

CREATE TABLE IF NOT EXISTS message_raw (
    message_id BIGINT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,

    raw_data BYTEA NOT NULL,
    raw_format TEXT NOT NULL,

    compression TEXT DEFAULT 'zlib',
    encryption_version INTEGER DEFAULT 0
);

-- ============================================================================
-- SYNC STATE
-- ============================================================================

CREATE TABLE IF NOT EXISTS sync_runs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    status TEXT DEFAULT 'running',

    messages_processed BIGINT DEFAULT 0,
    messages_added BIGINT DEFAULT 0,
    messages_updated BIGINT DEFAULT 0,
    errors_count BIGINT DEFAULT 0,

    error_message TEXT,
    cursor_before TEXT,
    cursor_after TEXT
);

CREATE TABLE IF NOT EXISTS sync_run_items (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sync_run_id BIGINT NOT NULL REFERENCES sync_runs(id) ON DELETE CASCADE,
    source_message_id TEXT NOT NULL,
    phase TEXT NOT NULL,
    status TEXT NOT NULL,
    error_kind TEXT NOT NULL,
    error_message TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sync_checkpoints (
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    checkpoint_type TEXT NOT NULL,
    checkpoint_value TEXT NOT NULL,

    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, checkpoint_type)
);

CREATE TABLE IF NOT EXISTS imap_folder_state (
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    mailbox TEXT NOT NULL,
    uidvalidity BIGINT NOT NULL,
    uidnext BIGINT NOT NULL,

    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, mailbox)
);

CREATE TABLE IF NOT EXISTS source_import_items (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    name TEXT NOT NULL,
    checksum TEXT,
    size BIGINT DEFAULT 0,
    modified_at TIMESTAMPTZ,
    imported_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'pending',
    records_imported INTEGER DEFAULT 0,
    error_message TEXT,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(source_id, provider, provider_id)
);

-- ============================================================================
-- COLLECTIONS
-- ============================================================================

CREATE TABLE IF NOT EXISTS collections (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS collection_sources (
    collection_id BIGINT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    PRIMARY KEY (collection_id, source_id)
);

CREATE INDEX IF NOT EXISTS idx_collection_sources_source_id
    ON collection_sources(source_id);

-- Confirmed per-account "me" identities used by sent-message detection
-- in dedup. Identity is account-scoped: an address confirmed for one
-- source does not imply it is "me" in any other source.
CREATE TABLE IF NOT EXISTS account_identities (
    source_id     BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    address       TEXT NOT NULL,
    source_signal TEXT NOT NULL DEFAULT '',
    confirmed_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source_id, address)
);

-- Marks one-time data migrations that have already run. Schema DDL is
-- idempotent via IF NOT EXISTS; this table is for *data* migrations
-- (e.g. moving legacy config into per-account records) that must run
-- exactly once.
CREATE TABLE IF NOT EXISTS applied_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Packed attachment storage (docs/internal/packed-attachments-design.md).
-- attachment_pack_index maps content-addressed blobs (attachment content and
-- thumbnails) to sealed pack files under attachments/packs/. Rows exist only
-- for live packed blobs; loose files have no row. pack_offset et al mirror
-- the pack footer's entry so reads need no footer parse ("offset" is a
-- reserved word in SQLite and PostgreSQL, hence the prefix).
CREATE TABLE IF NOT EXISTS attachment_pack_index (
    blob_hash   TEXT PRIMARY KEY,
    pack_id     TEXT NOT NULL,
    pack_offset BIGINT NOT NULL,
    stored_len  BIGINT NOT NULL,
    raw_len     BIGINT NOT NULL,
    flags       INTEGER NOT NULL,
    crc32c      BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachment_pack_index_pack
    ON attachment_pack_index(pack_id);

-- Immutable per-pack totals captured at seal/adoption. GC derives dead bytes
-- as stored_bytes minus the sum of the pack's live index rows.
CREATE TABLE IF NOT EXISTS attachment_packs (
    pack_id      TEXT PRIMARY KEY,
    entry_count  BIGINT NOT NULL,
    stored_bytes BIGINT NOT NULL,
    created_at   TEXT NOT NULL
);

-- ============================================================================
-- INDEXES
-- ============================================================================

CREATE INDEX IF NOT EXISTS idx_sources_type ON sources(source_type);

CREATE UNIQUE INDEX IF NOT EXISTS idx_participants_email ON participants(email_address)
    WHERE email_address IS NOT NULL;
-- idx_participants_phone is created (and upgraded from the legacy
-- non-unique form) in Go by Store.ensureParticipantsPhoneUniqueIndex
-- so existing DBs whose IF NOT EXISTS no-op'd the schema bump still
-- end up with a UNIQUE partial index.
CREATE INDEX IF NOT EXISTS idx_participants_canonical ON participants(canonical_id)
    WHERE canonical_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_participant_identifiers_value ON participant_identifiers(identifier_value);
CREATE INDEX IF NOT EXISTS idx_participant_identifiers_participant ON participant_identifiers(participant_id);

CREATE INDEX IF NOT EXISTS idx_conversations_source ON conversations(source_id);
CREATE INDEX IF NOT EXISTS idx_conversations_last_message ON conversations(last_message_at DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_type ON conversations(conversation_type);

CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_source ON messages(source_id);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_messages_sent_at ON messages(sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_type ON messages(message_type);
CREATE INDEX IF NOT EXISTS idx_messages_deleted ON messages(source_id, deleted_from_source_at);
CREATE INDEX IF NOT EXISTS idx_messages_source_message_id ON messages(source_message_id);

-- Full-text search GIN index on messages.search_fts is created by
-- PostgreSQLDialect.EnsureFTSIndex AFTER LegacyColumnMigrations add the
-- column, not here: a legacy DB missing search_fts would fail this index
-- during the schema-file Exec and roll back the whole apply. [cr2-10]

CREATE INDEX IF NOT EXISTS idx_message_recipients_message ON message_recipients(message_id);
CREATE INDEX IF NOT EXISTS idx_message_recipients_participant ON message_recipients(participant_id, recipient_type);

CREATE INDEX IF NOT EXISTS idx_reactions_message ON reactions(message_id);

CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_hash ON attachments(content_hash);
-- Thumbnail hash/path and LOWER(content_hash)/LOWER(thumbnail_hash) indexes
-- are created in Go (Store.InitSchema) under the maintenance escape hatch:
-- this file executes before that hatch is available, and the one-time index
-- builds over a populated attachments table can exceed the pool-wide 30s
-- statement_timeout on a large archive (finding S1). SQLite keeps the indexes
-- in schema.sql (no statement_timeout there).
CREATE INDEX IF NOT EXISTS idx_attachments_storage_path ON attachments(storage_path);
-- idx_attachments_msg_content_hash is created in Go (Store.InitSchema)
-- after a one-shot dedupe of legacy duplicate rows.

CREATE INDEX IF NOT EXISTS idx_labels_source ON labels(source_id);
CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label_id);

CREATE INDEX IF NOT EXISTS idx_sync_runs_source ON sync_runs(source_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sync_run_items_run_status
    ON sync_run_items(sync_run_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_source_import_items_source_provider
    ON source_import_items(source_id, provider, status);

CREATE INDEX IF NOT EXISTS idx_account_identities_address
    ON account_identities(address);

CREATE INDEX IF NOT EXISTS idx_collection_sources_source_id
    ON collection_sources(source_id);
