package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
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
func setInstructionEventCreatedAt(t *testing.T, st *store.Store, instructionID int64, action string, createdAt time.Time) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instruction_events SET created_at=? WHERE instruction_id=? AND action=?`,
	), createdAt, instructionID, action)
	require.NoError(t, err)
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

func TestMergePreservesOwnerMarkedUnsureInstructionAndEvent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	instructionID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
	instruction := testInstruction(t, st, instructionID)
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: instructionID, ExpectedRevision: instruction.Revision,
		Action: personfacts.InstructionActionUnsure, Actor: "owner",
	})
	require.NoError(err)
	assert.Equal(personfacts.InstructionReviewUnsure, decision.Instruction.ReviewState)
	var unsureEventID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id
		FROM person_instruction_events WHERE instruction_id=? AND action='unsure'`),
		instructionID).Scan(&unsureEventID))

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)

	got := testInstruction(t, st, instructionID)
	assert.Equal(alice.ID, got.PersonID)
	assert.Equal(personfacts.InstructionStatusProposed, got.Status)
	assert.Equal(personfacts.InstructionReviewUnsure, got.ReviewState)
	var eventPersonID int64
	var eventInstructionID sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT person_id,instruction_id FROM person_instruction_events WHERE id=?`,
	), unsureEventID).Scan(&eventPersonID, &eventInstructionID))
	assert.Equal(alice.ID, eventPersonID)
	require.True(eventInstructionID.Valid)
	assert.Equal(instructionID, eventInstructionID.Int64)
	assert.Equal(1, instructionEventCount(t, st, instructionID, "merge_moved"))
}

func TestMergeKeepsUnsureEventWhenOwnerInstructionWinsCollision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	winner := addTestInstruction(t, st, alice.ID, "Vee")
	unsureID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusProposed)
	unsure := testInstruction(t, st, unsureID)
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: unsureID, ExpectedRevision: unsure.Revision,
		Action: personfacts.InstructionActionUnsure, Actor: "owner",
	})
	require.NoError(err)
	assert.Equal(personfacts.InstructionReviewUnsure, decision.Instruction.ReviewState)
	var unsureEventID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id
		FROM person_instruction_events WHERE instruction_id=? AND action='unsure'`),
		unsureID).Scan(&unsureEventID))

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)

	got := testInstruction(t, st, winner.ID)
	assert.Equal(personfacts.InstructionOriginOwner, got.Origin)
	assert.Equal(personfacts.InstructionStatusActive, got.Status)
	_, err = st.GetPersonInstructionContext(t.Context(), unsureID)
	require.ErrorIs(err, store.ErrInstructionNotFound)
	var eventPersonID int64
	var eventInstructionID sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT person_id,instruction_id FROM person_instruction_events WHERE id=?`,
	), unsureEventID).Scan(&eventPersonID, &eventInstructionID))
	assert.Equal(alice.ID, eventPersonID)
	require.True(eventInstructionID.Valid)
	assert.Equal(winner.ID, eventInstructionID.Int64)
}

func TestMergeSplitMergePreservesOwnerMarkedUnsureInstructionAndEvent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	instructionID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
	instruction := testInstruction(t, st, instructionID)
	_, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: instructionID, ExpectedRevision: instruction.Revision,
		Action: personfacts.InstructionActionUnsure, Actor: "owner",
	})
	require.NoError(err)
	var eventID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id
		FROM person_instruction_events WHERE instruction_id=? AND action='unsure'`),
		instructionID).Scan(&eventID))

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	assert.Equal(personfacts.InstructionReviewUnsure, testInstruction(t, st, instructionID).ReviewState)

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, split.NewPerson.ID)
	require.NoError(err)
	got, err := st.GetPersonInstructionContext(t.Context(), instructionID)
	require.NoError(err)
	assert.Equal(alice.ID, got.PersonID)
	assert.Equal(personfacts.InstructionReviewUnsure, got.ReviewState)
	var eventPersonID int64
	var eventInstructionID sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT person_id,instruction_id FROM person_instruction_events WHERE id=?`,
	), eventID).Scan(&eventPersonID, &eventInstructionID))
	assert.Equal(alice.ID, eventPersonID)
	require.True(eventInstructionID.Valid)
	assert.Equal(instructionID, eventInstructionID.Int64)
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

func TestMergeCollisionCopiesTextFromAbsorbedWinner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: alice.ID, Kind: personfacts.InstructionKindAddressAs,
		ValueText: "iphone", Text: "Call me iphone", Actor: "owner",
	})
	require.NoError(err)
	absorbed, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: bob.ID, Kind: personfacts.InstructionKindAddressAs,
		ValueText: "iPhone", Text: "Call me iPhone", Actor: "owner",
	})
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET updated_at=? WHERE id=?`,
	), time.Now().UTC().Add(time.Hour), absorbed.ID)
	require.NoError(err)
	absorbed = testInstruction(t, st, absorbed.ID)

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)

	got := testInstruction(t, st, survivor.ID)
	assert.Equal(absorbed.Text, got.Text)
	assert.Equal(absorbed.ValueText, got.ValueText)
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
	var detailFields struct {
		AbsorbedInstructionID int64  `json:"absorbed_instruction_id"`
		Collision             bool   `json:"collision"`
		LifecycleSource       string `json:"lifecycle_source"`
	}
	require.NoError(t, json.Unmarshal([]byte(detail), &detailFields))
	assert.Equal(t, absorbed.ID, detailFields.AbsorbedInstructionID)
	assert.True(t, detailFields.Collision)
	assert.Equal(t, sourceName, detailFields.LifecycleSource)
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

func TestExactSplitPreservesOwnerEditThatInvalidatesSupersession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	owner := addTestInstruction(t, st, bob.ID, "Vee")
	proposalID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindAddressAs, "Val", "call me Val", personfacts.InstructionStatusProposed)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET review_state=? WHERE id=?`,
	), personfacts.InstructionReviewUnsure, proposalID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET supersedes_id=? WHERE id=?`,
	), owner.ID, proposalID)
	require.NoError(err)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	proposal := testInstruction(t, st, proposalID)
	kind := personfacts.InstructionKindTopic
	text := "avoid work"
	value := ""
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: proposalID, ExpectedRevision: proposal.Revision,
		Action: personfacts.InstructionActionConfirm,
		Edit:   &store.PersonInstructionEdit{Kind: &kind, Text: &text, ValueText: &value},
		Actor:  "owner",
	})
	require.NoError(err)
	assert.Nil(decision.Instruction.SupersedesID)

	current, err := st.GetPersonContext(t.Context(), merged.Person.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-after-incompatible-edit", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)

	got := testInstruction(t, st, proposalID)
	assert.Equal(split.NewPerson.ID, got.PersonID)
	assert.Equal(kind, got.Kind)
	assert.Nil(got.SupersedesID)
}

func TestOutOfOrderZeroParticipantSplitsPreserveInstructionLifecycleAndOrder(t *testing.T) {
	for _, splitEarlierFirst := range []bool{true, false} {
		name := "later merge first"
		if splitEarlierFirst {
			name = "earlier merge first"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ctx := t.Context()
			st := testutil.NewTestStore(t)
			alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
			diana := mustPromotedPerson(t, st, "diana@example.com", "diana")
			bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
			carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
			for _, person := range []*store.Person{bob, carol} {
				_, err := st.DB().ExecContext(ctx, st.Rebind(
					`DELETE FROM person_participants WHERE person_id=?`,
				), person.ID)
				require.NoError(err)
			}
			bob, err := st.GetPersonContext(ctx, bob.ID)
			require.NoError(err)
			carol, err = st.GetPersonContext(ctx, carol.ID)
			require.NoError(err)
			assert.Empty(bob.ParticipantIDs)
			assert.Empty(carol.ParticipantIDs)

			aliceInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
			dianaInstruction := addTestInstruction(t, st, diana.ID, "IPHONE")
			bobInstruction := addTestInstruction(t, st, bob.ID, "iphone")
			carolInstruction := addTestInstruction(t, st, carol.ID, "IPHONe")
			for _, instruction := range []*store.PersonInstruction{
				aliceInstruction, dianaInstruction, bobInstruction, carolInstruction,
			} {
				assert.Equal(aliceInstruction.InstructionKey, instruction.InstructionKey)
			}

			base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)
			ordered := []struct {
				instruction *store.PersonInstruction
				decisionAt  time.Time
				validUntil  time.Time
			}{
				{aliceInstruction, base, base.Add(24 * time.Hour)},
				{dianaInstruction, base.Add(time.Hour), base.Add(48 * time.Hour)},
				{bobInstruction, base.Add(2 * time.Hour), base.Add(72 * time.Hour)},
				{carolInstruction, base.Add(3 * time.Hour), base.Add(96 * time.Hour)},
			}
			for _, item := range ordered {
				setInstructionEventCreatedAt(t, st, item.instruction.ID, "propose", item.decisionAt)
				_, err := st.DB().ExecContext(ctx, st.Rebind(
					`UPDATE person_instructions SET valid_from=?,valid_until=? WHERE id=?`,
				), base.Add(-time.Hour), item.validUntil, item.instruction.ID)
				require.NoError(err)
			}

			earlier, err := mergeInstructionPeople(ctx, t, st, alice.ID, bob.ID)
			require.NoError(err)
			later, err := mergeInstructionPeople(ctx, t, st, alice.ID, carol.ID)
			require.NoError(err)

			split := func(merge *store.PersonMergeResult, key string) *store.PersonSplitResult {
				t.Helper()
				current, err := st.GetPersonContext(ctx, alice.ID)
				require.NoError(err)
				result, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
					SourcePersonID: alice.ID, MergeID: merge.Merge.ID,
					ParticipantIDs: []int64{}, ExpectedSourceRevision: current.Revision,
					IdempotencyKey: key, Actor: "owner",
				})
				require.NoError(err)
				require.True(result.ExactReversal)
				return result
			}
			var earlierSplit, laterSplit *store.PersonSplitResult
			if splitEarlierFirst {
				earlierSplit = split(earlier, "split-earlier-zero-participant-first")
				afterEarlierSplit := testInstruction(t, st, aliceInstruction.ID)
				assert.Equal(carolInstruction.Text, afterEarlierSplit.Text)
				require.NotNil(afterEarlierSplit.ValidUntil)
				assert.Equal(ordered[3].validUntil, afterEarlierSplit.ValidUntil.UTC())
				laterSplit = split(later, "split-later-zero-participant-second")
			} else {
				laterSplit = split(later, "split-later-zero-participant-first")
				afterLaterSplit := testInstruction(t, st, aliceInstruction.ID)
				assert.Equal(bobInstruction.Text, afterLaterSplit.Text)
				require.NotNil(afterLaterSplit.ValidUntil)
				assert.Equal(ordered[2].validUntil, afterLaterSplit.ValidUntil.UTC())
				earlierSplit = split(earlier, "split-earlier-zero-participant-second")
			}

			survivorInstruction := testInstruction(t, st, aliceInstruction.ID)
			assert.Equal(alice.ID, survivorInstruction.PersonID)
			assert.Equal(aliceInstruction.Text, survivorInstruction.Text)
			require.NotNil(survivorInstruction.ValidUntil)
			assert.Equal(ordered[0].validUntil, survivorInstruction.ValidUntil.UTC())
			assert.Equal(earlierSplit.NewPerson.ID, testInstruction(t, st, bobInstruction.ID).PersonID)
			assert.Equal(laterSplit.NewPerson.ID, testInstruction(t, st, carolInstruction.ID).PersonID)

			_, err = mergeInstructionPeople(ctx, t, st, alice.ID, diana.ID)
			require.NoError(err)
			winner := testInstruction(t, st, aliceInstruction.ID)
			assert.Equal(dianaInstruction.Text, winner.Text)
			require.NotNil(winner.ValidUntil)
			assert.Equal(ordered[1].validUntil, winner.ValidUntil.UTC())
		})
	}
}

func TestOutOfOrderParticipantSplitPreservesSelectedInstructionDecisionOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	diana := mustPromotedPerson(t, st, "diana@example.com", "diana")

	aliceInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
	bobInstruction := addTestInstruction(t, st, bob.ID, "iphone")
	carolInstruction := addTestInstruction(t, st, carol.ID, "IPHONe")
	dianaInstruction := addTestInstruction(t, st, diana.ID, "IPHONE")
	for _, instruction := range []*store.PersonInstruction{
		aliceInstruction, bobInstruction, carolInstruction, dianaInstruction,
	} {
		assert.Equal(aliceInstruction.InstructionKey, instruction.InstructionKey)
	}

	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)
	ordered := []struct {
		instruction *store.PersonInstruction
		decisionAt  time.Time
		validUntil  time.Time
	}{
		{aliceInstruction, base, base.Add(24 * time.Hour)},
		{bobInstruction, base.Add(time.Hour), base.Add(48 * time.Hour)},
		{dianaInstruction, base.Add(2 * time.Hour), base.Add(72 * time.Hour)},
		{carolInstruction, base.Add(3 * time.Hour), base.Add(96 * time.Hour)},
	}
	for _, item := range ordered {
		setInstructionEventCreatedAt(t, st, item.instruction.ID, "propose", item.decisionAt)
		_, err := st.DB().ExecContext(ctx, st.Rebind(
			`UPDATE person_instructions SET valid_from=?,valid_until=? WHERE id=?`,
		), base.Add(-time.Hour), item.validUntil, item.instruction.ID)
		require.NoError(err)
	}

	earlier, err := mergeInstructionPeople(ctx, t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = mergeInstructionPeople(ctx, t, st, alice.ID, carol.ID)
	require.NoError(err)
	survivor, err := st.GetPersonContext(ctx, alice.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: alice.ID, MergeID: earlier.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: survivor.Revision,
		IdempotencyKey: "split-earlier-participant-merge-first", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)
	assert.Equal(carolInstruction.Text, testInstruction(t, st, aliceInstruction.ID).Text)
	assert.Equal(split.NewPerson.ID, testInstruction(t, st, bobInstruction.ID).PersonID)

	_, err = mergeInstructionPeople(ctx, t, st, alice.ID, diana.ID)
	require.NoError(err)
	winner := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(carolInstruction.Text, winner.Text)
	require.NotNil(winner.ValidUntil)
	assert.Equal(ordered[3].validUntil, winner.ValidUntil.UTC())
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

func TestMergeInstructionCollisionUsesActiveScopedContactAndSplitRestoresBindings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	addPoint := func(personID int64) *store.PersonContactPoint {
		point, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.com",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceCardDAVImport,
				SourceRef: new("example-book"), SourceResourceUID: new("example-resource"),
				VCard: store.VCardIdentity{Property: "EMAIL", PropID: new("email-1")}},
		})
		require.NoError(err)
		return point
	}
	alicePoint := addPoint(alice.ID)
	bobPoint := addPoint(bob.ID)
	aliceInstruction, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: alice.ID, Kind: personfacts.InstructionKindAvoidAddress,
		ContactPointID: &alicePoint.Envelope.ID, Text: "avoid shared@example.com", Actor: "owner",
	})
	require.NoError(err)
	bobInstruction, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: bob.ID, Kind: personfacts.InstructionKindAvoidAddress,
		ContactPointID: &bobPoint.Envelope.ID, Text: "avoid shared@example.com", Actor: "owner",
	})
	require.NoError(err)
	assert.Equal(aliceInstruction.InstructionKey, bobInstruction.InstructionKey)
	later := time.Now().UTC().Add(time.Hour)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET updated_at=? WHERE id=?`,
	), later, bobInstruction.ID)
	require.NoError(err)
	require.NoError(st.SupersedePersonContactPointContext(t.Context(), alice.ID,
		alicePoint.Envelope.ID, nil))

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	got := testInstruction(t, st, aliceInstruction.ID)
	require.NotNil(got.ContactPointID)
	assert.Equal(bobPoint.Envelope.ID, *got.ContactPointID)
	assert.False(got.ScopeMissing)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restoredAlice := testInstruction(t, st, aliceInstruction.ID)
	require.NotNil(restoredAlice.ContactPointID)
	assert.Equal(alicePoint.Envelope.ID, *restoredAlice.ContactPointID)
	assert.True(restoredAlice.ScopeMissing)
	restoredBob := testInstruction(t, st, bobInstruction.ID)
	require.NotNil(restoredBob.ContactPointID)
	assert.Equal(bobPoint.Envelope.ID, *restoredBob.ContactPointID)
	assert.False(restoredBob.ScopeMissing)
	assert.Equal(split.NewPerson.ID, restoredBob.PersonID)
}

func TestDirectEarlierSplitRestoresContactAfterLaterMergeDeduplication(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	addPoint := func(personID int64) *store.PersonContactPoint {
		point, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.com",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceCardDAVImport,
				SourceRef: new("example-book"), SourceResourceUID: new("example-resource"),
				VCard: store.VCardIdentity{Property: "EMAIL", PropID: new("email-1")}},
		})
		require.NoError(err)
		return point
	}
	alicePoint := addPoint(alice.ID)
	bobPoint := addPoint(bob.ID)
	carolPoint := addPoint(carol.ID)
	bobInstruction, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: bob.ID, Kind: personfacts.InstructionKindAvoidAddress,
		ContactPointID: &bobPoint.Envelope.ID, Text: "avoid shared@example.com", Actor: "owner",
	})
	require.NoError(err)

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	mergedInstruction, err := st.GetPersonInstructionContext(t.Context(), bobInstruction.ID)
	require.NoError(err)
	require.NotNil(mergedInstruction.ContactPointID)
	assert.Equal(alicePoint.Envelope.ID, *mergedInstruction.ContactPointID)
	assert.False(mergedInstruction.ScopeMissing)
	_, err = mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)
	mergedInstruction, err = st.GetPersonInstructionContext(t.Context(), bobInstruction.ID)
	require.NoError(err)
	require.NotNil(mergedInstruction.ContactPointID)
	assert.Equal(carolPoint.Envelope.ID, *mergedInstruction.ContactPointID)
	assert.False(mergedInstruction.ScopeMissing)

	current, err := st.GetPerson(carol.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-earlier-contact-baseline", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)

	restoredBob, err := st.GetPersonInstructionContext(t.Context(), bobInstruction.ID)
	require.NoError(err)
	assert.Equal(split.NewPerson.ID, restoredBob.PersonID)
	require.NotNil(restoredBob.ContactPointID)
	assert.Equal(bobPoint.Envelope.ID, *restoredBob.ContactPointID)
	assert.False(restoredBob.ScopeMissing)
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

func TestNestedDeduplicationSplitThenRevokeRestoresOriginalLifecycleAndEvents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "Vee")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")
	carolInstruction := addTestInstruction(t, st, carol.ID, "Vee")
	eventID := insertTestInstructionEvent(t, st, bobInstruction, "confirm")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET updated_at=? WHERE id=?`,
	), time.Now().UTC().Add(time.Hour), bobInstruction.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET updated_at=? WHERE id=?`,
	), time.Now().UTC().Add(2*time.Hour), carolInstruction.ID)
	require.NoError(err)

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	firstMergeParticipants := append([]int64(nil), first.Person.ParticipantIDs...)
	second, err := mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)

	current, err := st.GetPerson(carol.ID)
	require.NoError(err)
	secondSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: second.Merge.ID,
		ParticipantIDs: firstMergeParticipants, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-second-collision", Actor: "owner",
	})
	require.NoError(err)
	require.True(secondSplit.ExactReversal)

	carolCurrent := testInstruction(t, st, carolInstruction.ID)
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: carolInstruction.ID, ExpectedRevision: carolCurrent.Revision,
		Action: personfacts.InstructionActionRevoke, Actor: "owner",
	})
	require.NoError(err)

	current, err = st.GetPerson(secondSplit.NewPerson.ID)
	require.NoError(err)
	firstSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: secondSplit.NewPerson.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-first-collision", Actor: "owner",
	})
	require.NoError(err)
	require.True(firstSplit.ExactReversal)

	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, aliceInstruction.ID).Status)
	assert.Equal(personfacts.InstructionStatusRevoked, testInstruction(t, st, carolInstruction.ID).Status)
	bobRestored := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(firstSplit.NewPerson.ID, bobRestored.PersonID)
	assert.Equal(personfacts.InstructionStatusActive, bobRestored.Status)
	var eventPersonID, eventInstructionID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT person_id,instruction_id FROM person_instruction_events WHERE id=?`,
	), eventID).Scan(&eventPersonID, &eventInstructionID))
	assert.Equal(firstSplit.NewPerson.ID, eventPersonID)
	assert.Equal(bobInstruction.ID, eventInstructionID)
}

func TestNestedDeduplicationSplitDoesNotTransferUnrelatedRevocation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "Vee")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")
	carolInstruction := addTestInstruction(t, st, carol.ID, "Vee")

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	second, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, carol.ID)
	require.NoError(err)

	current, err := st.GetPerson(alice.ID)
	require.NoError(err)
	firstSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: alice.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-bob-first", Actor: "owner",
	})
	require.NoError(err)

	bobCurrent := testInstruction(t, st, bobInstruction.ID)
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: bobInstruction.ID, ExpectedRevision: bobCurrent.Revision,
		Action: personfacts.InstructionActionRevoke, Actor: "owner",
	})
	require.NoError(err)

	current, err = st.GetPerson(alice.ID)
	require.NoError(err)
	secondSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: alice.ID, MergeID: second.Merge.ID,
		ParticipantIDs: carol.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-carol-second", Actor: "owner",
	})
	require.NoError(err)

	assert.Equal(firstSplit.NewPerson.ID, testInstruction(t, st, bobInstruction.ID).PersonID)
	assert.Equal(secondSplit.NewPerson.ID, testInstruction(t, st, carolInstruction.ID).PersonID)
	assert.Equal(personfacts.InstructionStatusRevoked, testInstruction(t, st, bobInstruction.ID).Status)
	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, carolInstruction.ID).Status)
	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, aliceInstruction.ID).Status)
}

func TestNestedExactSplitsKeepDeduplicatedInstructionOnOriginalPerson(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "Vee")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	firstMergeParticipants := append([]int64(nil), first.Person.ParticipantIDs...)
	second, err := mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)

	current, err := st.GetPerson(carol.ID)
	require.NoError(err)
	secondSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: second.Merge.ID,
		ParticipantIDs: firstMergeParticipants, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-second-merge", Actor: "owner",
	})
	require.NoError(err)
	require.True(secondSplit.ExactReversal)

	current, err = st.GetPerson(secondSplit.NewPerson.ID)
	require.NoError(err)
	firstSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: secondSplit.NewPerson.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-first-merge", Actor: "owner",
	})
	require.NoError(err)
	require.True(firstSplit.ExactReversal)

	assert.Equal(secondSplit.NewPerson.ID, testInstruction(t, st, aliceInstruction.ID).PersonID)
	assert.Equal(firstSplit.NewPerson.ID, testInstruction(t, st, bobInstruction.ID).PersonID)
}

func TestNestedExactSplitsPreserveDetachedRejectionEvents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	instructionID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
	instruction := testInstruction(t, st, instructionID)
	_, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: instructionID, ExpectedRevision: instruction.Revision,
		Action: personfacts.InstructionActionReject, Actor: "owner",
	})
	require.NoError(err)
	var eventID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT MAX(id)
		FROM person_instruction_events WHERE instruction_key=? AND action='reject'`),
		instruction.InstructionKey).Scan(&eventID))
	var initialInstructionID sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT instruction_id FROM person_instruction_events WHERE id=?`,
	), eventID).Scan(&initialInstructionID))
	require.True(initialInstructionID.Valid)

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	firstParticipants := append([]int64(nil), first.Person.ParticipantIDs...)
	var detachedInstructionID sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT instruction_id FROM person_instruction_events WHERE id=?`,
	), eventID).Scan(&detachedInstructionID))
	require.False(detachedInstructionID.Valid)
	second, err := mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)

	current, err := st.GetPerson(carol.ID)
	require.NoError(err)
	secondSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: second.Merge.ID,
		ParticipantIDs: firstParticipants, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-rejection-second", Actor: "owner",
	})
	require.NoError(err)
	require.True(secondSplit.ExactReversal)
	var nestedInstructionID sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT instruction_id FROM person_instruction_events WHERE id=?`,
	), eventID).Scan(&nestedInstructionID))
	assert.False(nestedInstructionID.Valid)

	current, err = st.GetPerson(secondSplit.NewPerson.ID)
	require.NoError(err)
	firstSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: secondSplit.NewPerson.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-rejection-first", Actor: "owner",
	})
	require.NoError(err)
	require.True(firstSplit.ExactReversal)

	var personID int64
	var restoredInstructionID sql.NullInt64
	var key string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT person_id,instruction_id,instruction_key FROM person_instruction_events WHERE id=?`,
	), eventID).Scan(&personID, &restoredInstructionID, &key))
	assert.Equal(firstSplit.NewPerson.ID, personID)
	assert.True(restoredInstructionID.Valid)
	assert.Equal(instructionID, restoredInstructionID.Int64)
	assert.Equal(instruction.InstructionKey, key)
}

func TestNestedExactSplitsRestoreOriginalInstructionValidity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "Vee")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")
	validUntil := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	equalUpdatedAt := time.Now().UTC().Truncate(time.Second)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_instructions SET valid_until=?,updated_at=? WHERE id=?`),
		validUntil, equalUpdatedAt, aliceInstruction.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET updated_at=? WHERE id=?`,
	), equalUpdatedAt, bobInstruction.ID)
	require.NoError(err)

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	firstMergeParticipants := append([]int64(nil), first.Person.ParticipantIDs...)
	second, err := mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)

	current, err := st.GetPerson(carol.ID)
	require.NoError(err)
	secondSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: second.Merge.ID,
		ParticipantIDs: firstMergeParticipants, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-second-merge", Actor: "owner",
	})
	require.NoError(err)
	require.True(secondSplit.ExactReversal)

	current, err = st.GetPerson(secondSplit.NewPerson.ID)
	require.NoError(err)
	firstSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: secondSplit.NewPerson.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-first-merge", Actor: "owner",
	})
	require.NoError(err)
	require.True(firstSplit.ExactReversal)

	restoredAlice := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(secondSplit.NewPerson.ID, restoredAlice.PersonID)
	require.NotNil(restoredAlice.ValidUntil)
	assert.Equal(validUntil, restoredAlice.ValidUntil.UTC())
	assert.Nil(testInstruction(t, st, bobInstruction.ID).ValidUntil)
}

func TestDirectEarlierNestedSplitPreservesInstructionLifecycleBaseline(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")
	carolInstruction := addTestInstruction(t, st, carol.ID, "Vee")
	validFrom := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	bobValidUntil := validFrom.Add(48 * time.Hour)
	carolValidUntil := validFrom.Add(72 * time.Hour)
	for _, instruction := range []struct {
		id         int64
		validUntil time.Time
		updatedAt  time.Time
	}{
		{bobInstruction.ID, bobValidUntil, validFrom.Add(time.Hour)},
		{carolInstruction.ID, carolValidUntil, validFrom.Add(3 * time.Hour)},
	} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions
			SET valid_from=?,valid_until=?,updated_at=? WHERE id=?`),
			validFrom, instruction.validUntil, instruction.updatedAt, instruction.id)
		require.NoError(err)
	}

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	mergedInstruction := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(alice.ID, mergedInstruction.PersonID)
	require.NotNil(mergedInstruction.ValidUntil)
	assert.Equal(bobValidUntil, mergedInstruction.ValidUntil.UTC())
	_, err = mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)

	current, err := st.GetPerson(carol.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-earlier-before-later", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)

	restoredBob := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(split.NewPerson.ID, restoredBob.PersonID)
	require.NotNil(restoredBob.ValidUntil)
	assert.Equal(bobValidUntil, restoredBob.ValidUntil.UTC())
	require.NotNil(testInstruction(t, st, carolInstruction.ID).ValidUntil)
	assert.Equal(carolValidUntil, testInstruction(t, st, carolInstruction.ID).ValidUntil.UTC())
}

func TestDirectEarlierNestedSplitReusesCanonicalSurvivorInstruction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	addTestInstruction(t, st, alice.ID, "Vee")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")
	carolInstruction := addTestInstruction(t, st, carol.ID, "Vee")

	earlier, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)

	current, err := st.GetPersonContext(t.Context(), carol.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: earlier.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-earlier-after-three-way-dedup", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)

	canonical := testInstruction(t, st, carolInstruction.ID)
	assert.Equal(carol.ID, canonical.PersonID)
	restoredBob := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(split.NewPerson.ID, restoredBob.PersonID)
}

func TestDirectEarlierNestedSplitIgnoresLaterSystemConflictMarker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")
	carolWinner := addTestInstruction(t, st, carol.ID, "Vee")
	insertTestPersonInstruction(t, st, carol.ID,
		personfacts.InstructionKindAddressAs, "Val", "call me Val", personfacts.InstructionStatusActive)
	validFrom := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	bobValidUntil := validFrom.Add(48 * time.Hour)
	carolValidUntil := validFrom.Add(72 * time.Hour)
	for _, instruction := range []struct {
		id         int64
		validUntil time.Time
		updatedAt  time.Time
	}{
		{bobInstruction.ID, bobValidUntil, validFrom.Add(time.Hour)},
		{carolWinner.ID, carolValidUntil, validFrom.Add(3 * time.Hour)},
	} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions
			SET valid_from=?,valid_until=?,updated_at=? WHERE id=?`),
			validFrom, instruction.validUntil, instruction.updatedAt, instruction.id)
		require.NoError(err)
	}

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)
	current := testInstruction(t, st, carolWinner.ID)
	require.NotNil(current.ValidUntil)
	assert.Equal(carolValidUntil, current.ValidUntil.UTC())
	assert.Equal(personfacts.InstructionReviewConflict, current.ReviewState)

	currentPerson, err := st.GetPerson(carol.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: carol.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: currentPerson.Revision,
		IdempotencyKey: "instruction-split-earlier-conflict-marker", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)

	restoredBob := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(split.NewPerson.ID, restoredBob.PersonID)
	require.NotNil(restoredBob.ValidUntil)
	assert.Equal(bobValidUntil, restoredBob.ValidUntil.UTC())
	assert.Equal(carolValidUntil, testInstruction(t, st, carolWinner.ID).ValidUntil.UTC())
}

func TestDirectEarlierNestedSplitReflagsRemainingInstructionConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "Vee")
	addTestInstruction(t, st, bob.ID, "Val")
	carolInstruction := addTestInstruction(t, st, carol.ID, "Vow")

	earlierMerge, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, carol.ID)
	require.NoError(err)
	assert.Equal(personfacts.InstructionReviewConflict, testInstruction(t, st, aliceInstruction.ID).ReviewState)
	assert.Equal(personfacts.InstructionReviewConflict, testInstruction(t, st, carolInstruction.ID).ReviewState)

	currentPerson, err := st.GetPerson(alice.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: alice.ID, MergeID: earlierMerge.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: currentPerson.Revision,
		IdempotencyKey: "instruction-split-reflags-conflict", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)

	remainingAlice := testInstruction(t, st, aliceInstruction.ID)
	remainingCarol := testInstruction(t, st, carolInstruction.ID)
	assert.Equal(personfacts.InstructionReviewConflict, remainingAlice.ReviewState)
	assert.Equal(personfacts.InstructionReviewConflict, remainingCarol.ReviewState)
	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
	require.NoError(err)
	assert.Len(rows, 2)

	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: remainingAlice.ID, ExpectedRevision: remainingAlice.Revision,
		Action: personfacts.InstructionActionConfirm, Actor: "owner",
	})
	require.NoError(err)
	assert.Equal(personfacts.InstructionStatusActive, testInstruction(t, st, aliceInstruction.ID).Status)
	assert.Equal(personfacts.InstructionStatusSuperseded, testInstruction(t, st, carolInstruction.ID).Status)
}

func TestDirectEarlierNestedSplitPreservesLaterWinnerInstructionState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
	bobInstruction := addTestInstruction(t, st, bob.ID, "iphone")
	carolInstruction := addTestInstruction(t, st, carol.ID, "IPHONE")
	assert.Equal(aliceInstruction.InstructionKey, bobInstruction.InstructionKey)
	assert.Equal(aliceInstruction.InstructionKey, carolInstruction.InstructionKey)
	validFrom := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	aliceValidUntil := validFrom.Add(24 * time.Hour)
	bobValidUntil := validFrom.Add(48 * time.Hour)
	carolValidUntil := validFrom.Add(72 * time.Hour)
	for _, instruction := range []struct {
		id         int64
		validUntil time.Time
		updatedAt  time.Time
	}{
		{aliceInstruction.ID, aliceValidUntil, validFrom.Add(time.Hour)},
		{bobInstruction.ID, bobValidUntil, validFrom},
		{carolInstruction.ID, carolValidUntil, validFrom.Add(2 * time.Hour)},
	} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions
			SET valid_from=?,valid_until=?,updated_at=? WHERE id=?`),
			validFrom, instruction.validUntil, instruction.updatedAt, instruction.id)
		require.NoError(err)
	}

	first, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, carol.ID)
	require.NoError(err)
	currentWinner := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal("call me IPHONE", currentWinner.Text)
	require.NotNil(currentWinner.ValidUntil)
	assert.Equal(carolValidUntil, currentWinner.ValidUntil.UTC())

	current, err := st.GetPerson(alice.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: alice.ID, MergeID: first.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-earlier-text-baseline", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)

	remainingWinner := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(alice.ID, remainingWinner.PersonID)
	assert.Equal("call me IPHONE", remainingWinner.Text)
	require.NotNil(remainingWinner.ValidUntil)
	assert.Equal(carolValidUntil, remainingWinner.ValidUntil.UTC())
	restoredBob := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(split.NewPerson.ID, restoredBob.PersonID)
	assert.Equal("call me iphone", restoredBob.Text)
	assert.Equal("iphone", restoredBob.ValueText)
	require.NotNil(restoredBob.ValidUntil)
	assert.Equal(bobValidUntil, restoredBob.ValidUntil.UTC())
}

func TestNestedMergeCollisionUsesLatestOwnerDecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
	bobInstruction := addTestInstruction(t, st, bob.ID, "iphone")
	assert.Equal(aliceInstruction.InstructionKey, bobInstruction.InstructionKey)

	validFrom := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	aliceValidUntil := validFrom.Add(24 * time.Hour)
	bobValidUntil := validFrom.Add(48 * time.Hour)
	for _, instruction := range []struct {
		id         int64
		validUntil time.Time
	}{
		{aliceInstruction.ID, aliceValidUntil},
		{bobInstruction.ID, bobValidUntil},
	} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions
			SET valid_from=?,valid_until=? WHERE id=?`), validFrom, instruction.validUntil, instruction.id)
		require.NoError(err)
		insertTestInstructionEvent(t, st, testInstruction(t, st, instruction.id), "confirm")
	}
	decisionAt := time.Now().UTC().Truncate(time.Microsecond)
	setInstructionEventCreatedAt(t, st, bobInstruction.ID, "confirm", decisionAt.Add(-time.Hour))
	setInstructionEventCreatedAt(t, st, aliceInstruction.ID, "confirm", decisionAt)

	_, err := mergeInstructionPeople(t.Context(), t, st, carol.ID, bob.ID)
	require.NoError(err)
	assert.True(testInstruction(t, st, bobInstruction.ID).UpdatedAt.After(
		testInstruction(t, st, aliceInstruction.ID).UpdatedAt,
	), "the earlier decision's instruction should have the newer merge-maintained timestamp")

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, carol.ID)
	require.NoError(err)

	winner := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal("call me iPhone", winner.Text)
	require.NotNil(winner.ValidUntil)
	assert.Equal(aliceValidUntil, winner.ValidUntil.UTC())
}

func TestNestedMergeKeepsUnsureDecisionAfterProposalCollision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	bobID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindAddressAs, "iphone", "call me iphone", personfacts.InstructionStatusProposed)
	bobInstruction := testInstruction(t, st, bobID)
	_, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: bobInstruction.ID, ExpectedRevision: bobInstruction.Revision,
		Action: personfacts.InstructionActionUnsure, Actor: "owner",
	})
	require.NoError(err)
	aliceID := insertTestPersonInstruction(t, st, alice.ID,
		personfacts.InstructionKindAddressAs, "iPhone", "call me iPhone", personfacts.InstructionStatusProposed)
	aliceInstruction := testInstruction(t, st, aliceID)
	assert.Equal(aliceInstruction.InstructionKey, testInstruction(t, st, bobID).InstructionKey)

	newerTimestamp := time.Now().UTC().Truncate(time.Microsecond)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET updated_at=? WHERE id=?`,
	), newerTimestamp.Add(-time.Hour), bobID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET updated_at=? WHERE id=?`,
	), newerTimestamp, aliceID)
	require.NoError(err)

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	merged := testInstruction(t, st, aliceID)
	assert.Equal(personfacts.InstructionReviewUnsure, merged.ReviewState)
	assert.Equal(1, instructionEventCount(t, st, aliceID, "unsure"))

	_, err = mergeInstructionPeople(t.Context(), t, st, carol.ID, alice.ID)
	require.NoError(err)

	retained := testInstruction(t, st, aliceID)
	assert.Equal(carol.ID, retained.PersonID)
	assert.Equal(personfacts.InstructionReviewUnsure, retained.ReviewState)
	assert.Equal(1, instructionEventCount(t, st, aliceID, "unsure"))
	var eventPersonID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT person_id
		FROM person_instruction_events WHERE instruction_id=? AND action='unsure'`),
		aliceID).Scan(&eventPersonID))
	assert.Equal(carol.ID, eventPersonID)
}

func TestExactSplitListsRestoredDeduplicatedInstruction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	addTestInstruction(t, st, alice.ID, "Vee")
	absorbed := addTestInstruction(t, st, bob.ID, "Vee")

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)

	rows, err := st.ListPersonInstructionsContext(t.Context(), split.NewPerson.ID, store.PersonInstructionFilter{})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(absorbed.ID, rows[0].ID)
	assert.True(rows[0].VisibleToAgents)
}

func TestExactSplitRestoresSameDayInstructionTimestamps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "Vee")
	bobInstruction := addTestInstruction(t, st, bob.ID, "Vee")

	tomorrow := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	aliceValidFrom, aliceValidUntil := tomorrow.Add(9*time.Hour), tomorrow.Add(17*time.Hour)
	bobValidFrom, bobValidUntil := tomorrow.Add(10*time.Hour), tomorrow.Add(18*time.Hour)
	for _, instruction := range []struct {
		id                    int64
		validFrom, validUntil time.Time
	}{
		{aliceInstruction.ID, aliceValidFrom, aliceValidUntil},
		{bobInstruction.ID, bobValidFrom, bobValidUntil},
	} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(
			`UPDATE person_instructions SET valid_from=?,valid_until=? WHERE id=?`,
		), instruction.validFrom, instruction.validUntil, instruction.id)
		require.NoError(err)
	}

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)

	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID,
		store.PersonInstructionFilter{AsOf: tomorrow.Add(12 * time.Hour)})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(aliceInstruction.ID, rows[0].ID)
	assert.True(rows[0].VisibleToAgents)
	assert.Equal(aliceValidFrom, rows[0].ValidFrom.UTC())
	require.NotNil(rows[0].ValidUntil)
	assert.Equal(aliceValidUntil, rows[0].ValidUntil.UTC())
}

func TestExactSplitExpiresRestoredInstructionBeforeRenewal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor := addTestInstruction(t, st, alice.ID, "Vee")
	absorbed := addTestInstruction(t, st, bob.ID, "Vee")

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	now := time.Now().UTC()
	validFrom := now.Add(-2 * time.Hour)
	validUntil := now.Add(-time.Hour)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET valid_from=?,valid_until=? WHERE id=?`,
	), validFrom, validUntil, survivor.ID)
	require.NoError(err)
	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)

	renewed, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: split.NewPerson.ID, Kind: personfacts.InstructionKindAddressAs,
		ValueText: "Vee", Text: "call me Vee", Actor: "owner",
	})
	require.NoError(err)
	assert.NotEqual(absorbed.ID, renewed.ID)
	assert.Equal(personfacts.InstructionStatusExpired, testInstruction(t, st, absorbed.ID).Status)
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

func TestExactSplitPreservesConfirmedWordingForDeduplicatedCaseVariant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivorID := insertTestPersonInstruction(t, st, alice.ID,
		personfacts.InstructionKindAddressAs, "iPhone", "Use iPhone", personfacts.InstructionStatusProposed)
	absorbedID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindAddressAs, "iphone", "Use iphone", personfacts.InstructionStatusProposed)
	assert.Equal(testInstruction(t, st, survivorID).InstructionKey,
		testInstruction(t, st, absorbedID).InstructionKey)
	survivor := testInstruction(t, st, survivorID)
	_, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: survivorID, ExpectedRevision: survivor.Revision,
		Action: personfacts.InstructionActionConfirm, Actor: "owner",
	})
	require.NoError(err)
	absorbed := testInstruction(t, st, absorbedID)
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: absorbedID, ExpectedRevision: absorbed.Revision,
		Action: personfacts.InstructionActionUnsure, Actor: "owner",
	})
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE person_instructions SET updated_at=? WHERE id=?`,
	), time.Now().UTC().Add(time.Hour), survivorID)
	require.NoError(err)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	current := testInstruction(t, st, survivorID)
	assert.Equal("Use iPhone", current.Text)
	assert.Equal("iPhone", current.ValueText)
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: survivorID, ExpectedRevision: current.Revision,
		Action: personfacts.InstructionActionConfirm, Actor: "owner",
	})
	require.NoError(err)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restored := testInstruction(t, st, absorbedID)
	assert.Equal(split.NewPerson.ID, restored.PersonID)
	assert.Equal(decision.Instruction.Text, restored.Text)
	assert.Equal(decision.Instruction.ValueText, restored.ValueText)
	assert.Equal(decision.Instruction.ConfirmedAt, restored.ConfirmedAt)
	assert.Equal(decision.Instruction.ConfirmedBy, restored.ConfirmedBy)
}

func TestNestedMergesPreferLatestOwnerProposalAfterSplit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	now := time.Now().UTC()
	addOwnerInstruction := func(personID int64, value, text string, validUntil time.Time) *store.PersonInstruction {
		row, err := st.AddPersonInstructionContext(ctx, store.PersonInstructionInput{
			PersonID: personID, Kind: personfacts.InstructionKindAddressAs,
			ValueText: value, Text: text, ValidUntil: &validUntil, Actor: "owner",
		})
		require.NoError(err)
		return row
	}
	bobInstruction := addOwnerInstruction(bob.ID, "iphone", "Use iphone", now.Add(48*time.Hour))
	aliceInstruction := addOwnerInstruction(alice.ID, "iPhone", "Use iPhone", now.Add(72*time.Hour))
	carolInstruction := addOwnerInstruction(carol.ID, "IPHONE", "Use IPHONE", now.Add(96*time.Hour))
	require.Equal(aliceInstruction.InstructionKey, bobInstruction.InstructionKey)
	require.Equal(aliceInstruction.InstructionKey, carolInstruction.InstructionKey)

	olderProposal := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	newerProposal := time.Date(2020, time.January, 2, 0, 0, 0, 0, time.UTC)
	bobCreated := time.Date(2019, time.January, 1, 0, 0, 0, 0, time.UTC)
	aliceCreated := time.Date(2019, time.January, 2, 0, 0, 0, 0, time.UTC)
	carolCreated := time.Date(2019, time.January, 3, 0, 0, 0, 0, time.UTC)
	maintenancePast := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	maintenanceFuture := time.Date(2050, time.January, 1, 0, 0, 0, 0, time.UTC)
	setInstructionTimes := func(id int64, createdAt, updatedAt time.Time) {
		_, err := st.DB().ExecContext(ctx, st.Rebind(
			`UPDATE person_instructions SET created_at=?,updated_at=? WHERE id=?`,
		), createdAt, updatedAt, id)
		require.NoError(err)
	}
	setInstructionTimes(bobInstruction.ID, bobCreated, maintenanceFuture)
	setInstructionTimes(aliceInstruction.ID, aliceCreated, maintenancePast)
	setInstructionTimes(carolInstruction.ID, carolCreated, maintenancePast)
	setInstructionEventCreatedAt(t, st, bobInstruction.ID, "propose", olderProposal)
	setInstructionEventCreatedAt(t, st, aliceInstruction.ID, "propose", newerProposal)
	setInstructionEventCreatedAt(t, st, carolInstruction.ID, "propose", newerProposal)

	first, err := mergeInstructionPeople(ctx, t, st, alice.ID, bob.ID)
	require.NoError(err)
	assert.Equal(alice.ID, first.Person.ID)
	current := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(aliceInstruction.Text, current.Text)
	assert.Equal(aliceInstruction.ValueText, current.ValueText)
	assert.Equal(aliceInstruction.ValidUntil, current.ValidUntil)

	// The first merge updates the surviving row's maintenance timestamp. Keep it
	// newer than Carol's so the second collision must use stable source history.
	setInstructionTimes(aliceInstruction.ID, aliceCreated, maintenanceFuture)
	second, err := mergeInstructionPeople(ctx, t, st, alice.ID, carol.ID)
	require.NoError(err)
	assert.Equal(alice.ID, second.Person.ID)
	current = testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(carolInstruction.Text, current.Text)
	assert.Equal(carolInstruction.ValueText, current.ValueText)
	assert.Equal(carolInstruction.ValidUntil, current.ValidUntil)

	split := splitInstructionMerge(t, st, second, carol.ParticipantIDs)
	require.True(split.ExactReversal)
	restored := testInstruction(t, st, carolInstruction.ID)
	assert.Equal(split.NewPerson.ID, restored.PersonID)
	assert.Equal(carolInstruction.Text, restored.Text)
	assert.True(newerProposal.Equal(instructionProposalTime(t, st, carolInstruction.ID)))

	currentPerson, err := st.GetPerson(alice.ID)
	require.NoError(err)
	restoredPerson, err := st.GetPerson(split.NewPerson.ID)
	require.NoError(err)
	_, err = st.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: alice.ID, AbsorbedID: restoredPerson.ID,
		ExpectedSurvivorRevision: currentPerson.Revision,
		ExpectedAbsorbedRevision: restoredPerson.Revision,
		IdempotencyKey:           "instruction-remerge-after-split", Actor: "owner",
	})
	require.NoError(err)
	current = testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(carolInstruction.Text, current.Text)
	assert.Equal(carolInstruction.ValidUntil, current.ValidUntil)
}

func TestMergeSplitMergeRestoresDecisionOrderForBothInstructions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")

	aliceInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
	carolInstruction := addTestInstruction(t, st, carol.ID, "IPHONE")
	bobInstruction := addTestInstruction(t, st, bob.ID, "iphone")
	assert.Equal(aliceInstruction.InstructionKey, carolInstruction.InstructionKey)
	assert.Equal(aliceInstruction.InstructionKey, bobInstruction.InstructionKey)

	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)
	setInstructionEventCreatedAt(t, st, aliceInstruction.ID, "propose", base)
	setInstructionEventCreatedAt(t, st, carolInstruction.ID, "propose", base.Add(time.Hour))
	setInstructionEventCreatedAt(t, st, bobInstruction.ID, "propose", base.Add(2*time.Hour))
	aliceValidUntil := base.Add(24 * time.Hour)
	carolValidUntil := base.Add(48 * time.Hour)
	bobValidUntil := base.Add(72 * time.Hour)
	for _, instruction := range []struct {
		id         int64
		validUntil time.Time
	}{
		{aliceInstruction.ID, aliceValidUntil},
		{carolInstruction.ID, carolValidUntil},
		{bobInstruction.ID, bobValidUntil},
	} {
		_, err := st.DB().ExecContext(ctx, st.Rebind(
			`UPDATE person_instructions SET valid_from=?,valid_until=? WHERE id=?`,
		), base.Add(-time.Hour), instruction.validUntil, instruction.id)
		require.NoError(err)
	}

	firstMerge, err := mergeInstructionPeople(ctx, t, st, alice.ID, bob.ID)
	require.NoError(err)
	mergedInstruction := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal("call me iphone", mergedInstruction.Text)
	require.NotNil(mergedInstruction.ValidUntil)
	assert.Equal(bobValidUntil, mergedInstruction.ValidUntil.UTC())

	split := splitInstructionMerge(t, st, firstMerge, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restoredAlice := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal("call me iPhone", restoredAlice.Text)
	require.NotNil(restoredAlice.ValidUntil)
	assert.Equal(aliceValidUntil, restoredAlice.ValidUntil.UTC())
	restoredBob := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(split.NewPerson.ID, restoredBob.PersonID)
	assert.Equal("call me iphone", restoredBob.Text)
	require.NotNil(restoredBob.ValidUntil)
	assert.Equal(bobValidUntil, restoredBob.ValidUntil.UTC())

	_, err = mergeInstructionPeople(ctx, t, st, alice.ID, carol.ID)
	require.NoError(err)
	winner := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal("call me IPHONE", winner.Text)
	assert.Equal("IPHONE", winner.ValueText)
	require.NotNil(winner.ValidUntil)
	assert.Equal(carolValidUntil, winner.ValidUntil.UTC())

	_, err = mergeInstructionPeople(ctx, t, st, alice.ID, split.NewPerson.ID)
	require.NoError(err)
	winner = testInstruction(t, st, aliceInstruction.ID)
	assert.Equal("call me iphone", winner.Text)
	assert.Equal("iphone", winner.ValueText)
	require.NotNil(winner.ValidUntil)
	assert.Equal(bobValidUntil, winner.ValidUntil.UTC())
}

func TestSplitEarlierNestedMergePreservesLaterInstructionDecisionOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")

	aliceInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
	bobInstruction := addTestInstruction(t, st, bob.ID, "iphone")
	carolInstruction := addTestInstruction(t, st, carol.ID, "IPHONE")
	assert.Equal(aliceInstruction.InstructionKey, bobInstruction.InstructionKey)
	assert.Equal(aliceInstruction.InstructionKey, carolInstruction.InstructionKey)

	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)
	setInstructionEventCreatedAt(t, st, aliceInstruction.ID, "propose", base)
	setInstructionEventCreatedAt(t, st, bobInstruction.ID, "propose", base.Add(time.Hour))
	setInstructionEventCreatedAt(t, st, carolInstruction.ID, "propose", base.Add(2*time.Hour))

	firstMerge, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	assert.Equal(bobInstruction.Text, testInstruction(t, st, aliceInstruction.ID).Text)

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, carol.ID)
	require.NoError(err)
	assert.Equal(carolInstruction.Text, testInstruction(t, st, aliceInstruction.ID).Text)

	current, err := st.GetPersonContext(t.Context(), alice.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: alice.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-earlier-nested-merge", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)
	assert.Equal(carolInstruction.Text, testInstruction(t, st, aliceInstruction.ID).Text)
	restoredBob := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(split.NewPerson.ID, restoredBob.PersonID)
	assert.Equal(bobInstruction.Text, restoredBob.Text)

	_, err = mergeInstructionPeople(t.Context(), t, st, alice.ID, split.NewPerson.ID)
	require.NoError(err)
	winner := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(carolInstruction.Text, winner.Text)
	assert.Equal(carolInstruction.ValueText, winner.ValueText)
}

func TestNestedMergeUsesSelectedLifecycleOrderAfterExtractionCollision(t *testing.T) {
	ownerDecisionAt := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	assertNestedMergeUsesSelectedLifecycleOrderAfterExtractionCollision(
		t, ownerDecisionAt, ownerDecisionAt.Add(time.Hour), ownerDecisionAt.Add(2*time.Hour),
	)
}

func assertNestedMergeUsesSelectedLifecycleOrderAfterExtractionCollision(
	t *testing.T, ownerDecisionAt, newerOwnerDecisionAt, extractionAt time.Time,
) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")

	ownerInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
	extractionID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindAddressAs, "iphone", "call me iphone", personfacts.InstructionStatusActive)
	extraction := testInstruction(t, st, extractionID)
	newerOwnerInstruction := addTestInstruction(t, st, carol.ID, "IPHONE")
	assert.Equal(ownerInstruction.InstructionKey, extraction.InstructionKey)
	assert.Equal(ownerInstruction.InstructionKey, newerOwnerInstruction.InstructionKey)

	require.True(ownerDecisionAt.Before(newerOwnerDecisionAt))
	require.True(newerOwnerDecisionAt.Before(extractionAt))
	setInstructionEventCreatedAt(t, st, ownerInstruction.ID, "propose", ownerDecisionAt)
	setInstructionEventCreatedAt(t, st, newerOwnerInstruction.ID, "propose", newerOwnerDecisionAt)
	_, err := st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_instruction_events
		(person_id,instruction_id,instruction_key,action,actor,detail_json,created_at)
		VALUES (?,?,?,'propose','extractor','{}',?)`),
		extraction.PersonID, extraction.ID, extraction.InstructionKey, extractionAt)
	require.NoError(err)

	firstMerge, err := mergeInstructionPeople(ctx, t, st, alice.ID, bob.ID)
	require.NoError(err)
	current := testInstruction(t, st, ownerInstruction.ID)
	assert.Equal(ownerInstruction.Text, current.Text)

	_, err = mergeInstructionPeople(ctx, t, st, firstMerge.Person.ID, carol.ID)
	require.NoError(err)
	winner := testInstruction(t, st, ownerInstruction.ID)
	assert.Equal(newerOwnerInstruction.Text, winner.Text)
	assert.Equal(newerOwnerInstruction.ValueText, winner.ValueText)
}

func instructionProposalTime(t *testing.T, st *store.Store, instructionID int64) time.Time {
	t.Helper()
	var createdAt time.Time
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT created_at
		FROM person_instruction_events WHERE instruction_id=? AND action='propose'
		ORDER BY created_at DESC,id DESC LIMIT 1`), instructionID).Scan(&createdAt))
	return createdAt
}

func TestExactSplitPreservesConfirmedSurvivorWordingAfterAbsorbedCollision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivorID := insertTestPersonInstruction(t, st, alice.ID,
		personfacts.InstructionKindAddressAs, "iphone", "Use iphone", personfacts.InstructionStatusProposed)
	absorbedID := insertTestPersonInstruction(t, st, bob.ID,
		personfacts.InstructionKindAddressAs, "iPhone", "Use iPhone", personfacts.InstructionStatusActive)

	merged, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	current := testInstruction(t, st, survivorID)
	require.Equal("Use iPhone", current.Text)
	require.Equal("iPhone", current.ValueText)
	_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: survivorID, ExpectedRevision: current.Revision,
		Action: personfacts.InstructionActionConfirm, Actor: "owner",
	})
	require.NoError(err)

	split := splitInstructionMerge(t, st, merged, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	confirmedSurvivor := testInstruction(t, st, survivorID)
	assert.Equal(alice.ID, confirmedSurvivor.PersonID)
	assert.Equal("Use iPhone", confirmedSurvivor.Text)
	assert.Equal("iPhone", confirmedSurvivor.ValueText)
	assert.Equal(personfacts.InstructionStatusActive, confirmedSurvivor.Status)
	require.NotNil(confirmedSurvivor.ConfirmedAt)
	assert.Equal("owner", *confirmedSurvivor.ConfirmedBy)
	restoredAbsorbed := testInstruction(t, st, absorbedID)
	assert.Equal(split.NewPerson.ID, restoredAbsorbed.PersonID)
	assert.Equal("Use iPhone", restoredAbsorbed.Text)
	assert.Equal("iPhone", restoredAbsorbed.ValueText)
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

func TestMergeConfirmSplitMergePrefersRestoredConfirmation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")
	aliceInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
	bobInstruction := addTestInstruction(t, st, bob.ID, "iphone")
	carolInstruction := addTestInstruction(t, st, carol.ID, "IPHONE")
	assert.Equal(aliceInstruction.InstructionKey, bobInstruction.InstructionKey)
	assert.Equal(aliceInstruction.InstructionKey, carolInstruction.InstructionKey)

	firstProposal := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	secondProposal := time.Date(2020, time.January, 2, 0, 0, 0, 0, time.UTC)
	thirdProposal := time.Date(2020, time.January, 3, 0, 0, 0, 0, time.UTC)
	setInstructionEventCreatedAt(t, st, aliceInstruction.ID, "propose", firstProposal)
	setInstructionEventCreatedAt(t, st, bobInstruction.ID, "propose", secondProposal)
	setInstructionEventCreatedAt(t, st, carolInstruction.ID, "propose", thirdProposal)

	firstMerge, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	mergedInstruction := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal("call me iphone", mergedInstruction.Text)
	decision, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: mergedInstruction.ID, ExpectedRevision: mergedInstruction.Revision,
		Action: personfacts.InstructionActionConfirm, Actor: "owner",
	})
	require.NoError(err)

	split := splitInstructionMerge(t, st, firstMerge, bob.ParticipantIDs)
	require.True(split.ExactReversal)
	restored := testInstruction(t, st, bobInstruction.ID)
	assert.Equal(split.NewPerson.ID, restored.PersonID)
	assert.Equal(decision.Instruction.ConfirmedAt, restored.ConfirmedAt)

	_, err = mergeInstructionPeople(t.Context(), t, st, split.NewPerson.ID, carol.ID)
	require.NoError(err)
	winner := testInstruction(t, st, bobInstruction.ID)
	assert.Equal("call me iphone", winner.Text)
	assert.Equal("iphone", winner.ValueText)
	assert.Equal(decision.Instruction.ConfirmedAt, winner.ConfirmedAt)
}

func TestMergeCollisionUsesLatestEventIDWhenDecisionTimesTie(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	survivor := addTestInstruction(t, st, alice.ID, "iPhone")
	absorbed := addTestInstruction(t, st, bob.ID, "iphone")
	survivorConfirmID := insertTestInstructionEvent(t, st, survivor, "confirm")
	absorbedConfirmID := insertTestInstructionEvent(t, st, absorbed, "confirm")
	require.Greater(absorbedConfirmID, survivorConfirmID)

	decisionAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	proposalAt := decisionAt.Add(-time.Hour)
	survivorCreatedAt := decisionAt.Add(-2 * time.Hour)
	absorbedCreatedAt := decisionAt.Add(-3 * time.Hour)
	setInstructionEventCreatedAt(t, st, survivor.ID, "propose", proposalAt)
	setInstructionEventCreatedAt(t, st, absorbed.ID, "propose", proposalAt)
	setInstructionEventCreatedAt(t, st, survivor.ID, "confirm", decisionAt)
	setInstructionEventCreatedAt(t, st, absorbed.ID, "confirm", decisionAt)
	for _, row := range []struct {
		id        int64
		createdAt time.Time
	}{{survivor.ID, survivorCreatedAt}, {absorbed.ID, absorbedCreatedAt}} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(
			"UPDATE person_instructions SET created_at=?,confirmed_at=?,confirmed_by='owner' WHERE id=?",
		), row.createdAt, decisionAt, row.id)
		require.NoError(err)
	}

	_, err := mergeInstructionPeople(t.Context(), t, st, alice.ID, bob.ID)
	require.NoError(err)
	winner := testInstruction(t, st, survivor.ID)
	assert.Equal("call me iphone", winner.Text)
	assert.Equal("iphone", winner.ValueText)
}

func TestNestedSplitReversalsIgnoreReversedDecisionMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	diana := mustPromotedPerson(t, st, "diana@example.com", "diana")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	carol := mustPromotedPerson(t, st, "carol@example.com", "carol")

	aliceInstruction := addTestInstruction(t, st, alice.ID, "iPhone")
	dianaInstruction := addTestInstruction(t, st, diana.ID, "IPHONE")
	bobInstruction := addTestInstruction(t, st, bob.ID, "iphone")
	carolInstruction := addTestInstruction(t, st, carol.ID, "IPHONe")
	for _, instruction := range []*store.PersonInstruction{aliceInstruction, dianaInstruction, bobInstruction, carolInstruction} {
		assert.Equal(aliceInstruction.InstructionKey, instruction.InstructionKey)
	}

	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)
	aliceValidUntil := base.Add(24 * time.Hour)
	dianaValidUntil := base.Add(48 * time.Hour)
	bobValidUntil := base.Add(72 * time.Hour)
	carolValidUntil := base.Add(96 * time.Hour)
	ordered := []struct {
		instruction *store.PersonInstruction
		decisionAt  time.Time
		validUntil  time.Time
	}{
		{aliceInstruction, base, aliceValidUntil},
		{dianaInstruction, base.Add(time.Hour), dianaValidUntil},
		{bobInstruction, base.Add(2 * time.Hour), bobValidUntil},
		{carolInstruction, base.Add(3 * time.Hour), carolValidUntil},
	}
	for _, item := range ordered {
		setInstructionEventCreatedAt(t, st, item.instruction.ID, "propose", item.decisionAt)
		_, err := st.DB().ExecContext(ctx, st.Rebind(
			`UPDATE person_instructions SET valid_from=?,valid_until=? WHERE id=?`,
		), base.Add(-time.Hour), item.validUntil, item.instruction.ID)
		require.NoError(err)
	}

	firstMerge, err := mergeInstructionPeople(ctx, t, st, alice.ID, bob.ID)
	require.NoError(err)
	secondMerge, err := mergeInstructionPeople(ctx, t, st, alice.ID, carol.ID)
	require.NoError(err)
	merged := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(carolInstruction.Text, merged.Text)
	require.NotNil(merged.ValidUntil)
	assert.Equal(carolValidUntil, merged.ValidUntil.UTC())

	split := splitInstructionMerge(t, st, secondMerge, carol.ParticipantIDs)
	require.True(split.ExactReversal)
	current, err := st.GetPersonContext(ctx, alice.ID)
	require.NoError(err)
	firstSplit, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: alice.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs: bob.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "instruction-split-b-after-c", Actor: "owner",
	})
	require.NoError(err)
	require.True(firstSplit.ExactReversal)

	restoredAlice := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(aliceInstruction.Text, restoredAlice.Text)
	require.NotNil(restoredAlice.ValidUntil)
	assert.Equal(aliceValidUntil, restoredAlice.ValidUntil.UTC())
	assert.Equal(firstSplit.NewPerson.ID, testInstruction(t, st, bobInstruction.ID).PersonID)
	assert.Equal(split.NewPerson.ID, testInstruction(t, st, carolInstruction.ID).PersonID)

	_, err = mergeInstructionPeople(ctx, t, st, alice.ID, diana.ID)
	require.NoError(err)
	winner := testInstruction(t, st, aliceInstruction.ID)
	assert.Equal(dianaInstruction.Text, winner.Text)
	assert.Equal(dianaInstruction.ValueText, winner.ValueText)
	require.NotNil(winner.ValidUntil)
	assert.Equal(dianaValidUntil, winner.ValidUntil.UTC())
}
