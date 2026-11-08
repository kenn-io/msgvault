package query

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

type sqliteExploreFixture struct {
	live  *SQLiteEngine
	cache *DuckDBEngine
	store *store.Store
	path  string
}

// Reuse the analytical fixture on both backends so the comparison exercises
// real live SQL and the committed cache, including their different time formats.
func liveExploreFixture(t *testing.T) sqliteExploreFixture {
	t.Helper()
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.test")
	other := b.AddSourceWithType("chat@example.test", "whatsapp")
	alice := b.AddParticipant("alice@example.test", "example.test", "Alice")
	bob := b.AddParticipant("bob@other.test", "other.test", "Bob")
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	first := b.AddMessage(MessageOpt{SourceID: source, Subject: "First", SentAt: base, ListID: new("<list@example.test>")})
	b.AddFrom(first, alice, "Alice envelope")
	b.AddTo(first, bob, "Bob")
	b.AddAttachment(first, 128, "file.txt")
	for i, typ := range []string{"whatsapp", "whatsapp", "event", "meeting", "item", "email"} {
		opt := MessageOpt{SourceID: other, MessageType: typ, SentAt: base.Add(time.Duration(i) * time.Hour), Subject: typ, SenderID: &alice}
		if i < 2 {
			opt.ConversationID = 900
			opt.ConversationType = "group_chat"
			opt.ConversationTitle = "Chat"
		}
		if i == 1 {
			opt.DeletedFromSourceAt = &base
			opt.HasAttachments = true
		}
		b.AddMessage(opt)
	}
	// Microseconds distinguish imported chat timestamps and adjacent date bounds.
	for _, microsecond := range []int{100, 200} {
		b.AddMessage(MessageOpt{SourceID: source, Subject: fmt.Sprintf("Precision %d", microsecond), SentAt: base.Add(10*time.Hour + time.Duration(microsecond)*time.Microsecond)})
	}
	for _, microsecond := range []int{200, 100} {
		b.AddMessage(MessageOpt{SourceID: other, MessageType: "whatsapp", ConversationID: 901, ConversationType: "group_chat", ConversationTitle: "Precision chat", SentAt: base.Add(11*time.Hour + time.Duration(microsecond)*time.Microsecond)})
	}
	b.AddConversationParticipant(900, bob)
	b.AddAttachment(2, 256, "chat.txt")
	// These rows compare UTC instants whose textual ordering disagrees.
	b.messages[2].SentAt = base.Add(30 * time.Minute)
	for _, s := range b.sources {
		_, err := st.DB().ExecContext(t.Context(), "INSERT INTO sources(id, source_type, identifier) VALUES(?, ?, ?)", s.ID, s.SourceType, s.AccountEmail)
		require.NoError(err)
	}
	for _, c := range b.conversations {
		src := source
		for _, m := range b.messages {
			if m.ConversationID == c.ID {
				src = m.SourceID
				break
			}
		}
		_, err := st.DB().ExecContext(t.Context(), "INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(?,?,?,?,?)", c.ID, src, c.SourceConversationID, c.ConversationType, c.Title)
		require.NoError(err)
	}
	for _, p := range b.participants {
		_, err := st.DB().ExecContext(t.Context(), "INSERT INTO participants(id,email_address,domain,display_name,phone_number) VALUES(?,?,?,?,NULLIF(?,''))", p.ID, p.Email, p.Domain, p.DisplayName, p.PhoneNumber)
		require.NoError(err)
	}
	for i, m := range b.messages {
		var sent any = m.SentAt
		if i == 2 {
			sent = m.SentAt.Format(time.RFC3339)
		}
		_, err := st.DB().ExecContext(t.Context(), `INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,subject,snippet,sent_at,sender_id,has_attachments,deleted_from_source_at,list_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, m.SourceID, m.SourceMessageID, m.ConversationID, m.resolvedMessageType(), m.Subject, m.Snippet, sent, m.SenderID, m.HasAttachments, m.DeletedFromSourceAt, m.ListID)
		require.NoError(err)
	}
	for _, r := range b.recipients {
		_, err := st.DB().ExecContext(t.Context(), "INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name) VALUES(?,?,?,?)", r.MessageID, r.ParticipantID, r.Type, r.DisplayName)
		require.NoError(err)
	}
	_, err = st.DB().ExecContext(t.Context(), "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(900,?)", bob)
	require.NoError(err)
	for _, a := range b.attachments {
		_, err := st.DB().ExecContext(t.Context(), "INSERT INTO attachments(id,message_id,size,filename,storage_path) VALUES(?,?,?,?,?)", a.ID, a.MessageID, a.Size, a.Filename, "synthetic")
		require.NoError(err)
	}
	// Export omits internally deleted rows and unsent rows.
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,sent_at,deleted_at) VALUES(90,1,'hidden',200,'email','2026-08-01','2026-08-02'),(91,1,'unsent',200,'email',NULL,NULL)`)
	require.NoError(err)
	live := NewSQLiteExploreEngine(st.DB(), path)
	t.Cleanup(func() { require.NoError(live.Close()) })
	return sqliteExploreFixture{live, b.BuildEngine(), st, path}
}

func TestSQLiteExploreMatchesCache(t *testing.T) {
	t.Parallel()
	fixture := liveExploreFixture(t)
	live, cache := fixture.live, fixture.cache
	after := time.Date(2026, 7, 1, 12, 15, 0, 0, time.UTC)
	before := after.Add(3 * time.Hour)
	precisionAfter := time.Date(2026, 7, 1, 22, 0, 0, 150000, time.UTC)
	precisionBefore := precisionAfter.Add(100 * time.Microsecond)
	requests := map[string]ExploreRequest{
		"all":                {},
		"microsecond dates":  {Context: Context{After: &precisionAfter, Before: &precisionBefore}},
		"source":             {Context: Context{SourceIDs: []int64{2}}},
		"account":            {Context: Context{AccountScopes: []search.AccountScope{{SourceID: new(int64(1)), Unattributed: true}}}},
		"dates":              {Context: Context{After: &after, Before: &before}},
		"kind":               {Context: Context{Kinds: []EntryKind{EntryConversation}}},
		"type":               {Context: Context{MessageTypes: []string{"email"}}},
		"domain":             {Context: Context{Domains: []string{"other.test"}}},
		"domain conjunction": {Context: Context{Domains: []string{"example.test"}, AdditionalDomainGroups: [][]string{{"other.test"}}}},
		"mailing list":       {Context: Context{MailingLists: []string{"<LIST@example.test>"}}},
		"list conjunction":   {Context: Context{MailingLists: []string{"<list@example.test>"}, AdditionalMailingListGroups: [][]string{{"<LIST@example.test>"}}}},
		"active":             {Context: Context{Deletion: DeletionActive}},
		"deleted":            {Context: Context{Deletion: DeletionDeleted}},
		"full text":          {Search: SearchSpec{Mode: SearchFullText, Query: "fixture", CandidateMessageIDs: []int64{1, 2, 3}, LexicalIndexRevision: "fts5:fixture"}},
		"empty full text":    {Search: SearchSpec{Mode: SearchFullText, Query: "missing", CandidateMessageIDs: []int64{}, LexicalIndexRevision: "fts5:fixture"}},
		"page":               {Page: PageSpec{Limit: 2, Offset: 1}},
		"past end":           {Page: PageSpec{Limit: 2, Offset: 100}},
	}
	for _, direction := range []IdentityDirection{IdentityDirectionAny, IdentityDirectionSender, IdentityDirectionRecipient} {
		requests["identity "+string(direction)] = ExploreRequest{Context: Context{Identity: &IdentityPredicate{SourceID: 1, ParticipantIDs: []int64{1}, Direction: direction}}}
	}
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			expected, err := cache.Explore(t.Context(), request)
			require.NoError(err)
			got, err := live.Explore(t.Context(), request)
			require.NoError(err)
			for i := range expected.Rows {
				expected.Rows[i].CounterpartParticipantID = nil
			}
			assert.Equal(expected.Rows, got.Rows)
			assert.Equal(expected.TotalCount, got.TotalCount)
			assert.Equal(expected.SearchProvenance, got.SearchProvenance)
			assert.Contains(got.CacheRevision, "live-")
		})
	}
}

func TestSQLiteExploreSnapshotRevision(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	fixture := liveExploreFixture(t)
	live, st, path := fixture.live, fixture.store, fixture.path
	writer, err := store.OpenForTest(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(writer.Close()) })
	initial, err := live.Explore(t.Context(), ExploreRequest{})
	require.NoError(err)
	again, err := live.Explore(t.Context(), ExploreRequest{})
	require.NoError(err)
	assert.Equal(initial.CacheRevision, again.CacheRevision)
	ctx, release, version, err := live.BeginExploreSnapshot(t.Context())
	require.NoError(err)
	// Candidate lookup and listing read the same transaction, even after a commit.
	var before int
	require.NoError(store.ReadDBContext(ctx, st.DB()).QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&before))
	_, err = writer.DB().ExecContext(t.Context(), "INSERT INTO messages(source_id,source_message_id,conversation_id,message_type,sent_at) VALUES(1,'new',200,'email','2026-09-01')")
	require.NoError(err)
	inside, err := live.Explore(ctx, ExploreRequest{})
	require.NoError(err)
	assert.Equal(version, inside.CacheRevision)
	var snapshotVersion int64
	require.NoError(store.ReadDBContext(ctx, st.DB()).QueryRowContext(ctx, "PRAGMA data_version").Scan(&snapshotVersion))
	assert.Equal(version, fmt.Sprintf("live-%s-%d", live.exploreReader.nonce, snapshotVersion))
	assert.Equal(initial.Rows, inside.Rows)
	assert.Equal(initial.TotalCount, inside.TotalCount)
	var after int
	require.NoError(store.ReadDBContext(ctx, st.DB()).QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&after))
	assert.Equal(before, after)
	release()
	next, err := live.Explore(t.Context(), ExploreRequest{})
	require.NoError(err)
	assert.NotEqual(version, next.CacheRevision)
	assert.Equal(initial.TotalCount+1, next.TotalCount)
	mutations := []string{
		"UPDATE messages SET deleted_at='2026-10-01' WHERE source_message_id='new'",
		"UPDATE messages SET deleted_at='2026-10-01' WHERE id=3",
		"UPDATE messages SET sent_at=CASE id WHEN 1 THEN '2026-07-01T15:00:00Z' ELSE '2026-07-01T12:00:00Z' END WHERE id IN (1,4)",
		"DELETE FROM conversation_participants WHERE conversation_id=900",
		"UPDATE participants SET display_name='Changed' WHERE id=1",
		"INSERT INTO labels(id,name) VALUES(10,'fixture')",
		"INSERT INTO message_labels(message_id,label_id) VALUES(1,10)",
		"UPDATE message_labels SET message_id=4 WHERE label_id=10",
		"INSERT INTO sync_runs(source_id,started_at) VALUES(1,'2026-07-01')",
	}
	for _, statement := range mutations {
		_, err = writer.DB().ExecContext(t.Context(), statement)
		require.NoError(err)
		result, err := live.Explore(t.Context(), ExploreRequest{})
		require.NoError(err)
		assert.NotEqual(next.CacheRevision, result.CacheRevision, statement)
		next = result
	}
	// A different physical reader must never reuse the old cursor's revision.
	require.NoError(live.exploreReader.conn.Close())
	live.exploreReader.conn = nil
	replaced, err := live.Explore(t.Context(), ExploreRequest{})
	require.NoError(err)
	assert.NotEqual(next.CacheRevision, replaced.CacheRevision)
}

func TestSQLiteExploreReaderLifecycle(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	live := liveExploreFixture(t).live
	ctx, release, _, err := live.BeginExploreSnapshot(t.Context())
	require.NoError(err)
	defer release()
	waiting, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		_, end, _, err := live.BeginExploreSnapshot(waiting)
		if err == nil {
			end()
		}
		finished <- err
	}()
	<-started
	cancel()
	select {
	case err := <-finished:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow("cancelled Explore remained blocked on the reader")
	}
	_, err = live.Explore(ctx, ExploreRequest{})
	require.NoError(err)
	release()
	cancelled, cancel := context.WithCancel(t.Context())
	_, end, _, err := live.BeginExploreSnapshot(cancelled)
	require.NoError(err)
	cancel()
	end()
	result, err := live.Explore(t.Context(), ExploreRequest{})
	require.NoError(err)
	assert.NotEmpty(result.Rows)
	require.NoError(live.Close())
	_, end, revision, err := live.BeginExploreSnapshot(t.Context())
	require.ErrorIs(err, sql.ErrConnDone)
	assert.Nil(end)
	assert.Empty(revision)
	explicit := NewSQLiteEngine(live.db)
	_, err = explicit.Explore(t.Context(), ExploreRequest{})
	var unavailable *CacheUnavailableError
	assert.ErrorAs(err, &unavailable)
}

func TestSQLiteExploreRefusesCacheOperations(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	live := liveExploreFixture(t).live
	for _, request := range []ExploreRequest{
		{Context: Context{ParticipantIDs: []int64{1}}},
		{Context: Context{AdditionalParticipantGroups: [][]int64{{1}}}},
		{SentOnly: true}, {PersonInteractionsOnly: true},
		{Search: SearchSpec{Mode: SearchSemantic}}, {Search: SearchSpec{Mode: SearchHybrid}},
	} {
		_, err := live.Explore(t.Context(), request)
		var unavailable *CacheUnavailableError
		assert.ErrorAs(err, &unavailable)
	}
}
