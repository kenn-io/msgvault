package store_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMergePersons_RootsAndBindings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant("merge-survivor@example.com", "Survivor", "example.com")
	absorbedParticipant := f.EnsureParticipant("merge-absorbed@example.com", "Absorbed", "example.com")
	survivor, created, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	require.True(created)
	absorbed, created, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	require.True(created)

	survivor, err = f.Store.UpdatePersonDisplayNameContext(
		t.Context(), survivor.ID, survivor.Revision, new("Kept Name"),
	)
	require.NoError(err)
	absorbedUID := absorbed.VCardUID
	rawVCard := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Absorbed\r\nEND:VCARD\r\n")
	envelope := parseStoreEnvelope(t, rawVCard, "merge-book", "absorbed-card")
	envelope.CanonicalPersonUID = absorbedUID
	storedEnvelope, err := f.Store.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: absorbed.ID, Envelope: envelope,
		},
	)
	require.NoError(err)
	seedFullProfile(t, f.Store, absorbed.ID)
	_, err = f.Store.RetirePersonUIDAliasContext(
		t.Context(), "older-absorbed-alias", &absorbed.ID, "test",
	)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)

	request := store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-roots-and-bindings", Actor: "test",
	}
	result, err := f.Store.MergePersonsContext(t.Context(), request)
	require.NoError(err)
	assert.Equal(survivor.ID, result.Person.ID)
	assert.Equal(survivor.VCardUID, result.Person.VCardUID)
	assert.Equal(new("Kept Name"), result.Person.DisplayName)
	assert.Equal(survivor.Revision+1, result.Person.Revision)
	assert.Equal([]int64{survivorParticipant, absorbedParticipant}, result.Person.ParticipantIDs)
	assert.Equal(survivor.ID, result.Merge.SurvivorPersonID)
	assert.Equal(absorbed.ID, result.Merge.AbsorbedPersonID)
	assert.Equal(absorbedUID, result.Merge.AbsorbedVCardUID)
	assert.Equal(1, result.Merge.SnapshotVersion)
	assert.Len(result.Merge.SnapshotSHA256, 64)

	_, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.ErrorIs(err, store.ErrPersonNotFound)
	alias, err := f.Store.ResolveRetiredPersonUIDContext(t.Context(), absorbedUID)
	require.NoError(err)
	require.NotNil(alias.SurvivingPersonID)
	assert.Equal(survivor.ID, *alias.SurvivingPersonID)

	profile, err := f.Store.GetPersonProfileContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.Len(profile.Names, 2)
	assert.Len(profile.ContactPoints, 2)
	assert.Len(profile.Addresses, 1)
	assert.Len(profile.Dates, 1)
	assert.Len(profile.Categories, 1)
	assert.Len(profile.Media, 1)
	movedEnvelope, err := f.Store.GetVCardResourceEnvelopeContext(
		t.Context(), "merge-book", "absorbed-card",
	)
	require.NoError(err)
	assert.Equal(storedEnvelope.ID, movedEnvelope.ID)
	assert.Equal(storedEnvelope.Revision+1, movedEnvelope.Revision)
	assert.Equal(survivor.ID, movedEnvelope.PersonID)
	assert.Equal(survivor.VCardUID, movedEnvelope.CanonicalPersonUID)
	assert.Equal(rawVCard, movedEnvelope.OriginalRawBytes)
	staleRevision := storedEnvelope.Revision
	_, err = f.Store.PutVCardResourceEnvelopeContext(t.Context(), store.VCardResourceEnvelopeInput{
		PersonID: survivor.ID, ExpectedRevision: &staleRevision,
		Envelope: movedEnvelope.ResourceEnvelope,
	})
	require.Error(err)
	require.ErrorIs(err, store.ErrVCardResourceWriteConflict)
	olderAlias, err := f.Store.ResolveRetiredPersonUIDContext(
		t.Context(), "older-absorbed-alias",
	)
	require.NoError(err)
	require.NotNil(olderAlias.SurvivingPersonID)
	assert.Equal(survivor.ID, *olderAlias.SurvivingPersonID)
	var (
		aliasOriginalID  sql.NullInt64
		aliasOriginalKey string
		aliasAction      string
	)
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT
		original_row_id, original_row_key, action
		FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'person_uid_aliases'`), result.Merge.ID).Scan(
		&aliasOriginalID, &aliasOriginalKey, &aliasAction,
	))
	assert.False(aliasOriginalID.Valid)
	assert.NotEmpty(aliasOriginalKey)
	assert.Equal("repointed", aliasAction)

	replayed, err := f.Store.MergePersonsContext(t.Context(), request)
	require.NoError(err)
	assert.Equal(result.Merge.ID, replayed.Merge.ID)
	assert.Equal(result.Person.Revision, replayed.Person.Revision)
	changedActor := request
	changedActor.Actor = "different-actor"
	_, err = f.Store.MergePersonsContext(t.Context(), changedActor)
	require.ErrorIs(err, store.ErrPersonMergeIdempotency)

	changedRequest := request
	changedRequest.AbsorbedID++
	_, err = f.Store.MergePersonsContext(t.Context(), changedRequest)
	require.Error(err)
	require.ErrorIs(err, store.ErrPersonMergeIdempotency)

	currentSurvivor, err := f.Store.GetPersonContext(t.Context(), result.Person.ID)
	require.NoError(err)
	_, err = f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: currentSurvivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: currentSurvivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-already-absorbed", Actor: "test",
	})
	require.ErrorIs(err, store.ErrPersonNotFound)
}

func TestMergePersons_CardDAVState(t *testing.T) {
	t.Run("resource binding follows exact split", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		ctx := t.Context()
		st, _, book := newCardDAVResourceStore(t)
		survivor := mustPromotedPerson(t.Context(), t, st, "carddav-merge-survivor@example.com", "Survivor")
		absorbed := mustPromotedPerson(t.Context(), t, st, "carddav-merge-absorbed@example.com", "Absorbed")

		var resourceID int64
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO carddav_resources (
			address_book_id, href, remote_etag, remote_body, remote_semantic_hash,
			local_hash, mapping_status, governance, person_id, person_revision_at_bind, owns_display_name
		) VALUES (?, ?, ?, ?, ?, ?, 'mapped', 'remote', ?, ?, TRUE) RETURNING id`),
			book.ID, book.CanonicalURL+"absorbed.vcf", `"etag"`, []byte("card"),
			"remote-hash", "local-hash", absorbed.ID, absorbed.Revision,
		).Scan(&resourceID))

		merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "carddav-resource-merge", Actor: "test",
		})
		require.NoError(err)
		var ownerID int64
		var ownsDisplayName bool
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(
			`SELECT person_id, owns_display_name FROM carddav_resources WHERE id = ?`), resourceID).Scan(&ownerID, &ownsDisplayName))
		assert.Equal(survivor.ID, ownerID)
		assert.False(ownsDisplayName)

		split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
			SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
			ParticipantIDs:         []int64{absorbed.ParticipantIDs[0]},
			ExpectedSourceRevision: merged.Person.Revision,
			IdempotencyKey:         "carddav-resource-split", Actor: "test",
		})
		require.NoError(err)
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(
			`SELECT person_id, owns_display_name FROM carddav_resources WHERE id = ?`), resourceID).Scan(&ownerID, &ownsDisplayName))
		assert.Equal(split.NewPerson.ID, ownerID)
		assert.True(ownsDisplayName)
	})

	t.Run("publication blocks merge", func(t *testing.T) {
		require := require.New(t)
		ctx := t.Context()
		st, _, _ := newCardDAVResourceStore(t)
		survivor := mustPromotedPerson(t.Context(), t, st, "carddav-published-survivor@example.com", "Survivor")
		absorbed := mustPromotedPerson(t.Context(), t, st, "carddav-published-absorbed@example.com", "Absorbed")
		_, err := st.DB().ExecContext(ctx, st.Rebind(
			`INSERT INTO carddav_publications (person_id, desired) VALUES (?, TRUE)`), absorbed.ID)
		require.NoError(err)

		_, err = st.MergePersonsContext(ctx, store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "carddav-publication-merge", Actor: "test",
		})
		require.ErrorIs(err, store.ErrPersonCardDAVPublished)
		_, err = st.GetPersonContext(ctx, absorbed.ID)
		require.NoError(err)
	})
}

type personMergeInspectionFixture struct {
	store               *store.Store
	person, unrelated   *store.Person
	absorbedParticipant int64
	merge               *store.PersonMergeResult
	survivorValueID     int64
	absorbedValueID     int64
}

func newPersonMergeInspectionFixture(t *testing.T, key string) personMergeInspectionFixture {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, key+"-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t.Context(), t, st, key+"-absorbed@example.com", "Absorbed")
	absorbedAlias, err := st.EnsureParticipant(
		key+"-absorbed-alias@example.com", "Absorbed Alias", "example.com")
	require.NoError(err)
	_, err = st.LinkParticipants(absorbed.ParticipantIDs[0], absorbedAlias)
	require.NoError(err)
	unrelated := mustPromotedPerson(t.Context(), t, st, key+"-unrelated@example.com", "Unrelated")
	_, err = st.AddPersonNameContext(ctx, absorbed.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Absorbed Name"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	survivorValue, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: survivor.ID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	absorbedValue, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: absorbed.ID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("chat")},
		Source: store.ProvenanceVCardImport,
	})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           key + "-merge", Actor: "test",
	})
	require.NoError(err)
	return personMergeInspectionFixture{
		store: st, person: &merged.Person, unrelated: unrelated,
		absorbedParticipant: absorbed.ParticipantIDs[0], merge: merged,
		survivorValueID: survivorValue.Value.ID, absorbedValueID: absorbedValue.Value.ID,
	}
}

type personMergeRecordReferenceFixture struct {
	store           *store.Store
	absorbed        *store.Person
	absorbedTarget  *store.Person
	merge           *store.PersonMergeResult
	absorbedValueID int64
}

func newPersonMergeRecordReferenceFixture(
	t *testing.T, key string,
) personMergeRecordReferenceFixture {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, key+"-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t.Context(), t, st, key+"-absorbed@example.com", "Absorbed")
	survivorTarget := mustPromotedPerson(t.Context(), t, st,
		key+"-survivor-target@example.com", "Survivor Target")
	absorbedTarget := mustPromotedPerson(t.Context(), t, st,
		key+"-absorbed-target@example.com", "Absorbed Target")
	definition := personTextDefinition(strings.ReplaceAll(key, "-", "_") + "_record_reference")
	definition.ValueType = store.AttributeValueRecordReference
	definition.FieldType = store.AttributeFieldPerson
	definition.RecordTarget = new("person")
	_, err := st.CreateAttributeDefinitionContext(ctx, definition)
	require.NoError(err)
	var absorbedValueID int64
	for personID, targetID := range map[int64]int64{
		survivor.ID: survivorTarget.ID,
		absorbed.ID: absorbedTarget.ID,
	} {
		write, writeErr := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: definition.Slug,
			Value: store.AttributeValue{
				Type:       store.AttributeValueRecordReference,
				RecordType: new("person"), RecordID: &targetID,
			},
			Source: store.ProvenanceUser,
		})
		require.NoError(writeErr)
		if personID == absorbed.ID {
			absorbedValueID = write.Value.ID
		}
	}
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           key + "-merge", Actor: "test",
	})
	require.NoError(err)
	require.Len(merged.ReviewCandidates, 1)
	return personMergeRecordReferenceFixture{
		store: st, absorbed: absorbed, absorbedTarget: absorbedTarget,
		merge: merged, absorbedValueID: absorbedValueID,
	}
}

func assertJSONEquivalent(t *testing.T, want, got any, msgAndArgs ...any) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, string(wantJSON), string(gotJSON), msgAndArgs...)
}

func TestPersonMerge_Inspect(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonMergeInspectionFixture(t, "inspect")
	ctx := t.Context()
	split, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: f.person.ID, MergeID: f.merge.Merge.ID,
		ParticipantIDs:         []int64{f.absorbedParticipant},
		ExpectedSourceRevision: f.person.Revision,
		IdempotencyKey:         "inspect-split", Actor: "test",
	})
	require.NoError(err)
	list, err := f.store.ListPersonMergesContext(ctx, split.SourcePerson.ID)
	require.NoError(err)
	require.Len(list, 1)
	assert.Equal(f.merge.Merge.ID, list[0].Merge.ID)
	assert.False(list[0].Labels.Incomplete)

	assert.Equal(3, list[0].ParticipantCount)
	assert.Equal(1, list[0].SplitCount)
	assert.Equal(1, list[0].PendingCandidateCount)
	assert.NotEmpty(list[0].RowActionCounts)
	newPersonList, err := f.store.ListPersonMergesContext(ctx, split.NewPerson.ID)
	require.NoError(err)
	require.Len(newPersonList, 1)
	unrelated, err := f.store.ListPersonMergesContext(ctx, f.unrelated.ID)
	require.NoError(err)
	assert.Empty(unrelated)

	detail, err := f.store.GetPersonMergeContext(ctx, f.merge.Merge.ID)
	require.NoError(err)
	assert.Equal(f.merge.Merge.ID, detail.Merge.ID)
	assert.Len(detail.Participants, 3)
	assert.Len(detail.Labels.ParticipantIDs, 3)
	assert.Equal(detail.Participants, list[0].Labels.Participants)
	assert.Empty(detail.Labels.Participants)
	assert.NotEmpty(detail.Rows)
	assert.Len(detail.Splits, 1)
	assert.Len(detail.ReviewCandidates, 1)
}

func TestPersonMerge_InspectOrderAndPageLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, "inspect-order-survivor@example.com", "Survivor")
	mergeIDs := []int64{}
	for index := range 3 {
		absorbed := mustPromotedPerson(t.Context(), t, st,
			fmt.Sprintf("inspect-order-%d@example.com", index), "Absorbed")
		survivor, _ = st.GetPersonContext(ctx, survivor.ID)
		merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           fmt.Sprintf("inspect-order-%d", index), Actor: "test",
		})
		require.NoError(err)
		survivor = &merged.Person
		mergeIDs = append(mergeIDs, merged.Merge.ID)
	}
	list, err := st.ListPersonMergesPageContext(ctx, survivor.ID, 2, 0)
	require.NoError(err)
	require.Len(list, 2)
	assert.Equal([]int64{mergeIDs[2], mergeIDs[1]},
		[]int64{list[0].Merge.ID, list[1].Merge.ID})
}

func TestPersonMerge_Snapshot(t *testing.T) {
	assert := assert.New(t)
	f := newPersonMergeInspectionFixture(t, "snapshot-read")
	ctx := t.Context()
	response, err := f.store.GetPersonMergeSnapshotContext(ctx, f.merge.Merge.ID)
	require.NoError(t, err)
	assert.Equal(f.merge.Merge.SnapshotVersion, response.Version)
	assert.Equal(f.merge.Merge.SnapshotSHA256, response.SHA256)
	assert.True(json.Valid(response.JSON))
	assert.Contains(string(response.JSON), `"persons"`)
	_, err = f.store.DB().ExecContext(ctx, f.store.Rebind(`UPDATE person_merges
		SET snapshot_blob = ? WHERE id = ?`), []byte("corrupt"), f.merge.Merge.ID)
	require.NoError(t, err)
	_, err = f.store.GetPersonMergeSnapshotContext(ctx, f.merge.Merge.ID)
	require.ErrorIs(t, err, store.ErrPersonMergeSnapshotCorrupt)
}

func TestPersonMerge_LabelReadStates(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		prepare                func(*testing.T, *personMergeInspectionFixture)
		incomplete, empty      bool
		profiles, participants int
	}{
		{"identifiers", func(t *testing.T, f *personMergeInspectionFixture) { denyPersonMergeIdentityReads(t, f.store) }, true, true, 2, 0},
		{"snapshot", func(t *testing.T, f *personMergeInspectionFixture) {
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_merges SET snapshot_blob = ? WHERE id = ?`), []byte("corrupt"), f.merge.Merge.ID)
			require.NoError(t, err)
		}, true, false, 0, 3},
		{"known empty", func(t *testing.T, f *personMergeInspectionFixture) {
			_, err := f.store.DB().ExecContext(t.Context(), `DELETE FROM participant_identifiers`)
			require.NoError(t, err)
			_, err = f.store.DB().ExecContext(t.Context(), `UPDATE participants SET email_address = NULL, phone_number = NULL`)
			require.NoError(t, err)
		}, false, true, 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPersonMergeInspectionFixture(t, "identity-read-failure")
			tc.prepare(t, &f)
			detail, err := f.store.GetPersonMergeContext(t.Context(), f.merge.Merge.ID)
			require.NoError(t, err)
			listed, err := f.store.ListPersonMergesContext(t.Context(), f.merge.Person.ID)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			assert.NotEmpty(t, detail.Participants)
			assert.NotEmpty(t, listed[0].Labels.Participants)
			assert.Equal(t, tc.incomplete, detail.Labels.Incomplete)
			assert.Equal(t, tc.incomplete, listed[0].Labels.Incomplete)
			assert.Len(t, detail.Labels.Profiles, tc.profiles)
			assert.Len(t, listed[0].Labels.Profiles, tc.profiles)
			assert.Len(t, detail.Labels.ParticipantIDs, tc.participants)
			assert.Len(t, listed[0].Labels.ParticipantIDs, tc.participants)
			assert.Equal(t, tc.empty, len(detail.Labels.Addresses) == 0)
			assert.Equal(t, tc.empty, len(listed[0].Labels.Addresses) == 0)
		})
	}
}

func TestPersonMerge_CandidateDecisionAcceptedAndIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonMergeInspectionFixture(t, "candidate-accept")
	ctx := t.Context()
	request := store.PersonMergeCandidateDecisionRequest{
		CandidateID: f.merge.ReviewCandidates[0].ID, PersonID: f.person.ID,
		ExpectedPersonRevision: f.person.Revision,
		Decision:               store.PersonMergeCandidateAccept, Actor: "reviewer",
	}
	accepted, err := f.store.DecidePersonMergeCandidateContext(ctx, request)
	require.NoError(err)
	assert.Equal(f.person.Revision+1, accepted.PersonRevision)
	assert.Equal("accepted", accepted.State)
	assert.Equal(new("reviewer"), accepted.ReviewedBy)
	assert.NotNil(accepted.ReviewedAt)
	require.NotNil(accepted.ResolutionValueID)
	assert.NotEqual(f.survivorValueID, *accepted.ResolutionValueID)
	values, err := f.store.ListPersonAttributeValuesContext(ctx, f.person.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(values, 1)
	require.NotNil(values[0].Value.Text)
	assert.Equal("chat", *values[0].Value.Text)
	history, err := f.store.ListPersonAttributeValuesContext(ctx, f.person.ID,
		store.PersonAttributeQuery{
			DefinitionSlug: store.AttributeSlugPrimaryChannel, IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(history, 3)
	historyIDs := make([]int64, 0, len(history))
	for _, value := range history {
		historyIDs = append(historyIDs, value.ID)
	}
	assert.Contains(historyIDs, f.survivorValueID)
	assert.Contains(historyIDs, f.absorbedValueID)
	assert.Contains(historyIDs, *accepted.ResolutionValueID)
	current, err := f.store.GetPersonContext(ctx, f.person.ID)
	require.NoError(err)
	assert.Equal(f.person.Revision+1, current.Revision)
	replayed, err := f.store.DecidePersonMergeCandidateContext(ctx, request)
	require.NoError(err)
	assertJSONEquivalent(t, accepted, replayed)
	assert.Equal(current.Revision, replayed.PersonRevision)
	afterReplay, err := f.store.GetPersonContext(ctx, f.person.ID)
	require.NoError(err)
	assert.Equal(current.Revision, afterReplay.Revision)
}

func TestPersonMerge_PendingRecordReferenceCandidateBlocksTargetDeletion(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	f := newPersonMergeRecordReferenceFixture(t, "candidate-target-delete")
	target, err := f.store.GetPersonContext(ctx, f.absorbedTarget.ID)
	require.NoError(err)
	err = f.store.DeletePersonContext(ctx, target.ID, target.Revision)
	require.ErrorIs(err, store.ErrPersonReferenced)
	_, err = f.store.GetPersonContext(ctx, target.ID)
	require.NoError(err)
	accepted, err := f.store.DecidePersonMergeCandidateContext(ctx,
		store.PersonMergeCandidateDecisionRequest{
			CandidateID: f.merge.ReviewCandidates[0].ID, PersonID: f.merge.Person.ID,
			ExpectedPersonRevision: f.merge.Person.Revision,
			Decision:               store.PersonMergeCandidateAccept, Actor: "reviewer",
		})
	require.NoError(err)
	require.Equal("accepted", accepted.State)
}

func TestPersonMerge_CandidateDecisionRejectedAndConflicts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonMergeInspectionFixture(t, "candidate-reject")
	ctx := t.Context()
	base := store.PersonMergeCandidateDecisionRequest{
		CandidateID: f.merge.ReviewCandidates[0].ID, PersonID: f.person.ID,
		ExpectedPersonRevision: f.person.Revision,
		Decision:               store.PersonMergeCandidateReject, Actor: "reviewer",
	}
	stale := base
	stale.ExpectedPersonRevision++
	_, err := f.store.DecidePersonMergeCandidateContext(ctx, stale)
	require.ErrorIs(err, store.ErrPersonRevisionConflict)
	wrongPerson := base
	wrongPerson.PersonID = f.unrelated.ID
	wrongPerson.ExpectedPersonRevision = f.unrelated.Revision
	_, err = f.store.DecidePersonMergeCandidateContext(ctx, wrongPerson)
	require.ErrorIs(err, store.ErrPersonMergeCandidateState)
	rejected, err := f.store.DecidePersonMergeCandidateContext(ctx, base)
	require.NoError(err)
	assert.Equal("rejected", rejected.State)
	assert.Equal(new("reviewer"), rejected.ReviewedBy)
	assert.NotNil(rejected.ReviewedAt)
	assert.Nil(rejected.ResolutionValueID)
	values, err := f.store.ListPersonAttributeValuesContext(ctx, f.person.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(values, 1)
	require.NotNil(values[0].Value.Text)
	assert.Equal("email", *values[0].Value.Text)
	changed := base
	changed.Decision = store.PersonMergeCandidateAccept
	_, err = f.store.DecidePersonMergeCandidateContext(ctx, changed)
	require.ErrorIs(err, store.ErrPersonMergeCandidateState)
}

func TestPersonMerge_CandidateDecisionRejectsChangedCurrentValue(t *testing.T) {
	require := require.New(t)
	f := newPersonMergeInspectionFixture(t, "candidate-current-changed")
	ctx := t.Context()
	_, err := f.store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("phone")},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	_, err = f.store.DecidePersonMergeCandidateContext(ctx,
		store.PersonMergeCandidateDecisionRequest{
			CandidateID: f.merge.ReviewCandidates[0].ID, PersonID: f.person.ID,
			ExpectedPersonRevision: f.person.Revision,
			Decision:               store.PersonMergeCandidateAccept, Actor: "reviewer",
		})
	require.ErrorIs(err, store.ErrPersonMergeCandidateState)
	current, err := f.store.ListPersonAttributeValuesContext(ctx, f.person.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(current, 1)
	require.NotNil(current[0].Value.Text)
	assert.Equal(t, "phone", *current[0].Value.Text)
}

func TestPersonMerge_CandidateDecisionRejectsInactiveDefinitionWithoutMutation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	f := newPersonMergeInspectionFixture(t, "candidate-inactive-definition")
	definition, err := f.store.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, store.AttributeSlugPrimaryChannel)
	require.NoError(err)
	_, err = f.store.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision,
		store.AttributeDefinitionUpdate{IsActive: new(false)})
	require.NoError(err)

	_, err = f.store.DecidePersonMergeCandidateContext(ctx,
		store.PersonMergeCandidateDecisionRequest{
			CandidateID: f.merge.ReviewCandidates[0].ID, PersonID: f.person.ID,
			ExpectedPersonRevision: f.person.Revision,
			Decision:               store.PersonMergeCandidateAccept, Actor: "reviewer",
		})
	require.ErrorIs(err, store.ErrPersonMergeCandidateState)
	require.ErrorIs(err, store.ErrAttributeDefinitionInactive)
	current, err := f.store.GetPersonContext(ctx, f.person.ID)
	require.NoError(err)
	assert.Equal(f.person.Revision, current.Revision)
	values, err := f.store.ListPersonAttributeValuesContext(ctx, f.person.ID,
		store.PersonAttributeQuery{
			DefinitionSlug: store.AttributeSlugPrimaryChannel, IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(values, 2)
	var currentValues int
	for _, value := range values {
		if value.ActiveUntil == nil {
			currentValues++
			require.NotNil(value.Value.Text)
			assert.Equal("email", *value.Value.Text)
		}
	}
	assert.Equal(1, currentValues)
	detail, err := f.store.GetPersonMergeContext(ctx, f.merge.Merge.ID)
	require.NoError(err)
	require.Len(detail.ReviewCandidates, 1)
	assert.Equal("pending", detail.ReviewCandidates[0].State)
	assert.Nil(detail.ReviewCandidates[0].ResolutionValueID)
	assert.Nil(detail.ReviewCandidates[0].ReviewedBy)
	assert.Nil(detail.ReviewCandidates[0].ReviewedAt)
}

func TestPersonMerge_CandidateDecisionMissingCandidate(t *testing.T) {
	f := newPersonMergeInspectionFixture(t, "candidate-missing")
	_, err := f.store.DecidePersonMergeCandidateContext(
		t.Context(),
		store.PersonMergeCandidateDecisionRequest{
			CandidateID: f.merge.ReviewCandidates[0].ID + 1_000_000,
			PersonID:    f.person.ID, ExpectedPersonRevision: f.person.Revision,
			Decision: store.PersonMergeCandidateReject, Actor: "reviewer",
		})
	require.ErrorIs(t, err, store.ErrPersonMergeCandidateNotFound)
}

func TestMergePersons_RevisionConflictChangesNothing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant("stale-survivor@example.com", "Survivor", "example.com")
	absorbedParticipant := f.EnsureParticipant("stale-absorbed@example.com", "Absorbed", "example.com")
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	_, err = f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision + 1,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-stale-revision", Actor: "test",
	})
	require.Error(err)
	require.ErrorIs(err, store.ErrPersonRevisionConflict)

	unchangedSurvivor, err := f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.Equal([]int64{survivorParticipant}, unchangedSurvivor.ParticipantIDs)
	unchangedAbsorbed, err := f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	assert.Equal([]int64{absorbedParticipant}, unchangedAbsorbed.ParticipantIDs)
	_, err = f.Store.ResolveRetiredPersonUIDContext(t.Context(), absorbed.VCardUID)
	require.ErrorIs(err, store.ErrPersonUIDAliasNotFound)
}

func TestMergePersons_Facts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant("facts-survivor@example.com", "Survivor", "example.com")
	absorbedParticipant := f.EnsureParticipant("facts-absorbed@example.com", "Absorbed", "example.com")
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	observerParticipant := f.EnsureParticipant("facts-observer@example.com", "Observer", "example.com")
	observer, _, err := f.Store.CreatePersonFromParticipant(observerParticipant)
	require.NoError(err)
	_, err = f.Store.AddPersonCategoryContext(t.Context(), survivor.ID, store.PersonCategoryInput{
		OriginalValue: "Friends", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = f.Store.AddPersonCategoryContext(t.Context(), absorbed.ID, store.PersonCategoryInput{
		OriginalValue: "friends", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport},
	})
	require.NoError(err)
	var absorbedCategoryID int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id
		FROM person_categories WHERE person_id = ? AND normalized_value = 'friends'`),
		absorbed.ID).Scan(&absorbedCategoryID))

	survivorChannel, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: survivor.ID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	absorbedChannel, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: absorbed.ID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("chat")},
		Source: store.ProvenanceVCardImport,
	})
	require.NoError(err)
	for personID, topic := range map[int64]string{
		survivor.ID: "music", absorbed.ID: "music",
	} {
		_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugAskMeAbout,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &topic},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	gardening := "gardening"
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: absorbed.ID, DefinitionSlug: store.AttributeSlugAskMeAbout,
		Ordinal: new(int64(1)),
		Value:   store.AttributeValue{Type: store.AttributeValueText, Text: &gardening},
		Source:  store.ProvenanceUser,
	})
	require.NoError(err)
	jsonDefinition := personTextDefinition("merge_json_equivalence")
	jsonDefinition.UniversalID = "test-merge-json-equivalence"
	jsonDefinition.ValueType = store.AttributeValueJSON
	jsonDefinition.FieldType = store.AttributeFieldJSON
	_, err = f.Store.CreateAttributeDefinitionContext(t.Context(), jsonDefinition)
	require.NoError(err)
	for personID, value := range map[int64]json.RawMessage{
		survivor.ID: json.RawMessage(`{"a":1,"b":2}`),
		absorbed.ID: json.RawMessage(`{"b":2,"a":1}`),
	} {
		_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: jsonDefinition.Slug,
			Value:  store.AttributeValue{Type: store.AttributeValueJSON, JSON: value},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	relatedDefinition := personTextDefinition("merge_related_person")
	relatedDefinition.UniversalID = "test-merge-related-person"
	relatedDefinition.ValueType = store.AttributeValueRecordReference
	relatedDefinition.FieldType = store.AttributeFieldPerson
	relatedDefinition.RecordTarget = new("person")
	_, err = f.Store.CreateAttributeDefinitionContext(t.Context(), relatedDefinition)
	require.NoError(err)
	relatedWrite, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: observer.ID, DefinitionSlug: relatedDefinition.Slug,
		Value: store.AttributeValue{
			Type: store.AttributeValueRecordReference, RecordType: new("person"), RecordID: &absorbed.ID,
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	var observerProjectionBefore int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT
		vcard_projection_revision FROM persons WHERE id = ?`), observer.ID).Scan(&observerProjectionBefore))
	survivor, err = f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)

	result, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-facts", Actor: "test",
	})
	require.NoError(err)
	require.Len(result.ReviewCandidates, 1)
	candidate := result.ReviewCandidates[0]
	assert.Equal("pending", candidate.State)
	assert.Equal(survivorChannel.Value.ID, candidate.SurvivorValueID)
	assert.Equal(absorbedChannel.Value.ID, candidate.AbsorbedValueID)

	currentChannels, err := f.Store.ListPersonAttributeValuesContext(t.Context(), survivor.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(currentChannels, 1)
	assert.Equal("email", *currentChannels[0].Value.Text)
	historicalChannels, err := f.Store.ListPersonAttributeValuesContext(t.Context(), survivor.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel, IncludeHistory: true})
	require.NoError(err)
	assert.Len(historicalChannels, 2)

	topics, err := f.Store.ListPersonAttributeValuesContext(t.Context(), survivor.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugAskMeAbout})
	require.NoError(err)
	require.Len(topics, 2)
	assert.Equal("music", *topics[0].Value.Text)
	assert.Equal("gardening", *topics[1].Value.Text)

	profile, err := f.Store.GetPersonProfileContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.Len(profile.Categories, 1)
	history, err := f.Store.GetPersonProfileHistoryContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.Len(history.Categories, 2)
	var (
		categoryCurrentID int64
		categoryKey       string
		categoryAction    string
	)
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT
		current_row_id, original_row_key, action
		FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'person_categories' AND original_row_id = ?`),
		result.Merge.ID, absorbedCategoryID).Scan(
		&categoryCurrentID, &categoryKey, &categoryAction,
	))
	assert.Equal(absorbedCategoryID, categoryCurrentID)
	assert.NotEmpty(categoryKey)
	assert.Equal("deduplicated", categoryAction)
	var (
		relatedRecordID   int64
		relatedAction     string
		relatedProvenance string
	)
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT
		value.value_record_id, journal.action, journal.provenance_kind
		FROM person_attribute_values value
		JOIN person_merge_rows journal
		  ON journal.table_name = 'person_attribute_values'
		 AND journal.original_row_id = value.id
		WHERE journal.merge_id = ? AND value.id = ?`),
		result.Merge.ID, relatedWrite.Value.ID).Scan(
		&relatedRecordID, &relatedAction, &relatedProvenance,
	))
	assert.Equal(survivor.ID, relatedRecordID)
	assert.Equal("repointed", relatedAction)
	assert.Equal("inbound_reference", relatedProvenance)
	var observerProjectionAfter int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT
		vcard_projection_revision FROM persons WHERE id = ?`), observer.ID).Scan(&observerProjectionAfter))
	assert.Equal(observerProjectionBefore+1, observerProjectionAfter)
}

func TestMergePersons_StructuredPropertyIdentityCollision(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant("property-survivor@example.com", "Survivor", "example.com")
	absorbedParticipant := f.EnsureParticipant("property-absorbed@example.com", "Absorbed", "example.com")
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	closedAt := time.Now().UTC().Add(-time.Hour)
	for _, value := range []struct {
		personID   int64
		name       string
		resourceID string
		until      *time.Time
	}{
		{personID: survivor.ID, name: "Current Name", resourceID: "resource-a"},
		{personID: absorbed.ID, name: "Historical Name", resourceID: "resource-a", until: &closedAt},
		{personID: absorbed.ID, name: "Distinct Resource Name", resourceID: "resource-b"},
	} {
		_, err = f.Store.AddPersonNameContext(t.Context(), value.personID, store.PersonNameInput{
			NameKind: store.PersonNameFormatted, Formatted: &value.name,
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceVCardImport, SourceRef: new("shared-book"),
				SourceResourceUID: &value.resourceID,
				VCard:             store.VCardIdentity{Property: "FN", PropID: new("shared-property")},
				ActiveUntil:       value.until,
			},
		})
		require.NoError(err)
	}
	survivor, err = f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)

	result, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-property-identity", Actor: "test",
	})
	require.NoError(err)
	var action string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT action
		FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'person_names' AND original_row_id = (
			SELECT id FROM person_names
			WHERE person_id = ? AND original_value = 'Historical Name'
		)`), result.Merge.ID, survivor.ID).Scan(&action))
	assert.Equal(t, "deduplicated", action)
	var distinctAction string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT journal.action
		FROM person_merge_rows journal
		JOIN person_names name ON name.id = journal.current_row_id
		WHERE journal.merge_id = ? AND journal.table_name = 'person_names'
		  AND name.original_value = 'Distinct Resource Name'`), result.Merge.ID).Scan(&distinctAction))
	assert.Equal(t, "moved", distinctAction,
		"the same property ID on another source resource remains distinct")
}

func TestMergePersons_RelationshipsAndReviews(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, "merge-rel-survivor@example.com", "Survivor")
	other := mustPromotedPerson(t.Context(), t, st, "merge-rel-other@example.com", "Other")
	absorbed := mustPromotedPerson(t.Context(), t, st, "merge-rel-absorbed@example.com", "Absorbed")

	related := func(personID int64) *store.RelatedResolution {
		t.Helper()
		resolution, err := st.ResolveRelatedValueContext(ctx, store.RelatedImport{
			PersonID: personID, RawValue: other.VCardUID, RawType: "agent",
			ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport,
			SourceRef: new("shared-related.vcf"), Actor: "test",
		})
		require.NoError(err)
		require.NotNil(resolution.Relationship)
		require.NotNil(resolution.Review)
		return resolution
	}
	survivorRelated := related(survivor.ID)
	absorbedRelated := related(absorbed.ID)
	incoming, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: other.ID, TargetPersonID: absorbed.ID, TypeSlug: "parent",
		Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	selfAfterMerge, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: survivor.ID, TargetPersonID: absorbed.ID, TypeSlug: "friend",
		Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	symmetricMove, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: absorbed.ID, TargetPersonID: other.ID, TypeSlug: "spouse",
		Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)

	selfReview, err := st.ResolveRelatedValueContext(ctx, store.RelatedImport{
		PersonID: survivor.ID, RawValue: absorbed.VCardUID, RawType: "unknown",
		ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport,
		SourceRef: new("self-after-merge.vcf"), Actor: "test",
	})
	require.NoError(err)
	require.NotNil(selfReview.Review)
	require.NotNil(selfReview.Review.MatchedPersonID)
	rejectedResolution, err := st.ResolveRelatedValueContext(ctx, store.RelatedImport{
		PersonID: absorbed.ID, RawValue: "Not an identity", RawType: "unknown",
		ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport,
		SourceRef: new("rejected-before-merge.vcf"), Actor: "test",
	})
	require.NoError(err)
	require.NotNil(rejectedResolution.Review)
	rejectedReview, err := st.RejectRelationshipReviewContext(
		ctx, rejectedResolution.Review.ID, "reviewer",
	)
	require.NoError(err)
	assert.Equal(store.RelationshipReviewRejected, rejectedReview.Status)
	resourceReview := func(personID int64, resourceUID string) *store.RelationshipReview {
		t.Helper()
		resolution, err := st.ResolveRelatedValueContext(ctx, store.RelatedImport{
			PersonID: personID, RawValue: "Resource-scoped unresolved identity",
			RawType: "unknown", ValueKind: store.RelatedValueKindText,
			Source: store.ProvenanceVCardImport, SourceRef: new("shared-related.vcf"),
			SourceResourceUID: &resourceUID, Actor: "test",
		})
		require.NoError(err)
		require.Nil(resolution.Relationship)
		require.NotNil(resolution.Review)
		return resolution.Review
	}
	survivorResourceReview := resourceReview(survivor.ID, "survivor-card")
	absorbedResourceReview := resourceReview(absorbed.ID, "absorbed-card")

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-relationships-and-reviews", Actor: "test",
	})
	require.NoError(err)

	relationships, err := st.ListPersonRelationshipsContext(ctx, survivor.ID, store.PersonRelationshipListOptions{})
	require.NoError(err)
	require.Len(relationships, 3)
	byRelationshipID := make(map[int64]store.PersonRelationshipView, len(relationships))
	for _, relationship := range relationships {
		byRelationshipID[relationship.Relationship.ID] = relationship
	}
	assert.Equal(other.ID, byRelationshipID[survivorRelated.Relationship.ID].CounterpartPersonID)
	assert.Equal(other.ID, byRelationshipID[incoming.ID].CounterpartPersonID)
	assert.Equal(survivor.ID, byRelationshipID[incoming.ID].Relationship.TargetPersonID)
	assert.Equal(survivor.ID, byRelationshipID[symmetricMove.ID].Relationship.SourcePersonID)
	assert.Equal(other.ID, byRelationshipID[symmetricMove.ID].Relationship.TargetPersonID)

	reviews, err := st.ListRelationshipReviewsContext(ctx, store.RelationshipReviewListOptions{PersonID: survivor.ID})
	require.NoError(err)
	require.Len(reviews, 5)
	byReviewID := make(map[int64]store.RelationshipReview, len(reviews))
	for _, review := range reviews {
		byReviewID[review.ID] = review
	}
	assert.Equal(survivorRelated.Relationship.ID, *byReviewID[survivorRelated.Review.ID].AcceptedRelationshipID)
	assert.Nil(byReviewID[selfReview.Review.ID].MatchedPersonID)
	assert.Equal(store.RelationshipReviewRejected, byReviewID[rejectedReview.ID].Status)
	require.NotNil(byReviewID[survivorResourceReview.ID].SourceResourceUID)
	assert.Equal("survivor-card", *byReviewID[survivorResourceReview.ID].SourceResourceUID)
	require.NotNil(byReviewID[absorbedResourceReview.ID].SourceResourceUID)
	assert.Equal("absorbed-card", *byReviewID[absorbedResourceReview.ID].SourceResourceUID)

	for _, want := range []struct {
		table     string
		original  int64
		action    string
		currentID *int64
	}{
		{table: "person_relationships", original: absorbedRelated.Relationship.ID, action: "deduplicated", currentID: &survivorRelated.Relationship.ID},
		{table: "person_relationships", original: selfAfterMerge.ID, action: "deleted_snapshot"},
		{table: "person_relationship_reviews", original: absorbedRelated.Review.ID, action: "deduplicated", currentID: &survivorRelated.Review.ID},
	} {
		var currentID sql.NullInt64
		var action string
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT current_row_id, action
			FROM person_merge_rows
			WHERE merge_id = ? AND table_name = ? AND original_row_id = ?`),
			result.Merge.ID, want.table, want.original).Scan(&currentID, &action))
		assert.Equal(want.action, action)
		if want.currentID == nil {
			assert.False(currentID.Valid)
		} else {
			require.True(currentID.Valid)
			assert.Equal(*want.currentID, currentID.Int64)
		}
	}
}

func TestMergePersons_Employments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, "merge-job-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t.Context(), t, st, "merge-job-absorbed@example.com", "Absorbed")
	sharedOrg := mustOrganization(t, st, "Shared Merge Employer")
	absorbedPrimaryOrg := mustOrganization(t, st, "Absorbed Primary Employer")
	historicalOrg := mustOrganization(t, st, "Historical Merge Employer")

	survivorPrimary, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: survivor.ID, OrganizationID: sharedOrg.ID, Title: new("Engineer"),
		IsPrimary: new(true), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	absorbedPrimary, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: absorbed.ID, OrganizationID: absorbedPrimaryOrg.ID, Title: new("Advisor"),
		IsPrimary: new(true), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	absorbedDuplicate, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: absorbed.ID, OrganizationID: sharedOrg.ID, Title: new("Engineer"),
		IsPrimary: new(false), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	absorbedHistory, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: absorbed.ID, OrganizationID: historicalOrg.ID, Title: new("Intern"),
		IsCurrent: new(false), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-employments", Actor: "test",
	})
	require.NoError(err)

	employments, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: survivor.ID})
	require.NoError(err)
	require.Len(employments, 3)
	byID := make(map[int64]store.Employment, len(employments))
	for _, employment := range employments {
		byID[employment.ID] = employment
	}
	assert.True(byID[survivorPrimary.ID].IsPrimary)
	assert.False(byID[absorbedPrimary.ID].IsPrimary)
	assert.True(byID[absorbedPrimary.ID].IsCurrent)
	assert.Equal(absorbedPrimary.Revision+1, byID[absorbedPrimary.ID].Revision)
	assert.False(byID[absorbedHistory.ID].IsCurrent)
	_, duplicateExists := byID[absorbedDuplicate.ID]
	assert.False(duplicateExists)

	var currentID sql.NullInt64
	var action string
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT current_row_id, action
		FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'employments' AND original_row_id = ?`),
		result.Merge.ID, absorbedDuplicate.ID).Scan(&currentID, &action))
	require.True(currentID.Valid)
	assert.Equal(survivorPrimary.ID, currentID.Int64)
	assert.Equal("deduplicated", action)
}

func TestMergePersons_InboundReferences(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, "merge-ref-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t.Context(), t, st, "merge-ref-absorbed@example.com", "Absorbed")
	observer := mustPromotedPerson(t.Context(), t, st, "merge-ref-observer@example.com", "Observer")
	organization := mustOrganization(t, st, "Merge Reference Organization")
	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: observer.ID, OrganizationID: organization.ID,
		Title: new("Observer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	definition := organizationTextDefinition("merge_related_person")
	definition.ValueType = store.AttributeValueRecordReference
	definition.FieldType = store.AttributeFieldPerson
	definition.RecordTarget = new("person")
	_, err = st.CreateAttributeDefinitionContext(ctx, definition)
	require.NoError(err)
	write, err := st.SetOrganizationAttributeValueContext(ctx, store.OrganizationAttributeValueInput{
		OrganizationID: organization.ID, DefinitionSlug: definition.Slug,
		Value: store.AttributeValue{
			Type: store.AttributeValueRecordReference, RecordType: new("person"), RecordID: &absorbed.ID,
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	var observerProjectionBefore int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), observer.ID).Scan(&observerProjectionBefore))

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-inbound-references", Actor: "test",
	})
	require.NoError(err)

	var recordID, currentRowID int64
	var action, provenance string
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT
		value.value_record_id, journal.current_row_id, journal.action, journal.provenance_kind
		FROM organization_attribute_values value
		JOIN person_merge_rows journal
		  ON journal.table_name = 'organization_attribute_values'
		 AND journal.original_row_id = value.id
		WHERE journal.merge_id = ? AND value.id = ?`), result.Merge.ID, write.Value.ID).Scan(
		&recordID, &currentRowID, &action, &provenance,
	))
	assert.Equal(survivor.ID, recordID)
	assert.Equal(write.Value.ID, currentRowID)
	assert.Equal("repointed", action)
	assert.Equal("inbound_reference", provenance)
	var observerProjectionAfter int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), observer.ID).Scan(&observerProjectionAfter))
	assert.Equal(observerProjectionBefore+1, observerProjectionAfter)
}

func TestMergePersons_IdentityCandidates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, "merge-candidate-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t.Context(), t, st, "merge-candidate-absorbed@example.com", "Absorbed")
	other := mustPromotedPerson(t.Context(), t, st, "merge-candidate-other@example.com", "Other")
	source, err := st.GetOrCreateSource("gmail", "merge-candidates")
	require.NoError(err)
	input := func(personID int64) store.IdentityMatchCandidateInput {
		return store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: personID,
			RightKind: store.IdentityMatchPerson, RightID: other.ID,
			Basis: store.IdentityMatchDisplayName, NormalizedValue: new("same person"),
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser,
			SourceID: &source.ID,
		}
	}
	absorbedCandidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, input(absorbed.ID))
	require.NoError(err)
	survivorCandidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, input(survivor.ID))
	require.NoError(err)
	require.Less(absorbedCandidate.ID, survivorCandidate.ID,
		"the merge policy must retain survivor provenance rather than the lowest row ID")
	evidence, err := st.AddIdentityMatchEvidenceContext(ctx, absorbedCandidate.ID,
		store.IdentityMatchEvidenceInput{
			EvidenceKind: "shared_name", Source: store.ProvenanceUser, SourceID: &source.ID,
		})
	require.NoError(err)
	selfCandidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx,
		store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: survivor.ID,
			RightKind: store.IdentityMatchPerson, RightID: absorbed.ID,
			Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate,
			Source: store.ProvenanceUser, SourceID: &source.ID,
		})
	require.NoError(err)
	selfEvidence, err := st.AddIdentityMatchEvidenceContext(ctx, selfCandidate.ID,
		store.IdentityMatchEvidenceInput{
			EvidenceKind: "self-collapse", Source: store.ProvenanceUser, SourceID: &source.ID,
		})
	require.NoError(err)
	priorRedirectID := absorbedCandidate.ID + 10_000
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO identity_match_candidate_redirects
		(retired_candidate_id, surviving_candidate_id, endpoints_collapsed)
		VALUES (?, ?, FALSE)`), priorRedirectID, absorbedCandidate.ID)
	require.NoError(err)

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-identity-candidates", Actor: "test",
	})
	require.NoError(err)

	var absorbedRedirect int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT surviving_candidate_id
		FROM identity_match_candidate_redirects WHERE retired_candidate_id = ?`),
		absorbedCandidate.ID).Scan(&absorbedRedirect))
	assert.Equal(survivorCandidate.ID, absorbedRedirect)
	mergedCandidate, err := st.GetIdentityMatchCandidateContext(ctx, absorbedRedirect)
	require.NoError(err)
	assert.Equal(survivorCandidate.ID, mergedCandidate.ID)
	assert.ElementsMatch([]int64{survivor.ID, other.ID},
		[]int64{mergedCandidate.LeftID, mergedCandidate.RightID})
	require.Len(mergedCandidate.Evidence, 1)
	assert.Equal(evidence.ID, mergedCandidate.Evidence[0].ID)
	var redirectedTo int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT surviving_candidate_id
		FROM identity_match_candidate_redirects WHERE retired_candidate_id = ?`),
		priorRedirectID).Scan(&redirectedTo))
	assert.Equal(survivorCandidate.ID, redirectedTo)
	var collapsed bool
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT endpoints_collapsed
		FROM identity_match_candidate_redirects WHERE retired_candidate_id = ?`),
		selfCandidate.ID).Scan(&collapsed))
	assert.True(collapsed)

	for _, want := range []struct {
		id        int64
		action    string
		currentID *int64
	}{
		{id: absorbedCandidate.ID, action: "deduplicated", currentID: &survivorCandidate.ID},
		{id: selfCandidate.ID, action: "deleted_snapshot"},
	} {
		var currentID sql.NullInt64
		var action string
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT current_row_id, action
			FROM person_merge_rows
			WHERE merge_id = ? AND table_name = 'identity_match_candidates'
			  AND original_row_id = ?`), result.Merge.ID, want.id).Scan(&currentID, &action))
		assert.Equal(want.action, action)
		if want.currentID == nil {
			assert.False(currentID.Valid)
		} else {
			require.True(currentID.Valid)
			assert.Equal(*want.currentID, currentID.Int64)
		}
	}
	var deletedDependentRows int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM person_merge_rows
		WHERE merge_id = ? AND action = 'deleted_snapshot'
		  AND table_name IN (
			'identity_match_candidate_sources',
			'identity_match_evidence',
			'identity_match_evidence_sources'
		  )`), result.Merge.ID).Scan(&deletedDependentRows))
	assert.Equal(3, deletedDependentRows)
	var selfEvidenceCount int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM identity_match_evidence WHERE id = ?`), selfEvidence.ID).Scan(&selfEvidenceCount))
	assert.Zero(selfEvidenceCount)
}

func TestMergePersons_DailyNotes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, "merge-note-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t.Context(), t, st, "merge-note-absorbed@example.com", "Absorbed")
	shared, err := st.CreateDailyNoteEntryContext(ctx, store.DailyNoteEntryInput{
		LocalDate: "2026-08-19", Body: "shared", Author: "test",
		PersonIDs: []int64{survivor.ID, absorbed.ID},
	})
	require.NoError(err)
	absorbedOnly, err := st.CreateDailyNoteEntryContext(ctx, store.DailyNoteEntryInput{
		LocalDate: "2026-08-19", Body: "absorbed", Author: "test",
		PersonIDs: []int64{absorbed.ID},
	})
	require.NoError(err)

	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-daily-notes", Actor: "test",
	})
	require.NoError(err)

	entries, err := st.ListDailyNoteEntriesForPersonContext(ctx, survivor.ID, "", 0, 0)
	require.NoError(err)
	require.Len(entries, 2)
	assert.Equal([]int64{survivor.ID}, entries[0].PersonIDs)
	assert.Equal([]int64{survivor.ID}, entries[1].PersonIDs)
	var absorbedRefs int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*)
		FROM daily_note_entry_persons WHERE person_id = ?`), absorbed.ID).Scan(&absorbedRefs))
	assert.Zero(absorbedRefs)

	journalRows, err := st.DB().QueryContext(ctx, st.Rebind(`SELECT
		action, current_row_key, provenance_kind
		FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'daily_note_entry_persons'
		ORDER BY action`), result.Merge.ID)
	require.NoError(err)
	defer func() { require.NoError(journalRows.Close()) }()
	var journalActions []string
	for journalRows.Next() {
		var action, currentKey, provenance string
		require.NoError(journalRows.Scan(&action, &currentKey, &provenance))
		journalActions = append(journalActions, action)
		assert.NotEmpty(currentKey)
		assert.Equal("inbound_reference", provenance)
	}
	require.NoError(journalRows.Err())
	assert.Equal([]string{"deduplicated", "repointed"}, journalActions)
	assert.NotEqual(shared.ID, absorbedOnly.ID)
}

func TestMergePersons_DerivedState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, survivor, absorbed, messageID := mergeDerivedStateFixture(t)
	ctx := t.Context()
	for _, personID := range []int64{survivor.ID, absorbed.ID} {
		var count int64
		require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT interaction_count
			FROM person_contact_state WHERE person_id = ?`), personID).Scan(&count))
		assert.Equal(int64(1), count)
	}

	result, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-derived-state", Actor: "test",
	})
	require.NoError(err)

	rows, err := f.Store.DB().QueryContext(ctx, f.Store.Rebind(`SELECT person_id, role, evidence
		FROM activity_event_persons WHERE message_id = ? ORDER BY person_id`), messageID)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	require.True(rows.Next())
	var personID int64
	var role, evidence string
	require.NoError(rows.Scan(&personID, &role, &evidence))
	assert.Equal(survivor.ID, personID)
	assert.Equal(string(store.RoleAddressed), role)
	assert.Equal(string(store.EvidenceDirect), evidence)
	assert.False(rows.Next())
	require.NoError(rows.Err())

	var interactionCount, identityRevision int64
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT
		interaction_count, identity_revision
		FROM person_contact_state WHERE person_id = ?`), survivor.ID).Scan(
		&interactionCount, &identityRevision,
	))
	revisions, err := f.Store.ContactRevisionsContext(ctx)
	require.NoError(err)
	assert.Equal(int64(3), interactionCount,
		"contact state must be rebuilt from three final native events, not summed")
	assert.Equal(revisions.IdentityRevision, identityRevision)
	var absorbedDerivedRows int
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT
		(SELECT COUNT(*) FROM activity_event_persons WHERE person_id = ?) +
		(SELECT COUNT(*) FROM person_contact_state WHERE person_id = ?)`),
		absorbed.ID, absorbed.ID).Scan(&absorbedDerivedRows))
	assert.Zero(absorbedDerivedRows)

	var journalCount int
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT COUNT(*)
		FROM person_merge_rows
		WHERE merge_id = ? AND table_name IN ('activity_event_persons', 'person_contact_state')`),
		result.Merge.ID).Scan(&journalCount))
	assert.Zero(journalCount, "derived activity is rebuilt from native messages, not snapshotted")
}

func TestMergePersons_DerivedRollback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, survivor, absorbed, messageID := mergeDerivedStateFixture(t)
	ctx := t.Context()
	if f.Store.IsPostgreSQL() {
		_, err := f.Store.DB().ExecContext(ctx, `
			CREATE FUNCTION fail_person_merge_contact_recompute() RETURNS trigger AS $$
			BEGIN
				RAISE EXCEPTION 'forced person merge contact recompute failure';
			END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER fail_person_merge_contact_recompute
			BEFORE INSERT OR UPDATE ON person_contact_state
			FOR EACH ROW EXECUTE FUNCTION fail_person_merge_contact_recompute();`)
		require.NoError(err)
	} else {
		_, err := f.Store.DB().ExecContext(ctx, `CREATE TRIGGER fail_person_merge_contact_recompute
			BEFORE INSERT ON person_contact_state BEGIN
				SELECT RAISE(ABORT, 'forced person merge contact recompute failure');
			END`)
		require.NoError(err)
		_, err = f.Store.DB().ExecContext(ctx, `CREATE TRIGGER fail_person_merge_contact_update
			BEFORE UPDATE ON person_contact_state BEGIN
				SELECT RAISE(ABORT, 'forced person merge contact recompute failure');
			END`)
		require.NoError(err)
	}

	_, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-derived-rollback", Actor: "test",
	})
	require.Error(err)
	assert.Contains(err.Error(), "forced person merge contact recompute failure")
	for _, person := range []*store.Person{survivor, absorbed} {
		got, getErr := f.Store.GetPersonContext(ctx, person.ID)
		require.NoError(getErr)
		assert.Equal(person.Revision, got.Revision)
	}
	var mergeCount int
	require.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*)
		FROM person_merges WHERE idempotency_key = 'merge-derived-rollback'`).Scan(&mergeCount))
	assert.Zero(mergeCount)
	var linkCount int
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT COUNT(*)
		FROM activity_event_persons WHERE message_id = ?`), messageID).Scan(&linkCount))
	assert.Equal(2, linkCount)
}

func mergeDerivedStateFixture(
	t *testing.T,
) (*storetest.Fixture, *store.Person, *store.Person, int64) {
	t.Helper()
	require := require.New(t)
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant(
		"merge-derived-survivor@example.com", "Survivor", "example.com")
	absorbedParticipant := f.EnsureParticipant(
		"merge-derived-absorbed@example.com", "Absorbed", "example.com")
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	ownerParticipant := f.EnsureParticipant(
		"merge-derived-owner@example.com", "Owner", "example.com")
	require.NoError(f.Store.AddAccountIdentity(
		f.Source.ID, "merge-derived-owner@example.com", "test"))
	message := f.NewMessage().
		WithSourceMessageID("merge-derived-state").
		WithSentAt(time.Date(2026, 8, 19, 5, 0, 0, 0, time.UTC)).
		WithIsFromMe(true).
		Build()
	message.SenderID = sql.NullInt64{Int64: ownerParticipant, Valid: true}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{ownerParticipant}, []string{"Owner"}))
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{survivorParticipant, absorbedParticipant},
		[]string{"Survivor", "Absorbed"}))
	for label, senderID := range map[string]int64{
		"survivor": survivorParticipant,
		"absorbed": absorbedParticipant,
	} {
		direct := f.NewMessage().
			WithSourceMessageID("merge-derived-direct-" + label).
			WithSentAt(time.Date(2026, 8, 19, 6, 0, 0, 0, time.UTC)).
			Build()
		direct.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
		directID, directErr := f.Store.UpsertMessage(direct)
		require.NoError(directErr)
		require.NoError(f.Store.ReplaceMessageRecipients(
			directID, "from", []int64{senderID}, []string{label}))
		require.NoError(f.Store.ReplaceMessageRecipients(
			directID, "to", []int64{ownerParticipant}, []string{"Owner"}))
	}
	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 1,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	survivor, err = f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	return f, survivor, absorbed, messageID
}

func TestMergePersons_ParticipantMergeRejectsCrossOriginLineage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant("lineage-survivor@example.com", "Survivor", "example.com")
	survivorAlias := f.EnsureParticipant(
		"lineage-survivor-alias@example.com", "Survivor Alias", "example.com")
	absorbedParticipant := f.EnsureParticipant("lineage-absorbed@example.com", "Absorbed", "example.com")
	_, err := f.Store.LinkParticipants(survivorParticipant, survivorAlias)
	require.NoError(err)
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	result, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-participant-lineage", Actor: "test",
	})
	require.NoError(err)

	err = f.Store.MergeParticipants(absorbedParticipant, survivorParticipant)
	require.ErrorIs(err, store.ErrPersonMergeLineageConflict)
	var lineageCount int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*)
		FROM person_merge_participants WHERE merge_id = ?`),
		result.Merge.ID).Scan(&lineageCount))
	assert.Equal(3, lineageCount)
	var participantCount int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*)
		FROM participants WHERE id IN (?, ?)`), absorbedParticipant, survivorParticipant).
		Scan(&participantCount))
	assert.Equal(2, participantCount, "the rejected consolidation must roll back")

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: result.Person.ID, MergeID: result.Merge.ID,
		ParticipantIDs:         []int64{absorbedParticipant},
		ExpectedSourceRevision: result.Person.Revision,
		IdempotencyKey:         "split-after-rejected-participant-merge", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.ElementsMatch(
		[]int64{survivorParticipant, survivorAlias}, split.SourcePerson.ParticipantIDs)
	assert.Equal([]int64{absorbedParticipant}, split.NewPerson.ParticipantIDs)
}

func TestMergePersons_ParticipantMergeRejectsDistinctPartialSplitLineage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant(
		"split-lineage-survivor@example.com", "Survivor", "example.com")
	firstAbsorbed := f.EnsureParticipant(
		"split-lineage-first@example.com", "First", "example.com")
	secondAbsorbed := f.EnsureParticipant(
		"split-lineage-second@example.com", "Second", "example.com")
	remainingAbsorbed := f.EnsureParticipant(
		"split-lineage-remaining@example.com", "Remaining", "example.com")
	_, err := f.Store.LinkParticipants(firstAbsorbed, secondAbsorbed)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(firstAbsorbed, remainingAbsorbed)
	require.NoError(err)
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(firstAbsorbed)
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-distinct-split-lineage", Actor: "test",
	})
	require.NoError(err)
	firstSplit, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         []int64{firstAbsorbed},
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "first-partial-lineage", Actor: "test",
	})
	require.NoError(err)
	assert.False(firstSplit.ExactReversal)
	secondSplit, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: firstSplit.SourcePerson.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         []int64{secondAbsorbed},
		ExpectedSourceRevision: firstSplit.SourcePerson.Revision,
		IdempotencyKey:         "second-partial-lineage", Actor: "test",
	})
	require.NoError(err)
	assert.False(secondSplit.ExactReversal)
	assert.NotEqual(firstSplit.Split.ID, secondSplit.Split.ID)

	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(
		`DELETE FROM person_participants WHERE participant_id IN (?, ?)`),
		firstAbsorbed, secondAbsorbed)
	require.NoError(err)
	err = f.Store.MergeParticipants(firstAbsorbed, secondAbsorbed)
	require.ErrorIs(err, store.ErrPersonMergeLineageConflict)
	var lineageCount, participantCount int
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT COUNT(*)
		FROM person_merge_participants
		WHERE merge_id = ? AND participant_id IN (?, ?) AND split_id IN (?, ?)`),
		merged.Merge.ID, firstAbsorbed, secondAbsorbed,
		firstSplit.Split.ID, secondSplit.Split.ID).Scan(&lineageCount))
	assert.Equal(2, lineageCount)
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT COUNT(*)
		FROM participants WHERE id IN (?, ?)`), firstAbsorbed, secondAbsorbed).
		Scan(&participantCount))
	assert.Equal(2, participantCount, "the rejected consolidation must roll back")
}

func TestMergePersons_DeleteCurrentLineageOwnerIsDomainConflict(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant("delete-lineage-survivor@example.com", "Survivor", "example.com")
	absorbedParticipant := f.EnsureParticipant("delete-lineage-absorbed@example.com", "Absorbed", "example.com")
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	result, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-delete-lineage-owner", Actor: "test",
	})
	require.NoError(err)

	err = f.Store.DeletePersonContext(t.Context(), result.Person.ID, result.Person.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrPersonMergeActive)
	_, getErr := f.Store.GetPersonContext(t.Context(), result.Person.ID)
	require.NoError(getErr)
}

func TestMergePersons_ChainedReplayAndLineageJournal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	participants := []int64{
		f.EnsureParticipant("chain-a@example.com", "A", "example.com"),
		f.EnsureParticipant("chain-b@example.com", "B", "example.com"),
		f.EnsureParticipant("chain-c@example.com", "C", "example.com"),
	}
	people := make([]*store.Person, 0, len(participants))
	var err error
	for _, participantID := range participants {
		person, _, createErr := f.Store.CreatePersonFromParticipant(participantID)
		require.NoError(createErr)
		people = append(people, person)
	}
	for personID, channel := range map[int64]string{people[0].ID: "email", people[1].ID: "chat"} {
		_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &channel},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	people[0], err = f.Store.GetPersonContext(t.Context(), people[0].ID)
	require.NoError(err)
	people[1], err = f.Store.GetPersonContext(t.Context(), people[1].ID)
	require.NoError(err)
	firstRequest := store.PersonMergeRequest{
		SurvivorID: people[0].ID, AbsorbedID: people[1].ID,
		ExpectedSurvivorRevision: people[0].Revision,
		ExpectedAbsorbedRevision: people[1].Revision,
		IdempotencyKey:           "merge-chain-first", Actor: "test",
	}
	first, err := f.Store.MergePersonsContext(t.Context(), firstRequest)
	require.NoError(err)
	require.Len(first.ReviewCandidates, 1)

	currentA, err := f.Store.GetPersonContext(t.Context(), people[0].ID)
	require.NoError(err)
	currentC, err := f.Store.GetPersonContext(t.Context(), people[2].ID)
	require.NoError(err)
	second, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: currentC.ID, AbsorbedID: currentA.ID,
		ExpectedSurvivorRevision: currentC.Revision,
		ExpectedAbsorbedRevision: currentA.Revision,
		IdempotencyKey:           "merge-chain-second", Actor: "test",
	})
	require.NoError(err)

	for _, want := range []struct {
		table string
		rowID int64
	}{
		{table: "person_merges", rowID: first.Merge.ID},
		{table: "person_merge_review_candidates", rowID: first.ReviewCandidates[0].ID},
	} {
		var action, key string
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT action, original_row_key
			FROM person_merge_rows
			WHERE merge_id = ? AND table_name = ? AND original_row_id = ?`),
			second.Merge.ID, want.table, want.rowID).Scan(&action, &key))
		assert.Equal("repointed", action)
		assert.NotEmpty(key)
	}

	replayed, err := f.Store.MergePersonsContext(t.Context(), firstRequest)
	require.NoError(err)
	assertJSONEquivalent(t, first, replayed,
		"idempotency must replay the original committed response")
}

func TestMergePersons_Rollback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	survivorParticipant, err := st.EnsureParticipant("rollback-survivor@example.com", "Survivor", "example.com")
	require.NoError(err)
	absorbedParticipant, err := st.EnsureParticipant("rollback-absorbed@example.com", "Absorbed", "example.com")
	require.NoError(err)
	survivor, _, err := st.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	_, err = st.AddPersonNameContext(t.Context(), absorbed.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Must Survive Rollback"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	absorbed, err = st.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_person_merge_alias
		BEFORE INSERT ON person_uid_aliases BEGIN
			SELECT RAISE(ABORT, 'forced merge rollback');
		END`)
	require.NoError(err)

	_, err = st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "merge-rollback", Actor: "test",
	})
	require.Error(err)
	assert.Contains(err.Error(), "forced merge rollback")

	unchangedSurvivor, err := st.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.Equal([]int64{survivorParticipant}, unchangedSurvivor.ParticipantIDs)
	unchangedAbsorbed, err := st.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	assert.Equal([]int64{absorbedParticipant}, unchangedAbsorbed.ParticipantIDs)
	profile, err := st.GetPersonProfileContext(t.Context(), absorbed.ID)
	require.NoError(err)
	assert.Len(profile.Names, 1)
	var mergeCount int
	require.NoError(st.DB().QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM person_merges WHERE idempotency_key = 'merge-rollback'`,
	).Scan(&mergeCount))
	assert.Zero(mergeCount)
}

func TestPersonMergeRollbackStages(t *testing.T) {
	stages := []struct {
		name, event, table, prepare string
	}{
		{name: "after snapshot insertion", event: "INSERT", table: "person_merge_participants"},
		{name: "midway row policies", event: "UPDATE", table: "person_names", prepare: "name"},
		{name: "alias retargeting", event: "UPDATE", table: "person_uid_aliases", prepare: "alias"},
		{name: "after root deletion", event: "INSERT", table: "person_uid_aliases"},
	}
	for index, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ctx := t.Context()
			st := testutil.NewTestStore(t)
			key := fmt.Sprintf("rollback-stage-%d", index)
			survivor := mustPromotedPerson(t.Context(), t, st, key+"-survivor@example.com", "Survivor")
			absorbed := mustPromotedPerson(t.Context(), t, st, key+"-absorbed@example.com", "Absorbed")
			switch stage.prepare {
			case "name":
				_, err := st.AddPersonNameContext(ctx, absorbed.ID, store.PersonNameInput{
					NameKind: store.PersonNameFormatted, Formatted: new("Rollback Name"),
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
				require.NoError(err)
			case "alias":
				_, err := st.RetirePersonUIDAliasContext(
					ctx, key+"-retired-uid", &absorbed.ID, "test")
				require.NoError(err)
			}
			var err error
			survivor, err = st.GetPersonContext(ctx, survivor.ID)
			require.NoError(err)
			absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
			require.NoError(err)
			installPersonMergeFailureTrigger(t, st, index, stage.event, stage.table)

			_, err = st.MergePersonsContext(ctx, store.PersonMergeRequest{
				SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
				ExpectedSurvivorRevision: survivor.Revision,
				ExpectedAbsorbedRevision: absorbed.Revision,
				IdempotencyKey:           key, Actor: "test",
			})
			require.Error(err)
			assert.Contains(err.Error(), "forced merge rollback stage")
			for _, before := range []*store.Person{survivor, absorbed} {
				after, getErr := st.GetPersonContext(ctx, before.ID)
				require.NoError(getErr)
				assert.Equal(before.Revision, after.Revision)
				assert.Equal(before.ParticipantIDs, after.ParticipantIDs)
			}
			assertPersonMergeConcurrencyState(t, st, 0)
		})
	}
}

func installPersonMergeFailureTrigger(
	t *testing.T, st *store.Store, index int, event, table string,
) {
	t.Helper()
	triggerName := fmt.Sprintf("fail_person_merge_stage_%d", index)
	if st.IsPostgreSQL() {
		functionName := triggerName + "_fn"
		_, err := st.DB().ExecContext(t.Context(), fmt.Sprintf(`
			CREATE FUNCTION %s() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'forced merge rollback stage'; END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER %s BEFORE %s ON %s
			FOR EACH ROW EXECUTE FUNCTION %s();`,
			functionName, triggerName, event, table, functionName))
		require.NoError(t, err)
		return
	}
	_, err := st.DB().ExecContext(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE %s ON %s BEGIN
			SELECT RAISE(ABORT, 'forced merge rollback stage');
		END`, triggerName, event, table))
	require.NoError(t, err)
}

func TestMergePersons_ErrorHygiene(t *testing.T) {
	t.Run("participant IDs", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewSQLiteTestStore(t)
		_, err := st.EnsureParticipant("error-participant-dummy@example.com", "Dummy", "example.com")
		require.NoError(err)
		survivorParticipant, err := st.EnsureParticipant(
			"error-participant-survivor@example.com", "Survivor", "example.com",
		)
		require.NoError(err)
		absorbedParticipant, err := st.EnsureParticipant(
			"error-participant-absorbed@example.com", "Absorbed", "example.com",
		)
		require.NoError(err)
		survivor, _, err := st.CreatePersonFromParticipant(survivorParticipant)
		require.NoError(err)
		absorbed, _, err := st.CreatePersonFromParticipant(absorbedParticipant)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_person_merge_participant
			BEFORE INSERT ON person_merge_participants BEGIN
				SELECT RAISE(ABORT, 'participant journal failure');
			END`)
		require.NoError(err)

		_, err = st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "merge-error-participant", Actor: "test",
		})
		require.Error(err)
		assert.NotContains(err.Error(), strconv.FormatInt(survivorParticipant, 10))
		assert.NotContains(err.Error(), strconv.FormatInt(absorbedParticipant, 10))
	})

	t.Run("serialized row keys", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewSQLiteTestStore(t)
		survivorParticipant, err := st.EnsureParticipant(
			"error-row-survivor@example.com", "Survivor", "example.com",
		)
		require.NoError(err)
		absorbedParticipant, err := st.EnsureParticipant(
			"error-row-absorbed@example.com", "Absorbed", "example.com",
		)
		require.NoError(err)
		survivor, _, err := st.CreatePersonFromParticipant(survivorParticipant)
		require.NoError(err)
		absorbed, _, err := st.CreatePersonFromParticipant(absorbedParticipant)
		require.NoError(err)
		privateUID := "private-retired-person-uid"
		_, err = st.RetirePersonUIDAliasContext(
			t.Context(), privateUID, &absorbed.ID, "test",
		)
		require.NoError(err)
		absorbed, err = st.GetPersonContext(t.Context(), absorbed.ID)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_person_merge_row
			BEFORE INSERT ON person_merge_rows BEGIN
				SELECT RAISE(ABORT, 'row journal failure');
			END`)
		require.NoError(err)

		_, err = st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "merge-error-row", Actor: "test",
		})
		require.Error(err)
		assert.NotContains(err.Error(), privateUID)
	})
}

func TestPersonMergeConcurrencyMergeMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, survivor, absorbed := newPersonMergeConcurrencyFixture(t, "merge-merge")
	release := personOperationContentionBarrier(t, st, 2)
	results := make(chan error, 2)
	for _, key := range []string{"concurrent-merge-a", "concurrent-merge-b"} {
		go func() {
			_, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
				SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
				ExpectedSurvivorRevision: survivor.Revision,
				ExpectedAbsorbedRevision: absorbed.Revision,
				IdempotencyKey:           key, Actor: "test",
			})
			results <- err
		}()
	}
	release()
	errs := []error{<-results, <-results}
	assert.Equal(1, countNilErrors(errs), "exactly one conflicting merge may commit")
	for _, err := range errs {
		if err != nil {
			assert.True(errors.Is(err, store.ErrPersonRevisionConflict) ||
				errors.Is(err, store.ErrPersonNotFound),
				"loser must report a typed stale-person error: %v", err)
		}
	}
	current, err := st.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.Equal(survivor.Revision+1, current.Revision)
	assertPersonMergeConcurrencyState(t, st, 1)
}

func personOperationContentionBarrier(
	t *testing.T, st *store.Store, operations int,
) func() {
	t.Helper()
	arrived := make(chan struct{}, operations)
	gate := make(chan struct{})
	restore := st.SetPersonOperationBeforeIdentityLockHookForTest(func() {
		select {
		case arrived <- struct{}{}:
		case <-gate:
			return
		}
		<-gate
	})
	t.Cleanup(restore)
	return func() {
		t.Helper()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for range operations {
			select {
			case <-arrived:
			case <-timer.C:
				close(gate)
				require.FailNow(t, "person-operation contention barrier was not reached")
			}
		}
		close(gate)
	}
}

func TestPersonMergeConcurrencyProfileUpdate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, survivor, absorbed := newPersonMergeConcurrencyFixture(t, "merge-profile")
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "concurrent-merge-profile", Actor: "test",
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := st.UpdatePersonDisplayNameContext(
			t.Context(), survivor.ID, survivor.Revision, new("Concurrent Name"))
		results <- err
	}()
	close(start)
	errs := []error{<-results, <-results}
	assert.Equal(1, countNilErrors(errs), "merge and stale profile update cannot both commit")
	current, err := st.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.Equal(survivor.Revision+1, current.Revision)
	var mergeCount int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_merges`).Scan(&mergeCount))
	assert.Contains([]int{0, 1}, mergeCount)
	assertPersonMergeConcurrencyState(t, st, mergeCount)
}

func TestPersonMergeConcurrencyIdentityLink(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, survivor, absorbed := newPersonMergeConcurrencyFixture(t, "merge-link")
	left, right := survivor.ParticipantIDs[0], absorbed.ParticipantIDs[0]
	start := make(chan struct{})
	mergeDone := make(chan error, 1)
	linkDone := make(chan error, 1)
	go func() {
		<-start
		_, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "concurrent-merge-link", Actor: "test",
		})
		mergeDone <- err
	}()
	go func() {
		<-start
		_, err := st.LinkParticipants(left, right)
		linkDone <- err
	}()
	close(start)
	require.NoError(<-mergeDone)
	linkErr := <-linkDone
	if linkErr != nil {
		require.ErrorIs(linkErr, store.ErrPersonBindingConflict)
	}
	current, err := st.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.Equal(survivor.Revision+1, current.Revision)
	assertPersonMergeConcurrencyState(t, st, 1)
}

func newPersonMergeConcurrencyFixture(
	t *testing.T, key string,
) (*store.Store, *store.Person, *store.Person) {
	t.Helper()
	st := testutil.NewTestStore(t)
	survivor := mustPromotedPerson(t.Context(), t, st, key+"-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t.Context(), t, st, key+"-absorbed@example.com", "Absorbed")
	return st, survivor, absorbed
}

func countNilErrors(errs []error) int {
	count := 0
	for _, err := range errs {
		if err == nil {
			count++
		}
	}
	return count
}

func assertPersonMergeConcurrencyState(t *testing.T, st *store.Store, wantMerges int) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	var mergeCount, orphanRows, orphanParticipants int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_merges`).Scan(&mergeCount))
	assert.Equal(wantMerges, mergeCount)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_merge_rows row_record
		LEFT JOIN person_merges merge_record ON merge_record.id = row_record.merge_id
		WHERE merge_record.id IS NULL`).Scan(&orphanRows))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_merge_participants lineage
		LEFT JOIN person_merges merge_record ON merge_record.id = lineage.merge_id
		WHERE merge_record.id IS NULL`).Scan(&orphanParticipants))
	assert.Zero(orphanRows)
	assert.Zero(orphanParticipants)
	assertSQLiteForeignKeysClean(t, st)
}

func assertSQLiteForeignKeysClean(t *testing.T, st *store.Store) {
	t.Helper()
	if st.IsPostgreSQL() {
		return
	}
	rows, err := st.DB().QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	assert.False(t, rows.Next(), "foreign_key_check must return no violations")
	require.NoError(t, rows.Err())
}

var personMergeTableColumns = map[string][]string{
	"person_merges": {
		"absorbed_person_id", "absorbed_revision_before", "absorbed_uid", "actor",
		"created_at", "current_person_id", "id", "idempotency_key", "identity_revision", "request_hash",
		"result_json", "snapshot_blob", "snapshot_sha256", "snapshot_version",
		"survivor_person_id_at_merge", "survivor_revision_after",
		"survivor_revision_before", "survivor_uid",
	},
	"person_splits": {
		"actor", "created_at", "id", "idempotency_key", "identity_revision", "is_exact_reversal",
		"merge_id", "new_person_id", "new_person_uid", "request_hash",
		"result_json", "source_person_id", "source_revision_after", "source_revision_before",
	},
	"person_merge_participants": {
		"merge_id", "origin_side", "participant_id", "split_id",
	},
	"person_merge_rows": {
		"action", "current_row_id", "current_row_key", "merge_id", "original_row_id",
		"original_row_key", "origin_side", "participant_id", "provenance_kind",
		"post_merge_row_json", "snapshot_path", "split_id", "table_name",
	},
	"person_merge_row_person_refs": {
		"column_name", "merge_id", "original_row_key", "person_id", "table_name",
	},
	"person_merge_review_candidates": {
		"absorbed_value_id", "created_at", "definition_id", "id", "merge_id",
		"resolution_value_id", "reviewed_at", "reviewed_by", "state",
		"survivor_person_id", "survivor_value_id",
	},
}

func TestPersonMergeSchema(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)

	assertPersonMergeSchema(t, st)
}

func TestPostgresPersonMergeSchema(t *testing.T) {
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL-only person merge schema assertion")
	}

	assertPersonMergeSchema(t, st)
	var currentRowIDType string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT data_type
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'person_merge_rows'
		  AND column_name = 'current_row_id'`).Scan(&currentRowIDType))
	assert.Equal(t, "bigint", currentRowIDType)
}

func TestPostgresMergePersonsReconcilesSingleAttributes(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL-only merge lock assertion")
	}
	ctx := t.Context()
	survivorParticipant, err := st.EnsureParticipant(
		"pg-merge-survivor@example.com", "Survivor", "example.com",
	)
	require.NoError(err)
	absorbedParticipant, err := st.EnsureParticipant(
		"pg-merge-absorbed@example.com", "Absorbed", "example.com",
	)
	require.NoError(err)
	survivor, _, err := st.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	for personID, value := range map[int64]string{survivor.ID: "email", absorbed.ID: "chat"} {
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &value},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	jsonDefinition := personTextDefinition("postgres_merge_json_equivalence")
	jsonDefinition.UniversalID = "test-postgres-merge-json-equivalence"
	jsonDefinition.ValueType = store.AttributeValueJSON
	jsonDefinition.FieldType = store.AttributeFieldJSON
	_, err = st.CreateAttributeDefinitionContext(ctx, jsonDefinition)
	require.NoError(err)
	for personID, value := range map[int64]json.RawMessage{
		survivor.ID: json.RawMessage(`{"a":1,"b":2}`),
		absorbed.ID: json.RawMessage(`{"b":2,"a":1}`),
	} {
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: jsonDefinition.Slug,
			Value:  store.AttributeValue{Type: store.AttributeValueJSON, JSON: value},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)

	result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "postgres-single-attribute-merge", Actor: "test",
	})
	require.NoError(err)
	assert.Len(t, result.ReviewCandidates, 1)
}

func TestPostgresMergePersonsFencesConcurrentEnvelopeWriter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL merge/envelope interleaving regression")
	}
	ctx := t.Context()
	survivorParticipant, err := st.EnsureParticipant(
		"pg-envelope-merge-survivor@example.com", "Survivor", "example.com",
	)
	require.NoError(err)
	absorbedParticipant, err := st.EnsureParticipant(
		"pg-envelope-merge-absorbed@example.com", "Absorbed", "example.com",
	)
	require.NoError(err)
	survivor, _, err := st.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Absorbed\r\nEND:VCARD\r\n")
	envelope := parseStoreEnvelope(t, raw, "pg-merge-book", "pg-merge-envelope")
	envelope.CanonicalPersonUID = absorbed.VCardUID
	stored, err := st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
		PersonID: absorbed.ID, Envelope: envelope,
	})
	require.NoError(err)
	replacement := replaceStoreFormattedName(t, stored.ResourceEnvelope, "Stale Replacement")

	gate := openPostgreSQLUpdateGate(ctx, t, st, 638901,
		"vcard_resource_envelopes", stored.ID, "wait_for_person_merge_envelope_move")
	type mergeOutcome struct {
		result *store.PersonMergeResult
		err    error
	}
	mergeDone := make(chan mergeOutcome, 1)
	gate.run(func() {
		result, mergeErr := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "postgres-envelope-fence", Actor: "test",
		})
		mergeDone <- mergeOutcome{result: result, err: mergeErr}
	})
	mergePID := waitForPostgreSQLBlockedPID(ctx, t, st, gate.holderPID,
		"UPDATE vcard_resource_envelopes", "merge did not reach the envelope ownership move")

	writerDone := make(chan error, 1)
	gate.run(func() {
		expected := stored.Revision
		_, writeErr := st.PutVCardResourceEnvelopeContext(t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: absorbed.ID, ExpectedRevision: &expected, Envelope: replacement,
		})
		writerDone <- writeErr
	})
	require.True(waitForPostgreSQLBlockedBy(ctx, t, st, mergePID,
		"UPDATE vcard_resource_envelopes"), "stale writer did not wait behind merge fence")

	gate.release(t.Context())
	merged := <-mergeDone
	require.NoError(merged.err)
	require.NotNil(merged.result)
	require.ErrorIs(<-writerDone, store.ErrVCardResourceWriteConflict)
	moved, err := st.GetVCardResourceEnvelopeContext(ctx, "pg-merge-book", "pg-merge-envelope")
	require.NoError(err)
	assert.Equal(survivor.ID, moved.PersonID)
	assert.Equal(stored.Revision+1, moved.Revision)
}

func TestPostgresMergePersonsFencesConcurrentReferenceSupersede(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL merge/reference supersede interleaving regression")
	}
	ctx := t.Context()
	survivor := mustPromotedPerson(t.Context(), t, st,
		"pg-reference-merge-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t.Context(), t, st,
		"pg-reference-merge-absorbed@example.com", "Absorbed")
	observer := mustPromotedPerson(t.Context(), t, st,
		"pg-reference-merge-observer@example.com", "Observer")
	definition := personTextDefinition("pg_merge_reference_supersede")
	definition.ValueType = store.AttributeValueRecordReference
	definition.FieldType = store.AttributeFieldPerson
	definition.RecordTarget = new("person")
	_, err := st.CreateAttributeDefinitionContext(ctx, definition)
	require.NoError(err)
	write, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: observer.ID, DefinitionSlug: definition.Slug,
		Value: store.AttributeValue{
			Type: store.AttributeValueRecordReference, RecordType: new("person"),
			RecordID: &absorbed.ID,
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)

	snapshotCaptured := make(chan struct{}, 1)
	releaseMerge := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseMerge) }) }
	t.Cleanup(release)
	restoreHook := st.SetPersonMergeAfterSnapshotHookForTest(func() {
		select {
		case snapshotCaptured <- struct{}{}:
		default:
		}
		<-releaseMerge
	})
	t.Cleanup(restoreHook)
	type mergeOutcome struct {
		result *store.PersonMergeResult
		err    error
	}
	mergeDone := make(chan mergeOutcome, 1)
	go func() {
		result, mergeErr := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "postgres-reference-supersede-merge", Actor: "test",
		})
		mergeDone <- mergeOutcome{result: result, err: mergeErr}
	}()
	select {
	case <-snapshotCaptured:
	case <-time.After(10 * time.Second):
		release()
		require.FailNow("merge did not pause after capturing its reversal snapshot")
	}

	type supersedeOutcome struct {
		write *store.PersonAttributeWrite
		err   error
	}
	supersedeDone := make(chan supersedeOutcome, 1)
	go func() {
		result, supersedeErr := st.SupersedePersonAttributeValueContext(
			t.Context(), store.PersonAttributeSupersedeInput{
				PersonID: observer.ID, DefinitionSlug: definition.Slug,
				ExpectedValueID: &write.Value.ID,
			})
		supersedeDone <- supersedeOutcome{write: result, err: supersedeErr}
	}()
	select {
	case early := <-supersedeDone:
		release()
		require.FailNow("reference supersede did not wait for merge identity lock",
			"result=%v err=%v", early.write, early.err)
	case <-time.After(500 * time.Millisecond): //nolint:kennlint // deadlinetest: absence check: the held merge identity lock keeps the supersede waiting
	}
	release()
	merged := <-mergeDone
	require.NoError(merged.err)
	require.NotNil(merged.result)
	superseded := <-supersedeDone
	require.NoError(superseded.err)
	require.NotNil(superseded.write)

	current, err := st.GetPersonContext(ctx, merged.result.Person.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: merged.result.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "postgres-reference-supersede-split", Actor: "test",
	})
	require.NoError(err)
	values, err := st.ListPersonAttributeValuesContext(ctx, observer.ID,
		store.PersonAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
	require.NoError(err)
	require.Len(values, 1)
	require.NotNil(values[0].Value.RecordID)
	assert.Equal(split.NewPerson.ID, *values[0].Value.RecordID)
	assert.NotNil(values[0].ActiveUntil)
	assert.NotNil(values[0].SupersededAt)
}

func assertPersonMergeSchema(t *testing.T, st *store.Store) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	for table, want := range personMergeTableColumns {
		got := func() []string {
			rows, err := st.DB().QueryContext(ctx, "SELECT * FROM "+table+" WHERE 1 = 0")
			require.NoError(err, "query %s", table)
			defer func() { require.NoError(rows.Close(), "close %s column query", table) }()
			columns, err := rows.Columns()
			require.NoError(err, "read %s columns", table)
			require.NoError(rows.Err(), "iterate %s column query", table)
			return columns
		}()
		sort.Strings(got)
		sort.Strings(want)
		assert.Equal(want, got, "columns for %s", table)
	}

	assertForeignKeyTarget(t, st, "person_merges", "current_person_id", "persons")
	assertForeignKeyTarget(t, st, "person_splits", "merge_id", "person_merges")
	assertForeignKeyTarget(t, st, "person_merge_participants", "participant_id", "participants")
	assertForeignKeyTarget(t, st, "person_merge_participants", "split_id", "person_splits")
	assertForeignKeyTarget(t, st, "person_merge_rows", "split_id", "person_splits")
	assertForeignKeyTarget(t, st, "person_merge_review_candidates", "definition_id", "attribute_definitions")
}

func assertForeignKeyTarget(t *testing.T, st *store.Store, table, column, target string) {
	t.Helper()
	query := `SELECT "table" FROM pragma_foreign_key_list(?) WHERE "from" = ?`
	args := []any{table, column}
	if st.IsPostgreSQL() {
		query = `SELECT ccu.table_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON tc.constraint_name = kcu.constraint_name
			 AND tc.constraint_schema = kcu.constraint_schema
			JOIN information_schema.constraint_column_usage ccu
			  ON tc.constraint_name = ccu.constraint_name
			 AND tc.constraint_schema = ccu.constraint_schema
			WHERE tc.constraint_type = 'FOREIGN KEY'
			  AND tc.table_schema = current_schema()
			  AND tc.table_name = ?
			  AND kcu.column_name = ?`
	}
	var got string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(query), args...).Scan(&got),
		"foreign key %s.%s", table, column)
	assert.Equal(t, target, got, "foreign key target for %s.%s", table, column)
}

func TestMergePersons_MovesBriefEnrollmentToSurvivor(t *testing.T) {
	for _, test := range []struct {
		name           string
		enrollSurvivor bool
		wantAction     string
	}{
		{name: "absorbed-only", wantAction: "moved"},
		{name: "both-enrolled", enrollSurvivor: true, wantAction: "deduplicated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ctx := t.Context()
			st := testutil.NewTestStore(t)
			survivor := mustPromotedPerson(t.Context(), t, st,
				"brief-survivor-"+test.name+"@example.com", "Survivor")
			absorbed := mustPromotedPerson(t.Context(), t, st,
				"brief-absorbed-"+test.name+"@example.com", "Absorbed")
			if test.enrollSurvivor {
				_, err := st.SetPersonBriefEnrollmentContext(ctx, survivor.ID, true, "owner", true)
				require.NoError(err)
			}
			_, err := st.SetPersonBriefEnrollmentContext(ctx, absorbed.ID, true, "owner", true)
			require.NoError(err)

			merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
				SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
				ExpectedSurvivorRevision: survivor.Revision,
				ExpectedAbsorbedRevision: absorbed.Revision,
				IdempotencyKey:           "brief-enrollment-merge-" + test.name, Actor: "test",
			})
			require.NoError(err)

			enrollment, err := st.GetPersonBriefEnrollmentContext(ctx, merged.Person.ID)
			require.NoError(err)
			assert.True(enrollment.Enrolled,
				"the survivor keeps the enrollment the merge consumed")
			tracking, err := st.GetPersonTrackingContext(ctx, merged.Person.ID)
			require.NoError(err)
			assert.True(tracking.Tracked,
				"an enrolled survivor must still have the tracking row enrollment requires")

			var action string
			require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
				SELECT action FROM person_merge_rows
				WHERE merge_id = ? AND table_name = 'person_brief_enrollments'`),
				merged.Merge.ID).Scan(&action))
			assert.Equal(test.wantAction, action)
		})
	}
}

func TestPersonMerge_DetailAddressLabels(t *testing.T) {
	t.Run("mailbox-case", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		owner, guest := mailboxCaseMergePeople(t, st)
		merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: owner.ID, AbsorbedID: guest.ID, ExpectedSurvivorRevision: owner.Revision, ExpectedAbsorbedRevision: guest.Revision, IdempotencyKey: "mailbox-case", Actor: "test"})
		require.NoError(t, err)
		detail, err := st.GetPersonMergeContext(t.Context(), merged.Merge.ID)
		require.NoError(t, err)
		assertPersonMergeIdentityFixture(t, detail, "mailbox-case")
		assert.Equal(t, 1, detail.Labels.AbsorbedAddressCount)
	})
	t.Run("current bindings", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newPersonMergeInspectionFixture(t, "detail-current-labels")
		ctx := t.Context()
		added, err := f.store.EnsureParticipant("Later@Example.org", "Later sender", "example.org")
		require.NoError(err)
		current, err := f.store.GetPersonContext(ctx, f.merge.Person.ID)
		require.NoError(err)
		_, err = f.store.LinkParticipants(current.ParticipantIDs[0], added)
		require.NoError(err)
		require.NoError(f.store.SetParticipantIdentifierContext(ctx, added, "beeper", "later-handle"))
		absorbedLater, err := f.store.EnsureParticipant("absorbed-later@example.org", "Later absorbed sender", "example.org")
		require.NoError(err)
		_, err = f.store.LinkParticipants(f.absorbedParticipant, absorbedLater)
		require.NoError(err)
		detail, err := f.store.GetPersonMergeContext(ctx, f.merge.Merge.ID)
		require.NoError(err)
		assert.Contains(detail.Labels.ParticipantIDs, added)
		assert.Contains(detail.Participants, store.PersonMergeParticipant{MergeID: f.merge.Merge.ID, ParticipantID: added, OriginSide: "survivor"})
		assert.Contains(detail.Participants, store.PersonMergeParticipant{MergeID: f.merge.Merge.ID, ParticipantID: absorbedLater, OriginSide: "absorbed"})
		for _, profile := range detail.Labels.Profiles {
			assert.NotContains(profile.ParticipantIDs, added)
			assert.NotContains(profile.ParticipantIDs, absorbedLater)
		}
		assert.Contains(detail.Labels.Addresses, store.ParticipantAddress{ParticipantID: added, Type: "beeper", Value: "later-handle", NormalizedValue: "beeper:later-handle", ParticipantDisplayName: "Later sender", IdentityKey: `["","","","beeper:later-handle"]`})
		require.Len(detail.Labels.Profiles, 2)
		assert.Equal(new("Survivor"), detail.Labels.Profiles[0].DisplayName)
		assert.Equal(new("Absorbed"), detail.Labels.Profiles[1].DisplayName)
		assert.False(detail.Labels.Incomplete)
	})
	t.Run("saved and archived WhatsApp phone", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewTestStore(t)
		ctx := t.Context()
		// Older imports can leave a raw Beeper anchor that observation capture classifies in place.
		participant, err := st.EnsureParticipantByIdentifierContext(ctx, "beeper", "+12025550123", "Reader")
		require.NoError(err)
		service, err := st.ResolveCommunicationServiceContext(ctx, "whatsapp")
		require.NoError(err)
		require.NoError(st.ClassifyParticipantIdentifierServiceContext(ctx, "beeper", "+12025550123", &service.ID, nil, nil))
		guest, _, err := st.CreatePersonFromParticipant(participant)
		require.NoError(err)
		_, err = st.AddPersonContactPointContext(ctx, guest.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressPhone, ServiceSlug: new("whatsapp"), OriginalValue: "+1 (202) 555-0123", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceCardDAVImport}})
		require.NoError(err)
		guest, err = st.GetPersonContext(ctx, guest.ID)
		require.NoError(err)
		owner := mustPromotedPerson(ctx, t, st, "owner@example.org", "Owner")
		merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: owner.ID, AbsorbedID: guest.ID, ExpectedSurvivorRevision: owner.Revision, ExpectedAbsorbedRevision: guest.Revision, IdempotencyKey: "saved-whatsapp", Actor: "test"})
		require.NoError(err)
		detail, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
		require.NoError(err)
		assertPersonMergeIdentityFixture(t, detail, "beeper-raw-phone")
		points, err := st.ListPersonContactPointsContext(ctx, owner.ID, true)
		require.NoError(err)
		require.Len(points, 1)
		var archived []store.ParticipantAddress
		for _, address := range detail.Labels.Addresses {
			if address.ParticipantID == participant {
				archived = append(archived, address)
			}
		}
		require.Len(archived, 1)
		assert.Equal(points[0].IdentityKey, archived[0].IdentityKey)
		assert.Equal("+12025550123", archived[0].DisplayValue)
	})

	t.Run("phone aliases and saved contacts", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewTestStore(t)
		ctx := t.Context()
		owner, guest := beeperPhoneMergePeople(t, st)
		_, err := st.AddPersonContactPointContext(ctx, guest.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550123", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		require.NoError(err)
		guest, err = st.GetPersonContext(ctx, guest.ID)
		require.NoError(err)
		merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: owner.ID, AbsorbedID: guest.ID, ExpectedSurvivorRevision: owner.Revision, ExpectedAbsorbedRevision: guest.Revision, IdempotencyKey: "phone-count", Actor: "test"})
		require.NoError(err)
		detail, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
		require.NoError(err)

		assertPersonMergeIdentityFixture(t, detail, "beeper-phone")
		assert.Equal(2, detail.Labels.AbsorbedAddressCount)
		points, err := st.ListPersonContactPointsContext(ctx, owner.ID, true)
		require.NoError(err)
		require.Len(points, 1)
		assert.Equal(detail.Labels.Profiles[1].ContactPoints[0].IdentityKey, points[0].IdentityKey)
		assert.Equal(detail.Labels.Addresses[0].IdentityKey, detail.Labels.Profiles[1].ContactPoints[0].IdentityKey)
		require.NoError(st.SetParticipantIdentifierContext(ctx, guest.ParticipantIDs[0], "beeper", "+12025550123"))
		alias, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
		require.NoError(err)
		assert.Equal(detail.Labels.Addresses, alias.Labels.Addresses)
		assert.Equal(2, alias.Labels.AbsorbedAddressCount)
		service, err := st.ResolveCommunicationServiceContext(ctx, "whatsapp")
		require.NoError(err)
		require.NoError(st.ClassifyParticipantIdentifierServiceContext(ctx, "beeper", "+12025550123", &service.ID, new("account"), new("garden")))
		scoped, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
		require.NoError(err)
		assert.Contains(scoped.Labels.Addresses, store.ParticipantAddress{ParticipantID: guest.ParticipantIDs[0], Type: "beeper", Value: "+12025550123", DisplayValue: "+12025550123", NormalizedValue: "+12025550123", ServiceSlug: "whatsapp", ServiceLabel: "WhatsApp", ScopeKind: "account", ScopeValue: "garden", ParticipantDisplayName: "Reader", IdentityKey: `["whatsapp","account","garden","+12025550123"]`})
		assert.Equal(3, scoped.Labels.AbsorbedAddressCount)
		require.NoError(st.SetParticipantIdentifierContext(ctx, guest.ParticipantIDs[0], "beeper", "+1 (202) 555-0123"))
		require.NoError(st.ClassifyParticipantIdentifierServiceContext(ctx, "beeper", "+1 (202) 555-0123", &service.ID, new("account"), new("work")))
		scoped, err = st.GetPersonMergeContext(ctx, merged.Merge.ID)
		require.NoError(err)
		assert.Equal(4, scoped.Labels.AbsorbedAddressCount)
	})
	t.Run("opaque provider aliases retain case", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewTestStore(t)
		ctx := t.Context()
		owner := mustPromotedPerson(ctx, t, st, "owner@example.org", "Owner")
		participant, key := migratedBeeperParticipant(ctx, t, st)
		guest, _, err := st.CreatePersonFromParticipant(participant)
		require.NoError(err)
		merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: owner.ID, AbsorbedID: guest.ID, ExpectedSurvivorRevision: owner.Revision, ExpectedAbsorbedRevision: guest.Revision, IdempotencyKey: "opaque-case", Actor: "test"})
		require.NoError(err)
		detail, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
		require.NoError(err)
		assertPersonMergeIdentityFixture(t, detail, "beeper-migrated")
		require.NoError(st.SetParticipantIdentifierContext(ctx, participant, "beeper", strings.ToUpper(key)))
		service, err := st.ResolveCommunicationServiceContext(ctx, "whatsapp")
		require.NoError(err)
		require.NoError(st.ClassifyParticipantIdentifierServiceContext(ctx, "beeper", strings.ToUpper(key), &service.ID, new("account"), new("garden-account")))
		detail, err = st.GetPersonMergeContext(ctx, merged.Merge.ID)
		require.NoError(err)
		assert.Equal(t, 2, detail.Labels.AbsorbedAddressCount)
	})
	t.Run("resolved contacts exclude fallback anchor", func(t *testing.T) {
		for _, tc := range []struct {
			name, phone, email, kind, value, namespace string
		}{
			{"phone", "+12025550123", "", "phone", "+12025550123", "beeper"},
			{"email", "", "reader@example.org", "email", "reader@example.org", "beeper"},
			{"only an anchor", "", "", "beeper", beeperidentity.Fallback("account-a", "user-a"), "beeper"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				st := testutil.NewTestStore(t)
				ctx := t.Context()
				var participant int64
				var err error
				switch {
				case tc.phone != "":
					participant, err = st.EnsureParticipantByPhone(ctx, tc.phone, "Reader", tc.namespace)
				case tc.email != "":
					participant, err = st.EnsureParticipantContext(ctx, tc.email, "Reader", "example.org")
				default:
					participant, err = st.EnsureParticipantByIdentifierContext(ctx, "beeper", tc.value, "Reader")
				}
				require.NoError(err)
				if tc.namespace == "beeper" {
					require.NoError(st.SetParticipantIdentifierContext(ctx, participant, "beeper", beeperidentity.Fallback("account-a", "user-a")))
				}
				guest, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
				require.NoError(err)
				ownerID, err := st.EnsureParticipantContext(ctx, "owner@example.org", "Owner", "example.org")
				require.NoError(err)
				owner, _, err := st.CreatePersonFromParticipantContext(ctx, ownerID)
				require.NoError(err)
				merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: owner.ID, AbsorbedID: guest.ID, ExpectedSurvivorRevision: owner.Revision, ExpectedAbsorbedRevision: guest.Revision, IdempotencyKey: "beeper-anchor", Actor: "test"})
				require.NoError(err)
				detail, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
				require.NoError(err)
				var addresses []store.ParticipantAddress
				for _, address := range detail.Labels.Addresses {
					if address.ParticipantID == participant {
						addresses = append(addresses, address)
					}
				}
				require.Len(addresses, 1)
				assert.Equal(tc.kind, addresses[0].Type)
				assert.Equal(tc.value, addresses[0].Value)
				if tc.kind == "phone" {
					assert.Equal(`["phone","","","+12025550123"]`, addresses[0].IdentityKey)
				}
			})
		}
	})

}

func denyPersonMergeIdentityReads(t *testing.T, st *store.Store) {
	t.Helper()
	require := require.New(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		_, err := st.DB().ExecContext(t.Context(), `ALTER TABLE participant_identifiers RENAME TO unavailable_identifiers`)
		require.NoError(err)
		return
	}
	st.DB().SetMaxOpenConns(1)
	st.DB().SetMaxIdleConns(1)
	var fail atomic.Bool
	fail.Store(true)
	t.Cleanup(func() { fail.Store(false) })
	conn, connErr := st.DB().Conn(t.Context())
	require.NoError(connErr)
	err := conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(ok)
		sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
			if fail.Load() && action == sqlite3.SQLITE_READ && table == "participant_identifiers" {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	require.NoError(conn.Close())
	require.NoError(err)
}

func TestPersonMergeHistoryKeepsSavedContacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scoped bool
		inputs []store.PersonContactPointInput
		want   []store.PersonMergeRecordedContactPoint
	}{
		{"curated and superseded", false, []store.PersonContactPointInput{
			{AddressKind: store.ContactAddressEmail, OriginalValue: "curated@example.org", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport}},
		}, []store.PersonMergeRecordedContactPoint{
			{AddressKind: store.ContactAddressEmail, OriginalValue: "curated@example.org", NormalizedValue: "curated@example.org", IdentityKey: "[\"email\",\"\",\"\",\"curated@example.org\"]"},
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", NormalizedValue: "+12025550144", IdentityKey: "[\"phone\",\"\",\"\",\"+12025550144\"]"},
		}},
		{"service scope and inactive catalog", true, []store.PersonContactPointInput{
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", ServiceSlug: new("whatsapp")},
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", ServiceSlug: new("signal")},
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", ServiceSlug: new("scoped-phone"), ScopeKind: new("workspace"), ScopeValue: new("garden")},
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", ServiceSlug: new("scoped-phone"), ScopeKind: new("workspace"), ScopeValue: new("work")},
			{AddressKind: store.ContactAddressUsername, OriginalValue: "alex", ServiceSlug: new("slack"), ScopeKind: new("workspace"), ScopeValue: new("garden")},
		}, []store.PersonMergeRecordedContactPoint{
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", NormalizedValue: "+12025550144", ServiceSlug: new("whatsapp"), ServiceLabel: new("WhatsApp"), IdentityKey: "[\"whatsapp\",\"\",\"\",\"+12025550144\"]"},
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", NormalizedValue: "+12025550144", ServiceSlug: new("signal"), ServiceLabel: new("Signal"), IdentityKey: "[\"signal\",\"\",\"\",\"+12025550144\"]"},
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", NormalizedValue: "+12025550144", ServiceSlug: new("scoped-phone"), ServiceLabel: new("Scoped phone"), ScopeKind: new("workspace"), ScopeValue: new("garden"), IdentityKey: "[\"scoped-phone\",\"workspace\",\"garden\",\"+12025550144\"]"},
			{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550144", NormalizedValue: "+12025550144", ServiceSlug: new("scoped-phone"), ServiceLabel: new("Scoped phone"), ScopeKind: new("workspace"), ScopeValue: new("work"), IdentityKey: "[\"scoped-phone\",\"workspace\",\"work\",\"+12025550144\"]"},
			{AddressKind: store.ContactAddressUsername, OriginalValue: "alex", NormalizedValue: "alex", ServiceSlug: new("slack"), ServiceLabel: new("Slack"), ScopeKind: new("workspace"), ScopeValue: new("garden"), IdentityKey: "[\"slack\",\"workspace\",\"garden\",\"alex\"]"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := storetest.New(t).Store
			ctx := t.Context()
			owner := mustPromotedPerson(ctx, t, st, "owner@example.org", "Owner")
			guest := mustPromotedPerson(ctx, t, st, "guest@example.org", "Guest")

			var serviceID int64
			if tc.scoped {
				service, _, err := st.EnsureCommunicationServiceContext(ctx, store.CommunicationServiceInput{
					Slug: "scoped-phone", DisplayLabel: "Scoped phone", ScopePolicy: store.ScopePolicyRequired,
					DefaultScopeKind: new("workspace"), Normalization: store.NormalizationPhoneE164, NormalizationVersion: 1,
				})
				require.NoError(err)
				serviceID = service.ID

			} else {
				old, err := st.AddPersonContactPointContext(ctx, guest.ID, store.PersonContactPointInput{
					AddressKind: store.ContactAddressEmail, OriginalValue: "retired@example.org", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
				require.NoError(err)
				require.NoError(st.SupersedePersonContactPointContext(ctx, guest.ID, old.Envelope.ID, nil))
			}
			for _, input := range tc.inputs {
				if tc.scoped {
					input.Envelope = store.ValueEnvelopeInput{Source: store.ProvenanceUser}
				}
				_, err := st.AddPersonContactPointContext(ctx, guest.ID, input)
				require.NoError(err)
			}
			guest, err := st.GetPersonContext(ctx, guest.ID)
			require.NoError(err)
			merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: owner.ID, AbsorbedID: guest.ID, ExpectedSurvivorRevision: owner.Revision, ExpectedAbsorbedRevision: guest.Revision, IdempotencyKey: "saved-contact-points", Actor: "test"})
			require.NoError(err)
			if tc.scoped {
				_, err = st.SetCommunicationServiceActiveContext(ctx, serviceID, false)
				require.NoError(err)
			}
			detail, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
			require.NoError(err)
			listed, err := st.ListPersonMergesContext(ctx, owner.ID)
			require.NoError(err)
			require.Len(listed, 1)
			for _, labels := range []store.PersonMergeLabels{detail.Labels, listed[0].Labels} {
				var points []store.PersonMergeRecordedContactPoint
				for _, recorded := range labels.Profiles {
					if recorded.ID == guest.ID {
						points = recorded.ContactPoints
					}
				}
				assert.ElementsMatch(tc.want, points)
			}
		})
	}
}

func TestPersonMergeCountsNewAddresses(t *testing.T) {
	for _, tc := range []struct {
		name, survivorSaved, absorbedSaved string
		laterLink                          bool
		want                               int
	}{
		{name: "archived survivor", absorbedSaved: "A@Example.org", want: 1},
		{name: "saved survivor", survivorSaved: "shared@example.org", absorbedSaved: "Shared@Example.org", want: 1},
		{name: "zero new addresses", survivorSaved: "b@example.org", want: 0},
		{name: "later linked survivor", absorbedSaved: "later@example.org", laterLink: true, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			owner := mustPromotedPerson(ctx, t, st, "a@example.org", "Owner")
			guest := mustPromotedPerson(ctx, t, st, "b@example.org", "Guest")
			for id, value := range map[int64]string{owner.ID: tc.survivorSaved, guest.ID: tc.absorbedSaved} {
				if value != "" {
					_, err := st.AddPersonContactPointContext(ctx, id, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: value, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
					require.NoError(err)
				}
			}
			owner, err := st.GetPersonContext(ctx, owner.ID)
			require.NoError(err)
			guest, err = st.GetPersonContext(ctx, guest.ID)
			require.NoError(err)
			merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: owner.ID, AbsorbedID: guest.ID, ExpectedSurvivorRevision: owner.Revision, ExpectedAbsorbedRevision: guest.Revision, IdempotencyKey: "new-addresses", Actor: "test"})
			require.NoError(err)
			if tc.laterLink {
				later, err := st.EnsureParticipantContext(ctx, "later@example.org", "Later", "example.org")
				require.NoError(err)
				_, err = st.LinkParticipants(owner.ParticipantIDs[0], later)
				require.NoError(err)
			}
			detail, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
			require.NoError(err)
			page, err := st.ListPersonMergesPageContext(ctx, owner.ID, 100, 0)
			require.NoError(err)
			require.Len(page, 1)
			assert.Equal(tc.want, detail.Labels.AbsorbedAddressCount)
			assert.Equal(tc.want, page[0].Labels.AbsorbedAddressCount)
		})
	}
}
