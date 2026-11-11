package store_test

import (
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

var xPostTime = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func xPostFixture(t *testing.T) (*store.Store, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	return st, source.ID
}

func xPostInput(source int64, id, conversation, text string, history ...string) store.XPersistPostInput {
	if len(history) == 0 {
		history = []string{id}
	}
	return store.XPersistPostInput{SourceID: source, OwnerID: "1001", PostID: id, AuthorID: "1002", ConversationID: conversation, EditHistory: history, Text: text, CreatedAt: xPostTime, FetchedAt: xPostTime.Add(time.Hour), Availability: "public", Raw: []byte(`{"post":{"id":"` + id + `"}}`), Author: &store.XAuthorInput{SourceID: source, UserID: "1002", Username: "example_author", DisplayName: "Example Author", FetchedAt: xPostTime.Add(time.Hour)}}
}

func TestXOldEditCannotRollbackAtomicProjection(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	newer := xPostInput(source, "102", "100", "current constellation", "100", "102")
	newer.CreatedAt = xPostTime.Add(time.Minute)
	newer.MentionIDs = []string{"1003"}
	first, err := st.PersistXPostContext(t.Context(), newer)
	_testRequire.NoError(err)
	_testAssert.True(first.Added)
	older := xPostInput(source, "100", "100", "obsolete telescope", "100")
	older.FetchedAt = xPostTime
	older.Author.FetchedAt = xPostTime
	older.Author.DisplayName = "Stale Author"
	older.MentionIDs = []string{"1004"}
	old, err := st.PersistXPostContext(t.Context(), older)
	_testRequire.NoError(err)
	_testAssert.Equal(first.MessageID, old.MessageID)
	target, err := st.ResolveXPostContext(t.Context(), source, "100")
	_testRequire.NoError(err)
	_testAssert.Equal("100", target.CanonicalID)
	_testAssert.Equal("102", target.CurrentID)
	_testAssert.Equal(first.MessageID, target.MessageID)
	body, err := st.GetMessageBodyText(first.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("current constellation", body)
	raw, err := st.GetMessageRaw(first.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal(newer.Raw, raw)
	var snippet, authorName, toID string
	var sent sql.NullTime
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT snippet,sent_at FROM messages WHERE id=?"), first.MessageID).Scan(&snippet, &sent))
	_testAssert.Equal("current constellation", snippet)
	_testAssert.WithinDuration(xPostTime, sent.Time, time.Microsecond)
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'"), first.MessageID).Scan(&authorName))
	_testAssert.Equal("Example Author", authorName)
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT pi.identifier_value FROM message_recipients mr JOIN participant_identifiers pi ON pi.participant_id=mr.participant_id WHERE mr.message_id=? AND mr.recipient_type='to' AND pi.identifier_type='x_user_id'"), first.MessageID).Scan(&toID))
	_testAssert.Equal("1003", toID)
	hits, count, err := st.SearchMessages("constellation", 0, 10)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(1), count)
	_testRequire.Len(hits, 1)
	_testAssert.Equal(first.MessageID, hits[0].ID)
	_, count, err = st.SearchMessages("obsolete", 0, 10)
	_testRequire.NoError(err)
	_testAssert.Zero(count)
	// Reject a splice joining a known post to a conflicting canonical chain.
	conflict := xPostInput(source, "104", "100", "conflict", "99", "102", "104")
	_, err = st.PersistXPostContext(t.Context(), conflict)
	_testRequire.Error(err)
	_, err = st.ResolveXPostContext(t.Context(), source, "104")
	_testRequire.Error(err)
}

func TestXPostProjectionRollsBackEveryDependent(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	seed := xPostInput(source, "100", "100", "original")
	saved, err := st.PersistXPostContext(t.Context(), seed)
	_testRequire.NoError(err)
	_testRequire.NoError(st.EnableEmbeddingChangeJournal(t.Context()))
	var journalBefore, observationsBefore int
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM embedding_changes").Scan(&journalBefore))
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM participant_contact_observations").Scan(&observationsBefore))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_x_raw() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic X raw failure'; END $$; CREATE TRIGGER reject_x_raw BEFORE INSERT OR UPDATE ON message_raw FOR EACH ROW EXECUTE FUNCTION reject_x_raw()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_x_raw BEFORE INSERT ON message_raw BEGIN SELECT RAISE(ABORT,'synthetic X raw failure'); END`)
	}
	_testRequire.NoError(err)
	change := xPostInput(source, "102", "100", "must roll back", "100", "102")
	change.Author.Username = "renamed_author"
	change.FetchedAt = change.FetchedAt.Add(time.Hour)
	change.Author.FetchedAt = change.FetchedAt
	change.MentionIDs = []string{"1003"}
	_, err = st.PersistXPostContext(t.Context(), change)
	_testRequire.ErrorContains(err, "synthetic X raw failure")
	body, err := st.GetMessageBodyText(saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("original", body)
	raw, err := st.GetMessageRaw(saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal(seed.Raw, raw)
	var aliases, journalAfter, observationsAfter, mentions int
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM x_post_versions").Scan(&aliases))
	_testAssert.Equal(1, aliases)
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM embedding_changes").Scan(&journalAfter))
	_testAssert.Equal(journalBefore, journalAfter)
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM participant_contact_observations").Scan(&observationsAfter))
	_testAssert.Equal(observationsBefore, observationsAfter)
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM message_recipients WHERE recipient_type='to'").Scan(&mentions))
	_testAssert.Zero(mentions)
}

func TestXSameVersionEnrichmentRequiresFreshEvidence(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	incomplete := xPostInput(source, "100", "100", "short preview")
	incomplete.TextIncomplete = true
	incomplete.Availability = "unverified"
	incomplete.Author = nil
	result, err := st.PersistXPostContext(t.Context(), incomplete)
	_testRequire.NoError(err)
	richer := xPostInput(source, "100", "100", "complete constellation text")
	richer.FetchedAt = incomplete.FetchedAt.Add(-time.Second)
	richer.Author.FetchedAt = richer.FetchedAt
	_, err = st.PersistXPostContext(t.Context(), richer)
	_testRequire.NoError(err)
	body, err := st.GetMessageBodyText(result.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("short preview", body)
	richer.FetchedAt = incomplete.FetchedAt.Add(time.Second)
	richer.Author.FetchedAt = richer.FetchedAt
	enriched, err := st.PersistXPostContext(t.Context(), richer)
	_testRequire.NoError(err)
	_testAssert.True(enriched.Updated)
	body, err = st.GetMessageBodyText(result.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("complete constellation text", body)
	incomplete.FetchedAt = richer.FetchedAt.Add(time.Hour)
	_, err = st.PersistXPostContext(t.Context(), incomplete)
	_testRequire.NoError(err)
	body, err = st.GetMessageBodyText(result.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("complete constellation text", body)
	raw, err := st.GetMessageRaw(result.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal(richer.Raw, raw)
}

func TestXLateRootConsolidationKeepsMessageIDs(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	left := xPostInput(source, "200", "100", "first reply")
	left.ReplyToPostID = "100"
	right := xPostInput(source, "201", "102", "second reply")
	right.ReplyToPostID = "102"
	l, err := st.PersistXPostContext(t.Context(), left)
	_testRequire.NoError(err)
	r, err := st.PersistXPostContext(t.Context(), right)
	_testRequire.NoError(err)
	lt, err := st.ResolveXPostContext(t.Context(), source, "200")
	_testRequire.NoError(err)
	rt, err := st.ResolveXPostContext(t.Context(), source, "201")
	_testRequire.NoError(err)
	_testRequire.NotEqual(lt.ConversationID, rt.ConversationID)
	for _, draft := range []struct {
		id, native, parent string
		conversation       int64
	}{{"left-draft", "100", "200", lt.ConversationID}, {"right-draft", "102", "201", rt.ConversationID}} {
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO chat_drafts(draft_id,source_id,conversation_id,source_conversation_id,conversation_type,reply_to_source_message_id,body,revision) VALUES(?,?,?,?,?,?,?,?)`), draft.id, source, draft.conversation, draft.native, "channel", draft.parent, "reviewed synthetic reply", 7)
		_testRequire.NoError(err)
	}
	root := xPostInput(source, "102", "102", "edited root", "100", "102")
	root.AuthorID = "1001"
	root.Author.UserID = "1001"
	root.Author.Username = "example_owner"
	rootResult, err := st.PersistXPostContext(t.Context(), root)
	_testRequire.NoError(err)
	_, err = st.ResolveXParentsContext(t.Context(), source, 0, 100)
	_testRequire.NoError(err)
	for _, item := range []struct {
		native string
		id     int64
	}{{"200", l.MessageID}, {"201", r.MessageID}} {
		target, err := st.ResolveXPostContext(t.Context(), source, item.native)
		_testRequire.NoError(err)
		_testAssert.Equal(item.id, target.MessageID)
		_testAssert.Equal(lt.ConversationID, target.ConversationID)
		var parent sql.NullInt64
		_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), item.id).Scan(&parent))
		_testRequire.True(parent.Valid)
		_testAssert.Equal(rootResult.MessageID, parent.Int64)
	}
	var conversations, messages int
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM conversations WHERE source_id=?"), source).Scan(&conversations))
	_testAssert.Equal(1, conversations)
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT message_count FROM conversations WHERE id=?"), lt.ConversationID).Scan(&messages))
	_testAssert.Equal(3, messages)
	for _, draft := range []struct{ id, native, parent string }{{"left-draft", "100", "200"}, {"right-draft", "102", "201"}} {
		var conversation, revision int64
		var native, parent, body string
		_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT conversation_id,source_conversation_id,reply_to_source_message_id,body,revision FROM chat_drafts WHERE draft_id=?"), draft.id).Scan(&conversation, &native, &parent, &body, &revision))
		_testAssert.Equal(lt.ConversationID, conversation)
		_testAssert.Equal(draft.native, native)
		_testAssert.Equal(draft.parent, parent)
		_testAssert.Equal("reviewed synthetic reply", body)
		_testAssert.Equal(int64(7), revision)
	}
}

func TestXParentResolvesAfterMonths(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	child := xPostInput(source, "200", "100", "delayed child")
	child.ReplyToPostID = "100"
	result, err := st.PersistXPostContext(t.Context(), child)
	_testRequire.NoError(err)
	next, err := st.ResolveXParentsContext(t.Context(), source, 0, 1)
	_testRequire.NoError(err)
	_testAssert.Equal(result.MessageID, next)
	var parent sql.NullInt64
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), result.MessageID).Scan(&parent))
	_testAssert.False(parent.Valid)
	root := xPostInput(source, "102", "100", "root months later", "100", "102")
	root.FetchedAt = xPostTime.AddDate(0, 4, 0)
	root.Author.FetchedAt = root.FetchedAt
	saved, err := st.PersistXPostContext(t.Context(), root)
	_testRequire.NoError(err)
	_, err = st.ResolveXParentsContext(t.Context(), source, 0, 1)
	_testRequire.NoError(err)
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), result.MessageID).Scan(&parent))
	_testRequire.True(parent.Valid)
	_testAssert.Equal(saved.MessageID, parent.Int64)
}

func TestXPostRejectsCyclesAndForeignParents(t *testing.T) {
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	self := xPostInput(source, "100", "100", "self")
	self.ReplyToPostID = "100"
	_, err := st.PersistXPostContext(t.Context(), self)
	_testRequire.Error(err)
	a := xPostInput(source, "100", "100", "first")
	a.ReplyToPostID = "200"
	_, err = st.PersistXPostContext(t.Context(), a)
	_testRequire.NoError(err)
	b := xPostInput(source, "200", "100", "second")
	b.ReplyToPostID = "100"
	_, err = st.PersistXPostContext(t.Context(), b)
	_testRequire.Error(err)
	foreign, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	p := xPostInput(foreign.ID, "300", "300", "foreign")
	p.OwnerID = "2001"
	_, err = st.PersistXPostContext(t.Context(), p)
	_testRequire.NoError(err)
	c := xPostInput(source, "400", "300", "own child")
	c.ReplyToPostID = "300"
	child, err := st.PersistXPostContext(t.Context(), c)
	_testRequire.NoError(err)
	_, err = st.ResolveXParentsContext(t.Context(), source, 0, 100)
	_testRequire.NoError(err)
	var linked sql.NullInt64
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), child.MessageID).Scan(&linked))
	assert.False(t, linked.Valid)
}

func TestXUnavailableAndSuppressedPostsNeverIngestText(t *testing.T) {
	for _, availability := range []string{"protected", "withheld"} {
		t.Run(availability, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st, source := xPostFixture(t)
			input := xPostInput(source, "100", "100", "must never archive")
			input.Availability = availability
			input.Author.Protected = availability == "protected"
			result, err := st.PersistXPostContext(t.Context(), input)
			_testRequire.NoError(err)
			_testAssert.True(result.Skipped)
			var messages int
			_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
			_testAssert.Zero(messages)
			target, err := st.ResolveXPostContext(t.Context(), source, "100")
			_testRequire.NoError(err)
			_testAssert.Equal("removed", target.Availability)
			_testAssert.Zero(target.MessageID)
			pending, err := st.PendingXRemovalsContext(t.Context(), source)
			_testRequire.NoError(err)
			_testRequire.Len(pending, 1)
			_testAssert.Equal(availability, pending[0].Reason)
			// A later page cannot restore text through the suppression committed
			// by the original unavailable write.
			newer := xPostInput(source, "102", "100", "also must not archive", "100", "102")
			result, err = st.PersistXPostContext(t.Context(), newer)
			_testRequire.NoError(err)
			_testAssert.True(result.Skipped)
			_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
			_testAssert.Zero(messages)
		})
	}
}

func TestXProtectedAuthorObservationAppliesAcrossSources(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, protectedSource := xPostFixture(t)
	protectedAt := xPostTime.Add(3 * time.Hour)
	protected := xPostInput(protectedSource, "100", "100", "unavailable post")
	protected.Availability = "protected"
	protected.FetchedAt = protectedAt
	protected.Author.FetchedAt = protectedAt
	protected.Author.Protected = true
	result, err := st.PersistXPostContext(t.Context(), protected)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)

	otherSource, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	delayed := xPostInput(otherSource.ID, "200", "200", "stale public response")
	delayed.OwnerID = "2001"
	delayed.FetchedAt = protectedAt.Add(-time.Minute)
	delayed.Author.FetchedAt = delayed.FetchedAt
	result, err = st.PersistXPostContext(t.Context(), delayed)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped, "a delayed public observation cannot override newer protected evidence from another source")
	target, err := st.ResolveXPostContext(t.Context(), otherSource.ID, delayed.PostID)
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
	_testAssert.Zero(target.MessageID)
	var messages int
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
	_testAssert.Zero(messages)

	unknown := xPostInput(otherSource.ID, "201", "201", "unknown author protection")
	unknown.OwnerID = "2001"
	unknown.Availability = "unverified"
	unknown.Author = nil
	unknown.FetchedAt = protectedAt.Add(time.Hour)
	result, err = st.PersistXPostContext(t.Context(), unknown)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped, "an unverified author observation cannot clear cross-source protection")
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
	_testAssert.Zero(messages)

	newerPublic := xPostInput(otherSource.ID, "202", "202", "public after confirmation")
	newerPublic.OwnerID = "2001"
	newerPublic.FetchedAt = protectedAt.Add(2 * time.Hour)
	newerPublic.Author.FetchedAt = newerPublic.FetchedAt
	result, err = st.PersistXPostContext(t.Context(), newerPublic)
	_testRequire.NoError(err)
	_testAssert.Positive(result.MessageID, "newer explicit public evidence permits later posts by the author")
	body, err := st.GetMessageBodyText(result.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("public after confirmation", body)
}

func TestXCompletedRemovalReplayPreservesFinishedCleanup(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	post := xPostInput(source, "100", "100", "removed content")
	saved, err := st.PersistXPostContext(t.Context(), post)
	_testRequire.NoError(err)

	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	work, err := st.RemoveXPostContentContext(t.Context(), source, post.PostID, "removed")
	_testRequire.NoError(err)
	work.DerivedComplete = true
	_testRequire.NoError(st.FinishXRemovalContext(t.Context(), source, work))

	replay := post
	replay.Availability = "removed"
	replay.FetchedAt = replay.FetchedAt.Add(time.Hour)
	replay.Author.FetchedAt = replay.FetchedAt
	result, err := st.PersistXPostContext(t.Context(), replay)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)
	_testAssert.Equal(saved.MessageID, result.MessageID)
	pending, err := st.PendingXRemovalsContext(t.Context(), source)
	_testRequire.NoError(err)
	_testAssert.Empty(pending, "a replayed completed tombstone must not queue derived cleanup again")
	var cleanupPending int
	_testRequire.NoError(st.DB().QueryRow(st.Rebind(`SELECT cleanup_pending FROM x_post_versions WHERE source_id=? AND post_id=?`), source, post.PostID).Scan(&cleanupPending))
	_testAssert.Zero(cleanupPending)
}

func TestXPostRequiresSourceOwnerAndCanonicalIDs(t *testing.T) {
	st, source := xPostFixture(t)
	for _, mutate := range []func(*store.XPersistPostInput){
		func(p *store.XPersistPostInput) { p.OwnerID = "2001" }, func(p *store.XPersistPostInput) { p.PostID = "01" }, func(p *store.XPersistPostInput) { p.EditHistory = []string{"102", "100"} }, func(p *store.XPersistPostInput) { p.AuthorID = "0" }, func(p *store.XPersistPostInput) { p.MentionIDs = []string{"01"} }, func(p *store.XPersistPostInput) { p.FetchedAt = time.Time{} },
	} {
		p := xPostInput(source, "100", "100", "invalid")
		mutate(&p)
		_, err := st.PersistXPostContext(t.Context(), p)
		require.Error(t, err)
	}
	var count int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&count))
	assert.Zero(t, count)
}

func xMessageMetadata(t *testing.T, st *store.Store, id int64) map[string]any {
	t.Helper()
	raw, err := st.GetMessageMetadata(id)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw.String), &result))
	return result
}

func TestXUnknownAvailabilityKeepsProtectedEvidence(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	private := xPostInput(source, "100", "100", "private text")
	private.Availability = "protected"
	private.Author.Protected = true
	_, err := st.PersistXPostContext(t.Context(), private)
	_testRequire.NoError(err)
	unknown := xPostInput(source, "200", "200", "still not public proof")
	unknown.Availability = "unverified"
	unknown.FetchedAt = private.FetchedAt.Add(time.Hour)
	unknown.Author.FetchedAt = unknown.FetchedAt
	unknown.Author.Username = "unverified_handle"
	unknown.Author.DisplayName = "Unverified Name"
	result, err := st.PersistXPostContext(t.Context(), unknown)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), source, "200")
	_testRequire.NoError(err)
	_testAssert.NotEqual("public", target.Availability)
	_testAssert.Zero(target.MessageID)
	var usernames int
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM participant_contact_observations WHERE address_kind='username' AND superseded_at IS NULL AND active_until IS NULL").Scan(&usernames))
	_testAssert.Zero(usernames)
}

func TestXSameVersionEvidenceEnrichesWithoutLosingText(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	parent := xPostInput(source, "300", "100", "parent")
	p, err := st.PersistXPostContext(t.Context(), parent)
	_testRequire.NoError(err)
	original := xPostInput(source, "200", "100", "complete constellation text")
	original.Author = nil
	original.Availability = "unverified"
	original.Raw = []byte(`{"post":{"id":"200","note_tweet":{"text":"complete constellation text"}}}`)
	saved, err := st.PersistXPostContext(t.Context(), original)
	_testRequire.NoError(err)
	evidence := xPostInput(source, "200", "100", "short preview")
	evidence.TextIncomplete = true
	evidence.FetchedAt = original.FetchedAt.Add(time.Hour)
	evidence.Author.FetchedAt = evidence.FetchedAt
	evidence.MentionIDs = []string{"1003"}
	evidence.ReplyToPostID = "300"
	_, err = st.PersistXPostContext(t.Context(), evidence)
	_testRequire.NoError(err)
	body, err := st.GetMessageBodyText(saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("complete constellation text", body)
	var linked sql.NullInt64
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), saved.MessageID).Scan(&linked))
	_testRequire.True(linked.Valid)
	_testAssert.Equal(p.MessageID, linked.Int64)
	var mentions int
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM message_recipients WHERE message_id=? AND recipient_type='to'"), saved.MessageID).Scan(&mentions))
	_testAssert.Equal(1, mentions)
	raw, err := st.GetMessageRaw(saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal(original.Raw, raw)
	stale := evidence
	stale.MentionIDs = []string{"1003", "1004"}
	stale.FetchedAt = original.FetchedAt.Add(-time.Second)
	_, err = st.PersistXPostContext(t.Context(), stale)
	_testRequire.NoError(err)
	evidence.Author = nil
	evidence.Availability = "unverified"
	evidence.MentionIDs = nil
	evidence.ReplyToPostID = ""
	evidence.FetchedAt = evidence.FetchedAt.Add(time.Hour)
	_, err = st.PersistXPostContext(t.Context(), evidence)
	_testRequire.NoError(err)
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM message_recipients WHERE message_id=? AND recipient_type='to'"), saved.MessageID).Scan(&mentions))
	_testAssert.Equal(1, mentions)
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), saved.MessageID).Scan(&linked))
	_testRequire.True(linked.Valid)
	_testAssert.Equal(p.MessageID, linked.Int64)
	metadata := xMessageMetadata(t, st, saved.MessageID)
	_testAssert.Equal(false, metadata["text_incomplete"])
}

func TestXFullTextEnrichmentRetainsKnownEvidence(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	parent := xPostInput(source, "300", "100", "parent")
	p, err := st.PersistXPostContext(t.Context(), parent)
	_testRequire.NoError(err)
	preview := xPostInput(source, "200", "100", "preview")
	preview.TextIncomplete = true
	preview.MentionIDs = []string{"1003"}
	preview.ReplyToPostID = "300"
	preview.QuotedPostID = "400"
	preview.Raw = []byte(`{"post":{"id":"200","text":"preview"},"author":{"id":"1002","username":"example_author","protected":false}}`)
	saved, err := st.PersistXPostContext(t.Context(), preview)
	_testRequire.NoError(err)
	full := xPostInput(source, "200", "100", "full constellation")
	full.Author = nil
	full.Availability = "unverified"
	full.FetchedAt = preview.FetchedAt.Add(time.Hour)
	full.Raw = []byte(`{"post":{"id":"200","note_tweet":{"text":"full constellation"}}}`)
	_, err = st.PersistXPostContext(t.Context(), full)
	_testRequire.NoError(err)
	body, err := st.GetMessageBodyText(saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal(full.Text, body)
	var parentID sql.NullInt64
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), saved.MessageID).Scan(&parentID))
	_testRequire.True(parentID.Valid)
	_testAssert.Equal(p.MessageID, parentID.Int64)
	var mentions int
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM message_recipients WHERE message_id=? AND recipient_type='to'"), saved.MessageID).Scan(&mentions))
	_testAssert.Equal(1, mentions)
	metadata := xMessageMetadata(t, st, saved.MessageID)
	_testAssert.Equal("300", metadata["referenced_message_id"])
	_testAssert.Equal("400", metadata["quoted_post_id"])
	var fromName string
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'"), saved.MessageID).Scan(&fromName))
	_testAssert.Equal("Example Author", fromName)
	raw, err := st.GetMessageRaw(saved.MessageID)
	_testRequire.NoError(err)
	var envelope map[string]any
	_testRequire.NoError(json.Unmarshal(raw, &envelope))
	_testAssert.Contains(envelope, "author")
}

func TestXProtectionEvidenceUsesExactAuthorAfterParticipantMerge(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	public := xPostInput(source, "100", "100", "public account")
	_, err := st.PersistXPostContext(t.Context(), public)
	_testRequire.NoError(err)
	private := xPostInput(source, "200", "200", "private account")
	private.AuthorID = "1003"
	private.Author.UserID = "1003"
	private.Author.Username = "private_account"
	private.Availability = "protected"
	private.Author.Protected = true
	_, err = st.PersistXPostContext(t.Context(), private)
	_testRequire.NoError(err)
	var publicID, privateID int64
	_testRequire.NoError(st.DB().QueryRow("SELECT participant_id FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1002'").Scan(&publicID))
	_testRequire.NoError(st.DB().QueryRow("SELECT participant_id FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1003'").Scan(&privateID))
	_testRequire.NoError(st.MergeParticipants(privateID, publicID))
	unknown := xPostInput(source, "300", "300", "must stay private")
	unknown.AuthorID = "1003"
	unknown.Author.UserID = "1003"
	unknown.Availability = "unverified"
	unknown.FetchedAt = private.FetchedAt.Add(time.Hour)
	result, err := st.PersistXPostContext(t.Context(), unknown)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)
	_testAssert.Zero(result.MessageID)
	var count int
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM messages WHERE source_id=?"), source).Scan(&count))
	_testAssert.Equal(1, count)
	stillPublic := public
	stillPublic.PostID = "400"
	stillPublic.ConversationID = "400"
	stillPublic.EditHistory = []string{"400"}
	stillPublic.Author = nil
	stillPublic.Availability = "unverified"
	stillPublic.FetchedAt = unknown.FetchedAt
	result, err = st.PersistXPostContext(t.Context(), stillPublic)
	_testRequire.NoError(err)
	_testAssert.True(result.Added)
	_testAssert.False(result.Skipped)
}
