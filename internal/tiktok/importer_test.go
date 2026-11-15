package tiktok

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCommentSyncResumesDeduplicatesAndLinksReplies(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	paths := []string{}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/business/get/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example-account","display_name":"Example Account"}}`)
		case "/business/video/list/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"videos":[{"item_id":9007199254740993,"caption":"Example video","create_time":1700000000}],"has_more":false}}`)
		case "/business/comment/list/":
			if r.URL.Query().Get("cursor") == "7" {
				_, _ = fmt.Fprint(w, `{"code":0,"data":{"comments":[{"comment_id":9007199254740995,"video_id":9007199254740993,"text":"Example comment","unique_identifier":"actor-example","display_name":"Example Viewer","replies":1,"create_time":1700000001}],"has_more":false}}`)
			} else {
				// Empty pages with has_more are valid; do not treat these as done.
				_, _ = fmt.Fprint(w, `{"code":0,"data":{"comments":[],"cursor":7,"has_more":true}}`)
			}
		case "/business/comment/reply/list/":
			assertions.Equal("9007199254740995", r.URL.Query().Get("comment_id"))
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"comments":[{"comment_id":9007199254740997,"parent_comment_id":9007199254740995,"video_id":9007199254740993,"text":"Example reply","unique_identifier":"actor-example","display_name":"Example Viewer","create_time":1700000002},{"comment_id":"9007199254740997","parent_comment_id":"9007199254740995","video_id":"9007199254740993","text":"Example reply","unique_identifier":"actor-example","display_name":"Example Viewer","create_time":1700000002}],"has_more":false}}`)
		default:
			http.NotFound(w, r)
		}
	})
	imp := NewImporter(st, c)
	src, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	var added int
	var complete bool
	for range 10 {
		sum, err := imp.Import(t.Context(), "app-example", "account-example", ImportOptions{MaxRequests: 2})
		requirements.NoError(err)
		added += sum.MessagesAdded
		if !sum.Partial {
			complete = true
			break
		}
	}
	requirements.True(complete)
	assertions.Equal(3, added)
	count, err := st.CountMessagesForSource(src.ID)
	requirements.NoError(err)
	assertions.EqualValues(3, count)
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, commentFormat, []string{"video:9007199254740993", "9007199254740995", "9007199254740997"})
	requirements.NoError(err)
	requirements.Len(rows, 3)
	var replyParent, commentParent int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), "SELECT reply_to_message_id FROM messages WHERE id = ?", rows["9007199254740997"].ID).Scan(&replyParent))
	requirements.NoError(st.DB().QueryRowContext(t.Context(), "SELECT reply_to_message_id FROM messages WHERE id = ?", rows["9007199254740995"].ID).Scan(&commentParent))
	assertions.Equal(rows["9007199254740995"].ID, replyParent)
	assertions.Equal(rows["video:9007199254740993"].ID, commentParent)
	assertions.Equal(rows["9007199254740995"].SenderID, rows["9007199254740997"].SenderID)
	assertions.True(rows["video:9007199254740993"].SourceIsFromMe.Bool)
	assertions.Contains(paths, "/business/comment/reply/list/")
	// This watermark is the message-facts invalidation boundary used by an
	// already-published analytics cache.
	var baseline int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	sum, err := imp.Import(t.Context(), "app-example", "account-example", ImportOptions{})
	requirements.NoError(err)
	assertions.False(sum.Partial)
	assertions.Zero(sum.MessagesAdded)
	assertions.Equal(4, sum.MessagesProcessed)
	var invalidations int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, baseline).Scan(&invalidations))
	assertions.Zero(invalidations, "unchanged sync must leave cached message facts valid")
}

func TestCommentSyncRejectsLoopingCursor(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/business/get/" {
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"videos":[],"cursor":0,"has_more":true}}`)
	})
	imp := NewImporter(st, c)
	_, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	sum, err := imp.Import(t.Context(), "app-example", "account-example", ImportOptions{})
	requirements.ErrorContains(err, "cursor")
	assert.True(t, sum.Partial)
}

func TestCommentRequestBudgetCountsRetriedHTTPAttempts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	videoCalls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/business/get/" {
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example"}}`)
			return
		}
		videoCalls++
		if videoCalls == 1 {
			http.Error(w, "synthetic transient error", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"videos":[],"has_more":false}}`)
	})
	c.wait = func(context.Context, time.Duration) error { return nil }
	imp := NewImporter(st, c)
	_, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	sum, err := imp.Import(t.Context(), "app-example", "account-example", ImportOptions{MaxRequests: 2})
	requirements.NoError(err)
	assertions.True(sum.Partial)
	assertions.Equal(2, sum.Requests)
	assertions.Equal(1, videoCalls, "retry must pause before dispatch exceeds the budget")
	sum, err = imp.Import(t.Context(), "app-example", "account-example", ImportOptions{MaxRequests: 2})
	requirements.NoError(err)
	assertions.False(sum.Partial)
	assertions.Equal(2, sum.Requests)
	assertions.Equal(2, videoCalls)
}

func TestCommentPersistSnapshots(t *testing.T) {
	for _, kind := range []string{"unchanged", "text", "raw evidence", "large integer evidence", "large float evidence", "large exponent evidence", "sender", "display name", "timestamp", "body repair", "recipient repair", "metadata repair", "equivalent JSON", "sparse evidence", "owner", "failed write retry", "identity mismatch"} {
		t.Run(kind, func(t *testing.T) {
			st := testutil.NewSQLiteTestStore(t)
			src, err := st.GetOrCreateSourceContext(t.Context(), CommentsSourceType, SourceIdentifier("app-example", "account-example"))
			require.NoError(t, err)
			r := &commentRun{imp: NewImporter(st, nil), source: src, app: "app-example", account: "account-example", sum: &ImportSummary{}}
			require.NoError(t, r.persistVideo(t.Context(), Video{ID: "video-example", Caption: "Example video", CreateTime: 1700000000}))
			comment := Comment{ID: "comment-example", VideoID: "video-example", ActorID: "viewer-example", DisplayName: "Example Viewer", Text: "Example comment", CreateTime: 1700000001, Extra: map[string]jsontext.Value{"extra_b": []byte(`2`), "extra_a": []byte(`9007199254740993`)}}
			switch kind {
			case "large integer evidence":
				comment.Extra["precise"] = []byte(`9007199254740992`)
			case "large float evidence":
				comment.Extra["precise"] = []byte(`9007199254740992.0`)
			case "large exponent evidence":
				comment.Extra["precise"] = []byte(`9.007199254740992e15`)
			}
			require.NoError(t, r.persistComment(t.Context(), comment, "video-example", ""))
			rows, err := st.StoredMessagesContext(t.Context(), src.ID, commentFormat, []string{string(comment.ID)})
			require.NoError(t, err)
			original := rows[string(comment.ID)]
			switch kind {
			case "text", "failed write retry":
				comment.Text = "Updated comment"
			case "raw evidence":
				comment.Status = "hidden"
			case "large integer evidence":
				comment.Extra["precise"] = []byte(`9007199254740993`)
			case "large float evidence":
				comment.Extra["precise"] = []byte(`9007199254740993.0`)
			case "large exponent evidence":
				comment.Extra["precise"] = []byte(`9.007199254740993e15`)
			case "sender":
				comment.ActorID = "other-viewer-example"
			case "display name":
				comment.DisplayName = "Updated Viewer"
			case "timestamp":
				comment.CreateTime++
			case "sparse evidence":
				comment.ActorID, comment.DisplayName, comment.CreateTime = "", "", 0
			case "owner":
				comment.Owner = true
			case "equivalent JSON":
				encoded, encodeErr := json.Marshal(comment, json.Deterministic(true))
				require.NoError(t, encodeErr)
				err = st.UpsertMessageRawWithFormatContext(t.Context(), original.ID, append([]byte("  "), encoded...), commentFormat)
				require.NoError(t, err)
				err = st.SetMessageMetadataContext(t.Context(), original.ID, sql.NullString{Valid: true, String: `{ "parent":"video:video-example", "video":"video-example", "account":"account-example", "app":"app-example" }`})
			case "body repair":
				_, err = st.DB().ExecContext(t.Context(), `DELETE FROM message_bodies WHERE message_id = ?`, original.ID)
			case "recipient repair":
				_, err = st.DB().ExecContext(t.Context(), `DELETE FROM message_recipients WHERE message_id = ?`, original.ID)
			case "metadata repair":
				_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET metadata = json_set(metadata, '$.extra', 'old') WHERE id = ?`, original.ID)
			case "identity mismatch":
				_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET metadata = json_set(metadata, '$.account', 'other-account') WHERE id = ?`, original.ID)
			}
			require.NoError(t, err)
			var baseline int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
			if kind == "failed write retry" {
				_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_comment_recipient BEFORE INSERT ON message_recipients BEGIN SELECT RAISE(ABORT, 'synthetic recipient failure'); END`)
				require.NoError(t, err)
				require.ErrorContains(t, r.persistComment(t.Context(), comment, "video-example", ""), "synthetic recipient failure")
				rows, err := st.StoredMessagesContext(t.Context(), src.ID, commentFormat, []string{string(comment.ID)})
				require.NoError(t, err)
				assert.Equal(t, original, rows[string(comment.ID)], "failed write rolls back the snapshot")
				var changes int
				require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ?`, baseline).Scan(&changes))
				assert.Zero(t, changes, "failed write rolls back cache invalidations")
				assert.Equal(t, 2, r.sum.MessagesProcessed)
				_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_comment_recipient`)
				require.NoError(t, err)
			}
			err = r.persistComment(t.Context(), comment, "video-example", "")
			if kind == "identity mismatch" {
				require.ErrorContains(t, err, "different thread or parent")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 3, r.sum.MessagesProcessed)
			assert.Equal(t, 2, r.sum.MessagesAdded)
			var invalidations int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, baseline).Scan(&invalidations))
			if kind == "unchanged" || kind == "equivalent JSON" {
				assert.Zero(t, invalidations)
			} else {
				assert.Positive(t, invalidations, "genuine changes still invalidate cached facts")
			}
			rows, err = st.StoredMessagesContext(t.Context(), src.ID, commentFormat, []string{string(comment.ID)})
			require.NoError(t, err)
			current := rows[string(comment.ID)]
			assert.Equal(t, original.ID, current.ID)
			assert.Equal(t, comment.Text, current.Snippet.String)
			if kind == "sparse evidence" {
				assert.Equal(t, original.SentAt, current.SentAt)
				assert.Equal(t, original.SenderID, current.SenderID)
			} else {
				assert.Equal(t, int64(comment.CreateTime), current.SentAt.Time.Unix())
			}
			assert.Equal(t, comment.Owner, current.SourceIsFromMe.Bool)
			parent, err := st.GetMessageReplyToMessageIDContext(t.Context(), original.ID)
			require.NoError(t, err)
			root, err := st.StoredMessagesContext(t.Context(), src.ID, commentFormat, []string{"video:video-example"})
			require.NoError(t, err)
			assert.Equal(t, sql.NullInt64{Valid: true, Int64: root["video:video-example"].ID}, parent)
			var saved Comment
			require.NoError(t, json.Unmarshal(current.Raw, &saved))
			assert.Equal(t, comment, saved)
			matches, err := st.MessageContentMatchesContext(t.Context(), original.ID, sql.NullString{String: comment.Text, Valid: true}, store.FTSDoc{Body: comment.Text, FromAddr: comment.DisplayName})
			require.NoError(t, err)
			assert.True(t, matches)
			matches, err = st.MessageRecipientsMatchContext(t.Context(), original.ID, []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{current.SenderID.Int64}, DisplayNames: []string{comment.DisplayName}}})
			require.NoError(t, err)
			assert.True(t, matches)
			metadata, err := st.GetMessageMetadataContext(t.Context(), original.ID)
			require.NoError(t, err)
			assert.JSONEq(t, `{"app":"app-example","account":"account-example","video":"video-example","parent":"video:video-example"}`, metadata.String)
			// The next retry is a no-op, including after a repaired failed write.
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
			require.NoError(t, r.persistComment(t.Context(), comment, "video-example", ""))
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, baseline).Scan(&invalidations))
			assert.Zero(t, invalidations)
		})
	}
}
