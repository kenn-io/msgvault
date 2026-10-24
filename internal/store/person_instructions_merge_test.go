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
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	owner := addTestInstruction(t, st, bob.ID, "Vee")
	confirmed := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusActive)
	proposal := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindFormat, "", "use bullets", personfacts.InstructionStatusProposed)
	_, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	for _, id := range []int64{owner.ID, confirmed} {
		got := testInstruction(t, st, id)
		assert.Equal(alice.ID, got.PersonID)
		assert.Equal(personfacts.InstructionReviewMerged, got.ReviewState)
		assert.Equal(1, instructionEventCount(t, st, id, "merge_moved"))
	}
	_, err = st.GetPersonInstructionContext(t.Context(), proposal)
	require.ErrorIs(err, store.ErrInstructionNotFound)
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
	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
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
	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
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
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	id := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusRejected)
	row := testInstruction(t, st, id)
	event := insertTestInstructionEvent(t, st, row, "reject")
	_, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	var personID int64
	var instructionID sql.NullInt64
	var key string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT person_id,instruction_id,instruction_key FROM person_instruction_events WHERE id=?`), event).Scan(&personID, &instructionID, &key))
	assert.Equal(alice.ID, personID)
	assert.False(instructionID.Valid)
	assert.Equal(row.InstructionKey, key)
}
func TestMergeFlagsSingleValuedConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	first := addTestInstruction(t, st, alice.ID, "Vee")
	second := addTestInstruction(t, st, bob.ID, "Val")
	_, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
	require.NoError(err)
	require.Len(rows, 2)
	for _, row := range rows {
		assert.Equal(personfacts.InstructionReviewConflict, row.ReviewState)
		assert.True(row.VisibleToAgents)
	}
	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, first.ID).Status)
	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, second.ID).Status)
}
func TestExactSplitRestoresSupersessionAcrossDigitBoundary(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	filler := mustPromotedPerson(t, st, "filler@example.com", "filler")
	for range 8 {
		addTestInstruction(t, st, filler.ID, "Vee") /* each iteration needs a distinct historical ID */
		var id int64
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT MAX(id) FROM person_instructions`).Scan(&id))
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET status='revoked' WHERE id=?`), id)
		require.NoError(err)
	}
	owner := addTestInstruction(t, st, bob.ID, "Vee")
	require.Equal(int64(9), owner.ID)
	proposed := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindAddressAs, "Val", "call me Val", personfacts.InstructionStatusProposed)
	require.Equal(int64(10), proposed)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET supersedes_id=? WHERE id=?`), owner.ID, proposed)
	require.NoError(err)
	addTestInstruction(t, st, alice.ID, "Vee")
	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	got := testInstruction(t, st, proposed)
	require.NotNil(got.SupersedesID)
	assert.Equal(owner.ID, *got.SupersedesID)
	assert.Equal(split.NewPerson.ID, got.PersonID)
}
func TestMergeRemapsInstructionScopeToSurvivorContact(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	points := []*store.PersonContactPoint{}
	for _, personID := range []int64{alice.ID, bob.ID} {
		point, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceCardDAVImport, SourceRef: new("example-book"), SourceResourceUID: new("example-resource"), VCard: store.VCardIdentity{Property: "EMAIL", PropID: new("email-1")}}})
		require.NoError(err)
		points = append(points, point)
	}
	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: bob.ID, Kind: personfacts.InstructionKindAvoidAddress, ContactPointID: &points[1].Envelope.ID, Text: "avoid shared@example.com", Actor: "owner"})
	require.NoError(err)
	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	got := testInstruction(t, st, row.ID)
	require.NotNil(got.ContactPointID)
	assert.Equal(points[0].Envelope.ID, *got.ContactPointID)
	assert.False(got.ScopeMissing)
	assert.Equal(row.InstructionKey, got.InstructionKey)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	got = testInstruction(t, st, row.ID)
	require.NotNil(got.ContactPointID)
	assert.Equal(points[1].Envelope.ID, *got.ContactPointID)
	assert.Equal(split.NewPerson.ID, got.PersonID)
	assert.False(got.ScopeMissing)
}

func TestMergeKeepsInstructionOnDifferentServiceScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	addPoint := func(personID int64, network string) *store.PersonContactPoint {
		point, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressUsername, ServiceSlug: new("irc"),
			ScopeKind: new("network"), ScopeValue: new(network), OriginalValue: "shared-user",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceCardDAVImport,
				SourceRef: new("example-book"), SourceResourceUID: new("example-resource"),
				VCard: store.VCardIdentity{Property: "IMPP", PropID: new("impp-1")}},
		})
		require.NoError(err)
		return point
	}
	addPoint(alice.ID, "irc.example.net")
	absorbedPoint := addPoint(bob.ID, "irc.example.org")
	instruction, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: bob.ID, Kind: personfacts.InstructionKindAvoidAddress,
		ContactPointID: &absorbedPoint.Envelope.ID, Text: "avoid shared-user on IRC",
		Actor: "owner",
	})
	require.NoError(err)

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	got := testInstruction(t, st, instruction.ID)
	require.NotNil(got.ContactPointID)
	assert.Equal(absorbedPoint.Envelope.ID, *got.ContactPointID)
}

func TestExactSplitRestoresInstructionsAndEvents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor := addTestInstruction(t, st, alice.ID, "Vee")
	owner := addTestInstruction(t, st, bob.ID, "Val")
	proposed := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
	event := insertTestInstructionEvent(t, st, testInstruction(t, st, proposed), "propose")
	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	got := testInstruction(t, st, survivor.ID)
	assert.Equal(survivor.Status, got.Status)
	assert.Equal(survivor.ReviewState, got.ReviewState)
	for _, id := range []int64{owner.ID, proposed} {
		got := testInstruction(t, st, id)
		assert.Equal(split.NewPerson.ID, got.PersonID)
		assert.Equal(personfacts.InstructionReviewSplit, got.ReviewState)
		assert.Greater(got.Revision, int64(1))
		assert.Equal(1, instructionEventCount(t, st, id, "split_moved"))
	}
	var eventPerson, eventInstruction int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT person_id,instruction_id FROM person_instruction_events WHERE id=?`), event).Scan(&eventPerson, &eventInstruction))
	assert.Equal(split.NewPerson.ID, eventPerson)
	assert.Equal(proposed, eventInstruction)
}
func TestPartialSplitLeavesInstructionsOnSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	participant, err := st.EnsureParticipant("bob-alias@example.com", "bob alias", "example.com")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_participants(person_id,participant_id) VALUES (?,?)`), bob.ID, participant)
	require.NoError(err)
	row := addTestInstruction(t, st, bob.ID, "Vee")
	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	assert.False(split.ExactReversal)
	got := testInstruction(t, st, row.ID)
	assert.Equal(alice.ID, got.PersonID)
	found := false
	for _, ref := range split.AmbiguousRows {
		if ref.TableName == "person_instructions" && ref.OriginalRowID != nil && *ref.OriginalRowID == row.ID {
			found = true
		}
	}
	assert.True(found)
}

func mergeInstructionPeople(ctx context.Context, t *testing.T, st *store.Store, survivorID, absorbedID int64) (*store.PersonMergeResult, error) {
	t.Helper()
	survivor, err := st.GetPerson(survivorID)
	require.NoError(t, err)
	absorbed, err := st.GetPerson(absorbedID)
	require.NoError(t, err)
	key := fmt.Sprintf("instruction-merge-%d-%d", survivorID, absorbedID)
	return st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: survivorID, AbsorbedID: absorbedID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: key, Actor: "owner"})
}

func TestNestedMergeDeduplicationRestoresEarlierInstructionAndEvents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")
	carolInstruction := addTestInstruction(t, st, carol.ID, "Vee")
	eventID := insertTestInstructionEvent(t, st, bobInstruction, "confirm")

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)

	current, err := st.GetPerson(carol.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-first-merge", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal, "unrestored rows: %+v", split.UnrestoredRows)

	restored, err := st.GetPersonInstructionContext(t.Context(), bobInstruction.ID)
	require.NoError(err)
	assert.Equal(split.NewPerson.ID, restored.PersonID)
	var eventPersonID, eventInstructionID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT person_id,instruction_id FROM person_instruction_events WHERE id=?`,
	), eventID).Scan(&eventPersonID, &eventInstructionID))
	assert.Equal(split.NewPerson.ID, eventPersonID)
	assert.Equal(bobInstruction.ID, eventInstructionID)
	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, carolInstruction.ID).Status)
}

func TestExactSplitPreservesPostMergeInstructionConfirmation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusProposed)
	absorbed := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusActive)
	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	current := testInstruction(t, st, survivor)
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: survivor, ExpectedRevision: current.Revision, Action: personfacts.InstructionActionConfirm, Actor: "owner"})
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	for _, id := range []int64{survivor, absorbed} {
		restored := testInstruction(t, st, id)
		assert.Equal(personfacts.InstructionStatusActive, restored.Status)
		assert.Equal(decision.Instruction.ConfirmedAt, restored.ConfirmedAt)
		assert.Equal(decision.Instruction.ConfirmedBy, restored.ConfirmedBy)
	}
}

func TestExactSplitPreservesConfirmedEditOverRestoredProposal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	active := addTestInstruction(t, st, bob.ID, "Vee")
	proposal := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindAddressAs, "Val", "call me Val", personfacts.InstructionStatusProposed)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = st.GetPersonInstructionContext(t.Context(), proposal)
	require.ErrorIs(err, store.ErrInstructionNotFound)

	current := testInstruction(t, st, active.ID)
	text, value := "call me Val", "Val"
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: active.ID, ExpectedRevision: current.Revision,
		Action: personfacts.InstructionActionConfirm,
		Edit:   &store.PersonInstructionEdit{Text: &text, ValueText: &value},
		Actor:  "owner",
	})
	require.NoError(err)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restoredProposal := testInstruction(t, st, proposal)
	confirmed := testInstruction(t, st, active.ID)
	assert.Equal(split.NewPerson.ID, restoredProposal.PersonID)
	assert.Equal(personfacts.InstructionStatusSuperseded, restoredProposal.Status)
	assert.Equal(personfacts.InstructionReviewNone, restoredProposal.ReviewState)
	assert.Equal(decision.Instruction.InstructionKey, restoredProposal.InstructionKey)
	assert.Equal(split.NewPerson.ID, confirmed.PersonID)
	assert.Equal(personfacts.InstructionStatusActive, confirmed.Status)
	assert.Equal("call me val", confirmed.NormalizedText)
	assert.Equal("Val", confirmed.ValueText)
	assert.Equal(decision.Instruction.ConfirmedBy, confirmed.ConfirmedBy)

	var live int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM person_instructions
		WHERE person_id=? AND instruction_key=?
		  AND status IN ('proposed','active','suspended')`),
		split.NewPerson.ID, confirmed.InstructionKey).Scan(&live))
	assert.Equal(1, live)
}

func TestExactSplitPreservesOwnerEditOverRestoredSuspendedInstruction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	owner := addTestInstruction(t, st, bob.ID, "Val")
	suspended := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusSuspended)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = st.GetPersonInstructionContext(t.Context(), suspended)
	require.ErrorIs(err, store.ErrInstructionNotFound)

	current := testInstruction(t, st, owner.ID)
	text, value := "call me Vee", "Vee"
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: owner.ID, ExpectedRevision: current.Revision,
		Action: personfacts.InstructionActionConfirm,
		Edit:   &store.PersonInstructionEdit{Text: &text, ValueText: &value},
		Actor:  "owner",
	})
	require.NoError(err)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restored := testInstruction(t, st, suspended)
	confirmed := testInstruction(t, st, owner.ID)
	assert.Equal(split.NewPerson.ID, restored.PersonID)
	assert.Equal(personfacts.InstructionStatusSuperseded, restored.Status)
	assert.Equal(personfacts.InstructionReviewNone, restored.ReviewState)
	assert.Equal(split.NewPerson.ID, confirmed.PersonID)
	assert.Equal(personfacts.InstructionStatusActive, confirmed.Status)
	assert.Equal(decision.Instruction.ConfirmedBy, confirmed.ConfirmedBy)
	assert.Equal(1, instructionEventCount(t, st, suspended, "supersede"))

	var live int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM person_instructions
		WHERE person_id=? AND instruction_key=?
		  AND status IN ('proposed','active','suspended')`),
		split.NewPerson.ID, confirmed.InstructionKey).Scan(&live))
	assert.Equal(1, live)
}

func TestExactSplitPreservesOwnerEditAcrossDeduplicatedAndSuspendedRestore(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor := addTestInstruction(t, st, alice.ID, "Val")
	deduplicated := addTestInstruction(t, st, bob.ID, "Val")
	suspended := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusSuspended)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = st.GetPersonInstructionContext(t.Context(), suspended)
	require.ErrorIs(err, store.ErrInstructionNotFound)

	current := testInstruction(t, st, survivor.ID)
	text, value := "call me Vee", "Vee"
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: survivor.ID, ExpectedRevision: current.Revision,
		Action: personfacts.InstructionActionConfirm,
		Edit:   &store.PersonInstructionEdit{Text: &text, ValueText: &value},
		Actor:  "owner",
	})
	require.NoError(err)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restored := testInstruction(t, st, deduplicated.ID)
	restoredSuspended := testInstruction(t, st, suspended)
	assert.Equal(split.NewPerson.ID, restored.PersonID)
	assert.Equal(personfacts.InstructionStatusActive, restored.Status)
	assert.Equal(decision.Instruction.InstructionKey, restored.InstructionKey)
	assert.Equal("call me vee", restored.NormalizedText)
	assert.Equal("Vee", restored.ValueText)
	assert.Equal(decision.Instruction.ConfirmedBy, restored.ConfirmedBy)
	assert.Equal(split.NewPerson.ID, restoredSuspended.PersonID)
	assert.Equal(personfacts.InstructionStatusSuperseded, restoredSuspended.Status)
	assert.Equal(personfacts.InstructionReviewNone, restoredSuspended.ReviewState)
	assert.Equal(1, instructionEventCount(t, st, suspended, "supersede"))

	var live int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM person_instructions
		WHERE person_id=? AND instruction_key=?
		  AND status IN ('proposed','active','suspended')`),
		split.NewPerson.ID, restored.InstructionKey).Scan(&live))
	assert.Equal(1, live)
}

func TestExactSplitPreservesConfirmedEditWhenRestoringDeduplicatedInstruction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	active := addTestInstruction(t, st, alice.ID, "Vee")
	deduplicated := addTestInstruction(t, st, bob.ID, "Vee")
	proposal := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindAddressAs, "Val", "call me Val", personfacts.InstructionStatusProposed)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = st.GetPersonInstructionContext(t.Context(), deduplicated.ID)
	require.ErrorIs(err, store.ErrInstructionNotFound)
	_, err = st.GetPersonInstructionContext(t.Context(), proposal)
	require.ErrorIs(err, store.ErrInstructionNotFound)

	current := testInstruction(t, st, active.ID)
	text, value := "call me Val", "Val"
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: active.ID, ExpectedRevision: current.Revision,
		Action: personfacts.InstructionActionConfirm,
		Edit:   &store.PersonInstructionEdit{Text: &text, ValueText: &value},
		Actor:  "owner",
	})
	require.NoError(err)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	confirmed := testInstruction(t, st, deduplicated.ID)
	restoredProposal := testInstruction(t, st, proposal)
	assert.Equal(split.NewPerson.ID, confirmed.PersonID)
	assert.Equal(personfacts.InstructionStatusActive, confirmed.Status)
	assert.Equal(decision.Instruction.InstructionKey, confirmed.InstructionKey)
	assert.Equal(split.NewPerson.ID, restoredProposal.PersonID)
	assert.Equal(personfacts.InstructionStatusSuperseded, restoredProposal.Status)
	assert.Equal(personfacts.InstructionReviewNone, restoredProposal.ReviewState)
	assert.Equal(decision.Instruction.ConfirmedBy, confirmed.ConfirmedBy)

	var live int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM person_instructions
		WHERE person_id=? AND instruction_key=?
		  AND status IN ('proposed','active','suspended')`),
		split.NewPerson.ID, confirmed.InstructionKey).Scan(&live))
	assert.Equal(1, live)
}

func TestExactSplitDoesNotReviveInstructionExpiredDuringMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	expired := addTestInstruction(t, st, alice.ID, "Vee")
	now := time.Now().UTC()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions
		SET valid_from=?,valid_until=?,updated_at=? WHERE id=?`),
		now.Add(-48*time.Hour), now.Add(-24*time.Hour), now.Add(time.Hour), expired.ID)
	require.NoError(err)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	assert.Equal(personfacts.InstructionStatusExpired, testInstruction(t, st, expired.ID).Status)
	renewed, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: alice.ID, Kind: personfacts.InstructionKindAddressAs,
		ValueText: "Vee", Text: "call me Vee", Actor: "owner",
	})
	require.NoError(err)
	assert.NotEqual(expired.ID, renewed.ID)

	current, err := st.GetPerson(alice.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: alice.ID, MergeID: merged.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-expiry", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)

	restoredExpired := testInstruction(t, st, expired.ID)
	assert.Equal(personfacts.InstructionStatusExpired, restoredExpired.Status)
	assert.Equal(personfacts.InstructionReviewNone, restoredExpired.ReviewState)
	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, renewed.ID).Status)
	assert.Equal(1, instructionEventCount(t, st, expired.ID, "expire"))
	var live int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM person_instructions
		WHERE person_id=? AND instruction_key=?
		  AND status IN ('proposed','active','suspended')`),
		alice.ID, renewed.InstructionKey).Scan(&live))
	assert.Equal(1, live)
}

func TestExactSplitKeepsDeduplicatedInstructionRevisionMonotonic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor := addTestInstruction(t, st, alice.ID, "Vee")
	absorbed := addTestInstruction(t, st, bob.ID, "Vee")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET revision=4 WHERE id=?`), absorbed.ID)
	require.NoError(err)
	staleRevision := int64(4)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET revision=10 WHERE id=?`), absorbed.ID)
	require.NoError(err)
	absorbed = testInstruction(t, st, absorbed.ID)
	assert.Equal(int64(10), absorbed.Revision)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	current := testInstruction(t, st, survivor.ID)
	assert.Less(current.Revision, absorbed.Revision)
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: survivor.ID, ExpectedRevision: current.Revision,
		Action: personfacts.InstructionActionConfirm, Actor: "owner",
	})
	require.NoError(err)
	current = testInstruction(t, st, survivor.ID)
	assert.Less(current.Revision, absorbed.Revision)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restored := testInstruction(t, st, absorbed.ID)
	assert.Greater(restored.Revision, absorbed.Revision)
	assert.Greater(restored.Revision, current.Revision)
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: absorbed.ID, ExpectedRevision: staleRevision,
		Action: personfacts.InstructionActionRevoke, Actor: "owner",
	})
	require.ErrorIs(err, store.ErrInstructionRevisionConflict)
}

func TestMergeInstructionConflictInvalidatesStaleDecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	first := addTestInstruction(t, st, alice.ID, "Vee")
	second := addTestInstruction(t, st, bob.ID, "Val")
	_, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	current := testInstruction(t, st, first.ID)
	assert.Greater(current.Revision, first.Revision)
	assert.True(current.UpdatedAt.After(first.UpdatedAt))
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: first.ID, ExpectedRevision: first.Revision, Action: personfacts.InstructionActionConfirm, Actor: "owner"})
	require.ErrorIs(err, store.ErrInstructionRevisionConflict)
	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, second.ID).Status)
}

func TestMergeExpiresElapsedOwnerBeforeResolvingCollision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	valid := addTestInstruction(t, st, alice.ID, "Vee")
	expired := addTestInstruction(t, st, bob.ID, "Vee")
	now := time.Now().UTC()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions
		SET valid_from=?,valid_until=?,updated_at=? WHERE id=?`),
		now.Add(-48*time.Hour), now.Add(-24*time.Hour), now.Add(time.Hour), expired.ID)
	require.NoError(err)

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)

	got := testInstruction(t, st, valid.ID)
	assert.Equal(personfacts.InstructionStatusActive, got.Status)
	assert.True(got.VisibleToAgents)
	expiredAfter, err := st.GetPersonInstructionContext(t.Context(), expired.ID)
	require.NoError(err)
	assert.Equal(personfacts.InstructionStatusExpired, expiredAfter.Status)
	assert.Equal(personfacts.InstructionReviewNone, expiredAfter.ReviewState)
	assert.Equal(1, instructionEventCount(t, st, expired.ID, "expire"))
	assert.Equal(1, instructionEventCount(t, st, expired.ID, "merge_moved"))
}

func TestMergeAndSplitKeepTerminalInstructionOutOfReview(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	row := addTestInstruction(t, st, bob.ID, "Vee")
	_, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: row.ID, ExpectedRevision: row.Revision,
		Action: personfacts.InstructionActionRevoke, Actor: "owner",
	})
	require.NoError(err)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	got := testInstruction(t, st, row.ID)
	assert.Equal(personfacts.InstructionStatusRevoked, got.Status)
	assert.Equal(personfacts.InstructionReviewNone, got.ReviewState)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	got = testInstruction(t, st, row.ID)
	assert.Equal(personfacts.InstructionStatusRevoked, got.Status)
	assert.Equal(personfacts.InstructionReviewNone, got.ReviewState)
	assert.Equal(1, instructionEventCount(t, st, row.ID, "split_moved"))
}

func TestExactSplitReparentsPostMergeInstructionEvents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	id := insertTestPersonInstruction(t, st, bob.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusActive)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET status='suspended' WHERE id=?`), id)
	require.NoError(err)
	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	current := testInstruction(t, st, id)
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: current.Revision, Action: personfacts.InstructionActionReject, Actor: "owner"})
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restored := testInstruction(t, st, id)
	assert.Equal(personfacts.InstructionStatusRejected, restored.Status)
	for _, action := range []string{"merge_moved", "reject", "split_moved"} {
		var personID int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT person_id FROM person_instruction_events WHERE instruction_id=? AND action=?`), id, action).Scan(&personID))
		assert.Equal(split.NewPerson.ID, personID, action)
	}
}
