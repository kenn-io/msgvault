package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXRemovalSuppressesEveryAliasAndNewerIntersectingVersion(t *testing.T) {
	st, source := xPostFixture(t)
	post := xPostInput(source, "102", "100", "removed constellation secret", "100", "102")
	saved, err := st.PersistXPostContext(t.Context(), post)
	require.NoError(t, err)
	control, err := st.LoadXControlContext(t.Context(), source)
	require.NoError(t, err)
	hold := store.XModelErrorResourceShape
	_, err = st.UpdateXControlContext(t.Context(), source, control.Revision, store.XControlPatch{OwnerID: "1001", ModelError: &hold, InitializeInstance: true})
	require.NoError(t, err)
	before, err := st.LoadXControlContext(t.Context(), source)
	require.NoError(t, err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	work, err := st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	require.NoError(t, err)
	assert.Equal(t, []int64{saved.MessageID}, work.MessageIDs)
	assert.True(t, work.Pending)
	body, err := st.GetMessageBodyText(saved.MessageID)
	require.NoError(t, err)
	assert.Empty(t, body)
	raw, err := st.GetMessageRaw(saved.MessageID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	assert.Empty(t, raw)
	var snippet, metadata sql.NullString
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT snippet,metadata FROM messages WHERE id=?`), saved.MessageID).Scan(&snippet, &metadata))
	assert.Empty(t, snippet.String)
	assert.NotContains(t, metadata.String, "constellation")
	_, count, err := st.SearchMessages("constellation", 0, 20)
	require.NoError(t, err)
	assert.Zero(t, count)
	require.Error(t, st.FinishXRemovalContext(t.Context(), source, work))
	later := xPostInput(source, "104", "100", "must never return", "100", "102", "104")
	result, err := st.PersistXPostContext(t.Context(), later)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), source, "104")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
	work.DerivedComplete = true
	require.NoError(t, st.FinishXRemovalContext(t.Context(), source, work))
	pending, err := st.PendingXRemovalsContext(t.Context(), source)
	require.NoError(t, err)
	assert.Empty(t, pending)
	// Completed root record producer must see every suppressed version only
	// after the Store and derived cleanup completion boundary has cleared.
	record, _, err := st.ExportXRemovalRecordContext(t.Context())
	require.NoError(t, err)
	require.Len(t, record.Sources, 1)
	assert.Equal(t, []string{"100", "102", "104"}, record.Sources[0].RemovedIDs)
	assert.Equal(t, before.InstanceID, record.Sources[0].InstanceID)
	after, err := st.LoadXControlContext(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	body, err = st.GetMessageBodyText(saved.MessageID)
	require.NoError(t, err)
	assert.Empty(t, body)
}

func TestXRemovalUnknownPostPersistsIDOnlySuppression(t *testing.T) {
	st, source := xPostFixture(t)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	work, err := st.RemoveXPostContentContext(t.Context(), source, "900", "removed")
	require.NoError(t, err)
	assert.Empty(t, work.MessageIDs)
	result, err := st.PersistXPostContext(t.Context(), xPostInput(source, "902", "900", "unseen removed secret", "900", "902"))
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), source, "902")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
}

func TestXRemovalSuppressionFollowsIntersectingShorterEditHistory(t *testing.T) {
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "old removed payload", "100", "102"))
	require.NoError(t, err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	require.NoError(t, err)
	next := xPostInput(source, "104", "100", "new leaked payload", "102", "104")
	result, err := st.PersistXPostContext(t.Context(), next)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	next.EditHistory = []string{"104"}
	result, err = st.PersistXPostContext(t.Context(), next)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), source, "104")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
}

func TestXRemovalPreservesIndependentAuthorSupport(t *testing.T) {
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "removed payload"))
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	independently := xPostInput(other.ID, "300", "300", "independent payload")
	independently.OwnerID = "2001"
	kept, err := st.PersistXPostContext(t.Context(), independently)
	require.NoError(t, err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	require.NoError(t, err)
	text, err := st.GetMessageBodyText(kept.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "independent payload", text)
	var name sql.NullString
	require.NoError(t, st.DB().QueryRow(`SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='x_user_id' AND pi.identifier_value='1002')`).Scan(&name))
	assert.Equal(t, "Example Author", name.String)
}

func TestXRemovalErasesProfileAfterLastIndependentSourceIsRemoved(t *testing.T) {
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "first source content"))
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	post := xPostInput(other.ID, "300", "300", "second source content")
	post.OwnerID = "2001"
	_, err = st.PersistXPostContext(t.Context(), post)
	require.NoError(t, err)
	for _, item := range []struct {
		source int64
		id     string
	}{{source, "100"}, {other.ID, "300"}} {
		execution, err := st.AcquireSyncExecutionContext(t.Context(), item.source)
		require.NoError(t, err)
		_, err = st.RemoveXPostContentContext(t.Context(), item.source, item.id, "removed")
		require.NoError(t, err)
		require.NoError(t, execution.Release())
	}
	var name sql.NullString
	require.NoError(t, st.DB().QueryRow(`SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='x_user_id' AND pi.identifier_value='1002')`).Scan(&name))
	assert.Empty(t, name.String, "removed sources and numeric identity anchors do not support a display label")
}

func TestXRemovalPreservesOwnerDraftReceiptAndCuratedPerson(t *testing.T) {
	st, source := xPostFixture(t)
	saved, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "provider text removed", "100", "102"))
	require.NoError(t, err)
	var pid, conversation int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT sender_id,conversation_id FROM messages WHERE id=?`), saved.MessageID).Scan(&pid, &conversation))
	person, _, err := st.CreatePersonFromParticipant(pid)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO chat_drafts(draft_id,source_id,conversation_id,source_conversation_id,conversation_type,reply_to_source_message_id,body,revision) VALUES(?,?,?,?,?,?,?,?)`), "owner-local-draft", source, conversation, "100", "channel", "102", "owner handwritten reply", 7)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO x_reply_attempts(draft_id,attempt_id,source_id,owner_id,credential_generation,draft_revision,target_post_id,target_canonical_id,target_message_id,source_conversation_id,conversation_id,body_digest,state,remote_id,reservation_id,reserved_micros) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), "owner-local-draft", "attempt-example", source, "1001", int64(2), 7, "102", "100", saved.MessageID, "100", conversation, "content-free-digest", "remote_accepted", "999", "reservation-example", 123)
	require.NoError(t, err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	require.NoError(t, err)
	var body, state, remoteID, name string
	var revision, amount int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT body,revision FROM chat_drafts WHERE draft_id=?`), "owner-local-draft").Scan(&body, &revision))
	assert.Equal(t, "owner handwritten reply", body)
	assert.Equal(t, int64(7), revision)
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT state,remote_id,reserved_micros FROM x_reply_attempts WHERE draft_id=? AND attempt_id=?`), "owner-local-draft", "attempt-example").Scan(&state, &remoteID, &amount))
	assert.Equal(t, "remote_accepted", state)
	assert.Equal(t, "999", remoteID)
	assert.Equal(t, int64(123), amount)
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT display_name FROM persons WHERE id=?`), person.ID).Scan(&name))
	assert.Equal(t, "Example Author", name)
	target, err := st.ResolveXPostContext(t.Context(), source, "102")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
}

func TestXRemovalDifferentIndependentNameDoesNotSupportRemovedLabel(t *testing.T) {
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "removed provider content"))
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	post := xPostInput(other.ID, "300", "300", "independent provider content")
	post.OwnerID = "2001"
	post.Author.DisplayName = "Independent Label"
	saved, err := st.PersistXPostContext(t.Context(), post)
	require.NoError(t, err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	require.NoError(t, err)
	var name sql.NullString
	require.NoError(t, st.DB().QueryRow(`SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='x_user_id' AND pi.identifier_value='1002')`).Scan(&name))
	assert.Empty(t, name.String)
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'`), saved.MessageID).Scan(&name))
	assert.Equal(t, "Independent Label", name.String)
}
