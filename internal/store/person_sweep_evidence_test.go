package store_test

import (
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/imazingcsv"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func addSweepBody(t *testing.T, f personSweepJournalFixture, id int64, body string) {
	t.Helper()
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`), id, body)
	require.NoError(t, err)
}

func loadSweepWindow(t *testing.T, f personSweepJournalFixture, lane peoplesweep.SourceClass, after int64) peoplesweep.PersonWindow {
	t.Helper()
	window, err := f.store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: f.alicePersonID, Lane: lane, AfterSequence: after,
		ThroughSequence: latestPersonSweepSequence(t, f.store), Limit: 100,
	})
	require.NoError(t, err)
	return window
}

func TestLoadPersonSweepWindowUsesExactDurableScope(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	collisionID, err := f.store.EnsureParticipant("collision@example.test", "Alice", "example.test")
	requirements.NoError(err)
	aliceMessage := f.insertMessage(t, "alice", "email", f.aliceID, time.Now().UTC())
	bobMessage := f.insertMessage(t, "bob", "email", f.bobID, time.Now().UTC())
	collisionMessage := f.insertMessage(t, "same-name-and-address", "email", collisionID, time.Now().UTC())
	addSweepBody(t, f, aliceMessage, "Alice durable text")
	addSweepBody(t, f, bobMessage, "Bob durable text")
	addSweepBody(t, f, collisionMessage, "Name and envelope address collision text")
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), collisionMessage, f.bobID, "alice@example.test")
	requirements.NoError(err)

	window := loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0)
	for _, item := range window.Seeds {
		checks.NotEqual(bobMessage, item.Ref.MessageID)
		checks.NotEqual(collisionMessage, item.Ref.MessageID)
		checks.Equal(f.alicePersonID, item.PersonID)
	}
	checks.Contains(sweepMessageIDs(window.Seeds), aliceMessage)
}

func TestPersonSweepDurableScopeMatchesResolver(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	aliasID, err := f.store.EnsureParticipant("scope-alias@example.test", "Scope Alias", "example.test")
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`INSERT INTO person_participants (person_id, participant_id) VALUES (?, ?)`), f.alicePersonID, aliasID)
	requirements.NoError(err)
	for index, sender := range []int64{f.aliceID, aliasID, f.bobID} {
		id := f.insertMessage(t, "scope-parity-"+string(rune('a'+index)), "email", sender, time.Now().UTC())
		addSweepBody(t, f, id, "Scope parity text")
	}

	resolved, err := resolver.Resolve(t.Context(), f.store,
		resolver.Reference{Kind: resolver.ReferencePerson, ID: f.alicePersonID}, nil)
	requirements.NoError(err)
	predicate, args := personscope.MessagePredicate(resolved.Scope, "m", "c")
	rows, err := f.store.DB().QueryContext(t.Context(), f.store.Rebind(`
		SELECT m.id FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL AND (`+predicate+`)
		ORDER BY COALESCE(m.sent_at,m.received_at,m.internal_date,m.archived_at) DESC,m.id DESC`), args...)
	requirements.NoError(err)
	t.Cleanup(func() { _ = rows.Close() })
	var want []int64
	for rows.Next() {
		var id int64
		requirements.NoError(rows.Scan(&id))
		want = append(want, id)
	}
	requirements.NoError(rows.Err())
	got, err := f.store.ListPersonSweepHistoricalCandidates(t.Context(),
		peoplesweep.HistoricalCandidateRequest{PersonID: f.alicePersonID, Limit: 100})
	requirements.NoError(err)
	assert.Equal(t, want, got)
}

func TestListPersonSweepHistoricalCandidatesFiltersBeforeLimit(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	eligible := f.insertMessage(t, "eligible-meeting", "meeting_transcript", f.aliceID,
		time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	f.insertMessage(t, "newer-conversation", "email", f.aliceID,
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	got, err := f.store.ListPersonSweepHistoricalCandidates(t.Context(),
		peoplesweep.HistoricalCandidateRequest{PersonID: f.alicePersonID,
			SourceClasses: []peoplesweep.SourceClass{peoplesweep.SourceMeetingText},
			SourceSince:   "2025-01-01", SourceUntil: "2026-01-01", Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, []int64{eligible}, got)
}

// TestListPersonSweepHistoricalCandidatesAuthoredByPersonSkipsOwnerMessages
// pins the listing filter the brief window relies on: with AuthoredByPerson
// the newest-first listing counts only messages the person sent on a source
// that authenticates its sender, so an owner-heavy conversation cannot fill
// the limit ahead of the person's own older messages, and the rows it returns
// are exactly the ones hydration marks with the person as its own subject.
func TestListPersonSweepHistoricalCandidatesAuthoredByPersonSkipsOwnerMessages(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	// The owner's messages reach the person through the direct chat's roster.
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO conversation_participants (conversation_id, participant_id, role)
		VALUES (?, ?, 'member')`), f.conversationID, f.aliceID)
	requirements.NoError(err)
	older := f.insertMessage(t, "alice-older", "slack_message", f.aliceID,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	newer := f.insertMessage(t, "alice-newer", "slack_message", f.aliceID,
		time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC))
	for day := 3; day <= 10; day++ {
		f.insertMessage(t, fmt.Sprintf("bob-%d", day), "slack_message", f.bobID,
			time.Date(2026, 5, day, 12, 0, 0, 0, time.UTC))
	}
	addSweepBody(t, f, older, "Alice's older text")
	addSweepBody(t, f, newer, "Alice's newer text")

	unfiltered, err := f.store.ListPersonSweepHistoricalCandidates(t.Context(),
		peoplesweep.HistoricalCandidateRequest{PersonID: f.alicePersonID, Limit: 4})
	requirements.NoError(err)
	requirements.Len(unfiltered, 4)
	checks.NotContains(unfiltered, older,
		"without the filter the owner's newer messages exhaust the limit first")

	authored, err := f.store.ListPersonSweepHistoricalCandidates(t.Context(),
		peoplesweep.HistoricalCandidateRequest{PersonID: f.alicePersonID, Limit: 4,
			AuthoredByPerson: true})
	requirements.NoError(err)
	checks.Equal([]int64{newer, older}, authored,
		"the filtered listing is newest-first over the person's own messages only")

	hydrated, err := f.store.HydratePersonSweepMessages(t.Context(), f.alicePersonID, authored, 0)
	requirements.NoError(err)
	requirements.Len(hydrated, 2)
	for _, item := range hydrated {
		requirements.NotNil(item.SubjectPersonID)
		checks.Equal(f.alicePersonID, *item.SubjectPersonID,
			"every listed candidate is evidence the person is the subject of")
	}

	// An email source does not authenticate its sender, so a message the
	// person sent there is never their own subject and must not be listed.
	mailSource, err := f.store.GetOrCreateSource("gmail", "alice-mail")
	requirements.NoError(err)
	mailConversation, err := f.store.EnsureConversationWithType(
		mailSource.ID, "alice-mail-thread", "", "Synthetic thread")
	requirements.NoError(err)
	mailID, err := f.store.UpsertMessage(&store.Message{
		SourceID: mailSource.ID, SourceMessageID: "alice-mail-1",
		ConversationID: mailConversation, MessageType: "email",
		SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true},
		SentAt:   sql.NullTime{Time: time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC), Valid: true},
		Subject:  sql.NullString{String: "Synthetic mail", Valid: true},
	})
	requirements.NoError(err)
	withMail, err := f.store.ListPersonSweepHistoricalCandidates(t.Context(),
		peoplesweep.HistoricalCandidateRequest{PersonID: f.alicePersonID, Limit: 4,
			AuthoredByPerson: true})
	requirements.NoError(err)
	checks.Equal([]int64{newer, older}, withMail)
	checks.NotContains(withMail, mailID)
}

func TestLoadPersonSweepWindowPreservesFromToAndGroupProvenance(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE conversations SET conversation_type = 'group_chat' WHERE id = ?`), f.conversationID)
	requirements.NoError(err)
	id := f.insertMessage(t, "edges", "email", f.aliceID, time.Now().UTC())
	recipientID := f.insertMessage(t, "recipient", "email", f.bobID, time.Now().UTC())
	groupID := f.insertMessage(t, "group", "email", f.bobID, time.Now().UTC())
	addSweepBody(t, f, id, "Exact edge text")
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), id, f.aliceID, "alice@example.test")
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), recipientID, f.aliceID, "alice@example.test")
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO conversation_participants (conversation_id, participant_id, role)
		VALUES (?, ?, 'member')`), f.conversationID, f.aliceID)
	requirements.NoError(err)

	window := loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0)
	item := sweepItem(t, window.Seeds, id)
	checks.ElementsMatch([]personscope.Role{personscope.RoleFrom, personscope.RoleTo, personscope.RoleConversationMember}, item.Provenance.Roles)
	checks.ElementsMatch([]personscope.Direction{personscope.FromPerson, personscope.ToPerson, personscope.Group}, item.Provenance.Directions)
	requirements.NotNil(item.SubjectPersonID)
	checks.Equal(f.alicePersonID, *item.SubjectPersonID)
	checks.Nil(sweepItem(t, window.Seeds, recipientID).SubjectPersonID)
	checks.Nil(sweepItem(t, window.Seeds, groupID).SubjectPersonID)
}

func TestLoadPersonSweepWindowIncludesLinkedAliases(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	aliasID, err := f.store.EnsureParticipant("alias@example.test", "Alias", "example.test")
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`INSERT INTO person_participants (person_id, participant_id) VALUES (?, ?)`), f.alicePersonID, aliasID)
	requirements.NoError(err)
	id := f.insertMessage(t, "alias", "email", aliasID, time.Now().UTC())
	addSweepBody(t, f, id, "Alias-owned text")

	window := loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0)
	checks.Equal(1, countSweepMessage(window.Seeds, id))
	checks.Contains(sweepItem(t, window.Seeds, id).Provenance.ParticipantIDs, aliasID)
}

func TestLoadPersonSweepWindowReturnsDeletionTombstone(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	id := f.insertMessage(t, "deleted", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "Secret durable text")
	seed := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id)
	input := sweepEvidenceInput(t, seed)
	key, err := personfacts.EvidenceKey(input)
	requirements.NoError(err)
	persistSweepEvidence(t, f.store, key, input)
	after := latestPersonSweepSequence(t, f.store)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM messages WHERE id = ?`), id)
	requirements.NoError(err)

	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, after).Seeds, id)
	checks.True(item.Tombstone)
	checks.Equal(key, item.EvidenceKey)
	checks.Equal(input.SourceVersion, item.SourceVersion)
	checks.Equal(seed.Ref, item.Ref)
	checks.Empty(item.Excerpt)
	checks.Empty(item.ContentSHA256)
	checks.NoError(peoplesweep.ValidatePersonSweepEvidenceItem(item))
}

func TestLoadPersonSweepWindowIncludesLateOldMessage(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	after := latestPersonSweepSequence(t, f.store)
	id := f.insertMessage(t, "late-old", "email", f.aliceID, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, id, "Late imported old text")
	assert.Contains(t, sweepMessageIDs(loadSweepWindow(t, f, peoplesweep.SourceConversationText, after).Seeds), id)
}

func TestLoadPersonSweepWindowSeparatesMeetingAndConversationLanes(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	conversationID := f.insertMessage(t, "chat", "email", f.aliceID, time.Now().UTC())
	meetingID := f.insertMessage(t, "meeting", "meeting_transcript", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, conversationID, "Chat text")
	addSweepBody(t, f, meetingID, "Meeting text")
	assert.Contains(t, sweepMessageIDs(loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds), conversationID)
	assert.NotContains(t, sweepMessageIDs(loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds), meetingID)
	assert.Equal(t, []int64{meetingID}, sweepMessageIDs(loadSweepWindow(t, f, peoplesweep.SourceMeetingText, 0).Seeds))
}

func TestLoadPersonSweepWindowBackstopCapturesFreshUpperKey(t *testing.T) {
	checks := assert.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	firstID := f.insertMessage(t, "backstop-first", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, firstID, "First current source")
	staleUpper := fmt.Sprintf("%020d", firstID)

	secondID := f.insertMessage(t, "backstop-second", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, secondID, "Second source added after initial reconciliation")

	window, err := f.store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: f.alicePersonID, Lane: peoplesweep.SourceConversationText,
		Mode: peoplesweep.GenerationCursorBackstop, ReconcileUpper: staleUpper, Limit: 100,
	})
	require.NoError(t, err)
	checks.Equal(fmt.Sprintf("%020d", secondID), window.CapturedUpperKey)
	checks.Equal(fmt.Sprintf("%020d", secondID), window.NextReconcileKey)
	checks.True(window.ReconciliationDone)
	checks.Contains(sweepMessageIDs(window.Seeds), firstID)
	checks.Contains(sweepMessageIDs(window.Seeds), secondID)
}

func TestLoadPersonSweepWindowBackstopContinuesCapturedRange(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	firstID := f.insertMessage(t, "backstop-page-first", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, firstID, "First page source")
	secondID := f.insertMessage(t, "backstop-page-second", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, secondID, "Second page source")

	first, err := f.store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: f.alicePersonID, Lane: peoplesweep.SourceConversationText,
		Mode: peoplesweep.GenerationCursorBackstop, Limit: 1,
	})
	requirements.NoError(err)
	checks.False(first.ReconciliationDone)
	checks.Equal(fmt.Sprintf("%020d", firstID), first.NextReconcileKey)
	checks.Equal(fmt.Sprintf("%020d", secondID), first.CapturedUpperKey)

	thirdID := f.insertMessage(t, "backstop-after-capture", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, thirdID, "Must wait for the next backstop")
	second, err := f.store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: f.alicePersonID, Lane: peoplesweep.SourceConversationText,
		Mode: peoplesweep.GenerationCursorBackstop, ReconcileAfter: first.NextReconcileKey,
		BackstopUpper: first.CapturedUpperKey, Limit: 1,
	})
	requirements.NoError(err)
	checks.True(second.ReconciliationDone)
	checks.Equal(first.CapturedUpperKey, second.CapturedUpperKey)
	checks.Equal(first.CapturedUpperKey, second.NextReconcileKey)
	checks.Contains(sweepMessageIDs(second.Seeds), secondID)
	checks.NotContains(sweepMessageIDs(second.Seeds), thirdID)
}

func TestPersonSweepEvidenceRejectsWrongCanonicalSubject(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	id := f.insertMessage(t, "subject", "email", f.bobID, time.Now().UTC())
	addSweepBody(t, f, id, "I work in a synthetic role")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), id, f.aliceID, "alice@example.test")
	requirements.NoError(err)
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id)
	checks.Nil(item.SubjectPersonID)
	wrong := f.alicePersonID
	input := sweepEvidenceInput(t, item)
	input.SubjectPersonID = &wrong
	result, err := (store.PersonSweepEvidenceAligner{Store: f.store}).Align(t.Context(), input)
	requirements.NoError(err)
	requirements.False(result.Accepted)
	requirements.NotNil(result.Failure)
	checks.Equal(personfacts.ReasonIdentityMismatch, result.Failure.Reason)
}

func TestPersonSweepEvidenceRejectsEditBetweenPacketAndPrepare(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	id := f.insertMessage(t, "edit", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "Before edit")
	input := sweepEvidenceInput(t, sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), "After edit", id)
	requirements.NoError(err)
	result, err := (store.PersonSweepEvidenceAligner{Store: f.store}).Align(t.Context(), input)
	requirements.NoError(err)
	checks.False(result.Accepted)
	checks.Equal(personfacts.ReasonUnalignedEvidence, result.Failure.Reason)
}

func TestPersonSweepEmailEvidenceDoesNotTrustUnauthenticatedFromHeader(t *testing.T) {
	checks := assert.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE sources SET source_type = 'gmail' WHERE id = ?`), f.sourceID)
	require.NoError(t, err)
	id := f.insertMessage(t, "authored subject", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "I work in product.\n\nOn Monday, Someone wrote:\n> I am the CEO.\n\n-- \nQuoted Signature")
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id)
	checks.Nil(item.SubjectPersonID)
	checks.Equal(personfacts.DirectOther, item.Directness)
	checks.Contains(item.Excerpt, "I work in product")
	checks.NotContains(item.Excerpt, "I am the CEO")
	checks.NotContains(item.Excerpt, "Quoted Signature")
}

func TestSearchPersonSweepMessagesPreservesNewestFirstCandidateRanking(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	older := f.insertMessage(t, "older", "email", f.aliceID,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := f.insertMessage(t, "newer", "email", f.aliceID,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, older, "needle older")
	addSweepBody(t, f, newer, "needle newer")

	got, err := f.store.SearchPersonSweepMessages(t.Context(), peoplesweep.ContextRequest{
		PersonID: f.alicePersonID, CandidateMessageIDs: []int64{newer, older},
		Target:        personfacts.TargetDescriptor{Description: "needle"},
		SourceClasses: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, Limit: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{newer}, sweepMessageIDs(got))
}

func TestPersonSweepAuthenticatedChatSenderCanBeDirectSelf(t *testing.T) {
	for _, sourceType := range []string{"matrix", "slack", "slackdump"} {
		t.Run(sourceType, func(t *testing.T) {
			f := newPersonSweepJournalFixture(t, true, false)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
				`UPDATE sources SET source_type = ? WHERE id = ?`), sourceType, f.sourceID)
			require.NoError(t, err)
			id := f.insertMessage(t, "authored chat", "chat", f.aliceID, time.Now().UTC())
			addSweepBody(t, f, id, "I work in product.")
			item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id)
			require.NotNil(t, item.SubjectPersonID)
			assert.Equal(t, personfacts.DirectSelf, item.Directness)
		})
	}
}

func TestPersonSweepAttributesImazingCSVOutgoingMessagesToPerson(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	// Import a real iMazing CSV export whose outgoing row was sent by the
	// archive owner, then track the owner as a person.
	exportRoot := t.TempDir()
	csvDir := filepath.Join(exportRoot, "csv")
	requirements.NoError(os.Mkdir(csvDir, 0o700))
	csvFile, err := os.Create(filepath.Join(csvDir, "messages.csv"))
	requirements.NoError(err)
	writer := csv.NewWriter(csvFile)
	requirements.NoError(writer.Write([]string{
		"Chat Session", "Message Date", "Delivered Date", "Read Date", "Service", "Type",
		"Sender ID", "Sender Name", "Status", "Replying to", "Subject", "Text", "Attachment", "Attachment type",
	}))
	requirements.NoError(writer.Write([]string{
		"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "I prefer chat", "", "",
	}))
	writer.Flush()
	requirements.NoError(writer.Error())
	requirements.NoError(csvFile.Close())
	_, err = imazingcsv.NewImporter(st, imazingcsv.Options{Owner: "+15550000001", Timezone: "UTC"}).
		ImportPath(t.Context(), exportRoot)
	requirements.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(imazingcsv.SourceType, "+15550000001")
	requirements.NoError(err)
	var ownerParticipantID, messageID int64
	requirements.NoError(st.DB().QueryRow(`
		SELECT id FROM participants WHERE phone_number = '+15550000001'`).Scan(&ownerParticipantID))
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id FROM messages WHERE source_id = ?`), source.ID).Scan(&messageID))
	person, _, err := st.CreatePersonFromParticipant(ownerParticipantID)
	requirements.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	requirements.NoError(err)

	items, err := st.HydratePersonSweepMessages(t.Context(), person.ID, []int64{messageID}, 0)
	requirements.NoError(err)
	requirements.Len(items, 1)
	requirements.NotNil(items[0].SubjectPersonID)
	checks.Equal(person.ID, *items[0].SubjectPersonID)
	checks.Equal(personfacts.DirectSelf, items[0].Directness)

	candidates, err := st.ListPersonSweepHistoricalCandidates(t.Context(), peoplesweep.HistoricalCandidateRequest{
		PersonID: person.ID, AuthoredByPerson: true, Limit: 10,
	})
	requirements.NoError(err)
	checks.Equal([]int64{messageID}, candidates,
		"outgoing imazing_csv messages must count as person authorship")
}

func TestPersonSweepEvidenceStatusChangesCoalesceToTerminalEffect(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	id := f.insertMessage(t, "status", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "Status evidence text")
	input := sweepEvidenceInput(t, sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id))
	key, err := personfacts.EvidenceKey(input)
	require.NoError(t, err)
	persistSweepEvidence(t, f.store, key, input)
	base := peoplesweep.ArchiveChange{PersonID: f.alicePersonID,
		SourceLane: peoplesweep.SourceConversationText, SourceID: f.sourceID, MessageID: id}
	archiveChange := func(sequence int64, personID int64, effect peoplesweep.EvidenceChangeEffect) peoplesweep.ArchiveChange {
		change := base
		change.Sequence = sequence
		change.PersonID = personID
		change.EvidenceEffect = effect
		return change
	}
	changes, err := f.store.BuildPersonSweepEvidenceStatusChanges(t.Context(), f.alicePersonID, []peoplesweep.ArchiveChange{
		archiveChange(1, f.bobPersonID, peoplesweep.EvidenceEffectSourceDeleted),
		archiveChange(2, f.alicePersonID, peoplesweep.EvidenceEffectSourceDeleted),
		archiveChange(3, f.alicePersonID, peoplesweep.EvidenceEffectSourceEdited),
		archiveChange(4, f.alicePersonID, peoplesweep.EvidenceEffectScopeUnlinked),
		archiveChange(5, f.alicePersonID, peoplesweep.EvidenceEffectIdentityReassigned),
		archiveChange(6, f.alicePersonID, peoplesweep.EvidenceEffectSourceReimported),
		archiveChange(7, f.alicePersonID, peoplesweep.EvidenceEffectScopeRelinked),
	})
	require.NoError(t, err)
	assert.Equal(t, []personfacts.EvidenceStatusChange{
		{EvidenceKey: key, SourceVersion: input.SourceVersion, Supported: true, Reason: personfacts.EvidenceStatusScopeRelinked},
	}, changes)
}

func TestPersonSweepReimportDoesNotReactivateStaleEvidenceVersion(t *testing.T) {
	must := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	id := f.insertMessage(t, "reimport", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "Evidence before reimport")
	input := sweepEvidenceInput(t,
		sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id))
	key, err := personfacts.EvidenceKey(input)
	must.NoError(err)
	persistSweepEvidence(t, f.store, key, input)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`),
		"Different evidence after reimport", id)
	must.NoError(err)

	changes, err := f.store.BuildPersonSweepEvidenceStatusChanges(t.Context(), f.alicePersonID,
		[]peoplesweep.ArchiveChange{{
			PersonID: f.alicePersonID, SourceLane: peoplesweep.SourceConversationText,
			SourceID: f.sourceID, MessageID: id,
			EvidenceEffect: peoplesweep.EvidenceEffectSourceReimported,
		}})
	must.NoError(err)
	assert.Empty(t, changes)
}

func TestPersonSweepEvidenceNonTextAttachmentsFailClosed(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	ref, err := peoplesweep.EncodePersonSweepEvidenceRef(peoplesweep.EvidenceRef{
		SourceLane: peoplesweep.SourceAttachmentOCR, SourceID: f.sourceID,
		MessageID: 1, AttachmentID: 1, SpanEnd: 1,
	})
	require.NoError(t, err)
	_, err = (store.PersonSweepEvidenceAligner{Store: f.store}).Align(t.Context(), personfacts.EvidenceInput{
		PersonID: f.alicePersonID, SourceClass: personfacts.EvidenceArchive, SourceRef: ref,
	})
	assert.ErrorIs(t, err, peoplesweep.ErrSourceTextUnavailable)
}

func TestLoadPersonSweepWindowNonTextAttachmentsFailClosed(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)

	_, err := f.store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: f.alicePersonID, Lane: peoplesweep.SourceAttachmentOCR, Limit: 10,
	})

	require.ErrorIs(t, err, peoplesweep.ErrSourceTextUnavailable)
}

func sweepEvidenceInput(t *testing.T, item peoplesweep.EvidenceItem) personfacts.EvidenceInput {
	t.Helper()
	ref, err := peoplesweep.EncodePersonSweepEvidenceRef(item.Ref)
	require.NoError(t, err)
	start, end := int64(item.Highlight.Start), int64(item.Highlight.End)
	return personfacts.EvidenceInput{PersonID: item.PersonID, SubjectPersonID: item.SubjectPersonID,
		SourceClass: personfacts.EvidenceArchive, SourceRef: ref, Directness: item.Directness,
		Authority: item.Authority, SourceVersion: item.SourceVersion, ContentSHA256: item.ContentSHA256,
		EventTime: item.EventTime, RecordedTime: item.RecordedTime, Excerpt: item.Excerpt,
		SpanStart: &start, SpanEnd: &end, IdentityScore: item.IdentityBasisPoints}
}

func persistSweepEvidence(t *testing.T, st *store.Store, key string, input personfacts.EvidenceInput) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_evidence
			(person_id,evidence_key,source_class,directness,authority,source_ref,source_url,
			 subject_person_id,subject_ref,span_start,span_end,excerpt,content_sha256,source_version,
			 event_time,recorded_time,identity_score)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		input.PersonID, key, input.SourceClass, input.Directness, input.Authority, input.SourceRef, "",
		input.SubjectPersonID, "", input.SpanStart, input.SpanEnd, input.Excerpt, input.ContentSHA256,
		input.SourceVersion, input.EventTime, input.RecordedTime, input.IdentityScore)
	require.NoError(t, err)
}

func sweepItem(t *testing.T, items []peoplesweep.EvidenceItem, id int64) peoplesweep.EvidenceItem {
	t.Helper()
	for _, item := range items {
		if item.Ref.MessageID == id {
			return item
		}
	}
	require.FailNow(t, "missing sweep evidence item", "message %d", id)
	return peoplesweep.EvidenceItem{}
}

func sweepMessageIDs(items []peoplesweep.EvidenceItem) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.Ref.MessageID)
	}
	return ids
}

func countSweepMessage(items []peoplesweep.EvidenceItem, id int64) int {
	count := 0
	for _, item := range items {
		if item.Ref.MessageID == id {
			count++
		}
	}
	return count
}

// TestPersonSweepRetrievalHonorsTheJournalSequenceBound pins the contract a
// brief boundary relies on: retrieval bounded by a journal sequence returns
// exactly the messages the journal had committed for the person at that
// sequence. A message journaled later is out on every path, a message whose
// last journal row is the bound itself is in, and a message the journal never
// saw, here one archived before the person was tracked, is in as well, because
// it can never trigger a regeneration and hiding it would lose old history.
func TestPersonSweepRetrievalHonorsTheJournalSequenceBound(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, false, false)
	unjournaled := f.insertMessage(t, "before-tracking", "email", f.aliceID,
		time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	addSweepBody(t, f, unjournaled, "archived before the person was tracked")
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.alicePersonID, true)
	requirements.NoError(err)
	requirements.Empty(personSweepChangesAfter(t, f.store, f.alicePersonID, 0),
		"tracking a person journals nothing for the messages already archived")

	atBound := f.insertMessage(t, "at-bound", "email", f.aliceID,
		time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))
	addSweepBody(t, f, atBound, "committed at the bound")
	through := latestPersonSweepSequence(t, f.store)
	bounded := personSweepChangesAfter(t, f.store, f.alicePersonID, 0)
	requirements.NotEmpty(bounded)
	checks.Equal(atBound, bounded[len(bounded)-1].MessageID)
	checks.Equal(through, bounded[len(bounded)-1].Sequence,
		"the bound is this message's own last journal row")

	late := f.insertMessage(t, "after-planning", "meeting_transcript", f.aliceID,
		time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	addSweepBody(t, f, late, "committed after the bound was captured")
	checks.Greater(latestPersonSweepSequence(t, f.store), through)

	unbounded, err := f.store.ListPersonSweepHistoricalCandidates(t.Context(),
		peoplesweep.HistoricalCandidateRequest{PersonID: f.alicePersonID, Limit: 10})
	requirements.NoError(err)
	checks.Equal([]int64{late, atBound, unjournaled}, unbounded,
		"a zero sequence leaves the listing unbounded")

	candidates, err := f.store.ListPersonSweepHistoricalCandidates(t.Context(),
		peoplesweep.HistoricalCandidateRequest{PersonID: f.alicePersonID, Limit: 10,
			ThroughSequence: through})
	requirements.NoError(err)
	checks.Equal([]int64{atBound, unjournaled}, candidates)

	retrieved, err := f.store.SearchPersonSweepMessages(t.Context(), peoplesweep.ContextRequest{
		PersonID: f.alicePersonID, CandidateMessageIDs: []int64{late, atBound, unjournaled},
		SourceClasses: []peoplesweep.SourceClass{
			peoplesweep.SourceConversationText, peoplesweep.SourceMeetingText},
		Limit: 10, ThroughSequence: through,
	})
	requirements.NoError(err)
	checks.Equal([]int64{atBound, unjournaled}, sweepMessageIDs(retrieved),
		"hydration drops a candidate the journal saw after the bound even when named directly")

	listed, err := f.store.SearchPersonSweepMessages(t.Context(), peoplesweep.ContextRequest{
		PersonID: f.alicePersonID, Limit: 10, ThroughSequence: through,
	})
	requirements.NoError(err)
	checks.Equal([]int64{atBound, unjournaled}, sweepMessageIDs(listed),
		"a search that lists its own candidates carries the bound into the listing")

	hydrated, err := f.store.HydratePersonSweepMessages(t.Context(), f.alicePersonID,
		[]int64{atBound, unjournaled}, through)
	requirements.NoError(err)
	checks.Equal([]int64{atBound, unjournaled}, sweepMessageIDs(hydrated))

	_, err = f.store.HydratePersonSweepMessages(t.Context(), f.alicePersonID, []int64{late}, through)
	requirements.ErrorIs(err, peoplesweep.ErrPersonSweepMessageUnavailable,
		"the last-contact read refuses a message past the bound with the typed unavailable error")

	hydratedLate, err := f.store.HydratePersonSweepMessages(t.Context(), f.alicePersonID,
		[]int64{late}, 0)
	requirements.NoError(err)
	checks.Equal([]int64{late}, sweepMessageIDs(hydratedLate))
}

func TestPersonSweepConversationWithStoredDocumentEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	id := f.insertMessage(t, "historical-document", "email", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "Conversation evidence")
	input := sweepEvidenceInput(t, sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id))
	key, err := personfacts.EvidenceKey(input)
	require.NoError(err)
	persistSweepEvidence(t, f.store, key, input)
	ref := peoplesweep.EvidenceRef{SourceLane: peoplesweep.SourceDocumentText, SourceID: f.sourceID, MessageID: id, AttachmentID: 7, OccurrenceKey: "occurrence", ChunkKey: "chunk", SpanEnd: 4}
	payload, err := json.Marshal(ref, json.Deterministic(true))
	require.NoError(err)
	document := input
	document.SourceRef = "person-sweep/v1:" + base64.RawURLEncoding.EncodeToString(payload)
	documentKey, err := personfacts.EvidenceKey(document)
	require.NoError(err)
	persistSweepEvidence(t, f.store, documentKey, document)

	decoded, err := peoplesweep.DecodePersonSweepEvidenceRef(document.SourceRef)
	require.NoError(err)
	assert.Equal(ref, decoded)
	_, err = (store.PersonSweepEvidenceAligner{Store: f.store}).Align(t.Context(), document)
	require.ErrorIs(err, peoplesweep.ErrSourceTextUnavailable)
	changes, err := f.store.BuildPersonSweepEvidenceStatusChanges(t.Context(), f.alicePersonID, []peoplesweep.ArchiveChange{
		{PersonID: f.alicePersonID, SourceLane: peoplesweep.SourceConversationText, SourceID: f.sourceID, MessageID: id, EvidenceEffect: peoplesweep.EvidenceEffectSourceEdited},
	})
	require.NoError(err)
	assert.Equal([]personfacts.EvidenceStatusChange{{EvidenceKey: key, SourceVersion: input.SourceVersion, Reason: personfacts.EvidenceStatusSourceEdited}}, changes)

	after := latestPersonSweepSequence(t, f.store)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
	require.NoError(err)
	changes, err = f.store.BuildPersonSweepEvidenceStatusChanges(t.Context(), f.alicePersonID, personSweepChangesAfter(t, f.store, f.alicePersonID, after))
	require.NoError(err)
	unsupported := personfacts.EvidenceStatusChange{EvidenceKey: documentKey, SourceVersion: document.SourceVersion, Reason: personfacts.EvidenceStatusSourceDeleted}
	assert.Contains(changes, unsupported)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET deleted_from_source_at = NULL WHERE id = ?`), id)
	require.NoError(err)
	changes, err = f.store.BuildPersonSweepEvidenceStatusChanges(t.Context(), f.alicePersonID, personSweepChangesAfter(t, f.store, f.alicePersonID, after))
	require.NoError(err)
	assert.Contains(changes, unsupported)
	for _, change := range changes {
		if change.EvidenceKey == documentKey {
			assert.False(change.Supported)
		}
	}
}
