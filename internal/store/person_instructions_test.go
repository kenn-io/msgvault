package store_test

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func instructionTextForTest(t *testing.T, instruction *store.PersonInstruction) any {
	t.Helper()
	data, err := json.Marshal(instruction)
	require.NoError(t, err)
	var encoded map[string]any
	require.NoError(t, json.Unmarshal(data, &encoded))
	return encoded["text"]
}

func instructionEventDetailForTest(t *testing.T, st *store.Store, id int64, action string) map[string]any {
	t.Helper()
	var data string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT detail_json FROM person_instruction_events
		WHERE instruction_id=? AND action=? ORDER BY id DESC LIMIT 1`), id, action).Scan(&data))
	var detail map[string]any
	require.NoError(t, json.Unmarshal([]byte(data), &detail))
	return detail
}

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
	 (person_id,instruction_key,kind,original_text,normalized_text,value_text,origin,status,stated_at,valid_from,confirmed_at,confirmed_by,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,'extraction',?,?,?,?,?,?,?) RETURNING id`), personID, personfacts.InstructionKey(kind, "", value, text), kind, text, personfacts.NormalizeInstructionText(text), value, status, now, now, confirmedAt, confirmedBy, now, now).Scan(&id)
	require.NoError(t, err)
	return id
}
func instructionEventCount(t *testing.T, st *store.Store, id int64, action string) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_instruction_events WHERE instruction_id=? AND action=?`), id, action).Scan(&count))
	return count
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
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	row := addTestInstruction(t, st, alice.ID, "Vee")
	assert.Equal("owner_entered", row.ContentTrust)
	assert.True(row.VisibleToAgents)
	assert.Equal(int64(1), row.Revision)
	assert.Equal(1, instructionEventCount(t, st, row.ID, "propose"))
	// Removing participants leaves the independent person profile intact.
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM person_participants WHERE person_id=?`), alice.ID)
	require.NoError(err)
	row = addTestInstruction(t, st, alice.ID, "Val")
	assert.True(row.VisibleToAgents)
	_, err = st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindAddressAs, ValueText: "Val", Text: "call me Val", Actor: "owner"})
	require.ErrorIs(err, store.ErrInstructionDuplicate)
}

func TestAddPersonInstructionPreservesOriginalCapitalization(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	text := "Use the spelling iPhone"

	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: alice.ID, Kind: personfacts.InstructionKindTopic, Text: text, Actor: "owner",
	})
	requirements.NoError(err)
	checks.Equal(text, instructionTextForTest(t, row))
	checks.Equal("use the spelling iphone", row.NormalizedText)
	checks.Equal(text, instructionEventDetailForTest(t, st, row.ID, "propose")["text"])

	loaded, err := st.GetPersonInstructionContext(t.Context(), row.ID)
	requirements.NoError(err)
	checks.Equal(text, instructionTextForTest(t, loaded))
	listed, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
	requirements.NoError(err)
	requirements.Len(listed, 1)
	checks.Equal(text, instructionTextForTest(t, &listed[0]))
}

func TestConfirmPersonInstructionEditPreservesOriginalCapitalization(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	id := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid travel", personfacts.InstructionStatusProposed)
	text := "Use the spelling iPhone"

	result, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionConfirm,
		Edit: &store.PersonInstructionEdit{Text: &text}, Actor: "owner",
	})
	requirements.NoError(err)
	checks.Equal(text, instructionTextForTest(t, &result.Instruction))
	checks.Equal("use the spelling iphone", result.Instruction.NormalizedText)
	detail := instructionEventDetailForTest(t, st, id, "edit")
	checks.Equal("avoid travel", detail["old_text"])
	checks.Equal(text, detail["new_text"])

	loaded, err := st.GetPersonInstructionContext(t.Context(), id)
	requirements.NoError(err)
	checks.Equal(text, instructionTextForTest(t, loaded))
}

func TestPersonInstructionOriginalTextMigrationBackfillsLegacyValue(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: alice.ID, Kind: personfacts.InstructionKindTopic,
		Text: "Use the spelling iPhone", Actor: "owner",
	})
	requirements.NoError(err)

	_, err = st.DB().ExecContext(t.Context(), `ALTER TABLE person_instructions DROP COLUMN original_text`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM applied_migrations WHERE name=?`), "person_instruction_original_text_v1")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE archive_metadata SET value=? WHERE key='schema_version'`), strconv.Itoa(store.SchemaVersion-1))
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))

	version, err := st.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	checks.Equal(store.SchemaVersion, version)
	var got string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT original_text FROM person_instructions WHERE id=?`), row.ID).Scan(&got))
	checks.Equal("use the spelling iphone", got)
}

func TestAddInstructionRejectsForeignContactPoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	bob := mustPromotedPerson(t, st, "bob@example.com", "bob")
	point, err := st.AddPersonContactPointContext(t.Context(), bob.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "bob@example.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(err)
	_, err = st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindAvoidAddress, ContactPointID: &point.Envelope.ID, Text: "avoid bob@example.com", Actor: "owner"})
	require.ErrorIs(err, store.ErrInstructionInvalid)
	assert.ErrorContains(err, "unsupported-target")
}
func TestAddInstructionRejectsExpansion(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	_, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindOther, Text: "please cc bob@example.com", Actor: "owner"})
	require.ErrorIs(t, err, store.ErrInstructionInvalid)
}

func TestAddPersonInstructionSeparatesContactServiceAndScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	scopes := []struct {
		service string
		kind    string
		value   string
	}{
		{service: "irc", kind: "network", value: "irc.example.net"},
		{service: "irc", kind: "network", value: "irc.example.org"},
		{service: "slack", kind: "workspace", value: "T0EXAMPLE"},
	}
	rows := make([]store.PersonInstruction, 0, len(scopes))
	for _, scope := range scopes {
		point, err := st.AddPersonContactPointContext(t.Context(), alice.ID, store.PersonContactPointInput{
			AddressKind:   store.ContactAddressUsername,
			ServiceSlug:   new(scope.service),
			ScopeKind:     new(scope.kind),
			ScopeValue:    new(scope.value),
			OriginalValue: "alice",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
		row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
			PersonID: alice.ID, Kind: personfacts.InstructionKindAddressAs,
			ContactPointID: &point.Envelope.ID, ValueText: "Vee", Text: "call me Vee", Actor: "owner",
		})
		require.NoError(err)
		rows = append(rows, *row)
	}
	assert.Len(rows, len(scopes))
	assert.NotEqual(rows[0].ScopeKey, rows[1].ScopeKey)
	assert.NotEqual(rows[0].ScopeKey, rows[2].ScopeKey)
	assert.NotEqual(rows[1].ScopeKey, rows[2].ScopeKey)
	for _, row := range rows {
		assert.Equal(personfacts.InstructionStatusActive, row.Status)
	}
}

func TestInstructionEditUsesNormalizedContactAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	point, err := st.AddPersonContactPointContext(t.Context(), alice.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice1234567@example.com",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: alice.ID, Kind: personfacts.InstructionKindAvoidAddress,
		ContactPointID: &point.Envelope.ID, Text: "avoid alice1234567@example.com", Actor: "owner",
	})
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET review_state='merged' WHERE id=?`), row.ID)
	require.NoError(err)
	text := row.NormalizedText
	result, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
		InstructionID: row.ID, ExpectedRevision: row.Revision, Action: personfacts.InstructionActionConfirm,
		Edit: &store.PersonInstructionEdit{Text: &text}, Actor: "owner",
	})
	require.NoError(err)
	assert.Equal(personfacts.InstructionReviewNone, result.Instruction.ReviewState)
}

func TestAddAddressAsSupersedesPrevious(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	first := addTestInstruction(t, st, alice.ID, "Vee")
	second := addTestInstruction(t, st, alice.ID, "Val")
	require.NotNil(second.SupersedesID)
	assert.Equal(first.ID, *second.SupersedesID)
	old, err := st.GetPersonInstructionContext(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(personfacts.InstructionStatusSuperseded, old.Status)
	assert.Equal(1, instructionEventCount(t, st, first.ID, "supersede"))
	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(second.ID, rows[0].ID)
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
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	id := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusProposed)
	d := store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionConfirm, Actor: "owner"}
	_, err := st.DecidePersonInstructionContext(t.Context(), d)
	require.NoError(err)
	_, err = st.DecidePersonInstructionContext(t.Context(), d)
	require.ErrorIs(err, store.ErrInstructionRevisionConflict)
	assert.Equal(1, instructionEventCount(t, st, id, "confirm"))
}
func TestConfirmProposedWithEditRekeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	id := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusProposed)
	text, value := "call me Val", "Val"
	result, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionConfirm, Edit: &store.PersonInstructionEdit{Text: &text, ValueText: &value}, Actor: "owner"})
	require.NoError(err)
	assert.Equal(personfacts.InstructionKey(personfacts.InstructionKindAddressAs, "", value, text), result.Instruction.InstructionKey)
	assert.Equal([]store.PersonInstructionFollowOn{{Kind: "save_nickname", Value: "Val"}}, result.FollowOns)
	assert.Equal("person_statement_owner_confirmed", result.Instruction.ContentTrust)
	for action, key := range map[string]string{"edit": personfacts.InstructionKey(personfacts.InstructionKindAddressAs, "", "Vee", "call me Vee"), "confirm": result.Instruction.InstructionKey} {
		var got string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT instruction_key FROM person_instruction_events WHERE instruction_id=? AND action=?`), id, action).Scan(&got))
		assert.Equal(key, got)
	}
}
func TestSuspendedLeavesOnlyByConfirmOrAdopt(t *testing.T) {
	for _, action := range []personfacts.InstructionAction{personfacts.InstructionActionConfirm, personfacts.InstructionActionAdopt, personfacts.InstructionActionRevoke, personfacts.InstructionActionUnsure} {
		t.Run(string(action), func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
			id := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Vee", "call me Vee", personfacts.InstructionStatusSuspended)
			result, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: action, Actor: "owner"})
			if action == personfacts.InstructionActionConfirm || action == personfacts.InstructionActionAdopt {
				require.NoError(err)
				assert.Equal(t, personfacts.InstructionStatusActive, result.Instruction.Status)
			} else {
				require.ErrorIs(err, store.ErrInstructionTransition)
			}
		})
	}
}
func TestListInstructionsHidesExpiredWithoutWorker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	until := time.Now().UTC().Add(24 * time.Hour)
	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindTiming, Text: "no calls before 9am", ValidUntil: &until, Actor: "owner"})
	require.NoError(err)
	asOf := time.Now().In(time.FixedZone("test-offset", -7*60*60))
	rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{AsOf: asOf})
	require.NoError(err)
	assert.Len(rows, 1)
	rows, err = st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{AsOf: until.Add(time.Hour)})
	require.NoError(err)
	assert.Empty(rows)
	got, err := st.GetPersonInstructionContext(t.Context(), row.ID)
	require.NoError(err)
	assert.Equal(personfacts.InstructionStatusActive, got.Status)
	rows, err = st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{AsOf: until.Add(time.Hour), IncludeExpired: true})
	require.NoError(err)
	require.Len(rows, 1)
	assert.False(rows[0].VisibleToAgents)
}
func TestInstructionReviewQueueOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	first := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
	second := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid travel", personfacts.InstructionStatusSuspended)
	rows, err := st.ListPersonInstructionReviewContext(t.Context(), 1)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(first, rows[0].ID)
	rows, err = st.ListPersonInstructionReviewContext(t.Context(), 2)
	require.NoError(err)
	require.Len(rows, 2)
	assert.Equal(second, rows[1].ID)
	_, err = st.ListPersonInstructionReviewContext(t.Context(), 201)
	require.ErrorIs(err, store.ErrInstructionInvalid)
}

func TestInstructionReviewQueueExcludesTerminalHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
	proposed := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
	suspended := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid meetings", personfacts.InstructionStatusSuspended)
	merged, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{
		PersonID: alice.ID, Kind: personfacts.InstructionKindAddressAs, ValueText: "Vee", Text: "call me Vee", Actor: "owner",
	})
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET review_state='merged' WHERE id=?`), merged.ID)
	require.NoError(err)
	terminal := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid travel", personfacts.InstructionStatusRevoked)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET review_state='split' WHERE id=?`), terminal)
	require.NoError(err)

	rows, err := st.ListPersonInstructionReviewContext(t.Context(), 200)
	require.NoError(err)
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	assert.ElementsMatch([]int64{proposed, suspended, merged.ID}, ids)
}

func TestInstructionDecisionRejectsExpiredMergedRowsBeforeSuperseding(t *testing.T) {
	for _, action := range []personfacts.InstructionAction{personfacts.InstructionActionConfirm, personfacts.InstructionActionAdopt} {
		t.Run(string(action), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
			valid := addTestInstruction(t, st, alice.ID, "Vee")
			expired := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindAddressAs, "Val", "call me Val", personfacts.InstructionStatusActive)
			status := personfacts.InstructionStatusActive
			if action == personfacts.InstructionActionAdopt {
				status = personfacts.InstructionStatusSuspended
			}
			now := time.Now().UTC()
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET status=?,review_state='merged',valid_from=?,valid_until=? WHERE id=?`), status, now.Add(-2*time.Hour), now.Add(-time.Hour), expired)
			require.NoError(err)

			_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{
				InstructionID: expired, ExpectedRevision: 1, Action: action, Actor: "owner",
			})
			require.ErrorIs(err, store.ErrInstructionInvalid)
			unchanged, err := st.GetPersonInstructionContext(t.Context(), expired)
			require.NoError(err)
			assert.Equal(status, unchanged.Status)
			assert.Equal(personfacts.InstructionReviewMerged, unchanged.ReviewState)
			assert.Equal(0, instructionEventCount(t, st, expired, string(action)))
			current, err := st.GetPersonInstructionContext(t.Context(), valid.ID)
			require.NoError(err)
			assert.Equal(personfacts.InstructionStatusActive, current.Status)
			assert.Equal(0, instructionEventCount(t, st, valid.ID, "supersede"))
		})
	}
}

func TestInstructionOwnerChoiceSupersedesCurrentSingleValue(t *testing.T) {
	for _, mode := range []string{"confirm-intervening", "adopt-suspended", "confirm-edit"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
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
			require.NoError(err)
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
			require.NoError(err)
			rows, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(result.Instruction.ID, rows[0].ID)
			superseded, err := st.GetPersonInstructionContext(t.Context(), newer.ID)
			require.NoError(err)
			assert.Equal(personfacts.InstructionStatusSuperseded, superseded.Status)
			assert.Equal(1, instructionEventCount(t, st, newer.ID, "supersede"))
		})
	}
}

func TestInstructionTerminalDecisionClearsReviewQueue(t *testing.T) {
	for _, mode := range []string{"reject-unsure", "reject-suspended", "revoke-reviewed", "supersede-reviewed"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
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
				require.NoError(err)
				_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionReject, Actor: "owner"})
				require.NoError(err)
			} else {
				row := addTestInstruction(t, st, alice.ID, "Vee")
				id = row.ID
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET review_state='merged' WHERE id=?`), id)
				require.NoError(err)
				if mode == "supersede-reviewed" {
					addTestInstruction(t, st, alice.ID, "Val")
				} else {
					_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: 1, Action: personfacts.InstructionActionRevoke, Actor: "owner"})
					require.NoError(err)
				}
			}
			row, err := st.GetPersonInstructionContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(personfacts.InstructionReviewNone, row.ReviewState)
			queue, err := st.ListPersonInstructionReviewContext(t.Context(), 50)
			require.NoError(err)
			assert.Empty(queue)
		})
	}
}

func TestInstructionRenewsExpiredLiveKey(t *testing.T) {
	for _, mode := range []string{"add", "confirm-edit"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
			until := time.Now().UTC().Add(time.Hour)
			input := store.PersonInstructionInput{PersonID: alice.ID, Kind: personfacts.InstructionKindTopic, Text: "avoid travel", ValidUntil: &until, Actor: "owner"}
			old, err := st.AddPersonInstructionContext(t.Context(), input)
			require.NoError(err)
			now := time.Now().UTC()
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET valid_from=?,valid_until=?,review_state='merged' WHERE id=?`), now.Add(-2*time.Hour), now.Add(-time.Hour), old.ID)
			require.NoError(err)
			visible, err := st.ListPersonInstructionsContext(t.Context(), alice.ID, store.PersonInstructionFilter{})
			require.NoError(err)
			assert.Empty(visible)
			var renewed *store.PersonInstruction
			if mode == "add" {
				renewed, err = st.AddPersonInstructionContext(t.Context(), input)
				require.NoError(err)
			} else {
				proposal := insertTestPersonInstruction(t, st, alice.ID, personfacts.InstructionKindTopic, "", "avoid work", personfacts.InstructionStatusProposed)
				result, err := st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: proposal, ExpectedRevision: 1, Action: personfacts.InstructionActionConfirm, Edit: &store.PersonInstructionEdit{Text: &input.Text}, Actor: "owner"})
				require.NoError(err)
				renewed = &result.Instruction
			}
			assert.NotEqual(old.ID, renewed.ID)
			assert.Equal(old.InstructionKey, renewed.InstructionKey)
			assert.Equal(personfacts.InstructionStatusActive, renewed.Status)
			assert.True(renewed.VisibleToAgents)
			expired, err := st.GetPersonInstructionContext(t.Context(), old.ID)
			require.NoError(err)
			assert.Equal(personfacts.InstructionStatusExpired, expired.Status)
			assert.Equal(personfacts.InstructionReviewNone, expired.ReviewState)
			assert.Equal(old.Revision+1, expired.Revision)
			assert.Equal(1, instructionEventCount(t, st, old.ID, "expire"))
			var actor string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT actor FROM person_instruction_events WHERE instruction_id=? AND action='expire'`), old.ID).Scan(&actor))
			assert.Regexp("^system:", actor)
		})
	}
}

func TestInstructionConfirmDoesNotReviveTerminalReviewedRows(t *testing.T) {
	for _, status := range []personfacts.InstructionStatus{personfacts.InstructionStatusRevoked, personfacts.InstructionStatusRejected, personfacts.InstructionStatusSuperseded, personfacts.InstructionStatusExpired} {
		for _, review := range []personfacts.InstructionReviewState{personfacts.InstructionReviewMerged, personfacts.InstructionReviewSplit} {
			t.Run(string(status)+"/"+string(review), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st := testutil.NewTestStore(t)
				alice := mustPromotedPerson(t, st, "alice@example.com", "alice")
				row := addTestInstruction(t, st, alice.ID, "Vee")
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_instructions SET status=?,review_state=? WHERE id=?`), status, review, row.ID)
				require.NoError(err)
				_, err = st.DecidePersonInstructionContext(t.Context(), store.PersonInstructionDecision{InstructionID: row.ID, ExpectedRevision: row.Revision, Action: personfacts.InstructionActionConfirm, Actor: "owner"})
				require.ErrorIs(err, store.ErrInstructionTransition)
				got, err := st.GetPersonInstructionContext(t.Context(), row.ID)
				require.NoError(err)
				assert.Equal(status, got.Status)
				assert.Equal(row.Revision, got.Revision)
				assert.Equal(0, instructionEventCount(t, st, row.ID, "confirm"))
			})
		}
	}
}
