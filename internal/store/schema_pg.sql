-- msgvault PostgreSQL schema
-- Native PostgreSQL types and identity columns, parallel to schema.sql.

CREATE TABLE IF NOT EXISTS archive_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Open catalog of communication services. Seeded slugs are presentation and
-- normalization metadata, NOT a database enum and not a compatibility
-- ceiling: an unknown bridge type or a custom service is registered as a new
-- row, never by a schema migration. Slugs are immutable machine identities;
-- display labels remain mutable and are never overwritten by re-seeding.
CREATE TABLE IF NOT EXISTS communication_services (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug                  TEXT NOT NULL UNIQUE,
    display_label         TEXT NOT NULL,
    scope_policy          TEXT NOT NULL DEFAULT 'none',
    default_scope_kind    TEXT,
    normalization         TEXT NOT NULL DEFAULT 'none',
    normalization_version INTEGER NOT NULL DEFAULT 1,
    uri_scheme            TEXT,
    profile_url_template  TEXT,
    is_system             BOOLEAN NOT NULL DEFAULT FALSE,
    is_active             BOOLEAN NOT NULL DEFAULT TRUE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Aliases resolve to one canonical service without changing captured source
-- values. A primary key makes alias uniqueness a database constraint.
CREATE TABLE IF NOT EXISTS communication_service_aliases (
    alias      TEXT PRIMARY KEY,
    service_id BIGINT NOT NULL REFERENCES communication_services(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_communication_service_aliases_service
    ON communication_service_aliases(service_id);

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

    service_id BIGINT REFERENCES communication_services(id) ON DELETE SET NULL,
    scope_kind TEXT,
    scope_value TEXT,

    UNIQUE(identifier_type, identifier_value)
);
-- Durable, user-curated people. A person's vCard UID is generated once and
-- never depends on mutable participant identifiers or link-graph topology.
-- UID lifecycle contract: UIDs are random and never reused. Deleting a
-- person retires its UID forever (no tombstones; a later re-promotion of
-- the same cluster creates a new person with a new UID), and a future
-- person-merge must keep the surviving person's UID and retire the other.
-- GENERATED ALWAYS AS IDENTITY (AUTOINCREMENT on SQLite) matters here:
-- person IDs are durable external handles, so a deleted person's ID must
-- never be recycled for a later person.
CREATE TABLE IF NOT EXISTS persons (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    vcard_uid    TEXT NOT NULL UNIQUE,
    display_name TEXT,
    revision     BIGINT NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Bindings are deliberately participant-local and are the source of truth
-- for person membership: a person covers exactly its bound participants,
-- never "whatever cluster a binding sits in". Link/unlink changes the
-- observed identity graph without rewriting curated person membership;
-- within one cluster, link/merge/promotion keep bindings all-or-none to at
-- most one person, while unlink may leave one person spanning the split
-- clusters until the user re-links or deletes the profile.
CREATE TABLE IF NOT EXISTS person_participants (
    person_id      BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    PRIMARY KEY (person_id, participant_id),
    UNIQUE(participant_id)
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
    source_is_from_me BOOLEAN,
    identity_is_from_me BOOLEAN NOT NULL DEFAULT FALSE,

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

    -- Content-change watermark; see content_columns.go and schema.sql for the
    -- rationale. Stamped by the triggers EnsureTriggers creates, never by
    -- application writes.
    content_changed_at TIMESTAMPTZ,

    UNIQUE(source_id, source_message_id)
);

CREATE TABLE IF NOT EXISTS message_recipients (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    recipient_type TEXT NOT NULL,
    display_name TEXT,
    -- Envelope address as it appeared in the message. Immutable under
    -- participant merges, unlike the participant row's email_address;
    -- identity discovery reads it so merges cannot rewrite evidence.
    -- NULL on rows from writers without envelope addresses (non-email
    -- importers, legacy rows).
    email_address TEXT

    -- Uniqueness spans the normalized envelope address so one participant
    -- can carry several alias snapshots on the same message (two aliases of
    -- an already-merged participant in one To: header, or rows preserved by
    -- a later participant merge). Enforced by idx_message_recipients_envelope,
    -- created in Go by Store.ensureRecipientEnvelopeUniqueIndex rather than
    -- here: on upgraded DBs email_address is a late ADD COLUMN that does not
    -- exist yet when this file runs, and legacy DBs additionally need their
    -- old table-level UNIQUE(message_id, participant_id, recipient_type)
    -- constraint dropped first.
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
    system_role TEXT,
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

-- Daemon-owned analytical Saved Views. Canonical state contains only the
-- query/view definition; result rows and transient selection remain client
-- state and are never persisted here.
CREATE TABLE IF NOT EXISTS saved_views (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT,
    canonical_state JSONB NOT NULL,
    schema_version  INTEGER NOT NULL,
    revision        BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

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

-- User-asserted identity links between participants. Edges are normalized
-- (participant_a < participant_b) and the graph is kept a forest: every
-- edge joins two previously distinct clusters, so deleting an edge
-- deterministically splits one cluster in two. Connected components resolve
-- to a canonical cluster (smallest member ID) at read time.
CREATE TABLE IF NOT EXISTS participant_links (
    participant_a BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    participant_b BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (participant_a, participant_b),
    CHECK (participant_a < participant_b)
);

CREATE INDEX IF NOT EXISTS idx_participant_links_b
    ON participant_links(participant_b);

-- ============================================================================
-- PORTABLE FIELD METADATA AND TYPED ATTRIBUTES
-- ============================================================================

-- Portable field-definition registry; vocabulary columns intentionally carry
-- no CHECK so both backends reject growing vocabularies through the same Go
-- validation. There is deliberately no is_unique column.
CREATE TABLE IF NOT EXISTS attribute_definitions (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    universal_id   TEXT NOT NULL UNIQUE,
    object_type    TEXT NOT NULL,
    slug           TEXT NOT NULL,
    label          TEXT NOT NULL,
    description    TEXT,
    value_type     TEXT NOT NULL,
    field_type     TEXT NOT NULL,
    record_target  TEXT,
    cardinality    TEXT NOT NULL DEFAULT 'single',
    display_order  BIGINT NOT NULL DEFAULT 0,
    is_required    BOOLEAN NOT NULL DEFAULT FALSE,
    ownership      TEXT NOT NULL DEFAULT 'user',
    ui_creatable   BOOLEAN NOT NULL DEFAULT TRUE,
    ui_editable    BOOLEAN NOT NULL DEFAULT TRUE,
    api_mutable    BOOLEAN NOT NULL DEFAULT TRUE,
    is_searchable  BOOLEAN NOT NULL DEFAULT FALSE,
    is_audited     BOOLEAN NOT NULL DEFAULT TRUE,
    is_deletable   BOOLEAN NOT NULL DEFAULT TRUE,
    history_exempt BOOLEAN NOT NULL DEFAULT FALSE,
    derived_source TEXT,
    options        JSONB,
    vcard_property TEXT,
    is_active      BOOLEAN NOT NULL DEFAULT TRUE,
    revision       BIGINT NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (LENGTH(universal_id) > 0),
    CHECK (LENGTH(slug) > 0),
    CHECK (LENGTH(label) > 0),
    CHECK (display_order >= 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_attribute_definitions_object_slug
    ON attribute_definitions(object_type, slug);

CREATE INDEX IF NOT EXISTS idx_attribute_definitions_active
    ON attribute_definitions(object_type, display_order, id)
    WHERE is_active = TRUE;

CREATE TABLE IF NOT EXISTS person_attribute_values (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id         BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    definition_id     BIGINT NOT NULL
                          REFERENCES attribute_definitions(id) ON DELETE RESTRICT,
    ordinal           BIGINT NOT NULL DEFAULT 0,
    value_text        TEXT,
    value_integer     BIGINT,
    value_real        DOUBLE PRECISION,
    value_boolean     BOOLEAN,
    value_date        TEXT,
    value_timestamp   TIMESTAMPTZ,
    value_json        JSONB,
    value_record_type TEXT,
    value_record_id   BIGINT,
    active_from       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    active_until      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at     TIMESTAMPTZ,
    source            TEXT NOT NULL,
    source_ref        TEXT,
    confidence        DOUBLE PRECISION,
    actor             TEXT,
    CHECK (ordinal >= 0),
    CHECK (source IN ('user', 'carddav_import', 'vcard_import',
                      'archive_observation', 'extraction', 'enrichment',
                      'system')),
    CHECK (active_until IS NULL OR active_until >= active_from),
    CHECK (confidence IS NULL OR (confidence >= 0.0 AND confidence <= 1.0)),
    CHECK (value_date IS NULL OR value_date LIKE '____-__-__'),
    CHECK (
        (CASE WHEN value_text      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_integer   IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_real      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_boolean   IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_date      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_timestamp IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_json      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_record_id IS NOT NULL THEN 1 ELSE 0 END) = 1
    ),
    CHECK ((value_record_id IS NULL) = (value_record_type IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_person_attribute_values_current
    ON person_attribute_values(person_id, definition_id, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_person_attribute_values_history
    ON person_attribute_values(person_id, definition_id, active_from DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_person_attribute_values_definition
    ON person_attribute_values(definition_id, active_until);

CREATE INDEX IF NOT EXISTS idx_person_attribute_values_current_text
    ON person_attribute_values(definition_id, value_text)
    WHERE value_text IS NOT NULL
      AND active_until IS NULL AND superseded_at IS NULL;

-- Person deletion checks for inbound record references; without this the
-- check scans the whole value table.
CREATE INDEX IF NOT EXISTS idx_person_attribute_values_record_ref
    ON person_attribute_values(value_record_type, value_record_id)
    WHERE value_record_id IS NOT NULL;

-- ============================================================================
-- PEOPLE PROFILE PRIMITIVES
-- ============================================================================

-- Structured, ordered person-name forms with the shared provenance and
-- two-axis history envelope.
CREATE TABLE IF NOT EXISTS person_names (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id             BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    name_kind             TEXT NOT NULL,
    formatted             TEXT,
    family_name           TEXT,
    given_name            TEXT,
    additional_names      TEXT,
    honorific_prefixes    TEXT,
    honorific_suffixes    TEXT,
    secondary_surname     TEXT,
    generation            TEXT,
    language              TEXT,
    script                TEXT,
    phonetic_system       TEXT,
    phonetic_script       TEXT,
    sort_as               TEXT,
    is_derived            BOOLEAN NOT NULL DEFAULT FALSE,
    original_value        TEXT NOT NULL,
    pref                  INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal               INTEGER NOT NULL DEFAULT 0,
    type_label            TEXT,
    type_tokens           TEXT,
    vcard_property        TEXT,
    vcard_group           TEXT,
    vcard_prop_id         TEXT,
    vcard_pid             TEXT,
    vcard_altid           TEXT,
    source                TEXT NOT NULL
        CHECK (source IN ('user', 'carddav_import', 'vcard_import',
                          'archive_observation', 'extraction', 'enrichment', 'system')),
    source_ref            TEXT,
    confidence            DOUBLE PRECISION
        CHECK (confidence IS NULL
               OR (confidence >= 0 AND confidence <= 1
                   AND source NOT IN ('user', 'carddav_import', 'vcard_import'))),
    active_from           TIMESTAMPTZ,
    active_until          TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_person_names_current
    ON person_names(person_id, name_kind, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_names_person
    ON person_names(person_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_names_property_identity
    ON person_names(person_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL
      AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS person_contact_points (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    address_kind TEXT NOT NULL,
    service_id BIGINT REFERENCES communication_services(id) ON DELETE RESTRICT,
    scope_kind TEXT,
    scope_value TEXT,
    original_value TEXT NOT NULL,
    normalized_value TEXT NOT NULL,
    normalization TEXT NOT NULL DEFAULT 'none',
    normalization_version INTEGER NOT NULL DEFAULT 1,
    uri TEXT,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT,
    type_tokens TEXT,
    vcard_property TEXT,
    vcard_group TEXT,
    vcard_prop_id TEXT,
    vcard_pid TEXT,
    vcard_altid TEXT,
    source TEXT NOT NULL
        CHECK (source IN ('user', 'carddav_import', 'vcard_import',
                          'archive_observation', 'extraction', 'enrichment', 'system')),
    source_ref TEXT,
    confidence DOUBLE PRECISION
        CHECK (confidence IS NULL
               OR (confidence >= 0 AND confidence <= 1
                   AND source NOT IN ('user', 'carddav_import', 'vcard_import'))),
    active_from TIMESTAMPTZ,
    active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_person_contact_points_current_lookup
    ON person_contact_points(address_kind, service_id, scope_kind, scope_value, normalized_value)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_contact_points_person_current
    ON person_contact_points(person_id, address_kind, pref, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_contact_points_person
    ON person_contact_points(person_id);
CREATE INDEX IF NOT EXISTS idx_person_contact_points_service
    ON person_contact_points(service_id) WHERE service_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_contact_points_property_identity
    ON person_contact_points(person_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL
      AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS person_addresses (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    address_kind TEXT NOT NULL DEFAULT 'postal',
    post_office_box TEXT,
    extended_address TEXT,
    street_address TEXT,
    locality TEXT,
    region TEXT,
    postal_code TEXT,
    country_name TEXT,
    extended_components TEXT,
    free_text TEXT,
    label TEXT,
    geo_uri TEXT,
    timezone TEXT,
    country_code TEXT,
    place_uri TEXT,
    original_value TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT,
    type_tokens TEXT,
    vcard_property TEXT,
    vcard_group TEXT,
    vcard_prop_id TEXT,
    vcard_pid TEXT,
    vcard_altid TEXT,
    source TEXT NOT NULL CHECK (source IN (
        'user', 'carddav_import', 'vcard_import', 'archive_observation',
        'extraction', 'enrichment', 'system'
    )),
    source_ref TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ,
    active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_person_addresses_current
    ON person_addresses(person_id, address_kind, pref, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_addresses_person ON person_addresses(person_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_addresses_property_identity
    ON person_addresses(person_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL
      AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS person_dates (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    date_kind TEXT NOT NULL,
    label TEXT,
    date_year INTEGER CHECK (date_year BETWEEN 1 AND 9999),
    date_month INTEGER CHECK (date_month BETWEEN 1 AND 12),
    date_day INTEGER CHECK (date_day BETWEEN 1 AND 31),
    date_text TEXT,
    calendar_scale TEXT,
    original_value TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT,
    type_tokens TEXT,
    vcard_property TEXT,
    vcard_group TEXT,
    vcard_prop_id TEXT,
    vcard_pid TEXT,
    vcard_altid TEXT,
    source TEXT NOT NULL CHECK (source IN (
        'user', 'carddav_import', 'vcard_import', 'archive_observation',
        'extraction', 'enrichment', 'system'
    )),
    source_ref TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ,
    active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ,
    CHECK (date_day IS NULL OR date_month IS NOT NULL OR date_year IS NULL)
);
CREATE INDEX IF NOT EXISTS idx_person_dates_current
    ON person_dates(person_id, date_kind, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_dates_month_day
    ON person_dates(date_month, date_day)
    WHERE active_until IS NULL AND superseded_at IS NULL AND date_month IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_person_dates_person ON person_dates(person_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_dates_property_identity
    ON person_dates(person_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL
      AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS person_categories (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    original_value TEXT NOT NULL,
    normalized_value TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT,
    type_tokens TEXT,
    vcard_property TEXT,
    vcard_group TEXT,
    vcard_prop_id TEXT,
    vcard_pid TEXT,
    vcard_altid TEXT,
    source TEXT NOT NULL CHECK (source IN (
        'user', 'carddav_import', 'vcard_import', 'archive_observation',
        'extraction', 'enrichment', 'system'
    )),
    source_ref TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ,
    active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_categories_current_value
    ON person_categories(person_id, normalized_value)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_categories_value
    ON person_categories(normalized_value)
    WHERE active_until IS NULL AND superseded_at IS NULL;

-- Person PHOTO, LOGO, SOUND, and KEY payloads are inline because the packed
-- attachment CAS has no general write API and its liveness/GC authority is
-- the attachments table. Hash and size metadata keep later CAS migration
-- possible without changing row identity.
CREATE TABLE IF NOT EXISTS person_media (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    media_kind TEXT NOT NULL,
    media_type TEXT,
    uri TEXT,
    data BYTEA,
    byte_size BIGINT,
    content_hash TEXT,
    original_value TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT,
    type_tokens TEXT,
    vcard_property TEXT,
    vcard_group TEXT,
    vcard_prop_id TEXT,
    vcard_pid TEXT,
    vcard_altid TEXT,
    source TEXT NOT NULL CHECK (source IN (
        'user', 'carddav_import', 'vcard_import', 'archive_observation',
        'extraction', 'enrichment', 'system'
    )),
    source_ref TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ,
    active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_person_media_current
    ON person_media(person_id, media_kind, pref, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_media_person ON person_media(person_id);
CREATE INDEX IF NOT EXISTS idx_person_media_content_hash
    ON person_media(content_hash) WHERE content_hash IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_media_property_identity
    ON person_media(person_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL
      AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS participant_contact_observations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    source_id BIGINT REFERENCES sources(id) ON DELETE CASCADE,
    address_kind TEXT NOT NULL,
    service_id BIGINT REFERENCES communication_services(id) ON DELETE SET NULL,
    scope_kind TEXT,
    scope_value TEXT,
    provider_user_id TEXT,
    original_value TEXT NOT NULL,
    normalized_value TEXT NOT NULL,
    normalization TEXT NOT NULL DEFAULT 'none',
    normalization_version INTEGER NOT NULL DEFAULT 1,
    observed_at TIMESTAMPTZ,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT,
    type_tokens TEXT,
    vcard_property TEXT,
    vcard_group TEXT,
    vcard_prop_id TEXT,
    vcard_pid TEXT,
    vcard_altid TEXT,
    source TEXT NOT NULL CHECK (source IN (
        'user', 'carddav_import', 'vcard_import', 'archive_observation',
        'extraction', 'enrichment', 'system'
    )),
    source_ref TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ,
    active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_participant_observations_current_lookup
    ON participant_contact_observations(
        address_kind, service_id, scope_kind, scope_value, normalized_value
    ) WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_participant_observations_participant
    ON participant_contact_observations(participant_id);
CREATE INDEX IF NOT EXISTS idx_participant_observations_source
    ON participant_contact_observations(source_id) WHERE source_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_participant_observations_provider_user
    ON participant_contact_observations(provider_user_id) WHERE provider_user_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_participant_observations_identity
    ON participant_contact_observations(
        participant_id, source_id, address_kind, service_id, scope_kind, scope_value,
        normalized_value
    ) WHERE active_until IS NULL AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS identity_match_candidates (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    left_kind TEXT NOT NULL,
    left_id BIGINT NOT NULL,
    right_kind TEXT NOT NULL,
    right_id BIGINT NOT NULL,
    basis TEXT NOT NULL,
    service_id BIGINT REFERENCES communication_services(id) ON DELETE SET NULL,
    scope_kind TEXT,
    scope_value TEXT,
    normalized_value TEXT,
    state TEXT NOT NULL DEFAULT 'candidate',
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    source TEXT NOT NULL CHECK (source IN (
        'user', 'carddav_import', 'vcard_import', 'archive_observation',
        'extraction', 'enrichment', 'system'
    )),
    source_ref TEXT,
    observation_conflict_origin TEXT CHECK (
        observation_conflict_origin IN ('generated', 'promoted')
    ),
    pre_conflict_state TEXT CHECK (
        pre_conflict_state IN ('candidate', 'accepted', 'rejected')
    ),
    decided_by TEXT,
    decided_at TIMESTAMPTZ,
    notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_identity_match_candidates_edge
    ON identity_match_candidates(
        left_kind, left_id, right_kind, right_id, basis,
        service_id, scope_kind, scope_value, normalized_value
    );
CREATE INDEX IF NOT EXISTS idx_identity_match_candidates_state
    ON identity_match_candidates(state, id);
CREATE INDEX IF NOT EXISTS idx_identity_match_candidates_value
    ON identity_match_candidates(basis, normalized_value)
    WHERE normalized_value IS NOT NULL;

CREATE TABLE IF NOT EXISTS identity_match_evidence (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    candidate_id BIGINT NOT NULL REFERENCES identity_match_candidates(id) ON DELETE CASCADE,
    evidence_kind TEXT NOT NULL,
    evidence_ref TEXT,
    detail TEXT,
    source TEXT NOT NULL CHECK (source IN (
        'user', 'carddav_import', 'vcard_import', 'archive_observation',
        'extraction', 'enrichment', 'system'
    )),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_identity_match_evidence_candidate
    ON identity_match_evidence(candidate_id, id);

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
-- idx_participants_email_lower is built by Store.buildLargeIndexesConcurrently:
-- participants can be large on an existing archive, and an inline build here
-- would run under the pool-wide statement_timeout.
CREATE INDEX IF NOT EXISTS idx_participant_identifiers_value ON participant_identifiers(identifier_value);
CREATE INDEX IF NOT EXISTS idx_participant_identifiers_participant ON participant_identifiers(participant_id);
-- idx_participant_identifiers_value_lower is likewise built by
-- Store.buildLargeIndexesConcurrently.

CREATE INDEX IF NOT EXISTS idx_conversations_source ON conversations(source_id);
CREATE INDEX IF NOT EXISTS idx_conversations_last_message ON conversations(last_message_at DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_type ON conversations(conversation_type);

CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_source ON messages(source_id);
-- idx_messages_source_id is built by Store.buildLargeIndexesConcurrently via
-- CREATE INDEX CONCURRENTLY on a dedicated autocommit connection, outside any
-- transaction; building it over an existing archive can take a long time,
-- and CONCURRENTLY cannot run inside a transaction (or the pool-wide
-- statement_timeout escape hatch, which only disables the timeout for one
-- transaction).
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
