package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func addTestInstruction(t *testing.T, st *store.Store, personID int64, value string) *store.PersonInstruction {
	t.Helper()
	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: personID, Kind: personfacts.InstructionKindAddressAs, ValueText: value, Text: "call me " + value, Actor: "owner"})
	require.NoError(t, err)
	return row
}
func insertTestPersonInstruction(t *testing.T, st *store.Store, personID int64, kind personfacts.InstructionKind, value, text string, status personfacts.InstructionStatus) int64 {
	t.Helper()
	now := time.Now().UTC().Add(-time.Hour)
	var confirmedAt any
	var confirmedBy any
	if status == personfacts.InstructionStatusActive {
		confirmedAt = now
		confirmedBy = "owner"
	}
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO person_instructions
 (person_id,instruction_key,kind,normalized_text,value_text,origin,status,stated_at,valid_from,confirmed_at,confirmed_by,created_at,updated_at)
 VALUES (?,?,?,?,?,'extraction',?,?,?,?,?,?,?) RETURNING id`), personID, personfacts.InstructionKey(kind, "", value, text), kind, personfacts.NormalizeInstructionText(text), value, status, now, now, confirmedAt, confirmedBy, now, now).Scan(&id)
	require.NoError(t, err)
	return id
}
func instructionEventCount(t *testing.T, st *store.Store, id int64, action string) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_instruction_events WHERE instruction_id=? AND action=?`), id, action).Scan(&count))
	return count
}
func TestPersonInstructionSchemaInitializesTwice(t *testing.T) {
	st := testutil.NewTestStore(t)
	require.NoError(t, st.InitSchemaContext(t.Context()))
	require.NoError(t, st.InitSchemaContext(t.Context()))
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_instructions`).Scan(&n))
	assert.Zero(t, n)
}
func TestPersonInstructionIDsNeverReused(t *testing.T) {
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite AUTOINCREMENT")
	}
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	first := addTestInstruction(t, st, alice.ID, "Vee")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM person_instructions WHERE id=?`), first.ID)
	require.NoError(t, err)
	second := addTestInstruction(t, st, alice.ID, "Val")
	assert.Greater(t, second.ID, first.ID)
}
func TestAddInstructionOwnerRowIsVisible(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	row := addTestInstruction(t, st, alice.ID, "Vee")
	assert.Equal(t, "owner_entered", row.ContentTrust)
	assert.True(t, row.VisibleToAgents)
	assert.Equal(t, int64(1), row.Revision)
	assert.Equal(t, 1, instructionEventCount(t, st, row.ID, "propose"))
	// Removing participants leaves the independent person profile intact.
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM person_participants WHERE person_id=?`), alice.ID)
	require.NoError(t, err)
	row = addTestInstruction(t, st, alice.ID, "Val")
	assert.True(t, row.VisibleToAgents)
	_, err = st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindAddressAs, ValueText: "Val", Text: "call me Val", Actor: "owner"})
	assert.ErrorIs(t, err, store.ErrInstructionDuplicate)
}
func TestAddInstructionRejectsForeignContactPoint(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	point, err := st.AddPersonContactPointContext(t.Context(), bob.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "bob@example.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	_, err = st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindAvoidAddress, ContactPointID: &point.Envelope.ID, Text: "avoid bob@example.com", Actor: "owner"})
	require.ErrorIs(t, err, store.ErrInstructionInvalid)
	assert.ErrorContains(t, err, "unsupported-target")
}
func TestAddInstructionRejectsExpansion(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	_, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindOther, Text: "please cc bob@example.com", Actor: "owner"})
	assert.ErrorIs(t, err, store.ErrInstructionInvalid)
}
func TestAddAddressAsSupersedesPrevious(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	first := addTestInstruction(t, st, alice.ID, "Vee")
	second := addTestInstruction(t, st, alice.ID, "Val")
	require.NotNil(t, second.SupersedesID)
	assert.Equal(t, first.ID, *second.SupersedesID)
	old, err := st.GetPersonInstructionContext(t.Context(), first.ID)
	require.NoError(t, err)
	assert.Equal(t, personfacts.InstructionStatusSuperseded, old.Status)
	assert.Equal(t, 1, instructionEventCount(t, st, first.ID, "supersede"))
	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, second.ID, rows[0].ID)
}
func TestAddInstructionAfterRevokeCreatesNewRow(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	first := addTestInstruction(t, st, alice.ID, "Vee")
	_, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: first.ID, ExpectedRevision: first.Revision, Action: personfacts.InstructionActionRevoke, Actor: "owner"})
	require.NoError(t, err)
	second := addTestInstruction(t, st, alice.ID, "Vee")
	assert.NotEqual(t, first.ID, second.ID)
	rejected := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Val", "call me Val", personfacts.InstructionStatusRejected)
	third := addTestInstruction(t, st, alice.ID, "Val")
	assert.NotEqual(t, rejected, third.ID)
}
func TestDecideInstructionStaleRevisionConflicts(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	id := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusProposed)
	d := store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionConfirm, Actor: "owner"}
	_, err := st.DecidePersonInstructionContext(t.Context(), d)
	require.NoError(t, err)
	_, err = st.DecidePersonInstructionContext(t.Context(), d)
	assert.ErrorIs(t, err, store.ErrInstructionRevisionConflict)
	assert.Equal(t, 1, instructionEventCount(t, st, id, "confirm"))
}
func TestConfirmProposedWithEditRekeys(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	id := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusProposed)
	text, value := "call me Val", "Val"
	result, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionConfirm, Edit: &store.PersonInstructionEdit{Text: &text, ValueText: &value}, Actor: "owner"})
	require.NoError(t, err)
	assert.Equal(t, personfacts.InstructionKey(personfacts.InstructionKindAddressAs, "", value, text), result.Instruction.InstructionKey)
	assert.Equal(t, []store.PersonInstructionFollowOn{{Kind: "save_nickname", Value: "Val"}}, result.FollowOns)
	assert.Equal(t, "person_statement_owner_confirmed", result.Instruction.ContentTrust)
	for action, key := range map[string]string{"edit": personfacts.InstructionKey(personfacts.InstructionKindAddressAs, "", "Vee", "call me Vee"), "confirm": result.Instruction.InstructionKey} {
		var got string
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT instruction_key FROM person_instruction_events WHERE instruction_id=? AND action=?`), id, action).Scan(&got))
		assert.Equal(t, key, got)
	}
}
func TestSuspendedLeavesOnlyByConfirmOrAdopt(t *testing.T) {
	for _, action := range []personfacts.InstructionAction{personfacts.InstructionActionConfirm, personfacts.InstructionActionAdopt, personfacts.InstructionActionRevoke, personfacts.InstructionActionUnsure} {
		t.Run(string(action), func(t *testing.T) {
			st := testutil.NewTestStore(t)
			alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
			id := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusSuspended)
			result, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: action, Actor: "owner"})
			if action == personfacts.InstructionActionConfirm || action == personfacts.InstructionActionAdopt {
				require.NoError(t, err)
				assert.Equal(t, personfacts.InstructionStatusActive, result.Instruction.Status)
			} else {
				assert.ErrorIs(t, err, store.ErrInstructionTransition)
			}
		})
	}
}
func TestListInstructionsHidesExpiredWithoutWorker(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	until := time.Now().UTC().Add(24 * time.Hour)
	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindTiming, Text: "no calls before 9am", ValidUntil: &until, Actor: "owner"})
	require.NoError(t, err)
	asOf := time.Now().In(time.FixedZone("test-offset", -7*60*60))
	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{AsOf: asOf})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	rows, err = st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{AsOf: until.Add(time.Hour)})
	require.NoError(t, err)
	assert.Empty(t, rows)
	got, err := st.GetPersonInstructionContext(t.Context(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, personfacts.InstructionStatusActive, got.Status)
	rows, err = st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{AsOf: until.Add(time.Hour), IncludeExpired: true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].VisibleToAgents)
}
func TestInstructionReviewQueueOrder(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	first := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
	second := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid travel", personfacts.InstructionStatusSuspended)
	rows, err := st.ListPersonInstructionReviewContext(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, first, rows[0].ID)
	rows, err = st.ListPersonInstructionReviewContext(t.Context(), 2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, second, rows[1].ID)
	_, err = st.ListPersonInstructionReviewContext(t.Context(), 201)
	assert.ErrorIs(t, err, store.ErrInstructionInvalid)
}

func TestInstructionOwnerChoiceSupersedesCurrentSingleValue(t *testing.T) {
	for _, mode := range []string{"confirm-intervening", "adopt-suspended", "confirm-edit"} {
		t.Run(mode, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
			old := addTestInstruction(t, st, alice.ID, "Vee")
			kind, status := personfacts.InstructionKindAddressAs, personfacts.InstructionStatusProposed
			text, value := "call me Val", "Val"
			if mode == "adopt-suspended" {
				status = personfacts.InstructionStatusSuspended
			}
			if mode == "confirm-edit" {
				kind, text, value = personfacts.InstructionKindTopic, "avoid work", ""
			}
			id := insertTestPersonInstruction(t, st, alice.ID, kind, value, text, status)
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET supersedes_id=? WHERE id=?`), old.ID, id)
			require.NoError(t, err)
			newer := addTestInstruction(t, st, alice.ID, "Alice")
			decision := store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionConfirm, Actor: "owner"}
			if mode == "adopt-suspended" {
				decision.Action = personfacts.InstructionActionAdopt
			}
			if mode == "confirm-edit" {
				finalKind, finalText, finalValue := personfacts.InstructionKindAddressAs, "call me Val", "Val"
				decision.Edit = &store.PersonInstructionEdit{Kind: &finalKind, Text: &finalText, ValueText: &finalValue}
			}
			result, err := st.DecidePersonInstructionContext(t.Context(), decision)
			require.NoError(t, err)
			rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, result.Instruction.ID, rows[0].ID)
			superseded, err := st.GetPersonInstructionContext(t.Context(), newer.ID)
			require.NoError(t, err)
			assert.Equal(t, personfacts.InstructionStatusSuperseded, superseded.Status)
			assert.Equal(t, 1, instructionEventCount(t, st, newer.ID, "supersede"))
		})
	}
}

func TestInstructionTerminalDecisionClearsReviewQueue(t *testing.T) {
	for _, mode := range []string{"reject-unsure", "reject-suspended", "revoke-reviewed", "supersede-reviewed"} {
		t.Run(mode, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
			var id int64
			if mode == "reject-unsure" || mode == "reject-suspended" {
				status := personfacts.InstructionStatusProposed
				if mode == "reject-suspended" {
					status = personfacts.InstructionStatusSuspended
				}
				id = insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid work", status)
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET review_state='unsure' WHERE id=?`), id)
				require.NoError(t, err)
				_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionReject, Actor: "owner"})
				require.NoError(t, err)
			} else {
				row := addTestInstruction(t, st, alice.ID, "Vee")
				id = row.ID
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET review_state='merged' WHERE id=?`), id)
				require.NoError(t, err)
				if mode == "supersede-reviewed" {
					addTestInstruction(t, st, alice.ID, "Val")
				} else {
					_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionRevoke, Actor: "owner"})
					require.NoError(t, err)
				}
			}
			row, err := st.GetPersonInstructionContext(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, personfacts.InstructionReviewNone, row.ReviewState)
			queue, err := st.ListPersonInstructionReviewContext(t.Context(), 50)
			require.NoError(t, err)
			assert.Empty(t, queue)
		})
	}
}

func TestInstructionRenewsExpiredLiveKey(t *testing.T) {
	for _, mode := range []string{"add", "confirm-edit"} {
		t.Run(mode, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
			until := time.Now().UTC().Add(time.Hour)
			input := store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindTopic, Text: "avoid travel", ValidUntil: &until, Actor: "owner"}
			old, err := st.AddPersonInstructionContext(t.Context(), input)
			require.NoError(t, err)
			now := time.Now().UTC()
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET valid_from=?,valid_until=?,review_state='merged' WHERE id=?`), now.Add(-2*time.Hour), now.Add(-time.Hour), old.ID)
			require.NoError(t, err)
			visible, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
			require.NoError(t, err)
			assert.Empty(t, visible)
			var renewed *store.PersonInstruction
			if mode == "add" {
				renewed, err = st.AddPersonInstructionContext(t.Context(), input)
				require.NoError(t, err)
			} else {
				proposal := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
				result, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: proposal, ExpectedRevision: 1, Action: personfacts.InstructionActionConfirm, Edit: &store.PersonInstructionEdit{Text: &input.Text}, Actor: "owner"})
				require.NoError(t, err)
				renewed = &result.Instruction
			}
			assert.NotEqual(t, old.ID, renewed.ID)
			assert.Equal(t, old.InstructionKey, renewed.InstructionKey)
			assert.Equal(t, personfacts.InstructionStatusActive, renewed.Status)
			assert.True(t, renewed.VisibleToAgents)
			expired, err := st.GetPersonInstructionContext(t.Context(), old.ID)
			require.NoError(t, err)
			assert.Equal(t, personfacts.InstructionStatusExpired, expired.Status)
			assert.Equal(t, personfacts.InstructionReviewNone, expired.ReviewState)
			assert.Equal(t, old.Revision+1, expired.Revision)
			assert.Equal(t, 1, instructionEventCount(t, st, old.ID, "expire"))
			var actor string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT actor FROM person_instruction_events WHERE instruction_id=? AND action='expire'`), old.ID).Scan(&actor))
			assert.Regexp(t, "^system:", actor)
		})
	}
}

func TestInstructionConfirmDoesNotReviveTerminalReviewedRows(t *testing.T) {
	for _, status := range []personfacts.InstructionStatus{personfacts.InstructionStatusRevoked, personfacts.InstructionStatusRejected, personfacts.InstructionStatusSuperseded, personfacts.InstructionStatusExpired} {
		for _, review := range []personfacts.InstructionReviewState{personfacts.InstructionReviewMerged, personfacts.InstructionReviewSplit} {
			t.Run(string(status)+"/"+string(review), func(t *testing.T) {
				st := testutil.NewTestStore(t)
				alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
				row := addTestInstruction(t, st, alice.ID, "Vee")
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET status=?,review_state=? WHERE id=?`), status, review, row.ID)
				require.NoError(t, err)
				_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: row.ID, ExpectedRevision: row.Revision, Action: personfacts.InstructionActionConfirm, Actor: "owner"})
				assert.ErrorIs(t, err, store.ErrInstructionTransition)
				got, err := st.GetPersonInstructionContext(t.Context(), row.ID)
				require.NoError(t, err)
				assert.Equal(t, status, got.Status)
				assert.Equal(t, row.Revision, got.Revision)
				assert.Equal(t, 0, instructionEventCount(t, st, row.ID, "confirm"))
			})
		}
	}
}
