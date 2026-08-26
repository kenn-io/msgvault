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

-- Commit-ordered discovery for context-coupled embedding documents. This is a
-- locked singleton row, not BIGSERIAL/nextval: allocation and publication are
-- one source transaction, so rollback restores both and later allocators cannot
-- commit ahead of the transaction that holds the clock row.
CREATE TABLE IF NOT EXISTS embedding_change_clock (
    singleton SMALLINT PRIMARY KEY CHECK (singleton = 1),
    sequence BIGINT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT FALSE
);

INSERT INTO embedding_change_clock (singleton, sequence) VALUES (1, 0)
ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS embedding_changes (
    sequence BIGINT PRIMARY KEY,
    kind TEXT NOT NULL,
    message_id BIGINT,
    old_message_type TEXT,
    new_message_type TEXT,
    old_conversation_id BIGINT,
    new_conversation_id BIGINT,
    old_sent_at TIMESTAMPTZ,
    new_sent_at TIMESTAMPTZ,
    participant_id BIGINT
);

CREATE INDEX IF NOT EXISTS idx_embedding_changes_message_id
    ON embedding_changes(message_id);

-- Records importer-owned service registrations without exposing provenance as
-- a user-facing alias or overloading catalog metadata. User edits remove these
-- markers, which makes an explicitly configured service authoritative again.
-- This table ships with its first writer; existing unmarked services remain
-- user-owned because their provenance cannot be inferred safely.
CREATE TABLE IF NOT EXISTS communication_service_discoveries (
    service_id      BIGINT NOT NULL REFERENCES communication_services(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,
    discovery_kind  TEXT NOT NULL,
    PRIMARY KEY (service_id, provider, discovery_kind)
);

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

-- One external CardDAV connection. Passwords remain in the private token file
-- and are never persisted here.
CREATE TABLE IF NOT EXISTS carddav_discovery_lock (
    singleton SMALLINT PRIMARY KEY CHECK (singleton = 1)
);
INSERT INTO carddav_discovery_lock(singleton) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS carddav_accounts (
    id                    SMALLINT PRIMARY KEY CHECK (id = 1),
    base_url              TEXT NOT NULL,
    username              TEXT NOT NULL,
    principal_url         TEXT NOT NULL,
    home_url              TEXT NOT NULL,
    connection_generation BIGINT NOT NULL DEFAULT 1 CHECK (connection_generation > 0),
    discovery_revision    BIGINT NOT NULL DEFAULT 0 CHECK (discovery_revision >= 0),
    discovered_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS carddav_account_home_urls (
    account_id      SMALLINT NOT NULL REFERENCES carddav_accounts(id) ON DELETE CASCADE,
    home_url        TEXT NOT NULL,
    discovery_index INTEGER NOT NULL CHECK (discovery_index >= 0),
    PRIMARY KEY (account_id, home_url),
    UNIQUE (account_id, discovery_index)
);

CREATE TABLE IF NOT EXISTS carddav_retry_gate (
    account_id     SMALLINT PRIMARY KEY REFERENCES carddav_accounts(id) ON DELETE CASCADE,
    retry_after_at TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS carddav_address_books (
    id                       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id               SMALLINT NOT NULL REFERENCES carddav_accounts(id) ON DELETE CASCADE,
    canonical_url            TEXT NOT NULL,
    discovery_alias_url      TEXT,
    display_name             TEXT NOT NULL,
    discovery_index          INTEGER NOT NULL CHECK (discovery_index >= 0),
    supports_sync_collection BOOLEAN NOT NULL DEFAULT FALSE,
    supports_multiget        BOOLEAN NOT NULL DEFAULT FALSE,
    supported_vcard_versions JSONB NOT NULL DEFAULT '[]'::jsonb,
    can_create               BOOLEAN,
    can_update               BOOLEAN,
    can_delete               BOOLEAN,
    is_write_target          BOOLEAN NOT NULL DEFAULT FALSE,
    is_subscribed            BOOLEAN NOT NULL DEFAULT FALSE,
    is_lookup_source         BOOLEAN NOT NULL DEFAULT TRUE,
    sync_token               TEXT NOT NULL DEFAULT '',
    sync_revision            BIGINT NOT NULL DEFAULT 1 CHECK (sync_revision > 0),
    needs_full_reconcile     BOOLEAN NOT NULL DEFAULT FALSE,
    last_seen_revision       BIGINT NOT NULL CHECK (last_seen_revision >= 0),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (NOT is_write_target OR is_subscribed)
);

CREATE TABLE IF NOT EXISTS carddav_address_book_urls (
    account_id       SMALLINT NOT NULL REFERENCES carddav_accounts(id) ON DELETE CASCADE,
    address_book_id  BIGINT NOT NULL REFERENCES carddav_address_books(id) ON DELETE CASCADE,
    url_role         TEXT NOT NULL CHECK (url_role IN ('canonical', 'alias')),
    normalized_url   TEXT NOT NULL,
    PRIMARY KEY (address_book_id, url_role),
    UNIQUE (account_id, normalized_url)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_carddav_one_write_target
    ON carddav_address_books(account_id) WHERE is_write_target = TRUE;
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
-- vcard_projection_revision serializes native vCard envelope commits against
-- the semantic writes they project; see person_vcard_projection_revision.go.
-- It is deliberately separate from `revision`, the caller-facing
-- compare-and-swap token for the person record itself.
CREATE TABLE IF NOT EXISTS persons (
    id                        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    vcard_uid                 TEXT NOT NULL UNIQUE,
    display_name              TEXT,
    revision                  BIGINT NOT NULL DEFAULT 1,
    vcard_projection_revision BIGINT NOT NULL DEFAULT 1,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
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

-- Explicit opt-in for future profile maintenance. Row presence is the state;
-- no row means the person is not tracked.
CREATE TABLE IF NOT EXISTS person_tracking (
    person_id  BIGINT PRIMARY KEY REFERENCES persons(id) ON DELETE CASCADE,
    tracked_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Immutable, commit-ordered discovery for tracked-person archive changes.
-- The locked singleton row makes allocation order equal transaction commit
-- order and rolls allocation back with an aborted source mutation.
CREATE TABLE IF NOT EXISTS person_sweep_change_clock (
    singleton BOOLEAN PRIMARY KEY CHECK (singleton),
    sequence BIGINT NOT NULL CHECK (sequence >= 0),
    enabled BOOLEAN NOT NULL
);

INSERT INTO person_sweep_change_clock (singleton, sequence, enabled)
VALUES (TRUE, 0, TRUE)
ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS person_sweep_changes (
    sequence BIGINT PRIMARY KEY CHECK (sequence > 0),
    person_id BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    source_lane TEXT NOT NULL CHECK (source_lane IN (
        'conversation_text', 'meeting_text', 'attachment_caption',
        'attachment_ocr', 'document_text'
    )),
    change_kind TEXT NOT NULL CHECK (change_kind IN (
        'upsert', 'delete', 'scope', 'tracking', 'publication'
    )),
    evidence_effect TEXT NOT NULL DEFAULT '' CHECK (evidence_effect IN (
        '', 'source-deleted', 'source-edited', 'scope-unlinked',
        'identity-reassigned', 'source-reimported', 'scope-relinked'
    )),
    source_id BIGINT,
    message_id BIGINT,
    attachment_id BIGINT,
    occurrence_key TEXT NOT NULL DEFAULT '',
    recorded_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_person_sweep_changes_person_sequence
    ON person_sweep_changes(person_id, sequence);
CREATE INDEX IF NOT EXISTS idx_person_sweep_changes_source_sequence
    ON person_sweep_changes(source_id, sequence);

CREATE TABLE IF NOT EXISTS person_sweep_work (
    person_id                 BIGINT PRIMARY KEY REFERENCES persons(id) ON DELETE CASCADE,
    dirty_through_sequence    BIGINT NOT NULL CHECK (dirty_through_sequence >= 0),
    available_at              TIMESTAMPTZ NOT NULL,
    attempt_count             INTEGER NOT NULL CHECK (attempt_count >= 0),
    last_failure_class        TEXT NOT NULL DEFAULT '',
    lease_owner               TEXT NOT NULL DEFAULT '',
    lease_until               TIMESTAMPTZ,
    lease_fence               BIGINT NOT NULL CHECK (lease_fence >= 0),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_person_sweep_work_available
    ON person_sweep_work(available_at, person_id);
CREATE INDEX IF NOT EXISTS idx_person_sweep_work_lease
    ON person_sweep_work(lease_until, person_id);

CREATE TABLE IF NOT EXISTS person_sweep_cursors (
    person_id                   BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    source_lane                 TEXT NOT NULL CHECK (source_lane IN (
        'conversation_text', 'meeting_text', 'attachment_caption',
        'attachment_ocr', 'document_text'
    )),
    program_fingerprint         TEXT NOT NULL,
    catalog_fingerprint         TEXT NOT NULL,
    optimistic_sequence         BIGINT NOT NULL CHECK (optimistic_sequence >= 0),
    optimistic_document_key     TEXT NOT NULL DEFAULT '',
    reconcile_upper_key         TEXT NOT NULL,
    reconcile_after_key         TEXT NOT NULL,
    reconcile_document_key      TEXT NOT NULL DEFAULT '',
    reconciliation_complete     BOOLEAN NOT NULL,
    backstop_upper_key           TEXT NOT NULL DEFAULT '',
    backstop_after_key           TEXT NOT NULL DEFAULT '',
    backstop_document_key        TEXT NOT NULL DEFAULT '',
    last_backstop_at            TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (person_id, source_lane, program_fingerprint, catalog_fingerprint)
);

CREATE INDEX IF NOT EXISTS idx_person_sweep_cursors_incomplete
    ON person_sweep_cursors(person_id)
    WHERE reconciliation_complete = FALSE;

CREATE TABLE IF NOT EXISTS person_inference_profiles (
    fingerprint          TEXT PRIMARY KEY,
    provider_kind        TEXT NOT NULL,
    endpoint             TEXT NOT NULL,
    model                TEXT NOT NULL,
    api_key_env          TEXT NOT NULL,
    allow_anonymous      BOOLEAN NOT NULL DEFAULT FALSE,
    retention_posture    TEXT NOT NULL,
    training_posture     TEXT NOT NULL,
    allowed_sources      JSONB NOT NULL,
    source_since         TEXT NOT NULL,
    source_until         TEXT,
    allow_sensitive      BOOLEAN NOT NULL DEFAULT FALSE,
    policy_json          JSONB NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS person_inference_consents (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    profile_fingerprint  TEXT NOT NULL REFERENCES person_inference_profiles(fingerprint),
    granted_by           TEXT NOT NULL,
    granted_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_by           TEXT,
    revoked_at           TIMESTAMPTZ,
    CHECK ((revoked_by IS NULL) = (revoked_at IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_inference_consents_active
    ON person_inference_consents(profile_fingerprint)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS person_semantic_embedding_profiles (
    fingerprint             TEXT PRIMARY KEY,
    purpose                 TEXT NOT NULL,
    destination             TEXT NOT NULL,
    api_format              TEXT NOT NULL,
    model                   TEXT NOT NULL,
    api_key_env             TEXT NOT NULL,
    retention_posture       TEXT NOT NULL,
    training_posture        TEXT NOT NULL,
    renderer_policy         TEXT NOT NULL,
    disclosed_field_classes JSONB NOT NULL,
    corpus_scope            TEXT NOT NULL,
    policy_json             JSONB NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS person_semantic_embedding_consents (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    profile_fingerprint TEXT NOT NULL REFERENCES person_semantic_embedding_profiles(fingerprint),
    granted_by          TEXT NOT NULL,
    granted_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_by          TEXT,
    revoked_at          TIMESTAMPTZ,
    CHECK ((revoked_by IS NULL) = (revoked_at IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_semantic_embedding_consents_active
    ON person_semantic_embedding_consents(profile_fingerprint)
    WHERE revoked_at IS NULL;

-- Immutable evidence and generation envelopes for automatic person facts.
-- Rows remain append-only for the lifetime of their owning person.
CREATE TABLE IF NOT EXISTS person_fact_evidence (
    id                BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    person_id         BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    evidence_key      TEXT NOT NULL,
    source_class      TEXT NOT NULL
                           CHECK (source_class IN ('archive', 'public', 'system', 'provider_assertion')),
    directness        TEXT NOT NULL
                           CHECK (directness IN ('direct-self', 'direct-other', 'indirect')),
    authority         TEXT NOT NULL
                           CHECK (authority IN ('authoritative', 'ordinary', 'aggregator')),
    source_ref        TEXT NOT NULL DEFAULT '',
    source_url        TEXT NOT NULL DEFAULT '',
    subject_person_id BIGINT,
    subject_ref       TEXT NOT NULL DEFAULT '',
    span_start        BIGINT,
    span_end          BIGINT,
    excerpt           TEXT NOT NULL DEFAULT '',
    content_sha256    TEXT NOT NULL DEFAULT '',
    source_version    TEXT NOT NULL DEFAULT '',
    event_time        TIMESTAMPTZ NOT NULL,
    recorded_time     TIMESTAMPTZ NOT NULL,
    identity_score    INTEGER NOT NULL CHECK (identity_score BETWEEN 0 AND 1000),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(person_id, evidence_key),
    CHECK ((span_start IS NULL) = (span_end IS NULL)),
    CHECK (span_start IS NULL OR (span_start >= 0 AND span_end >= span_start))
);

CREATE TABLE IF NOT EXISTS person_fact_generations (
    id                          BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    person_id                   BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    generation_key              TEXT NOT NULL,
    source_cursors_json         JSONB NOT NULL,
    program_id                  TEXT NOT NULL,
    program_version             TEXT NOT NULL,
    program_fingerprint         TEXT NOT NULL
                                     CHECK (program_fingerprint ~ '^[0-9a-f]{64}$'),
    catalog_fingerprint         TEXT NOT NULL,
    provider                    TEXT NOT NULL,
    provider_version            TEXT NOT NULL,
    model                       TEXT NOT NULL,
    model_version               TEXT NOT NULL,
    provider_policy_fingerprint TEXT NOT NULL,
    resolved_at                 TIMESTAMPTZ NOT NULL,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(person_id, generation_key)
);

CREATE TABLE IF NOT EXISTS person_sweep_runs (
    id                          TEXT PRIMARY KEY,
    kind                        TEXT NOT NULL CHECK (kind IN ('scheduled', 'manual')),
    mode                        TEXT NOT NULL CHECK (mode IN ('incremental', 'backstop')),
    status                      TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'partial', 'failed')),
    program_fingerprint         TEXT NOT NULL,
    catalog_fingerprint         TEXT NOT NULL,
    provider_fingerprint        TEXT NOT NULL,
    attempt_count               INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    success_count               INTEGER NOT NULL DEFAULT 0 CHECK (success_count >= 0),
    failure_count               INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
    projected_write_count       INTEGER NOT NULL DEFAULT 0 CHECK (projected_write_count >= 0),
    actual_requests             INTEGER NOT NULL DEFAULT 0 CHECK (actual_requests >= 0),
    actual_input_tokens         BIGINT NOT NULL DEFAULT 0 CHECK (actual_input_tokens >= 0),
    actual_output_tokens        BIGINT NOT NULL DEFAULT 0 CHECK (actual_output_tokens >= 0),
    actual_cost_micro_usd       BIGINT NOT NULL DEFAULT 0 CHECK (actual_cost_micro_usd >= 0),
    started_at                  TIMESTAMPTZ NOT NULL,
    completed_at                TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS person_sweep_attempts (
    id                          TEXT PRIMARY KEY,
    run_id                      TEXT NOT NULL REFERENCES person_sweep_runs(id) ON DELETE CASCADE,
    person_id                   BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    lease_fence                 BIGINT NOT NULL CHECK (lease_fence >= 0),
    mode                        TEXT NOT NULL CHECK (mode IN ('incremental', 'backstop')),
    status                      TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    failure_class               TEXT NOT NULL DEFAULT '' CHECK (failure_class IN (
        '', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
        'provider_http', 'invalid_output', 'archive_gap', 'internal'
    )),
    cursor_envelope_json        TEXT NOT NULL,
    envelope_hash               TEXT NOT NULL,
    program_fingerprint         TEXT NOT NULL,
    catalog_fingerprint         TEXT NOT NULL,
    provider_fingerprint        TEXT NOT NULL,
    generation_id               BIGINT REFERENCES person_fact_generations(id) ON DELETE SET NULL,
    generation_key              TEXT NOT NULL DEFAULT '',
    seed_count                   INTEGER NOT NULL DEFAULT 0 CHECK (seed_count >= 0),
    context_count                INTEGER NOT NULL DEFAULT 0 CHECK (context_count >= 0),
    claim_count                  INTEGER NOT NULL DEFAULT 0 CHECK (claim_count >= 0),
    decision_count               INTEGER NOT NULL DEFAULT 0 CHECK (decision_count >= 0),
    projected_write_count       INTEGER NOT NULL DEFAULT 0 CHECK (projected_write_count >= 0),
    request_count                INTEGER NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    provider_request_id         TEXT NOT NULL DEFAULT '',
    input_tokens                BIGINT NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens               BIGINT NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    estimated_cost_micro_usd    BIGINT NOT NULL DEFAULT 0 CHECK (estimated_cost_micro_usd >= 0),
    latency_milliseconds        BIGINT NOT NULL DEFAULT 0 CHECK (latency_milliseconds >= 0),
    retry_at                    TIMESTAMPTZ,
    started_at                  TIMESTAMPTZ NOT NULL,
    completed_at                TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_person_started
    ON person_sweep_attempts(person_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_run
    ON person_sweep_attempts(run_id, id);
CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_generation
    ON person_sweep_attempts(generation_id);

CREATE TABLE IF NOT EXISTS person_sweep_batches (
    attempt_id                  TEXT NOT NULL REFERENCES person_sweep_attempts(id) ON DELETE CASCADE,
    batch_ordinal               INTEGER NOT NULL CHECK (batch_ordinal >= 0),
    utc_day                     TEXT NOT NULL,
    reservation_id              TEXT NOT NULL,
    budget_fingerprint          TEXT NOT NULL,
    input_hash                  TEXT NOT NULL,
    item_count                  INTEGER NOT NULL CHECK (item_count >= 0),
    status                      TEXT NOT NULL CHECK (status IN ('reserved', 'running', 'succeeded', 'failed', 'cancelled')),
    provider_request_id         TEXT NOT NULL DEFAULT '',
    reserved_requests           INTEGER NOT NULL CHECK (reserved_requests >= 0),
    reserved_input_tokens       BIGINT NOT NULL CHECK (reserved_input_tokens >= 0),
    reserved_output_tokens      BIGINT NOT NULL CHECK (reserved_output_tokens >= 0),
    reserved_cost_micro_usd     BIGINT NOT NULL CHECK (reserved_cost_micro_usd >= 0),
    actual_requests             INTEGER NOT NULL DEFAULT 0 CHECK (actual_requests >= 0),
    actual_input_tokens         BIGINT NOT NULL DEFAULT 0 CHECK (actual_input_tokens >= 0),
    actual_output_tokens        BIGINT NOT NULL DEFAULT 0 CHECK (actual_output_tokens >= 0),
    actual_cost_micro_usd       BIGINT NOT NULL DEFAULT 0 CHECK (actual_cost_micro_usd >= 0),
    latency_milliseconds        BIGINT NOT NULL DEFAULT 0 CHECK (latency_milliseconds >= 0),
    failure_class               TEXT NOT NULL DEFAULT '' CHECK (failure_class IN (
        '', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
        'provider_http', 'invalid_output', 'archive_gap', 'internal'
    )),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at                TIMESTAMPTZ,
    PRIMARY KEY (attempt_id, batch_ordinal)
);

CREATE TABLE IF NOT EXISTS person_sweep_daily_usage (
    utc_day                     TEXT PRIMARY KEY,
    reserved_requests           INTEGER NOT NULL DEFAULT 0 CHECK (reserved_requests >= 0),
    reserved_input_tokens       BIGINT NOT NULL DEFAULT 0 CHECK (reserved_input_tokens >= 0),
    reserved_output_tokens      BIGINT NOT NULL DEFAULT 0 CHECK (reserved_output_tokens >= 0),
    reserved_cost_micro_usd     BIGINT NOT NULL DEFAULT 0 CHECK (reserved_cost_micro_usd >= 0),
    actual_requests             INTEGER NOT NULL DEFAULT 0 CHECK (actual_requests >= 0),
    actual_input_tokens         BIGINT NOT NULL DEFAULT 0 CHECK (actual_input_tokens >= 0),
    actual_output_tokens        BIGINT NOT NULL DEFAULT 0 CHECK (actual_output_tokens >= 0),
    actual_cost_micro_usd       BIGINT NOT NULL DEFAULT 0 CHECK (actual_cost_micro_usd >= 0),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS person_fact_claims (
    id                    BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    person_id             BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    generation_id         BIGINT NOT NULL REFERENCES person_fact_generations(id) ON DELETE CASCADE,
    claim_key             TEXT NOT NULL,
    target_kind           TEXT NOT NULL,
    target_key            TEXT NOT NULL,
    target_revision       TEXT NOT NULL,
    relation              TEXT NOT NULL CHECK (relation IN ('support', 'contradict', 'supersede', 'invalid')),
    submitted_value_json  TEXT NOT NULL,
    normalized_value_json JSONB,
    value_fingerprint     TEXT,
    valid_from            TIMESTAMPTZ,
    valid_until           TIMESTAMPTZ,
    origin                TEXT NOT NULL CHECK (origin IN ('extraction', 'enrichment', 'system', 'invalid')),
    confidence_json       JSONB NOT NULL,
    rejection_action      TEXT CHECK (rejection_action IN (
                              'applied', 'retained', 'superseded', 'invalid', 'identity-rejected',
                              'policy-rejected', 'conflict-rejected', 'ambiguous-retained')),
    rejection_reason      TEXT CHECK (rejection_reason IN (
                              'malformed-value', 'unsupported-target', 'stale-target-revision',
                              'unaligned-evidence', 'identity-mismatch', 'sensitive-policy',
                              'pin-retained', 'below-threshold', 'insufficient-margin',
                              'competing-tie', 'explicit-contradiction', 'explicit-supersession',
                              'organization-ambiguous', 'applied-projection', 'evidence-unsupported',
                              'outside-validity')),
    rejection_detail      TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((rejection_action IS NULL AND rejection_reason IS NULL AND rejection_detail IS NULL) OR
           (rejection_action IS NOT NULL AND rejection_reason IS NOT NULL AND
            rejection_detail IS NOT NULL AND rejection_detail <> '')),
    UNIQUE(person_id, claim_key),
    CHECK ((normalized_value_json IS NULL) = (value_fingerprint IS NULL)),
    CHECK (valid_from IS NULL OR valid_until IS NULL OR valid_until >= valid_from)
);

CREATE TABLE IF NOT EXISTS person_fact_claim_evidence (
    claim_id    BIGINT NOT NULL REFERENCES person_fact_claims(id) ON DELETE CASCADE,
    evidence_id BIGINT NOT NULL REFERENCES person_fact_evidence(id) ON DELETE CASCADE,
    PRIMARY KEY (claim_id, evidence_id)
);

CREATE TABLE IF NOT EXISTS person_fact_evidence_status_events (
    id             BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    person_id      BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    generation_id  BIGINT NOT NULL REFERENCES person_fact_generations(id) ON DELETE CASCADE,
    evidence_id    BIGINT NOT NULL REFERENCES person_fact_evidence(id) ON DELETE CASCADE,
    evidence_key   TEXT NOT NULL,
    source_version TEXT NOT NULL,
    supported      BOOLEAN NOT NULL,
    reason         TEXT NOT NULL CHECK (reason IN (
                       'source-deleted', 'source-edited', 'scope-unlinked',
                       'identity-reassigned', 'source-reimported', 'scope-relinked')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(person_id, generation_id, evidence_key, source_version)
);

CREATE TABLE IF NOT EXISTS person_fact_resolutions (
    id                          BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    person_id                   BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    generation_id               BIGINT NOT NULL REFERENCES person_fact_generations(id) ON DELETE CASCADE,
    target_kind                 TEXT NOT NULL,
    target_key                  TEXT NOT NULL,
    target_revision             TEXT NOT NULL,
    resolver_version            TEXT NOT NULL,
    input_fingerprint           TEXT NOT NULL,
    provider_policy_fingerprint TEXT NOT NULL,
    resolved_at                 TIMESTAMPTZ NOT NULL,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(generation_id, target_kind, target_key, resolver_version, input_fingerprint)
);

CREATE TABLE IF NOT EXISTS person_fact_decisions (
    id                 BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    person_id          BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    resolution_id      BIGINT NOT NULL REFERENCES person_fact_resolutions(id) ON DELETE CASCADE,
    claim_id           BIGINT REFERENCES person_fact_claims(id) ON DELETE CASCADE,
    decision_key       TEXT NOT NULL,
    action             TEXT NOT NULL CHECK (action IN (
                           'applied', 'retained', 'superseded', 'invalid',
                           'identity-rejected', 'policy-rejected', 'conflict-rejected',
                           'ambiguous-retained')),
    reason             TEXT NOT NULL CHECK (reason IN (
                           'malformed-value', 'unsupported-target', 'stale-target-revision',
                           'unaligned-evidence', 'identity-mismatch', 'sensitive-policy',
                           'pin-retained', 'below-threshold', 'insufficient-margin',
                           'competing-tie', 'explicit-contradiction', 'explicit-supersession',
                           'organization-ambiguous', 'applied-projection', 'evidence-unsupported',
                           'outside-validity')),
    score_json         JSONB NOT NULL,
    competing_claim_id BIGINT REFERENCES person_fact_claims(id) ON DELETE SET NULL,
    projection_kind    TEXT,
    projection_row_id  BIGINT,
    resolved_organization_id BIGINT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(person_id, decision_key),
    CHECK ((projection_kind IS NULL) = (projection_row_id IS NULL))
);

CREATE TABLE IF NOT EXISTS person_fact_pin_events (
    id              BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    person_id       BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    target_kind     TEXT NOT NULL CHECK (target_kind IN ('attribute', 'employment')),
    target_key      TEXT NOT NULL,
    target_revision TEXT NOT NULL,
    pinned          BOOLEAN NOT NULL,
    actor           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_person_fact_generations_person
    ON person_fact_generations(person_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_person_fact_evidence_person
    ON person_fact_evidence(person_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_person_fact_evidence_status_latest
    ON person_fact_evidence_status_events(person_id, evidence_key, source_version, id DESC);
CREATE INDEX IF NOT EXISTS idx_person_fact_claims_person_target
    ON person_fact_claims(person_id, target_kind, target_key, id DESC);
CREATE INDEX IF NOT EXISTS idx_person_fact_claim_evidence_evidence
    ON person_fact_claim_evidence(evidence_id, claim_id);
CREATE INDEX IF NOT EXISTS idx_person_fact_resolutions_person_target
    ON person_fact_resolutions(person_id, target_kind, target_key, id DESC);
CREATE INDEX IF NOT EXISTS idx_person_fact_decisions_person
    ON person_fact_decisions(person_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_person_fact_decisions_projection
    ON person_fact_decisions(person_id, projection_kind, projection_row_id);
CREATE INDEX IF NOT EXISTS idx_person_fact_pin_events_latest
    ON person_fact_pin_events(person_id, target_kind, target_key, id DESC);

-- Lossless native vCard resources. Typed profile tables remain the semantic
-- source of truth; this table retains exact wire bodies and normalized
-- occurrence metadata for future CardDAV layers.
CREATE TABLE IF NOT EXISTS vcard_resource_envelopes (
    id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id              BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    canonical_person_uid   TEXT NOT NULL,
    source_ref             TEXT NOT NULL CHECK(length(trim(source_ref)) > 0),
    source_resource_uid    TEXT NOT NULL CHECK(length(trim(source_resource_uid)) > 0),
    href                   TEXT,
    original_raw_bytes     BYTEA NOT NULL,
    stored_body            BYTEA NOT NULL,
    resource_metadata      JSONB NOT NULL,
    projection_fingerprint TEXT,
    content_hash           TEXT NOT NULL,
    etag                   TEXT NOT NULL,
    revision               BIGINT NOT NULL DEFAULT 1,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(source_ref, source_resource_uid)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vcard_resource_envelopes_href
    ON vcard_resource_envelopes(source_ref, href)
    WHERE href IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_vcard_resource_envelopes_person
    ON vcard_resource_envelopes(person_id);
CREATE INDEX IF NOT EXISTS idx_vcard_resource_envelopes_canonical_uid
    ON vcard_resource_envelopes(canonical_person_uid);

-- Retired canonical UIDs are a separate namespace from source-resource UIDs.
CREATE TABLE IF NOT EXISTS person_uid_aliases (
    retired_uid          TEXT PRIMARY KEY,
    surviving_person_id  BIGINT REFERENCES persons(id) ON DELETE SET NULL,
    reason               TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS carddav_resources (
    id                      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    address_book_id         BIGINT NOT NULL REFERENCES carddav_address_books(id) ON DELETE CASCADE,
    href                    TEXT NOT NULL,
    remote_uid              TEXT,
    remote_etag             TEXT NOT NULL,
    remote_body             BYTEA NOT NULL,
    remote_semantic_hash    TEXT NOT NULL,
    local_hash              TEXT NOT NULL,
    mapping_status          TEXT NOT NULL CHECK (mapping_status IN ('mapped', 'unbound', 'ambiguous')),
    mapping_revision        BIGINT NOT NULL DEFAULT 1 CHECK (mapping_revision > 0),
    governance              TEXT NOT NULL CHECK (governance IN ('remote', 'local', 'none')),
    person_id               BIGINT REFERENCES persons(id) ON DELETE SET NULL,
    person_revision_at_bind BIGINT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (address_book_id, href)
);
CREATE TABLE IF NOT EXISTS carddav_publications (
    person_id                   BIGINT PRIMARY KEY REFERENCES persons(id) ON DELETE RESTRICT,
    desired                     BOOLEAN NOT NULL DEFAULT TRUE,
    address_book_id             BIGINT REFERENCES carddav_address_books(id) ON DELETE CASCADE,
    href                        TEXT,
    pending_operation           TEXT CHECK (pending_operation IN ('create', 'update', 'delete')),
    outgoing_body               BYTEA,
    outgoing_semantic_hash      TEXT,
    local_hash                  TEXT,
    remote_etag                 TEXT,
    connection_generation       BIGINT,
    book_sync_revision          BIGINT,
    mapping_revision            BIGINT,
    previous_mapping_revision   BIGINT,
    create_recovery_used        BOOLEAN NOT NULL DEFAULT FALSE,
    mutation_revision           BIGINT NOT NULL DEFAULT 0 CHECK (mutation_revision >= 0),
    pending_started_at          TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((pending_operation IS NULL) OR
           (address_book_id IS NOT NULL AND href IS NOT NULL AND
            local_hash IS NOT NULL AND connection_generation IS NOT NULL AND
            book_sync_revision IS NOT NULL AND mapping_revision IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_carddav_publications_pending
    ON carddav_publications(pending_operation, person_id);
CREATE TABLE IF NOT EXISTS carddav_conflicts (
    id                       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    address_book_id          BIGINT NOT NULL REFERENCES carddav_address_books(id) ON DELETE CASCADE,
    href                     TEXT NOT NULL,
    base_local_hash          TEXT NOT NULL,
    local_hash               TEXT NOT NULL,
    base_remote_hash         TEXT NOT NULL,
    base_remote_etag         TEXT NOT NULL,
    remote_etag              TEXT,
    mapping_revision         BIGINT NOT NULL CHECK (mapping_revision > 0),
    local_body               BYTEA,
    remote_body              BYTEA,
    local_tombstone          BOOLEAN NOT NULL DEFAULT FALSE,
    remote_tombstone         BOOLEAN NOT NULL DEFAULT FALSE,
    pending_operation        TEXT CHECK (pending_operation IN ('delete')),
    connection_generation    BIGINT,
    book_sync_revision       BIGINT,
    previous_mapping_revision BIGINT,
    pending_started_at       TIMESTAMPTZ,
    status                   TEXT NOT NULL DEFAULT 'unresolved' CHECK (status IN ('unresolved', 'resolved')),
    resolution               TEXT CHECK (resolution IN ('keep_local', 'keep_remote')),
    resolved_at              TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (local_tombstone OR local_body IS NOT NULL),
    CHECK (remote_tombstone OR remote_body IS NOT NULL),
    CONSTRAINT carddav_conflicts_pending_invariant CHECK (pending_operation IS NULL OR
           (status = 'unresolved' AND local_tombstone AND remote_etag IS NOT NULL AND
            connection_generation IS NOT NULL AND book_sync_revision IS NOT NULL AND
            previous_mapping_revision IS NOT NULL AND pending_started_at IS NOT NULL)),
    CHECK ((status = 'unresolved' AND resolution IS NULL AND resolved_at IS NULL) OR
           (status = 'resolved' AND resolution IS NOT NULL AND resolved_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_carddav_one_unresolved_conflict
    ON carddav_conflicts(address_book_id, href) WHERE status = 'unresolved';
CREATE INDEX IF NOT EXISTS idx_carddav_conflicts_resolved_at
    ON carddav_conflicts(status, resolved_at);
CREATE INDEX IF NOT EXISTS idx_carddav_resources_person
    ON carddav_resources(person_id) WHERE person_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_person_uid_aliases_survivor
    ON person_uid_aliases(surviving_person_id)
    WHERE surviving_person_id IS NOT NULL;

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
CREATE INDEX IF NOT EXISTS idx_conversation_participants_participant
    ON conversation_participants(participant_id, conversation_id);

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
	attachment_state TEXT,
	attachment_skip_reason TEXT,

    attachment_role TEXT NOT NULL DEFAULT 'unknown'
        CHECK (attachment_role IN ('standalone', 'inline', 'avatar', 'thumbnail', 'preview', 'sticker', 'ui_asset', 'unknown')),
    role_source TEXT NOT NULL DEFAULT 'unknown'
        CHECK (role_source IN ('mime_disposition', 'provider_explicit', 'importer_semantics', 'legacy_api', 'raw_mime_repair', 'unknown')),
    source_part_key TEXT CHECK (source_part_key IS NULL OR source_part_key != ''),
    content_id TEXT,

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

CREATE TABLE IF NOT EXISTS attachment_role_repair_progress (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    last_message_id BIGINT NOT NULL DEFAULT 0,
    completed INTEGER NOT NULL DEFAULT 0 CHECK (completed IN (0, 1)),
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS attachment_change_log (
    sequence                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_kind              TEXT NOT NULL CHECK (event_kind IN (
                                'attachment_insert', 'attachment_update',
                                'attachment_delete', 'message_live_enter',
                                'message_live_exit')),
    old_message_id          BIGINT,
    new_message_id          BIGINT,
    old_attachment_id       BIGINT,
    new_attachment_id       BIGINT,
    old_content_hash        TEXT,
    new_content_hash        TEXT,
    old_source_part_key     TEXT,
    new_source_part_key     TEXT,
    old_role                TEXT,
    new_role                TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS attachment_change_consumers (
    consumer_key            TEXT PRIMARY KEY,
    baseline_sequence       BIGINT NOT NULL,
    last_sequence           BIGINT NOT NULL,
    reconciliation_complete BOOLEAN NOT NULL DEFAULT FALSE,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (baseline_sequence >= 0),
    CHECK (last_sequence >= baseline_sequence)
);

CREATE TABLE IF NOT EXISTS visual_generations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    fingerprint TEXT NOT NULL UNIQUE,
    model TEXT NOT NULL,
    dimension INTEGER NOT NULL CHECK (dimension = 1024),
    state TEXT NOT NULL DEFAULT 'building'
        CHECK (state IN ('building', 'active', 'retired')),
    source_fence BIGINT NOT NULL DEFAULT 0,
    consented_at TIMESTAMPTZ,
    -- Docbank Voyage policy fingerprint the consent was recorded against. A
    -- re-probed manifest changes the fingerprint and requires new consent.
    consent_policy_fingerprint TEXT,
    -- Policy fingerprint the archive was last reconciled under. A change
    -- forces a full re-evaluation of every candidate without discarding the
    -- generation's published vectors.
    capability_fingerprint TEXT,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    activated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_visual_generations_active
    ON visual_generations(state) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS idx_visual_generations_building
    ON visual_generations(state) WHERE state = 'building';

CREATE TABLE IF NOT EXISTS visual_publications (
    generation_id BIGINT NOT NULL REFERENCES visual_generations(id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    blob_hash TEXT NOT NULL,
    media_input_key TEXT NOT NULL,
    published_revision TEXT,
    prepared_revision TEXT,
    source_fence BIGINT NOT NULL DEFAULT 0,
    representative_attachment_id BIGINT,
    attachment_role TEXT NOT NULL,
    role_source TEXT NOT NULL,
    current_vector_token TEXT,
    pending_vector_token TEXT,
    state TEXT NOT NULL CHECK (state IN ('current', 'stale', 'tombstoned')),
    outcome_kind TEXT,
    outcome_reason TEXT,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (generation_id, message_id, blob_hash, media_input_key)
);

-- Backend vector tokens the archive no longer references but whose backend
-- rows still need deleting. Every statement that drops a token from
-- current_vector_token or pending_vector_token records it here in the same
-- transaction, so a crashed or failed inline backend delete is retried by
-- the obsolete-token sweep instead of orphaning the vector. Multi-row: any
-- number of tokens can be pending cleanup for one owner.
CREATE TABLE IF NOT EXISTS visual_obsolete_tokens (
    generation_id BIGINT NOT NULL REFERENCES visual_generations(id) ON DELETE CASCADE,
    vector_token TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (generation_id, vector_token)
);

CREATE TABLE IF NOT EXISTS visual_work_claims (
    generation_id BIGINT NOT NULL REFERENCES visual_generations(id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    blob_hash TEXT NOT NULL,
    media_input_key TEXT NOT NULL,
    proposed_revision TEXT NOT NULL,
    lease_owner TEXT NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    fencing_token BIGINT NOT NULL,
    source_fence BIGINT NOT NULL,
    -- CAS stamp of the owning message's context at claim time. Commit
    -- refuses when the live stamp differs, so a subject or body edit during
    -- a provider request can never publish a vector of the old context.
    claimed_content_stamp TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (generation_id, message_id, blob_hash, media_input_key, proposed_revision)
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

CREATE TABLE IF NOT EXISTS person_sweep_sync_publications (
    sync_run_id BIGINT PRIMARY KEY REFERENCES sync_runs(id) ON DELETE CASCADE,
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    lower_sequence BIGINT NOT NULL CHECK (lower_sequence >= 0),
    upper_sequence BIGINT CHECK (
        upper_sequence IS NULL OR upper_sequence >= lower_sequence
    ),
    published_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_person_sweep_sync_publications_source
    ON person_sweep_sync_publications(source_id, sync_run_id);

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
    highest_modseq NUMERIC(20, 0) NOT NULL DEFAULT 0,

    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, mailbox)
);

CREATE TABLE IF NOT EXISTS imap_message_memberships (
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    mailbox TEXT NOT NULL,
    uidvalidity BIGINT NOT NULL,
    uid BIGINT NOT NULL,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    flags JSONB NOT NULL DEFAULT '[]'::JSONB,

    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, mailbox, uidvalidity, uid)
);

CREATE INDEX IF NOT EXISTS idx_imap_message_memberships_source_message
    ON imap_message_memberships(source_id, message_id);

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

-- ============================================================================
-- ORGANIZATIONS & EMPLOYMENT
-- ============================================================================
CREATE TABLE IF NOT EXISTS organizations (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            TEXT NOT NULL,
    name_normalized TEXT NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'other',
    primary_domain  TEXT,
    description     TEXT,
    revision        BIGINT NOT NULL DEFAULT 1,
    merged_into_id  BIGINT REFERENCES organizations(id) ON DELETE RESTRICT,
    retired_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (LENGTH(name) > 0),
    CHECK (merged_into_id IS NULL OR merged_into_id <> id)
);
CREATE INDEX IF NOT EXISTS idx_organizations_name_normalized
    ON organizations(name_normalized);
CREATE INDEX IF NOT EXISTS idx_organizations_primary_domain
    ON organizations(primary_domain)
    WHERE primary_domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_organizations_merged_into
    ON organizations(merged_into_id)
    WHERE merged_into_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS organization_names (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name_kind TEXT NOT NULL,
    formatted TEXT,
    original_value TEXT NOT NULL, name_normalized TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT, type_tokens TEXT,
    vcard_property TEXT, vcard_group TEXT, vcard_prop_id TEXT,
    vcard_pid TEXT, vcard_altid TEXT,
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ, active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_names_active
    ON organization_names(organization_id, name_kind, name_normalized)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_organization_names_lookup
    ON organization_names(name_normalized)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_names_property_identity
    ON organization_names(organization_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS organization_identifiers (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    identifier_kind TEXT NOT NULL, identifier_value TEXT NOT NULL,
    normalized_value TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT, type_tokens TEXT,
    vcard_property TEXT, vcard_group TEXT, vcard_prop_id TEXT,
    vcard_pid TEXT, vcard_altid TEXT,
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ, active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_identifiers_active
    ON organization_identifiers(organization_id, identifier_kind, normalized_value)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_organization_identifiers_lookup
    ON organization_identifiers(identifier_kind, normalized_value)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_identifiers_property_identity
    ON organization_identifiers(organization_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS organization_addresses (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    address_kind TEXT NOT NULL DEFAULT 'postal',
    post_office_box TEXT, extended_address TEXT, street_address TEXT,
    locality TEXT, region TEXT, postal_code TEXT, country_name TEXT,
    extended_components TEXT, free_text TEXT, label TEXT, geo_uri TEXT,
    timezone TEXT, country_code TEXT, place_uri TEXT,
    original_value TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT, type_tokens TEXT,
    vcard_property TEXT, vcard_group TEXT, vcard_prop_id TEXT,
    vcard_pid TEXT, vcard_altid TEXT,
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ, active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_organization_addresses_current
    ON organization_addresses(organization_id, address_kind, pref, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_addresses_property_identity
    ON organization_addresses(organization_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS organization_contact_points (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    address_kind TEXT NOT NULL,
    service_id BIGINT REFERENCES communication_services(id) ON DELETE RESTRICT,
    scope_kind TEXT, scope_value TEXT,
    original_value TEXT NOT NULL, normalized_value TEXT NOT NULL,
    normalization TEXT NOT NULL DEFAULT 'none',
    normalization_version INTEGER NOT NULL DEFAULT 1,
    uri TEXT,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT, type_tokens TEXT,
    vcard_property TEXT, vcard_group TEXT, vcard_prop_id TEXT,
    vcard_pid TEXT, vcard_altid TEXT,
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ, active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_organization_contact_points_current_lookup
    ON organization_contact_points(address_kind, service_id, scope_kind, scope_value, normalized_value)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_contact_points_property_identity
    ON organization_contact_points(organization_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS organization_categories (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    original_value TEXT NOT NULL, normalized_value TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT, type_tokens TEXT,
    vcard_property TEXT, vcard_group TEXT, vcard_prop_id TEXT,
    vcard_pid TEXT, vcard_altid TEXT,
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ, active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_categories_current_value
    ON organization_categories(organization_id, normalized_value)
    WHERE active_until IS NULL AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS organization_media (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    media_kind TEXT NOT NULL, media_type TEXT, uri TEXT, data BYTEA,
    byte_size BIGINT, content_hash TEXT, original_value TEXT NOT NULL,
    pref INTEGER CHECK (pref IS NULL OR pref BETWEEN 1 AND 100),
    ordinal INTEGER NOT NULL DEFAULT 0,
    type_label TEXT, type_tokens TEXT,
    vcard_property TEXT, vcard_group TEXT, vcard_prop_id TEXT,
    vcard_pid TEXT, vcard_altid TEXT,
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    active_from TIMESTAMPTZ, active_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_organization_media_current
    ON organization_media(organization_id, media_kind, pref, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_media_property_identity
    ON organization_media(organization_id, source, source_ref, vcard_property, vcard_prop_id)
    WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL;

CREATE TABLE IF NOT EXISTS employments (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id        BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    organization_id  BIGINT NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    title            TEXT,
    title_normalized TEXT NOT NULL DEFAULT '',
    role             TEXT,
    department       TEXT,
    location         TEXT,
    address_id       BIGINT REFERENCES organization_addresses(id) ON DELETE SET NULL,
    description      TEXT,
    start_year       INTEGER,
    start_month      INTEGER,
    start_day        INTEGER,
    end_year         INTEGER,
    end_month        INTEGER,
    end_day          INTEGER,
    is_current       BOOLEAN NOT NULL DEFAULT TRUE,
    is_primary       BOOLEAN NOT NULL DEFAULT FALSE,
    source           TEXT NOT NULL DEFAULT 'user',
    source_ref       TEXT,
    confidence       DOUBLE PRECISION CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import'))),
    revision         BIGINT NOT NULL DEFAULT 1,
    created_by       TEXT NOT NULL DEFAULT 'user',
    updated_by       TEXT NOT NULL DEFAULT 'user',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (start_year BETWEEN 1 AND 9999),
    CHECK (start_month BETWEEN 1 AND 12),
    CHECK (start_day BETWEEN 1 AND 31),
    CHECK (end_year BETWEEN 1 AND 9999),
    CHECK (end_month BETWEEN 1 AND 12),
    CHECK (end_day BETWEEN 1 AND 31),
    CHECK (start_day IS NULL OR start_month IS NOT NULL),
    CHECK (end_day IS NULL OR end_month IS NOT NULL),
    CHECK (start_month IS NULL OR start_year IS NOT NULL),
    CHECK (end_month IS NULL OR end_year IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_employments_one_primary_current
    ON employments(person_id) WHERE is_primary AND is_current;
CREATE UNIQUE INDEX IF NOT EXISTS idx_employments_active_person_org_title
    ON employments(person_id, organization_id, title_normalized) WHERE is_current;
CREATE INDEX IF NOT EXISTS idx_employments_person ON employments(person_id);
CREATE INDEX IF NOT EXISTS idx_employments_organization ON employments(organization_id);
CREATE INDEX IF NOT EXISTS idx_employments_person_current ON employments(person_id) WHERE is_current;
CREATE INDEX IF NOT EXISTS idx_employments_address ON employments(address_id) WHERE address_id IS NOT NULL;

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
    -- Non-NULL only when an identity-match candidate inserted this exact edge.
    -- User-created links remain NULL; system-match rejection removes only an
    -- edge owned by that system decision.
    identity_match_candidate_id BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (participant_a, participant_b),
    CHECK (participant_a < participant_b)
);

CREATE INDEX IF NOT EXISTS idx_participant_links_b
    ON participant_links(participant_b);

-- ============================================================================
-- PERSON RELATIONSHIPS
-- ============================================================================

-- Relationship type metadata. A type is presentation and interchange
-- metadata, never a second copy of an edge: forward_label and reverse_label
-- let ONE person_relationships row render correctly from both endpoints, so
-- there is no mirrored row that can drift (the failure mode seen in personal
-- CRMs that store both directions).
--
-- Label contract. A person_relationships row asserts exactly one sentence:
--
--     <source person> is the <forward_label> of <target person>.
--
-- The inverse sentence is implied by the same row:
--
--     <target person> is the <reverse_label> of <source person>.
--
-- Worked example. Type 'parent' has forward_label 'parent' and reverse_label
-- 'child'. The row (source=alice, target=bob, type=parent) asserts "alice is
-- the parent of bob". Alice's relationship list shows "bob - child"; Bob's
-- shows "alice - parent". One row, two correct labels.
--
-- slug and universal_id are immutable machine identity; labels, colour, icon,
-- and description are mutable presentation. universal_id is an OPAQUE UUID,
-- hardcoded per seeded type so the same type has the same identity in every
-- install (which keeps exports and API clients portable) and minted randomly
-- for user-created types. It is deliberately not derived from the slug: a
-- derived identifier would make the slug load-bearing for identity, so the
-- slug would stop being independent and the two columns would collapse into
-- one immutable string wearing two names.
--
-- is_symmetric types (friend, spouse, sibling) need no orientation: writes
-- normalize the endpoints to (lower id, higher id) so the unordered pair has
-- one representation and the active-edge unique index rejects the mirror.
--
-- is_canonical/inverse_type_id exist because the IANA RELATED registry
-- contains one genuine inverse PAIR: 'parent' and 'child'. Both values must
-- map for lossless vCard interchange, but they describe the same edge from
-- opposite ends. 'child' is therefore marked non-canonical and points at
-- 'parent'; a write using 'child' is stored as 'parent' with the endpoints
-- swapped. Without this, (bob child-of alice) and (alice parent-of bob) would
-- both be storable and the duplicate-active-edge rule would not hold.
--
-- vcard_related_type is mutable interchange metadata and UNIQUE (NULLs are
-- distinct in both backends) so each registered RELATED TYPE value resolves
-- to exactly one type on import. Startup seed reconciliation preserves a
-- user's mapping choice instead of restoring the original seed value.
--
-- ownership is TEXT ('system' | 'user') rather than an is_system boolean, and
-- carries no CHECK: the roadmap leaves room for a third ownership kind such as
-- vendor or plugin, and widening a TEXT vocabulary needs no SQLite table
-- rebuild whereas a boolean would need a new column. It is validated in Go so
-- both backends reject the same values.
CREATE TABLE IF NOT EXISTS relationship_types (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    universal_id       TEXT NOT NULL UNIQUE,
    slug               TEXT NOT NULL UNIQUE,
    forward_label      TEXT NOT NULL,
    reverse_label      TEXT NOT NULL,
    is_symmetric       BOOLEAN NOT NULL DEFAULT FALSE,
    is_canonical       BOOLEAN NOT NULL DEFAULT TRUE,
    inverse_type_id    BIGINT REFERENCES relationship_types(id) ON DELETE SET NULL,
    vcard_related_type TEXT UNIQUE,
    color              TEXT,
    icon               TEXT,
    description        TEXT,
    ownership          TEXT NOT NULL DEFAULT 'user',
    is_deletable       BOOLEAN NOT NULL DEFAULT TRUE,
    revision           BIGINT NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (is_symmetric = FALSE OR forward_label = reverse_label),
    CHECK (is_canonical = TRUE OR inverse_type_id IS NOT NULL)
);

-- One canonical person-to-person relationship edge. The row asserts
-- "source_person_id is the type's forward_label of target_person_id"; the
-- reverse label renders the same row from the other endpoint. Non-canonical
-- inverse types are rewritten by the writer and symmetric types order their
-- endpoints, so there is never a second mirror row.
--
-- start_year/month/day and end_year/month/day store nullable partial-date
-- components. A bound's precision degrades year -> month -> day, and every
-- present relationship bound must include a year. The store validates calendar
-- dates and compares bounds at shared precision; the CHECKs make the portable
-- component shape and range true even for writers that bypass Go.
--
-- start_* / end_* are world time, while created_at / updated_at are transaction
-- time. Ending fills end_* and retains history; the partial unique index allows
-- a new row only after the earlier edge is no longer active.
CREATE TABLE IF NOT EXISTS person_relationships (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_person_id     BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    target_person_id     BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    relationship_type_id BIGINT NOT NULL REFERENCES relationship_types(id),
    start_year           INTEGER,
    start_month          INTEGER,
    start_day            INTEGER,
    end_year             INTEGER,
    end_month            INTEGER,
    end_day              INTEGER,
    status               TEXT NOT NULL DEFAULT 'active',
    notes                TEXT,
    source               TEXT NOT NULL DEFAULT 'user',
    source_ref           TEXT,
    source_resource_uid  TEXT,
    confidence           DOUBLE PRECISION,
    vcard_property       TEXT,
    vcard_group          TEXT,
    vcard_prop_id        TEXT,
    vcard_pid            TEXT,
    vcard_altid          TEXT,
    created_by           TEXT NOT NULL DEFAULT 'user',
    updated_by           TEXT NOT NULL DEFAULT 'user',
    revision             BIGINT NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (source_person_id <> target_person_id),
    CHECK (start_year BETWEEN 1 AND 9999),
    CHECK (start_month BETWEEN 1 AND 12),
    CHECK (start_day BETWEEN 1 AND 31),
    CHECK (end_year BETWEEN 1 AND 9999),
    CHECK (end_month BETWEEN 1 AND 12),
    CHECK (end_day BETWEEN 1 AND 31),
    CHECK (start_day IS NULL OR start_month IS NOT NULL),
    CHECK (end_day IS NULL OR end_month IS NOT NULL),
    CHECK (start_month IS NULL OR start_year IS NOT NULL),
    CHECK (end_month IS NULL OR end_year IS NOT NULL),
    CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1
           AND source NOT IN ('user', 'carddav_import', 'vcard_import')))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_person_relationships_active_unique
    ON person_relationships(source_person_id, target_person_id, relationship_type_id)
    WHERE end_year IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_relationships_source
    ON person_relationships(source_person_id);
CREATE INDEX IF NOT EXISTS idx_person_relationships_target
    ON person_relationships(target_person_id);
CREATE INDEX IF NOT EXISTS idx_person_relationships_target_active
    ON person_relationships(target_person_id, relationship_type_id)
    WHERE end_year IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_relationships_type
    ON person_relationships(relationship_type_id);

-- Decision ledger for imported vCard RELATED occurrences. Exact UID plus a
-- recognized type is the only automatic match and is recorded here as an
-- already-accepted row; every other imported assertion stays pending for a
-- human decision. Decisions are durable across re-import: rejections never
-- link, and an accepted row whose edge was deleted is not resurrected.
CREATE TABLE IF NOT EXISTS person_relationship_reviews (
    id                       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    person_id                BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    raw_related_value        TEXT NOT NULL,
    raw_related_type         TEXT NOT NULL DEFAULT '',
    value_kind               TEXT NOT NULL,
    matched_person_id        BIGINT REFERENCES persons(id) ON DELETE SET NULL,
    accepted_relationship_id BIGINT REFERENCES person_relationships(id) ON DELETE SET NULL,
    status                   TEXT NOT NULL DEFAULT 'pending',
    source                   TEXT NOT NULL,
    source_ref               TEXT,
    source_resource_uid      TEXT,
    vcard_property           TEXT,
    vcard_group              TEXT,
    vcard_prop_id            TEXT,
    vcard_pid                TEXT,
    vcard_altid              TEXT,
    created_by               TEXT NOT NULL DEFAULT 'system',
    reviewed_by              TEXT,
    reviewed_at              TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (matched_person_id IS NULL OR matched_person_id <> person_id)
);

-- One review per parsed property occurrence. COALESCE makes the nullable
-- provenance/property identity fields participate in uniqueness identically
-- on SQLite and PostgreSQL while preserving exact re-import idempotency.
CREATE UNIQUE INDEX IF NOT EXISTS idx_person_relationship_reviews_occurrence_unique
    ON person_relationship_reviews(
        person_id, raw_related_type, raw_related_value, source,
        COALESCE(source_ref, ''), COALESCE(vcard_property, ''),
        COALESCE(vcard_group, ''), COALESCE(vcard_prop_id, ''),
        COALESCE(vcard_pid, ''), COALESCE(vcard_altid, '')
    );
CREATE INDEX IF NOT EXISTS idx_person_relationship_reviews_status
    ON person_relationship_reviews(status, person_id, id);

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
    is_sensitive   BOOLEAN NOT NULL DEFAULT FALSE,
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

-- Durable operation history for reversible person merges. Historical person IDs
-- deliberately are not foreign keys: absorbed roots are deleted, while the
-- immutable IDs remain part of the audit record.
CREATE TABLE IF NOT EXISTS person_merges (
    id                          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    idempotency_key             TEXT NOT NULL UNIQUE,
    request_hash                TEXT NOT NULL,
    survivor_person_id_at_merge BIGINT NOT NULL,
    absorbed_person_id          BIGINT NOT NULL,
    current_person_id           BIGINT REFERENCES persons(id) ON DELETE SET NULL,
    survivor_uid                TEXT NOT NULL,
    absorbed_uid                TEXT NOT NULL,
    survivor_revision_before    BIGINT NOT NULL,
    absorbed_revision_before    BIGINT NOT NULL,
    survivor_revision_after     BIGINT NOT NULL,
    actor                       TEXT NOT NULL,
    snapshot_version            INTEGER NOT NULL,
    snapshot_blob               BYTEA NOT NULL,
    snapshot_sha256             TEXT NOT NULL,
    result_json                 TEXT,
    identity_revision           BIGINT CHECK(identity_revision IS NULL OR identity_revision > 0),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (survivor_person_id_at_merge <> absorbed_person_id),
    CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    CHECK (length(request_hash) = 64),
    CHECK (snapshot_version > 0),
    CHECK (length(snapshot_sha256) = 64)
);
CREATE INDEX IF NOT EXISTS idx_person_merges_current_person
    ON person_merges(current_person_id, id DESC);

-- Split headers retain historical source/new person IDs without foreign keys:
-- either resulting person can be absorbed by a later merge.
CREATE TABLE IF NOT EXISTS person_splits (
    id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merge_id               BIGINT NOT NULL REFERENCES person_merges(id) ON DELETE CASCADE,
    idempotency_key        TEXT NOT NULL UNIQUE,
    request_hash           TEXT NOT NULL,
    source_person_id       BIGINT NOT NULL,
    new_person_id          BIGINT NOT NULL,
    new_person_uid         TEXT NOT NULL,
    source_revision_before BIGINT NOT NULL,
    source_revision_after  BIGINT NOT NULL,
    actor                  TEXT NOT NULL,
    is_exact_reversal      BOOLEAN NOT NULL,
    result_json            TEXT,
    identity_revision      BIGINT CHECK(identity_revision IS NULL OR identity_revision > 0),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (source_person_id <> new_person_id),
    CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    CHECK (length(request_hash) = 64)
);
CREATE INDEX IF NOT EXISTS idx_person_splits_merge
    ON person_splits(merge_id, id);

CREATE TABLE IF NOT EXISTS person_merge_participants (
    merge_id       BIGINT NOT NULL REFERENCES person_merges(id) ON DELETE CASCADE,
    participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE RESTRICT,
    origin_side    TEXT NOT NULL CHECK(origin_side IN ('survivor', 'absorbed')),
    split_id       BIGINT REFERENCES person_splits(id) ON DELETE RESTRICT,
    PRIMARY KEY (merge_id, participant_id)
);
CREATE INDEX IF NOT EXISTS idx_person_merge_participants_split
    ON person_merge_participants(split_id)
    WHERE split_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS person_merge_rows (
    merge_id        BIGINT NOT NULL REFERENCES person_merges(id) ON DELETE CASCADE,
    table_name      TEXT NOT NULL,
    original_row_id BIGINT,
    original_row_key TEXT NOT NULL CHECK(original_row_key <> ''),
    current_row_id  BIGINT,
    current_row_key TEXT,
    origin_side     TEXT NOT NULL CHECK(origin_side IN ('survivor', 'absorbed')),
    provenance_kind TEXT NOT NULL CHECK(provenance_kind IN (
        'participant_exact', 'absorbed_profile', 'derived', 'inbound_reference'
    )),
    participant_id  BIGINT REFERENCES participants(id) ON DELETE RESTRICT,
    action          TEXT NOT NULL CHECK(action IN (
        'moved', 'repointed', 'deduplicated', 'deleted_snapshot', 'recomputed'
    )),
    snapshot_path   TEXT NOT NULL,
    post_merge_row_json TEXT,
    split_id        BIGINT REFERENCES person_splits(id) ON DELETE RESTRICT,
    UNIQUE (merge_id, table_name, original_row_key)
);
CREATE INDEX IF NOT EXISTS idx_person_merge_rows_split
    ON person_merge_rows(split_id)
    WHERE split_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_person_merge_rows_participant
    ON person_merge_rows(participant_id, merge_id)
    WHERE participant_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_person_merge_rows_current_id
    ON person_merge_rows(table_name, current_row_id)
    WHERE current_row_id IS NOT NULL AND split_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_person_merge_rows_current_key
    ON person_merge_rows(table_name, current_row_key)
    WHERE current_row_key IS NOT NULL AND split_id IS NULL;

CREATE TABLE IF NOT EXISTS person_merge_row_person_refs (
    merge_id        BIGINT NOT NULL,
    table_name      TEXT NOT NULL,
    original_row_key TEXT NOT NULL,
    column_name     TEXT NOT NULL,
    person_id       BIGINT NOT NULL,
    PRIMARY KEY (merge_id, table_name, original_row_key, column_name),
    FOREIGN KEY (merge_id, table_name, original_row_key)
        REFERENCES person_merge_rows(merge_id, table_name, original_row_key)
        ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_person_merge_row_person_refs_person
    ON person_merge_row_person_refs(person_id, merge_id);

CREATE TABLE IF NOT EXISTS person_merge_review_candidates (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merge_id            BIGINT NOT NULL REFERENCES person_merges(id) ON DELETE CASCADE,
    survivor_person_id  BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    definition_id       BIGINT NOT NULL REFERENCES attribute_definitions(id) ON DELETE RESTRICT,
    survivor_value_id   BIGINT NOT NULL REFERENCES person_attribute_values(id) ON DELETE RESTRICT,
    absorbed_value_id   BIGINT NOT NULL REFERENCES person_attribute_values(id) ON DELETE RESTRICT,
    state               TEXT NOT NULL DEFAULT 'pending'
                            CHECK(state IN ('pending', 'accepted', 'rejected')),
    resolution_value_id BIGINT REFERENCES person_attribute_values(id) ON DELETE RESTRICT,
    reviewed_by         TEXT,
    reviewed_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (merge_id, definition_id)
);
CREATE INDEX IF NOT EXISTS idx_person_merge_review_candidates_person
    ON person_merge_review_candidates(survivor_person_id, state, id);

CREATE TABLE IF NOT EXISTS organization_attribute_values (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
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

CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_attribute_values_current
    ON organization_attribute_values(organization_id, definition_id, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_organization_attribute_values_history
    ON organization_attribute_values(
        organization_id, definition_id, active_from DESC, id DESC
    );

CREATE INDEX IF NOT EXISTS idx_organization_attribute_values_definition
    ON organization_attribute_values(definition_id, active_until);

CREATE INDEX IF NOT EXISTS idx_organization_attribute_values_current_text
    ON organization_attribute_values(definition_id, value_text)
    WHERE value_text IS NOT NULL
      AND active_until IS NULL AND superseded_at IS NULL;

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
    source                TEXT NOT NULL,
    source_ref            TEXT,
    source_resource_uid   TEXT,
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
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
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
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
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
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
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
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
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
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
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
    source TEXT NOT NULL,
    source_ref TEXT,
    source_resource_uid TEXT,
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
    source TEXT NOT NULL,
    confidence DOUBLE PRECISION CHECK (confidence IS NULL OR (
        confidence >= 0 AND confidence <= 1
        AND source NOT IN ('user', 'carddav_import', 'vcard_import')
    )),
    source_ref TEXT,
    observation_conflict_origin TEXT CHECK (
        observation_conflict_origin IN ('generated', 'promoted')
    ),
    pre_conflict_state TEXT CHECK (
        pre_conflict_state IN ('candidate', 'accepted', 'rejected')
    ),
    application_pending BOOLEAN NOT NULL DEFAULT TRUE,
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

-- A participant merge can collapse duplicate candidates while an accepted
-- application is waiting for the identity lock. Record the exact survivor,
-- or a confirmed endpoint collapse, so that waiter can finish safely.
CREATE TABLE IF NOT EXISTS identity_match_candidate_redirects (
    retired_candidate_id BIGINT PRIMARY KEY,
    surviving_candidate_id BIGINT REFERENCES identity_match_candidates(id) ON DELETE CASCADE,
    endpoints_collapsed BOOLEAN NOT NULL DEFAULT FALSE,
    CHECK ((endpoints_collapsed = TRUE AND surviving_candidate_id IS NULL) OR
           (endpoints_collapsed = FALSE AND surviving_candidate_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_identity_match_candidate_redirects_survivor
    ON identity_match_candidate_redirects(surviving_candidate_id)
    WHERE surviving_candidate_id IS NOT NULL;

-- Generated identity candidates may be supported by observations from more
-- than one archive source. Keeping the support rows separate from the
-- candidate's display source lets source removal recompute stale suggestions
-- without deleting explicit user decisions.
CREATE TABLE IF NOT EXISTS identity_match_candidate_sources (
    candidate_id BIGINT NOT NULL REFERENCES identity_match_candidates(id) ON DELETE CASCADE,
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    is_conservative BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (candidate_id, source_id)
);
CREATE INDEX IF NOT EXISTS idx_identity_match_candidate_sources_source
    ON identity_match_candidate_sources(source_id, candidate_id);

CREATE TABLE IF NOT EXISTS identity_match_evidence (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    candidate_id BIGINT NOT NULL REFERENCES identity_match_candidates(id) ON DELETE CASCADE,
    evidence_kind TEXT NOT NULL,
    evidence_ref TEXT,
    detail TEXT,
    source TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_identity_match_evidence_candidate
    ON identity_match_evidence(candidate_id, id);

CREATE TABLE IF NOT EXISTS identity_match_evidence_sources (
    evidence_id BIGINT NOT NULL REFERENCES identity_match_evidence(id) ON DELETE CASCADE,
    source_id BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    is_conservative BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (evidence_id, source_id)
);
CREATE INDEX IF NOT EXISTS idx_identity_match_evidence_sources_source
    ON identity_match_evidence_sources(source_id, evidence_id);

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

-- ==========================================================================
-- DOCUMENT ATTACHMENT EXTRACTION
-- ==========================================================================

CREATE TABLE IF NOT EXISTS document_extraction_profiles (
    id                    TEXT PRIMARY KEY,
    fingerprint           TEXT NOT NULL UNIQUE,
    provider              TEXT NOT NULL,
    endpoint              TEXT NOT NULL,
    region                TEXT NOT NULL,
    model                 TEXT NOT NULL,
    retention_posture     TEXT NOT NULL,
    training_posture      TEXT NOT NULL,
    allowed_media_types   JSONB NOT NULL,
    policy_json           JSONB NOT NULL,
    enabled               BOOLEAN NOT NULL DEFAULT FALSE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    retired_at            TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS document_provider_consents (
    profile_id            TEXT PRIMARY KEY REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    profile_fingerprint   TEXT NOT NULL,
    retention_posture     TEXT NOT NULL,
    training_posture      TEXT NOT NULL,
    consented_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS document_extraction_rebuilds (
    id                    TEXT PRIMARY KEY,
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    extraction_input_key  TEXT NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('building', 'completed')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at          TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_document_extraction_rebuilds_active
    ON document_extraction_rebuilds(profile_id, extraction_input_key)
    WHERE state = 'building';

CREATE TABLE IF NOT EXISTS document_extraction_rebuild_targets (
    rebuild_id            TEXT NOT NULL REFERENCES document_extraction_rebuilds(id) ON DELETE CASCADE,
    canonical_blob_hash   TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    PRIMARY KEY (rebuild_id, canonical_blob_hash)
);

CREATE TABLE IF NOT EXISTS document_extractions (
    id                    TEXT PRIMARY KEY,
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    rebuild_id            TEXT REFERENCES document_extraction_rebuilds(id) ON DELETE SET NULL,
    canonical_blob_hash   TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    extraction_input_key  TEXT NOT NULL DEFAULT 'original',
    state                 TEXT NOT NULL CHECK (state IN ('staging', 'ready', 'terminal', 'tombstoned')),
    lease_owner           TEXT,
    lease_fence           BIGINT NOT NULL DEFAULT 0,
    lease_until           TIMESTAMPTZ,
    attempt_count         INTEGER NOT NULL DEFAULT 0,
    request_count         INTEGER NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    retry_count           INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0 AND retry_count <= request_count),
    provider_latency_ms   BIGINT NOT NULL DEFAULT 0 CHECK (provider_latency_ms >= 0),
    next_retry_at         TIMESTAMPTZ,
    local_bytes           BIGINT NOT NULL,
    provider_bytes        BIGINT,
    units_processed       INTEGER,
    returned_model        TEXT,
    manifest_checksum     TEXT,
    normalization_version INTEGER,
    document_family       TEXT,
    unit_kind             TEXT,
    normalized_truncated  BOOLEAN NOT NULL DEFAULT FALSE,
    terminal_reason       TEXT,
    source_sequence       BIGINT NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    published_at          TIMESTAMPTZ,
    CHECK (local_bytes >= 0),
    CHECK (provider_bytes IS NULL OR provider_bytes >= 0),
    CHECK (units_processed IS NULL OR units_processed >= 0)
);

CREATE INDEX IF NOT EXISTS idx_document_extractions_owner
    ON document_extractions(profile_id, canonical_blob_hash, extraction_input_key, state);
CREATE INDEX IF NOT EXISTS idx_document_extractions_lease
    ON document_extractions(state, lease_until);
CREATE UNIQUE INDEX IF NOT EXISTS idx_document_extractions_vector_identity
    ON document_extractions(id, profile_id, canonical_blob_hash, extraction_input_key, source_sequence);

CREATE TABLE IF NOT EXISTS document_extraction_claims (
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    canonical_blob_hash   TEXT NOT NULL,
    extraction_input_key  TEXT NOT NULL,
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    lease_owner           TEXT NOT NULL,
    lease_fence           BIGINT NOT NULL,
    lease_until           TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (profile_id, canonical_blob_hash, extraction_input_key)
);

CREATE TABLE IF NOT EXISTS document_extraction_heads (
    profile_id            TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE CASCADE,
    canonical_blob_hash   TEXT NOT NULL,
    extraction_input_key  TEXT NOT NULL,
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    source_sequence       BIGINT NOT NULL DEFAULT 0,
    switched_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (profile_id, canonical_blob_hash, extraction_input_key)
);

CREATE TABLE IF NOT EXISTS document_units (
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    unit_index            INTEGER NOT NULL,
    unit_kind             TEXT NOT NULL,
    text                  TEXT NOT NULL,
    header_text           TEXT,
    footer_text           TEXT,
    width                 INTEGER,
    height                INTEGER,
    dpi                    INTEGER,
    checksum              TEXT NOT NULL,
    char_count            INTEGER NOT NULL,
    truncated             BOOLEAN NOT NULL DEFAULT FALSE,
    heading_marks         JSONB NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (extraction_id, unit_index),
    CHECK (unit_index >= 0),
    CHECK (char_count >= 0)
);

CREATE TABLE IF NOT EXISTS document_chunks (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    extraction_id         TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
    chunk_key             TEXT NOT NULL,
    ordinal               INTEGER NOT NULL,
    text                  TEXT NOT NULL,
    heading_path          JSONB NOT NULL,
    first_unit_index      INTEGER NOT NULL,
    last_unit_index       INTEGER NOT NULL,
    synthetic_prefix_len  INTEGER NOT NULL DEFAULT 0,
    checksum              TEXT NOT NULL,
    char_count            INTEGER NOT NULL,
    table_chunk           BOOLEAN NOT NULL DEFAULT FALSE,
    code_chunk            BOOLEAN NOT NULL DEFAULT FALSE,
    truncated             BOOLEAN NOT NULL DEFAULT FALSE,
    search_fts            TSVECTOR GENERATED ALWAYS AS (to_tsvector('simple', COALESCE(text, ''))) STORED,
    UNIQUE (extraction_id, chunk_key),
    UNIQUE (extraction_id, ordinal),
    CHECK (ordinal >= 0),
    CHECK (first_unit_index >= 0 AND last_unit_index >= first_unit_index),
    CHECK (synthetic_prefix_len >= 0 AND char_count >= 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_document_chunks_vector_identity
    ON document_chunks(id, extraction_id, chunk_key, checksum);

CREATE INDEX IF NOT EXISTS idx_document_chunks_search_fts
    ON document_chunks USING GIN(search_fts);

CREATE TABLE IF NOT EXISTS document_chunk_spans (
    extraction_id         TEXT NOT NULL,
    chunk_key             TEXT NOT NULL,
    span_ordinal          INTEGER NOT NULL,
    unit_index            INTEGER NOT NULL,
    start_char            INTEGER NOT NULL,
    end_char              INTEGER NOT NULL,
    synthetic             BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (extraction_id, chunk_key, span_ordinal),
    FOREIGN KEY (extraction_id, chunk_key)
        REFERENCES document_chunks(extraction_id, chunk_key) ON DELETE CASCADE,
    FOREIGN KEY (extraction_id, unit_index)
        REFERENCES document_units(extraction_id, unit_index) ON DELETE CASCADE,
    CHECK (span_ordinal >= 0),
    CHECK (start_char >= 0 AND end_char >= start_char)
);

CREATE TABLE IF NOT EXISTS document_occurrences (
    occurrence_key        TEXT PRIMARY KEY,
    attachment_id         BIGINT NOT NULL UNIQUE REFERENCES attachments(id) ON DELETE CASCADE,
    message_id            BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    source_id             BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    source_part_key       TEXT,
    stable_source_part    BOOLEAN NOT NULL DEFAULT FALSE,
    canonical_blob_hash   TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    filename              TEXT,
    mime_type             TEXT,
    attachment_role       TEXT NOT NULL,
    role_source           TEXT NOT NULL,
    source_sequence       BIGINT NOT NULL DEFAULT 0,
    reconciled_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_document_occurrences_hash
    ON document_occurrences(canonical_blob_hash, message_id, occurrence_key);
CREATE INDEX IF NOT EXISTS idx_document_occurrences_source
    ON document_occurrences(source_id, message_id);

CREATE TABLE IF NOT EXISTS document_index_state (
    singleton             SMALLINT PRIMARY KEY CHECK (singleton = 1),
    revision              BIGINT NOT NULL DEFAULT 0,
    target_profile_id     TEXT,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO document_index_state(singleton, revision) VALUES (1, 0)
ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS document_vector_generations (
    id                           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    fingerprint                  TEXT NOT NULL,
    target_extraction_profile_id TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE RESTRICT,
    embedding_profile            TEXT NOT NULL,
    model                        TEXT NOT NULL,
    dimension                    INTEGER NOT NULL CHECK (dimension > 0),
    state                        TEXT NOT NULL CHECK (state IN ('building', 'active', 'retired')),
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    activated_at                 TIMESTAMPTZ,
    retired_at                   TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_document_vector_generations_building
    ON document_vector_generations(state) WHERE state = 'building';
CREATE UNIQUE INDEX IF NOT EXISTS idx_document_vector_generations_active
    ON document_vector_generations(state) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS idx_document_vector_generations_live_fingerprint
    ON document_vector_generations(fingerprint) WHERE state <> 'retired';

CREATE TABLE IF NOT EXISTS document_vector_consents (
    egress_fingerprint           TEXT PRIMARY KEY,
    purpose                      TEXT NOT NULL CHECK (purpose IN ('document_embedding', 'query_embedding')),
    generation_fingerprint       TEXT NOT NULL,
    target_extraction_profile_id  TEXT NOT NULL REFERENCES document_extraction_profiles(id) ON DELETE RESTRICT,
    embedding_profile            TEXT NOT NULL,
    model                        TEXT NOT NULL,
    dimension                    INTEGER NOT NULL CHECK (dimension > 0),
    consented_at                 TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS document_vector_provider_usage (
    fingerprint          TEXT PRIMARY KEY,
    provider_calls       BIGINT NOT NULL DEFAULT 0 CHECK (provider_calls >= 0),
    provider_documents   BIGINT NOT NULL DEFAULT 0 CHECK (provider_documents >= 0),
    provider_chunks      BIGINT NOT NULL DEFAULT 0 CHECK (provider_chunks >= 0),
    provider_input_chars BIGINT NOT NULL DEFAULT 0 CHECK (provider_input_chars >= 0),
    updated_at           TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS document_vector_build_progress (
    generation_id  BIGINT PRIMARY KEY REFERENCES document_vector_generations(id) ON DELETE CASCADE,
    after_chunk_id BIGINT NOT NULL CHECK (after_chunk_id > 0),
    updated_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS document_vector_publications (
    generation_id                BIGINT NOT NULL REFERENCES document_vector_generations(id) ON DELETE RESTRICT,
    extraction_id                TEXT NOT NULL,
    extraction_profile_id        TEXT NOT NULL,
    canonical_blob_hash          TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
    extraction_input_key         TEXT NOT NULL,
    chunk_id                     BIGINT NOT NULL,
    chunk_key                    TEXT NOT NULL,
    chunk_checksum               TEXT NOT NULL,
    source_sequence              BIGINT NOT NULL,
    token                        TEXT NOT NULL UNIQUE,
    state                        TEXT NOT NULL CHECK (state IN ('pending', 'ready', 'failed')),
    lease_owner                  TEXT,
    lease_fence                  BIGINT NOT NULL DEFAULT 0,
    lease_until                  TIMESTAMPTZ,
    attempt_count                INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_retry_at                TIMESTAMPTZ,
    error_code                   TEXT,
    backend_cleaned_at           TIMESTAMPTZ,
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (generation_id, extraction_id, chunk_id)
);
CREATE INDEX IF NOT EXISTS idx_document_vector_publications_cleanup
    ON document_vector_publications(generation_id, backend_cleaned_at, token);

-- Foreign-key cascades can remove occurrences before asynchronous attachment
-- reconciliation observes the deletion. Invalidate search cursors at the
-- authoritative row mutation so every deletion path is covered.
CREATE OR REPLACE FUNCTION bump_document_index_revision_after_occurrence_delete()
RETURNS TRIGGER AS $$
BEGIN
    UPDATE document_index_state
    SET revision = revision + 1, updated_at = CURRENT_TIMESTAMP
    WHERE singleton = 1;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_document_occurrence_delete_revision ON document_occurrences;
CREATE TRIGGER trg_document_occurrence_delete_revision
AFTER DELETE ON document_occurrences
FOR EACH ROW EXECUTE FUNCTION bump_document_index_revision_after_occurrence_delete();

-- Message type is a document-search filter. Invalidate cursors when an indexed
-- occurrence moves between filter scopes, regardless of which importer made
-- the authoritative message update.
CREATE OR REPLACE FUNCTION bump_document_index_revision_after_message_type_update()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.message_type IS DISTINCT FROM NEW.message_type
       AND EXISTS (
           SELECT 1
           FROM document_occurrences o
           JOIN document_extraction_heads h
             ON h.canonical_blob_hash = o.canonical_blob_hash
           WHERE o.message_id = NEW.id
       ) THEN
        UPDATE document_index_state
        SET revision = revision + 1, updated_at = CURRENT_TIMESTAMP
        WHERE singleton = 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_document_message_type_revision ON messages;
CREATE TRIGGER trg_document_message_type_revision
AFTER UPDATE OF message_type ON messages
FOR EACH ROW EXECUTE FUNCTION bump_document_index_revision_after_message_type_update();
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

-- ============================================================================
-- DATED ACTIVITY SPINE
-- ============================================================================

CREATE TABLE IF NOT EXISTS activity_events (
    message_id                         BIGINT PRIMARY KEY
                                                  REFERENCES messages(id) ON DELETE CASCADE,
    ref_kind                           TEXT NOT NULL
                                                  CHECK (ref_kind IN ('message', 'meeting')),
    source_id                          BIGINT NOT NULL
                                                  REFERENCES sources(id) ON DELETE CASCADE,
    conversation_id                    BIGINT
                                                  REFERENCES conversations(id) ON DELETE SET NULL,
    channel                            TEXT NOT NULL
                                                  CHECK (channel IN ('email', 'chat', 'meeting', 'other')),
    occurred_at                        TIMESTAMPTZ NOT NULL,
    date_origin                        TEXT NOT NULL
                                                  CHECK (date_origin IN ('sent_at', 'received_at', 'internal_date')),
    date_precision                     TEXT NOT NULL
                                                  CHECK (date_precision IN ('timestamp', 'day')),
    timezone                           TEXT NOT NULL CHECK (LENGTH(timezone) > 0),
    utc_offset_minutes                 INTEGER NOT NULL
                                                  CHECK (utc_offset_minutes BETWEEN -840 AND 840),
    local_date                         TEXT NOT NULL
                                                  CHECK (LENGTH(local_date) = 10
                                                      AND SUBSTR(local_date, 1, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                      AND SUBSTR(local_date, 2, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                      AND SUBSTR(local_date, 3, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                      AND SUBSTR(local_date, 4, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                      AND SUBSTR(local_date, 5, 1) = '-'
                                                      AND SUBSTR(local_date, 6, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                      AND SUBSTR(local_date, 7, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                      AND SUBSTR(local_date, 8, 1) = '-'
                                                      AND SUBSTR(local_date, 9, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                      AND SUBSTR(local_date, 10, 1) IN ('0','1','2','3','4','5','6','7','8','9')),
    direction                          TEXT NOT NULL
                                                  CHECK (direction IN ('inbound', 'outbound', 'observed')),
    owner_source_id                    BIGINT
                                                  REFERENCES sources(id) ON DELETE SET NULL,
    owner_address                      TEXT NOT NULL DEFAULT '',
    projected_last_modified            TIMESTAMPTZ NOT NULL,
    projected_identity_revision        BIGINT NOT NULL
                                                  CHECK (projected_identity_revision >= 0),
    projected_account_identity_revision BIGINT NOT NULL
                                                  CHECK (projected_account_identity_revision >= 0),
    created_at                          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_activity_events_local_date
    ON activity_events(local_date, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_activity_events_source
    ON activity_events(source_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS activity_event_persons (
    message_id BIGINT NOT NULL REFERENCES activity_events(message_id) ON DELETE CASCADE,
    person_id  BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    role       TEXT NOT NULL
                    CHECK (role IN ('sender', 'addressed', 'organizer', 'attendee', 'member')),
    evidence   TEXT NOT NULL CHECK (evidence IN ('direct', 'co_presence')),
    local_date TEXT NOT NULL CHECK (
        LENGTH(local_date) = 10
        AND SUBSTR(local_date, 1, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 2, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 3, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 4, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 5, 1) = '-'
        AND SUBSTR(local_date, 6, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 7, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 8, 1) = '-'
        AND SUBSTR(local_date, 9, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 10, 1) IN ('0','1','2','3','4','5','6','7','8','9')
    ),
    PRIMARY KEY (message_id, person_id)
);

CREATE INDEX IF NOT EXISTS idx_activity_event_persons_person_date
    ON activity_event_persons(person_id, local_date, message_id);
CREATE INDEX IF NOT EXISTS idx_activity_event_persons_date_person
    ON activity_event_persons(local_date, person_id, message_id);

CREATE TABLE IF NOT EXISTS person_contact_state (
    person_id                 BIGINT PRIMARY KEY REFERENCES persons(id) ON DELETE CASCADE,
    first_contact_at          TIMESTAMPTZ,
    first_contact_message_id  BIGINT,
    last_contact_at           TIMESTAMPTZ,
    last_contact_message_id   BIGINT,
    last_contact_channel      TEXT
                                    CHECK (last_contact_channel IS NULL
                                        OR last_contact_channel IN ('email', 'chat', 'meeting', 'other')),
    last_contact_source_id    BIGINT,
    last_contact_owner        TEXT,
    last_inbound_at           TIMESTAMPTZ,
    last_inbound_message_id   BIGINT,
    last_outbound_at          TIMESTAMPTZ,
    last_outbound_message_id  BIGINT,
    interaction_count         BIGINT NOT NULL DEFAULT 0 CHECK (interaction_count >= 0),
    identity_revision         BIGINT NOT NULL DEFAULT 0 CHECK (identity_revision >= 0),
    account_identity_revision BIGINT NOT NULL DEFAULT 0
                                    CHECK (account_identity_revision >= 0),
    dirty_at                  TIMESTAMPTZ,
    computed_at               TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_person_contact_state_dirty
    ON person_contact_state(person_id) WHERE dirty_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_person_contact_state_last_contact
    ON person_contact_state(last_contact_at DESC);

-- ============================================================================
-- AUTHORED DAILY NOTES
-- ============================================================================

CREATE TABLE IF NOT EXISTS daily_note_day_sequences (
    local_date TEXT PRIMARY KEY CHECK (
        LENGTH(local_date) = 10
        AND SUBSTR(local_date, 1, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 2, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 3, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 4, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 5, 1) = '-'
        AND SUBSTR(local_date, 6, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 7, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 8, 1) = '-'
        AND SUBSTR(local_date, 9, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 10, 1) IN ('0','1','2','3','4','5','6','7','8','9')
    ),
    last_ordinal BIGINT NOT NULL CHECK (last_ordinal > 0)
);

CREATE TABLE IF NOT EXISTS daily_note_entries (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    local_date TEXT NOT NULL CHECK (
        LENGTH(local_date) = 10
        AND SUBSTR(local_date, 1, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 2, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 3, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 4, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 5, 1) = '-'
        AND SUBSTR(local_date, 6, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 7, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 8, 1) = '-'
        AND SUBSTR(local_date, 9, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 10, 1) IN ('0','1','2','3','4','5','6','7','8','9')
    ),
    ordinal    BIGINT NOT NULL CHECK (ordinal > 0),
    body       TEXT NOT NULL,
    author     TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT 'user' CHECK (source = 'user'),
    source_ref TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(local_date, ordinal)
);

CREATE INDEX IF NOT EXISTS idx_daily_note_entries_date
    ON daily_note_entries(local_date, ordinal, id);

CREATE TABLE IF NOT EXISTS daily_note_entry_persons (
    entry_id  BIGINT NOT NULL REFERENCES daily_note_entries(id) ON DELETE CASCADE,
    person_id BIGINT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    PRIMARY KEY (entry_id, person_id)
);

CREATE INDEX IF NOT EXISTS idx_daily_note_entry_persons_person
    ON daily_note_entry_persons(person_id, entry_id);

CREATE TABLE IF NOT EXISTS activity_projection_queue (
    message_id         BIGINT PRIMARY KEY REFERENCES messages(id)
                       ON DELETE CASCADE ON UPDATE CASCADE,
    revision           BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    processed_revision BIGINT NOT NULL DEFAULT 0
                              CHECK (processed_revision >= 0
                                 AND processed_revision <= revision),
    queued_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_activity_projection_queue_pending
    ON activity_projection_queue(message_id)
    WHERE revision > processed_revision;


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
