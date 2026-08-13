package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func subsetPersonDefinition(slug string) AttributeDefinitionInput {
	return AttributeDefinitionInput{
		UniversalID: "test-" + slug,
		ObjectType:  AttributeObjectPerson,
		Slug:        slug,
		Label:       "Test " + slug,
		ValueType:   AttributeValueText,
		FieldType:   AttributeFieldText,
		Cardinality: AttributeCardinalitySingle,
		Ownership:   AttributeOwnershipUser,
		UICreatable: true,
		UIEditable:  true,
		APIMutable:  true,
		IsAudited:   true,
		IsDeletable: true,
	}
}

// createTestSourceDB creates a source database with schema and test
// data. Returns the path to the database.
func createTestSourceDB(t *testing.T, dir string, msgCount int) string {
	t.Helper()
	require := require.New(t)

	dbPath := filepath.Join(dir, "msgvault.db")

	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err, "open db")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'test@example.com')`)
	require.NoError(err, "insert source")

	_, err = db.Exec(`
		INSERT INTO participants
			(id, email_address, display_name, domain)
		VALUES
			(1, 'alice@example.com', 'Alice', 'example.com'),
			(2, 'bob@example.com', 'Bob', 'example.com'),
			(3, 'charlie@example.com', 'Charlie', 'example.com')`)
	require.NoError(err, "insert participants")

	_, err = db.Exec(`
		INSERT INTO participant_identifiers
			(id, participant_id, identifier_type, identifier_value)
		VALUES
			(1, 1, 'email', 'alice@example.com'),
			(2, 2, 'email', 'bob@example.com'),
			(3, 3, 'email', 'charlie@example.com')`)
	require.NoError(err, "insert participant_identifiers")

	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES
			(1, 1, 'email_thread', 'Thread 1', 5, 2),
			(2, 1, 'email_thread', 'Thread 2', 5, 2)`)
	require.NoError(err, "insert conversations")

	_, err = db.Exec(`
		INSERT INTO conversation_participants
			(conversation_id, participant_id)
		VALUES (1, 1), (1, 2), (2, 2), (2, 3)`)
	require.NoError(err, "insert conversation_participants")

	_, err = db.Exec(`
		INSERT INTO labels (id, source_id, name, label_type)
		VALUES
			(1, 1, 'INBOX', 'system'),
			(2, 1, 'SENT', 'system'),
			(3, 1, 'Work', 'user')`)
	require.NoError(err, "insert labels")

	for i := 1; i <= msgCount; i++ {
		convID := 1
		senderID := 1
		if i > msgCount/2 {
			convID = 2
			senderID = 2
		}

		_, err = db.Exec(`
			INSERT INTO messages
				(id, conversation_id, source_id, source_message_id,
				 message_type, sent_at, sender_id, subject)
			VALUES (?, ?, 1, ?,
				'email',
				datetime('2024-01-01', '+' || ? || ' hours'),
				?, ?)`,
			i, convID, fmt.Sprintf("msg_%d", i),
			i, senderID, "Subject "+string(rune('A'+i%26)))
		require.NoError(err, "insert message %d", i)

		_, err = db.Exec(
			`INSERT INTO message_bodies (message_id, body_text)
			 VALUES (?, ?)`,
			i, "Body of message "+string(rune('A'+i%26)))
		require.NoError(err, "insert message_body %d", i)

		_, err = db.Exec(
			`INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			 VALUES (?, ?, 'from')`,
			i, senderID)
		require.NoError(err, "insert message_recipient from %d", i)

		toID := 2
		if senderID == 2 {
			toID = 3
		}
		_, err = db.Exec(
			`INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			 VALUES (?, ?, 'to')`,
			i, toID)
		require.NoError(err, "insert message_recipient to %d", i)

		labelID := (i % 3) + 1
		_, err = db.Exec(
			`INSERT INTO message_labels (message_id, label_id)
			 VALUES (?, ?)`,
			i, labelID)
		require.NoError(err, "insert message_label %d", i)
	}

	return dbPath
}

func TestCopySubset_Basic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 10)

	result, err := CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err, "CopySubset")

	assert.Equal(int64(5), result.Messages, "Messages")

	db, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = db.Close() }()

	var count int64

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM messages",
	).Scan(&count), "count messages")
	assert.Equal(int64(5), count, "destination messages")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM participants",
	).Scan(&count), "count participants")
	assert.NotZero(count, "expected participants to be copied")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM conversations",
	).Scan(&count), "count conversations")
	assert.NotZero(count, "expected conversations to be copied")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM labels",
	).Scan(&count), "count labels")
	assert.NotZero(count, "expected labels to be copied")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM message_labels",
	).Scan(&count), "count message_labels")
	assert.NotZero(count, "expected message_labels to be copied")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM message_bodies",
	).Scan(&count), "count message_bodies")
	assert.Equal(int64(5), count, "destination message_bodies")

	fkRows, err := db.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "foreign key violations found in destination database")
}

func TestCopySubsetExcludesDocumentDerivativesAndHostedConsent(t *testing.T) {
	require := require.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")
	srcDB := createTestSourceDB(t, srcDir, 1)
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=ON")
	require.NoError(err)
	fingerprint := strings.Repeat("a", 64)
	_, err = db.Exec(`
		INSERT INTO document_extraction_profiles
			(id, fingerprint, provider, endpoint, region, model,
			 retention_posture, training_posture, allowed_media_types, policy_json, enabled)
		VALUES ('profile-subset', ?, 'mistral', 'https://api.mistral.ai/v1/ocr', 'eu',
		        'mistral-ocr-4-0', 'standard', 'opted-out', '["application/pdf"]', '{}', TRUE);
		INSERT INTO document_provider_consents
			(profile_id, profile_fingerprint, retention_posture, training_posture)
		VALUES ('profile-subset', ?, 'standard', 'opted-out');
		INSERT INTO document_extractions
			(id, profile_id, canonical_blob_hash, state, local_bytes,
			 returned_model, manifest_checksum, units_processed)
		VALUES ('subset-extraction', 'profile-subset', ?, 'ready', 10,
		        'mistral-ocr-4-0', ?, 1);
		INSERT INTO document_units
			(extraction_id, unit_index, unit_kind, text, checksum, char_count)
		VALUES ('subset-extraction', 0, 'page', 'private extracted evidence', ?, 26)`,
		fingerprint, fingerprint, strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64),
	)
	require.NoError(err)
	require.NoError(db.Close())

	_, err = CopySubset(srcDB, dstDir, 1, false)
	require.NoError(err)
	destination, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = destination.Close() }()
	for _, table := range []string{
		"document_extraction_profiles", "document_provider_consents", "document_extractions",
		"document_extraction_rebuilds", "document_extraction_rebuild_targets",
		"document_extraction_heads", "document_units", "document_chunks", "document_chunk_spans",
		"document_occurrences", "document_extraction_claims",
	} {
		var count int
		require.NoError(destination.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count), table)
		assert.Zero(t, count, table+" must require a target-side rebuild")
	}
}

func TestCopySubset_UpgradedMessageColumnOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")
	srcDB := createTestSourceDB(t, srcDir, 1)

	st, err := Open(srcDB)
	require.NoError(err, "open source for upgrade")
	_, err = st.DB().Exec(`
		ALTER TABLE messages DROP COLUMN identity_is_from_me;
		ALTER TABLE messages DROP COLUMN source_is_from_me;
		DELETE FROM applied_migrations
		WHERE name = 'message_attribution_provenance_v2';
	`)
	require.NoError(err, "simulate pre-attribution schema")
	require.NoError(st.InitSchema(), "upgrade source schema")
	_, err = st.DB().Exec(`
		UPDATE messages
		SET is_from_me = TRUE,
		    source_is_from_me = FALSE,
		    identity_is_from_me = TRUE,
		    metadata = '{"schema":"upgraded"}',
		    embed_gen = 7
		WHERE id = 1
	`)
	require.NoError(err, "seed upgraded message columns")
	require.NoError(st.Close(), "close upgraded source")

	result, err := CopySubset(srcDB, dstDir, 1, false)
	require.NoError(err, "CopySubset from upgraded schema")
	assert.Equal(int64(1), result.Messages)

	db, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open copied database")
	defer func() { _ = db.Close() }()

	var sourceMessageID, messageType, subject, metadata string
	var isFromMe, sourceIsFromMe, identityIsFromMe bool
	var embedGen int64
	require.NoError(db.QueryRow(`
		SELECT source_message_id, message_type, subject,
		       is_from_me, source_is_from_me, identity_is_from_me,
		       metadata, embed_gen
		FROM messages
		WHERE id = 1
	`).Scan(
		&sourceMessageID,
		&messageType,
		&subject,
		&isFromMe,
		&sourceIsFromMe,
		&identityIsFromMe,
		&metadata,
		&embedGen,
	))
	assert.Equal("msg_1", sourceMessageID)
	assert.Equal("email", messageType)
	assert.Equal("Subject B", subject)
	assert.True(isFromMe)
	assert.False(sourceIsFromMe)
	assert.True(identityIsFromMe)
	assert.JSONEq(`{"schema":"upgraded"}`, metadata)
	assert.Equal(int64(7), embedGen)
}

func TestCopySubset_AllRows(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	result, err := CopySubset(srcDB, dstDir, 100, false)
	require.NoError(t, err, "CopySubset")

	assert.Equal(t, int64(5), result.Messages, "Messages (all available)")
}

func TestCopySubset_PreservesPersonProfiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	displayName := "alice"
	person, err = source.UpdatePersonDisplayName(person.ID, person.Revision, &displayName)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 1, false)
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	copied, err := destination.GetPerson(person.ID)
	require.NoError(err)
	assert.Equal(person.ID, copied.ID)
	assert.Equal(person.VCardUID, copied.VCardUID)
	assert.Equal(person.DisplayName, copied.DisplayName)
	assert.Equal(person.Revision, copied.Revision)
	assert.Equal(person.ParticipantIDs, copied.ParticipantIDs)
}

func TestCopySubset_ExcludesStructuredProfilesByDefault(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, err = source.AddPersonNameContext(ctx, person.ID, PersonNameInput{
		NameKind:  PersonNameFormatted,
		Formatted: new("Private Profile Name"),
		Envelope:  ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	history, err := destination.GetPersonProfileHistoryContext(ctx, person.ID)
	require.NoError(err)
	assert.Empty(history.Names,
		"a shared subset must not copy structured profile values without an explicit opt-in")
}

func TestCopySubset_LegacyParticipantIdentifiersCopyByColumnName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 1)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		DROP INDEX IF EXISTS idx_participant_identifiers_service_scope;
		ALTER TABLE participant_identifiers RENAME TO participant_identifiers_current;
		CREATE TABLE participant_identifiers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
			identifier_type TEXT NOT NULL,
			identifier_value TEXT NOT NULL,
			display_value TEXT,
			is_primary BOOLEAN NOT NULL DEFAULT FALSE,
			UNIQUE(identifier_type, identifier_value)
		);
		INSERT INTO participant_identifiers (
			id, participant_id, identifier_type, identifier_value,
			display_value, is_primary
		)
		SELECT id, participant_id, identifier_type, identifier_value,
			display_value, is_primary
		FROM participant_identifiers_current;
		DROP TABLE participant_identifiers_current;
	`)
	require.NoError(err, "rebuild legacy participant_identifiers")
	require.NoError(db.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 1, false)
	require.NoError(err, "copy legacy participant identifiers")
	destination, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	var participantID int64
	var serviceID sql.NullInt64
	require.NoError(destination.QueryRow(`SELECT participant_id, service_id
		FROM participant_identifiers
		WHERE identifier_type = 'email' AND identifier_value = 'bob@example.com'`).
		Scan(&participantID, &serviceID))
	assert.Equal(int64(2), participantID)
	assert.False(serviceID.Valid, "missing legacy service metadata must use the destination default")
}

func TestCopySubsetRemapsParticipantIdentifierServicesWithoutProfiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 1)
	source, err := Open(srcDB)
	require.NoError(err)

	var sourceServiceID int64
	require.NoError(source.db.QueryRow(
		`SELECT id FROM communication_services WHERE slug = 'whatsapp'`,
	).Scan(&sourceServiceID))
	_, err = source.db.Exec(`UPDATE communication_services
		SET slug = 'subset-custom-chat', display_label = 'Subset Custom Chat',
		    is_system = FALSE
		WHERE id = ?`, sourceServiceID)
	require.NoError(err)
	_, err = source.db.Exec(`UPDATE participant_identifiers
		SET service_id = ?, scope_kind = 'account', scope_value = 'synthetic-account'
		WHERE participant_id = 2`, sourceServiceID)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 1, false)
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	copiedService, err := destination.ResolveCommunicationServiceContext(
		ctx, "subset-custom-chat",
	)
	require.NoError(err)
	var identifierServiceID int64
	require.NoError(destination.db.QueryRow(`SELECT service_id
		FROM participant_identifiers WHERE participant_id = 2`).Scan(&identifierServiceID))
	assert.Equal(copiedService.ID, identifierServiceID)
	assert.NotEqual(sourceServiceID, identifierServiceID,
		"the destination service ID must be resolved from its immutable slug")
}

func TestCopySubsetPreservesStructuredProfileHistoryAndDependencies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, _, err = source.EnsureCommunicationServiceContext(ctx, CommunicationServiceInput{
		Slug: "source-only-offset", DisplayLabel: "Source Only Offset",
		ScopePolicy: ScopePolicyNone, Normalization: NormalizationNone,
		NormalizationVersion: 1,
	})
	require.NoError(err)
	service, _, err := source.EnsureCommunicationServiceContext(ctx, CommunicationServiceInput{
		Slug: "example-chat", DisplayLabel: "Example Chat", Aliases: []string{"example-im"},
		ScopePolicy: ScopePolicyNone, Normalization: NormalizationLower,
		NormalizationVersion: 1,
	})
	require.NoError(err)
	profileSource, err := source.GetOrCreateSource("profile-fixture", "profile-only")
	require.NoError(err)
	_, err = source.DB().Exec(`INSERT INTO labels (
		id, source_id, source_label_id, name, label_type
	) VALUES (?, ?, ?, ?, ?)`,
		9001, profileSource.ID, "profile-private", "Profile Private", "user",
	)
	require.NoError(err)

	oldName, err := source.AddPersonNameContext(ctx, person.ID, PersonNameInput{
		NameKind: PersonNameFormatted, Formatted: new("Robert Example"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceVCardImport},
	})
	require.NoError(err)
	require.NoError(source.SupersedePersonNameContext(ctx, person.ID, oldName.Envelope.ID, nil))
	_, err = source.AddPersonNameContext(ctx, person.ID, PersonNameInput{
		NameKind: PersonNameFormatted, Formatted: new("Bob Example"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	_, err = source.AddPersonContactPointContext(ctx, person.ID, PersonContactPointInput{
		AddressKind: ContactAddressUsername, ServiceSlug: &service.Slug,
		OriginalValue: "BobExample", Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	_, err = source.AddPersonAddressContext(ctx, person.ID, PersonAddressInput{
		AddressKind: PersonAddressPostal, StreetAddress: new("123 Example St"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	_, err = source.AddPersonDateContext(ctx, person.ID, PersonDateInput{
		DateKind: PersonDateBirthday, Date: PartialDate{Year: new(1985), Month: new(4), Day: new(12)},
		Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	_, err = source.AddPersonCategoryContext(ctx, person.ID, PersonCategoryInput{
		OriginalValue: "Friends", Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	_, err = source.AddPersonMediaContext(ctx, person.ID, PersonMediaInput{
		MediaKind: PersonMediaPhoto, URI: new("https://example.invalid/photo.jpg"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	firstObservation, err := source.RecordContactObservationContext(ctx, 2, ParticipantContactObservationInput{
		SourceID: &profileSource.ID, AddressKind: ContactAddressUsername,
		ServiceSlug: &service.Slug, ProviderUserID: new("provider-bob"),
		OriginalValue: "BobExample",
		Envelope:      ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
	})
	require.NoError(err)
	require.False(firstObservation.Conflicting)
	secondObservation, err := source.RecordContactObservationContext(
		ctx, 3, ParticipantContactObservationInput{
			SourceID: &profileSource.ID, AddressKind: ContactAddressUsername,
			ServiceSlug: &service.Slug, ProviderUserID: new("provider-charlie"),
			OriginalValue: "BobExample",
			Envelope:      ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
		},
	)
	require.NoError(err)
	require.True(secondObservation.Conflicting)
	require.NotNil(secondObservation.CandidateID)
	_, err = source.AddIdentityMatchEvidenceContext(
		ctx, *secondObservation.CandidateID, IdentityMatchEvidenceInput{
			EvidenceKind: "shared_username", EvidenceRef: new("fixture-evidence"),
			Detail: new("reviewed source observation"), Source: ProvenanceSystem,
		},
	)
	require.NoError(err)
	decisionNote := "keep identities separate"
	decidedCandidate, err := source.DecideIdentityMatchCandidateContext(
		ctx, *secondObservation.CandidateID, IdentityMatchStateRejected,
		"user", &decisionNote,
	)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 1, CopySubsetOptions{
		IncludeProfiles: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	history, err := destination.GetPersonProfileHistoryContext(ctx, person.ID)
	require.NoError(err)
	assert.Len(history.Names, 2)
	assert.Len(history.ContactPoints, 1)
	assert.Len(history.Addresses, 1)
	assert.Len(history.Dates, 1)
	assert.Len(history.Categories, 1)
	assert.Len(history.Media, 1)
	assert.Len(history.Observations, 1)
	copiedService, err := destination.ResolveCommunicationServiceContext(ctx, "example-im")
	require.NoError(err)
	assert.Equal("example-chat", copiedService.Slug)
	assert.Equal("Example Chat", copiedService.DisplayLabel)
	assert.NotEqual(service.ID, copiedService.ID,
		"candidate service IDs must be remapped through the immutable slug")
	copiedProfileSource, err := destination.GetSourceByID(profileSource.ID)
	require.NoError(err)
	assert.Equal("profile-only", copiedProfileSource.Identifier)
	candidates, err := destination.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	copiedCandidate := candidates[0]
	assert.Equal(decidedCandidate.ID, copiedCandidate.ID)
	assert.Equal(IdentityMatchStateRejected, copiedCandidate.State)
	assert.Equal(decidedCandidate.DecidedBy, copiedCandidate.DecidedBy)
	assert.Equal(decidedCandidate.DecidedAt, copiedCandidate.DecidedAt)
	assert.Equal(decidedCandidate.Notes, copiedCandidate.Notes)
	require.NotNil(copiedCandidate.ServiceSlug)
	assert.Equal("example-chat", *copiedCandidate.ServiceSlug)
	require.Len(copiedCandidate.Evidence, 1)
	assert.Equal("shared_username", copiedCandidate.Evidence[0].EvidenceKind)
	require.NotNil(copiedCandidate.Evidence[0].EvidenceRef)
	assert.Equal("fixture-evidence", *copiedCandidate.Evidence[0].EvidenceRef)
	var leakedProfileLabels int
	require.NoError(destination.DB().QueryRow(
		`SELECT COUNT(*) FROM labels WHERE source_id = ?`, profileSource.ID,
	).Scan(&leakedProfileLabels))
	assert.Zero(leakedProfileLabels,
		"profile-only provenance must not broaden message label selection")
}

func TestCopySubset_AttributesRequireExplicitOptIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)

	input := subsetPersonDefinition("synthetic_preference")
	input.UniversalID = "test-synthetic-preference"
	input.FieldType = AttributeFieldSelect
	input.Options = &AttributeOptions{Choices: []AttributeChoice{
		{Value: "alpha", Label: "Alpha"},
		{Value: "beta", Label: "Beta"},
	}}
	definition, err := source.CreateAttributeDefinitionContext(ctx, input)
	require.NoError(err)
	_, err = source.db.Exec(
		`UPDATE attribute_definitions SET id = 42 WHERE id = ?`, definition.ID)
	require.NoError(err)

	firstAt := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	sourceRef := "fixture:synthetic-preference"
	actor := "synthetic-agent"
	confidence := 0.75
	first, err := source.SetPersonAttributeValueContext(ctx, PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: input.Slug,
		Value:      AttributeValue{Type: AttributeValueText, Text: new("alpha")},
		ActiveFrom: &firstAt, Source: ProvenanceExtraction,
		SourceRef: &sourceRef, Confidence: &confidence, Actor: &actor,
	})
	require.NoError(err)
	secondAt := firstAt.Add(24 * time.Hour)
	_, err = source.SetPersonAttributeValueContext(ctx, PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: input.Slug,
		Value:      AttributeValue{Type: AttributeValueText, Text: new("beta")},
		ActiveFrom: &secondAt, Source: ProvenanceUser,
		ExpectedValueID: &first.Value.ID,
	})
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	_, err = destination.GetAttributeDefinitionBySlugContext(
		ctx, AttributeObjectPerson, input.Slug)
	require.ErrorIs(err, ErrAttributeDefinitionNotFound,
		"shared subsets must not copy person attribute definitions by default")

	history, err := destination.ListPersonAttributeValuesContext(
		ctx, person.ID, PersonAttributeQuery{
			DefinitionSlug: input.Slug,
			IncludeHistory: true,
		})
	require.NoError(err)
	assert.Empty(history,
		"shared subsets must not copy current or historical person attribute values by default")

	attributesDir := filepath.Join(t.TempDir(), "attributes")
	_, err = CopySubsetWithOptions(srcDB, attributesDir, 5, CopySubsetOptions{
		IncludeAttributes: true,
	})
	require.NoError(err)
	withAttributes, err := Open(filepath.Join(attributesDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = withAttributes.Close() })

	copiedDefinition, err := withAttributes.GetAttributeDefinitionBySlugContext(
		ctx, AttributeObjectPerson, input.Slug)
	require.NoError(err)
	assert.Equal(input.UniversalID, copiedDefinition.UniversalID)
	assert.NotEqual(int64(42), copiedDefinition.ID,
		"destination definition ID must be local rather than copied from the source")
	assert.Equal(input.Slug, copiedDefinition.Slug)
	require.NotNil(copiedDefinition.Options)
	assert.Equal(input.Options.Choices, copiedDefinition.Options.Choices)

	history, err = withAttributes.ListPersonAttributeValuesContext(
		ctx, person.ID, PersonAttributeQuery{
			DefinitionSlug: input.Slug,
			IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(history, 2)
	assert.Equal(copiedDefinition.ID, history[0].DefinitionID)
	assert.Equal("beta", *history[0].Value.Text)
	assert.Equal(copiedDefinition.ID, history[1].DefinitionID)
	assert.Equal("alpha", *history[1].Value.Text)
	assert.Equal(ProvenanceExtraction, history[1].Source)
	assert.Equal(sourceRef, *history[1].SourceRef)
	assert.InDelta(confidence, *history[1].Confidence, 0)
	assert.Equal(actor, *history[1].Actor)
	require.NotNil(history[1].ActiveUntil)
	require.NotNil(history[1].SupersededAt)
}

func TestCopySubset_RecordReferencesFollowIdentityPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	owner, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	targetParticipant, err := source.EnsureParticipant(
		"attribute-target@example.com", "attribute target", "example.com")
	require.NoError(err)
	target, _, err := source.CreatePersonFromParticipant(targetParticipant)
	require.NoError(err)

	input := subsetPersonDefinition("synthetic_person_reference")
	input.UniversalID = "test-synthetic-person-reference"
	input.ValueType = AttributeValueRecordReference
	input.FieldType = AttributeFieldPerson
	input.RecordTarget = new("person")
	_, err = source.CreateAttributeDefinitionContext(ctx, input)
	require.NoError(err)
	write, err := source.SetPersonAttributeValueContext(ctx, PersonAttributeValueInput{
		PersonID: owner.ID, DefinitionSlug: input.Slug,
		Value: AttributeValue{
			Type:       AttributeValueRecordReference,
			RecordType: new("person"),
			RecordID:   &target.ID,
		},
		Source: ProvenanceUser,
	})
	require.NoError(err)
	require.NoError(source.Close())

	defaultDir := filepath.Join(t.TempDir(), "default")
	_, err = CopySubset(srcDB, defaultDir, 5, false)
	require.NoError(err)
	defaultSubset, err := Open(filepath.Join(defaultDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = defaultSubset.Close() }()
	_, err = defaultSubset.GetPerson(owner.ID)
	require.NoError(err, "message-derived owner remains included")
	_, err = defaultSubset.GetPerson(target.ID)
	require.ErrorIs(err, ErrPersonNotFound,
		"off-message record target stays outside the default identity boundary")
	defaultValues, err := defaultSubset.ListPersonAttributeValuesContext(
		ctx, owner.ID, PersonAttributeQuery{
			DefinitionSlug: input.Slug,
			IncludeHistory: true,
		})
	require.NoError(err)
	assert.Empty(defaultValues,
		"record references to excluded identities must not dangle in the subset")

	identityDir := filepath.Join(t.TempDir(), "identity")
	_, err = CopySubsetWithOptions(srcDB, identityDir, 5, CopySubsetOptions{
		IncludeIdentity:   true,
		IncludeAttributes: true,
	})
	require.NoError(err)
	identitySubset, err := Open(filepath.Join(identityDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = identitySubset.Close() }()
	copiedTarget, err := identitySubset.GetPerson(target.ID)
	require.NoError(err)
	assert.Equal(target.ParticipantIDs, copiedTarget.ParticipantIDs)
	identityValues, err := identitySubset.ListPersonAttributeValuesContext(
		ctx, owner.ID, PersonAttributeQuery{
			DefinitionSlug: input.Slug,
			IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(identityValues, 1)
	assert.Equal(write.Value.ID, identityValues[0].ID)
	assert.Equal(target.ID, *identityValues[0].Value.RecordID)
}

// TestCopySubset_IncludeIdentityPreservesClusters covers a promoted linked
// cluster whose second member has no messages in the subset: with the
// identity opt-in, the cluster-mate row, the link edge, and both person
// bindings must all survive the copy, so the destination aggregates the
// cluster exactly like the source.
func TestCopySubset_IncludeIdentityPreservesClusters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	alias, err := source.EnsureParticipant("offline-alias@example.com", "Alias", "example.com")
	require.NoError(err)
	_, err = source.LinkParticipants(2, alias)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 5, true)
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	members, err := destination.ClusterMembers(2)
	require.NoError(err)
	assert.Equal([]int64{2, alias}, members)

	copied, err := destination.GetPerson(person.ID)
	require.NoError(err)
	assert.Equal(person.ParticipantIDs, copied.ParticipantIDs)
}

// TestCopySubset_DefaultExcludesOffMessageIdentities pins the privacy
// boundary: without the identity opt-in, a linked identity with no messages
// in the subset must not be copied — not its participant row, not its
// identifiers, not the link edge — and the person spanning it is skipped
// entirely rather than copied with a truncated binding set.
func TestCopySubset_DefaultExcludesOffMessageIdentities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	alias, err := source.EnsureParticipant("offline-alias@example.com", "Alias", "example.com")
	require.NoError(err)
	_, err = source.LinkParticipants(2, alias)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err)
	db, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })

	var count int64
	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM participants WHERE id = ?", alias).Scan(&count))
	assert.Zero(count, "off-message cluster-mate must not be copied")
	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM participant_links").Scan(&count))
	assert.Zero(count, "link edge to an excluded participant must not be copied")
	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM persons WHERE id = ?", person.ID).Scan(&count))
	assert.Zero(count, "person with out-of-subset bindings must be skipped, not truncated")
}

// TestCopySubset_IncludeIdentitySpansUnlinkedClusters is the regression for
// a person left spanning disconnected clusters by an unlink: the identity
// closure must expand through person bindings (not just link edges) so the
// copied profile keeps its complete binding set.
func TestCopySubset_IncludeIdentitySpansUnlinkedClusters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	alias, err := source.EnsureParticipant("offline-alias@example.com", "Alias", "example.com")
	require.NoError(err)
	_, err = source.LinkParticipants(2, alias)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, err = source.UnlinkParticipants(2, alias)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 5, true)
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	copied, err := destination.GetPerson(person.ID)
	require.NoError(err)
	assert.Equal([]int64{2, alias}, copied.ParticipantIDs)
	assert.Equal(person.Revision, copied.Revision)
}

func TestCopySubset_FTSPopulated(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	_, err := CopySubset(srcDB, dstDir, 5, false)
	require.NoError(t, err, "CopySubset")

	db, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var count int64
	err = db.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&count)
	if err != nil {
		t.Skip("FTS5 not available")
	}
	assert.NotZero(t, count, "expected FTS index to be populated")
}

func TestCopySubset_ConversationCounts(t *testing.T) {
	require := require.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 10)

	_, err := CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err, "CopySubset")

	db, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`
		SELECT c.id, c.message_count,
			(SELECT COUNT(*) FROM messages m
			 WHERE m.conversation_id = c.id) AS actual_count
		FROM conversations c`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id, denormalized, actual int64
		require.NoError(rows.Scan(&id, &denormalized, &actual))
		assert.Equal(t, actual, denormalized,
			"conversation %d: denormalized count=%d, actual=%d", id, denormalized, actual)
	}
	require.NoError(rows.Err(), "conversation rows")
}

func TestCopySubset_DestinationEmptyDir(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	require.NoError(os.MkdirAll(dstDir, 0755))

	result, err := CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err, "CopySubset with pre-existing empty dir")

	assert.Equal(int64(5), result.Messages, "Messages")

	_, err = os.Stat(filepath.Join(dstDir, "msgvault.db"))
	assert.NoError(err, "msgvault.db not created")
}

func TestCopySubset_DestinationDBExists(t *testing.T) {
	require := require.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	require.NoError(os.MkdirAll(dstDir, 0755))
	require.NoError(os.WriteFile(
		filepath.Join(dstDir, "msgvault.db"), []byte("existing"), 0644,
	))

	_, err := CopySubset(srcDB, dstDir, 5, false)
	require.Error(err, "expected error when destination DB exists")
	assert.ErrorContains(t, err, "destination database already exists")
}

func TestCopySubset_SQLInjectionInPath(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	quotedDir := filepath.Join(srcDir, "test'db")
	require.NoError(t, os.MkdirAll(quotedDir, 0755))
	srcDB := createTestSourceDB(t, quotedDir, 3)

	result, err := CopySubset(srcDB, dstDir, 3, false)
	require.NoError(t, err, "CopySubset with quoted path")
	assert.Equal(t, int64(3), result.Messages, "Messages")
}

func TestCopySubset_NonPositiveRowCount(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		_, err := CopySubset("/tmp/fake.db", t.TempDir(), n, false)
		assert.Error(t, err, "CopySubset(rowCount=%d) should error", n)
	}
}

func TestCopySubset_TimestampFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	dbPath := filepath.Join(srcDir, "msgvault.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err)

	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'test@example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO participants (id, email_address, domain)
		VALUES (1, 'alice@example.com', 'example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES (1, 1, 'email_thread', 'Thread', 3, 1)`)
	require.NoError(err)

	// msg 1: only received_at (no sent_at), most recent
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, received_at, sender_id, subject)
		VALUES (1, 1, 1, 'msg_1', 'email', '2025-06-01', 1,
			'Received only')`)
	require.NoError(err)

	// msg 2: only internal_date (no sent_at), second most recent
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, internal_date, sender_id, subject)
		VALUES (2, 1, 1, 'msg_2', 'email', '2025-05-01', 1,
			'Internal only')`)
	require.NoError(err)

	// msg 3: has sent_at, oldest
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, sent_at, sender_id, subject)
		VALUES (3, 1, 1, 'msg_3', 'email', '2025-04-01', 1,
			'Sent only')`)
	require.NoError(err)

	for i := 1; i <= 3; i++ {
		_, err = db.Exec(`
			INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			VALUES (?, 1, 'from')`, i)
		require.NoError(err)
	}
	_ = db.Close()

	// Request 2 most recent — should get msg 1 and 2 (by fallback
	// timestamps), not just msg 3 (the only one with sent_at).
	result, err := CopySubset(dbPath, dstDir, 2, false)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(2), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	var subjects []string
	rows, err := dstDB.Query("SELECT subject FROM messages")
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		require.NoError(rows.Scan(&s))
		subjects = append(subjects, s)
	}
	require.NoError(rows.Err(), "subject rows")

	for _, s := range subjects {
		assert.NotEqual("Sent only", s,
			"oldest message (sent_at only) should not be selected")
	}

	// last_message_at must use the fallback timestamp, not be NULL
	var lastMsg sql.NullString
	require.NoError(dstDB.QueryRow(
		"SELECT last_message_at FROM conversations",
	).Scan(&lastMsg))
	assert.True(lastMsg.Valid, "last_message_at is NULL; should use fallback timestamp")
}

func TestCopySubset_TieBreaker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	dbPath := filepath.Join(srcDir, "msgvault.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err)

	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'test@example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO participants (id, email_address, domain)
		VALUES (1, 'alice@example.com', 'example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES (1, 1, 'email_thread', 'Thread', 4, 1)`)
	require.NoError(err)

	// 4 messages with identical timestamps; higher IDs should win
	sameTime := "2025-06-01 12:00:00"
	for i := 1; i <= 4; i++ {
		_, err = db.Exec(`
			INSERT INTO messages
				(id, conversation_id, source_id, source_message_id,
				 message_type, sent_at, sender_id, subject)
			VALUES (?, 1, 1, ?, 'email', ?, 1, ?)`,
			i, fmt.Sprintf("msg_%d", i), sameTime,
			fmt.Sprintf("Msg %d", i))
		require.NoError(err, "insert message %d", i)
		_, err = db.Exec(`
			INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			VALUES (?, 1, 'from')`, i)
		require.NoError(err)
	}
	_ = db.Close()

	// Select 2 of 4 — should get IDs 4 and 3 (highest IDs)
	result, err := CopySubset(dbPath, dstDir, 2, false)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(2), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	rows, err := dstDB.Query(
		"SELECT id FROM messages ORDER BY id")
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(rows.Err(), "id rows")

	assert.Equal([]int64{3, 4}, ids, "selected IDs")
}

func TestCopySubset_ReplyToOrphanNulled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	dbPath := filepath.Join(srcDir, "msgvault.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err)

	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'test@example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO participants (id, email_address, domain)
		VALUES (1, 'alice@example.com', 'example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES (1, 1, 'email_thread', 'Thread', 2, 1)`)
	require.NoError(err)

	// Old parent message (won't be selected with limit 1)
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, sent_at, sender_id, subject)
		VALUES (1, 1, 1, 'parent', 'email', '2020-01-01', 1,
			'Parent')`)
	require.NoError(err)

	// Recent reply referencing the parent
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, sent_at, sender_id, subject,
			 reply_to_message_id)
		VALUES (2, 1, 1, 'reply', 'email', '2025-06-01', 1,
			'Reply', 1)`)
	require.NoError(err)

	for i := 1; i <= 2; i++ {
		_, err = db.Exec(`
			INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			VALUES (?, 1, 'from')`, i)
		require.NoError(err)
	}
	_ = db.Close()

	// Select only 1 most recent — the reply, not the parent
	result, err := CopySubset(dbPath, dstDir, 1, false)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(1), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	// reply_to_message_id should be nulled out since parent
	// wasn't included
	var replyTo sql.NullInt64
	require.NoError(dstDB.QueryRow(`
		SELECT reply_to_message_id FROM messages
		WHERE subject = 'Reply'`,
	).Scan(&replyTo))
	assert.False(replyTo.Valid,
		"reply_to_message_id = %d, want NULL (parent excluded)", replyTo.Int64)

	// FK integrity must pass
	fkRows, err := dstDB.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "FK violations with orphaned reply_to_message_id")
}

func TestCopySubset_ExcludesSoftDeleted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 10)

	// Soft-delete the 5 most recent messages
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		UPDATE messages SET deleted_from_source_at = '2025-01-01'
		WHERE id IN (
			SELECT id FROM messages ORDER BY sent_at DESC LIMIT 5
		)`)
	require.NoError(err, "soft-delete messages")
	_ = db.Close()

	// Request 5 messages — should get the 5 non-deleted ones
	result, err := CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(5), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	// None of the copied messages should be soft-deleted
	var deletedCount int64
	require.NoError(dstDB.QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE deleted_from_source_at IS NOT NULL`,
	).Scan(&deletedCount))
	assert.Equal(int64(0), deletedCount, "soft-deleted messages in subset")
}

func TestCopySubset_ReactionParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	// Add a reactor participant who is neither sender nor recipient
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO participants
			(id, email_address, display_name, domain)
		VALUES (100, 'reactor@example.com', 'Reactor', 'example.com')`)
	require.NoError(err, "insert reactor")
	_, err = db.Exec(`
		INSERT INTO reactions
			(id, message_id, participant_id,
			 reaction_type, reaction_value)
		VALUES (1, 1, 100, 'emoji', 'thumbsup')`)
	require.NoError(err, "insert reaction")
	_ = db.Close()

	result, err := CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(5), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	// Reactor participant must be present
	var reactorCount int64
	require.NoError(dstDB.QueryRow(`
		SELECT COUNT(*) FROM participants
		WHERE email_address = 'reactor@example.com'`,
	).Scan(&reactorCount))
	assert.Equal(int64(1), reactorCount, "reactor participant count")

	// Reaction must be present
	var rxnCount int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM reactions",
	).Scan(&rxnCount))
	assert.Equal(int64(1), rxnCount, "reactions count")

	// FK integrity
	fkRows, err := dstDB.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "FK violations with reaction participants")
}

// TestCopySubset_NullSourceIDLabels verifies that user-created labels
// with NULL source_id are preserved when attached to selected messages.
func TestCopySubset_NullSourceIDLabels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	// Add a user-created label with NULL source_id and attach it
	// to message 1.
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO labels (id, source_id, name, label_type)
		VALUES (100, NULL, 'My Custom Label', 'user')`)
	require.NoError(err, "insert null-source label")
	_, err = db.Exec(`
		INSERT INTO message_labels (message_id, label_id)
		VALUES (1, 100)`)
	require.NoError(err, "insert message_label")
	_ = db.Close()

	result, err := CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err, "CopySubset")

	// The 3 source-scoped labels + 1 user-created label
	assert.Equal(int64(4), result.Labels, "Labels")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	var labelName string
	require.NoError(dstDB.QueryRow(`
		SELECT name FROM labels WHERE source_id IS NULL`,
	).Scan(&labelName), "query null-source label")
	assert.Equal("My Custom Label", labelName, "label name")

	// message_labels link must be preserved
	var mlCount int64
	require.NoError(dstDB.QueryRow(`
		SELECT COUNT(*) FROM message_labels WHERE label_id = 100`,
	).Scan(&mlCount))
	assert.Equal(int64(1), mlCount, "message_labels for label 100")

	// FK integrity
	fkRows, err := dstDB.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "FK violations with null-source-id labels")
}

// TestCopySubset_SourceFKViolationIgnored verifies that pre-existing FK
// violations in the source DB (outside the copied subset) don't cause
// CopySubset to fail. This guards against the regression where src was
// still attached during PRAGMA foreign_key_check.
func TestCopySubset_SourceFKViolationIgnored(t *testing.T) {
	require := require.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	// Inject an FK violation in the source: a message_labels row
	// referencing a non-existent label_id.
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO message_labels (message_id, label_id)
		VALUES (1, 9999)`)
	require.NoError(err, "inject FK violation")
	_ = db.Close()

	// CopySubset should succeed — FK check must only scan destination
	result, err := CopySubset(srcDB, dstDir, 3, false)
	require.NoError(err, "CopySubset (source FK leak)")
	assert.Equal(t, int64(3), result.Messages, "Messages")
}

func TestCopySubset_MissingSourceDB(t *testing.T) {
	assert := assert.New(t)
	dstDir := filepath.Join(t.TempDir(), "dst")
	fakeSrc := filepath.Join(t.TempDir(), "nonexistent.db")

	_, err := CopySubset(fakeSrc, dstDir, 5, false)
	require.Error(t, err, "expected error for missing source DB")
	require.ErrorContains(t, err, "source database not found")

	// ATTACH on a missing path would create a file; verify it wasn't
	_, statErr := os.Stat(fakeSrc)
	assert.True(os.IsNotExist(statErr), "missing source path was created as a side effect")

	// Destination should be cleaned up
	_, statErr = os.Stat(dstDir)
	assert.True(os.IsNotExist(statErr), "destination directory was not cleaned up")
}

func TestCopySubset_MultiSourceScoping(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	dbPath := filepath.Join(srcDir, "msgvault.db")

	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err, "open db")

	// Two sources: only source 1 will have recent messages
	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES
			(1, 'gmail', 'alice@example.com'),
			(2, 'gmail', 'bob@example.com')`)
	require.NoError(err, "insert sources")

	_, err = db.Exec(`
		INSERT INTO participants
			(id, email_address, display_name, domain)
		VALUES
			(1, 'alice@example.com', 'Alice', 'example.com'),
			(2, 'bob@example.com', 'Bob', 'example.com')`)
	require.NoError(err, "insert participants")

	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES
			(1, 1, 'email_thread', 'Alice thread', 2, 1),
			(2, 2, 'email_thread', 'Bob thread', 2, 1)`)
	require.NoError(err, "insert conversations")

	// Labels for both sources
	_, err = db.Exec(`
		INSERT INTO labels (id, source_id, name, label_type) VALUES
			(1, 1, 'INBOX', 'system'),
			(2, 1, 'Work', 'user'),
			(3, 2, 'INBOX', 'system'),
			(4, 2, 'Personal', 'user')`)
	require.NoError(err, "insert labels")

	// Source 1 messages: recent (will be selected)
	for i := 1; i <= 3; i++ {
		_, err = db.Exec(`
			INSERT INTO messages
				(id, conversation_id, source_id, source_message_id,
				 message_type, sent_at, sender_id, subject)
			VALUES (?, 1, 1, ?, 'email',
				datetime('2025-01-01', '+' || ? || ' hours'),
				1, ?)`,
			i, fmt.Sprintf("msg_%d", i), i,
			fmt.Sprintf("Alice msg %d", i))
		require.NoError(err, "insert alice message %d", i)
		_, err = db.Exec(
			`INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			 VALUES (?, 1, 'from')`, i)
		require.NoError(err, "insert alice recipient %d", i)
	}

	// Source 2 messages: older (won't be selected with limit 3)
	for i := 4; i <= 6; i++ {
		_, err = db.Exec(`
			INSERT INTO messages
				(id, conversation_id, source_id, source_message_id,
				 message_type, sent_at, sender_id, subject)
			VALUES (?, 2, 2, ?, 'email',
				datetime('2020-01-01', '+' || ? || ' hours'),
				2, ?)`,
			i, fmt.Sprintf("msg_%d", i), i,
			fmt.Sprintf("Bob msg %d", i))
		require.NoError(err, "insert bob message %d", i)
		_, err = db.Exec(
			`INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			 VALUES (?, 2, 'from')`, i)
		require.NoError(err, "insert bob recipient %d", i)
	}

	_ = db.Close()

	// Select only 3 most recent = all Alice, no Bob
	result, err := CopySubset(dbPath, dstDir, 3, false)
	require.NoError(err, "CopySubset")

	assert.Equal(int64(1), result.Sources, "Sources (only Alice's)")
	assert.Equal(int64(3), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	// Only source 1 should be present
	var srcCount int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM sources",
	).Scan(&srcCount))
	assert.Equal(int64(1), srcCount, "sources count")

	var identifier string
	require.NoError(dstDB.QueryRow(
		"SELECT identifier FROM sources",
	).Scan(&identifier))
	assert.Equal("alice@example.com", identifier, "source identifier")

	// Only source 1 labels should be present
	var labelCount int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM labels",
	).Scan(&labelCount))
	assert.Equal(int64(2), labelCount, "labels count (Alice's labels only)")

	// No Bob conversations
	var convCount int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM conversations",
	).Scan(&convCount))
	assert.Equal(int64(1), convCount, "conversations (Alice's only)")

	// FK integrity check
	fkRows, err := dstDB.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "foreign key violations in multi-source subset")
}

func TestCopySubset_LegacySourceWithoutOAuthApp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	// Create a source DB, then drop the oauth_app column to simulate
	// a pre-oauth_app database.
	srcDB := createTestSourceDB(t, srcDir, 3)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	// SQLite doesn't support DROP COLUMN before 3.35. Rebuild the
	// table without oauth_app to simulate an old schema.
	_, err = db.Exec(`
		CREATE TABLE sources_old AS
			SELECT id, source_type, identifier, display_name,
			       google_user_id, last_sync_at, sync_cursor,
			       sync_config, created_at, updated_at
			FROM sources;
		DROP TABLE sources;
		ALTER TABLE sources_old RENAME TO sources;
	`)
	require.NoError(err, "rebuild sources without oauth_app")
	_ = db.Close()

	// CopySubset should succeed with NULL oauth_app in destination
	result, err := CopySubset(srcDB, dstDir, 3, false)
	require.NoError(err, "CopySubset from legacy DB")
	assert.Equal(int64(3), result.Messages, "Messages")

	// Verify oauth_app is NULL in the destination
	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	var oauthApp sql.NullString
	require.NoError(dstDB.QueryRow(
		"SELECT oauth_app FROM sources",
	).Scan(&oauthApp), "query oauth_app")
	assert.False(oauthApp.Valid, "oauth_app = %q, want NULL", oauthApp.String)
}

func TestCopySubset_ControlCharInPath(t *testing.T) {
	dstDir := filepath.Join(t.TempDir(), "dst")
	base := t.TempDir()

	controlPaths := []string{
		filepath.Join(base, "test\ndb", "msgvault.db"),
		filepath.Join(base, "test\tdb", "msgvault.db"),
		filepath.Join(base, "test\x7Fdb", "msgvault.db"),
		filepath.Join(base, "test\x01db", "msgvault.db"),
	}
	for _, p := range controlPaths {
		_, err := CopySubset(p, dstDir, 5, false)
		assert.Error(t, err, "CopySubset(%q) should reject control chars", p)
	}
}

// TestCopySubset_LegacySourceMissingAttributionColumns covers a source archive
// whose messages table lacks a column the destination schema has.
//
// source_is_from_me and identity_is_from_me are both added to older archives
// by SQLiteDialect.LegacyColumnMigrations(), so an archive written before
// those migrations legitimately lacks them. The copy intersects the source and
// destination messages columns at run time, so such an archive copies the
// columns the two schemas share and leaves the absent ones at the
// destination's own default.
//
// TestCopySubset_LegacySourceWithoutOAuthApp rebuilds its table via CREATE
// TABLE ... AS SELECT to avoid ALTER TABLE ... DROP COLUMN, which SQLite only
// added in 3.35; this test does use DROP COLUMN and so needs SQLite 3.35 or
// newer. The messages table cannot be rebuilt the other way: the triggers
// trg_message_bodies_last_modified_upd and trg_message_bodies_last_modified_ins
// update messages, which resolves to main.messages, so the rename back — ALTER
// TABLE ... RENAME TO messages — fails its schema reparse with "error in
// trigger trg_message_bodies_last_modified_upd: no such table: main.messages".
// Neither attribution column is indexed or named by any trigger or view, so
// ALTER TABLE ... DROP COLUMN works directly.
func TestCopySubset_LegacySourceMissingAttributionColumns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 3)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err, "open source db")

	for _, col := range []string{"source_is_from_me", "identity_is_from_me"} {
		_, err = db.Exec(
			`ALTER TABLE messages DROP COLUMN ` + col,
		)
		require.NoError(err, "drop messages.%s", col)
	}

	var srcCount int
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&srcCount),
		"count source messages")
	require.Equal(3, srcCount, "source messages")
	require.NoError(db.Close(), "close source db")

	result, err := CopySubset(srcDB, dstDir, 100, false)
	require.NoError(err, "CopySubset from a source missing attribution columns")
	assert.Equal(int64(srcCount), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination db")
	defer func() { _ = dstDB.Close() }()

	var dstCount int
	require.NoError(
		dstDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&dstCount),
		"count destination messages")
	assert.Equal(srcCount, dstCount, "destination message count")

	// The absent columns take the destination schema's defaults:
	// source_is_from_me has none (NULL), identity_is_from_me defaults to FALSE.
	var sourceDefaults int
	require.NoError(dstDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE source_is_from_me IS NULL`,
	).Scan(&sourceDefaults), "count source_is_from_me defaults")
	assert.Equal(dstCount, sourceDefaults,
		"source_is_from_me should hold its schema default (NULL)")

	var identityDefaults int
	require.NoError(dstDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE identity_is_from_me = 0`,
	).Scan(&identityDefaults), "count identity_is_from_me defaults")
	assert.Equal(dstCount, identityDefaults,
		"identity_is_from_me should hold its schema default (FALSE)")
}

// TestCopySubset_SourceOnlyColumnWithQuoteInName covers a source archive whose
// messages table carries a column the destination schema does not have, and
// whose name contains a double quote. The column falls outside the two
// schemas' intersection, so it is never interpolated into the copy's SQL and
// the copy proceeds without it.
func TestCopySubset_SourceOnlyColumnWithQuoteInName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 3)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err, "open source db")
	_, err = db.Exec(`ALTER TABLE messages ADD COLUMN "we""ird" TEXT`)
	require.NoError(err, `add messages."we""ird"`)
	require.NoError(db.Close(), "close source db")

	result, err := CopySubset(srcDB, dstDir, 100, false)
	require.NoError(err, "CopySubset from a source with a quoted column name")
	assert.Equal(int64(3), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination db")
	defer func() { _ = dstDB.Close() }()

	var dstCount int
	require.NoError(
		dstDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&dstCount),
		"count destination messages")
	assert.Equal(3, dstCount, "destination message count")
}

// TestCopySubset_CommonColumnWithQuoteIsEscapedAndCopied covers a column
// present in both schemas whose name contains a double quote. That name is
// interpolated into the copy's SQL, so commonColumns escapes it — doubling the
// quote, the way SQL escapes one inside a quoted identifier — rather than
// refusing the copy. The name also carries an injection payload, which the
// escaping renders inert.
func TestCopySubset_CommonColumnWithQuoteIsEscapedAndCopied(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")

	// Closes the identifier, ends the statement, drops the table, and comments
	// out whatever the interpolator appends after it.
	const hostile = `we"ird" ); DROP TABLE t; --`
	quoted := `"` + strings.ReplaceAll(hostile, `"`, `""`) + `"`

	for _, path := range []string{srcPath, dstPath} {
		db, err := sql.Open("sqlite3", path)
		require.NoError(err, "open %s", path)
		_, err = db.Exec(`CREATE TABLE t (id INTEGER, ` + quoted + ` TEXT)`)
		require.NoError(err, "create t in %s", path)
		require.NoError(db.Close(), "close %s", path)
	}

	srcDB, err := sql.Open("sqlite3", srcPath)
	require.NoError(err, "open source db")
	_, err = srcDB.Exec(`INSERT INTO t (id, ` + quoted + `) VALUES (1, 'carried')`)
	require.NoError(err, "seed source row")
	require.NoError(srcDB.Close(), "close source db")

	db, err := sql.Open("sqlite3", dstPath)
	require.NoError(err, "open destination db")
	defer func() { _ = db.Close() }()
	_, err = db.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS src", srcPath))
	require.NoError(err, "attach source db")

	tx, err := db.Begin()
	require.NoError(err, "begin transaction")
	defer func() { _ = tx.Rollback() }()

	cols, err := commonColumns(tx, "t")
	require.NoError(err, "commonColumns must render a quoted column name, not refuse it")
	assert.Equal([]string{`"id"`, quoted}, cols,
		"the embedded quote must be doubled, not dropped and not rejected")

	// The rendered list is what the copy interpolates, so run the copy it feeds.
	list := strings.Join(cols, ", ")
	_, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO t (%s) SELECT %s FROM src.t`, list, list))
	require.NoError(err, "copy through the escaped column list")

	var carried string
	require.NoError(tx.QueryRow(`SELECT `+quoted+` FROM t WHERE id = 1`).Scan(&carried),
		"read the copied value")
	assert.Equal("carried", carried, "the copy must carry the oddly named column's value")

	var tables int
	require.NoError(tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 't'`).Scan(&tables),
		"read sqlite_master")
	assert.Equal(1, tables, "the payload riding in the column name must not have executed")
}

// TestCopySubset_SourceColumnCaseDiffers covers a source archive that declares
// a messages column in a different case than the destination schema. SQLite
// compares identifiers case-insensitively, so the two are the same column and
// its values must be copied rather than left at the destination's default.
func TestCopySubset_SourceColumnCaseDiffers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 3)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err, "open source db")
	_, err = db.Exec(`ALTER TABLE messages DROP COLUMN identity_is_from_me`)
	require.NoError(err, "drop messages.identity_is_from_me")
	_, err = db.Exec(`ALTER TABLE messages ADD COLUMN Identity_Is_From_Me BOOLEAN`)
	require.NoError(err, "add messages.Identity_Is_From_Me")
	_, err = db.Exec(`UPDATE messages SET Identity_Is_From_Me = TRUE`)
	require.NoError(err, "set messages.Identity_Is_From_Me")
	require.NoError(db.Close(), "close source db")

	result, err := CopySubset(srcDB, dstDir, 100, false)
	require.NoError(err, "CopySubset from a source whose column case differs")
	assert.Equal(int64(3), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination db")
	defer func() { _ = dstDB.Close() }()

	var copied int
	require.NoError(dstDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE identity_is_from_me = 1`,
	).Scan(&copied), "count copied identity_is_from_me")
	assert.Equal(3, copied,
		"identity_is_from_me should carry the source's values, not the default")
}

// TestCopySubset_SourceOnlyColumnUnicodeLookalike covers a source archive whose
// messages table carries a source-only column whose name differs from a
// destination column's only under Unicode case conversion: "İ" (U+0130, capital
// I with dot above) where the destination has ASCII "i".
//
// SQLite folds identifiers over ASCII only, so İdentity_is_from_me and
// identity_is_from_me are two different columns and the source simply lacks the
// destination's. Go's strings.ToLower folds them together, which would put
// identity_is_from_me in the copy's column list and make the copy select a
// column src.messages does not have; SQLite's double-quoted-string misfeature
// would then read that name as a string literal and store the text
// "identity_is_from_me" in every destination row. The destination's own default
// (FALSE) must win instead.
//
// This is the non-ASCII counterpart to
// TestCopySubset_SourceColumnCaseDiffers, which covers the ordinary ASCII case
// where the two spellings are the same column.
func TestCopySubset_SourceOnlyColumnUnicodeLookalike(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 3)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err, "open source db")
	_, err = db.Exec(`ALTER TABLE messages DROP COLUMN identity_is_from_me`)
	require.NoError(err, "drop messages.identity_is_from_me")
	_, err = db.Exec(`ALTER TABLE messages ADD COLUMN "İdentity_is_from_me" TEXT`)
	require.NoError(err, "add messages.İdentity_is_from_me")
	_, err = db.Exec(`UPDATE messages SET "İdentity_is_from_me" = 'source value'`)
	require.NoError(err, "set messages.İdentity_is_from_me")
	require.NoError(db.Close(), "close source db")

	result, err := CopySubset(srcDB, dstDir, 100, false)
	require.NoError(err, "CopySubset from a source with a Unicode-lookalike column")
	assert.Equal(int64(3), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination db")
	defer func() { _ = dstDB.Close() }()

	var defaulted int
	require.NoError(dstDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE identity_is_from_me = 0`,
	).Scan(&defaulted), "count identity_is_from_me defaults")
	assert.Equal(3, defaulted,
		"identity_is_from_me should hold the destination default (FALSE), "+
			"the source not having that column")

	// Name the failure the Unicode fold produces: the quoted column name
	// degrading to a string literal writes its own text into every row.
	var literals int
	require.NoError(dstDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE identity_is_from_me = 'identity_is_from_me'`,
	).Scan(&literals), "count identity_is_from_me string literals")
	assert.Equal(0, literals,
		"identity_is_from_me must not hold its own name as a string")
}

// TestCopySubset_NullWatermarkIsRestamped covers what the positional copy can
// carry through that no other write path can.
//
// The copy names content_changed_at whenever the source has it, which supplies
// the value explicitly and so bypasses the column's DEFAULT, and a database
// created from schema.sql has no AFTER INSERT trigger behind that default. So a NULL
// watermark in the source arrives as a NULL watermark in the subset — and stays
// one: the change feed's range predicate excludes NULL, and the migration that
// would fill it in already ran on this database while it was empty and is
// recorded as applied. The message would never appear in the feed again.
//
// The source's NULL is written the way one can now exist at all: an INSERT that
// names content_changed_at and gives it NULL. That is the hole the DEFAULT
// leaves open on a fresh database, so it is the shape worth copying badly.
func TestCopySubset_NullWatermarkIsRestamped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcPath := createTestSourceDB(t, srcDir, 4)
	srcDB, err := sql.Open("sqlite3", srcPath+"?_foreign_keys=OFF")
	require.NoError(err, "open source")
	_, err = srcDB.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id, message_type,
			 subject, content_changed_at)
		VALUES (99, 1, 1, 'msg_unwatermarked', 'email', 'No watermark', NULL)`)
	require.NoError(err, "insert a message with no watermark")
	var srcWatermark sql.NullString
	require.NoError(srcDB.QueryRow(
		"SELECT content_changed_at FROM messages WHERE id = 99").Scan(&srcWatermark),
		"read the source watermark")
	require.False(srcWatermark.Valid,
		"the source fixture is only meaningful if the NULL survived the insert: a "+
			"DEFAULT does not apply to a column the statement names")
	require.NoError(srcDB.Close(), "close source")

	_, err = CopySubset(srcPath, dstDir, 100, false)
	require.NoError(err, "CopySubset")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination")
	defer func() { _ = dstDB.Close() }()

	var missing int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE content_changed_at IS NULL").Scan(&missing),
		"count unwatermarked messages")
	assert.Zero(missing,
		"the subset carried a NULL content_changed_at through. Nothing in the "+
			"destination will ever stamp it — the INSERT trigger is absent on a "+
			"fresh database and the backfill has already been recorded as applied — "+
			"so the change feed can never report that message again")

	// Read the stored text rather than the scanned value: go-sqlite3 converts a
	// DATETIME column to time.Time on the way out, which would hide the width
	// the feed actually compares. The feed's cursor comparison is lexical, so a
	// substitute in any other shape sorts into the wrong place.
	var copied string
	require.NoError(dstDB.QueryRow(
		"SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = 99").Scan(&copied),
		"read the copied watermark")
	_, parseErr := time.Parse(SQLiteTimestampLayout, copied)
	assert.NoErrorf(parseErr,
		"the substituted watermark %q must be in the format SQLiteDialect."+
			"ContentChangedNow writes", copied)

	var oddlyShaped int64
	require.NoError(dstDB.QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE CAST(content_changed_at AS TEXT) NOT GLOB
			'[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9]'`,
	).Scan(&oddlyShaped), "count oddly shaped watermarks")
	assert.Zero(oddlyShaped,
		"every watermark in the subset must share one textual shape: the feed "+
			"orders them lexically, so a stamp of a different width sorts wrong")
}

// TestCopySubset_LegacySourceWithoutContentChangedAt covers a source database
// created before content_changed_at existed at all — not one where the column
// exists and holds NULL (TestCopySubset_NullWatermarkIsRestamped covers that).
//
// The destination is always built from the current schema, so the positional
// `INSERT INTO messages SELECT * FROM src.messages` this copy used to run
// supplied one value fewer than the destination has columns and SQLite rejected
// the whole statement ("table messages has 34 columns but 33 values were
// supplied"). TestCopySubset_LegacySourceWithoutOAuthApp establishes that older
// source schemas are supported; a copy that only works when the source is
// already current is a regression in that, and the NULL restamp that follows
// the INSERT never got to run because the INSERT failed first.
func TestCopySubset_LegacySourceWithoutContentChangedAt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcPath := createTestSourceDB(t, srcDir, 3)
	srcDB, err := sql.Open("sqlite3", srcPath+"?_foreign_keys=OFF")
	require.NoError(err, "open source")

	// Remove every schema object that references the column, then the column
	// itself, to leave a messages table shaped the way a pre-feature archive's
	// is.
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_ins`,
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_at`,
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_ins`,
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_upd`,
		`DROP INDEX IF EXISTS idx_messages_content_changed_at`,
		`ALTER TABLE messages DROP COLUMN content_changed_at`,
	} {
		_, err = srcDB.Exec(stmt)
		require.NoErrorf(err, "prepare pre-feature source: %s", stmt)
	}

	var present int
	require.NoError(srcDB.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages')
		 WHERE name = 'content_changed_at'`).Scan(&present),
		"inspect the source's messages columns")
	require.Zero(present,
		"the fixture is only meaningful if the source genuinely lacks the column")
	require.NoError(srcDB.Close(), "close source")

	result, err := CopySubset(srcPath, dstDir, 100, false)
	require.NoError(err, "CopySubset from a source without content_changed_at")
	assert.Equal(int64(3), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination")
	defer func() { _ = dstDB.Close() }()

	var copied, missing int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM messages").Scan(&copied), "count copied messages")
	assert.Equal(int64(3), copied, "copied messages")
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE content_changed_at IS NULL").Scan(&missing),
		"count unwatermarked messages")
	assert.Zero(missing,
		"a message copied from a pre-feature source must be stamped on arrival: "+
			"nothing in the destination will ever stamp it later, so the change "+
			"feed could never report it")

	var oddlyShaped int64
	require.NoError(dstDB.QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE CAST(content_changed_at AS TEXT) NOT GLOB
			'[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9]'`,
	).Scan(&oddlyShaped), "count oddly shaped watermarks")
	assert.Zero(oddlyShaped,
		"every watermark in the subset must share one textual shape: the feed "+
			"orders them lexically, so a stamp of a different width sorts wrong")

	// The rest of the row must survive intact — a fallback that shifts columns
	// would still copy three rows.
	var subject string
	var sourceMessageID string
	require.NoError(dstDB.QueryRow(
		"SELECT source_message_id, subject FROM messages WHERE id = 1").
		Scan(&sourceMessageID, &subject), "read a copied row")
	assert.Equal("msg_1", sourceMessageID, "source_message_id")
	assert.Equal("Subject B", subject, "subject")
}

// TestCopySubset_BodyTriggersRestampWatermarks pins what the copy actually does
// to the two watermarks, which is not what the restamp statement alone suggests.
//
// The restamp names only content_changed_at, so it fires no trigger on
// `messages`. But the `INSERT INTO message_bodies` that follows it fires
// trg_message_bodies_content_changed_ins and schema.sql's pre-existing
// trg_message_bodies_last_modified_ins, and both of those write the parent row
// directly. The result is a split: a copied message that HAS a body carries
// copy-time values for both columns, and a bodyless one carries the source's.
//
// That split is accepted, not repaired — a subset is a new archive whose feed
// consumers start from an empty cursor, and last_modified has behaved this way
// since long before content_changed_at existed. It is pinned here because it is
// invisible from the copy statement and a reader would otherwise conclude, as
// the code comment once did, that a subset preserves the source's watermarks.
func TestCopySubset_BodyTriggersRestampWatermarks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	const (
		srcLastModified      = "2001-02-03 04:05:06"
		srcContentChangedAt  = "2001-02-03 04:05:06.000"
		bodylessMessageID    = 99
		messageWithBodyID    = 1
		messagesWithBodyLast = 2
	)

	srcPath := createTestSourceDB(t, srcDir, messagesWithBodyLast)
	srcDB, err := sql.Open("sqlite3", srcPath+"?_foreign_keys=OFF")
	require.NoError(err, "open source")

	// createTestSourceDB gives every message a body. Add one without, because
	// the presence of a body is the whole variable here.
	_, err = srcDB.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id, message_type, subject)
		VALUES (?, 1, 1, 'msg_bodyless', 'email', 'No body')`, bodylessMessageID)
	require.NoError(err, "insert a message with no body")

	// Force both watermarks to a known instant far in the past, so a copy-time
	// stamp is unmistakable.
	_, err = srcDB.Exec(
		`UPDATE messages SET last_modified = ?, content_changed_at = ?`,
		srcLastModified, srcContentChangedAt)
	require.NoError(err, "age the source watermarks")
	require.NoError(srcDB.Close(), "close source")

	_, err = CopySubset(srcPath, dstDir, 100, false)
	require.NoError(err, "CopySubset")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination")
	defer func() { _ = dstDB.Close() }()

	// Read the stored text, not the scanned value: go-sqlite3 converts a
	// DATETIME column to time.Time on the way out, which would hide the exact
	// stored form the feed compares lexically.
	read := func(id int64) (lastModified, contentChangedAt string) {
		t.Helper()
		require.NoError(dstDB.QueryRow(`
			SELECT CAST(last_modified AS TEXT), CAST(content_changed_at AS TEXT)
			FROM messages WHERE id = ?`, id).Scan(&lastModified, &contentChangedAt),
			"read the copied watermarks for message %d", id)
		return lastModified, contentChangedAt
	}

	bodylessLM, bodylessCC := read(bodylessMessageID)
	assert.Equal(srcLastModified, bodylessLM,
		"a message with no body has nothing to fire the message_bodies triggers, "+
			"so its last_modified is the source's")
	assert.Equal(srcContentChangedAt, bodylessCC,
		"and so is its content_changed_at: the restamp only fills NULLs, and this "+
			"one was not NULL")

	for id := int64(messageWithBodyID); id <= messagesWithBodyLast; id++ {
		withBodyLM, withBodyCC := read(id)
		assert.NotEqualf(srcLastModified, withBodyLM,
			"message %d has a body, so trg_message_bodies_last_modified_ins rewrote "+
				"last_modified when the body was copied; a subset does not preserve it", id)
		assert.NotEqualf(srcContentChangedAt, withBodyCC,
			"message %d has a body, so trg_message_bodies_content_changed_ins rewrote "+
				"content_changed_at when the body was copied", id)

		_, parseErr := time.Parse(SQLiteTimestampLayout, withBodyCC)
		require.NoErrorf(parseErr,
			"the rewritten watermark %q on message %d must still be in the format "+
				"SQLiteDialect.ContentChangedNow writes, or the feed's lexical cursor "+
				"orders it wrong", withBodyCC, id)
		assert.Greaterf(withBodyCC, srcContentChangedAt,
			"a copy-time stamp must sort ABOVE the source value it replaced (%q vs %q) "+
				"on message %d", withBodyCC, srcContentChangedAt, id)
	}

	// Whatever the split, no row may leave the copy unwatermarked: the feed's
	// range predicate excludes NULL and nothing in the destination would ever
	// stamp it later.
	var missing int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE content_changed_at IS NULL").Scan(&missing),
		"count unwatermarked messages")
	assert.Zero(missing, "every copied message must carry a watermark")
}

// TestCopySubset_UpgradedAuxiliaryColumnOrder covers copying from a source
// archive whose labels, participants, and conversations tables carry the
// upgraded column order: the legacy ALTER TABLE ADD COLUMN migrations append
// system_role, phone_number/canonical_id, and title/conversation_type at the
// end of their tables, while a fresh schema.sql database declares them
// mid-table. A positional SELECT * copy from such a source into a fresh
// subset lands values in the wrong columns — labels.system_role and
// labels.color swap, which loses the 'sent' role that sent-folder identity
// discovery depends on. The copy names its columns, so every value must land
// in its own column.
func TestCopySubset_UpgradedAuxiliaryColumnOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")
	srcDB := createTestSourceDB(t, srcDir, 2)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err, "open source db")

	// Rebuild each table into the shape the legacy ADD COLUMN migrations
	// produce: the late-added columns re-appended at the end, in migration
	// order. Indexes on the dropped columns go first — SQLite refuses to
	// drop an indexed column.
	for _, stmt := range []string{
		`ALTER TABLE labels DROP COLUMN system_role`,
		`ALTER TABLE labels ADD COLUMN system_role TEXT`,

		`DROP INDEX IF EXISTS idx_participants_phone`,
		`DROP INDEX IF EXISTS idx_participants_canonical`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_participant_display_name`,
		`ALTER TABLE participants DROP COLUMN phone_number`,
		`ALTER TABLE participants DROP COLUMN canonical_id`,
		`ALTER TABLE participants ADD COLUMN phone_number TEXT`,
		`ALTER TABLE participants ADD COLUMN canonical_id TEXT`,

		`DROP INDEX IF EXISTS idx_conversations_type`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_conversation_title`,
		`ALTER TABLE conversations DROP COLUMN conversation_type`,
		`ALTER TABLE conversations DROP COLUMN title`,
		`ALTER TABLE conversations ADD COLUMN title TEXT`,
		`ALTER TABLE conversations ADD COLUMN conversation_type TEXT NOT NULL DEFAULT 'email_thread'`,
	} {
		_, err = db.Exec(stmt)
		require.NoError(err, "rebuild upgraded order: %s", stmt)
	}

	_, err = db.Exec(`UPDATE labels
		SET system_role = 'sent', color = '#16a765' WHERE id = 2`)
	require.NoError(err, "seed upgraded label columns")
	_, err = db.Exec(`UPDATE participants
		SET phone_number = '+15550100', canonical_id = 'alice@example.com'
		WHERE id = 1`)
	require.NoError(err, "seed upgraded participant columns")
	_, err = db.Exec(`UPDATE conversations
		SET title = 'Thread 1', conversation_type = 'email_thread'`)
	require.NoError(err, "seed upgraded conversation columns")
	require.NoError(db.Close(), "close source db")

	_, err = CopySubset(srcDB, dstDir, 100, false)
	require.NoError(err, "CopySubset from upgraded column order")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination db")
	defer func() { _ = dstDB.Close() }()

	var systemRole, color sql.NullString
	require.NoError(dstDB.QueryRow(
		`SELECT system_role, color FROM labels WHERE id = 2`,
	).Scan(&systemRole, &color), "read copied label")
	assert.Equal("sent", systemRole.String, "labels.system_role")
	assert.Equal("#16a765", color.String, "labels.color")

	var phone, canonical, displayName, domain sql.NullString
	require.NoError(dstDB.QueryRow(
		`SELECT phone_number, canonical_id, display_name, domain
		 FROM participants WHERE id = 1`,
	).Scan(&phone, &canonical, &displayName, &domain),
		"read copied participant")
	assert.Equal("+15550100", phone.String, "participants.phone_number")
	assert.Equal("alice@example.com", canonical.String,
		"participants.canonical_id")
	assert.Equal("Alice", displayName.String, "participants.display_name")
	assert.Equal("example.com", domain.String, "participants.domain")

	var title, convType string
	require.NoError(dstDB.QueryRow(
		`SELECT title, conversation_type FROM conversations WHERE id = 1`,
	).Scan(&title, &convType), "read copied conversation")
	assert.Equal("Thread 1", title, "conversations.title")
	assert.Equal("email_thread", convType, "conversations.conversation_type")
}

// TestCopySubset_LegacyMessageRecipientsWithoutEnvelopeAddress covers a
// source archive from before message_recipients.email_address was added. The
// destination has the new column, so the copy must match columns by name and
// let the destination default the missing envelope snapshot to NULL.
func TestCopySubset_LegacyMessageRecipientsWithoutEnvelopeAddress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 2)
	dstDir := filepath.Join(t.TempDir(), "dst")

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err, "open source db")
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_message_recipients_envelope`,
		`DROP INDEX IF EXISTS idx_message_recipients_message`,
		`DROP INDEX IF EXISTS idx_message_recipients_participant`,
		`CREATE TABLE message_recipients_legacy (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
			recipient_type TEXT NOT NULL,
			display_name TEXT,
			UNIQUE(message_id, participant_id, recipient_type)
		)`,
		`INSERT INTO message_recipients_legacy
			(id, message_id, participant_id, recipient_type, display_name)
		 SELECT id, message_id, participant_id, recipient_type, display_name
		 FROM message_recipients`,
		`DROP TABLE message_recipients`,
		`ALTER TABLE message_recipients_legacy RENAME TO message_recipients`,
		`CREATE INDEX idx_message_recipients_message
			ON message_recipients(message_id)`,
		`CREATE INDEX idx_message_recipients_participant
			ON message_recipients(participant_id, recipient_type)`,
	} {
		_, err = db.Exec(stmt)
		require.NoError(err, "rebuild legacy message_recipients: %s", stmt)
	}
	require.NoError(db.Close(), "close source db")

	_, err = CopySubset(srcDB, dstDir, 100, false)
	require.NoError(err, "CopySubset from source without email_address")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open destination db")
	defer func() { _ = dstDB.Close() }()

	var count int64
	require.NoError(dstDB.QueryRow(
		`SELECT COUNT(*) FROM message_recipients WHERE email_address IS NULL`,
	).Scan(&count), "count legacy recipients without envelope snapshots")
	assert.Equal(int64(4), count)
}

func TestCopySubsetPreservesRelationshipsAndDecisionLedgerWithProfiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	alice, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	bob, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)

	// A user-defined type and an edge that uses it, plus a seeded-type edge,
	// so the copy exercises both catalog reconciliation and id remapping.
	mentor, err := source.CreateRelationshipTypeContext(ctx, RelationshipTypeInput{
		Slug: "mentor", ForwardLabel: "mentor", ReverseLabel: "mentee",
	})
	require.NoError(err)
	_, err = source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: alice.ID, TargetPersonID: bob.ID, TypeSlug: "mentor",
		Source: ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	_, err = source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: alice.ID, TargetPersonID: bob.ID, TypeSlug: "parent",
		Source: ProvenanceUser, Actor: "user",
	})
	require.NoError(err)

	// An automatically imported relationship that the user then deleted:
	// its accepted-with-cleared-edge tombstone must survive the copy.
	in := RelatedImport{PersonID: alice.ID, RawValue: bob.VCardUID, RawType: "friend",
		ValueKind: RelatedValueKindText, Source: ProvenanceVCardImport, Actor: "system"}
	resolved, err := source.ResolveRelatedValueContext(ctx, in)
	require.NoError(err)
	require.NotNil(resolved.Relationship)
	edge, err := source.GetPersonRelationshipContext(ctx, resolved.Relationship.ID)
	require.NoError(err)
	require.NoError(source.DeletePersonRelationshipContext(ctx, edge.ID, edge.Revision))
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{IncludeProfiles: true})
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	copiedMentor, err := destination.GetRelationshipTypeBySlugContext(ctx, "mentor")
	require.NoError(err)
	assert.Equal(mentor.UniversalID, copiedMentor.UniversalID)

	views, err := destination.ListPersonRelationshipsContext(ctx, alice.ID, PersonRelationshipListOptions{})
	require.NoError(err)
	slugs := make([]string, 0, len(views))
	for _, view := range views {
		slugs = append(slugs, view.Relationship.TypeSlug)
	}
	assert.ElementsMatch([]string{"mentor", "parent"}, slugs)

	// Re-importing the deleted occurrence into the subset must hit the
	// copied tombstone, not recreate the relationship.
	again, err := destination.ResolveRelatedValueContext(ctx, in)
	require.NoError(err)
	assert.Nil(again.Relationship)
	require.NotNil(again.Review)
	assert.Equal(RelationshipReviewAccepted, again.Review.Status)
	assert.Nil(again.Review.AcceptedRelationshipID)
}

func TestCopySubsetExcludesRelationshipsByDefault(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	alice, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	bob, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, err = source.CreateRelationshipTypeContext(ctx, RelationshipTypeInput{
		Slug: "mentor", ForwardLabel: "mentor", ReverseLabel: "mentee",
	})
	require.NoError(err)
	_, err = source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: alice.ID, TargetPersonID: bob.ID, TypeSlug: "mentor",
		Source: ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	views, err := destination.ListPersonRelationshipsContext(ctx, alice.ID, PersonRelationshipListOptions{})
	require.NoError(err)
	assert.Empty(views, "a shared subset must not copy relationships without the profiles opt-in")
	_, err = destination.GetRelationshipTypeBySlugContext(ctx, "mentor")
	require.ErrorIs(err, ErrRelationshipTypeNotFound)
}
