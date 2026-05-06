-- msgvault unified schema
-- Supports: Gmail, Apple Messages, Google Messages, WhatsApp

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
    message_type TEXT NOT NULL,  -- 'email', 'imessage', 'sms', 'mms', 'rcs', 'whatsapp', 'fbmessenger'

    -- Timestamps (sent_at is canonical, others platform-specific)
    sent_at DATETIME,
    received_at DATETIME,
    read_at DATETIME,
    delivered_at DATETIME,
    internal_date DATETIME,      -- Gmail internal date

    -- Sender
    sender_id INTEGER REFERENCES participants(id),
    is_from_me BOOLEAN DEFAULT FALSE,

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

    -- Platform-specific metadata
    metadata JSON,

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

-- Sync checkpoints (for resumable imports)
CREATE TABLE IF NOT EXISTS sync_checkpoints (
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    checkpoint_type TEXT NOT NULL,  -- 'message_id', 'timestamp', 'page_token'
    checkpoint_value TEXT NOT NULL,

    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, checkpoint_type)
);

-- ============================================================================
-- INDEXES
-- ============================================================================

-- Sources
CREATE INDEX IF NOT EXISTS idx_sources_type ON sources(source_type);

-- Participants
CREATE UNIQUE INDEX IF NOT EXISTS idx_participants_email ON participants(email_address)
    WHERE email_address IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_participants_phone ON participants(phone_number)
    WHERE phone_number IS NOT NULL;
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
CREATE INDEX IF NOT EXISTS idx_attachments_storage_path ON attachments(storage_path);

-- Labels
CREATE INDEX IF NOT EXISTS idx_labels_source ON labels(source_id);
CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label_id);

-- Sync
CREATE INDEX IF NOT EXISTS idx_sync_runs_source ON sync_runs(source_id, started_at DESC);

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
