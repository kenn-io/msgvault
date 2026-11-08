package messageimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImporterCreatesMessageAndSyncRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	sourceHookCalls := 0
	var cacheLabels []string
	importer := NewImporter(st, Hooks{
		AfterSourceSetup: func() error {
			sourceHookCalls++
			return nil
		},
		RefreshCache: func(_ context.Context, label string) error {
			cacheLabels = append(cacheLabels, label)
			return nil
		},
	})

	result, err := importer.Import(t.Context(), validRequest(t))
	require.NoError(err)

	assert.Equal(StatusCreated, result.Status)
	assert.Equal("entry-42", result.SourceMessageID)
	assert.NotZero(result.SourceID)
	assert.NotZero(result.MessageID)
	assert.Equal(1, sourceHookCalls)
	assert.Equal([]string{"message_import:feeds"}, cacheLabels)

	var sourceType, identifier, displayName string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT source_type, identifier, display_name FROM sources WHERE id = ?
	`), result.SourceID).Scan(&sourceType, &identifier, &displayName))
	assert.Equal(SourceType, sourceType)
	assert.Equal("feeds", identifier)
	assert.Equal("Feeds", displayName)

	identities, err := st.ListAccountIdentities(result.SourceID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("user@example.com", identities[0].Address)

	var (
		messageType, sourceMessageID, subject, snippet string
		sentAt                                         time.Time
		isFromMe                                       bool
		senderName                                     sql.NullString
		identifierType, identifierValue                string
		conversationType, conversationKey, convTitle   string
		bodyText, rawFormat, metadataJSON              string
		bodyHTML                                       sql.NullString
		messageCount                                   int
	)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT m.message_type, m.source_message_id, m.subject, m.snippet, m.sent_at, m.is_from_me,
		       p.display_name, pi.identifier_type, pi.identifier_value,
		       c.conversation_type, c.source_conversation_id, c.title, c.message_count,
		       mb.body_text, mb.body_html, mr.raw_format, m.metadata
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN participants p ON p.id = m.sender_id
		JOIN participant_identifiers pi ON pi.participant_id = p.id
		JOIN message_bodies mb ON mb.message_id = m.id
		JOIN message_raw mr ON mr.message_id = m.id
		WHERE m.id = ?
	`), result.MessageID).Scan(
		&messageType, &sourceMessageID, &subject, &snippet, &sentAt, &isFromMe,
		&senderName, &identifierType, &identifierValue,
		&conversationType, &conversationKey, &convTitle, &messageCount,
		&bodyText, &bodyHTML, &rawFormat, &metadataJSON,
	))
	assert.Equal("feed_item", messageType)
	assert.Equal("entry-42", sourceMessageID)
	assert.Equal("Example Project 1.2.0", subject)
	assert.Equal("## Changes\n\n- Faster widgetsync for everyone.\n\nhttps://example.com/releases/1.2.0", snippet)
	assert.True(sentAt.Equal(time.Date(2026, time.November, 7, 15, 0, 0, 0, time.UTC)))
	assert.False(isFromMe)
	assert.Equal("Example Project", senderName.String)
	assert.Equal(SenderIdentifierType, identifierType)
	assert.Equal("url:https://example.com/blog", identifierValue)
	assert.Equal(ConversationType, conversationType)
	assert.Equal("feed-7", conversationKey)
	assert.Equal("Example Project releases", convTitle)
	assert.Equal(1, messageCount)
	assert.Contains(bodyText, "Faster widgetsync")
	assert.False(bodyHTML.Valid)
	assert.Equal(RawFormat, rawFormat)

	var metadata map[string]any
	require.NoError(json.Unmarshal([]byte(metadataJSON), &metadata))
	assert.Equal(SourceType, metadata["platform"])

	latest, err := st.GetLatestSync(result.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Equal(int64(1), latest.MessagesAdded)
	assert.Equal(int64(0), latest.MessagesUpdated)
}

func TestImporterUnchangedRetryKeepsTheStoredMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	cacheCalls := 0
	importer := NewImporter(st, Hooks{
		RefreshCache: func(context.Context, string) error {
			cacheCalls++
			return nil
		},
	})

	first, err := importer.Import(t.Context(), validRequest(t))
	require.NoError(err)

	sentinel := time.Date(2000, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE messages SET last_modified = ? WHERE id = ?
	`), sentinel, first.MessageID)
	require.NoError(err)
	var watermarkBefore string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?
	`), first.MessageID).Scan(&watermarkBefore))

	retry, err := importer.Import(t.Context(), validRequest(t))
	require.NoError(err)
	assert.Equal(StatusUpdated, retry.Status)
	assert.Equal(first.MessageID, retry.MessageID)

	var watermarkAfter string
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?
	`), first.MessageID).Scan(&watermarkAfter))
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COUNT(*) FROM messages WHERE source_id = ?
	`), first.SourceID).Scan(&count))
	assert.Equal(watermarkBefore, watermarkAfter, "an unchanged retry writes nothing")
	assert.Equal(1, count)

	latest, err := st.GetLatestSync(first.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Equal(int64(0), latest.MessagesAdded)
	assert.Equal(int64(0), latest.MessagesUpdated)
	assert.Equal(2, cacheCalls)
}

func TestImporterReplacementUpdatesTheSameMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})

	first, err := importer.Import(t.Context(), validRequest(t))
	require.NoError(err)

	replacement := validRequest(t)
	replacement.Message.Title = "Example Project 1.2.1"
	replacement.Message.BodyMarkdown = ""
	replacement.Message.BodyHTML = "<p>Fixed the <b>gizmocrash</b>.</p>"
	updated, err := importer.Import(t.Context(), replacement)
	require.NoError(err)
	assert.Equal(StatusUpdated, updated.Status)
	assert.Equal(first.MessageID, updated.MessageID)

	var subject, bodyText string
	var bodyHTML sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT m.subject, mb.body_text, mb.body_html
		FROM messages m JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.id = ?
	`), first.MessageID).Scan(&subject, &bodyText, &bodyHTML))
	assert.Equal("Example Project 1.2.1", subject)
	assert.Equal("Fixed the gizmocrash.\n\nhttps://example.com/releases/1.2.0", bodyText)
	assert.Contains(bodyHTML.String, "<b>gizmocrash</b>")

	latest, err := st.GetLatestSync(first.SourceID)
	require.NoError(err)
	assert.Equal(int64(0), latest.MessagesAdded)
	assert.Equal(int64(1), latest.MessagesUpdated)
}

// Keyword search finds an imported message right after import, by its type
// as well as its words, and a search-index rebuild derives the same entry.
func TestImporterMakesMessagesSearchable(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	importer := NewImporter(st, Hooks{})

	req := validRequest(t)
	req.Message.URL = "https://changelog.example.net/1.2.0"
	first, err := importer.Import(t.Context(), req)
	require.NoError(err)

	searchIDs := func(q *search.Query) []int64 {
		t.Helper()
		ids, _, err := st.SearchMessageIDsQueryContext(t.Context(), q, 10)
		require.NoError(err)
		return ids
	}
	assertFound := func(stage string) {
		t.Helper()
		assert.Equal(t, []int64{first.MessageID}, searchIDs(&search.Query{TextTerms: []string{"widgetsync"}}), "%s: body", stage)
		assert.Equal(t, []int64{first.MessageID}, searchIDs(&search.Query{TextTerms: []string{"Project"}}), "%s: title", stage)
		// PostgreSQL indexes a URL's host as one lexeme, so its first label is
		// the URL word both backends match.
		assert.Equal(t, []int64{first.MessageID}, searchIDs(&search.Query{TextTerms: []string{"changelog"}}), "%s: url", stage)
		assert.Equal(t, []int64{first.MessageID}, searchIDs(&search.Query{
			TextTerms: []string{"widgetsync"}, MessageTypes: []string{"feed_item"},
		}), "%s: type filter", stage)
		assert.Empty(t, searchIDs(&search.Query{
			TextTerms: []string{"widgetsync"}, MessageTypes: []string{"email"},
		}), "%s: other type", stage)
	}

	assertFound("after import")
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	assertFound("after rebuild")

	replacement := req
	replacement.Message.BodyMarkdown = "Now about gizmocrash."
	_, err = importer.Import(t.Context(), replacement)
	require.NoError(err)
	assert.Equal(t, []int64{first.MessageID}, searchIDs(&search.Query{TextTerms: []string{"gizmocrash"}}))
	assert.Empty(t, searchIDs(&search.Query{TextTerms: []string{"widgetsync"}}), "the replaced body leaves the index")
}

func TestImporterIndexesWhatARebuildIndexes(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	// A participant known by an email and, from another source, a phone. A
	// rebuild indexes a non-email message's sender by phone, else email.
	participant, err := st.EnsureParticipantContext(t.Context(), "both@example.com", "Both", "example.com")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE participants SET phone_number = ? WHERE id = ?
	`), "+15550100777", participant)
	require.NoError(err)

	req := validRequest(t)
	req.Message.Sender = MessageImportSender{Name: "Both", Email: "both@example.com"}
	result, err := NewImporter(st, Hooks{}).Import(t.Context(), req)
	require.NoError(err)

	indexed := func() string {
		t.Helper()
		query := `SELECT subject || char(10) || body || char(10) || from_addr || char(10) ||
			to_addr || char(10) || cc_addr FROM messages_fts WHERE rowid = ?`
		if st.IsPostgreSQL() {
			query = `SELECT search_fts::text FROM messages WHERE id = ?`
		}
		var entry string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(query), result.MessageID).Scan(&entry))
		return entry
	}
	afterImport := indexed()
	assert.Contains(t, afterImport, "15550100777", "the sender is indexed by phone")
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	assert.Equal(t, indexed(), afterImport, "a rebuild changes nothing")
}

func TestImporterArchivesABodyTooDenseToIndexWhole(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	// 300,000 distinct two-character CJK words, about 2 MB: inside the
	// import's 16 MiB limit, but past PostgreSQL's 1 MB tsvector limit if
	// indexed whole.
	var body strings.Builder
	for i := range 300_000 {
		body.WriteRune(rune(0x4E00 + i/2000))
		body.WriteRune(rune(0x4E00 + i%2000))
		body.WriteByte(' ')
	}
	req := validRequest(t)
	req.Message.BodyMarkdown = body.String()

	result, err := NewImporter(st, Hooks{}).Import(t.Context(), req)
	require.NoError(err, "the message is archived, its index bounded")
	ids, _, err := st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"一丁"}}, 10)
	require.NoError(err)
	assert.Equal(t, []int64{result.MessageID}, ids, "the indexed part is searchable")
}

func TestImporterScopesExternalIDsBySource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})

	feeds, err := importer.Import(t.Context(), validRequest(t))
	require.NoError(err)
	other := validRequest(t)
	other.Source.Identifier = "bookmarks"
	bookmarks, err := importer.Import(t.Context(), other)
	require.NoError(err)

	assert.Equal(StatusCreated, bookmarks.Status)
	assert.NotEqual(feeds.SourceID, bookmarks.SourceID)
	assert.NotEqual(feeds.MessageID, bookmarks.MessageID)
}

func TestImporterResolvesSenders(t *testing.T) {
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})

	senderOf := func(t *testing.T, sourceIdentifier, externalID string, sender MessageImportSender) (int64, bool) {
		t.Helper()
		req := validRequest(t)
		req.Source.Identifier = sourceIdentifier
		req.Message.ExternalID = externalID
		req.Message.Sender = sender
		result, err := importer.Import(t.Context(), req)
		require.NoError(t, err)
		var senderID int64
		var isFromMe bool
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`
			SELECT sender_id, is_from_me FROM messages WHERE id = ?
		`), result.MessageID).Scan(&senderID, &isFromMe))
		return senderID, isFromMe
	}

	t.Run("one site is one sender across sources", func(t *testing.T) {
		first, _ := senderOf(t, "feeds", "u1", MessageImportSender{Name: "Site", URL: "https://site.example/"})
		second, _ := senderOf(t, "bookmarks", "u2", MessageImportSender{URL: "https://SITE.example/"})
		assert.Equal(t, first, second)
	})
	t.Run("an id names a sender only inside its source", func(t *testing.T) {
		first, _ := senderOf(t, "feeds", "i1", MessageImportSender{ID: "author-7", Name: "Author"})
		again, _ := senderOf(t, "feeds", "i2", MessageImportSender{ID: "author-7"})
		elsewhere, _ := senderOf(t, "bookmarks", "i3", MessageImportSender{ID: "author-7", Name: "Author"})
		assert.Equal(t, first, again)
		assert.NotEqual(t, first, elsewhere)
	})
	t.Run("a name names a sender only inside its source", func(t *testing.T) {
		first, _ := senderOf(t, "feeds", "n1", MessageImportSender{Name: "Release bot"})
		elsewhere, _ := senderOf(t, "bookmarks", "n2", MessageImportSender{Name: "Release bot"})
		assert.NotEqual(t, first, elsewhere)
	})
	t.Run("an email joins that address's participant", func(t *testing.T) {
		existing, err := st.EnsureParticipantContext(t.Context(), "author@example.com", "Author", "example.com")
		require.NoError(t, err)
		sender, fromMe := senderOf(t, "feeds", "e1", MessageImportSender{Email: "Author@example.com", ID: "author-9"})
		assert.Equal(t, existing, sender)
		assert.False(t, fromMe)
	})
	t.Run("the account email marks a message as sent by the owner", func(t *testing.T) {
		_, fromMe := senderOf(t, "feeds", "e2", MessageImportSender{Email: "user@example.com"})
		assert.True(t, fromMe)
	})
	t.Run("a phone joins that number's participant", func(t *testing.T) {
		existing, err := st.EnsurePhoneParticipantContext(t.Context(), "+15550100123", "Texter")
		require.NoError(t, err)
		sender, _ := senderOf(t, "feeds", "p1", MessageImportSender{Phone: "+1 555 010 0123"})
		assert.Equal(t, existing, sender)
	})
}

func TestImporterMovesMessageBetweenConversations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})

	first, err := importer.Import(t.Context(), validRequest(t))
	require.NoError(err)
	moved := validRequest(t)
	moved.Message.Conversation.ExternalID = "feed-8"
	_, err = importer.Import(t.Context(), moved)
	require.NoError(err)

	counts := map[string]int{}
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT source_conversation_id, message_count FROM conversations WHERE source_id = ?
	`), first.SourceID)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var key string
		var count int
		require.NoError(rows.Scan(&key, &count))
		counts[key] = count
	}
	require.NoError(rows.Err())
	assert.Equal(map[string]int{"feed-7": 0, "feed-8": 1}, counts)
}

func TestImporterReturnsValidationErrorsBeforeWriting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	sourceHookCalls := 0
	importer := NewImporter(st, Hooks{
		AfterSourceSetup: func() error {
			sourceHookCalls++
			return nil
		},
	})
	req := validRequest(t)
	req.Message.MessageType = "Feed Item"

	_, err := importer.Import(t.Context(), req)
	require.ErrorIs(err, ErrValidation)
	var sources int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COUNT(*) FROM sources WHERE source_type = ?
	`), SourceType).Scan(&sources))
	assert.Equal(0, sources)
	assert.Equal(0, sourceHookCalls)
}

func TestImporterSucceedsWhenCacheRefreshFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{
		RefreshCache: func(context.Context, string) error { return errors.New("synthetic cache failure") },
	})

	result, err := importer.Import(t.Context(), validRequest(t))
	require.NoError(err)
	assert.Equal(StatusCreated, result.Status)

	latest, err := st.GetLatestSync(result.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
}

func TestImporterUnavailableWithoutStore(t *testing.T) {
	t.Parallel()
	_, err := NewImporter(nil, Hooks{}).Import(t.Context(), Request{})
	require.ErrorIs(t, err, ErrUnavailable)
}
