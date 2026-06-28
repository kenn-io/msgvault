package teams

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type recordingEnqueuer struct {
	ids []int64
}

func (e *recordingEnqueuer) EnqueueMessages(_ context.Context, ids []int64) error {
	e.ids = append(e.ids, ids...)
	return nil
}

func fakeChatGraph(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[
			  {"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z",
			   "from":{"user":{"id":"alice@outlook.com","displayName":"Alice","userIdentityType":"emailUser"}},
			   "body":{"contentType":"text","content":"hello world"}}
			]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
}

func TestImportChatsEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := fakeChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false})
	require.NoError(err)
	assert.EqualValues(1, sum.ChatsProcessed)
	assert.EqualValues(1, sum.MessagesAdded)

	var cnt int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='teams'`).Scan(&cnt))
	assert.Equal(1, cnt)
}

func TestImportChatsPopulatesConversationParticipantsAndStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:stats@thread.v2","chatType":"group","topic":"Stats"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[
				{"id":"mem1","userId":"aad-alice","email":"alice@example.com","displayName":"Alice"},
				{"id":"mem2","userId":"aad-bob","email":"bob@example.com","displayName":"Bob"}
			]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[
				{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z",
				 "from":{"user":{"id":"alice@example.com","displayName":"Alice","userIdentityType":"emailUser"}},
				 "body":{"contentType":"text","content":"hello stats"}}
			]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false})
	require.NoError(err)

	var conversationID int64
	var messageCount, participantCount int
	var lastMessageAt sql.NullTime
	var preview sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id, message_count, participant_count, last_message_at, last_message_preview
		FROM conversations
		WHERE source_conversation_id = ?
	`), "19:stats@thread.v2").Scan(&conversationID, &messageCount, &participantCount, &lastMessageAt, &preview))
	assert.Equal(1, messageCount)
	assert.Equal(2, participantCount)
	assert.True(lastMessageAt.Valid)
	assert.Equal("hello stats", preview.String)

	var linkedParticipants int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ?
		  AND p.email_address IN ('alice@example.com', 'bob@example.com')
	`), conversationID).Scan(&linkedParticipants))
	assert.Equal(2, linkedParticipants)
}

func fakeChannelGraph(t *testing.T) *httptest.Server {
	t.Helper()

	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General","membershipType":"standard"}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			_, _ = w.Write([]byte(`{"value":[{"id":"c1","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z","body":{"contentType":"text","content":"channel post"}}],"@odata.deltaLink":"` + serverURL + `/delta?token=next"}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	return srv
}

func TestInlineImageDownloaded(t *testing.T) {
	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case r.URL.Path == "/me/chats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			body := `<div><img src="` + serverURL + `/chats/19:x@thread.v2/messages/m1/hostedContents/1/$value"></div>`
			_, _ = w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"html","content":` + jsonString(t, body) + `}}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)
	dir := t.TempDir()

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", AttachmentsDir: dir})
	require.NoError(t, err)
	assert.EqualValues(t, 1, sum.InlineImagesCopied)
	assert.EqualValues(t, 0, sum.Errors)
}

func TestContentlessGraphAttachmentDoesNotSetMessageAttachmentStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:card@thread.v2","chatType":"oneOnOne","topic":"Card"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[
				{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z",
				 "body":{"contentType":"text","content":"card"},
				 "attachments":[{"id":"card1","contentType":"application/vnd.microsoft.card.adaptive","name":"card"}]}
			]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
	})
	require.NoError(err)

	var hasAttachments bool
	var messageAttachmentCount int
	var actualAttachmentRows int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.has_attachments, m.attachment_count, COUNT(a.id)
		FROM messages m
		LEFT JOIN attachments a ON a.message_id = m.id
		WHERE m.source_message_id = ?
		GROUP BY m.id, m.has_attachments, m.attachment_count
	`), chatSourceMessageID("19:card@thread.v2", "m1")).Scan(&hasAttachments, &messageAttachmentCount, &actualAttachmentRows))
	assert.False(hasAttachments)
	assert.Equal(0, messageAttachmentCount)
	assert.Equal(0, actualAttachmentRows)
}

func TestTeamsReimportRemovesStaleInlineAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	includeImage := true
	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case r.URL.Path == "/me/chats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[{"id":"19:inline-edit@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			body := `<p>edited</p>`
			if includeImage {
				body = `<div><img src="` + serverURL + `/chats/19:inline-edit@thread.v2/messages/m1/hostedContents/1/$value"></div>`
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"html","content":` + jsonString(t, body) + `}}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)
	attachmentsDir := t.TempDir()

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		AttachmentsDir:  attachmentsDir,
		Full:            true,
	})
	require.NoError(err)

	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id FROM messages WHERE source_message_id = ?
	`), chatSourceMessageID("19:inline-edit@thread.v2", "m1")).Scan(&messageID))
	var attachmentCount int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`),
		messageID,
	).Scan(&attachmentCount))
	require.Equal(1, attachmentCount)

	includeImage = false
	_, err = imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		AttachmentsDir:  attachmentsDir,
		Full:            true,
	})
	require.NoError(err)

	var hasAttachments bool
	var denormalizedCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.has_attachments, m.attachment_count, COUNT(a.id)
		FROM messages m
		LEFT JOIN attachments a ON a.message_id = m.id
		WHERE m.id = ?
		GROUP BY m.id, m.has_attachments, m.attachment_count
	`), messageID).Scan(&hasAttachments, &denormalizedCount, &attachmentCount))
	assert.False(hasAttachments)
	assert.Equal(0, denormalizedCount)
	assert.Equal(0, attachmentCount)
}

func jsonString(t *testing.T, s string) string {
	t.Helper()

	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

// TestBackfillInlineMedia exercises the path fix end-to-end: it pre-seeds a
// message whose stored HTML body contains a hostedContents URL, then runs
// BackfillInlineMedia and asserts the inline image was fetched (with the
// correct non-doubled version path) and recorded as an attachment.
func TestBackfillInlineMedia(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Regression guard: a doubled version segment ("/v1.0/v1.0") must 404.
		if strings.Contains(r.URL.Path, "/v1.0/v1.0") {
			http.Error(w, "404", http.StatusNotFound)
			return
		}
		if r.URL.Path == "/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/1/$value" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
			return
		}
		http.Error(w, "404", http.StatusNotFound)
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)

	// Pre-seed: source, conversation, message, and an HTML body that contains a
	// hostedContents inline-image URL.
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "oneOnOne", "DM")
	require.NoError(err)
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "teams",
	})
	require.NoError(err)
	bodyHTML := `<div><img src="` + srv.URL + `/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/1/$value"></div>`
	require.NoError(st.UpsertMessageBody(msgID,
		sql.NullString{String: "hello", Valid: true},
		sql.NullString{String: bodyHTML, Valid: true}))

	// baseURL carries the version segment, exactly like production, so the fix
	// (stripping baseURL's path) is what makes the fetch path resolve correctly.
	client := NewClient(srv.URL+"/v1.0", func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, client)

	sum, err := imp.BackfillInlineMedia(context.Background(), ImportOptions{
		Email:          "me@example.com",
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	assert.EqualValues(1, sum.InlineImagesCopied)
	assert.EqualValues(0, sum.Errors)

	var attCount int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`),
		msgID,
	).Scan(&attCount))
	assert.Equal(1, attCount, "an inline-image attachment row should exist for the message")

	var hasAttachments bool
	var messageAttachmentCount int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT has_attachments, attachment_count FROM messages WHERE id = ?`),
		msgID,
	).Scan(&hasAttachments, &messageAttachmentCount))
	assert.True(hasAttachments, "backfill should refresh the message attachment flag")
	assert.Equal(1, messageAttachmentCount, "backfill should refresh the message attachment count")
}

// TestHostedFetchPath verifies that an absolute Graph hostedContents URL is
// rewritten to a path relative to the client baseURL WITHOUT duplicating the
// API-version segment. Production baseURL carries "/v1.0"; using u.Path
// verbatim would yield "/v1.0/v1.0/..." and 404 every inline fetch.
func TestHostedFetchPath(t *testing.T) {
	assert := assert.New(t)
	const hosted = "https://graph.microsoft.com/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/abc/$value"

	// Production: baseURL includes /v1.0 — the version must not be doubled.
	got := hostedFetchPath("https://graph.microsoft.com/v1.0", hosted)
	assert.Equal("/chats/19:x@thread.v2/messages/m1/hostedContents/abc/$value", got)
	assert.NotContains(got, "/v1.0", "version segment must be stripped, not doubled")

	// httptest: baseURL has no path — keep the full path so the fake server matches.
	gotTest := hostedFetchPath("http://127.0.0.1:1234", "http://127.0.0.1:1234/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/abc/$value")
	assert.Equal("/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/abc/$value", gotTest)

	// Query string is preserved.
	gotQ := hostedFetchPath("https://graph.microsoft.com/v1.0", hosted+"?foo=bar")
	assert.Equal("/chats/19:x@thread.v2/messages/m1/hostedContents/abc/$value?foo=bar", gotQ)

	malicious := "https://graph.microsoft.com/v1.0https://attacker.example/hostedContents/1/$value"
	assert.Empty(hostedFetchPath("https://graph.microsoft.com/v1.0", malicious))
}

// fakeLimitChatGraph returns a fake Graph server that serves a single chat
// with 3 messages, so the --limit flag can be tested against it.
func fakeLimitChatGraph(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:limit@thread.v2","chatType":"oneOnOne","topic":"LimitTest"}]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[
			  {"id":"lm1","createdDateTime":"2025-03-01T00:00:00Z","lastModifiedDateTime":"2025-03-01T00:00:00Z","body":{"contentType":"text","content":"msg one"}},
			  {"id":"lm2","createdDateTime":"2025-03-01T00:00:01Z","lastModifiedDateTime":"2025-03-01T00:00:01Z","body":{"contentType":"text","content":"msg two"}},
			  {"id":"lm3","createdDateTime":"2025-03-01T00:00:02Z","lastModifiedDateTime":"2025-03-01T00:00:02Z","body":{"contentType":"text","content":"msg three"}}
			]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
}

func TestImportChatsLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := fakeLimitChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)
	sum, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		Limit:           2,
	})
	require.NoError(err)
	assert.EqualValues(1, sum.ChatsProcessed)
	assert.EqualValues(2, sum.MessagesAdded)

	var cnt int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='teams'`).Scan(&cnt))
	assert.Equal(2, cnt)
}

func TestLimitedChatImportDoesNotAdvanceCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := fakeLimitChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		Limit:           2,
	})
	require.NoError(err)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	require.True(run.CursorAfter.Valid)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Empty(state.ChatCursor("19:limit@thread.v2"))
}

func TestLimitedChatImportStopsPaging(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	serverURL := ""
	var secondPageRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:paged-limit@thread.v2","chatType":"oneOnOne","topic":"Paged"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/chats/19:paged-limit@thread.v2/messages":
			_, _ = w.Write([]byte(`{"value":[
				{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"text","content":"one"}}
			],"@odata.nextLink":"` + serverURL + `/chat-page-2"}`))
		case r.URL.Path == "/chat-page-2":
			secondPageRequests++
			_, _ = w.Write([]byte(`{"value":[]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		Limit:           1,
	})
	require.NoError(err)
	assert.EqualValues(1, sum.MessagesAdded)
	assert.Equal(0, secondPageRequests)
}

func TestChatMemberFetchFailureDoesNotAdvanceCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:memberfail@thread.v2","chatType":"group","topic":"Chat"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			http.Error(w, "members unavailable", http.StatusBadRequest)
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"text","content":"hello"}}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
	})
	require.NoError(err)
	assert.EqualValues(1, sum.Errors)
	assert.EqualValues(1, sum.MessagesAdded)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	require.True(run.CursorAfter.Valid)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Empty(state.ChatCursor("19:memberfail@thread.v2"))
}

func TestRawArchiveFailureFailsImport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := fakeChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)
	installFailingTeamsRawArchiveTrigger(t, st)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
	})
	require.Error(err)
	assert.Contains(err.Error(), "archive teams message raw")

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	_, err = st.GetLastSuccessfulSync(src.ID)
	require.ErrorIs(err, store.ErrSyncRunNotFound)

	var failedRuns int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM sync_runs
		WHERE source_id = ? AND status = 'failed'
	`), src.ID).Scan(&failedRuns))
	assert.Equal(1, failedRuns)
}

func installFailingTeamsRawArchiveTrigger(t *testing.T, st *store.Store) {
	t.Helper()

	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`
			CREATE OR REPLACE FUNCTION fail_teams_raw_archive()
			RETURNS trigger AS $$
			BEGIN
				IF NEW.raw_format = 'teams_json' THEN
					RAISE EXCEPTION 'raw archive blocked';
				END IF;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql;

			CREATE TRIGGER fail_teams_raw_archive
			BEFORE INSERT ON message_raw
			FOR EACH ROW
			EXECUTE FUNCTION fail_teams_raw_archive();
		`)
	} else {
		_, err = st.DB().Exec(`
			CREATE TRIGGER fail_teams_raw_archive
			BEFORE INSERT ON message_raw
			WHEN NEW.raw_format = 'teams_json'
			BEGIN
				SELECT RAISE(ABORT, 'raw archive blocked');
			END
		`)
	}
	require.NoError(t, err)
}

func TestChatMessageIDsAreNamespacedByConversation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[` +
				`{"id":"chatA","chatType":"group","topic":"A"},` +
				`{"id":"chatB","chatType":"group","topic":"B"}` +
				`]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[{"id":"same","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"text","content":"hello"}}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false})
	require.NoError(err)

	var count int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id IN (?, ?)`),
		chatSourceMessageID("chatA", "same"), chatSourceMessageID("chatB", "same"),
	).Scan(&count))
	assert.Equal(2, count)
}

func TestImportChannelsEndToEnd(t *testing.T) {
	assert := assert.New(t)
	srv := fakeChannelGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: true})
	require.NoError(t, err)
	assert.EqualValues(1, sum.ChannelsProcessed)
	assert.EqualValues(1, sum.MessagesAdded)

	src, _ := st.GetOrCreateSource("teams", "me@example.com")
	prev, _ := st.GetLastSuccessfulSync(src.ID)
	state, _ := LoadSyncState(prev.CursorAfter.String)
	assert.Contains(state.ChannelDelta("team1/chanA"), "token=next")
}

func TestLimitedChannelImportDoesNotAdvanceDelta(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General"}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[` +
				`{"id":"c1","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z","body":{"contentType":"text","content":"one"}},` +
				`{"id":"c2","createdDateTime":"2025-02-01T00:00:01Z","lastModifiedDateTime":"2025-02-01T00:00:01Z","body":{"contentType":"text","content":"two"}}` +
				`]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			_, _ = w.Write([]byte(`{"value":[],"@odata.deltaLink":"` + serverURL + `/delta?token=next"}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: true, Limit: 1})
	require.NoError(err)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Empty(state.ChannelDelta("team1/chanA"))
}

func TestLimitedChannelImportStopsPagingAndDeltaPrime(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	serverURL := ""
	var secondPageRequests int
	var deltaRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General"}]}`))
		case r.URL.Path == "/teams/team1/channels/chanA/messages":
			_, _ = w.Write([]byte(`{"value":[
				{"id":"c1","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z","body":{"contentType":"text","content":"one"}}
			],"@odata.nextLink":"` + serverURL + `/channel-page-2"}`))
		case r.URL.Path == "/channel-page-2":
			secondPageRequests++
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/replies"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			deltaRequests++
			_, _ = w.Write([]byte(`{"value":[],"@odata.deltaLink":"` + serverURL + `/delta?token=next"}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: true, Limit: 1})
	require.NoError(err)
	assert.EqualValues(1, sum.MessagesAdded)
	assert.Equal(0, secondPageRequests)
	assert.Equal(0, deltaRequests)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Empty(state.ChannelDelta("team1/chanA"))
}

func TestChannelReplyFetchErrorDoesNotAdvanceDelta(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General"}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[{"id":"root","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z","body":{"contentType":"text","content":"root"}}]}`))
		case strings.Contains(r.URL.Path, "/replies"):
			http.Error(w, "reply failure", http.StatusBadRequest)
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			_, _ = w.Write([]byte(`{"value":[],"@odata.deltaLink":"` + serverURL + `/delta?token=next"}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: true})
	require.NoError(err)
	assert.EqualValues(1, sum.Errors)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Empty(state.ChannelDelta("team1/chanA"))
}

func TestChannelDeltaPrimeErrorStillPersistsBackfill(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General"}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[{"id":"root","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z","body":{"contentType":"text","content":"root"}}]}`))
		case strings.Contains(r.URL.Path, "/replies"):
			_, _ = w.Write([]byte(`{"value":[{"id":"reply","replyToId":"root","createdDateTime":"2025-02-01T00:00:01Z","lastModifiedDateTime":"2025-02-01T00:00:01Z","body":{"contentType":"text","content":"reply"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			http.Error(w, "delta unavailable", http.StatusBadRequest)
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: true})
	require.NoError(err)
	assert.EqualValues(1, sum.Errors)
	assert.EqualValues(2, sum.MessagesAdded)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Empty(state.ChannelDelta("team1/chanA"))

	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages
		WHERE source_id = ? AND source_message_id IN (?, ?)
	`), src.ID,
		channelSourceMessageID("team1", "chanA", "root"),
		channelSourceMessageID("team1", "chanA", "reply"),
	).Scan(&count))
	assert.Equal(2, count)
}

func TestChannelDeltaPrimeMessageReplacesBackfilledVersion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General"}]}`))
		case strings.Contains(r.URL.Path, "/replies"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			_, _ = w.Write([]byte(`{"value":[{"id":"root","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:02Z","body":{"contentType":"text","content":"edited root"}}],"@odata.deltaLink":"` + serverURL + `/delta?token=next"}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[{"id":"root","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z","body":{"contentType":"text","content":"original root"}}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: true})
	require.NoError(err)

	var bodyText string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text
		FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_message_id = ?
	`), channelSourceMessageID("team1", "chanA", "root")).Scan(&bodyText))
	assert.Equal("edited root", bodyText)
}

// TestReplyBeforeRoot verifies that channel reply threading is preserved even
// when a delta page returns a reply (c2) before its root (c1). The old
// inline-SetReplyTo approach would silently drop the link because the root
// was not yet persisted. The two-phase collect-then-link approach fixes this.
func TestReplyBeforeRoot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General","membershipType":"standard"}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			// Reply (c2) arrives BEFORE its root (c1) in a single delta page.
			_, _ = w.Write([]byte(`{"value":[` +
				`{"id":"c2","replyToId":"c1","createdDateTime":"2025-02-01T00:00:01Z","lastModifiedDateTime":"2025-02-01T00:00:01Z","body":{"contentType":"text","content":"a reply"}},` +
				`{"id":"c1","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z","body":{"contentType":"text","content":"the root"}}` +
				`],"@odata.deltaLink":"` + serverURL + `/delta?token=x"}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			// Backfill roots endpoint returns empty — all messages come via delta.
			_, _ = w.Write([]byte(`{"value":[]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: true})
	require.NoError(err)

	// The reply (c2) must be linked to the root (c1).
	var replyTo, rootID sql.NullInt64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT reply_to_message_id FROM messages WHERE source_message_id = ?`),
		channelSourceMessageID("team1", "chanA", "c2"),
	).Scan(&replyTo))
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`),
		channelSourceMessageID("team1", "chanA", "c1"),
	).Scan(&rootID))
	require.True(replyTo.Valid, "reply_to_message_id should be set on c2")
	assert.Equal(rootID.Int64, replyTo.Int64)
}

func TestRecipientAndMentionRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Fake Graph server:
	// - /me/chats → one oneOnOne chat
	// - /chats/{id}/members → two members: alice (sender) and bob
	// - chat /messages → one message from alice @mentioning bob
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:chat1@thread.v2","chatType":"oneOnOne","topic":"Chat"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[
                {"id":"mem1","userId":"aad-alice","email":"alice@x.com","displayName":"Alice"},
                {"id":"mem2","userId":"aad-bob","email":"bob@x.com","displayName":"Bob"}
            ]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[{
                "id":"msg1",
                "createdDateTime":"2025-01-01T00:00:00Z",
                "lastModifiedDateTime":"2025-01-01T00:00:00Z",
                "from":{"user":{"id":"alice@x.com","displayName":"Alice","userIdentityType":"emailUser"}},
                "body":{"contentType":"text","content":"hey @Bob"},
                "mentions":[{"id":0,"mentionText":"Bob","mentioned":{"user":{"id":"aad-bob","displayName":"Bob","userIdentityType":"aadUser"}}}]
            }]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)

	_, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false})
	require.NoError(err)

	// Get the message ID
	var msgID int64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`),
		chatSourceMessageID("19:chat1@thread.v2", "msg1"),
	).Scan(&msgID))

	// Should have a "to" row for bob but NOT alice (the sender)
	var toCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
        SELECT COUNT(*) FROM message_recipients mr
        JOIN participants p ON p.id = mr.participant_id
        WHERE mr.message_id = ? AND mr.recipient_type = 'to' AND p.email_address = 'bob@x.com'
    `), msgID).Scan(&toCount))
	assert.Equal(1, toCount, "should have a 'to' row for bob")

	var aliceToCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
        SELECT COUNT(*) FROM message_recipients mr
        JOIN participants p ON p.id = mr.participant_id
        WHERE mr.message_id = ? AND mr.recipient_type = 'to' AND p.email_address = 'alice@x.com'
    `), msgID).Scan(&aliceToCount))
	assert.Equal(0, aliceToCount, "alice is the sender so should NOT appear in 'to' rows")

	var fromCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
        SELECT COUNT(*) FROM message_recipients mr
        JOIN participants p ON p.id = mr.participant_id
        WHERE mr.message_id = ? AND mr.recipient_type = 'from' AND p.email_address = 'alice@x.com'
    `), msgID).Scan(&fromCount))
	assert.Equal(1, fromCount, "should have a 'from' row for the message sender")

	// Should have a "mention" row for bob
	var mentionCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
        SELECT COUNT(*) FROM message_recipients mr
        JOIN participants p ON p.id = mr.participant_id
        WHERE mr.message_id = ? AND mr.recipient_type = 'mention' AND p.email_address = 'bob@x.com'
    `), msgID).Scan(&mentionCount))
	assert.Equal(1, mentionCount, "should have a 'mention' row for bob")
}

// TestImportProgressCallback verifies that ImportOptions.Progress is called at least
// once per conversation and that the message contains the word "messages".
func TestImportProgressCallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := fakeChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	var lines []string
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)
	sum, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		Progress:        func(msg string) { lines = append(lines, msg) },
	})
	require.NoError(err)
	assert.EqualValues(1, sum.ChatsProcessed)
	assert.NotEmpty(lines, "Progress should have been called at least once")
	// Each progress line should mention messages
	for _, l := range lines {
		assert.Contains(l, "messages", "progress line should mention messages count: %q", l)
	}
}

// TestImportChannelProgressCallback verifies progress is called for channel conversations.
func TestImportChannelProgressCallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := fakeChannelGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	var lines []string
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: true,
		Progress:        func(msg string) { lines = append(lines, msg) },
	})
	require.NoError(err)
	assert.EqualValues(1, sum.ChannelsProcessed)
	assert.NotEmpty(lines, "Progress should have been called for channel conversations")
}

// TestCheckpointFlushedAfterEachConversation verifies that after a successful Import,
// GetLatestCheckpointedSync does NOT return a stale failed checkpoint (since the run
// completed successfully), but that a checkpoint WAS written mid-run (visible via the
// completed sync_run's cursor_before column, or by checking an interrupted run).
// We test the happy path: after completion, cursor_before on the completed run is set.
func TestCheckpointFlushedAfterEachConversation(t *testing.T) {
	require := require.New(t)

	srv := fakeChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)
	_, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
	})
	require.NoError(err)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)

	// After a successful run, the latest run is completed. Its cursor_before should
	// have been written by the per-conversation checkpoint flush.
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	require.True(run.CursorBefore.Valid, "cursor_before should be set after per-conversation checkpoint flush")

	// The stored checkpoint must parse as a SyncState containing the synced chat cursor.
	state, err := LoadSyncState(run.CursorBefore.String)
	require.NoError(err)
	cursor := state.ChatCursor("19:x@thread.v2")
	require.NotEmpty(cursor, "SyncState in cursor_before should have a cursor for the synced chat")
}

// TestResumeFromCheckpoint verifies that after a failed sync (simulated by writing a
// checkpoint without completing), a fresh Import merges the checkpoint cursor so
// conversations already covered by the checkpoint start from their advanced cursor.
func TestResumeFromCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Server that returns one chat with one message newer than our pre-seeded cursor.
	var requestedSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.Contains(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			requestedSince = r.URL.Query().Get("$filter")
			_, _ = w.Write([]byte(`{"value":[
			  {"id":"m2","createdDateTime":"2025-06-01T00:00:00Z","lastModifiedDateTime":"2025-06-01T00:00:00Z",
			   "body":{"contentType":"text","content":"second message"}}
			]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)

	// Simulate a prior interrupted sync: start a sync run, write a checkpoint with a
	// SyncState that already has a cursor for the chat, then fail the run (not complete).
	// This is what would happen if the importer checkpointed mid-run then crashed.
	checkpointState := NewSyncState()
	checkpointState.SetChatCursor("19:x@thread.v2", "2025-03-01T00:00:00.000000000Z")
	blob, _ := checkpointState.Marshal()

	syncID, err := st.StartSync(src.ID, "teams")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         blob,
		MessagesProcessed: 5,
	}))
	require.NoError(st.FailSync(syncID, "simulated crash"))

	// Now run a fresh import. It should pick up the checkpoint cursor.
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)
	sum, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
	})
	require.NoError(err)
	assert.EqualValues(1, sum.ChatsProcessed)

	// The ListChatMessages request should have been made with the checkpoint cursor,
	// meaning the since parameter was non-empty (the fake server captures it).
	assert.NotEmpty(requestedSince, "import should have requested messages since the checkpoint cursor")
}

// TestFullIgnoresCursor verifies that ImportOptions.Full forces a full backfill:
// even when a prior completed sync left a cursor for the chat, the messages
// request is made with no $filter (since), so already-seen messages are
// re-fetched and re-persisted (repair path).
func TestFullIgnoresCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var requestedSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.Contains(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			requestedSince = r.URL.Query().Get("$filter")
			_, _ = w.Write([]byte(`{"value":[
			  {"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z",
			   "body":{"contentType":"text","content":"hello"}}
			]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)

	// Seed a prior completed sync whose cursor would normally skip m1.
	priorState := NewSyncState()
	priorState.SetChatCursor("19:x@thread.v2", "2025-03-01T00:00:00.000000000Z")
	blob, _ := priorState.Marshal()
	syncID, err := st.StartSync(src.ID, "teams")
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, blob))

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)
	sum, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		Full:            true,
	})
	require.NoError(err)
	assert.EqualValues(1, sum.ChatsProcessed)
	assert.Empty(requestedSince, "Full=true should request a full backfill with no $filter cursor")
	assert.EqualValues(1, sum.MessagesProcessed, "the previously-seen message should be re-fetched")
}

func TestImportMigratesLegacyRawMessageID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := fakeChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "direct_chat", "DM")
	require.NoError(err)
	legacyID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "teams",
	})
	require.NoError(err)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err = imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		Full:            true,
	})
	require.NoError(err)

	var rowCount int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`),
		src.ID,
	).Scan(&rowCount))
	assert.Equal(1, rowCount)

	var gotID int64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`),
		chatSourceMessageID("19:x@thread.v2", "m1"),
	).Scan(&gotID))
	assert.Equal(legacyID, gotID)

	var rawCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id = 'm1'`).Scan(&rawCount))
	assert.Equal(0, rawCount)
}

func TestImportMigratesLegacyRawMessageIDBeforeDelete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z","deletedDateTime":"2025-01-02T00:00:00Z"}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "direct_chat", "DM")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "teams",
	})
	require.NoError(err)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err = imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		Full:            true,
	})
	require.NoError(err)

	var deleted sql.NullTime
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE source_message_id = ?`),
		chatSourceMessageID("19:x@thread.v2", "m1"),
	).Scan(&deleted))
	assert.True(deleted.Valid)

	var rawCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id = 'm1'`).Scan(&rawCount))
	assert.Equal(0, rawCount)
}

// TestRawBlobPreservesEventDetail proves the raw archive blob (json.Marshal(gm))
// retains the eventDetail field, since EventDetail is json.RawMessage.
func TestRawBlobPreservesEventDetail(t *testing.T) {
	gm := &ChatMessage{
		ID:          "sys1",
		EventDetail: json.RawMessage([]byte(`{"@odata.type":"#microsoft.graph.callRecordingEventMessageDetail","callRecordingUrl":"https://sp/rec.mp4","callRecordingDisplayName":"Dev guild"}`)),
	}
	raw, err := json.Marshal(gm)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "callRecordingUrl")
	assert.Contains(t, string(raw), "https://sp/rec.mp4")
}

func TestTeamsImportEnqueuesPersistedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := fakeChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)
	enqueuer := &recordingEnqueuer{}

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		EmbedEnqueuer:   enqueuer,
	})
	require.NoError(err)
	require.Len(enqueuer.ids, 1)

	var storedID int64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`),
		chatSourceMessageID("19:x@thread.v2", "m1"),
	).Scan(&storedID))
	assert.Equal([]int64{storedID}, enqueuer.ids)
}

func TestTeamsReimportReplacesRemovedChildCollections(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	includeChildren := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:child@thread.v2","chatType":"group","topic":"Chat"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[{"id":"mem1","userId":"aad-bob","email":"bob@example.com","displayName":"Bob"}]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			children := ""
			if includeChildren {
				children = `,"attachments":[{"id":"a1","contentType":"reference","contentUrl":"https://sp/file.docx","name":"file.docx"}]` +
					`,"mentions":[{"id":0,"mentionText":"Bob","mentioned":{"user":{"id":"aad-bob","displayName":"Bob","userIdentityType":"aadUser"}}}]` +
					`,"reactions":[{"reactionType":"like","createdDateTime":"2025-01-01T00:00:01Z","user":{"user":{"id":"aad-bob","displayName":"Bob","userIdentityType":"aadUser"}}}]`
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"msg1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z","from":{"user":{"id":"alice@example.com","displayName":"Alice","userIdentityType":"emailUser"}},"body":{"contentType":"text","content":"hello"}` + children + `}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))

	_, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false, Full: true})
	require.NoError(err)
	includeChildren = false
	_, err = imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false, Full: true})
	require.NoError(err)

	var msgID int64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`),
		chatSourceMessageID("19:child@thread.v2", "msg1"),
	).Scan(&msgID))
	for table, query := range map[string]string{
		"mentions":    `SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'mention'`,
		"reactions":   `SELECT COUNT(*) FROM reactions WHERE message_id = ?`,
		"attachments": `SELECT COUNT(*) FROM attachments WHERE message_id = ? AND storage_path LIKE 'https://%'`,
	} {
		var count int
		require.NoError(st.DB().QueryRow(st.Rebind(query), msgID).Scan(&count), table)
		assert.Equal(0, count, table)
	}
}

// TestCallRecordingAndAttachmentsPersisted verifies that:
//   - a systemEventMessage's eventDetail call-recording link is stored as an attachment,
//   - a non-reference/reference attachment carrying a contentUrl is stored as an attachment,
//   - the recording URL is indexed into the message body text.
func TestCallRecordingAndAttachmentsPersisted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:rec@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[
			  {"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z",
			   "body":{"contentType":"html","content":"<p>here is the deck</p>"},
			   "attachments":[{"id":"a1","contentType":"reference","contentUrl":"https://sp/deck.pptx","name":"deck.pptx"}]},
			  {"id":"sys1","messageType":"unknownFutureValue","createdDateTime":"2025-01-02T00:00:00Z","lastModifiedDateTime":"2025-01-02T00:00:00Z",
			   "body":{"contentType":"html","content":"<systemEventMessage/>"},
			   "eventDetail":{"@odata.type":"#microsoft.graph.callRecordingEventMessageDetail","callRecordingUrl":"https://sp/rec.mp4","callRecordingDisplayName":"Dev guild"}}
			]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)
	_, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false})
	require.NoError(err)

	// Recording attachment row exists.
	var recCount int
	require.NoError(st.DB().QueryRow(`
        SELECT COUNT(*) FROM attachments a
        JOIN messages m ON m.id = a.message_id
        WHERE a.storage_path = 'https://sp/rec.mp4'`).Scan(&recCount))
	assert.Equal(1, recCount, "recording attachment row should exist")
	var recHash sql.NullString
	require.NoError(st.DB().QueryRow(`
        SELECT a.content_hash FROM attachments a
        JOIN messages m ON m.id = a.message_id
        WHERE a.storage_path = 'https://sp/rec.mp4'`).Scan(&recHash))
	assert.False(recHash.Valid && recHash.String != "", "URL-backed recording links should not look exportable by content hash")

	// Reference attachment row exists.
	var refCount int
	require.NoError(st.DB().QueryRow(`
        SELECT COUNT(*) FROM attachments a
        JOIN messages m ON m.id = a.message_id
        WHERE a.storage_path = 'https://sp/deck.pptx'`).Scan(&refCount))
	assert.Equal(1, refCount, "reference attachment row should exist")
	var refHash sql.NullString
	require.NoError(st.DB().QueryRow(`
        SELECT a.content_hash FROM attachments a
        JOIN messages m ON m.id = a.message_id
        WHERE a.storage_path = 'https://sp/deck.pptx'`).Scan(&refHash))
	assert.False(refHash.Valid && refHash.String != "", "URL-backed reference links should not look exportable by content hash")

	// Body text for the system message contains the recording URL.
	var bodyText sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
        SELECT mb.body_text FROM message_bodies mb
        JOIN messages m ON m.id = mb.message_id
        WHERE m.source_message_id = ?`), chatSourceMessageID("19:rec@thread.v2", "sys1")).Scan(&bodyText))
	assert.True(bodyText.Valid)
	assert.Contains(bodyText.String, "rec.mp4")
}

func TestTeamsMixedInlineAndLinkAttachmentsRefreshMessageStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case r.URL.Path == "/me/chats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[{"id":"19:mixed@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			body := `<div><img src="` + serverURL + `/chats/19:mixed@thread.v2/messages/m1/hostedContents/1/$value"></div>`
			_, _ = w.Write([]byte(`{"value":[{
				"id":"m1",
				"createdDateTime":"2025-01-01T00:00:00Z",
				"lastModifiedDateTime":"2025-01-01T00:00:00Z",
				"body":{"contentType":"html","content":` + jsonString(t, body) + `},
				"attachments":[{"id":"a1","contentType":"reference","contentUrl":"https://sp/file.docx","name":"file.docx"}]
			}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()

	st := testutil.NewTestStore(t)
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(context.Background(), ImportOptions{
		Email:           "me@example.com",
		IncludeChannels: false,
		AttachmentsDir:  t.TempDir(),
	})
	require.NoError(err)

	var hasAttachments bool
	var messageAttachmentCount int
	var actualAttachmentRows int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.has_attachments, m.attachment_count, COUNT(a.id)
		FROM messages m
		LEFT JOIN attachments a ON a.message_id = m.id
		WHERE m.source_message_id = ?
		GROUP BY m.id, m.has_attachments, m.attachment_count
	`), chatSourceMessageID("19:mixed@thread.v2", "m1")).Scan(&hasAttachments, &messageAttachmentCount, &actualAttachmentRows))
	assert.True(hasAttachments)
	assert.Equal(2, actualAttachmentRows)
	assert.Equal(2, messageAttachmentCount)
}

func TestDuplicateMentionDedup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Message that @mentions bob twice should produce exactly one 'mention' row
	// and sum.Errors should remain 0.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:chat1@thread.v2","chatType":"oneOnOne","topic":"Chat"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[
                {"id":"mem1","userId":"aad-alice","email":"alice@x.com","displayName":"Alice"},
                {"id":"mem2","userId":"aad-bob","email":"bob@x.com","displayName":"Bob"}
            ]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			// Two mention entries for bob (same aad id).
			_, _ = w.Write([]byte(`{"value":[{
                "id":"msg1",
                "createdDateTime":"2025-01-01T00:00:00Z",
                "lastModifiedDateTime":"2025-01-01T00:00:00Z",
                "from":{"user":{"id":"alice@x.com","displayName":"Alice","userIdentityType":"emailUser"}},
                "body":{"contentType":"text","content":"hey @Bob @Bob"},
                "mentions":[
                    {"id":0,"mentionText":"Bob","mentioned":{"user":{"id":"aad-bob","displayName":"Bob","userIdentityType":"aadUser"}}},
                    {"id":1,"mentionText":"Bob","mentioned":{"user":{"id":"aad-bob","displayName":"Bob","userIdentityType":"aadUser"}}}
                ]
            }]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, c)

	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false})
	require.NoError(err)
	assert.EqualValues(0, sum.Errors, "no errors expected")

	var msgID int64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`),
		chatSourceMessageID("19:chat1@thread.v2", "msg1"),
	).Scan(&msgID))

	var mentionCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
        SELECT COUNT(*) FROM message_recipients mr
        JOIN participants p ON p.id = mr.participant_id
        WHERE mr.message_id = ? AND mr.recipient_type = 'mention' AND p.email_address = 'bob@x.com'
    `), msgID).Scan(&mentionCount))
	assert.Equal(1, mentionCount, "duplicate @mention should produce exactly one mention row")
}
