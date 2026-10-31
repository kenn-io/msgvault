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
	st, source := xPostFixture(t)
	newer := xPostInput(source, "102", "100", "current constellation", "100", "102")
	newer.CreatedAt = xPostTime.Add(time.Minute)
	newer.MentionIDs = []string{"1003"}
	first, err := st.PersistXPostContext(t.Context(), newer)
	require.NoError(t, err)
	assert.True(t, first.Added)
	older := xPostInput(source, "100", "100", "obsolete telescope", "100")
	older.FetchedAt = xPostTime
	older.Author.FetchedAt = xPostTime
	older.Author.DisplayName = "Stale Author"
	older.MentionIDs = []string{"1004"}
	old, err := st.PersistXPostContext(t.Context(), older)
	require.NoError(t, err)
	assert.Equal(t, first.MessageID, old.MessageID)
	target, err := st.ResolveXPostContext(t.Context(), source, "100")
	require.NoError(t, err)
	assert.Equal(t, "100", target.CanonicalID)
	assert.Equal(t, "102", target.CurrentID)
	assert.Equal(t, first.MessageID, target.MessageID)
	body, err := st.GetMessageBodyText(first.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "current constellation", body)
	raw, err := st.GetMessageRaw(first.MessageID)
	require.NoError(t, err)
	assert.Equal(t, newer.Raw, raw)
	var snippet, authorName, toID string
	var sent sql.NullTime
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT snippet,sent_at FROM messages WHERE id=?"), first.MessageID).Scan(&snippet, &sent))
	assert.Equal(t, "current constellation", snippet)
	assert.WithinDuration(t, xPostTime, sent.Time, time.Microsecond)
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'"), first.MessageID).Scan(&authorName))
	assert.Equal(t, "Example Author", authorName)
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT pi.identifier_value FROM message_recipients mr JOIN participant_identifiers pi ON pi.participant_id=mr.participant_id WHERE mr.message_id=? AND mr.recipient_type='to' AND pi.identifier_type='x_user_id'"), first.MessageID).Scan(&toID))
	assert.Equal(t, "1003", toID)
	hits, count, err := st.SearchMessages("constellation", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	require.Len(t, hits, 1)
	assert.Equal(t, first.MessageID, hits[0].ID)
	_, count, err = st.SearchMessages("obsolete", 0, 10)
	require.NoError(t, err)
	assert.Zero(t, count)
	// Reject a splice joining a known post to a conflicting canonical chain.
	conflict := xPostInput(source, "104", "100", "conflict", "99", "102", "104")
	_, err = st.PersistXPostContext(t.Context(), conflict)
	require.Error(t, err)
	_, err = st.ResolveXPostContext(t.Context(), source, "104")
	require.Error(t, err)
}

func TestXPostProjectionRollsBackEveryDependent(t *testing.T) {
	st, source := xPostFixture(t)
	seed := xPostInput(source, "100", "100", "original")
	saved, err := st.PersistXPostContext(t.Context(), seed)
	require.NoError(t, err)
	require.NoError(t, st.EnableEmbeddingChangeJournal(t.Context()))
	var journalBefore, observationsBefore int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM embedding_changes").Scan(&journalBefore))
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM participant_contact_observations").Scan(&observationsBefore))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_x_raw() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic X raw failure'; END $$; CREATE TRIGGER reject_x_raw BEFORE INSERT OR UPDATE ON message_raw FOR EACH ROW EXECUTE FUNCTION reject_x_raw()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_x_raw BEFORE INSERT ON message_raw BEGIN SELECT RAISE(ABORT,'synthetic X raw failure'); END`)
	}
	require.NoError(t, err)
	change := xPostInput(source, "102", "100", "must roll back", "100", "102")
	change.Author.Username = "renamed_author"
	change.FetchedAt = change.FetchedAt.Add(time.Hour)
	change.Author.FetchedAt = change.FetchedAt
	change.MentionIDs = []string{"1003"}
	_, err = st.PersistXPostContext(t.Context(), change)
	require.ErrorContains(t, err, "synthetic X raw failure")
	body, err := st.GetMessageBodyText(saved.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "original", body)
	raw, err := st.GetMessageRaw(saved.MessageID)
	require.NoError(t, err)
	assert.Equal(t, seed.Raw, raw)
	var aliases, journalAfter, observationsAfter, mentions int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM x_post_versions").Scan(&aliases))
	assert.Equal(t, 1, aliases)
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM embedding_changes").Scan(&journalAfter))
	assert.Equal(t, journalBefore, journalAfter)
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM participant_contact_observations").Scan(&observationsAfter))
	assert.Equal(t, observationsBefore, observationsAfter)
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM message_recipients WHERE recipient_type='to'").Scan(&mentions))
	assert.Zero(t, mentions)
}

func TestXSameVersionEnrichmentRequiresFreshEvidence(t *testing.T) {
	st, source := xPostFixture(t)
	incomplete := xPostInput(source, "100", "100", "short preview")
	incomplete.TextIncomplete = true
	incomplete.Availability = "unverified"
	incomplete.Author = nil
	result, err := st.PersistXPostContext(t.Context(), incomplete)
	require.NoError(t, err)
	richer := xPostInput(source, "100", "100", "complete constellation text")
	richer.FetchedAt = incomplete.FetchedAt.Add(-time.Second)
	richer.Author.FetchedAt = richer.FetchedAt
	_, err = st.PersistXPostContext(t.Context(), richer)
	require.NoError(t, err)
	body, err := st.GetMessageBodyText(result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "short preview", body)
	richer.FetchedAt = incomplete.FetchedAt.Add(time.Second)
	richer.Author.FetchedAt = richer.FetchedAt
	enriched, err := st.PersistXPostContext(t.Context(), richer)
	require.NoError(t, err)
	assert.True(t, enriched.Updated)
	body, err = st.GetMessageBodyText(result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "complete constellation text", body)
	incomplete.FetchedAt = richer.FetchedAt.Add(time.Hour)
	_, err = st.PersistXPostContext(t.Context(), incomplete)
	require.NoError(t, err)
	body, err = st.GetMessageBodyText(result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "complete constellation text", body)
	raw, err := st.GetMessageRaw(result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, richer.Raw, raw)
}

func TestXLateRootConsolidationKeepsMessageIDs(t *testing.T) {
	st, source := xPostFixture(t)
	left := xPostInput(source, "200", "100", "first reply")
	left.ReplyToPostID = "100"
	right := xPostInput(source, "201", "102", "second reply")
	right.ReplyToPostID = "102"
	l, err := st.PersistXPostContext(t.Context(), left)
	require.NoError(t, err)
	r, err := st.PersistXPostContext(t.Context(), right)
	require.NoError(t, err)
	lt, err := st.ResolveXPostContext(t.Context(), source, "200")
	require.NoError(t, err)
	rt, err := st.ResolveXPostContext(t.Context(), source, "201")
	require.NoError(t, err)
	require.NotEqual(t, lt.ConversationID, rt.ConversationID)
	for _, draft := range []struct {
		id, native, parent string
		conversation       int64
	}{{"left-draft", "100", "200", lt.ConversationID}, {"right-draft", "102", "201", rt.ConversationID}} {
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO chat_drafts(draft_id,source_id,conversation_id,source_conversation_id,conversation_type,reply_to_source_message_id,body,revision) VALUES(?,?,?,?,?,?,?,?)`), draft.id, source, draft.conversation, draft.native, "channel", draft.parent, "reviewed synthetic reply", 7)
		require.NoError(t, err)
	}
	root := xPostInput(source, "102", "102", "edited root", "100", "102")
	root.AuthorID = "1001"
	root.Author.UserID = "1001"
	root.Author.Username = "example_owner"
	rootResult, err := st.PersistXPostContext(t.Context(), root)
	require.NoError(t, err)
	_, err = st.ResolveXParentsContext(t.Context(), source, 0, 100)
	require.NoError(t, err)
	for _, item := range []struct {
		native string
		id     int64
	}{{"200", l.MessageID}, {"201", r.MessageID}} {
		target, err := st.ResolveXPostContext(t.Context(), source, item.native)
		require.NoError(t, err)
		assert.Equal(t, item.id, target.MessageID)
		assert.Equal(t, lt.ConversationID, target.ConversationID)
		var parent sql.NullInt64
		require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), item.id).Scan(&parent))
		require.True(t, parent.Valid)
		assert.Equal(t, rootResult.MessageID, parent.Int64)
	}
	var conversations, messages int
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT count(*) FROM conversations WHERE source_id=?"), source).Scan(&conversations))
	assert.Equal(t, 1, conversations)
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT message_count FROM conversations WHERE id=?"), lt.ConversationID).Scan(&messages))
	assert.Equal(t, 3, messages)
	for _, draft := range []struct{ id, native, parent string }{{"left-draft", "100", "200"}, {"right-draft", "102", "201"}} {
		var conversation, revision int64
		var native, parent, body string
		require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT conversation_id,source_conversation_id,reply_to_source_message_id,body,revision FROM chat_drafts WHERE draft_id=?"), draft.id).Scan(&conversation, &native, &parent, &body, &revision))
		assert.Equal(t, lt.ConversationID, conversation)
		assert.Equal(t, draft.native, native)
		assert.Equal(t, draft.parent, parent)
		assert.Equal(t, "reviewed synthetic reply", body)
		assert.Equal(t, int64(7), revision)
	}
}

func TestXParentResolvesAfterMonths(t *testing.T) {
	st, source := xPostFixture(t)
	child := xPostInput(source, "200", "100", "delayed child")
	child.ReplyToPostID = "100"
	result, err := st.PersistXPostContext(t.Context(), child)
	require.NoError(t, err)
	next, err := st.ResolveXParentsContext(t.Context(), source, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, result.MessageID, next)
	var parent sql.NullInt64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), result.MessageID).Scan(&parent))
	assert.False(t, parent.Valid)
	root := xPostInput(source, "102", "100", "root months later", "100", "102")
	root.FetchedAt = xPostTime.AddDate(0, 4, 0)
	root.Author.FetchedAt = root.FetchedAt
	saved, err := st.PersistXPostContext(t.Context(), root)
	require.NoError(t, err)
	_, err = st.ResolveXParentsContext(t.Context(), source, 0, 1)
	require.NoError(t, err)
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), result.MessageID).Scan(&parent))
	require.True(t, parent.Valid)
	assert.Equal(t, saved.MessageID, parent.Int64)
}

func TestXPostRejectsCyclesAndForeignParents(t *testing.T) {
	st, source := xPostFixture(t)
	self := xPostInput(source, "100", "100", "self")
	self.ReplyToPostID = "100"
	_, err := st.PersistXPostContext(t.Context(), self)
	require.Error(t, err)
	a := xPostInput(source, "100", "100", "first")
	a.ReplyToPostID = "200"
	_, err = st.PersistXPostContext(t.Context(), a)
	require.NoError(t, err)
	b := xPostInput(source, "200", "100", "second")
	b.ReplyToPostID = "100"
	_, err = st.PersistXPostContext(t.Context(), b)
	require.Error(t, err)
	foreign, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	p := xPostInput(foreign.ID, "300", "300", "foreign")
	p.OwnerID = "2001"
	_, err = st.PersistXPostContext(t.Context(), p)
	require.NoError(t, err)
	c := xPostInput(source, "400", "300", "own child")
	c.ReplyToPostID = "300"
	child, err := st.PersistXPostContext(t.Context(), c)
	require.NoError(t, err)
	_, err = st.ResolveXParentsContext(t.Context(), source, 0, 100)
	require.NoError(t, err)
	var linked sql.NullInt64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), child.MessageID).Scan(&linked))
	assert.False(t, linked.Valid)
}

func TestXUnavailableAndSuppressedPostsNeverIngestText(t *testing.T) {
	for _, availability := range []string{"protected", "withheld"} {
		t.Run(availability, func(t *testing.T) {
			st, source := xPostFixture(t)
			input := xPostInput(source, "100", "100", "must never archive")
			input.Availability = availability
			input.Author.Protected = availability == "protected"
			result, err := st.PersistXPostContext(t.Context(), input)
			require.NoError(t, err)
			assert.True(t, result.Skipped)
			var messages int
			require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
			assert.Zero(t, messages)
			target, err := st.ResolveXPostContext(t.Context(), source, "100")
			require.NoError(t, err)
			assert.Equal(t, availability, target.Availability)
			assert.Zero(t, target.MessageID)
			// A later page that shares a suppressed alias cannot restore text.
			_, err = st.DB().Exec(st.Rebind("UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP WHERE source_id=?"), source)
			require.NoError(t, err)
			newer := xPostInput(source, "102", "100", "also must not archive", "100", "102")
			result, err = st.PersistXPostContext(t.Context(), newer)
			require.NoError(t, err)
			assert.True(t, result.Skipped)
			require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
			assert.Zero(t, messages)
		})
	}
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
	st, source := xPostFixture(t)
	private := xPostInput(source, "100", "100", "private text")
	private.Availability = "protected"
	private.Author.Protected = true
	_, err := st.PersistXPostContext(t.Context(), private)
	require.NoError(t, err)
	unknown := xPostInput(source, "200", "200", "still not public proof")
	unknown.Availability = "unverified"
	unknown.FetchedAt = private.FetchedAt.Add(time.Hour)
	unknown.Author.FetchedAt = unknown.FetchedAt
	unknown.Author.Username = "unverified_handle"
	unknown.Author.DisplayName = "Unverified Name"
	result, err := st.PersistXPostContext(t.Context(), unknown)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), source, "200")
	require.NoError(t, err)
	assert.NotEqual(t, "public", target.Availability)
	assert.Zero(t, target.MessageID)
	var usernames int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM participant_contact_observations WHERE address_kind='username' AND superseded_at IS NULL AND active_until IS NULL").Scan(&usernames))
	assert.Zero(t, usernames)
}

func TestXSameVersionEvidenceEnrichesWithoutLosingText(t *testing.T) {
	st, source := xPostFixture(t)
	parent := xPostInput(source, "300", "100", "parent")
	p, err := st.PersistXPostContext(t.Context(), parent)
	require.NoError(t, err)
	original := xPostInput(source, "200", "100", "complete constellation text")
	original.Author = nil
	original.Availability = "unverified"
	original.Raw = []byte(`{"post":{"id":"200","note_tweet":{"text":"complete constellation text"}}}`)
	saved, err := st.PersistXPostContext(t.Context(), original)
	require.NoError(t, err)
	evidence := xPostInput(source, "200", "100", "short preview")
	evidence.TextIncomplete = true
	evidence.FetchedAt = original.FetchedAt.Add(time.Hour)
	evidence.Author.FetchedAt = evidence.FetchedAt
	evidence.MentionIDs = []string{"1003"}
	evidence.ReplyToPostID = "300"
	_, err = st.PersistXPostContext(t.Context(), evidence)
	require.NoError(t, err)
	body, err := st.GetMessageBodyText(saved.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "complete constellation text", body)
	var linked sql.NullInt64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), saved.MessageID).Scan(&linked))
	require.True(t, linked.Valid)
	assert.Equal(t, p.MessageID, linked.Int64)
	var mentions int
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT count(*) FROM message_recipients WHERE message_id=? AND recipient_type='to'"), saved.MessageID).Scan(&mentions))
	assert.Equal(t, 1, mentions)
	raw, err := st.GetMessageRaw(saved.MessageID)
	require.NoError(t, err)
	assert.Equal(t, original.Raw, raw)
	stale := evidence
	stale.MentionIDs = []string{"1003", "1004"}
	stale.FetchedAt = original.FetchedAt.Add(-time.Second)
	_, err = st.PersistXPostContext(t.Context(), stale)
	require.NoError(t, err)
	evidence.Author = nil
	evidence.Availability = "unverified"
	evidence.MentionIDs = nil
	evidence.ReplyToPostID = ""
	evidence.FetchedAt = evidence.FetchedAt.Add(time.Hour)
	_, err = st.PersistXPostContext(t.Context(), evidence)
	require.NoError(t, err)
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT count(*) FROM message_recipients WHERE message_id=? AND recipient_type='to'"), saved.MessageID).Scan(&mentions))
	assert.Equal(t, 1, mentions)
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), saved.MessageID).Scan(&linked))
	require.True(t, linked.Valid)
	assert.Equal(t, p.MessageID, linked.Int64)
	metadata := xMessageMetadata(t, st, saved.MessageID)
	assert.Equal(t, false, metadata["text_incomplete"])
}

func TestXFullTextEnrichmentRetainsKnownEvidence(t *testing.T) {
	st, source := xPostFixture(t)
	parent := xPostInput(source, "300", "100", "parent")
	p, err := st.PersistXPostContext(t.Context(), parent)
	require.NoError(t, err)
	preview := xPostInput(source, "200", "100", "preview")
	preview.TextIncomplete = true
	preview.MentionIDs = []string{"1003"}
	preview.ReplyToPostID = "300"
	preview.QuotedPostID = "400"
	preview.Raw = []byte(`{"post":{"id":"200","text":"preview"},"author":{"id":"1002","username":"example_author","protected":false}}`)
	saved, err := st.PersistXPostContext(t.Context(), preview)
	require.NoError(t, err)
	full := xPostInput(source, "200", "100", "full constellation")
	full.Author = nil
	full.Availability = "unverified"
	full.FetchedAt = preview.FetchedAt.Add(time.Hour)
	full.Raw = []byte(`{"post":{"id":"200","note_tweet":{"text":"full constellation"}}}`)
	_, err = st.PersistXPostContext(t.Context(), full)
	require.NoError(t, err)
	body, err := st.GetMessageBodyText(saved.MessageID)
	require.NoError(t, err)
	assert.Equal(t, full.Text, body)
	var parentID sql.NullInt64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), saved.MessageID).Scan(&parentID))
	require.True(t, parentID.Valid)
	assert.Equal(t, p.MessageID, parentID.Int64)
	var mentions int
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT count(*) FROM message_recipients WHERE message_id=? AND recipient_type='to'"), saved.MessageID).Scan(&mentions))
	assert.Equal(t, 1, mentions)
	metadata := xMessageMetadata(t, st, saved.MessageID)
	assert.Equal(t, "300", metadata["referenced_message_id"])
	assert.Equal(t, "400", metadata["quoted_post_id"])
	var fromName string
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'"), saved.MessageID).Scan(&fromName))
	assert.Equal(t, "Example Author", fromName)
	raw, err := st.GetMessageRaw(saved.MessageID)
	require.NoError(t, err)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(raw, &envelope))
	assert.Contains(t, envelope, "author")
}

func TestXProtectionEvidenceUsesExactAuthorAfterParticipantMerge(t *testing.T) {
	st, source := xPostFixture(t)
	public := xPostInput(source, "100", "100", "public account")
	_, err := st.PersistXPostContext(t.Context(), public)
	require.NoError(t, err)
	private := xPostInput(source, "200", "200", "private account")
	private.AuthorID = "1003"
	private.Author.UserID = "1003"
	private.Author.Username = "private_account"
	private.Availability = "protected"
	private.Author.Protected = true
	_, err = st.PersistXPostContext(t.Context(), private)
	require.NoError(t, err)
	var publicID, privateID int64
	require.NoError(t, st.DB().QueryRow("SELECT participant_id FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1002'").Scan(&publicID))
	require.NoError(t, st.DB().QueryRow("SELECT participant_id FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1003'").Scan(&privateID))
	require.NoError(t, st.MergeParticipants(privateID, publicID))
	unknown := xPostInput(source, "300", "300", "must stay private")
	unknown.AuthorID = "1003"
	unknown.Author.UserID = "1003"
	unknown.Availability = "unverified"
	unknown.FetchedAt = private.FetchedAt.Add(time.Hour)
	result, err := st.PersistXPostContext(t.Context(), unknown)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	assert.Zero(t, result.MessageID)
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT count(*) FROM messages WHERE source_id=?"), source).Scan(&count))
	assert.Equal(t, 1, count)
	stillPublic := public
	stillPublic.PostID = "400"
	stillPublic.ConversationID = "400"
	stillPublic.EditHistory = []string{"400"}
	stillPublic.Author = nil
	stillPublic.Availability = "unverified"
	stillPublic.FetchedAt = unknown.FetchedAt
	result, err = st.PersistXPostContext(t.Context(), stillPublic)
	require.NoError(t, err)
	assert.True(t, result.Added)
	assert.False(t, result.Skipped)
}
