package teams

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
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
	assert := assert.New(t)
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
	assert.EqualValues(1, sum.InlineImagesCopied)
	assert.EqualValues(0, sum.Errors)
	var role, roleSource string
	require.NoError(t, st.DB().QueryRow(`
		SELECT attachment_role, role_source FROM attachments LIMIT 1`).Scan(&role, &roleSource))
	assert.Equal("inline", role)
	assert.Equal("importer_semantics", roleSource)
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

// TestTeamsInlineMarkerSurvivesLinkAttachmentReplacement guards the ordering
// hazard inside persistMessage: inline hosted-content markers are written
// first and carry the raw Graph URL as their storage path, so the link
// attachment replacement that runs afterwards must not treat them as stale
// links. Losing the marker loses the recorded skip and makes the next sync
// re-fetch media the size cap already excluded.
func TestTeamsInlineMarkerSurvivesLinkAttachmentReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	hostedFetches := 0
	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
			hostedFetches++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("12345678901"))
		case r.URL.Path == "/me/chats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[{"id":"19:marker@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			body := `<div><img src="` + serverURL + `/chats/19:marker@thread.v2/messages/m1/hostedContents/1/$value"></div>`
			_, _ = w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z",
				"body":{"contentType":"html","content":` + jsonString(t, body) + `},
				"attachments":[{"id":"a1","contentType":"reference","name":"spec.pdf","contentUrl":"https://example.com/spec.pdf"}]}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	opts := ImportOptions{
		Email: "me@example.com", AttachmentsDir: t.TempDir(), Full: true,
		MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 10},
	}
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.EqualValues(1, sum.InlineImagesSkipped)
	require.Equal(1, hostedFetches)

	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, hostedFetches,
		"the surviving oversize marker must keep the second sync from re-fetching")

	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`),
		chatSourceMessageID("19:marker@thread.v2", "m1")).Scan(&messageID))
	markerID := "teams:inline:/chats/19:marker@thread.v2/messages/m1/hostedContents/1/$value"
	markers, err := st.MessageTeamsInlineAttachments(messageID)
	require.NoError(err)
	require.Contains(markers, markerID)
	assert.Equal(attachmentpolicy.StateSkipped, markers[markerID].State)
	assert.Equal(attachmentpolicy.SkipSizeCap, markers[markerID].SkipReason)
	assert.Greater(markers[markerID].Size, 10, "the observed oversize must survive the link replacement")

	var linkRows int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments WHERE message_id = ? AND storage_path = ?
	`), messageID, "https://example.com/spec.pdf").Scan(&linkRows))
	assert.Equal(1, linkRows, "the link attachment must still be recorded")
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

func TestBackfillInlineMediaPolicySkipsChannelWithoutFetch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte("must not download"))
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "channel-1", "channel", "Channel")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: src.ID,
		SourceMessageID: "channel-message-1", MessageType: "teams",
	})
	require.NoError(err)
	bodyHTML := `<img src="` + srv.URL + `/v1.0/teams/t/channels/c/messages/m/hostedContents/1/$value">`
	require.NoError(st.UpsertMessageBody(messageID, sql.NullString{}, sql.NullString{String: bodyHTML, Valid: true}))
	client := NewClient(srv.URL+"/v1.0", func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, client)

	sum, err := imp.BackfillInlineMedia(context.Background(), ImportOptions{
		Email: "me@example.com", AttachmentsDir: t.TempDir(),
		MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeDirect, MaxBytes: 100 << 20},
	})
	require.NoError(err)
	assert.EqualValues(1, sum.InlineImagesSkipped)
	assert.Zero(sum.InlineImagesCopied)
	assert.Zero(requests)

	var state, reason string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT attachment_state, attachment_skip_reason FROM attachments WHERE message_id = ?
	`), messageID).Scan(&state, &reason))
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipPolicyScope), reason)
}

func TestBackfillInlineMediaPreservesLegacyStoredMediaUntilReplacementSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		policy     attachmentpolicy.Policy
	}{
		{
			name:   "policy exclusion",
			policy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone},
		},
		{
			name:       "transient fetch failure",
			statusCode: http.StatusServiceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("teams", "me@example.com")
			require.NoError(err)
			conversationID, err := st.EnsureConversationWithType(src.ID, "chat-legacy", "direct_chat", "Chat")
			require.NoError(err)
			messageID, err := st.UpsertMessage(&store.Message{
				ConversationID: conversationID, SourceID: src.ID,
				SourceMessageID: "message-legacy", MessageType: "teams",
			})
			require.NoError(err)
			bodyHTML := `<img src="` + srv.URL + `/v1.0/chats/c/messages/m/hostedContents/1/$value">`
			require.NoError(st.UpsertMessageBody(messageID, sql.NullString{}, sql.NullString{String: bodyHTML, Valid: true}))
			require.NoError(st.UpsertAttachment(messageID, "", "", "legacy/inline.png", "legacy-inline-hash", 12))

			imp := NewImporter(st, NewClient(srv.URL+"/v1.0", func(context.Context) (string, error) { return "t", nil }, 50))
			_, err = imp.BackfillInlineMedia(t.Context(), ImportOptions{
				Email: "me@example.com", AttachmentsDir: t.TempDir(), MediaPolicy: tc.policy,
			})
			require.NoError(err)

			var legacyCount int
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT COUNT(*) FROM attachments
				WHERE message_id = ? AND content_hash = 'legacy-inline-hash'
			`), messageID).Scan(&legacyCount))
			assert.Equal(1, legacyCount, "legacy stored media must survive until a replacement is archived")
		})
	}
}

func TestBackfillInlineMediaRecordsStreamedOversizeForRetryPolicy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("12345678901"))
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "chat-1", "direct_chat", "Chat")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: src.ID,
		SourceMessageID: "message-1", MessageType: "teams",
	})
	require.NoError(err)
	bodyHTML := `<img src="` + srv.URL + `/v1.0/chats/c/messages/m/hostedContents/1/$value">`
	require.NoError(st.UpsertMessageBody(messageID, sql.NullString{}, sql.NullString{String: bodyHTML, Valid: true}))
	client := NewClient(srv.URL+"/v1.0", func(context.Context) (string, error) { return "t", nil }, 50)
	imp := NewImporter(st, client)
	opts := ImportOptions{
		Email: "me@example.com", AttachmentsDir: t.TempDir(), OnlyIncomplete: true,
		MediaPolicy: attachmentpolicy.Policy{MaxBytes: 10},
	}

	sum, err := imp.BackfillInlineMedia(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.InlineImagesSkipped)
	assert.Zero(sum.InlineImagesCopied)
	var size int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT size FROM attachments WHERE message_id = ?
	`), messageID).Scan(&size))
	assert.Greater(size, int64(10))
	second, err := imp.BackfillInlineMedia(t.Context(), opts)
	require.NoError(err)
	assert.Zero(second.MessagesProcessed)
	assert.Equal(1, requests)
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

// TestChannelMediaPolicyUsesTeamParticipantCount verifies that channel imports
// evaluate media_max_participants against the team's membership. Channels
// carried no participant count at all, so the threshold silently never applied
// to them — the largest conversations in an archive. Membership is also
// persisted, so a later backfill or purge reads the same count.
func TestChannelMediaPolicyUsesTeamParticipantCount(t *testing.T) {
	for _, tc := range []struct {
		name             string
		membersStatus    int
		maxParticipants  int
		wantState        string
		wantReason       string
		wantFetches      int
		wantParticipants int
		wantErrors       int64
	}{
		{
			name: "team over the limit skips media", maxParticipants: 2,
			wantState:  string(attachmentpolicy.StateSkipped),
			wantReason: string(attachmentpolicy.SkipParticipantThreshold),
			// The team's three members are still archived on the channel.
			wantParticipants: 3,
		},
		{
			name: "team within the limit stores media", maxParticipants: 5,
			wantState: string(attachmentpolicy.StateStored), wantFetches: 1,
			wantParticipants: 3,
		},
		{
			name: "unknown membership fails closed", membersStatus: http.StatusInternalServerError,
			maxParticipants: 2,
			wantState:       string(attachmentpolicy.StateSkipped),
			wantReason:      string(attachmentpolicy.SkipParticipantThreshold),
			wantErrors:      1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			hostedFetches := 0
			serverURL := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
					hostedFetches++
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write([]byte("PNGDATA"))
				case strings.HasSuffix(r.URL.Path, "/members"):
					if tc.membersStatus != 0 {
						w.Header().Set("Retry-After", "0")
						http.Error(w, "members unavailable", tc.membersStatus)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[
						{"id":"mem1","userId":"aad-alice","email":"alice@example.com","displayName":"Alice"},
						{"id":"mem2","userId":"aad-bob","email":"bob@example.com","displayName":"Bob"},
						{"id":"mem3","userId":"aad-carol","email":"carol@example.com","displayName":"Carol"}
					]}`))
				case r.URL.Path == "/me/chats":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[]}`))
				case r.URL.Path == "/me/joinedTeams":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
				case strings.HasSuffix(r.URL.Path, "/channels"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General","membershipType":"standard"}]}`))
				case strings.Contains(r.URL.Path, "/replies"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[]}`))
				case strings.HasSuffix(r.URL.Path, "/messages/delta"):
					w.Header().Set("Content-Type", "application/json")
					body := `<div><img src="` + serverURL + `/teams/team1/channels/chanA/messages/c1/hostedContents/1/$value"></div>`
					_, _ = w.Write([]byte(`{"value":[{"id":"c1","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z",` +
						`"body":{"contentType":"html","content":` + jsonString(t, body) + `}}],"@odata.deltaLink":"` + serverURL + `/delta?token=next"}`))
				case strings.HasSuffix(r.URL.Path, "/messages"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[]}`))
				default:
					http.Error(w, "404", http.StatusNotFound)
				}
			}))
			serverURL = srv.URL
			defer srv.Close()
			st := testutil.NewTestStore(t)

			imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
			sum, err := imp.Import(t.Context(), ImportOptions{
				Email: "me@example.com", IncludeChannels: true, AttachmentsDir: t.TempDir(),
				MediaPolicy: attachmentpolicy.Policy{
					Scope: attachmentpolicy.ScopeAll, MaxParticipants: tc.maxParticipants,
					MaxBytes: 100 << 20,
				},
			})
			require.NoError(err)
			assert.Equal(tc.wantErrors, sum.Errors)
			assert.Equal(tc.wantFetches, hostedFetches)

			var state, reason, contentHash string
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT COALESCE(a.attachment_state, ''), COALESCE(a.attachment_skip_reason, ''),
				       COALESCE(a.content_hash, '')
				FROM attachments a JOIN messages m ON m.id = a.message_id
				WHERE m.source_message_id = ?
			`), channelSourceMessageID("team1", "chanA", "c1")).Scan(&state, &reason, &contentHash))
			assert.Equal(tc.wantState, state)
			assert.Equal(tc.wantReason, reason)
			if tc.wantState == string(attachmentpolicy.StateStored) {
				assert.NotEmpty(contentHash)
			} else {
				assert.Empty(contentHash)
			}

			var participantCount int
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT COALESCE(participant_count, 0) FROM conversations WHERE source_conversation_id = ?
			`), "team1/chanA").Scan(&participantCount))
			assert.Equal(tc.wantParticipants, participantCount,
				"channel membership must be archived so backfill and purge see the same count")
		})
	}
}

// privateChannelGraph is a fake Graph server for a team whose single channel is
// private: a three-member team roster, a one-member channel roster, and one
// channel message carrying inline hosted content. The channel roster is
// unreadable until channelRosterReadable is set, so a test can fail the roster
// during sync and repair it before a backfill.
type privateChannelGraph struct {
	channelRosterReadable bool
	hostedFetches         int
	teamRosterReads       int
	url                   string
}

func newPrivateChannelGraph(t *testing.T) *privateChannelGraph {
	t.Helper()

	g := &privateChannelGraph{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
			g.hostedFetches++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case strings.Contains(r.URL.Path, "/channels/") && strings.HasSuffix(r.URL.Path, "/members"):
			if !g.channelRosterReadable {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "channel members unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[
				{"id":"mem1","userId":"aad-alice","email":"alice@example.com","displayName":"Alice"}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			g.teamRosterReads++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[
				{"id":"mem1","userId":"aad-alice","email":"alice@example.com","displayName":"Alice"},
				{"id":"mem2","userId":"aad-bob","email":"bob@example.com","displayName":"Bob"},
				{"id":"mem3","userId":"aad-carol","email":"carol@example.com","displayName":"Carol"}
			]}`))
		case r.URL.Path == "/me/chats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"Secret","membershipType":"private"}]}`))
		case strings.Contains(r.URL.Path, "/replies"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			w.Header().Set("Content-Type", "application/json")
			body := `<div><img src="` + g.url + `/teams/team1/channels/chanA/messages/c1/hostedContents/1/$value"></div>`
			_, _ = w.Write([]byte(`{"value":[{"id":"c1","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z",` +
				`"body":{"contentType":"html","content":` + jsonString(t, body) + `}}],"@odata.deltaLink":"` + g.url + `/delta?token=next"}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	g.url = srv.URL
	t.Cleanup(srv.Close)
	return g
}

// channelMediaState returns the archived download outcome and participant count
// for the fixture channel message.
func channelMediaState(t *testing.T, st *store.Store) (state, reason string, participants int) {
	t.Helper()

	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(a.attachment_state, ''), COALESCE(a.attachment_skip_reason, '')
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?
	`), channelSourceMessageID("team1", "chanA", "c1")).Scan(&state, &reason))
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(participant_count, 0) FROM conversations WHERE source_conversation_id = ?
	`), "team1/chanA").Scan(&participants))
	return state, reason, participants
}

// TestPrivateChannelMediaPolicyUsesChannelRoster pins the roster a private
// channel is evaluated against. Private and shared channels carry membership of
// their own, so inheriting the team roster both overstates a small private
// channel (excluding media it should keep) and understates a private channel in
// a small team. The fixture is deliberately contradictory: the team is over the
// limit while the channel is under it.
func TestPrivateChannelMediaPolicyUsesChannelRoster(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		channelRosterReadable bool
		wantState             string
		wantReason            string
		wantFetches           int
		wantParticipants      int
		wantErrors            int64
	}{
		{
			name: "channel roster under the limit stores media", channelRosterReadable: true,
			wantState: string(attachmentpolicy.StateStored), wantFetches: 1,
			wantParticipants: 1,
		},
		{
			name:       "unreadable channel roster fails closed",
			wantState:  string(attachmentpolicy.StateSkipped),
			wantReason: string(attachmentpolicy.SkipParticipantThreshold),
			wantErrors: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			graph := newPrivateChannelGraph(t)
			graph.channelRosterReadable = tc.channelRosterReadable
			st := testutil.NewTestStore(t)

			imp := NewImporter(st, NewClient(graph.url, func(context.Context) (string, error) { return "t", nil }, 50))
			sum, err := imp.Import(t.Context(), ImportOptions{
				Email: "me@example.com", IncludeChannels: true, AttachmentsDir: t.TempDir(),
				MediaPolicy: attachmentpolicy.Policy{
					Scope: attachmentpolicy.ScopeAll, MaxParticipants: 2, MaxBytes: 100 << 20,
				},
			})
			require.NoError(err)
			assert.Equal(tc.wantErrors, sum.Errors)
			assert.Equal(tc.wantFetches, graph.hostedFetches)
			assert.Zero(graph.teamRosterReads,
				"a team of only private channels never needs its team roster resolved")

			state, reason, participants := channelMediaState(t, st)
			assert.Equal(tc.wantState, state)
			assert.Equal(tc.wantReason, reason)
			assert.Equal(tc.wantParticipants, participants,
				"the channel's own membership is what purge and later runs must read")
		})
	}
}

// TestBackfillPrivateChannelMediaRefreshesChannelRoster is the backfill half of
// TestPrivateChannelMediaPolicyUsesChannelRoster: a channel whose own roster was
// unreadable during sync must be re-resolved from the channel, not from the
// team it hangs off — the team roster here would exclude the media.
func TestBackfillPrivateChannelMediaRefreshesChannelRoster(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	graph := newPrivateChannelGraph(t)
	st := testutil.NewTestStore(t)
	attachmentsDir := t.TempDir()
	policy := attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: 2, MaxBytes: 100 << 20,
	}

	imp := NewImporter(st, NewClient(graph.url, func(context.Context) (string, error) { return "t", nil }, 50))
	syncSum, err := imp.Import(t.Context(), ImportOptions{
		Email: "me@example.com", IncludeChannels: true,
		AttachmentsDir: attachmentsDir, MediaPolicy: policy,
	})
	require.NoError(err)
	require.EqualValues(1, syncSum.Errors, "the unreadable channel roster is counted once")
	require.Zero(graph.hostedFetches, "sync must fail closed while membership is unknown")

	graph.channelRosterReadable = true
	sum, err := imp.BackfillInlineMedia(t.Context(), ImportOptions{
		Email: "me@example.com", AttachmentsDir: attachmentsDir, MediaPolicy: policy,
	})
	require.NoError(err)
	assert.Equal(1, graph.hostedFetches,
		"the channel's own roster is under the limit the team roster exceeds")
	assert.EqualValues(1, sum.InlineImagesCopied)
	assert.Zero(sum.Errors)

	state, reason, participants := channelMediaState(t, st)
	assert.Equal(string(attachmentpolicy.StateStored), state)
	assert.Empty(reason)
	assert.Equal(1, participants)
}

// TestBackfillChannelMediaRefreshesUnknownTeamMembership covers the durable
// half of the channel participant threshold. Sync fails closed on an unreadable
// roster using a count that only lives in memory, so the channel is archived
// with no members; the backfill then reads that archived zero. Unless it
// re-resolves the roster first, the zero reads as "small conversation" and the
// media the threshold excluded gets downloaded after all.
func TestBackfillChannelMediaRefreshesUnknownTeamMembership(t *testing.T) {
	for _, tc := range []struct {
		name             string
		rosterReadable   bool
		maxParticipants  int
		wantFetches      int
		wantCopied       int64
		wantSkipped      int64
		wantErrors       int64
		wantState        string
		wantReason       string
		wantParticipants int
	}{
		{
			name:           "refreshed roster within the limit stores media",
			rosterReadable: true, maxParticipants: 5,
			wantFetches: 1, wantCopied: 1,
			wantState:        string(attachmentpolicy.StateStored),
			wantParticipants: 3,
		},
		{
			name:            "roster still unreadable stays skipped",
			maxParticipants: 5,
			wantSkipped:     1, wantErrors: 1,
			wantState:  string(attachmentpolicy.StateSkipped),
			wantReason: string(attachmentpolicy.SkipParticipantThreshold),
		},
		{
			name:           "refreshed roster over the limit stays skipped",
			rosterReadable: true, maxParticipants: 2,
			wantSkipped: 1,
			wantState:   string(attachmentpolicy.StateSkipped),
			wantReason:  string(attachmentpolicy.SkipParticipantThreshold),
			// The roster is archived even when it excludes the media, so
			// purge and later runs evaluate the same membership.
			wantParticipants: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			hostedFetches := 0
			rosterReadable := false
			serverURL := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
					hostedFetches++
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write([]byte("PNGDATA"))
				case strings.HasSuffix(r.URL.Path, "/members"):
					if !rosterReadable {
						w.Header().Set("Retry-After", "0")
						http.Error(w, "members unavailable", http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[
						{"id":"mem1","userId":"aad-alice","email":"alice@example.com","displayName":"Alice"},
						{"id":"mem2","userId":"aad-bob","email":"bob@example.com","displayName":"Bob"},
						{"id":"mem3","userId":"aad-carol","email":"carol@example.com","displayName":"Carol"}
					]}`))
				case r.URL.Path == "/me/chats":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[]}`))
				case r.URL.Path == "/me/joinedTeams":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
				case strings.HasSuffix(r.URL.Path, "/channels"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General","membershipType":"standard"}]}`))
				case strings.Contains(r.URL.Path, "/replies"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[]}`))
				case strings.HasSuffix(r.URL.Path, "/messages/delta"):
					w.Header().Set("Content-Type", "application/json")
					body := `<div><img src="` + serverURL + `/teams/team1/channels/chanA/messages/c1/hostedContents/1/$value"></div>`
					_, _ = w.Write([]byte(`{"value":[{"id":"c1","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z",` +
						`"body":{"contentType":"html","content":` + jsonString(t, body) + `}}],"@odata.deltaLink":"` + serverURL + `/delta?token=next"}`))
				case strings.HasSuffix(r.URL.Path, "/messages"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"value":[]}`))
				default:
					http.Error(w, "404", http.StatusNotFound)
				}
			}))
			serverURL = srv.URL
			defer srv.Close()
			st := testutil.NewTestStore(t)
			attachmentsDir := t.TempDir()

			archivedParticipants := func() int {
				var count int
				require.NoError(st.DB().QueryRow(st.Rebind(`
					SELECT COALESCE(participant_count, 0) FROM conversations WHERE source_conversation_id = ?
				`), "team1/chanA").Scan(&count))
				return count
			}

			imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
			policy := attachmentpolicy.Policy{
				Scope: attachmentpolicy.ScopeAll, MaxParticipants: tc.maxParticipants,
				MaxBytes: 100 << 20,
			}
			syncSum, err := imp.Import(t.Context(), ImportOptions{
				Email: "me@example.com", IncludeChannels: true,
				AttachmentsDir: attachmentsDir, MediaPolicy: policy,
			})
			require.NoError(err)
			require.EqualValues(1, syncSum.Errors, "the unreadable roster is counted once")
			require.Zero(hostedFetches, "sync must fail closed while membership is unknown")
			require.Zero(archivedParticipants(), "sync could not archive the roster")

			rosterReadable = tc.rosterReadable
			sum, err := imp.BackfillInlineMedia(t.Context(), ImportOptions{
				Email: "me@example.com", AttachmentsDir: attachmentsDir, MediaPolicy: policy,
			})
			require.NoError(err)
			assert.Equal(tc.wantFetches, hostedFetches)
			assert.Equal(tc.wantCopied, sum.InlineImagesCopied)
			assert.Equal(tc.wantSkipped, sum.InlineImagesSkipped)
			assert.Equal(tc.wantErrors, sum.Errors)

			var state, reason, contentHash string
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT COALESCE(a.attachment_state, ''), COALESCE(a.attachment_skip_reason, ''),
				       COALESCE(a.content_hash, '')
				FROM attachments a JOIN messages m ON m.id = a.message_id
				WHERE m.source_message_id = ?
			`), channelSourceMessageID("team1", "chanA", "c1")).Scan(&state, &reason, &contentHash))
			assert.Equal(tc.wantState, state)
			assert.Equal(tc.wantReason, reason)
			if tc.wantState == string(attachmentpolicy.StateStored) {
				assert.NotEmpty(contentHash)
			} else {
				assert.Empty(contentHash)
			}

			assert.Equal(tc.wantParticipants, archivedParticipants(),
				"a roster resolved during backfill must be archived for purge and later runs")
		})
	}
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
				"attachments":[
					{"id":"a1","contentType":"reference","contentUrl":"https://sp/file.docx","name":"file.docx"},
					{"id":"a2","contentType":"reference","contentUrl":"https://sp/notes.txt","name":"notes.txt"}
				]
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
	assert.Equal(3, actualAttachmentRows)
	assert.Equal(3, messageAttachmentCount)
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

// graphMembersJSON renders a Graph member collection of the given size.
func graphMembersJSON(size int) string {
	members := make([]string, 0, size)
	for i := 1; i <= size; i++ {
		members = append(members, fmt.Sprintf(
			`{"id":"mem%d","userId":"aad-user%d","email":"user%d@example.com","displayName":"User %d"}`,
			i, i, i, i))
	}
	return `{"value":[` + strings.Join(members, ",") + `]}`
}

// chatRosterGraph is a fake Graph server for one group chat whose single
// message carries inline hosted content. The test controls the roster it
// serves: rosterFails makes the members call fail, and rosterSize sets how
// many members a successful call reports.
type chatRosterGraph struct {
	rosterFails   bool
	rosterSize    int
	hostedFetches int
	url           string
}

const rosterChatID = "19:media@thread.v2"

func newChatRosterGraph(t *testing.T, rosterSize int) *chatRosterGraph {
	t.Helper()

	g := &chatRosterGraph{rosterSize: rosterSize}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
			g.hostedFetches++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case strings.HasSuffix(r.URL.Path, "/members"):
			if g.rosterFails {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "members unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(graphMembersJSON(g.rosterSize)))
		case r.URL.Path == "/me/chats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[{"id":"` + rosterChatID + `","chatType":"group","topic":"Media"}]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			body := `<div><img src="` + g.url + `/chats/` + rosterChatID + `/messages/m1/hostedContents/1/$value"></div>`
			_, _ = w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z",` +
				`"lastModifiedDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"html","content":` +
				jsonString(t, body) + `}}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	g.url = srv.URL
	t.Cleanup(srv.Close)
	return g
}

// chatMediaState returns the archived download outcome of the fixture chat
// message's inline hosted content.
func chatMediaState(t *testing.T, st *store.Store) (state, reason string) {
	t.Helper()

	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(a.attachment_state, ''), COALESCE(a.attachment_skip_reason, '')
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?
	`), chatSourceMessageID(rosterChatID, "m1")).Scan(&state, &reason))
	return state, reason
}

// chatMembershipRecord returns the roster the fixture chat's provider metadata
// records: the exact member count, and whether the roster is marked unreadable.
func chatMembershipRecord(t *testing.T, st *store.Store) (count int, unknown bool) {
	t.Helper()

	var metadata string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(CAST(metadata AS TEXT), '') FROM conversations
		WHERE source_conversation_id = ?
	`), rosterChatID).Scan(&metadata))
	if metadata == "" {
		return 0, false
	}
	var record struct {
		MemberCount        int  `json:"member_count"`
		MemberCountUnknown bool `json:"member_count_unknown"`
	}
	require.NoError(t, json.Unmarshal([]byte(metadata), &record))
	return record.MemberCount, record.MemberCountUnknown
}

// TestChatMediaPolicyFailsClosedOnUnreadableRoster covers a chat whose member
// listing fails: the participant threshold cannot be evaluated, so media must
// not be downloaded as if the chat were small — and the failure must be
// archived, since a backfill reads the archive rather than the run's memory.
// Both backfill modes must still reach the message: the archived unknown
// roster decides the download, not whether the message is worth revisiting.
func TestChatMediaPolicyFailsClosedOnUnreadableRoster(t *testing.T) {
	for _, tc := range []struct {
		name           string
		onlyIncomplete bool
	}{
		{name: "full backfill"},
		{name: "incomplete-only backfill", onlyIncomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			graph := newChatRosterGraph(t, 3)
			graph.rosterFails = true
			st := testutil.NewTestStore(t)
			attachmentsDir := t.TempDir()
			policy := attachmentpolicy.Policy{
				Scope: attachmentpolicy.ScopeAll, MaxParticipants: 5, MaxBytes: 100 << 20,
			}

			imp := NewImporter(st, NewClient(graph.url,
				func(context.Context) (string, error) { return "t", nil }, 50))
			sum, err := imp.Import(t.Context(), ImportOptions{
				Email: "me@example.com", AttachmentsDir: attachmentsDir, MediaPolicy: policy,
			})
			require.NoError(err)
			require.EqualValues(1, sum.Errors, "the unreadable roster is counted once")
			assert.Zero(graph.hostedFetches,
				"an unreadable roster must not read as a chat under the limit")

			state, reason := chatMediaState(t, st)
			assert.Equal(string(attachmentpolicy.StateSkipped), state)
			assert.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
			count, unknown := chatMembershipRecord(t, st)
			assert.True(unknown, "an unreadable roster must be archived, not left absent")
			assert.Zero(count)

			// The backfill re-resolves what the sync could not read, and
			// archives the count purge and later runs evaluate.
			graph.rosterFails = false
			backfill, err := imp.BackfillInlineMedia(t.Context(), ImportOptions{
				Email: "me@example.com", AttachmentsDir: attachmentsDir,
				MediaPolicy: policy, OnlyIncomplete: tc.onlyIncomplete,
			})
			require.NoError(err)
			assert.EqualValues(1, backfill.InlineImagesCopied)
			assert.Zero(backfill.Errors)

			state, reason = chatMediaState(t, st)
			assert.Equal(string(attachmentpolicy.StateStored), state)
			assert.Empty(reason)
			count, unknown = chatMembershipRecord(t, st)
			assert.Equal(3, count)
			assert.False(unknown)
		})
	}
}

// TestChatMediaPolicyReresolvesRosterAfterLaterOutage covers a roster that
// was read once and then could not be read again. The archived count is kept
// for reference, but the roster is unresolved: the backfill must re-resolve it
// rather than trust the stale count, stay closed while the read keeps failing,
// and release the media once a read succeeds and shows the chat under the
// limit — without waiting for another sync to notice the shrink.
func TestChatMediaPolicyReresolvesRosterAfterLaterOutage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	graph := newChatRosterGraph(t, 6)
	st := testutil.NewTestStore(t)
	attachmentsDir := t.TempDir()
	policy := attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: 4, MaxBytes: 100 << 20,
	}
	opts := ImportOptions{
		Email: "me@example.com", AttachmentsDir: attachmentsDir, MediaPolicy: policy,
	}
	imp := NewImporter(st, NewClient(graph.url, func(context.Context) (string, error) { return "t", nil }, 50))

	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.Zero(graph.hostedFetches)
	count, unknown := chatMembershipRecord(t, st)
	require.Equal(6, count)
	require.False(unknown)

	graph.rosterFails = true
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.Errors, "the unreadable roster is counted once")
	count, unknown = chatMembershipRecord(t, st)
	assert.Equal(6, count, "the count read earlier stays for reference")
	assert.True(unknown, "a later failed read must be archived, not hidden behind the earlier count")

	// While the roster stays unreadable the backfill re-resolves it, fails
	// closed again, and downloads nothing.
	backfill, err := imp.BackfillInlineMedia(t.Context(), opts)
	require.NoError(err)
	assert.Zero(backfill.InlineImagesCopied)
	assert.Zero(graph.hostedFetches, "a stale count must not admit media while the roster is unresolved")
	state, reason := chatMediaState(t, st)
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
	count, unknown = chatMembershipRecord(t, st)
	assert.Equal(6, count)
	assert.True(unknown)

	// A readable roster resolves it, and the shrunken chat's media is released.
	graph.rosterFails = false
	graph.rosterSize = 3
	backfill, err = imp.BackfillInlineMedia(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, backfill.InlineImagesCopied)
	assert.Zero(backfill.Errors)
	state, reason = chatMediaState(t, st)
	assert.Equal(string(attachmentpolicy.StateStored), state)
	assert.Empty(reason)
	count, unknown = chatMembershipRecord(t, st)
	assert.Equal(3, count)
	assert.False(unknown)
}

// TestChatMediaPolicyFollowsRosterShrink pins the direction the archived
// membership must be able to move. Participant rows only ever accumulate, so a
// conversation whose roster drops below the threshold would stay excluded from
// media forever if policy kept reading them.
func TestChatMediaPolicyFollowsRosterShrink(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	graph := newChatRosterGraph(t, 6)
	st := testutil.NewTestStore(t)
	attachmentsDir := t.TempDir()
	policy := attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: 4, MaxBytes: 100 << 20,
	}
	opts := ImportOptions{
		Email: "me@example.com", AttachmentsDir: attachmentsDir, MediaPolicy: policy,
	}

	imp := NewImporter(st, NewClient(graph.url, func(context.Context) (string, error) { return "t", nil }, 50))
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.Zero(graph.hostedFetches)
	state, reason := chatMediaState(t, st)
	require.Equal(string(attachmentpolicy.StateSkipped), state)
	require.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
	count, _ := chatMembershipRecord(t, st)
	require.Equal(6, count)

	graph.rosterSize = 2
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, graph.hostedFetches)
	state, reason = chatMediaState(t, st)
	assert.Equal(string(attachmentpolicy.StateStored), state)
	assert.Empty(reason)

	count, unknown := chatMembershipRecord(t, st)
	assert.Equal(2, count, "the archived roster must follow the chat's current membership")
	assert.False(unknown)
	var accumulatedParticipants int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(participant_count, 0) FROM conversations WHERE source_conversation_id = ?
	`), rosterChatID).Scan(&accumulatedParticipants))
	assert.Equal(6, accumulatedParticipants, "the participant rows keep every member ever seen")

	messageID := messageIDBySourceMessageID(t, st, chatSourceMessageID(rosterChatID, "m1"))
	conversation, err := st.AttachmentConversation(messageID)
	require.NoError(err)
	assert.Equal(2, conversation.ParticipantCount,
		"purge and backfill evaluate the roster, not the accumulated rows")
}

// messageIDBySourceMessageID resolves an archived message's internal ID.
func messageIDBySourceMessageID(t *testing.T, st *store.Store, sourceMessageID string) int64 {
	t.Helper()

	var messageID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), sourceMessageID).Scan(&messageID))
	return messageID
}

// A message can share the exact lastModifiedDateTime of the stored cursor.
// Graph only supports an exclusive "gt" filter on that property and its
// timestamps are millisecond-resolution, so without a backwards overlap such a
// message would be filtered out on every subsequent sync and lost permanently.
//
// Unlike a mock that merely echoes $filter back, this server applies the
// filter, so the test fails if the overlap is removed.
func TestChatSyncRecoversMessageSharingCursorTimestamp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	const tieTS = "2026-01-01T00:00:00.123Z"
	tie, err := time.Parse(time.RFC3339Nano, tieTS)
	require.NoError(err)

	var secondSync atomic.Bool
	var filters graphFilterFake

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:tie@thread.v2","chatType":"oneOnOne","topic":"Tie"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			// m2 lands after the first sync, sharing m1's exact timestamp.
			ids := []string{"tm1"}
			if secondSync.Load() {
				ids = append(ids, "tm2")
			}
			cutoff, ok := filters.cutoff(w, r.URL.Query().Get("$filter"))
			if !ok {
				return
			}
			var out []string
			if cutoff == nil || tie.After(*cutoff) { // exactly what Graph does for "gt"
				for _, id := range ids {
					out = append(out, `{"id":"`+id+`","createdDateTime":"`+tieTS+
						`","lastModifiedDateTime":"`+tieTS+
						`","body":{"contentType":"text","content":"`+id+`"}}`)
				}
			}
			_, _ = w.Write([]byte(`{"value":[` + strings.Join(out, ",") + `]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	opts := ImportOptions{Email: "me@example.com", IncludeChannels: false}

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	require.EqualValues(1, sum.MessagesAdded, "first sync stores tm1")

	// The cursor is now exactly tieTS, the timestamp tm2 also carries.
	secondSync.Store(true)
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.EqualValues(2, sum.MessagesProcessed, "second sync re-reads tm1 and recovers tm2")
	assert.EqualValues(1, sum.MessagesAdded,
		"only tm2 is new; the re-read of tm1 must not be counted as added")

	filters.check(t)

	var got []string
	rows, err := st.DB().Query(`SELECT source_message_id FROM messages WHERE message_type='teams' ORDER BY source_message_id`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		require.NoError(rows.Scan(&id))
		got = append(got, id)
	}
	require.NoError(rows.Err())
	assert.Len(got, 2, "both messages persisted, no duplicate from the overlap re-read")
}

// graphFilterFake applies a Graph `$filter` inside a test double the way the
// service does. A filter Graph would reject is answered 400 and recorded for
// the test body to assert on rather than failed on the spot: a require inside
// an HTTP handler aborts the server goroutine, not the test.
type graphFilterFake struct {
	mu  sync.Mutex
	err error
}

// cutoff extracts the timestamp from a "lastModifiedDateTime gt <ts>" filter,
// returning a nil cutoff when there is no filter. It rejects any other operator
// the way Graph does. The bool reports whether the handler should carry on.
func (g *graphFilterFake) cutoff(w http.ResponseWriter, filter string) (*time.Time, bool) {
	if filter == "" {
		return nil, true
	}
	const prefix = "lastModifiedDateTime gt "
	if !strings.HasPrefix(filter, prefix) {
		g.reject(w, fmt.Errorf("graph rejects any operator but gt/lt on lastModifiedDateTime; got %q", filter))
		return nil, false
	}
	ts, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(filter, prefix))
	if err != nil {
		g.reject(w, fmt.Errorf("malformed $filter timestamp in %q: %w", filter, err))
		return nil, false
	}
	return &ts, true
}

// reject records the first bad filter and answers the way Graph would.
func (g *graphFilterFake) reject(w http.ResponseWriter, err error) {
	g.mu.Lock()
	if g.err == nil {
		g.err = err
	}
	g.mu.Unlock()
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// check fails the test if the fake ever saw a filter Graph would have rejected.
func (g *graphFilterFake) check(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	require.NoError(t, g.err)
}

// roborev raised that the overlap is fetched under opts.Limit, so preexisting
// boundary messages can exhaust the cap before a newly arrived tied message is
// returned. That is real, but it is not data loss: a truncated read never
// advances the cursor (see the truncated check in syncChats), so the tie stays
// in range and any unlimited run recovers it.
//
// Tied messages have no deterministic secondary ordering, so the fake returns
// them in a fixed order to make the truncation deterministic; the assertions
// below do not depend on which tied message the cap happens to admit.
func TestLimitedChatSyncDoesNotStrandTiedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	const tieTS = "2026-01-01T00:00:00.123Z"
	const chatID = "19:tielimit@thread.v2"
	tie, err := time.Parse(time.RFC3339Nano, tieTS)
	require.NoError(err)

	var lateArrived atomic.Bool
	var filters graphFilterFake

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"` + chatID + `","chatType":"oneOnOne","topic":"TieLimit"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			ids := []string{"ta1", "ta2"}
			if lateArrived.Load() {
				ids = append(ids, "tlate")
			}
			cutoff, ok := filters.cutoff(w, r.URL.Query().Get("$filter"))
			if !ok {
				return
			}
			var out []string
			if cutoff == nil || tie.After(*cutoff) {
				for _, id := range ids {
					out = append(out, `{"id":"`+id+`","createdDateTime":"`+tieTS+
						`","lastModifiedDateTime":"`+tieTS+
						`","body":{"contentType":"text","content":"`+id+`"}}`)
				}
			}
			_, _ = w.Write([]byte(`{"value":[` + strings.Join(out, ",") + `]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
	base := ImportOptions{Email: "me@example.com", IncludeChannels: false}

	_, err = imp.Import(context.Background(), base)
	require.NoError(err)
	require.Equal(2, teamsMessageCount(t, st), "first sync archives both tied messages")

	// A third message arrives sharing the cursor timestamp; the cap is smaller
	// than the number of tied messages now in range.
	lateArrived.Store(true)
	limited := base
	limited.Limit = 1
	for i := range 3 {
		_, lerr := imp.Import(context.Background(), limited)
		require.NoErrorf(lerr, "limited sync %d", i)
	}

	// Guard against the test going vacuous: the cap must actually be deferring
	// the late message, otherwise the assertions below prove nothing.
	require.Equal(2, teamsMessageCount(t, st),
		"the cap should still be deferring the late tied message at this point")

	// That deferral is fine. What must hold is that the cursor did not move
	// past the deferred message.
	cursor := chatCursorAfterLastSync(t, st, chatID)
	require.NotEmpty(cursor, "cursor exists after the unlimited first sync")
	parsed, perr := time.Parse(time.RFC3339Nano, cursor)
	require.NoError(perr)
	assert.False(parsed.After(tie),
		"a capped run must not advance the cursor past the tied timestamp (cursor=%s)", cursor)

	// Therefore an unlimited run still recovers it: nothing was lost.
	_, err = imp.Import(context.Background(), base)
	require.NoError(err)
	assert.Equal(3, teamsMessageCount(t, st),
		"an unlimited sync recovers the late tied message the cap had deferred")

	filters.check(t)
}

func teamsMessageCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type='teams'`).Scan(&n))
	return n
}

func chatCursorAfterLastSync(t *testing.T, st *store.Store, chatID string) string {
	t.Helper()
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(t, err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(t, err)
	require.True(t, run.CursorAfter.Valid)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(t, err)
	return state.ChatCursor(chatID)
}
