package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func testInstruction(t *testing.T, st *store.Store, id int64) *store.PersonInstruction {
	t.Helper()
	row, err := st.GetPersonInstructionContext(t.Context(), id)
	require.NoError(t, err)
	return row
}
func splitInstructionMerge(t *testing.T, st *store.Store, merged *store.PersonMergeResult, participants []int64) *store.PersonSplitResult {
	t.Helper()
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: participants, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "instruction-split", Actor: "owner"})
	require.NoError(t, err)
	return split
}
func insertTestInstructionEvent(t *testing.T, st *store.Store, row *store.PersonInstruction, action string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO person_instruction_events(person_id,instruction_id,instruction_key,action,actor,detail_json) VALUES (?,?,?,?,'owner','{}') RETURNING id`), row.PersonID, row.ID, row.InstructionKey, action).Scan(&id))
	return id
}
func TestMergeMovesCuratedInstructionsAndDiscardsProposals(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	owner := addTestInstruction(t, st, bob.ID, "Vee")
	confirmed := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusActive)
	proposal := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindFormat, "", "use bullets", personfacts.InstructionStatusProposed)
	_, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	for _, id := range []int64{owner.ID, confirmed} {
		got := testInstruction(t, st, id)
		assert.Equal(t, alice.ID, got.PersonID)
		assert.Equal(t, personfacts.InstructionReviewMerged, got.ReviewState)
		assert.Equal(t, 1, instructionEventCount(t, st, id, "merge_moved"))
	}
	_, err = st.GetPersonInstructionContext(t.Context(), proposal)
	assert.ErrorIs(t, err, store.ErrInstructionNotFound)
}
func TestMergeCollisionAbsorbedOwnerOverSurvivorProposal(t *testing.T) {
	testInstructionCollision(t, "absorbed-owner")
}
func TestMergeCollisionSurvivorOwnerOverNewerAbsorbedConfirmed(t *testing.T) {
	testInstructionCollision(t, "survivor-owner")
}
func TestMergeCollisionAbsorbedConfirmedOverSurvivorProposal(t *testing.T) {
	testInstructionCollision(t, "absorbed-confirmed")
}
func TestMergeCollisionOwnerVersusOwnerLaterWins(t *testing.T) {
	testInstructionCollision(t, "both-owner")
}
func testInstructionCollision(t *testing.T, mode string) {
	t.Helper()
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	var survivor, absorbed *store.PersonInstruction
	if mode == "survivor-owner" || mode == "both-owner" {
		survivor = addTestInstruction(t, st, alice.ID, "Vee")
	} else {
		survivor = testInstruction(t, st, insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusProposed))
	}
	if mode == "absorbed-owner" || mode == "both-owner" {
		absorbed = addTestInstruction(t, st, bob.ID, "Vee")
		if mode == "both-owner" {
			_, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: absorbed.ID, ExpectedRevision: absorbed.Revision, Action: personfacts.InstructionActionRevoke, Actor: "owner"})
			require.NoError(t, err)
			absorbed = addTestInstruction(t, st, bob.ID, "Vee")
		}
	} else {
		absorbed = testInstruction(t, st, insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusActive))
	}
	later := time.Now().UTC().Add(time.Hour)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET updated_at=? WHERE id=?`), later, absorbed.ID)
	require.NoError(t, err)
	absorbed = testInstruction(t, st, absorbed.ID)
	source := absorbed
	sourceName := "absorbed"
	if mode == "survivor-owner" {
		source = survivor
		sourceName = "survivor"
	}
	merged, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	live, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
	require.NoError(t, err)
	require.Len(t, live, 1)
	got := testInstruction(t, st, survivor.ID)
	assert.Equal(t, source.Origin, got.Origin)
	assert.Equal(t, source.Status, got.Status)
	assert.Equal(t, source.StatedAt, got.StatedAt)
	assert.Equal(t, source.ConfirmedAt, got.ConfirmedAt)
	assert.Equal(t, source.ConfirmedBy, got.ConfirmedBy)
	assert.Equal(t, source.ValidFrom, got.ValidFrom)
	assert.Equal(t, source.ValidUntil, got.ValidUntil)
	assert.Equal(t, source.Adopted, got.Adopted)
	assert.Equal(t, survivor.InstructionKey, got.InstructionKey)
	var detail string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT detail_json FROM person_instruction_events WHERE instruction_id=? AND action='merge_moved'`), got.ID).Scan(&detail))
	assert.JSONEq(t, fmt.Sprintf(`{"absorbed_instruction_id":%d,"collision":true,"lifecycle_source":%q}`, absorbed.ID, sourceName), detail)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(t, split.ExactReversal)
	restoredSurvivor := testInstruction(t, st, survivor.ID)
	assert.Equal(t, survivor.Origin, restoredSurvivor.Origin)
	assert.Equal(t, survivor.Status, restoredSurvivor.Status)
	assert.Equal(t, survivor.StatedAt, restoredSurvivor.StatedAt)
	assert.Equal(t, survivor.ConfirmedAt, restoredSurvivor.ConfirmedAt)
	assert.Equal(t, survivor.ReviewState, restoredSurvivor.ReviewState)
	restored := testInstruction(t, st, absorbed.ID)
	assert.Equal(t, split.NewPerson.ID, restored.PersonID)
	assert.Equal(t, personfacts.InstructionReviewSplit, restored.ReviewState)
	assert.Greater(t, restored.Revision, absorbed.Revision)
}
func TestMergeDeduplicatedInstructionKeepsEvents(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	s := addTestInstruction(t, st, alice.ID, "Vee")
	a := addTestInstruction(t, st, bob.ID, "Vee")
	ids := []int64{}
	for _, action := range []string{"propose", "confirm", "revoke", "reject"} {
		ids = append(ids, insertTestInstructionEvent(t, st, a, action))
	}
	merged, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	check := func(personID, instructionID int64) {
		t.Helper()
		for _, id := range ids {
			var p int64
			var i sql.NullInt64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT person_id,instruction_id FROM person_instruction_events WHERE id=?`), id).Scan(&p, &i))
			assert.Equal(t, personID, p)
			require.True(t, i.Valid)
			assert.Equal(t, instructionID, i.Int64)
		}
	}
	check(alice.ID, s.ID)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(t, split.ExactReversal)
	check(split.NewPerson.ID, a.ID)
}
func TestMergeInheritsAbsorbedRejectionMemory(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	id := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusRejected)
	row := testInstruction(t, st, id)
	event := insertTestInstructionEvent(t, st, row, "reject")
	_, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	var personID int64
	var instructionID sql.NullInt64
	var key string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT person_id,instruction_id,instruction_key FROM person_instruction_events WHERE id=?`), event).Scan(&personID, &instructionID, &key))
	assert.Equal(t, alice.ID, personID)
	assert.False(t, instructionID.Valid)
	assert.Equal(t, row.InstructionKey, key)
}
func TestMergeFlagsSingleValuedConflict(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	first := addTestInstruction(t, st, alice.ID, "Vee")
	second := addTestInstruction(t, st, bob.ID, "Val")
	_, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, personfacts.InstructionReviewConflict, row.ReviewState)
		assert.True(t, row.VisibleToAgents)
	}
	assert.Equal(t, personfacts.InstructionStatusActive, testInstruction(t, st, first.ID).Status)
	assert.Equal(t, personfacts.InstructionStatusActive, testInstruction(t, st, second.ID).Status)
}
func TestExactSplitRestoresSupersessionAcrossDigitBoundary(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	filler := mustPromotedPerson(t, st, "filler@example.com", "filler")
	for range 8 {
		addTestInstruction(t, st, filler.ID, "Vee") /* each iteration needs a distinct historical ID */
		var id int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT MAX(id) FROM person_instructions`).Scan(&id))
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET status='revoked' WHERE id=?`), id)
		require.NoError(t, err)
	}
	owner := addTestInstruction(t, st, bob.ID, "Vee")
	require.Equal(t, int64(9), owner.ID)
	proposed := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindAddressAs, "Val", "call me Val", personfacts.InstructionStatusProposed)
	require.Equal(t, int64(10), proposed)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET supersedes_id=? WHERE id=?`), owner.ID, proposed)
	require.NoError(t, err)
	addTestInstruction(t, st, alice.ID, "Vee")
	merged, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(t, split.ExactReversal)
	got := testInstruction(t, st, proposed)
	require.NotNil(t, got.SupersedesID)
	assert.Equal(t, owner.ID, *got.SupersedesID)
	assert.Equal(t, split.NewPerson.ID, got.PersonID)
}
func TestMergeRemapsInstructionScopeToSurvivorContact(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	points := []*store.PersonContactPoint{}
	for _, personID := range []int64{alice.ID, bob.ID} {
		point, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceCardDAVImport, SourceRef: new("example-book"), SourceResourceUID: new("example-resource"), VCard: store.VCardIdentity{Property: "EMAIL", PropID: new("email-1")}}})
		require.NoError(t, err)
		points = append(points, point)
	}
	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: bob.ID, Kind: personfacts.InstructionKindAvoidAddress, ContactPointID: &points[1].Envelope.ID, Text: "avoid shared@example.com", Actor: "owner"})
	require.NoError(t, err)
	merged, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	got := testInstruction(t, st, row.ID)
	require.NotNil(t, got.ContactPointID)
	assert.Equal(t, points[0].Envelope.ID, *got.ContactPointID)
	assert.False(t, got.ScopeMissing)
	assert.Equal(t, row.InstructionKey, got.InstructionKey)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(t, split.ExactReversal)
	got = testInstruction(t, st, row.ID)
	require.NotNil(t, got.ContactPointID)
	assert.Equal(t, points[1].Envelope.ID, *got.ContactPointID)
	assert.Equal(t, split.NewPerson.ID, got.PersonID)
	assert.False(t, got.ScopeMissing)
}
func TestExactSplitRestoresInstructionsAndEvents(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor := addTestInstruction(t, st, alice.ID, "Vee")
	owner := addTestInstruction(t, st, bob.ID, "Val")
	proposed := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
	event := insertTestInstructionEvent(t, st, testInstruction(t, st, proposed), "propose")
	merged, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(t, split.ExactReversal)
	got := testInstruction(t, st, survivor.ID)
	assert.Equal(t, survivor.Status, got.Status)
	assert.Equal(t, survivor.ReviewState, got.ReviewState)
	for _, id := range []int64{owner.ID, proposed} {
		got := testInstruction(t, st, id)
		assert.Equal(t, split.NewPerson.ID, got.PersonID)
		assert.Equal(t, personfacts.InstructionReviewSplit, got.ReviewState)
		assert.Greater(t, got.Revision, int64(1))
		assert.Equal(t, 1, instructionEventCount(t, st, id, "split_moved"))
	}
	var eventPerson, eventInstruction int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT person_id,instruction_id FROM person_instruction_events WHERE id=?`), event).Scan(&eventPerson, &eventInstruction))
	assert.Equal(t, split.NewPerson.ID, eventPerson)
	assert.Equal(t, proposed, eventInstruction)
}
func TestPartialSplitLeavesInstructionsOnSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	participant, err := st.EnsureParticipant("bob-alias@example.com", "bob alias", "example.com")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_participants(person_id,participant_id) VALUES (?,?)`), bob.ID, participant)
	require.NoError(t, err)
	row := addTestInstruction(t, st, bob.ID, "Vee")
	merged, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	assert.False(t, split.ExactReversal)
	got := testInstruction(t, st, row.ID)
	assert.Equal(t, alice.ID, got.PersonID)
	found := false
	for _, ref := range split.AmbiguousRows {
		if ref.TableName == "person_instructions" && ref.OriginalRowID != nil && *ref.OriginalRowID == row.ID {
			found = true
		}
	}
	assert.True(t, found)
}

func mergeInstructionPeople(t *testing.T, ctx context.Context, st *store.Store, survivorID, absorbedID int64) (*store.PersonMergeResult, error) {
	t.Helper()
	survivor, err := st.GetPerson(survivorID)
	require.NoError(t, err)
	absorbed, err := st.GetPerson(absorbedID)
	require.NoError(t, err)
	return st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: survivorID, AbsorbedID: absorbedID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "instruction-merge", Actor: "owner"})
}

func TestExactSplitPreservesPostMergeInstructionConfirmation(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusProposed)
	absorbed := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusActive)
	merged, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	current := testInstruction(t, st, survivor)
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: survivor, ExpectedRevision: current.Revision, Action: personfacts.InstructionActionConfirm, Actor: "owner"})
	require.NoError(t, err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(t, split.ExactReversal)
	for _, id := range []int64{survivor, absorbed} {
		restored := testInstruction(t, st, id)
		assert.Equal(t, personfacts.InstructionStatusActive, restored.Status)
		assert.Equal(t, decision.Instruction.ConfirmedAt, restored.ConfirmedAt)
		assert.Equal(t, decision.Instruction.ConfirmedBy, restored.ConfirmedBy)
	}
}

func TestMergeInstructionConflictInvalidatesStaleDecision(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	first := addTestInstruction(t, st, alice.ID, "Vee")
	second := addTestInstruction(t, st, bob.ID, "Val")
	_, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	current := testInstruction(t, st, first.ID)
	assert.Greater(t, current.Revision, first.Revision)
	assert.True(t, current.UpdatedAt.After(first.UpdatedAt))
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: first.ID, ExpectedRevision: first.Revision, Action: personfacts.InstructionActionConfirm, Actor: "owner"})
	assert.ErrorIs(t, err, store.ErrInstructionRevisionConflict)
	assert.Equal(t, personfacts.InstructionStatusActive, testInstruction(t, st, second.ID).Status)
}

func TestExactSplitReparentsPostMergeInstructionEvents(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	id := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusActive)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET status='suspended' WHERE id=?`), id)
	require.NoError(t, err)
	merged, err := mergeInstructionPeople(t, t.Context(), st, alice.ID, bob.ID)
	require.NoError(t, err)
	current := testInstruction(t, st, id)
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: current.Revision, Action: personfacts.InstructionActionReject, Actor: "owner"})
	require.NoError(t, err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(t, split.ExactReversal)
	restored := testInstruction(t, st, id)
	assert.Equal(t, personfacts.InstructionStatusRejected, restored.Status)
	for _, action := range []string{"merge_moved", "reject", "split_moved"} {
		var personID int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT person_id FROM person_instruction_events WHERE instruction_id=? AND action=?`), id, action).Scan(&personID))
		assert.Equal(t, split.NewPerson.ID, personID, action)
	}
}
