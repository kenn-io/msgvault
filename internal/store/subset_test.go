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

	"go.kenn.io/msgvault/internal/vcard"
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

type subsetPersonMergeFixture struct {
	mergeID, sourcePersonID int64
	absorbedUID             string
}

func seedSubsetPersonMerge(
	t *testing.T, sourcePath string, missingSplitParticipant bool,
) subsetPersonMergeFixture {
	t.Helper()
	require := require.New(t)
	ctx := context.Background()
	st, err := Open(sourcePath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	participantID := int64(3)
	if missingSplitParticipant {
		created, err := st.EnsureParticipant(
			"subset-merge-hidden@example.com", "Hidden", "example.com")
		require.NoError(err)
		participantID = created
	}
	_, err = st.LinkParticipants(2, participantID)
	require.NoError(err)
	survivor, _, err := st.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, err = st.AddPersonNameContext(ctx, absorbed.ID, PersonNameInput{
		NameKind: PersonNameFormatted, Formatted: new("Subset Absorbed"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	for personID, value := range map[int64]string{
		survivor.ID: "email", absorbed.ID: "chat",
	} {
		_, err = st.SetPersonAttributeValueContext(ctx, PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
			Value:  AttributeValue{Type: AttributeValueText, Text: &value},
			Source: ProvenanceUser,
		})
		require.NoError(err)
	}
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-person-merge", Actor: "test",
	})
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(ctx, PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         []int64{participantID},
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "subset-person-split", Actor: "test",
	})
	require.NoError(err)
	return subsetPersonMergeFixture{
		mergeID: merged.Merge.ID, sourcePersonID: split.SourcePerson.ID,
		absorbedUID: absorbed.VCardUID,
	}
}

func TestSubsetCompletePersonMergePacket(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	sourceDir := t.TempDir()
	destinationDir := filepath.Join(t.TempDir(), "subset")
	sourcePath := createTestSourceDB(t, sourceDir, 4)
	fixture := seedSubsetPersonMerge(t, sourcePath, false)
	source, err := Open(sourcePath)
	require.NoError(err)
	sourceIdentityRevision, err := source.IdentityRevision()
	require.NoError(err)
	require.NoError(source.Close())
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	destinationIdentityRevision, err := destination.IdentityRevision()
	require.NoError(err)
	assert.Positive(sourceIdentityRevision)
	assert.Equal(sourceIdentityRevision, destinationIdentityRevision)
	detail, err := destination.GetPersonMergeContext(ctx, fixture.mergeID)
	require.NoError(err)
	assert.Len(detail.Participants, 3)
	assert.NotEmpty(detail.Rows)
	assert.Len(detail.Splits, 1)
	assert.Len(detail.ReviewCandidates, 1)
	snapshot, err := destination.GetPersonMergeSnapshotContext(
		ctx, fixture.mergeID)
	require.NoError(err)
	assert.NotEmpty(snapshot.JSON)
	alias, err := destination.ResolveRetiredPersonUIDContext(
		context.Background(), fixture.absorbedUID)
	require.NoError(err)
	require.NotNil(alias.SurvivingPersonID)
	assert.Equal(fixture.sourcePersonID, *alias.SurvivingPersonID)
}

func TestSubsetIncludesFullySplitPersonMergePacket(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-closed-merge", Actor: "test",
	})
	require.NoError(err)
	split, err := source.SplitPersonMergeContext(ctx, PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "subset-close-merge", Actor: "test",
	})
	require.NoError(err)
	require.True(split.ExactReversal)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	copyResult, err := CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	assert.Equal(int64(1), copyResult.PersonMergePackets)
	assert.Zero(copyResult.OmittedPersonMergePackets)

	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	detail, err := destination.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.NoError(err)
	assert.Nil(detail.Merge.CurrentPersonID)
	assert.Len(detail.Splits, 1)
	_, err = destination.GetPersonContext(ctx, split.SourcePerson.ID)
	require.NoError(err)
	_, err = destination.GetPersonContext(ctx, split.NewPerson.ID)
	require.NoError(err)
}

func TestSubsetPersonMergePacketCanSplitAfterNewPersonCreation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-roundtrip-merge", Actor: "test",
	})
	require.NoError(err)
	historicalAbsorbedID := absorbed.ID
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	copyResult, err := CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	assert.Equal(int64(1), copyResult.PersonMergePackets)
	assert.Zero(copyResult.OmittedPersonMergePackets)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })

	unrelatedParticipant, err := destination.EnsureParticipant(
		"subset-roundtrip-unrelated@example.com", "Unrelated", "example.com")
	require.NoError(err)
	unrelated, created, err := destination.CreatePersonFromParticipantContext(
		ctx, unrelatedParticipant)
	require.NoError(err)
	require.True(created)
	assert.Greater(unrelated.ID, historicalAbsorbedID,
		"new profiles must not reuse IDs embedded in imported merge lineage")
	current, err := destination.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)
	split, err := destination.SplitPersonMergeContext(ctx, PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "subset-roundtrip-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Contains(split.NewPerson.ParticipantIDs, int64(2))
}

func TestSubsetPersonMergePacketReservesRestorableRowIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	other, _, err := source.CreatePersonFromParticipant(3)
	require.NoError(err)
	historical, err := source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: survivor.ID, TargetPersonID: absorbed.ID, TypeSlug: "friend",
		Source: ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	survivor, err = source.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = source.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-row-id-merge", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	copyResult, err := CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	assert.Equal(int64(1), copyResult.PersonMergePackets)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })

	current, err := destination.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)
	created, err := destination.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: current.ID, TargetPersonID: other.ID, TypeSlug: "friend",
		Source: ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	assert.Greater(created.ID, historical.ID,
		"new rows must not reuse IDs embedded in imported merge snapshots")
	current, err = destination.GetPersonContext(ctx, current.ID)
	require.NoError(err)
	split, err := destination.SplitPersonMergeContext(ctx, PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "subset-row-id-split", Actor: "test",
	})
	require.NoError(err)
	restored, err := destination.GetPersonRelationshipContext(ctx, historical.ID)
	require.NoError(err)
	assert.ElementsMatch([]int64{split.SourcePerson.ID, split.NewPerson.ID},
		[]int64{restored.SourcePersonID, restored.TargetPersonID})
}

func TestSubsetCorruptPersonMergeSnapshotIsReported(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-corrupt-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = source.DB().ExecContext(ctx, source.Rebind(
		`UPDATE person_merges SET snapshot_blob = ? WHERE id = ?`),
		[]byte("corrupt"), merged.Merge.ID)
	require.NoError(err)
	require.NoError(source.Close())

	_, err = CopySubsetWithOptions(sourcePath, filepath.Join(t.TempDir(), "subset"), 4,
		CopySubsetOptions{
			IncludeIdentity: true, IncludeProfiles: true,
			IncludeAttributes: true, IncludeVCardResources: true,
		})
	require.ErrorIs(err, ErrPersonMergeSnapshotCorrupt)
}

func TestSubsetPersonMergePacketWithAbsorbedTrackingIsComplete(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, err = source.SetPersonTrackingContext(ctx, absorbed.ID, true)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-absorbed-tracking", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	_, err = destination.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.NoError(err)
	tracking, err := destination.GetPersonTrackingContext(ctx, merged.Person.ID)
	require.NoError(err)
	require.True(tracking.Tracked)
}

func TestSubsetPersonMergePacketRebuildsDerivedActivity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(3)
	require.NoError(err)
	occurredAt := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	_, err = source.DB().ExecContext(ctx, source.Rebind(`INSERT INTO activity_events (
		message_id, ref_kind, source_id, channel, occurred_at, date_origin,
		date_precision, timezone, utc_offset_minutes, local_date, direction,
		owner_address, projected_last_modified, projected_identity_revision,
		projected_account_identity_revision
	) VALUES (?, 'message', 1, 'email', ?, 'sent_at', 'timestamp',
		'UTC', 0, '2024-01-01', 'inbound', 'private-owner@example.com', ?, 1, 1)`),
		1, occurredAt, occurredAt)
	require.NoError(err)
	_, err = source.DB().ExecContext(ctx, source.Rebind(`INSERT INTO activity_event_persons
		(message_id, person_id, role, evidence, local_date)
		VALUES (?, ?, 'sender', 'direct', '2024-01-01')`), 1, survivor.ID)
	require.NoError(err)
	_, err = source.DB().ExecContext(ctx, source.Rebind(`INSERT INTO person_contact_state (
		person_id, first_contact_message_id, last_contact_message_id,
		last_contact_owner, interaction_count
	) VALUES (?, 1, 1, 'private-owner@example.com', 1)`), survivor.ID)
	require.NoError(err)

	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-derived-activity-merge", Actor: "test",
	})
	require.NoError(err)
	var snapshotBlob []byte
	var snapshotSHA256 string
	require.NoError(source.DB().QueryRowContext(ctx, source.Rebind(`SELECT
		snapshot_blob, snapshot_sha256 FROM person_merges WHERE id = ?`),
		merged.Merge.ID).Scan(&snapshotBlob, &snapshotSHA256))
	snapshot, err := decodePersonMergeSnapshot(snapshotBlob, snapshotSHA256)
	require.NoError(err)
	snapshotTables := make([]string, 0, len(snapshot.Rows))
	for _, row := range snapshot.Rows {
		snapshotTables = append(snapshotTables, row.TableName)
	}
	assert.NotContains(snapshotTables, "activity_event_persons")
	assert.NotContains(snapshotTables, "person_contact_state")
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 1, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	_, err = destination.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)
	_, err = destination.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.NoError(err)
}

func TestSubsetPersonMergePacketWithAbsorbedSplitResultIsOmitted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	first := seedSubsetPersonMerge(t, sourcePath, false)
	source, err := Open(sourcePath)
	require.NoError(err)
	detail, err := source.GetPersonMergeContext(ctx, first.mergeID)
	require.NoError(err)
	require.Len(detail.Splits, 1)
	survivor, err := source.GetPersonContext(ctx, first.sourcePersonID)
	require.NoError(err)
	absorbed, err := source.GetPersonContext(ctx, detail.Splits[0].NewPersonID)
	require.NoError(err)
	_, err = source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-absorbed-split-result", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	_, err = destination.GetPersonMergeContext(ctx, first.mergeID)
	require.ErrorIs(err, ErrPersonMergeNotFound)
	var danglingSplits int
	require.NoError(destination.DB().QueryRowContext(ctx, `SELECT COUNT(*)
		FROM person_splits split_record
		LEFT JOIN persons source_person ON source_person.id = split_record.source_person_id
		LEFT JOIN persons new_person ON new_person.id = split_record.new_person_id
		WHERE source_person.id IS NULL OR new_person.id IS NULL`).Scan(&danglingSplits))
	assert.Zero(danglingSplits)
}

func TestSubsetPersonMergePacketWithRemappedDefinitionIsOmitted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	definitionInput := subsetPersonDefinition("merge_remapped_definition")
	definition, err := source.CreateAttributeDefinitionContext(ctx, definitionInput)
	require.NoError(err)
	_, err = source.DB().ExecContext(ctx,
		`UPDATE attribute_definitions SET id = 4242 WHERE id = ?`, definition.ID)
	require.NoError(err)
	for personID, value := range map[int64]string{
		survivor.ID: "survivor", absorbed.ID: "absorbed",
	} {
		_, err = source.SetPersonAttributeValueContext(ctx, PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: definitionInput.Slug,
			Value:  AttributeValue{Type: AttributeValueText, Text: &value},
			Source: ProvenanceUser,
		})
		require.NoError(err)
	}
	survivor, err = source.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = source.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-remapped-definition", Actor: "test",
	})
	require.NoError(err)
	require.Len(merged.ReviewCandidates, 1)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	var mergeCount, candidateCount int
	require.NoError(destination.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM person_merges`).Scan(&mergeCount))
	require.NoError(destination.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM person_merge_review_candidates`).Scan(&candidateCount))
	assert.Zero(mergeCount)
	assert.Zero(candidateCount)
	copiedDefinition, err := destination.GetAttributeDefinitionBySlugContext(
		ctx, AttributeObjectPerson, definitionInput.Slug)
	require.NoError(err)
	assert.NotEqual(int64(4242), copiedDefinition.ID)
}

func TestSubsetPersonMergePacketWithRemappedRelationshipTypeIsOmitted(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	other, _, err := source.CreatePersonFromParticipant(3)
	require.NoError(err)
	relationshipType, err := source.CreateRelationshipTypeContext(ctx,
		RelationshipTypeInput{
			Slug: "subset-remapped-relationship", ForwardLabel: "knows",
			ReverseLabel: "known by",
		})
	require.NoError(err)
	_, err = source.DB().ExecContext(ctx,
		`UPDATE relationship_types SET id = 4242 WHERE id = ?`, relationshipType.ID)
	require.NoError(err)
	_, err = source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: absorbed.ID, TargetPersonID: other.ID,
		TypeSlug: relationshipType.Slug, Source: ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	survivor, err = source.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = source.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-remapped-relationship", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	copiedType, err := destination.GetRelationshipTypeBySlugContext(ctx, relationshipType.Slug)
	require.NoError(err)
	require.NotEqual(int64(4242), copiedType.ID)
	_, err = destination.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.ErrorIs(err, ErrPersonMergeNotFound)
}

func TestSubsetPersonMergePacketWithRemappedOrganizationDefinitionIsOmitted(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	organization, err := source.CreateOrganizationContext(ctx, OrganizationInput{
		Name: "Remapped Definition Org", Kind: OrganizationKindCompany,
	})
	require.NoError(err)
	definitionInput := subsetPersonDefinition("merge_remapped_org_definition")
	definitionInput.ObjectType = AttributeObjectOrganization
	definitionInput.ValueType = AttributeValueRecordReference
	definitionInput.FieldType = AttributeFieldPerson
	definitionInput.RecordTarget = new("person")
	definition, err := source.CreateAttributeDefinitionContext(ctx, definitionInput)
	require.NoError(err)
	_, err = source.DB().ExecContext(ctx,
		`UPDATE attribute_definitions SET id = 4242 WHERE id = ?`, definition.ID)
	require.NoError(err)
	_, err = source.SetOrganizationAttributeValueContext(ctx, OrganizationAttributeValueInput{
		OrganizationID: organization.ID, DefinitionSlug: definitionInput.Slug,
		Value: AttributeValue{
			Type: AttributeValueRecordReference, RecordType: new("person"),
			RecordID: &absorbed.ID,
		},
		Source: ProvenanceUser,
	})
	require.NoError(err)
	_, err = source.AddEmploymentContext(ctx, EmploymentInput{
		PersonID: survivor.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: ProvenanceUser,
	})
	require.NoError(err)
	survivor, err = source.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = source.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-remapped-org-definition", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	copiedDefinition, err := destination.GetAttributeDefinitionBySlugContext(
		ctx, AttributeObjectOrganization, definitionInput.Slug)
	require.NoError(err)
	require.NotEqual(int64(4242), copiedDefinition.ID)
	_, err = destination.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.ErrorIs(err, ErrPersonMergeNotFound)
}

func TestSubsetPersonMergePacketWithRemappedServiceIsOmitted(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	var sourceServiceID int64
	require.NoError(source.DB().QueryRowContext(ctx,
		`SELECT id FROM communication_services WHERE slug = 'whatsapp'`).Scan(&sourceServiceID))
	_, err = source.DB().ExecContext(ctx, `UPDATE communication_services
		SET slug = 'subset-merge-custom-chat', display_label = 'Subset Merge Custom Chat',
		    normalization = 'lower', is_system = FALSE
		WHERE id = ?`, sourceServiceID)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	serviceSlug := "subset-merge-custom-chat"
	_, err = source.AddPersonContactPointContext(ctx, absorbed.ID, PersonContactPointInput{
		AddressKind: ContactAddressUsername, ServiceSlug: &serviceSlug,
		OriginalValue: "absorbed-user",
		Envelope:      ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	survivor, err = source.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = source.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-remapped-service", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	copiedService, err := destination.ResolveCommunicationServiceContext(ctx, serviceSlug)
	require.NoError(err)
	require.NotEqual(sourceServiceID, copiedService.ID)
	_, err = destination.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.ErrorIs(err, ErrPersonMergeNotFound)
}

func TestSubsetIncompletePersonMergePacketIsOmitted(t *testing.T) {
	require := require.New(t)
	sourceDir := t.TempDir()
	destinationDir := filepath.Join(t.TempDir(), "subset")
	sourcePath := createTestSourceDB(t, sourceDir, 4)
	fixture := seedSubsetPersonMerge(t, sourcePath, true)
	_, err := CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	_, err = destination.GetPersonContext(context.Background(), fixture.sourcePersonID)
	require.NoError(err)
	for _, table := range []string{
		"person_merges", "person_merge_participants", "person_merge_rows",
		"person_merge_review_candidates", "person_splits",
	} {
		var count int
		require.NoError(destination.DB().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM `+table).Scan(&count))
		assert.Zero(t, count, table)
	}
	_, err = destination.ResolveRetiredPersonUIDContext(
		context.Background(), fixture.absorbedUID)
	assert.ErrorIs(t, err, ErrPersonUIDAliasNotFound)
}

func TestSubsetMergeAliasIsOmittedWithoutAttributes(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	fixture := seedSubsetPersonMerge(t, sourcePath, false)
	destinationDir := filepath.Join(t.TempDir(), "subset")
	result, err := CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	assert.Zero(t, result.PersonMergePackets)
	assert.Equal(t, int64(1), result.OmittedPersonMergePackets)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })

	_, err = destination.GetPersonMergeContext(ctx, fixture.mergeID)
	require.ErrorIs(err, ErrPersonMergeNotFound)
	_, err = destination.ResolveRetiredPersonUIDContext(ctx, fixture.absorbedUID)
	require.ErrorIs(err, ErrPersonUIDAliasNotFound,
		"a merge-created alias must not outlive its omitted lineage packet")
}

func TestSubsetPersonMergePacketWithMissingRelationshipDependencyIsOmitted(t *testing.T) {
	require := require.New(t)
	sourceDir := t.TempDir()
	destinationDir := filepath.Join(t.TempDir(), "subset")
	sourcePath := createTestSourceDB(t, sourceDir, 4)
	ctx := context.Background()
	st, err := Open(sourcePath)
	require.NoError(err)
	hiddenParticipant, err := st.EnsureParticipant(
		"subset-merge-outside@example.com", "Outside", "example.com")
	require.NoError(err)
	hidden, _, err := st.CreatePersonFromParticipant(hiddenParticipant)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, err = st.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: absorbed.ID, TargetPersonID: hidden.ID, TypeSlug: "friend",
		Source: ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	survivor, _, err := st.CreatePersonFromParticipant(1)
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-missing-relationship", Actor: "test",
	})
	require.NoError(err)
	absorbedUID := absorbed.VCardUID
	require.NoError(st.Close())

	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	_, err = destination.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.ErrorIs(err, ErrPersonMergeNotFound)
	_, err = destination.ResolveRetiredPersonUIDContext(ctx, absorbedUID)
	require.ErrorIs(err, ErrPersonUIDAliasNotFound)
}

func TestSubsetPersonMergePacketWithUnchangedHiddenRelationshipIsOmitted(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	hiddenParticipant, err := source.EnsureParticipant(
		"subset-unchanged-hidden@example.com", "Hidden", "example.com")
	require.NoError(err)
	hidden, _, err := source.CreatePersonFromParticipant(hiddenParticipant)
	require.NoError(err)
	survivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, err = source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: survivor.ID, TargetPersonID: hidden.ID, TypeSlug: "acquaintance",
		Source: ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	survivor, err = source.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = source.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-unchanged-hidden-relationship", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	_, err = destination.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.ErrorIs(err, ErrPersonMergeNotFound)
}

func TestSubsetPersonMergePacketWithMissingRelationshipReviewDependencyIsOmitted(t *testing.T) {
	for _, dependency := range []string{"matched_person", "accepted_relationship"} {
		t.Run(dependency, func(t *testing.T) {
			require := require.New(t)
			ctx := context.Background()
			sourcePath := createTestSourceDB(t, t.TempDir(), 4)
			source, err := Open(sourcePath)
			require.NoError(err)
			hiddenParticipantA, err := source.EnsureParticipant(
				"subset-review-hidden-a@example.com", "Hidden A", "example.com")
			require.NoError(err)
			hiddenParticipantB, err := source.EnsureParticipant(
				"subset-review-hidden-b@example.com", "Hidden B", "example.com")
			require.NoError(err)
			hiddenA, _, err := source.CreatePersonFromParticipant(hiddenParticipantA)
			require.NoError(err)
			hiddenB, _, err := source.CreatePersonFromParticipant(hiddenParticipantB)
			require.NoError(err)
			survivor, _, err := source.CreatePersonFromParticipant(1)
			require.NoError(err)
			absorbed, _, err := source.CreatePersonFromParticipant(2)
			require.NoError(err)

			var matchedPersonID, acceptedRelationshipID sql.NullInt64
			switch dependency {
			case "matched_person":
				matchedPersonID = sql.NullInt64{Int64: hiddenA.ID, Valid: true}
			case "accepted_relationship":
				edge, err := source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
					SourcePersonID: hiddenA.ID, TargetPersonID: hiddenB.ID,
					TypeSlug: "friend", Source: ProvenanceUser, Actor: "test",
				})
				require.NoError(err)
				acceptedRelationshipID = sql.NullInt64{Int64: edge.ID, Valid: true}
			}
			_, err = source.DB().ExecContext(ctx, `INSERT INTO person_relationship_reviews (
				person_id, raw_related_value, raw_related_type, value_kind,
				matched_person_id, accepted_relationship_id, source
			) VALUES (?, ?, 'friend', 'text', ?, ?, 'system')`,
				absorbed.ID, dependency, matchedPersonID, acceptedRelationshipID)
			require.NoError(err)
			survivor, err = source.GetPersonContext(ctx, survivor.ID)
			require.NoError(err)
			absorbed, err = source.GetPersonContext(ctx, absorbed.ID)
			require.NoError(err)
			merged, err := source.MergePersonsContext(ctx, PersonMergeRequest{
				SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
				ExpectedSurvivorRevision: survivor.Revision,
				ExpectedAbsorbedRevision: absorbed.Revision,
				IdempotencyKey:           "subset-review-" + dependency, Actor: "test",
			})
			require.NoError(err)
			require.NoError(source.Close())

			destinationDir := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
				IncludeIdentity: true, IncludeProfiles: true,
				IncludeAttributes: true, IncludeVCardResources: true,
			})
			require.NoError(err)
			destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(destination.Close()) })
			_, err = destination.GetPersonMergeContext(ctx, merged.Merge.ID)
			require.ErrorIs(err, ErrPersonMergeNotFound)
		})
	}
}

func TestSubsetPersonMergePacketWithOmittedPriorMergeIsOmitted(t *testing.T) {
	require := require.New(t)
	sourceDir := t.TempDir()
	destinationDir := filepath.Join(t.TempDir(), "subset")
	sourcePath := createTestSourceDB(t, sourceDir, 4)
	first := seedSubsetPersonMerge(t, sourcePath, true)
	ctx := context.Background()
	st, err := Open(sourcePath)
	require.NoError(err)
	current, err := st.GetPersonContext(ctx, first.sourcePersonID)
	require.NoError(err)
	third, _, err := st.CreatePersonFromParticipant(3)
	require.NoError(err)
	current, err = st.GetPersonContext(ctx, current.ID)
	require.NoError(err)
	third, err = st.GetPersonContext(ctx, third.ID)
	require.NoError(err)
	second, err := st.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: third.ID, AbsorbedID: current.ID,
		ExpectedSurvivorRevision: third.Revision,
		ExpectedAbsorbedRevision: current.Revision,
		IdempotencyKey:           "subset-dependent-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = st.SplitPersonMergeContext(ctx, PersonSplitRequest{
		SourcePersonID: second.Person.ID, MergeID: second.Merge.ID,
		ParticipantIDs:         current.ParticipantIDs,
		ExpectedSourceRevision: second.Person.Revision,
		IdempotencyKey:         "subset-dependent-split", Actor: "test",
	})
	require.NoError(err)
	require.NoError(st.Close())

	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	for _, mergeID := range []int64{first.mergeID, second.Merge.ID} {
		_, err = destination.GetPersonMergeContext(ctx, mergeID)
		assert.ErrorIs(t, err, ErrPersonMergeNotFound)
	}
}

func TestSubsetPersonMergePacketWithOmittedSplitOwnerIsOmitted(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	source, err := Open(sourcePath)
	require.NoError(err)
	extraAbsorbedParticipant, err := source.EnsureParticipant(
		"subset-split-extra@example.com", "Extra", "example.com")
	require.NoError(err)
	_, err = source.DB().ExecContext(ctx, `INSERT INTO message_recipients
		(message_id, participant_id, recipient_type) VALUES (1, ?, 'cc')`,
		extraAbsorbedParticipant)
	require.NoError(err)
	_, err = source.LinkParticipants(3, extraAbsorbedParticipant)
	require.NoError(err)
	hiddenParticipant, err := source.EnsureParticipant(
		"subset-split-hidden@example.com", "Hidden", "example.com")
	require.NoError(err)
	hidden, _, err := source.CreatePersonFromParticipant(hiddenParticipant)
	require.NoError(err)
	outerSurvivor, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	innerSurvivor, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	innerAbsorbed, _, err := source.CreatePersonFromParticipant(3)
	require.NoError(err)
	_, err = source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: outerSurvivor.ID, TargetPersonID: hidden.ID,
		TypeSlug: "friend", Source: ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	outerSurvivor, err = source.GetPersonContext(ctx, outerSurvivor.ID)
	require.NoError(err)
	innerMerge, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: innerSurvivor.ID, AbsorbedID: innerAbsorbed.ID,
		ExpectedSurvivorRevision: innerSurvivor.Revision,
		ExpectedAbsorbedRevision: innerAbsorbed.Revision,
		IdempotencyKey:           "subset-split-inner-merge", Actor: "test",
	})
	require.NoError(err)
	outerMerge, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: outerSurvivor.ID, AbsorbedID: innerMerge.Person.ID,
		ExpectedSurvivorRevision: outerSurvivor.Revision,
		ExpectedAbsorbedRevision: innerMerge.Person.Revision,
		IdempotencyKey:           "subset-split-outer-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = source.SplitPersonMergeContext(ctx, PersonSplitRequest{
		SourcePersonID: outerMerge.Person.ID, MergeID: outerMerge.Merge.ID,
		ParticipantIDs:         innerAbsorbed.ParticipantIDs,
		ExpectedSourceRevision: outerMerge.Person.Revision,
		IdempotencyKey:         "subset-split-outer-partial", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	result, err := CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	assert.Equal(t, int64(2), result.OmittedPersonMergePackets)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	for _, mergeID := range []int64{innerMerge.Merge.ID, outerMerge.Merge.ID} {
		_, err = destination.GetPersonMergeContext(ctx, mergeID)
		require.ErrorIs(err, ErrPersonMergeNotFound)
	}
}

func TestSubsetPersonMergePacketsPruneAliasDependenciesToFixedPoint(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	sourcePath := createTestSourceDB(t, t.TempDir(), 4)
	first := seedSubsetPersonMerge(t, sourcePath, true)
	source, err := Open(sourcePath)
	require.NoError(err)
	survivor, err := source.GetPersonContext(ctx, first.sourcePersonID)
	require.NoError(err)
	absorbed, _, err := source.CreatePersonFromParticipant(3)
	require.NoError(err)
	absorbedUID := absorbed.VCardUID
	second, err := source.MergePersonsContext(ctx, PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "subset-alias-dependent-merge", Actor: "test",
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubsetWithOptions(sourcePath, destinationDir, 4, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true,
		IncludeAttributes: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(destination.Close()) })
	for _, mergeID := range []int64{first.mergeID, second.Merge.ID} {
		_, err = destination.GetPersonMergeContext(ctx, mergeID)
		require.ErrorIs(err, ErrPersonMergeNotFound)
	}
	for _, retiredUID := range []string{first.absorbedUID, absorbedUID} {
		_, err = destination.ResolveRetiredPersonUIDContext(ctx, retiredUID)
		require.ErrorIs(err, ErrPersonUIDAliasNotFound)
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

func seedAcceptedSubsetParticipantLink(t *testing.T, srcDB string) int64 {
	t.Helper()
	require := require.New(t)
	st, err := Open(srcDB)
	require.NoError(err, "open source store")
	defer func() { _ = st.Close() }()

	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		context.Background(), IdentityMatchCandidateInput{
			LeftKind:        IdentityMatchParticipant,
			LeftID:          2,
			RightKind:       IdentityMatchParticipant,
			RightID:         3,
			Basis:           IdentityMatchStableProviderID,
			NormalizedValue: new("subset-provider-id"),
			State:           IdentityMatchStateCandidate,
			Source:          ProvenanceArchiveObservation,
		},
	)
	require.NoError(err, "create identity match candidate")
	require.True(created, "identity match candidate must be new")
	accepted, _, err := st.AcceptIdentityMatchCandidateContext(
		context.Background(), candidate.ID, "system", nil,
	)
	require.NoError(err, "accept identity match candidate")
	require.Equal(IdentityMatchStateAccepted, accepted.State)
	return accepted.ID
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
		VALUES ('subset-extraction', 0, 'page', 'private extracted evidence', ?, 26);
		INSERT INTO person_inference_profiles
			(fingerprint, provider_kind, endpoint, model, api_key_env,
			 retention_posture, training_posture, allowed_sources,
			 source_since, policy_json)
		VALUES (?, 'openai_compatible', 'https://api.example.test/v1',
		        'gpt-test', 'TEST_KEY', 'zero_retention', 'no_training',
		        '["conversation_text"]', '2025-01-01', '{}');
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'cli');
		INSERT INTO person_semantic_embedding_profiles
			(fingerprint, purpose, destination, api_format, model, api_key_env,
			 retention_posture, training_posture, renderer_policy,
			 disclosed_field_classes, corpus_scope, policy_json)
		VALUES (?, 'semantic_person_embeddings', 'https://embedding.example.test/v1/embeddings',
		        'openai', 'synthetic-model', 'TEST_KEY', 'zero_data_retention',
		        'no_training', 'person-semantic-v1', '["person_display_name"]',
		        'all_durable_people', '{}');
		INSERT INTO person_semantic_embedding_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'cli')`,
		fingerprint, fingerprint, strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64),
		strings.Repeat("e", 64), strings.Repeat("e", 64),
		strings.Repeat("f", 64), strings.Repeat("f", 64),
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
		"person_inference_profiles", "person_inference_consents",
		"person_semantic_embedding_profiles", "person_semantic_embedding_consents",
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
	// A pre-attribution archive also predates the activity queue trigger, which
	// names source_is_from_me and would otherwise block the DROP COLUMN; the
	// upgrade below reinstalls it through its own migration.
	_, err = st.DB().Exec(`
		DROP TRIGGER trg_activity_queue_messages_update;
		ALTER TABLE messages DROP COLUMN identity_is_from_me;
		ALTER TABLE messages DROP COLUMN source_is_from_me;
		DELETE FROM applied_migrations
		WHERE name IN ('message_attribution_provenance_v3',
		               'activity_projection_triggers_v4');
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

func TestCopySubset_UpgradedAttachmentColumnOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 1)
	dstDir := filepath.Join(t.TempDir(), "dst")

	legacy, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = legacy.Exec(`
		DROP TRIGGER IF EXISTS trg_attachment_message_live_change;
		DROP TABLE attachments;
		CREATE TABLE attachments (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			filename TEXT,
			mime_type TEXT,
			size INTEGER,
			content_hash TEXT,
			storage_path TEXT NOT NULL,
			media_type TEXT,
			width INTEGER,
			height INTEGER,
			duration_ms INTEGER,
			thumbnail_hash TEXT,
			thumbnail_path TEXT,
			source_attachment_id TEXT,
			attachment_metadata JSON,
			encryption_version INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	require.NoError(err, "restore pre-occurrence attachment schema")
	_, err = legacy.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationAttachmentOccurrenceUnique)
	require.NoError(err, "reopen attachment occurrence migration")
	require.NoError(legacy.Close())

	upgraded, err := Open(srcDB)
	require.NoError(err)
	require.NoError(upgraded.InitSchema(), "upgrade attachment schema")
	hash := strings.Repeat("ab", 32)
	require.NoError(upgraded.UpsertAttachmentRecord(t.Context(), 1, AttachmentWrite{
		Filename: "evidence.pdf", MIMEType: "application/pdf", Size: 1234,
		ContentHash: hash, StoragePath: hash[:2] + "/" + hash,
		SourceAttachmentID: "teams:link:a1", Metadata: `{"origin":"upgraded"}`,
		Role: AttachmentRoleStandalone, RoleSource: AttachmentRoleSourceProviderExplicit,
		SourcePartKey: "teams:link:a1", ContentID: "evidence@example.invalid",
	}))
	_, err = upgraded.DB().Exec(`UPDATE attachments SET encryption_version = 7 WHERE message_id = 1`)
	require.NoError(err)
	require.NoError(upgraded.Close())

	result, err := CopySubset(srcDB, dstDir, 1, false)
	require.NoError(err, "copy subset from upgraded attachment schema")
	assert.Equal(int64(1), result.Messages)

	destination, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = destination.Close() }()
	var (
		filename, mimeType, contentHash, storagePath   string
		sourceAttachmentID, metadata, role, roleSource string
		sourcePartKey, contentID                       string
		size, encryptionVersion                        int64
	)
	require.NoError(destination.QueryRow(`
		SELECT filename, mime_type, size, content_hash, storage_path,
		       source_attachment_id, attachment_metadata, attachment_role,
		       role_source, source_part_key, content_id, encryption_version
		FROM attachments WHERE message_id = 1
	`).Scan(
		&filename, &mimeType, &size, &contentHash, &storagePath,
		&sourceAttachmentID, &metadata, &role, &roleSource,
		&sourcePartKey, &contentID, &encryptionVersion,
	))
	assert.Equal("evidence.pdf", filename)
	assert.Equal("application/pdf", mimeType)
	assert.Equal(int64(1234), size)
	assert.Equal(hash, contentHash)
	assert.Equal(hash[:2]+"/"+hash, storagePath)
	assert.Equal("teams:link:a1", sourceAttachmentID)
	assert.JSONEq(`{"origin":"upgraded"}`, metadata)
	assert.Equal(string(AttachmentRoleStandalone), role)
	assert.Equal(string(AttachmentRoleSourceProviderExplicit), roleSource)
	assert.Equal("teams:link:a1", sourcePartKey)
	assert.Equal("evidence@example.invalid", contentID)
	assert.Equal(int64(7), encryptionVersion)
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
	_, err = source.db.Exec(`INSERT INTO communication_service_discoveries (
		service_id, provider, discovery_kind
	) VALUES (?, 'beeper', 'routing_fallback')`, sourceServiceID)
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
	discovered, err := destination.IsCommunicationServiceDiscoveredContext(
		ctx, copiedService.ID, "beeper", "routing_fallback")
	require.NoError(err)
	assert.True(discovered, "subset copies must preserve importer provenance")
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

func TestCopySubsetPreservesIdentityMatchSourceSupport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	supportSource, err := source.GetOrCreateSource("beeper", "support-only")
	require.NoError(err)
	candidate, created, err := source.UpsertIdentityMatchCandidateContext(
		ctx, IdentityMatchCandidateInput{
			LeftKind: IdentityMatchParticipant, LeftID: 2,
			RightKind: IdentityMatchParticipant, RightID: 3,
			Basis:           IdentityMatchStableProviderID,
			NormalizedValue: new("subset-supported-provider"),
			State:           IdentityMatchStateCandidate,
			Source:          ProvenanceArchiveObservation,
			SourceID:        &supportSource.ID,
		},
	)
	require.NoError(err)
	require.True(created)
	evidence, err := source.AddIdentityMatchEvidenceContext(
		ctx, candidate.ID, IdentityMatchEvidenceInput{
			EvidenceKind: "stable_provider_id",
			Detail:       new("subset-supported-provider"),
			Source:       ProvenanceArchiveObservation,
			SourceID:     &supportSource.ID,
		},
	)
	require.NoError(err)
	for participantID, originalValue := range map[int64]string{
		2: "bob@subset.example",
		3: "charlie@subset.example",
	} {
		_, err = source.RecordContactObservationContext(
			ctx, participantID, ParticipantContactObservationInput{
				SourceID:       &supportSource.ID,
				AddressKind:    ContactAddressEmail,
				ProviderUserID: new("subset-supported-provider"),
				OriginalValue:  originalValue,
				Envelope:       ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
			},
		)
		require.NoError(err)
	}
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{
		IncludeProfiles: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	copiedSource, err := destination.GetSourceByID(supportSource.ID)
	require.NoError(err, "support-only source must be copied before support rows")
	assert.Equal("support-only", copiedSource.Identifier)
	var candidateSupport, evidenceSupport int
	require.NoError(destination.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_candidate_sources
		WHERE candidate_id = ? AND source_id = ?`, candidate.ID, supportSource.ID).
		Scan(&candidateSupport))
	require.NoError(destination.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_evidence_sources
		WHERE evidence_id = ? AND source_id = ?`, evidence.ID, supportSource.ID).
		Scan(&evidenceSupport))
	assert.Equal(1, candidateSupport)
	assert.Equal(1, evidenceSupport)

	// Removing the selected message source must not erase profile matches that
	// are still supported by the copied support-only source.
	require.NoError(destination.RemoveSource(1))
	reloaded, err := destination.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(IdentityMatchStateCandidate, reloaded.State)
	require.Len(reloaded.Evidence, 1)
	assert.Equal(evidence.ID, reloaded.Evidence[0].ID)
}

func TestCopySubsetOmitsConservativeIdentitySourceMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	supportSource, err := source.GetOrCreateSource("beeper", "exact-support")
	require.NoError(err)
	legacySource, err := source.GetOrCreateSource("beeper", "legacy-private")
	require.NoError(err)
	_, err = source.DB().Exec(
		`UPDATE sources SET sync_config = ? WHERE id = ?`,
		`{"account_email":"excluded@example.invalid","sync_token":"synthetic"}`,
		legacySource.ID,
	)
	require.NoError(err)
	candidate, _, err := source.UpsertIdentityMatchCandidateContext(
		ctx, IdentityMatchCandidateInput{
			LeftKind: IdentityMatchParticipant, LeftID: 2,
			RightKind: IdentityMatchParticipant, RightID: 3,
			Basis:           IdentityMatchStableProviderID,
			NormalizedValue: new("subset-conservative-provider"),
			State:           IdentityMatchStateCandidate,
			Source:          ProvenanceArchiveObservation,
			SourceID:        &supportSource.ID,
		},
	)
	require.NoError(err)
	evidence, err := source.AddIdentityMatchEvidenceContext(
		ctx, candidate.ID, IdentityMatchEvidenceInput{
			EvidenceKind: "stable_provider_id",
			Detail:       new("subset-conservative-provider"),
			Source:       ProvenanceArchiveObservation,
			SourceID:     &supportSource.ID,
		},
	)
	require.NoError(err)
	_, err = source.DB().Exec(
		`INSERT INTO identity_match_candidate_sources
			(candidate_id, source_id, is_conservative) VALUES (?, ?, TRUE)`,
		candidate.ID, legacySource.ID,
	)
	require.NoError(err)
	_, err = source.DB().Exec(
		`INSERT INTO identity_match_evidence_sources
			(evidence_id, source_id, is_conservative) VALUES (?, ?, TRUE)`,
		evidence.ID, legacySource.ID,
	)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{
		IncludeProfiles: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	var leakedSource, leakedAccountMetadata int
	require.NoError(destination.DB().QueryRow(
		destination.Rebind(`SELECT COUNT(*) FROM sources WHERE id = ?`),
		legacySource.ID,
	).Scan(&leakedSource))
	require.NoError(destination.DB().QueryRow(
		`SELECT COUNT(*) FROM sources WHERE sync_config LIKE '%excluded@example.invalid%'`,
	).Scan(&leakedAccountMetadata))
	assert.Zero(leakedSource,
		"conservative legacy support must not copy the excluded source row")
	assert.Zero(leakedAccountMetadata,
		"excluded source account metadata must remain absent from the subset")

	var copiedLegacyCandidateSupport, copiedLegacyEvidenceSupport int
	require.NoError(destination.DB().QueryRow(destination.Rebind(`
		SELECT COUNT(*) FROM identity_match_candidate_sources
		WHERE candidate_id = ? AND source_id = ?
	`), candidate.ID, legacySource.ID).Scan(&copiedLegacyCandidateSupport))
	require.NoError(destination.DB().QueryRow(destination.Rebind(`
		SELECT COUNT(*) FROM identity_match_evidence_sources
		WHERE evidence_id = ? AND source_id = ?
	`), evidence.ID, legacySource.ID).Scan(&copiedLegacyEvidenceSupport))
	assert.Zero(copiedLegacyCandidateSupport,
		"conservative candidate support must not become an export dependency")
	assert.Zero(copiedLegacyEvidenceSupport,
		"conservative evidence support must not become an export dependency")
}

func TestCopySubsetPreservesConservativeSupportForIncludedSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	unrelatedSource, err := source.GetOrCreateSource("beeper", "unrelated-exact-support")
	require.NoError(err)
	selectedSourceID := int64(1)
	normalized := "subset-upgraded-provider"
	candidate, _, err := source.UpsertIdentityMatchCandidateContext(
		ctx, IdentityMatchCandidateInput{
			LeftKind: IdentityMatchParticipant, LeftID: 2,
			RightKind: IdentityMatchParticipant, RightID: 3,
			Basis:           IdentityMatchStableProviderID,
			NormalizedValue: &normalized,
			State:           IdentityMatchStateCandidate,
			Source:          ProvenanceArchiveObservation,
			SourceID:        &unrelatedSource.ID,
		},
	)
	require.NoError(err)
	evidence, err := source.AddIdentityMatchEvidenceContext(
		ctx, candidate.ID, IdentityMatchEvidenceInput{
			EvidenceKind: "stable_provider_id", Detail: &normalized,
			Source: ProvenanceArchiveObservation, SourceID: &unrelatedSource.ID,
		},
	)
	require.NoError(err)
	for participantID, originalValue := range map[int64]string{
		2: "bob@upgraded-subset.example",
		3: "charlie@upgraded-subset.example",
	} {
		_, err = source.RecordContactObservationContext(
			ctx, participantID, ParticipantContactObservationInput{
				SourceID:       &selectedSourceID,
				AddressKind:    ContactAddressEmail,
				ProviderUserID: &normalized,
				OriginalValue:  originalValue,
				Envelope:       ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
			},
		)
		require.NoError(err)
	}
	_, _, err = source.AcceptIdentityMatchCandidateContext(
		ctx, candidate.ID, string(ProvenanceSystem), nil,
	)
	require.NoError(err)
	_, err = source.DB().Exec(
		`INSERT INTO identity_match_candidate_sources
			(candidate_id, source_id, is_conservative) VALUES (?, ?, TRUE)`,
		candidate.ID, selectedSourceID,
	)
	require.NoError(err)
	_, err = source.DB().Exec(
		`INSERT INTO identity_match_evidence_sources
			(evidence_id, source_id, is_conservative) VALUES (?, ?, TRUE)`,
		evidence.ID, selectedSourceID,
	)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{
		IncludeProfiles: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	var candidateSupport, evidenceSupport int
	require.NoError(destination.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_candidate_sources
		WHERE candidate_id = ? AND source_id = ? AND is_conservative = TRUE`,
		candidate.ID, selectedSourceID).Scan(&candidateSupport))
	require.NoError(destination.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_evidence_sources
		WHERE evidence_id = ? AND source_id = ? AND is_conservative = TRUE`,
		evidence.ID, selectedSourceID).Scan(&evidenceSupport))
	assert.Equal(1, candidateSupport)
	assert.Equal(1, evidenceSupport)

	require.NoError(destination.RemoveSource(unrelatedSource.ID))
	reloaded, err := destination.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(IdentityMatchStateAccepted, reloaded.State)
	require.Len(reloaded.Evidence, 1)
	assert.Equal(evidence.ID, reloaded.Evidence[0].ID)
	var linkCount int
	require.NoError(destination.DB().QueryRow(`SELECT COUNT(*) FROM participant_links
		WHERE participant_a = 2 AND participant_b = 3`).Scan(&linkCount))
	assert.Equal(1, linkCount)
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
	input.IsSensitive = true
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
	assert.True(copiedDefinition.IsSensitive)
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

func TestCopySubsetPreservesRawLegacySeedCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)

	_, err = source.DB().Exec(source.Rebind(`
		DELETE FROM attribute_definitions WHERE universal_id = ?
	`), AttributeUniversalIDLocation)
	require.NoError(err)
	legacy := subsetPersonDefinition(AttributeSlugLocation)
	legacy.UniversalID = "994e8d78-4711-42ec-9801-e3348e6fd133"
	legacy.Label = "Legacy location notes"
	legacy.FieldType = AttributeFieldTextarea
	legacy.Cardinality = AttributeCardinalityMulti
	legacy.Options = &AttributeOptions{MaxLength: 240}
	_, err = source.CreateAttributeDefinitionContext(ctx, legacy)
	require.NoError(err)
	legacyValue := "Old town"
	_, err = source.SetPersonAttributeValueContext(ctx, PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: legacy.Slug,
		Value:  AttributeValue{Type: AttributeValueText, Text: &legacyValue},
		Source: ProvenanceUser,
	})
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "legacy-seed-collision")
	_, err = CopySubsetWithOptions(srcDB, destinationDir, 5, CopySubsetOptions{
		IncludeAttributes: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	preserved, err := destination.GetAttributeDefinitionBySlugContext(
		ctx, AttributeObjectPerson, AttributeSlugLocation)
	require.NoError(err)
	assert.Equal(legacy.UniversalID, preserved.UniversalID)
	assert.Equal(legacy.Label, preserved.Label)
	assert.Equal(legacy.FieldType, preserved.FieldType)
	assert.Equal(legacy.Cardinality, preserved.Cardinality)

	values, err := destination.ListPersonAttributeValuesContext(ctx, person.ID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugLocation})
	require.NoError(err)
	require.Len(values, 1)
	require.NotNil(values[0].Value.Text)
	assert.Equal(legacyValue, *values[0].Value.Text)

	definitions, err := destination.ListAttributeDefinitionsContext(ctx,
		AttributeDefinitionFilter{ObjectType: AttributeObjectPerson})
	require.NoError(err)
	var canonical *AttributeDefinition
	for i := range definitions {
		if definitions[i].UniversalID == AttributeUniversalIDLocation {
			canonical = &definitions[i]
			break
		}
	}
	require.NotNil(canonical)
	assert.NotEqual(AttributeSlugLocation, canonical.Slug)
}

func TestCopySubsetResolvesCombinedRawSeedIdentityCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	_, err = source.DB().Exec(source.Rebind(`
		UPDATE attribute_definitions
		SET slug = 'location_custom'
		WHERE universal_id = ?
	`), AttributeUniversalIDLocation)
	require.NoError(err)
	legacy := subsetPersonDefinition(AttributeSlugLocation)
	legacy.UniversalID = "994e8d78-4711-42ec-9801-e3348e6fd133"
	legacyDefinition, err := source.CreateAttributeDefinitionContext(ctx, legacy)
	require.NoError(err)
	require.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "combined-seed-collision")
	_, err = CopySubsetWithOptions(srcDB, destinationDir, 5, CopySubsetOptions{
		IncludeAttributes: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	canonical, err := destination.GetAttributeDefinitionBySlugContext(
		ctx, AttributeObjectPerson, "location_custom")
	require.NoError(err)
	assert.Equal(AttributeUniversalIDLocation, canonical.UniversalID)
	assert.Equal(AttributeOwnershipSystem, canonical.Ownership)
	preserved, err := destination.GetAttributeDefinitionBySlugContext(
		ctx, AttributeObjectPerson, AttributeSlugLocation)
	require.NoError(err)
	assert.Equal(legacyDefinition.UniversalID, preserved.UniversalID)
}

func TestCopySubset_AttributesDefaultLegacySensitivityToFalse(t *testing.T) {
	require := require.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 3)
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`ALTER TABLE attribute_definitions DROP COLUMN is_sensitive`)
	require.NoError(err)
	require.NoError(db.Close())

	dstDir := filepath.Join(t.TempDir(), "legacy-attributes")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 3, CopySubsetOptions{
		IncludeAttributes: true,
	})
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	definition, err := destination.GetAttributeDefinitionBySlugContext(
		t.Context(), AttributeObjectPerson, AttributeSlugAskMeAbout)
	require.NoError(err)
	assert.False(t, definition.IsSensitive)
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
// Neither attribution column is indexed, and the only trigger naming one
// (trg_activity_queue_messages_update, on source_is_from_me) postdates such
// archives, so it is dropped first and ALTER TABLE ... DROP COLUMN works
// directly.
func TestCopySubset_LegacySourceMissingAttributionColumns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 3)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err, "open source db")

	_, err = db.Exec(`DROP TRIGGER trg_activity_queue_messages_update`)
	require.NoError(err, "drop the activity trigger that names source_is_from_me")

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
	// order. Indexes and triggers on the dropped columns go first — SQLite
	// refuses to drop a column referenced by either — and the activity
	// trigger is recreated afterwards with its schema.sql definition, since
	// a real upgraded archive carries it.
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
		`DROP TRIGGER IF EXISTS trg_activity_queue_conversation_type_update`,
		`ALTER TABLE conversations DROP COLUMN conversation_type`,
		`ALTER TABLE conversations DROP COLUMN title`,
		`ALTER TABLE conversations ADD COLUMN title TEXT`,
		`ALTER TABLE conversations ADD COLUMN conversation_type TEXT NOT NULL DEFAULT 'email_thread'`,
		`CREATE TRIGGER trg_activity_queue_conversation_type_update
		 AFTER UPDATE OF conversation_type ON conversations FOR EACH ROW
		 WHEN OLD.conversation_type IS NOT NEW.conversation_type
		 BEGIN
		     INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		     SELECT id, 1, CURRENT_TIMESTAMP
		     FROM messages WHERE conversation_id = NEW.id
		     ON CONFLICT(message_id) DO UPDATE SET
		         revision = activity_projection_queue.revision + 1,
		         queued_at = CURRENT_TIMESTAMP;
		 END`,
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

// TestCopySubset_UpgradedParticipantLinkColumnOrder covers an upgraded source
// whose identity-match ownership column was appended after created_at by an
// ALTER TABLE migration. The destination is fresh, so its declaration order
// differs. Copying by explicit column names must preserve both values.
func TestCopySubset_UpgradedParticipantLinkColumnOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 5)

	st, err := Open(srcDB)
	require.NoError(err, "open source for participant link upgrade")
	_, err = st.DB().Exec(`
		ALTER TABLE participant_links DROP COLUMN identity_match_candidate_id;
		ALTER TABLE participant_links ADD COLUMN identity_match_candidate_id INTEGER;
	`)
	require.NoError(err, "append participant link ownership column")
	require.NoError(st.Close(), "close upgraded source")

	candidateID := seedAcceptedSubsetParticipantLink(t, srcDB)
	sourceDB, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err, "open source for timestamp check")
	var sourceCreatedAt string
	require.NoError(sourceDB.QueryRow(`
		SELECT created_at FROM participant_links
		WHERE participant_a = 2 AND participant_b = 3
	`).Scan(&sourceCreatedAt), "read source participant link timestamp")
	require.NoError(sourceDB.Close(), "close source timestamp check")
	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{
		IncludeProfiles: true,
	})
	require.NoError(err, "copy upgraded participant links")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open copied database")
	defer func() { _ = dstDB.Close() }()

	var owner sql.NullInt64
	var createdAt string
	require.NoError(dstDB.QueryRow(`
		SELECT identity_match_candidate_id, created_at
		FROM participant_links
		WHERE participant_a = 2 AND participant_b = 3
	`).Scan(&owner, &createdAt), "read copied participant link")
	require.True(owner.Valid, "copied participant link ownership")
	assert.Equal(candidateID, owner.Int64, "identity match candidate owner")
	assert.Equal(sourceCreatedAt, createdAt, "participant link created_at")
}

func TestCopySubset_ExcludesParticipantLinkOwnershipWithoutProfiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	candidateID := seedAcceptedSubsetParticipantLink(t, srcDB)
	dstDir := filepath.Join(t.TempDir(), "dst")

	_, err := CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{})
	require.NoError(err, "copy without profiles")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open copied database")
	defer func() { _ = dstDB.Close() }()

	var owner sql.NullInt64
	require.NoError(dstDB.QueryRow(`
		SELECT identity_match_candidate_id
		FROM participant_links
		WHERE participant_a = 2 AND participant_b = 3
	`).Scan(&owner), "read profile-free participant link")
	assert.False(owner.Valid, "profile-free copies must not retain candidate ownership")

	var copiedCandidates int
	require.NoError(dstDB.QueryRow(
		`SELECT COUNT(*) FROM identity_match_candidates WHERE id = ?`, candidateID,
	).Scan(&copiedCandidates), "count omitted identity match candidate")
	assert.Zero(copiedCandidates, "profile-free copies omit candidate metadata")
}

func TestCopySubset_PreservesParticipantLinkOwnershipWithProfiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	candidateID := seedAcceptedSubsetParticipantLink(t, srcDB)
	dstDir := filepath.Join(t.TempDir(), "dst")

	_, err := CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{
		IncludeProfiles: true,
	})
	require.NoError(err, "copy with profiles")

	dstDB, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err, "open copied database")
	defer func() { _ = dstDB.Close() }()

	var owner sql.NullInt64
	var state string
	require.NoError(dstDB.QueryRow(`
		SELECT link.identity_match_candidate_id, candidate.state
		FROM participant_links link
		JOIN identity_match_candidates candidate
		  ON candidate.id = link.identity_match_candidate_id
		WHERE link.participant_a = 2 AND link.participant_b = 3
	`).Scan(&owner, &state), "read profile-preserving participant link")
	require.True(owner.Valid, "profile-preserving copies retain candidate ownership")
	assert.Equal(candidateID, owner.Int64, "identity match candidate owner")
	assert.Equal(string(IdentityMatchStateAccepted), state, "copied candidate state")
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
	// The mentor edge carries the resource it was read from, which scopes
	// its vCard identity to one card and must survive the copy verbatim.
	_, err = source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: alice.ID, TargetPersonID: bob.ID, TypeSlug: "mentor",
		Source: ProvenanceVCardImport, Actor: "system",
		SourceRef: new("book"), SourceResourceUID: new("card-alice"),
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
		ValueKind: RelatedValueKindText, Source: ProvenanceVCardImport, Actor: "system",
		SourceRef: new("book"), SourceResourceUID: new("card-alice")}
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
		if view.Relationship.TypeSlug == "mentor" {
			require.NotNil(view.Relationship.SourceResourceUID,
				"the copied edge must keep its source resource")
			assert.Equal("card-alice", *view.Relationship.SourceResourceUID)
		}
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
	require.NotNil(again.Review.SourceResourceUID,
		"the copied review must keep its source resource")
	assert.Equal("card-alice", *again.Review.SourceResourceUID)
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

func TestCopySubset_ProfilesIncludeEmploymentsAndOrganizations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	organization, err := source.CreateOrganizationContext(ctx, OrganizationInput{
		Name: "Example Org", Kind: OrganizationKindCompany,
	})
	require.NoError(err)
	profile, err := source.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, OrganizationProfileInput{
			Names: []OrganizationNameInput{{
				Name: "Example Organisation", NameKind: OrganizationNameKindAlias,
				Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
			}},
		})
	require.NoError(err)
	require.Len(profile.Names, 1)
	definition := subsetPersonDefinition("org_industry")
	definition.ObjectType = AttributeObjectOrganization
	_, err = source.CreateAttributeDefinitionContext(ctx, definition)
	require.NoError(err)
	industry := "archiving"
	_, err = source.SetOrganizationAttributeValueContext(ctx, OrganizationAttributeValueInput{
		OrganizationID: organization.ID, DefinitionSlug: definition.Slug,
		Value:  AttributeValue{Type: AttributeValueText, Text: &industry},
		Source: ProvenanceUser,
	})
	require.NoError(err)
	employment, err := source.AddEmploymentContext(ctx, EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: ProvenanceUser,
	})
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	copied, err := CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{
		IncludeProfiles: true, IncludeAttributes: true,
	})
	require.NoError(err)
	assert.Equal(int64(1), copied.Organizations,
		"the result reports exported organizations for auditing")
	assert.Equal(int64(1), copied.Employments,
		"the result reports exported employments for auditing")
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	copiedOrganization, err := destination.GetOrganizationContext(ctx, organization.ID)
	require.NoError(err)
	assert.Equal("Example Org", copiedOrganization.Name)
	copiedEmployments, err := destination.ListEmploymentsContext(ctx,
		EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Len(copiedEmployments, 1)
	assert.Equal(employment.ID, copiedEmployments[0].ID)
	copiedProfile, err := destination.GetOrganizationProfileContext(ctx, organization.ID, false)
	require.NoError(err)
	require.Len(copiedProfile.Names, 1)
	assert.Equal("Example Organisation", copiedProfile.Names[0].Name)
	values, err := destination.ListOrganizationAttributeValuesContext(ctx,
		organization.ID, OrganizationAttributeQuery{})
	require.NoError(err)
	require.Len(values, 1)
	require.NotNil(values[0].Value.Text)
	assert.Equal("archiving", *values[0].Value.Text)
}

func TestCopySubset_ExcludesEmploymentsByDefault(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	organization, err := source.CreateOrganizationContext(ctx, OrganizationInput{
		Name: "Private Employer", Kind: OrganizationKindCompany,
	})
	require.NoError(err)
	_, err = source.AddEmploymentContext(ctx, EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: ProvenanceUser,
	})
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubset(srcDB, dstDir, 5, false)
	require.NoError(err)
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })

	_, err = destination.GetOrganizationContext(ctx, organization.ID)
	require.ErrorIs(err, ErrOrganizationNotFound,
		"a shared subset must not copy employers without the profiles opt-in")
	employments, err := destination.ListEmploymentsContext(ctx,
		EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	assert.Empty(employments)
}

func TestCopySubset_OrganizationRecordReferencesFollowIdentityPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	owner, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	targetParticipant, err := source.EnsureParticipant(
		"org-attribute-target@example.com", "org attribute target", "example.com")
	require.NoError(err)
	target, _, err := source.CreatePersonFromParticipant(targetParticipant)
	require.NoError(err)
	organization, err := source.CreateOrganizationContext(ctx, OrganizationInput{
		Name: "Reference Org", Kind: OrganizationKindCompany,
	})
	require.NoError(err)
	_, err = source.AddEmploymentContext(ctx, EmploymentInput{
		PersonID: owner.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: ProvenanceUser,
	})
	require.NoError(err)

	definition := subsetPersonDefinition("org_primary_contact")
	definition.UniversalID = "test-org-primary-contact"
	definition.ObjectType = AttributeObjectOrganization
	definition.ValueType = AttributeValueRecordReference
	definition.FieldType = AttributeFieldPerson
	definition.RecordTarget = new("person")
	_, err = source.CreateAttributeDefinitionContext(ctx, definition)
	require.NoError(err)
	write, err := source.SetOrganizationAttributeValueContext(ctx, OrganizationAttributeValueInput{
		OrganizationID: organization.ID, DefinitionSlug: definition.Slug,
		Value: AttributeValue{
			Type:       AttributeValueRecordReference,
			RecordType: new("person"),
			RecordID:   &target.ID,
		},
		Source: ProvenanceUser,
	})
	require.NoError(err)
	require.NoError(source.Close())

	boundedDir := filepath.Join(t.TempDir(), "bounded")
	_, err = CopySubsetWithOptions(srcDB, boundedDir, 5, CopySubsetOptions{
		IncludeProfiles: true, IncludeAttributes: true,
	})
	require.NoError(err)
	boundedSubset, err := Open(filepath.Join(boundedDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = boundedSubset.Close() }()
	_, err = boundedSubset.GetPerson(target.ID)
	require.ErrorIs(err, ErrPersonNotFound,
		"off-message reference target stays outside the default identity boundary")
	boundedValues, err := boundedSubset.ListOrganizationAttributeValuesContext(
		ctx, organization.ID, OrganizationAttributeQuery{IncludeHistory: true})
	require.NoError(err)
	assert.Empty(boundedValues,
		"organization references to excluded identities must not dangle in the subset")

	identityDir := filepath.Join(t.TempDir(), "identity")
	_, err = CopySubsetWithOptions(srcDB, identityDir, 5, CopySubsetOptions{
		IncludeIdentity: true, IncludeProfiles: true, IncludeAttributes: true,
	})
	require.NoError(err)
	identitySubset, err := Open(filepath.Join(identityDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = identitySubset.Close() }()
	copiedTarget, err := identitySubset.GetPerson(target.ID)
	require.NoError(err,
		"identity closure must follow organization-attribute references from employers of included people")
	assert.Equal(target.ParticipantIDs, copiedTarget.ParticipantIDs)
	identityValues, err := identitySubset.ListOrganizationAttributeValuesContext(
		ctx, organization.ID, OrganizationAttributeQuery{})
	require.NoError(err)
	require.Len(identityValues, 1)
	assert.Equal(write.Value.ID, identityValues[0].ID)
	assert.Equal(target.ID, *identityValues[0].Value.RecordID)
}

func TestCopySubsetVCardResourcesRequireExplicitOptIn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	person, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Bob Example\r\n" +
		"item1.EMAIL;TYPE=home;X-KEEP=one:bob@example.com\r\n" +
		"item1.X-ABLABEL:Home\r\nEND:VCARD\r\n")
	envelope, err := vcard.ParseResourceEnvelope(raw)
	require.NoError(err)
	envelope.SourceRef = "address-book"
	envelope.SourceResourceUID = "source-bob"
	envelope.CanonicalPersonUID = person.VCardUID
	created, err := source.PutVCardResourceEnvelopeContext(
		ctx, VCardResourceEnvelopeInput{PersonID: person.ID, Envelope: envelope},
	)
	require.NoError(err)
	_, err = source.RetirePersonUIDAliasContext(
		ctx, "retired-bob", &person.ID, "merge",
	)
	require.NoError(err)
	require.NoError(source.Close())

	// An opaque body carries data classes the structured options gate
	// separately, so nothing short of its own opt-in may copy it.
	requireResourcesWithheld := func(name string, options CopySubsetOptions) {
		dir := filepath.Join(t.TempDir(), name)
		_, err := CopySubsetWithOptions(srcDB, dir, 1, options)
		require.NoError(err, name)
		subset, err := Open(filepath.Join(dir, "msgvault.db"))
		require.NoError(err, name)
		t.Cleanup(func() { _ = subset.Close() })
		_, err = subset.GetVCardResourceEnvelopeContext(
			ctx, "address-book", "source-bob",
		)
		require.ErrorIs(err, ErrVCardResourceNotFound, name)
		_, err = subset.ResolveRetiredPersonUIDContext(ctx, "retired-bob")
		require.ErrorIs(err, ErrPersonUIDAliasNotFound, name)
	}
	requireResourcesWithheld("default", CopySubsetOptions{})
	requireResourcesWithheld("profiles", CopySubsetOptions{IncludeProfiles: true})

	// Bodies without the profiles they project into is not a subset that
	// can be built, and the caller must hear that rather than get an
	// archive with the resources quietly missing.
	orphanDir := filepath.Join(t.TempDir(), "resources-without-profiles")
	_, err = CopySubsetWithOptions(srcDB, orphanDir, 1,
		CopySubsetOptions{IncludeVCardResources: true})
	require.ErrorIs(err, ErrSubsetVCardResourcesRequireProfiles)
	_, statErr := os.Stat(filepath.Join(orphanDir, "msgvault.db"))
	require.ErrorIs(statErr, os.ErrNotExist,
		"a refused subset must leave no database behind")

	resourcesDir := filepath.Join(t.TempDir(), "resources")
	_, err = CopySubsetWithOptions(srcDB, resourcesDir, 1, CopySubsetOptions{
		IncludeProfiles: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	resourcesSubset, err := Open(filepath.Join(resourcesDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = resourcesSubset.Close() })
	copied, err := resourcesSubset.GetVCardResourceEnvelopeContext(
		ctx, "address-book", "source-bob",
	)
	require.NoError(err)
	assert.Equal(created.ID, copied.ID)
	assert.Equal(created.Revision, copied.Revision)
	assert.Equal(raw, copied.OriginalRawBytes)
	assert.Equal(raw, copied.StoredBody)
	assert.Equal(created.PropertyTree, copied.PropertyTree)
	assert.Equal(created.Residue, copied.Residue)
	alias, err := resourcesSubset.ResolveRetiredPersonUIDContext(ctx, "retired-bob")
	require.NoError(err)
	require.NotNil(alias.SurvivingPersonID)
	assert.Equal(person.ID, *alias.SurvivingPersonID)
}

func TestCopySubsetVCardResourcesFollowProfileBoundary(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	included, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	offSubsetParticipant, err := source.EnsureParticipant(
		"vcard-outsider@example.com", "vCard Outsider", "example.com")
	require.NoError(err)
	excluded, _, err := source.CreatePersonFromParticipant(offSubsetParticipant)
	require.NoError(err)

	putEnvelope := func(person *Person, sourceUID, formattedName string) {
		raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:" + formattedName +
			"\r\nEND:VCARD\r\n")
		envelope, err := vcard.ParseResourceEnvelope(raw)
		require.NoError(err, sourceUID)
		envelope.SourceRef = "address-book"
		envelope.SourceResourceUID = sourceUID
		envelope.CanonicalPersonUID = person.VCardUID
		_, err = source.PutVCardResourceEnvelopeContext(
			ctx, VCardResourceEnvelopeInput{PersonID: person.ID, Envelope: envelope},
		)
		require.NoError(err, sourceUID)
		_, err = source.RetirePersonUIDAliasContext(
			ctx, "retired-"+sourceUID, &person.ID, "merge",
		)
		require.NoError(err, sourceUID)
	}
	putEnvelope(included, "source-included", "Bob Example")
	putEnvelope(excluded, "source-excluded", "Outsider Example")
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "resources")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 1, CopySubsetOptions{
		IncludeProfiles: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	subset, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = subset.Close() })

	copied, err := subset.GetVCardResourceEnvelopeContext(
		ctx, "address-book", "source-included",
	)
	require.NoError(err)
	assert.Equal(included.ID, copied.PersonID)
	alias, err := subset.ResolveRetiredPersonUIDContext(ctx, "retired-source-included")
	require.NoError(err)
	require.NotNil(alias.SurvivingPersonID)
	assert.Equal(included.ID, *alias.SurvivingPersonID)

	_, err = subset.GetPerson(excluded.ID)
	require.ErrorIs(err, ErrPersonNotFound,
		"a person without messages stays outside the default identity boundary")
	_, err = subset.GetVCardResourceEnvelopeContext(
		ctx, "address-book", "source-excluded",
	)
	require.ErrorIs(err, ErrVCardResourceNotFound,
		"the opt-in must not reach vCard bodies of people outside the subset")
	_, err = subset.ResolveRetiredPersonUIDContext(ctx, "retired-source-excluded")
	require.ErrorIs(err, ErrPersonUIDAliasNotFound)
}

func TestCopySubsetReleasesVCardMappingsToOwnersLeftBehind(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	included, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	offSubsetParticipant, err := source.EnsureParticipant(
		"vcard-outsider@example.com", "vCard Outsider", "example.com")
	require.NoError(err)
	excluded, _, err := source.CreatePersonFromParticipant(offSubsetParticipant)
	require.NoError(err)
	insider, _, err := source.CreatePersonFromParticipant(3)
	require.NoError(err)
	outsideEdge, err := source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: included.ID, TargetPersonID: excluded.ID, TypeSlug: "kin",
		Source: ProvenanceVCardImport, Actor: "system",
	})
	require.NoError(err)
	insideEdge, err := source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: included.ID, TargetPersonID: insider.ID, TypeSlug: "sibling",
		Source: ProvenanceVCardImport, Actor: "system",
	})
	require.NoError(err)

	// One RELATED line is owned by the edge to the outsider, the other by the
	// edge to a person inside the subset; FN is owned by nothing. All are
	// opaque bytes to the copy, and only the mapping to the edge the subset
	// leaves behind may go.
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Bob Example\r\n" +
		"RELATED;TYPE=kin;X-KEEP=yes:urn:uuid:" + excluded.VCardUID + "\r\n" +
		"RELATED;TYPE=sibling:urn:uuid:" + insider.VCardUID + "\r\n" +
		"END:VCARD\r\n")
	envelope, err := vcard.ParseResourceEnvelope(raw)
	require.NoError(err)
	envelope.SourceRef = "address-book"
	envelope.SourceResourceUID = "source-included"
	envelope.CanonicalPersonUID = included.VCardUID
	related := make([]vcard.PropertyOccurrence, 0, 2)
	for _, occurrence := range envelope.PropertyTree {
		if occurrence.Property.Name == "RELATED" {
			related = append(related, occurrence)
		}
	}
	require.Len(related, 2)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: related[0].Identity, SourceRef: "address-book",
		Table: "person_relationships", RowID: outsideEdge.ID, Field: "related",
		Kind: vcard.HandlingRelationship,
	}, {
		Identity: related[1].Identity, SourceRef: "address-book",
		Table: "person_relationships", RowID: insideEdge.ID, Field: "related",
		Kind: vcard.HandlingRelationship,
	}}
	envelope.Residue = vcard.ResidueWithMappings(envelope.PropertyTree, envelope.NativeMappings)
	_, err = source.PutVCardResourceEnvelopeContext(
		ctx, VCardResourceEnvelopeInput{PersonID: included.ID, Envelope: envelope},
	)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "resources")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 1, CopySubsetOptions{
		IncludeProfiles: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	subset, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = subset.Close() })

	_, err = subset.GetPersonRelationshipContext(ctx, outsideEdge.ID)
	require.ErrorIs(err, ErrPersonRelationshipNotFound,
		"the edge to the outsider stays outside the subset")
	_, err = subset.GetPersonRelationshipContext(ctx, insideEdge.ID)
	require.NoError(err, "the edge between included people is copied")
	copied, err := subset.GetVCardResourceEnvelopeContext(
		ctx, "address-book", "source-included",
	)
	require.NoError(err)
	assert.Equal(raw, copied.StoredBody, "the body is copied verbatim")
	require.Len(copied.NativeMappings, 1,
		"only the mapping to an owner the subset did not copy may go: a later "+
			"projection would retire that occurrence as stale instead of keeping it")
	assert.Equal(insideEdge.ID, copied.NativeMappings[0].RowID,
		"a mapping to a copied owner is kept, though its owner lands after the envelope")
	require.Len(copied.Residue, 2, "the released occurrence joins the residue")
	assert.Equal("RELATED", copied.Residue[1].Property.Name)
	assert.Contains(copied.Residue[1].Property.RawValue, excluded.VCardUID)
}

func TestCopySubsetCopiesRelationshipsFromSourcesWithoutResourceColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	alice, _, err := source.CreatePersonFromParticipant(1)
	require.NoError(err)
	bob, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	_, err = source.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
		SourcePersonID: alice.ID, TargetPersonID: bob.ID, TypeSlug: "kin",
		Source: ProvenanceUser, Actor: "user",
	})
	require.NoError(err)
	resolved, err := source.ResolveRelatedValueContext(ctx, RelatedImport{
		PersonID: alice.ID, RawValue: bob.VCardUID, RawType: "friend",
		ValueKind: RelatedValueKindText, Source: ProvenanceVCardImport, Actor: "system",
	})
	require.NoError(err)
	require.NotNil(resolved.Review)
	// An archive whose relationship tables predate the resource column: the
	// column and the index that names it are gone, exactly as before the
	// upgrade that added them.
	_, err = source.DB().Exec(`
		DROP INDEX idx_person_relationship_reviews_occurrence_unique;
		ALTER TABLE person_relationship_reviews DROP COLUMN source_resource_uid;
		ALTER TABLE person_relationships DROP COLUMN source_resource_uid;
	`)
	require.NoError(err, "simulate a source that predates source_resource_uid")
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "dst")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 5, CopySubsetOptions{IncludeProfiles: true})
	require.NoError(err, "profiles copy from a pre-upgrade source")
	destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })
	views, err := destination.ListPersonRelationshipsContext(
		ctx, alice.ID, PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(views, 2)
	for _, view := range views {
		assert.Nil(view.Relationship.SourceResourceUID,
			"rows the source never scoped to a resource stay unscoped")
	}
	reviews, err := destination.ListRelationshipReviewsContext(
		ctx, RelationshipReviewListOptions{PersonID: alice.ID})
	require.NoError(err)
	require.NotEmpty(reviews)
	assert.Nil(reviews[0].SourceResourceUID)
}
func TestCopySubsetReleasesReviewMappingsWhoseAcceptedEdgeWasFiltered(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srcDB := createTestSourceDB(t, t.TempDir(), 5)
	source, err := Open(srcDB)
	require.NoError(err)
	included, _, err := source.CreatePersonFromParticipant(2)
	require.NoError(err)
	offSubsetParticipant, err := source.EnsureParticipant(
		"vcard-outsider@example.com", "vCard Outsider", "example.com")
	require.NoError(err)
	excluded, _, err := source.CreatePersonFromParticipant(offSubsetParticipant)
	require.NoError(err)
	// An accepted review whose edge reaches outside the subset boundary. The
	// copy keeps the ledger row but clears the edge, so the review will never
	// reappear in a projection snapshot.
	resolved, err := source.ResolveRelatedValueContext(ctx, RelatedImport{
		PersonID: included.ID, RawValue: excluded.VCardUID, RawType: "kin",
		ValueKind: RelatedValueKindText, Source: ProvenanceVCardImport, Actor: "importer",
		SourceRef: new("address-book"), SourceResourceUID: new("source-included"),
	})
	require.NoError(err)
	require.NotNil(resolved.Review)
	require.NotNil(resolved.Relationship)

	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Bob Example\r\n" +
		"RELATED;TYPE=kin;X-KEEP=yes:urn:uuid:" + excluded.VCardUID + "\r\n" +
		"END:VCARD\r\n")
	envelope, err := vcard.ParseResourceEnvelope(raw)
	require.NoError(err)
	envelope.SourceRef = "address-book"
	envelope.SourceResourceUID = "source-included"
	envelope.CanonicalPersonUID = included.VCardUID
	var related vcard.PropertyOccurrence
	for _, occurrence := range envelope.PropertyTree {
		if occurrence.Property.Name == "RELATED" {
			related = occurrence
		}
	}
	require.NotEmpty(related.Property.Name)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: related.Identity, SourceRef: "address-book",
		Table: "person_relationship_reviews", RowID: resolved.Review.ID, Field: "related",
		Kind: vcard.HandlingRelationship,
	}}
	envelope.Residue = vcard.ResidueWithMappings(envelope.PropertyTree, envelope.NativeMappings)
	_, err = source.PutVCardResourceEnvelopeContext(
		ctx, VCardResourceEnvelopeInput{PersonID: included.ID, Envelope: envelope},
	)
	require.NoError(err)
	require.NoError(source.Close())

	dstDir := filepath.Join(t.TempDir(), "resources")
	_, err = CopySubsetWithOptions(srcDB, dstDir, 1, CopySubsetOptions{
		IncludeProfiles: true, IncludeVCardResources: true,
	})
	require.NoError(err)
	subset, err := Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = subset.Close() })

	reviews, err := subset.ListRelationshipReviewsContext(
		ctx, RelationshipReviewListOptions{PersonID: included.ID})
	require.NoError(err)
	require.Len(reviews, 1, "the ledger row survives the copy")
	assert.Nil(reviews[0].AcceptedRelationshipID, "with its filtered edge cleared")
	copied, err := subset.GetVCardResourceEnvelopeContext(
		ctx, "address-book", "source-included",
	)
	require.NoError(err)
	assert.Empty(copied.NativeMappings,
		"a review whose accepted edge the copy filtered out never reappears in a "+
			"snapshot, so keeping its mapping would delete the occurrence on the "+
			"next projection")
	require.Len(copied.Residue, 2)
	assert.Equal("RELATED", copied.Residue[1].Property.Name)
}
