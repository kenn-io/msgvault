package store_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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
	f := newPersonSweepJournalFixture(t, true, false)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE sources SET source_type = 'slack' WHERE id = ?`), f.sourceID)
	require.NoError(t, err)
	id := f.insertMessage(t, "authored chat", "chat", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "I work in product.")
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id)
	require.NotNil(t, item.SubjectPersonID)
	assert.Equal(t, personfacts.DirectSelf, item.Directness)
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

func TestLoadPersonSweepWindowReturnsCanonicalDocumentTombstone(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f, personID := newPersonSweepDocumentFixture(t)
	_, err := f.Store.SetPersonTrackingContext(t.Context(), personID, true)
	requirements.NoError(err)
	seeds, err := f.Store.SearchPersonSweepDocuments(t.Context(), peoplesweep.DocumentContextRequest{
		PersonID: personID, Query: "synthetic", Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(seeds, 1)
	input := sweepEvidenceInput(t, seeds[0])
	key, err := personfacts.EvidenceKey(input)
	requirements.NoError(err)
	persistSweepEvidence(t, f.Store, key, input)
	priorInput := input
	priorInput.SourceVersion += ":prior"
	priorKey, err := personfacts.EvidenceKey(priorInput)
	requirements.NoError(err)
	persistSweepEvidence(t, f.Store, priorKey, priorInput)
	status, err := f.Store.BuildPersonSweepEvidenceStatusChanges(t.Context(), personID, []peoplesweep.ArchiveChange{{
		PersonID: personID, SourceLane: peoplesweep.SourceDocumentText,
		SourceID: seeds[0].Ref.SourceID, MessageID: seeds[0].Ref.MessageID,
		EvidenceEffect: peoplesweep.EvidenceEffectSourceDeleted,
	}})
	requirements.NoError(err)
	checks.Empty(status, "document lifecycle changes without canonical occurrence coordinates must not match")
	after := latestPersonSweepSequence(t, f.Store)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), seeds[0].Ref.MessageID)
	requirements.NoError(err)

	deleted, err := f.Store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: personID, Lane: peoplesweep.SourceDocumentText,
		AfterSequence: after, ThroughSequence: latestPersonSweepSequence(t, f.Store), Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(deleted.Seeds, 2)
	checks.ElementsMatch([]string{key, priorKey}, []string{
		deleted.Seeds[0].EvidenceKey, deleted.Seeds[1].EvidenceKey,
	})
	tombstone := sweepEvidenceItemByKey(t, deleted.Seeds, key)
	checks.True(tombstone.Tombstone)
	checks.Equal(key, tombstone.EvidenceKey)
	checks.Equal(input.SourceVersion, tombstone.SourceVersion)
	checks.Equal(seeds[0].Ref, tombstone.Ref)
	checks.NotZero(tombstone.Ref.AttachmentID)
	checks.NotEmpty(tombstone.Ref.OccurrenceKey)
	checks.NotEmpty(tombstone.Ref.ChunkKey)
	checks.Empty(tombstone.Excerpt)
	checks.Empty(tombstone.ContentSHA256)
	checks.NoError(peoplesweep.ValidatePersonSweepEvidenceItem(tombstone))
}

func TestDocumentOccurrenceRemovalDeactivatesStoredEvidence(t *testing.T) {
	tests := []struct {
		name         string
		update       string
		updateArgs   func(int64) []any
		wantEligible bool
		wantKind     peoplesweep.ChangeKind
		wantEffect   peoplesweep.EvidenceChangeEffect
		wantReason   personfacts.EvidenceStatusReason
	}{
		{
			name:         "ineligible occurrence",
			update:       `UPDATE attachments SET attachment_role = 'unknown' WHERE id = ?`,
			updateArgs:   func(id int64) []any { return []any{id} },
			wantEligible: false,
			wantKind:     peoplesweep.ChangeScope,
			wantEffect:   peoplesweep.EvidenceEffectScopeUnlinked,
			wantReason:   personfacts.EvidenceStatusScopeUnlinked,
		},
		{
			name:         "replaced occurrence",
			update:       `UPDATE attachments SET source_part_key = source_part_key || ':replacement' WHERE id = ?`,
			updateArgs:   func(id int64) []any { return []any{id} },
			wantEligible: true,
			wantKind:     peoplesweep.ChangeScope,
			wantEffect:   peoplesweep.EvidenceEffectScopeUnlinked,
			wantReason:   personfacts.EvidenceStatusScopeUnlinked,
		},
		{
			name:   "edited occurrence content",
			update: `UPDATE attachments SET content_hash = ?, storage_path = ? WHERE id = ?`,
			updateArgs: func(id int64) []any {
				hash := strings.Repeat("f", 64)
				return []any{hash, hash[:2] + "/" + hash, id}
			},
			wantEligible: true,
			wantKind:     peoplesweep.ChangeUpsert,
			wantEffect:   peoplesweep.EvidenceEffectSourceEdited,
			wantReason:   personfacts.EvidenceStatusSourceEdited,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f, personID := newPersonSweepDocumentFixture(t)
			_, err := f.Store.SetPersonTrackingContext(t.Context(), personID, true)
			requirements.NoError(err)
			seeds, err := f.Store.SearchPersonSweepDocuments(t.Context(), peoplesweep.DocumentContextRequest{
				PersonID: personID, Query: "synthetic", Limit: 10,
			})
			requirements.NoError(err)
			requirements.Len(seeds, 1)
			input := sweepEvidenceInput(t, seeds[0])
			key, err := personfacts.EvidenceKey(input)
			requirements.NoError(err)
			persistSweepEvidence(t, f.Store, key, input)

			before := latestPersonSweepSequence(t, f.Store)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(tt.update),
				tt.updateArgs(seeds[0].Ref.AttachmentID)...)
			requirements.NoError(err)
			_, eligible, err := f.Store.ReconcileDocumentOccurrence(
				t.Context(), seeds[0].Ref.AttachmentID, 2)
			requirements.NoError(err)
			checks.Equal(tt.wantEligible, eligible)

			changes := personSweepChangesAfter(t, f.Store, personID, before)
			var oldOccurrenceChanges []peoplesweep.ArchiveChange
			for _, change := range changes {
				if change.SourceLane == peoplesweep.SourceDocumentText &&
					change.AttachmentID == seeds[0].Ref.AttachmentID &&
					change.OccurrenceKey == seeds[0].Ref.OccurrenceKey {
					oldOccurrenceChanges = append(oldOccurrenceChanges, change)
				}
			}
			requirements.Len(oldOccurrenceChanges, 1)
			checks.Equal(tt.wantKind, oldOccurrenceChanges[0].Kind)
			checks.Equal(tt.wantEffect, oldOccurrenceChanges[0].EvidenceEffect)

			status, err := f.Store.BuildPersonSweepEvidenceStatusChanges(
				t.Context(), personID, changes)
			requirements.NoError(err)
			checks.Equal([]personfacts.EvidenceStatusChange{{
				EvidenceKey: key, SourceVersion: input.SourceVersion,
				Supported: false, Reason: tt.wantReason,
			}}, status)
		})
	}
}

func TestLoadPersonSweepWindowPagesOneDocumentAcrossChunkContinuation(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f, personID := newPersonSweepDocumentFixture(t)
	_, err := f.Store.SetPersonTrackingContext(t.Context(), personID, true)
	must.NoError(err)

	var extractionID string
	must.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT extraction_id FROM document_chunks ORDER BY ordinal LIMIT 1`)).Scan(&extractionID))
	for ordinal := 1; ordinal < 4; ordinal++ {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO document_chunks
				(extraction_id, chunk_key, ordinal, text, heading_path,
				 first_unit_index, last_unit_index, synthetic_prefix_len,
				 checksum, char_count, table_chunk, code_chunk, truncated)
			SELECT extraction_id, ?, ?, ?, heading_path,
			       first_unit_index, last_unit_index, synthetic_prefix_len,
			       ?, char_count, table_chunk, code_chunk, truncated
			FROM document_chunks WHERE extraction_id = ? AND ordinal = 0`),
			fmt.Sprintf("sweep-extra-%d", ordinal), ordinal,
			fmt.Sprintf("document chunk %d", ordinal), fmt.Sprintf("checksum-%d", ordinal), extractionID)
		must.NoError(err)
	}

	var change peoplesweep.ArchiveChange
	change.PersonID = personID
	change.SourceLane = peoplesweep.SourceDocumentText
	must.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT source_id, message_id, attachment_id, occurrence_key
		FROM document_occurrences LIMIT 1`)).Scan(
		&change.SourceID, &change.MessageID, &change.AttachmentID, &change.OccurrenceKey))
	var sequence int64
	must.NoError(f.Store.DB().QueryRowContext(t.Context(), `
		UPDATE person_sweep_change_clock SET sequence = sequence + 1
		WHERE singleton = TRUE RETURNING sequence`).Scan(&sequence))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_sweep_changes
			(sequence, person_id, source_lane, change_kind, evidence_effect,
			 source_id, message_id, attachment_id, occurrence_key, recorded_at)
		VALUES (?, ?, ?, 'upsert', '', ?, ?, ?, ?, ?)`), sequence, personID,
		peoplesweep.SourceDocumentText, change.SourceID, change.MessageID,
		change.AttachmentID, change.OccurrenceKey, time.Now().UTC())
	must.NoError(err)

	first, err := f.Store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: personID, Lane: peoplesweep.SourceDocumentText,
		Mode: peoplesweep.GenerationCursorOptimistic, AfterSequence: sequence - 1, Limit: 2,
	})
	must.NoError(err)
	must.Len(first.Seeds, 2)
	checks.Equal(sequence-1, first.NextSequence)
	checks.NotEmpty(first.NextDocumentKey)

	second, err := f.Store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: personID, Lane: peoplesweep.SourceDocumentText,
		Mode: peoplesweep.GenerationCursorOptimistic, AfterSequence: first.NextSequence,
		DocumentAfter: first.NextDocumentKey, Limit: 2,
	})
	must.NoError(err)
	must.Len(second.Seeds, 2)
	checks.Equal(sequence, second.NextSequence)
	checks.Empty(second.NextDocumentKey)

	chunkKeys := make([]string, 0, 4)
	for _, item := range append(first.Seeds, second.Seeds...) {
		chunkKeys = append(chunkKeys, item.Ref.ChunkKey)
	}
	checks.Len(chunkKeys, 4)
	checks.Len(map[string]struct{}{chunkKeys[0]: {}, chunkKeys[1]: {}, chunkKeys[2]: {}, chunkKeys[3]: {}}, 4)
}

func TestPersonSweepEvidenceAppliesPolicyDateAndSourceBounds(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	old := f.insertMessage(t, "old", "email", f.aliceID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	current := f.insertMessage(t, "current", "email", f.aliceID, time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, old, "needle old")
	addSweepBody(t, f, current, "needle current")
	got, err := f.store.SearchPersonSweepMessages(t.Context(), peoplesweep.ContextRequest{
		PersonID: f.alicePersonID, CandidateMessageIDs: []int64{old, current},
		Target:        personfacts.TargetDescriptor{Description: "needle"},
		SourceClasses: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:   "2026-01-01", SourceUntil: "2026-12-31", Limit: 10,
	})
	requirements.NoError(err)
	checks.Equal([]int64{current}, sweepMessageIDs(got))

	got, err = f.store.SearchPersonSweepMessages(t.Context(), peoplesweep.ContextRequest{
		PersonID: f.alicePersonID, CandidateMessageIDs: []int64{current},
		Target:        personfacts.TargetDescriptor{Description: "needle"},
		SourceClasses: []peoplesweep.SourceClass{peoplesweep.SourceDocumentText}, Limit: 10,
	})
	requirements.NoError(err)
	checks.Empty(got)
}

func TestPersonSweepEvidenceAlignsDocumentChunkSpans(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f, personID := newPersonSweepDocumentFixture(t)
	var attachmentID int64
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(),
		`SELECT attachment_id FROM document_occurrences LIMIT 1`).Scan(&attachmentID))
	window, err := f.Store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: personID, Lane: peoplesweep.SourceDocumentText,
		ReconcileUpper: fmt.Sprintf("%020d", attachmentID), Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(window.Seeds, 1)
	checks.Equal("alpha synthetic role evidence", window.Seeds[0].Excerpt)
	checks.True(window.ReconciliationDone)
	items, err := f.Store.SearchPersonSweepDocuments(t.Context(), peoplesweep.DocumentContextRequest{
		PersonID: personID, Query: "synthetic", Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(items, 1)
	item := items[0]
	checks.Equal(item.Highlight.Start, item.Ref.SpanStart)
	checks.Equal(item.Highlight.End, item.Ref.SpanEnd)
	checks.Equal("chunk-0", item.Ref.ChunkKey)
	checks.NotEmpty(item.Ref.OccurrenceKey)
	result, err := (store.PersonSweepEvidenceAligner{Store: f.Store}).Align(t.Context(), sweepEvidenceInput(t, item))
	requirements.NoError(err)
	checks.True(result.Accepted)
}

func TestSearchPersonSweepDocumentsPreservesCoordinates(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f, personID := newPersonSweepDocumentFixture(t)
	got, err := f.Store.SearchPersonSweepDocuments(t.Context(), peoplesweep.DocumentContextRequest{
		PersonID: personID, Query: "synthetic", Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(got, 1)
	checks.Equal(peoplesweep.SourceDocumentText, got[0].Ref.SourceLane)
	checks.NotZero(got[0].Ref.AttachmentID)
	checks.NotEmpty(got[0].Ref.OccurrenceKey)
	checks.Equal("chunk-0", got[0].Ref.ChunkKey)
	checks.Equal(peoplesweep.TextSpan{Start: 6, End: 15}, got[0].Highlight)
	checks.Equal([]personscope.Role{personscope.RoleFrom}, got[0].Provenance.Roles)
	checks.False(got[0].EventTime.IsZero())
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

func newPersonSweepDocumentFixture(t *testing.T) (*storetest.Fixture, int64) {
	t.Helper()
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "alpha synthetic role evidence", "person-sweep-document")
	participantID := f.EnsureParticipant("document-person@example.test", "Document Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	var messageID int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT message_id FROM document_occurrences WHERE canonical_blob_hash = ?`), hash).Scan(&messageID))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE attachments SET media_type = 'document' WHERE message_id = ?`), messageID)
	require.NoError(t, err)
	when := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE messages SET sender_id = ?, sent_at = ? WHERE id = ?`), participantID, when, messageID)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE sources SET source_type = 'slack' WHERE id = ?`), f.Source.ID)
	require.NoError(t, err)
	return f, person.ID
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

func sweepEvidenceItemByKey(t *testing.T, items []peoplesweep.EvidenceItem, key string) peoplesweep.EvidenceItem {
	t.Helper()
	for _, item := range items {
		if item.EvidenceKey == key {
			return item
		}
	}
	require.FailNow(t, "missing sweep evidence item", "evidence key %s", key)
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
