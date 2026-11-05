package teams

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func messageAttachmentFlags(t *testing.T, st *store.Store, messageID int64) (bool, int) {
	t.Helper()
	var hasAttachments bool
	var attachmentCount int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT has_attachments, attachment_count FROM messages WHERE id = ?`), messageID,
	).Scan(&hasAttachments, &attachmentCount))
	return hasAttachments, attachmentCount
}

// A cancel that lands after the backfill replaces a message's inline
// attachment rows must still refresh its attachment flags. The message's
// media is then complete, so --only-incomplete runs never revisit it.
//
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestBackfillInlineMediaCancelAfterReplaceRefreshesFlags(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/1/$value" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
			return
		}
		http.Error(w, "404", http.StatusNotFound)
	}))
	defer srv.Close()

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "oneOnOne", "DM")
	require.NoError(err)
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: src.ID, SourceMessageID: "m1", MessageType: "teams",
	})
	require.NoError(err)
	bodyHTML := `<div><img src="` + srv.URL + `/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/1/$value"></div>`
	require.NoError(st.UpsertMessageBody(msgID,
		sql.NullString{String: "body", Valid: true}, sql.NullString{String: bodyHTML, Valid: true}))

	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var rows int
		require.NoError(st.DB().QueryRowContext(queryCtx, st.Rebind(
			`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), msgID).Scan(&rows))
		return rows > 0
	})

	client := NewClient(srv.URL+"/v1.0", func(context.Context) (string, error) { return "t", nil }, 50)
	sum, _ := NewImporter(st, client).BackfillInlineMedia(ctx, ImportOptions{
		Email: "me@example.com", AttachmentsDir: t.TempDir(),
	})
	require.True(canceled(), "the cancel must land after the attachment rows commit")
	assert.EqualValues(1, sum.InlineImagesCopied)

	hasAttachments, attachmentCount := messageAttachmentFlags(t, st, msgID)
	assert.True(hasAttachments, "has_attachments must match the stored inline image")
	assert.Equal(1, attachmentCount, "attachment_count must match the stored inline image")
}

// A cancel that lands while a synced message's reactions are resolved, after
// its inline image rows commit, must still refresh its attachment flags. The
// body shows one image twice: the message row counts both references until
// the refresh counts the single stored row.
func TestImportCancelAfterInlineImageRefreshesFlags(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General"}]}`))
		case strings.HasSuffix(r.URL.Path, "/hostedContents/1/$value"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			img := serverURL + `/teams/team1/channels/chanA/messages/root/hostedContents/1/$value`
			_, _ = w.Write([]byte(`{"value":[{"id":"root","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z",
			  "body":{"contentType":"html","content":"<img src=\"` + img + `\"><img src=\"` + img + `\">"},
			  "reactions":[{"reactionType":"like","createdDateTime":"2025-02-01T00:00:02Z","user":{"user":{"id":"reactor","displayName":"Reactor","userIdentityType":"aadUser"}}}]}]}`))
		case r.URL.Path == "/users/reactor":
			// The reaction is resolved after the inline image rows commit
			// and before the message's attachment flags are refreshed.
			cancel()
			http.Error(w, "404", http.StatusNotFound)
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)
	client := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)

	_, err := NewImporter(st, client).Import(ctx, ImportOptions{
		Email: "me@example.com", IncludeChannels: true, AttachmentsDir: t.TempDir(),
	})
	require.ErrorIs(err, context.Canceled)

	var msgID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM messages WHERE source_message_id LIKE ?`), "%root").Scan(&msgID))
	var rows int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM attachments WHERE message_id = ? AND content_hash <> ''`), msgID).Scan(&rows))
	require.Equal(1, rows, "the inline image row commits before the cancel")
	hasAttachments, attachmentCount := messageAttachmentFlags(t, st, msgID)
	assert.True(hasAttachments, "has_attachments must match the stored inline image")
	assert.Equal(1, attachmentCount, "attachment_count must match the stored inline image")
}

// A cancel that lands right after a synced message's inline image rows commit
// fails the raw-archive write that follows, which ends the message's persist
// early. The attachment flags must still be refreshed on that exit. The body
// shows one image twice: the message row counts both references until the
// refresh counts the single stored row.
//
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportCancelAfterInlineImageCommitRefreshesFlags(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[]}`))
		case r.URL.Path == "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_, _ = w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General"}]}`))
		case strings.HasSuffix(r.URL.Path, "/hostedContents/1/$value"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			img := serverURL + `/teams/team1/channels/chanA/messages/root/hostedContents/1/$value`
			_, _ = w.Write([]byte(`{"value":[{"id":"root","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z",
			  "body":{"contentType":"html","content":"<img src=\"` + img + `\"><img src=\"` + img + `\">"}}]}`))
		default:
			http.Error(w, "404", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	defer srv.Close()
	st := testutil.NewTestStore(t)
	client := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)

	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var rows int
		require.NoError(st.DB().QueryRowContext(queryCtx,
			`SELECT COUNT(*) FROM attachments WHERE content_hash <> ''`).Scan(&rows))
		return rows > 0
	})

	_, err := NewImporter(st, client).Import(ctx, ImportOptions{
		Email: "me@example.com", IncludeChannels: true, AttachmentsDir: t.TempDir(),
	})
	require.ErrorIs(err, context.Canceled)
	require.True(canceled(), "the cancel must land after the inline image rows commit")

	var msgID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM messages WHERE source_message_id LIKE ?`), "%root").Scan(&msgID))
	var rows int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), msgID).Scan(&rows))
	require.Equal(1, rows, "the inline image row commits before the cancel")
	hasAttachments, attachmentCount := messageAttachmentFlags(t, st, msgID)
	assert.True(hasAttachments, "has_attachments must match the stored inline image")
	assert.Equal(rows, attachmentCount, "attachment_count must match the stored inline image")
}
