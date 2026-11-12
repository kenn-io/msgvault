package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXRemovalSuppressesEveryAliasAndNewerIntersectingVersion(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	post := xPostInput(source, "102", "100", "removed constellation secret", "100", "102")
	saved, err := st.PersistXPostContext(t.Context(), post)
	_testRequire.NoError(err)
	control, err := st.LoadXControlContext(t.Context(), source)
	_testRequire.NoError(err)
	hold := store.XModelErrorResourceShape
	_, err = st.UpdateXControlContext(t.Context(), source, control.Revision, store.XControlPatch{OwnerID: "1001", ModelError: &hold, InitializeInstance: true})
	_testRequire.NoError(err)
	before, err := st.LoadXControlContext(t.Context(), source)
	_testRequire.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	work, err := st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	_testRequire.NoError(err)
	_testAssert.Equal([]int64{saved.MessageID}, work.MessageIDs)
	_testAssert.True(work.Pending)
	body, err := st.GetMessageBodyText(t.Context(), saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Empty(body)
	raw, err := st.GetMessageRaw(saved.MessageID)
	_testRequire.ErrorIs(err, sql.ErrNoRows)
	_testAssert.Empty(raw)
	var snippet, metadata sql.NullString
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT snippet,metadata FROM messages WHERE id=?`), saved.MessageID).Scan(&snippet, &metadata))
	_testAssert.Empty(snippet.String)
	_testAssert.NotContains(metadata.String, "constellation")
	_, count, err := st.SearchMessages("constellation", 0, 20)
	_testRequire.NoError(err)
	_testAssert.Zero(count)
	_testRequire.Error(st.FinishXRemovalContext(t.Context(), source, work))
	later := xPostInput(source, "104", "100", "must never return", "100", "102", "104")
	result, err := st.PersistXPostContext(t.Context(), later)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), source, "104")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
	work.DerivedComplete = true
	_testRequire.NoError(st.FinishXRemovalContext(t.Context(), source, work))
	pending, err := st.PendingXRemovalsContext(t.Context(), source)
	_testRequire.NoError(err)
	_testAssert.Empty(pending)
	// Completed root record producer must see every suppressed version only
	// after the Store and derived cleanup completion boundary has cleared.
	record, _, err := st.ExportXRemovalRecordContext(t.Context())
	_testRequire.NoError(err)
	_testRequire.Len(record.Sources, 1)
	_testAssert.Equal([]string{"100", "102", "104"}, record.Sources[0].RemovedIDs)
	_testAssert.Equal(before.InstanceID, record.Sources[0].InstanceID)
	after, err := st.LoadXControlContext(t.Context(), source)
	_testRequire.NoError(err)
	_testAssert.Equal(before, after)
	body, err = st.GetMessageBodyText(t.Context(), saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Empty(body)
}

func TestXRemovalUnknownPostPersistsIDOnlySuppression(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	work, err := st.RemoveXPostContentContext(t.Context(), source, "900", "removed")
	_testRequire.NoError(err)
	_testAssert.Empty(work.MessageIDs)
	result, err := st.PersistXPostContext(t.Context(), xPostInput(source, "902", "900", "unseen removed secret", "900", "902"))
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), source, "902")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
}

func TestXRemovalSuppressionFollowsIntersectingShorterEditHistory(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "old removed payload", "100", "102"))
	_testRequire.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	_testRequire.NoError(err)
	next := xPostInput(source, "104", "100", "new leaked payload", "102", "104")
	result, err := st.PersistXPostContext(t.Context(), next)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)
	next.EditHistory = []string{"104"}
	result, err = st.PersistXPostContext(t.Context(), next)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), source, "104")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
}

func TestXRemovalPreservesIndependentAuthorSupport(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "removed payload"))
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	independently := xPostInput(other.ID, "300", "300", "independent payload")
	independently.OwnerID = "2001"
	kept, err := st.PersistXPostContext(t.Context(), independently)
	_testRequire.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	_testRequire.NoError(err)
	text, err := st.GetMessageBodyText(t.Context(), kept.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("independent payload", text)
	var name sql.NullString
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), `SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='x_user_id' AND pi.identifier_value='1002')`).Scan(&name))
	_testAssert.Equal("Example Author", name.String)
}

func TestXRemovalErasesProfileAfterLastIndependentSourceIsRemoved(t *testing.T) {
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "first source content"))
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	post := xPostInput(other.ID, "300", "300", "second source content")
	post.OwnerID = "2001"
	_, err = st.PersistXPostContext(t.Context(), post)
	_testRequire.NoError(err)
	for _, item := range []struct {
		source int64
		id     string
	}{{source, "100"}, {other.ID, "300"}} {
		execution, err := st.AcquireSyncExecutionContext(t.Context(), item.source)
		_testRequire.NoError(err)
		_, err = st.RemoveXPostContentContext(t.Context(), item.source, item.id, "removed")
		_testRequire.NoError(err)
		_testRequire.NoError(execution.Release())
	}
	var name sql.NullString
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), `SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='x_user_id' AND pi.identifier_value='1002')`).Scan(&name))
	assert.Empty(t, name.String, "removed sources and numeric identity anchors do not support a display label")
}

func TestXRemovalPreservesOwnerDraftReceiptAndCuratedPerson(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	saved, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "provider text removed", "100", "102"))
	_testRequire.NoError(err)
	var pid, conversation int64
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id,conversation_id FROM messages WHERE id=?`), saved.MessageID).Scan(&pid, &conversation))
	person, _, err := st.CreatePersonFromParticipant(pid)
	_testRequire.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO chat_drafts(draft_id,source_id,conversation_id,source_conversation_id,conversation_type,reply_to_source_message_id,body,revision) VALUES(?,?,?,?,?,?,?,?)`), "owner-local-draft", source, conversation, "100", "channel", "102", "owner handwritten reply", 7)
	_testRequire.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO x_reply_attempts(draft_id,attempt_id,source_id,owner_id,credential_generation,draft_revision,target_post_id,target_canonical_id,target_message_id,source_conversation_id,conversation_id,body_digest,state,remote_id,reservation_id,reserved_micros) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), "owner-local-draft", "attempt-example", source, "1001", int64(2), 7, "102", "100", saved.MessageID, "100", conversation, "content-free-digest", "remote_accepted", "999", "reservation-example", 123)
	_testRequire.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	_testRequire.NoError(err)
	var body, state, remoteID, name string
	var revision, amount int64
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body,revision FROM chat_drafts WHERE draft_id=?`), "owner-local-draft").Scan(&body, &revision))
	_testAssert.Equal("owner handwritten reply", body)
	_testAssert.Equal(int64(7), revision)
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT state,remote_id,reserved_micros FROM x_reply_attempts WHERE draft_id=? AND attempt_id=?`), "owner-local-draft", "attempt-example").Scan(&state, &remoteID, &amount))
	_testAssert.Equal("remote_accepted", state)
	_testAssert.Equal("999", remoteID)
	_testAssert.Equal(int64(123), amount)
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name FROM persons WHERE id=?`), person.ID).Scan(&name))
	_testAssert.Equal("Example Author", name)
	target, err := st.ResolveXPostContext(t.Context(), source, "102")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
}

func TestXRemovalDifferentIndependentNameDoesNotSupportRemovedLabel(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "removed provider content"))
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	post := xPostInput(other.ID, "300", "300", "independent provider content")
	post.OwnerID = "2001"
	post.Author.DisplayName = "Independent Label"
	saved, err := st.PersistXPostContext(t.Context(), post)
	_testRequire.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	_testRequire.NoError(err)
	var name sql.NullString
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), `SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='x_user_id' AND pi.identifier_value='1002')`).Scan(&name))
	_testAssert.Empty(name.String)
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'`), saved.MessageID).Scan(&name))
	_testAssert.Equal("Independent Label", name.String)
}

func TestMarkXRemovalPendingPreservesContentAndRecovery(t *testing.T) {
	needs := require.New(t)
	checks := assert.New(t)
	st, source := xPostFixture(t)
	saved, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "retained until cleanup", "100", "102"))
	needs.NoError(err)
	_, err = st.PersistXPostContext(t.Context(), xPostInput(source, "200", "200", "other post"))
	needs.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,cleanup_pending=1,suppression_reason='deleted' WHERE source_id=? AND canonical_id='200'`), source)
	needs.NoError(err)
	var observation int64
	needs.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM participant_contact_observations WHERE source_id=? AND address_kind='provider_identity' AND source_resource_uid='x:author:1002'`), source).Scan(&observation))
	posts := []store.XRemovalWork{{SourceID: source, CanonicalID: "100", Reason: "withheld"}, {SourceID: source, CanonicalID: "200", Reason: "removed"}, {SourceID: source, CanonicalID: "999", Reason: "removed"}}
	profile := store.XAuthorProfileRemovalWork{SourceID: source, ObservationID: observation, AuthorID: "1002"}
	needs.NoError(st.MarkXRemovalPendingContext(t.Context(), posts, []store.XAuthorProfileRemovalWork{profile}))
	needs.NoError(st.MarkXRemovalPendingContext(t.Context(), posts, []store.XAuthorProfileRemovalWork{profile}))
	var tokens string
	needs.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT type_tokens FROM participant_contact_observations WHERE id=?`), observation).Scan(&tokens))
	var pendingTokens int
	for token := range strings.SplitSeq(tokens, ",") {
		if token == "x_author_profile_cleanup_pending" {
			pendingTokens++
		}
	}
	checks.Equal(1, pendingTokens)
	var pending int
	var suppressed sql.NullTime
	var reason string
	needs.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT cleanup_pending,suppressed_at,suppression_reason FROM x_post_versions WHERE source_id=? AND post_id='102'`), source).Scan(&pending, &suppressed, &reason))
	checks.Equal(1, pending)
	checks.False(suppressed.Valid)
	checks.Equal("withheld", reason)
	body, err := st.GetMessageBodyText(t.Context(), saved.MessageID)
	needs.NoError(err)
	checks.Equal("retained until cleanup", body)
	raw, err := st.GetMessageRaw(saved.MessageID)
	needs.NoError(err)
	checks.NotEmpty(raw)
	var existingReason string
	needs.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT suppression_reason FROM x_post_versions WHERE source_id=? AND post_id='200'`), source).Scan(&existingReason))
	checks.Equal("deleted", existingReason)
	var unknown int
	needs.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM x_post_versions WHERE source_id=? AND post_id='999'`), source).Scan(&unknown))
	checks.Zero(unknown)
	works, err := st.PendingXRemovalsContext(t.Context(), source)
	needs.NoError(err)
	needs.Len(works, 2)
	checks.Equal("withheld", works[0].Reason)
	profiles, err := st.PendingXAuthorProfileRemovalsContext(t.Context())
	needs.NoError(err)
	needs.Len(profiles, 1)
	profile.DerivedComplete = true
	needs.NoError(st.FinishXAuthorProfileRemovalContext(t.Context(), profile))
	profiles, err = st.PendingXAuthorProfileRemovalsContext(t.Context())
	needs.NoError(err)
	checks.Empty(profiles)
	blocked, err := st.XCleanupPendingContext(t.Context())
	needs.NoError(err)
	checks.True(blocked)
	scoped := st.ScopedToSync(source+1, 1)
	needs.Error(scoped.MarkXRemovalPendingContext(t.Context(), posts, nil))
}

func TestMarkXRemovalPendingReplacesCompletedReasonForRecovery(t *testing.T) {
	needs := require.New(t)
	checks := assert.New(t)
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "synthetic post"))
	needs.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	needs.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	work, err := st.RemoveXPostContentContext(t.Context(), source, "100", "deleted")
	needs.NoError(err)
	work.DerivedComplete = true
	needs.NoError(st.FinishXRemovalContext(t.Context(), source, work))
	needs.NoError(st.MarkXRemovalPendingContext(t.Context(), []store.XRemovalWork{{SourceID: source, CanonicalID: "100", Reason: "removed"}}, nil))
	pending, err := st.PendingXRemovalsContext(t.Context(), source)
	needs.NoError(err)
	needs.Len(pending, 1)
	checks.Equal("removed", pending[0].Reason)
	// Resume replays the reason durably admitted before waiting for cleanup.
	work, err = st.RemoveXPostContentContext(t.Context(), source, pending[0].CanonicalID, pending[0].Reason)
	needs.NoError(err)
	work.DerivedComplete = true
	needs.NoError(st.FinishXRemovalContext(t.Context(), source, work))
	target, err := st.ResolveXPostContext(t.Context(), source, "100")
	needs.NoError(err)
	checks.Equal("removed", target.Availability)
}
