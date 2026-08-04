-- msgvault unified schema
-- Supports: Gmail, Apple Messages, Google Messages, WhatsApp

CREATE TABLE IF NOT EXISTS archive_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- ============================================================================
-- SOURCES & IDENTITY
-- ============================================================================

-- Message sources (accounts from different platforms)
CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY,
    source_type TEXT NOT NULL,  -- 'gmail', 'apple_messages', 'google_messages', 'whatsapp'
    identifier TEXT NOT NULL,   -- email, phone number, or account ID
    display_name TEXT,

    -- Gmail-specific (for backward compatibility during transition)
    google_user_id TEXT UNIQUE,

    -- Sync state
    last_sync_at DATETIME,
    sync_cursor TEXT,           -- platform-specific: historyId, rowid, timestamp
    sync_config JSON,           -- platform-specific sync settings
    oauth_app TEXT,             -- named OAuth app binding (NULL = default)

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_type, identifier)
);

-- Participants (unified contacts across platforms)
CREATE TABLE IF NOT EXISTS participants (
    id INTEGER PRIMARY KEY,
    email_address TEXT,         -- for email participants
    phone_number TEXT,          -- normalized E.164 format
    display_name TEXT,
    domain TEXT,                -- extracted from email for aggregation

    -- For cross-platform dedup (normalized phone/email)
    canonical_id TEXT,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Participant identifiers (for linking multiple contact methods)
CREATE TABLE IF NOT EXISTS participant_identifiers (
    id INTEGER PRIMARY KEY,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    identifier_type TEXT NOT NULL,  -- 'email', 'phone', 'apple_id', 'whatsapp'
    identifier_value TEXT NOT NULL, -- normalized value
    display_value TEXT,             -- original format for display

    is_primary BOOLEAN DEFAULT FALSE,

    UNIQUE(identifier_type, identifier_value)
);

-- Durable, user-curated people. A person's vCard UID is generated once and
-- never depends on mutable participant identifiers or link-graph topology.
-- UID lifecycle contract: UIDs are random and never reused. Deleting a
-- person retires its UID forever (no tombstones; a later re-promotion of
-- the same cluster creates a new person with a new UID), and a future
-- person-merge must keep the surviving person's UID and retire the other.
-- AUTOINCREMENT (IDENTITY on PostgreSQL) matters here: person IDs are
-- durable external handles, so a deleted person's ID must never be
-- recycled for a later person the way plain rowid allocation would.
CREATE TABLE IF NOT EXISTS persons (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    vcard_uid    TEXT NOT NULL UNIQUE,
    display_name TEXT,
    revision     INTEGER NOT NULL DEFAULT 1,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Bindings are deliberately participant-local and are the source of truth
-- for person membership: a person covers exactly its bound participants,
-- never "whatever cluster a binding sits in". Link/unlink changes the
-- observed identity graph without rewriting curated person membership;
-- within one cluster, link/merge/promotion keep bindings all-or-none to at
-- most one person, while unlink may leave one person spanning the split
-- clusters until the user re-links or deletes the profile.
CREATE TABLE IF NOT EXISTS person_participants (
    person_id      INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    PRIMARY KEY (person_id, participant_id),
    UNIQUE(participant_id)
);

-- ============================================================================
-- CONVERSATIONS & MESSAGES
-- ============================================================================

-- Conversations (threads for email, chats for messaging)
CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    -- Platform-specific ID for dedup on re-import
    source_conversation_id TEXT,

    -- Type and metadata
    conversation_type TEXT NOT NULL,  -- 'email_thread', 'group_chat', 'direct_chat', 'channel'
    title TEXT,                       -- email subject, group name, or NULL for DMs

    -- Denormalized stats (updated on message insert)
    participant_count INTEGER DEFAULT 0,
    message_count INTEGER DEFAULT 0,
    unread_count INTEGER DEFAULT 0,
    last_message_at DATETIME,
    last_message_preview TEXT,

    -- Platform-specific metadata
    metadata JSON,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_id, source_conversation_id)
);

-- Conversation participants (who's in each conversation)
CREATE TABLE IF NOT EXISTS conversation_participants (
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    role TEXT DEFAULT 'member',  -- 'owner', 'admin', 'member' for groups
    joined_at DATETIME,
    left_at DATETIME,

    PRIMARY KEY (conversation_id, participant_id)
);

-- Messages (unified across all platforms)
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    -- Platform-specific ID for dedup
    source_message_id TEXT,

    -- RFC822 Message-ID for cross-mailbox dedup (IMAP)
    rfc822_message_id TEXT,

    -- Message classification
    message_type TEXT NOT NULL,  -- 'email', 'imessage', 'sms', 'mms', 'rcs', 'whatsapp', 'fbmessenger', 'teams'

    -- Timestamps (sent_at is canonical, others platform-specific)
    sent_at DATETIME,
    received_at DATETIME,
    read_at DATETIME,
    delivered_at DATETIME,
    internal_date DATETIME,      -- Gmail internal date

    -- Sender
    sender_id INTEGER REFERENCES participants(id),
    is_from_me BOOLEAN DEFAULT FALSE,
    source_is_from_me BOOLEAN,
    identity_is_from_me BOOLEAN NOT NULL DEFAULT FALSE,

    -- Content
    subject TEXT,               -- email subject, NULL for chat
    snippet TEXT,               -- preview/excerpt

    -- Threading (for email and replies)
    reply_to_message_id INTEGER REFERENCES messages(id),
    thread_position INTEGER,    -- position in thread/conversation

    -- Status flags
    is_read BOOLEAN DEFAULT TRUE,
    is_delivered BOOLEAN,
    is_sent BOOLEAN DEFAULT TRUE,
    is_edited BOOLEAN DEFAULT FALSE,
    is_forwarded BOOLEAN DEFAULT FALSE,

    -- Size and attachment tracking
    size_estimate INTEGER,
    has_attachments BOOLEAN DEFAULT FALSE,
    attachment_count INTEGER DEFAULT 0,

    -- Soft delete tracking
    deleted_at DATETIME,
    deleted_from_source_at DATETIME,
    delete_batch_id TEXT,

    -- Archival info
    archived_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    indexing_version INTEGER DEFAULT 1,

    -- Row-level last-modified watermark, maintained ENTIRELY by the
    -- database (triggers below), never by application write paths. Used by
    -- the embed worker as an optimistic-CAS token: it captures this value
    -- when it reads a message's content and stamps embed_gen only if the
    -- value is unchanged at stamp time, so a concurrent content edit
    -- (e.g. repair-encoding) that lands between read and stamp leaves the
    -- row unstamped and it is re-embedded with the corrected content.
    last_modified DATETIME DEFAULT CURRENT_TIMESTAMP,

    -- Platform-specific metadata
    metadata JSON,

    -- Vector-embedding watermark: the index generation this message's
    -- embeddings were last written for. NULL means "needs embedding"
    -- (new rows default to NULL); a value equal to the active/building
    -- generation id means "covered". The scan-and-fill embed worker
    -- finds work via (embed_gen IS NULL OR embed_gen <> <target>) and
    -- stamps this column after a successful upsert (or skip).
    embed_gen INTEGER,

    -- Content-change watermark, maintained ENTIRELY by the database (triggers
    -- created by EnsureTriggers), never by application write paths. Unlike
    -- last_modified above, which bumps on ANY change to the row, this moves
    -- only when the message's own content, routing, or lifecycle actually
    -- changes value -- see MessagesContentColumns in content_columns.go for
    -- the list and the reason each column is in or out. It exists so a
    -- consumer maintaining an incremental copy of the archive can page "what
    -- changed since X?" without being woken by internal bookkeeping such as
    -- embedding watermarks or index versions.
    --
    -- The DEFAULT is the INSERT-time writer on a fresh database, and it must
    -- stay byte-compatible with SQLiteDialect.ContentChangedNow (the trigger
    -- that stamps everything else) -- the feed's cursor comparison is lexical,
    -- so a stamp of a different width sorts into the wrong place. It is here
    -- rather than only in the trigger because SQLite triggers cannot assign to
    -- NEW: an AFTER INSERT trigger has to re-UPDATE the row it just saw, which
    -- also re-fires the blanket last_modified trigger, turning one row write
    -- into three (measured ~6x slower and a 17% larger file over a 100k-row
    -- bulk insert). A database upgraded by ALTER TABLE ADD COLUMN cannot carry
    -- this DEFAULT -- SQLite rejects a non-constant default there -- so an
    -- INSERT trigger guarded by WHEN NEW.content_changed_at IS NULL stamps
    -- those rows instead. EnsureTriggers creates that trigger ONLY on an
    -- archive whose column carries no default: on a fresh database this DEFAULT
    -- is the writer and the trigger is dropped and not recreated, because
    -- merely having a row trigger on messages costs every INSERT a compiled
    -- trigger subprogram whether or not its body runs.
    --
    -- This column MUST stay last so that a fresh database and one upgraded by
    -- the ALTER TABLE ADD COLUMN migration declare their columns in the same
    -- order (ALTER TABLE always appends). subset.go no longer depends on that
    -- -- it copies messages by the column list the source and destination share
    -- -- but the two layouts do meet, and a divergence silently breaks anything
    -- that reads a message row by position.
    -- TestContentChangedAt_ColumnOrderMatchesAfterUpgrade pins it.
    content_changed_at DATETIME DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),

    UNIQUE(source_id, source_message_id)
);

-- Message recipients (To/Cc/Bcc for email, participants for group messages)
CREATE TABLE IF NOT EXISTS message_recipients (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    recipient_type TEXT NOT NULL,  -- 'to', 'cc', 'bcc', 'mention'
    display_name TEXT,             -- as it appeared in the message

    UNIQUE(message_id, participant_id, recipient_type)
);

-- ============================================================================
-- REACTIONS & INTERACTIONS
-- ============================================================================

-- Reactions (tapbacks, emoji reactions)
CREATE TABLE IF NOT EXISTS reactions (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    -- Reaction type and value
    reaction_type TEXT NOT NULL,  -- 'tapback', 'emoji', 'like'
    reaction_value TEXT NOT NULL, -- 'heart', 'thumbsup', etc. or emoji

    -- Apple tapback types: 'love', 'like', 'dislike', 'laugh', 'emphasis', 'question'

    created_at DATETIME,
    removed_at DATETIME,

    UNIQUE(message_id, participant_id, reaction_type, reaction_value)
);

-- ============================================================================
-- ATTACHMENTS & MEDIA
-- ============================================================================

-- Attachments (content-addressed storage)
CREATE TABLE IF NOT EXISTS attachments (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,

    -- File identification
    filename TEXT,
    mime_type TEXT,
    size INTEGER,

    -- Content-addressed storage (deduplication)
    content_hash TEXT,              -- SHA-256 of content
    storage_path TEXT NOT NULL,     -- relative path: ab/abcd1234...

    -- Media metadata
    media_type TEXT,                -- 'image', 'video', 'audio', 'document', 'sticker', 'gif', 'voice_note'
    width INTEGER,
    height INTEGER,
    duration_ms INTEGER,            -- for audio/video

    -- Thumbnail (for images/videos)
    thumbnail_hash TEXT,
    thumbnail_path TEXT,

    -- Platform-specific
    source_attachment_id TEXT,      -- original ID from platform
    attachment_metadata JSON,       -- EXIF, etc.

    -- Encryption
    encryption_version INTEGER DEFAULT 0,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- ============================================================================
-- LABELS & ORGANIZATION
-- ============================================================================

-- Labels (Gmail labels, user tags)
CREATE TABLE IF NOT EXISTS labels (
    id INTEGER PRIMARY KEY,
    source_id INTEGER REFERENCES sources(id) ON DELETE CASCADE,  -- NULL for user-created

    source_label_id TEXT,           -- Gmail label ID
    name TEXT NOT NULL,
    label_type TEXT,                -- 'system', 'user', 'auto'
    color TEXT,

    UNIQUE(source_id, name)
);

-- Message labels
CREATE TABLE IF NOT EXISTS message_labels (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,

    PRIMARY KEY (message_id, label_id)
);

-- ============================================================================
-- RAW DATA STORAGE
-- ============================================================================

-- Message bodies (separated from messages to keep messages B-tree small)
CREATE TABLE IF NOT EXISTS message_bodies (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    body_text TEXT,
    body_html TEXT
);

-- ============================================================================
-- LAST-MODIFIED TRIGGERS
-- ============================================================================
-- messages.last_modified is bumped to CURRENT_TIMESTAMP on ANY change to a
-- message row OR any insert/update of its body row. This is a TRUE row-level
-- last-modified (blanket, not column-specific): the embed worker uses it as an
-- optimistic-CAS token, so it must move whenever any embeddable content could
-- have changed. No application write path bumps it manually — the database
-- owns it via these triggers. InitSchema re-execs schema.sql idempotently, so
-- `IF NOT EXISTS` makes these safe on both fresh and existing databases.

-- On messages: re-stamp last_modified after an UPDATE. The WHEN guard
-- (OLD.last_modified = NEW.last_modified) prevents infinite recursion: the
-- trigger's own UPDATE changes last_modified, so on the re-fire
-- OLD.last_modified <> NEW.last_modified and WHEN evaluates false, regardless
-- of the recursive_triggers pragma. It also yields to an explicit
-- last_modified write in the original UPDATE rather than clobbering it.
--
-- This trigger is NOT created here. It needs an `UPDATE OF <every column except
-- content_changed_at>` scope -- without it, the content_changed_at stamp (a
-- second UPDATE on SQLite) re-enters this trigger and destroys the explicit
-- write the guard above promises to yield to. That column list has to be read
-- from the live table, which SQL alone cannot do, so SQLiteDialect.EnsureTriggers
-- builds it -- see lastModifiedUpdateOfColumns. InitSchema always runs
-- EnsureTriggers, and it DROPs before CREATEing, so an archive still carrying an
-- older blanket definition is corrected on open.
--
-- The message_bodies triggers below stay here: they write messages.last_modified
-- directly instead of reacting to a messages UPDATE, so nothing can re-enter them.

-- On message_bodies: a body change must bump the parent message's
-- last_modified so the worker's CAS token covers body edits too.
CREATE TRIGGER IF NOT EXISTS trg_message_bodies_last_modified_upd
AFTER UPDATE ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;

CREATE TRIGGER IF NOT EXISTS trg_message_bodies_last_modified_ins
AFTER INSERT ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;

-- Original message data (for re-parsing/export)
CREATE TABLE IF NOT EXISTS message_raw (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,

    raw_data BLOB NOT NULL,
    raw_format TEXT NOT NULL,       -- 'mime', 'imessage_archive', 'whatsapp_json', 'rcs_json'

    compression TEXT DEFAULT 'zlib',
    encryption_version INTEGER DEFAULT 0
);

-- ============================================================================
-- SYNC STATE
-- ============================================================================

-- Sync runs (for debugging and resumability)
CREATE TABLE IF NOT EXISTS sync_runs (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    started_at DATETIME NOT NULL,
    completed_at DATETIME,
    status TEXT DEFAULT 'running',  -- 'running', 'completed', 'failed', 'cancelled'

    messages_processed INTEGER DEFAULT 0,
    messages_added INTEGER DEFAULT 0,
    messages_updated INTEGER DEFAULT 0,
    errors_count INTEGER DEFAULT 0,

    error_message TEXT,
    cursor_before TEXT,
    cursor_after TEXT
);

-- Per-item sync outcomes, for diagnosing partial sync completion.
-- status='error' is actionable and contributes to sync_runs.errors_count.
-- status='skipped' records expected item churn, such as Gmail messages that
-- were deleted between a history/list response and raw-message fetch.
CREATE TABLE IF NOT EXISTS sync_run_items (
    id INTEGER PRIMARY KEY,
    sync_run_id INTEGER NOT NULL REFERENCES sync_runs(id) ON DELETE CASCADE,
    source_message_id TEXT NOT NULL,
    phase TEXT NOT NULL,
    status TEXT NOT NULL,
    error_kind TEXT NOT NULL,
    error_message TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Sync checkpoints (for resumable imports)
CREATE TABLE IF NOT EXISTS sync_checkpoints (
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    checkpoint_type TEXT NOT NULL,  -- 'message_id', 'timestamp', 'page_token'
    checkpoint_value TEXT NOT NULL,

    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, checkpoint_type)
);

-- Per-mailbox IMAP sync state from the last fully completed sync.
-- UIDVALIDITY/UIDNEXT let subsequent syncs skip mailboxes that have
-- not changed instead of re-enumerating every folder.
CREATE TABLE IF NOT EXISTS imap_folder_state (
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    mailbox TEXT NOT NULL,
    uidvalidity INTEGER NOT NULL,
    uidnext INTEGER NOT NULL,

    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, mailbox)
);

-- Imported source items (files/objects already processed for resumable adapters)
CREATE TABLE IF NOT EXISTS source_import_items (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    name TEXT NOT NULL,
    checksum TEXT,
    size INTEGER DEFAULT 0,
    modified_at DATETIME,
    imported_at DATETIME,
    status TEXT NOT NULL DEFAULT 'pending',
    records_imported INTEGER DEFAULT 0,
    error_message TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(source_id, provider, provider_id)
);

-- ============================================================================
-- INDEXES
-- ============================================================================

-- Sources
CREATE INDEX IF NOT EXISTS idx_sources_type ON sources(source_type);

-- Participants
CREATE UNIQUE INDEX IF NOT EXISTS idx_participants_email ON participants(email_address)
    WHERE email_address IS NOT NULL;
-- idx_participants_phone is created (and upgraded from the legacy
-- non-unique form) in Go by Store.ensureParticipantsPhoneUniqueIndex
-- so existing DBs whose IF NOT EXISTS no-op'd the schema bump still
-- end up with a UNIQUE partial index.
CREATE INDEX IF NOT EXISTS idx_participants_canonical ON participants(canonical_id)
    WHERE canonical_id IS NOT NULL;

-- Participant identifiers
CREATE INDEX IF NOT EXISTS idx_participant_identifiers_value ON participant_identifiers(identifier_value);
CREATE INDEX IF NOT EXISTS idx_participant_identifiers_participant ON participant_identifiers(participant_id);

-- Conversations
CREATE INDEX IF NOT EXISTS idx_conversations_source ON conversations(source_id);
CREATE INDEX IF NOT EXISTS idx_conversations_last_message ON conversations(last_message_at DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_type ON conversations(conversation_type);

-- Messages
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_source ON messages(source_id);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_messages_sent_at ON messages(sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_type ON messages(message_type);
CREATE INDEX IF NOT EXISTS idx_messages_deleted ON messages(source_id, deleted_from_source_at);
CREATE INDEX IF NOT EXISTS idx_messages_source_message_id ON messages(source_message_id);

-- Message recipients
CREATE INDEX IF NOT EXISTS idx_message_recipients_message ON message_recipients(message_id);
CREATE INDEX IF NOT EXISTS idx_message_recipients_participant ON message_recipients(participant_id, recipient_type);

-- Reactions
CREATE INDEX IF NOT EXISTS idx_reactions_message ON reactions(message_id);

-- Attachments
CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_hash ON attachments(content_hash);
CREATE INDEX IF NOT EXISTS idx_attachments_content_hash_lower ON attachments(LOWER(content_hash));
CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_hash ON attachments(thumbnail_hash);
CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_hash_lower ON attachments(LOWER(thumbnail_hash));
CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_path ON attachments(thumbnail_path);
CREATE INDEX IF NOT EXISTS idx_attachments_storage_path ON attachments(storage_path);
-- The partial unique index on (message_id, content_hash) for
-- UpsertAttachment idempotency is created in Go (Store.InitSchema)
-- after a one-shot dedupe of legacy duplicate rows.

-- Labels
CREATE INDEX IF NOT EXISTS idx_labels_source ON labels(source_id);
CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label_id);

-- Sync
CREATE INDEX IF NOT EXISTS idx_sync_runs_source ON sync_runs(source_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sync_run_items_run_status
    ON sync_run_items(sync_run_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_source_import_items_source_provider
    ON source_import_items(source_id, provider, status);

-- ============================================================================
-- COLLECTIONS
-- ============================================================================

-- Collections (named groupings of sources treated as a single logical archive)
CREATE TABLE IF NOT EXISTS collections (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    description TEXT    NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Collection membership (many sources per collection)
CREATE TABLE IF NOT EXISTS collection_sources (
    collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    source_id     INTEGER NOT NULL REFERENCES sources(id)     ON DELETE CASCADE,
    PRIMARY KEY (collection_id, source_id)
);

CREATE INDEX IF NOT EXISTS idx_collection_sources_source_id
    ON collection_sources(source_id);

-- Daemon-owned analytical Saved Views. Canonical state contains only the
-- query/view definition; result rows and transient selection remain client
-- state and are never persisted here.
CREATE TABLE IF NOT EXISTS saved_views (
    id              INTEGER PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT,
    canonical_state JSON NOT NULL,
    schema_version  INTEGER NOT NULL,
    revision        INTEGER NOT NULL DEFAULT 1,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- ============================================================================
-- ACCOUNT IDENTITIES
-- ============================================================================

-- Confirmed per-account "me" identities used by sent-message detection
-- in dedup. Identity is account-scoped: an address confirmed for one
-- source does not imply it is "me" in any other source.
CREATE TABLE IF NOT EXISTS account_identities (
    source_id    INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    address      TEXT NOT NULL,             -- case-preserved
    source_signal TEXT NOT NULL DEFAULT '', -- sorted comma-separated signal set, e.g. 'manual' or 'account-identifier,manual'
    confirmed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source_id, address)
);

CREATE INDEX IF NOT EXISTS idx_account_identities_address
    ON account_identities(address);

-- User-asserted identity links between participants. Edges are normalized
-- (participant_a < participant_b) and the graph is kept a forest: every
-- edge joins two previously distinct clusters, so deleting an edge
-- deterministically splits one cluster in two. Connected components resolve
-- to a canonical cluster (smallest member ID) at read time.
CREATE TABLE IF NOT EXISTS participant_links (
    participant_a INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    participant_b INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (participant_a, participant_b),
    CHECK (participant_a < participant_b)
);

CREATE INDEX IF NOT EXISTS idx_participant_links_b
    ON participant_links(participant_b);

-- ============================================================================
-- APPLIED MIGRATIONS
-- ============================================================================

-- Marks one-time data migrations that have already run. Schema DDL is
-- idempotent via IF NOT EXISTS; this table is for *data* migrations
-- (e.g. moving legacy config into per-account records) that must run
-- exactly once.
CREATE TABLE IF NOT EXISTS applied_migrations (
    name        TEXT PRIMARY KEY,
    applied_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
