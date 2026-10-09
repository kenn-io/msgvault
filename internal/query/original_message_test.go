package query_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// originalMIME carries bytes a re-serializer would change: CRLF and bare LF
// line endings, 8-bit octets, a NUL, trailing whitespace and no final newline.
var originalMIME = []byte("From: Sender <sender@example.com>\r\n" +
	"To: Recipient <recipient@example.org>\r\n" +
	"Subject: Quarterly  report \r\n" +
	"Content-Type: text/plain; charset=latin1\r\n" +
	"\r\n" +
	"caf\xe9 \x00 line\n" +
	"trailing spaces   \r\n" +
	"no final newline")

func originalEngine(f *storetest.Fixture) *query.SQLiteEngine {
	if f.Store.IsPostgreSQL() {
		return query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	return query.NewSQLiteEngine(f.Store.DB())
}

func TestReadOriginalMessage(t *testing.T) {
	must := require.New(t)
	ctx := context.Background()
	f := storetest.New(t)
	engine := originalEngine(f)

	withRaw := f.NewMessage().WithSourceMessageID("provider-abc").WithSubject("Quarterly report").Create(t, f.Store)
	must.NoError(f.Store.UpsertMessageRaw(withRaw, originalMIME))
	calendarJSON := f.NewMessage().WithSourceMessageID("calendar-event").Create(t, f.Store)
	must.NoError(f.Store.UpsertMessageRawWithFormat(calendarJSON, []byte(`{"kind":"event"}`), "gcal_json"))
	noRaw := f.NewMessage().WithSourceMessageID("chat-only").Create(t, f.Store)
	dedupLoser := f.NewMessage().WithSourceMessageID("dedup-loser").Create(t, f.Store)
	must.NoError(f.Store.UpsertMessageRaw(dedupLoser, originalMIME))
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), dedupLoser)
	must.NoError(err)

	t.Run("by internal id returns exact bytes and provenance", func(t *testing.T) {
		checks := assert.New(t)
		got, err := engine.ReadOriginalMessage(ctx, query.MessageRef{ID: withRaw}, 0)
		require.NoError(t, err)
		checks.Equal(originalMIME, got.MIME)
		checks.Equal(withRaw, got.MessageID)
		checks.Equal("provider-abc", got.SourceMessageID)
		checks.Equal(f.Source.ID, got.SourceID)
		checks.Equal(f.ConvID, got.ConversationID)
		checks.Equal("default-thread", got.SourceConversationID)
		checks.Equal("test@example.com", got.Account)
		checks.Equal("gmail", got.SourceType)
		checks.Nil(got.LastSyncAt)
	})

	t.Run("by provider id", func(t *testing.T) {
		got, err := engine.ReadOriginalMessage(ctx, query.MessageRef{SourceMessageID: "provider-abc"}, 0)
		require.NoError(t, err)
		assert.Equal(t, withRaw, got.MessageID)
		assert.Equal(t, originalMIME, got.MIME)
	})

	t.Run("reports last sync time", func(t *testing.T) {
		must := require.New(t)
		syncedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
		_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE sources SET last_sync_at = ? WHERE id = ?`), syncedAt, f.Source.ID)
		must.NoError(err)
		got, err := engine.ReadOriginalMessage(ctx, query.MessageRef{ID: withRaw}, 0)
		must.NoError(err)
		must.NotNil(got.LastSyncAt)
		assert.True(t, syncedAt.Equal(*got.LastSyncAt), "last sync %v", got.LastSyncAt)
	})

	t.Run("numeric provider id never resolves an internal id", func(t *testing.T) {
		_, err := engine.ReadOriginalMessage(ctx, query.MessageRef{SourceMessageID: strconv.FormatInt(withRaw, 10)}, 0)
		require.ErrorIs(t, err, store.ErrMessageNotFound)
	})

	t.Run("non-MIME raw is unavailable", func(t *testing.T) {
		_, err := engine.ReadOriginalMessage(ctx, query.MessageRef{ID: calendarJSON}, 0)
		require.ErrorIs(t, err, query.ErrOriginalMIMEUnavailable)
	})

	t.Run("missing raw is unavailable", func(t *testing.T) {
		_, err := engine.ReadOriginalMessage(ctx, query.MessageRef{ID: noRaw}, 0)
		require.ErrorIs(t, err, query.ErrOriginalMIMEUnavailable)
	})

	t.Run("dedup loser is not found", func(t *testing.T) {
		_, err := engine.ReadOriginalMessage(ctx, query.MessageRef{ID: dedupLoser}, 0)
		require.ErrorIs(t, err, store.ErrMessageNotFound)
		_, err = engine.ReadOriginalMessage(ctx, query.MessageRef{SourceMessageID: "dedup-loser"}, 0)
		require.ErrorIs(t, err, store.ErrMessageNotFound)
	})

	t.Run("unknown message is not found", func(t *testing.T) {
		_, err := engine.ReadOriginalMessage(ctx, query.MessageRef{ID: 999999}, 0)
		require.ErrorIs(t, err, store.ErrMessageNotFound)
	})

	t.Run("reference needs exactly one identifier", func(t *testing.T) {
		_, err := engine.ReadOriginalMessage(ctx, query.MessageRef{}, 0)
		require.ErrorIs(t, err, query.ErrInvalidMessageRef)
		_, err = engine.ReadOriginalMessage(ctx, query.MessageRef{ID: withRaw, SourceMessageID: "provider-abc"}, 0)
		require.ErrorIs(t, err, query.ErrInvalidMessageRef)
	})

	t.Run("account narrows an internal id", func(t *testing.T) {
		_, err := engine.ReadOriginalMessage(ctx, query.MessageRef{ID: withRaw, Account: "other@example.com"}, 0)
		require.ErrorIs(t, err, store.ErrMessageNotFound)
	})
}

func TestReadOriginalMessageBounded(t *testing.T) {
	f := storetest.New(t)
	engine := originalEngine(f)
	incompressible := make([]byte, 4096)
	_, err := rand.Read(incompressible)
	require.NoError(t, err)
	for _, tc := range []struct {
		name       string
		raw        []byte
		compressed bool
	}{
		{name: "small zlib", raw: []byte("a"), compressed: true},
		{name: "incompressible zlib", raw: incompressible, compressed: true},
		{name: "compressible zlib", raw: bytes.Repeat([]byte("a"), 65536), compressed: true},
		{name: "uncompressed", raw: originalMIME},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			id := f.NewMessage().Create(t, f.Store)
			must.NoError(f.Store.UpsertMessageRaw(id, tc.raw))
			if !tc.compressed {
				_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?`), tc.raw, id)
				must.NoError(err)
			}
			ref := query.MessageRef{ID: id}
			for _, limit := range []int64{0, int64(len(tc.raw)), math.MaxInt64 - 1} {
				got, err := engine.ReadOriginalMessage(t.Context(), ref, limit)
				must.NoError(err)
				checks.Equal(tc.raw, got.MIME)
			}
			if len(tc.raw) > 1 {
				got, err := engine.ReadOriginalMessage(t.Context(), ref, int64(len(tc.raw)-1))
				must.ErrorIs(err, query.ErrOriginalMessageTooLarge)
				checks.Nil(got)
			}
			for _, limit := range []int64{-1, math.MaxInt64} {
				_, err := engine.ReadOriginalMessage(t.Context(), ref, limit)
				must.Error(err)
			}
		})
	}
}

func TestReadOriginalMessageBoundedCorruption(t *testing.T) {
	must := require.New(t)
	f := storetest.New(t)
	engine := originalEngine(f)
	id := f.NewMessage().Create(t, f.Store)
	must.NoError(f.Store.UpsertMessageRaw(id, bytes.Repeat([]byte("a"), 65536)))
	var compressed []byte
	must.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT raw_data FROM message_raw WHERE message_id = ?`), id).Scan(&compressed))
	compressed[len(compressed)-1] ^= 1 // Invalid checksum after the decoded size limit.
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), compressed, id)
	must.NoError(err)
	ref := query.MessageRef{ID: id}
	_, err = engine.ReadOriginalMessage(t.Context(), ref, 1024)
	must.ErrorIs(err, query.ErrOriginalMessageTooLarge)
	_, err = engine.ReadOriginalMessage(t.Context(), ref, 65536)
	must.ErrorIs(err, zlib.ErrChecksum)
	_, err = engine.ReadOriginalMessage(t.Context(), ref, 0)
	must.ErrorIs(err, zlib.ErrChecksum)

	// Reject a stored blob beyond the input budget before attempting to decode it.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), bytes.Repeat([]byte("x"), 4096), id)
	must.NoError(err)
	_, err = engine.ReadOriginalMessage(t.Context(), ref, 1)
	must.ErrorIs(err, query.ErrOriginalMessageTooLarge)
	_, err = engine.ReadOriginalMessage(t.Context(), ref, 0)
	must.ErrorIs(err, zlib.ErrHeader)
}

func TestReadOriginalMessageAllowsNullSourceMessageID(t *testing.T) {
	must := require.New(t)
	f := storetest.New(t)
	engine := originalEngine(f)

	id := f.NewMessage().WithSourceMessageID("temporary-provider-id").Create(t, f.Store)
	must.NoError(f.Store.UpsertMessageRaw(id, originalMIME))
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET source_message_id = NULL WHERE id = ?`), id)
	must.NoError(err)

	got, err := engine.ReadOriginalMessage(context.Background(), query.MessageRef{ID: id}, 0)
	must.NoError(err)
	must.Empty(got.SourceMessageID)
}

func TestReadOriginalMessageAmbiguousProviderID(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	ctx := context.Background()
	f := storetest.New(t)
	engine := originalEngine(f)

	first := f.NewMessage().WithSourceMessageID("shared-id").Create(t, f.Store)
	must.NoError(f.Store.UpsertMessageRaw(first, []byte("first\r\n")))

	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	must.NoError(err)
	otherConv, err := f.Store.EnsureConversation(other.ID, "other-thread", "Other")
	must.NoError(err)
	second := storetest.NewMessage(other.ID, otherConv).WithSourceMessageID("shared-id").Create(t, f.Store)
	must.NoError(f.Store.UpsertMessageRaw(second, []byte("second\r\n")))

	_, err = engine.ReadOriginalMessage(ctx, query.MessageRef{SourceMessageID: "shared-id"}, 0)
	var ambiguous *query.AmbiguousError
	must.ErrorAs(err, &ambiguous)
	checks.Equal([]string{"other@example.com", "test@example.com"}, ambiguous.Accounts)

	got, err := engine.ReadOriginalMessage(ctx, query.MessageRef{SourceMessageID: "shared-id", Account: "other@example.com"}, 0)
	must.NoError(err)
	checks.Equal(second, got.MessageID)
	checks.Equal([]byte("second\r\n"), got.MIME)
}

func TestListThread(t *testing.T) {
	must := require.New(t)
	ctx := context.Background()
	f := storetest.New(t)
	engine := originalEngine(f)

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	undated := f.NewMessage().WithSourceMessageID("undated").Create(t, f.Store)
	third := f.NewMessage().WithSourceMessageID("third").WithSentAt(base.Add(2*time.Hour)).Create(t, f.Store)
	firstA := f.NewMessage().WithSourceMessageID("first-a").WithSubject("Kickoff").WithSentAt(base).Create(t, f.Store)
	firstB := f.NewMessage().WithSourceMessageID("first-b").WithSentAt(base).Create(t, f.Store)
	must.NoError(f.Store.UpsertMessageRaw(firstA, []byte("a\r\n")))
	must.NoError(f.Store.UpsertMessageRawWithFormat(third, []byte(`{}`), "gcal_json"))
	loser := f.NewMessage().WithSourceMessageID("loser").WithSentAt(base.Add(time.Hour)).Create(t, f.Store)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), loser)
	must.NoError(err)
	sourceDeleted := f.NewMessage().WithSourceMessageID("source-deleted").WithSentAt(base.Add(90*time.Minute)).Create(t, f.Store)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), sourceDeleted)
	must.NoError(err)

	sender := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	recipient := f.EnsureParticipant("recipient@example.org", "Recipient", "example.org")
	fallbackSender := f.EnsureParticipant("fallback@example.com", "Fallback Sender", "example.com")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sender_id = ? WHERE id IN (?, ?)`),
		fallbackSender, firstA, firstB)
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(firstA, "from", []int64{sender}, []string{"Sender"}))
	must.NoError(f.Store.ReplaceMessageRecipients(firstA, "to", []int64{recipient}, []string{"Recipient"}))

	ids := func(page *query.ThreadPage) []int64 {
		out := make([]int64, 0, len(page.Messages))
		for _, m := range page.Messages {
			out = append(out, m.ID)
		}
		return out
	}

	t.Run("anchored by message lists live messages chronologically", func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		page, err := engine.ListThread(ctx, query.ThreadQuery{ID: third})
		must.NoError(err)
		checks.Equal([]int64{firstA, firstB, sourceDeleted, third, undated}, ids(page))
		checks.Equal(int64(5), page.Total)
		checks.False(page.HasMore)
		checks.Equal(third, page.MessageID)
		checks.Equal(f.ConvID, page.ConversationID)
		checks.Equal("default-thread", page.SourceConversationID)
		checks.Equal("test@example.com", page.Account)

		first := page.Messages[0]
		checks.True(first.HasRaw, "mime raw stored")
		checks.Equal("Kickoff", first.Subject)
		must.NotNil(first.SentAt)
		checks.True(base.Equal(*first.SentAt))
		checks.Equal([]query.Address{{Email: "sender@example.com", Name: "Sender"}}, first.From)
		checks.Equal([]query.Address{{Email: "recipient@example.org", Name: "Recipient"}}, first.To)
		checks.Equal([]query.Address{{Email: "fallback@example.com", Name: "Fallback Sender"}}, page.Messages[1].From)
		checks.False(page.Messages[3].HasRaw, "non-MIME raw is not original MIME")
		checks.NotNil(page.Messages[2].DeletedFromSourceAt)
		checks.Nil(page.Messages[4].SentAt)
	})

	t.Run("pages", func(t *testing.T) {
		checks := assert.New(t)
		page, err := engine.ListThread(ctx, query.ThreadQuery{ThreadID: "default-thread", Limit: 2, Offset: 2})
		require.NoError(t, err)
		checks.Equal([]int64{sourceDeleted, third}, ids(page))
		checks.Equal(int64(5), page.Total)
		checks.True(page.HasMore)
		checks.Equal(2, page.Offset)
		checks.Zero(page.MessageID, "no anchor for thread lookups")
	})

	t.Run("unknown thread", func(t *testing.T) {
		_, err := engine.ListThread(ctx, query.ThreadQuery{ThreadID: "missing"})
		require.ErrorIs(t, err, query.ErrThreadNotFound)
	})

	t.Run("needs exactly one anchor", func(t *testing.T) {
		_, err := engine.ListThread(ctx, query.ThreadQuery{})
		require.ErrorIs(t, err, query.ErrInvalidMessageRef)
		_, err = engine.ListThread(ctx, query.ThreadQuery{ThreadID: "default-thread", ID: third})
		require.ErrorIs(t, err, query.ErrInvalidMessageRef)
	})

	t.Run("thread id in two accounts is ambiguous", func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
		must.NoError(err)
		otherConv, err := f.Store.EnsureConversation(other.ID, "default-thread", "Other")
		must.NoError(err)
		otherMsg := storetest.NewMessage(other.ID, otherConv).Create(t, f.Store)

		_, err = engine.ListThread(ctx, query.ThreadQuery{ThreadID: "default-thread"})
		var ambiguous *query.AmbiguousError
		must.ErrorAs(err, &ambiguous)
		checks.Equal([]string{"other@example.com", "test@example.com"}, ambiguous.Accounts)

		page, err := engine.ListThread(ctx, query.ThreadQuery{ThreadID: "default-thread", Account: "other@example.com"})
		must.NoError(err)
		checks.Equal([]int64{otherMsg}, ids(page))
	})
}

func TestListThreadAllowsNullSourceMessageID(t *testing.T) {
	must := require.New(t)
	f := storetest.New(t)
	engine := originalEngine(f)
	id := f.NewMessage().WithSourceMessageID("temporary-provider-id").Create(t, f.Store)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET source_message_id = NULL WHERE id = ?`), id)
	must.NoError(err)

	page, err := engine.ListThread(context.Background(), query.ThreadQuery{ThreadID: "default-thread"})
	must.NoError(err)
	must.Len(page.Messages, 1)
	must.Equal(id, page.Messages[0].ID)
	must.Empty(page.Messages[0].SourceMessageID)
}

func TestListThreadRequiresVisibleMessages(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	engine := originalEngine(f)
	ctx := t.Context()

	_, err := engine.ListThread(ctx, query.ThreadQuery{ThreadID: "default-thread"})
	must.ErrorIs(err, query.ErrThreadNotFound, "empty conversations are not exportable threads")

	survivor := f.CreateMessage("survivor")
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	must.NoError(err)
	otherConv, err := f.Store.EnsureConversation(other.ID, "default-thread", "Duplicate")
	must.NoError(err)
	duplicate := storetest.NewMessage(other.ID, otherConv).WithSourceMessageID("duplicate").Create(t, f.Store)
	_, err = f.Store.MergeDuplicates(survivor, []int64{duplicate}, "thread-visibility")
	must.NoError(err)

	_, err = engine.ListThread(ctx, query.ThreadQuery{ThreadID: "default-thread", Account: "other@example.com"})
	must.ErrorIs(err, query.ErrThreadNotFound, "hidden-only conversations are not exportable threads")

	must.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "survivor"))
	page, err := engine.ListThread(ctx, query.ThreadQuery{ThreadID: "default-thread"})
	must.NoError(err, "hidden-only conversations must not make the visible thread ambiguous")
	must.Len(page.Messages, 1)
	checks.Equal(survivor, page.Messages[0].ID)
	checks.NotNil(page.Messages[0].DeletedFromSourceAt, "source-deleted messages remain archived")
}

func TestListThreadAll(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	ids := f.CreateMessages(501)
	sender := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	recipient := f.EnsureParticipant("recipient@example.com", "Recipient", "example.com")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), sender, ids[500])
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(ids[500], "to", []int64{recipient}, []string{"Recipient"}))

	if !f.Store.IsPostgreSQL() {
		// Keep this archive small while exercising the database's real bind limit.
		f.Store.DB().SetMaxOpenConns(1)
		conn, err := f.Store.DB().Conn(t.Context())
		must.NoError(err)
		must.NoError(conn.Raw(func(driverConn any) error {
			sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
			must.True(ok)
			sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 1000)
			return nil
		}))
		must.NoError(conn.Close())
	}

	page, err := originalEngine(f).ListThread(t.Context(), query.ThreadQuery{
		ThreadID: "default-thread", All: true, Limit: 1, Offset: 1,
	})
	must.NoError(err)
	must.Len(page.Messages, 501)
	checks.Equal(int64(501), page.Total)
	checks.False(page.HasMore)
	checks.Zero(page.Offset)
	for i, message := range page.Messages {
		checks.Equal(ids[i], message.ID)
	}
	checks.Equal([]query.Address{{Email: "sender@example.com", Name: "Sender"}}, page.Messages[500].From)
	checks.Equal([]query.Address{{Email: "recipient@example.com", Name: "Recipient"}}, page.Messages[500].To)
}

func TestResolveMessageID(t *testing.T) {
	must := require.New(t)
	f := storetest.New(t)
	engine := originalEngine(f)
	internal := f.CreateMessage("internal")
	f.CreateMessage(strconv.FormatInt(internal, 10))
	numeric := f.CreateMessage("999999")
	deleted := f.CreateMessage("source-deleted")
	loser := f.CreateMessage("dedup-loser")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), deleted)
	must.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), loser)
	must.NoError(err)
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	must.NoError(err)
	f.CreateMessage("shared")
	convID, err := f.Store.EnsureConversation(other.ID, "other-thread", "Other")
	must.NoError(err)
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: convID, SourceMessageID: "shared", MessageType: "email"})
	must.NoError(err)
	for _, tc := range []struct {
		name, ref string
		want      int64
		wantErr   error
	}{
		{name: "internal before provider", ref: strconv.FormatInt(internal, 10), want: internal},
		{name: "numeric provider fallback", ref: "999999", want: numeric},
		{name: "source-deleted retained", ref: "source-deleted", want: deleted},
		{name: "dedup internal hidden", ref: strconv.FormatInt(loser, 10), wantErr: store.ErrMessageNotFound},
		{name: "ambiguous provider", ref: "shared", wantErr: query.ErrAmbiguousReference},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			id, _, err := engine.ResolveMessageID(t.Context(), tc.ref)
			if tc.wantErr != nil {
				must.ErrorIs(err, tc.wantErr)
				return
			}
			must.NoError(err)
			assert.Equal(t, tc.want, id)
		})
	}
}
