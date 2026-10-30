package threads

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const writerSource = "threads:123:456"

var writerTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func writerFixture(t *testing.T) (*Writer, *store.Store) {
	t.Helper()
	s := testutil.NewTestStore(t)
	if strings.HasPrefix(os.Getenv("MSGVAULT_TEST_DB"), "postgres") {
		require.True(t, s.IsPostgreSQL())
	}
	source, err := s.GetOrCreateSource("threads", writerSource)
	require.NoError(t, err)
	return NewWriter(s, source.ID, writerSource), s
}
func writerMedia(id, text string) Media { return Media{ID: id, Timestamp: writerTime, Text: &text} }
func writerReply(id, root, parent string) Media {
	m := writerMedia(id, "reply "+id)
	m.RootPost = &MediaReference{ID: root}
	m.RepliedTo = &MediaReference{ID: parent}
	m.IsReply = new(true)
	return m
}
func applyWriter(t *testing.T, w *Writer, m Media, o ItemOrigin) int64 {
	t.Helper()
	id, err := w.Apply(context.Background(), m, o)
	require.NoError(t, err)
	require.Positive(t, id)
	return id
}
func writerMetadata(t *testing.T, s *store.Store, id int64) map[string]any {
	t.Helper()
	raw, err := s.GetMessageMetadataContext(context.Background(), id)
	require.NoError(t, err)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw.String), &metadata))
	return metadata
}
func writerSender(t *testing.T, s *store.Store, id int64) int64 {
	t.Helper()
	r := writerRecipients(t, s, id)
	require.Len(t, r, 1)
	return r[0].ParticipantID
}

// The Store's email-recipient reader omits addressless senders. Inspect the
// actual persisted rows: a Threads occurrence must never invent an email.
func writerRecipients(t *testing.T, s *store.Store, id int64) []store.MessageRecipient {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(), s.Rebind(`SELECT participant_id, COALESCE(display_name,''), COALESCE(email_address,'') FROM message_recipients WHERE message_id=? AND recipient_type='from'`), id)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var recipients []store.MessageRecipient
	for rows.Next() {
		var recipient store.MessageRecipient
		require.NoError(t, rows.Scan(&recipient.ParticipantID, &recipient.DisplayName, &recipient.EnvelopeAddress))
		assert.Empty(t, recipient.EnvelopeAddress)
		recipients = append(recipients, recipient)
	}
	require.NoError(t, rows.Err())
	return recipients
}
func writerBody(t *testing.T, s *store.Store, id int64) string {
	t.Helper()
	body, _, err := s.GetMessageBodyAndRawContext(context.Background(), id)
	require.NoError(t, err)
	return body
}

func TestThreadsWriterSnapshotsAndParents(t *testing.T) {
	t.Run("shuffled nested replies and root", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		w, s := writerFixture(t)
		o := ItemOrigin{Kind: "conversation", RootID: "100"}
		child := applyWriter(t, w, writerReply("102", "100", "101"), o)
		parent := applyWriter(t, w, writerReply("101", "100", "100"), o)
		root := applyWriter(t, w, writerMedia("100", "Root title"), ItemOrigin{Kind: "own_posts"})
		require.NoError(w.ResolveParents(context.Background()))
		for _, edge := range [][2]int64{{child, parent}, {parent, root}} {
			got, err := s.GetMessageReplyToMessageIDContext(context.Background(), edge[0])
			require.NoError(err)
			assert.Equal(sql.NullInt64{Int64: edge[1], Valid: true}, got)
		}
		assert.Equal(child, applyWriter(t, w, writerReply("102", "100", "101"), o))
		count, err := s.CountMessagesForSource(w.sourceID)
		require.NoError(err)
		assert.Equal(int64(3), count)
		meta := writerMetadata(t, s, root)
		assert.Equal("100", meta["root_id"])
		assert.Equal(true, meta["root_owned"])
		assert.Equal(false, writerMetadata(t, s, child)["root_owned"])
		msg, err := s.GetMessageContext(context.Background(), child)
		require.NoError(err)
		assert.Equal("100", msg.SourceConversationID)
		assert.Equal("Root title", msg.ConversationTitle)
		var members int
		require.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=? AND role='member'`), msg.ConversationID).Scan(&members))
		assert.Equal(3, members)
	})
	t.Run("partial external root filled in place", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		w, s := writerFixture(t)
		m := writerReply("201", "200", "200")
		id := applyWriter(t, w, m, ItemOrigin{Kind: "own_replies"})
		before, err := s.GetMessageContext(context.Background(), id)
		require.NoError(err)
		assert.Contains(before.ConversationTitle, "Partial")
		assert.True(before.IsFromMe)
		root := applyWriter(t, w, writerMedia("200", "Accessible external root"), ItemOrigin{Kind: "conversation", RootID: "200"})
		after, err := s.GetMessageContext(context.Background(), id)
		require.NoError(err)
		assert.Equal(before.ConversationID, after.ConversationID)
		assert.Equal("Accessible external root", after.ConversationTitle)
		assert.Equal(false, writerMetadata(t, s, root)["root_owned"])
	})
	t.Run("media quote and repost", func(t *testing.T) {
		assert := assert.New(t)
		w, s := writerFixture(t)
		m := writerMedia("300", "")
		m.MediaType = new("IMAGE")
		m.MediaURL = new("https://cdn.example.invalid/photo")
		id := applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"})
		assert.Empty(writerBody(t, s, id))
		quote := writerMedia("301", "quote")
		quote.IsQuotePost = new(true)
		quote.QuotedPost = &MediaReference{ID: "999"}
		qid := applyWriter(t, w, quote, ItemOrigin{Kind: "own_posts"})
		assert.Equal("301", writerMetadata(t, s, qid)["root_id"])
		quote.ID = "302"
		quote.RepostedPost = &MediaReference{ID: "999"}
		skipped, err := w.Apply(context.Background(), quote, ItemOrigin{Kind: "own_posts"})
		require.NoError(t, err)
		assert.Zero(skipped)
	})
	t.Run("reject incomplete and malformed graph", func(t *testing.T) {
		assert := assert.New(t)
		w, _ := writerFixture(t)
		_, err := w.Apply(context.Background(), writerMedia("400", "no root"), ItemOrigin{Kind: "own_replies"})
		assert.Error(err)
		cases := []struct {
			m Media
			o ItemOrigin
		}{
			{writerReply("401", "400", "401"), ItemOrigin{Kind: "conversation", RootID: "400"}},
			{writerReply("402", "999", "400"), ItemOrigin{Kind: "conversation", RootID: "400"}},
			{writerReply("403", "400", "400"), ItemOrigin{Kind: "own_posts"}},
			{writerMedia("bad", "x"), ItemOrigin{Kind: "own_posts"}},
		}
		for _, tc := range cases {
			_, err = w.Apply(context.Background(), tc.m, tc.o)
			assert.Error(err)
		}
		applyWriter(t, w, writerReply("410", "400", "411"), ItemOrigin{Kind: "conversation", RootID: "400"})
		_, err = w.Apply(context.Background(), writerReply("411", "400", "410"), ItemOrigin{Kind: "conversation", RootID: "400"})
		assert.Error(err)
		applyWriter(t, w, writerMedia("420", "other root"), ItemOrigin{Kind: "own_posts"})
		_, err = w.Apply(context.Background(), writerReply("421", "400", "420"), ItemOrigin{Kind: "conversation", RootID: "400"})
		assert.Error(err)
	})
}

func TestThreadsEditCrashRepairAndSparseSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w, s := writerFixture(t)
	m := writerMedia("500", "original")
	m.Username = new("author_a")
	m.Permalink = new("https://www.threads.com/@author_a/post/example")
	m.HasReplies = new(true)
	m.Raw = []byte(`{"id":"500","text":"original"}`)
	id := applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"})
	m.Text = new("edited")
	w.afterPersist = func() error { return errors.New("interrupted after snapshot") }
	_, err := w.Apply(context.Background(), m, ItemOrigin{Kind: "own_posts"})
	require.ErrorContains(err, "interrupted")
	assert.Equal("edited", writerBody(t, s, id))
	assert.Equal(true, writerMetadata(t, s, id)["text_changed"])
	var edited bool
	require.NoError(s.DB().QueryRow(s.Rebind(`SELECT is_edited FROM messages WHERE id=?`), id).Scan(&edited))
	assert.False(edited)
	w.afterPersist = nil
	applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"})
	require.NoError(s.DB().QueryRow(s.Rebind(`SELECT is_edited FROM messages WHERE id=?`), id).Scan(&edited))
	assert.True(edited)
	sparse := Media{ID: m.ID, Timestamp: m.Timestamp, HideStatus: new("hidden")}
	applyWriter(t, w, sparse, ItemOrigin{Kind: "own_posts"})
	meta := writerMetadata(t, s, id)
	assert.Equal(*m.Permalink, meta["permalink"])
	assert.Equal(true, meta["has_replies"])
	assert.Equal("edited", writerBody(t, s, id))
	sparse.Permalink = new("")
	sparse.HasReplies = new(false)
	applyWriter(t, w, sparse, ItemOrigin{Kind: "own_posts"})
	meta = writerMetadata(t, s, id)
	assert.Equal("", meta["permalink"])
	assert.Equal(false, meta["has_replies"])
	assert.Equal("hidden", meta["hide_status"])
	provisional := writerMedia("500", "provisional body")
	provisional.Timestamp = writerTime.Add(time.Hour)
	assert.Equal(id, applyWriter(t, w, provisional, ItemOrigin{Kind: "sent_provisional", RootID: "500"}))
	assert.Equal("edited", writerBody(t, s, id))
	msg, err := s.GetMessageContext(context.Background(), id)
	require.NoError(err)
	assert.Equal(writerTime, msg.SentAt.UTC())
	assert.Nil(msg.DeletedAt)
}

func TestThreadsEmptyRootCaptionReplacesStaleTitle(t *testing.T) {
	for _, prior := range []bool{false, true} {
		t.Run(fmt.Sprint(prior), func(t *testing.T) {
			w, st := writerFixture(t)
			applyWriter(t, w, writerReply("101", "100", "100"), ItemOrigin{Kind: "conversation", RootID: "100"})
			if prior {
				applyWriter(t, w, writerMedia("100", "Old root title"), ItemOrigin{Kind: "own_posts"})
			}
			applyWriter(t, w, writerMedia("100", ""), ItemOrigin{Kind: "own_posts"})
			var title string
			require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT title FROM conversations WHERE source_id=? AND source_conversation_id='100'`), w.sourceID).Scan(&title))
			assert.Equal(t, "Threads post", title)
		})
	}
}

// Hegel shrank this to a single root one nanosecond below an offset boundary.
func TestThreadsFrozenRootOffsetNanosecondRegression(t *testing.T) {
	for _, boundary := range []struct{ offset, nanos int }{{1, 0}, {60, 0}, {60, 123}} {
		t.Run(fmt.Sprint(boundary), func(t *testing.T) {
			w, st := writerFixture(t)
			floor := time.Date(2026, 10, 1, 12, 0, 0, boundary.nanos, time.FixedZone("synthetic", boundary.offset))
			for n, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
				m := writerMedia(fmt.Sprint(1000+n), "synthetic root")
				m.Timestamp = floor.Add(delta)
				applyWriter(t, w, m, ItemOrigin{Kind: "own_posts"})
			}
			upper, err := st.ThreadsOwnRootUpperKeyContext(t.Context(), w.sourceID)
			require.NoError(t, err)
			roots, err := st.ListThreadsOwnRootsContext(t.Context(), w.sourceID, 0, upper, floor, 3)
			require.NoError(t, err)
			got := []string{}
			for _, root := range roots {
				got = append(got, root.MediaID)
			}
			assert.Equal(t, []string{"1001", "1002"}, got)
		})
	}
}
