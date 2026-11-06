package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonMergeRelatedDriftKeepsDerivedRefresh(t *testing.T) {
	t.Run("meeting and new mail", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		root := t.TempDir()
		path, cache := filepath.Join(root, "test.db"), filepath.Join(root, "analytics")
		st, err := store.OpenForTest(path)
		require.NoError(err)
		t.Cleanup(func() { require.NoError(st.Close()) })
		require.NoError(st.InitSchema())
		firstID, err := st.EnsureParticipant("first@example.test", "First", "example.test")
		require.NoError(err)
		secondID, err := st.EnsureParticipant("second@example.test", "Second", "example.test")
		require.NoError(err)
		source, err := st.GetOrCreateSource("gmail", "owner@example.test")
		require.NoError(err)
		messageID, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: "mail", MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: "mail", ConversationType: "email"},
			Recipients:   []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{firstID, secondID}}},
		})
		require.NoError(err)
		_, err = st.DB().Exec(`INSERT INTO labels (source_id, source_label_id, name) VALUES (?, 'unread', 'Unread')`, source.ID)
		require.NoError(err)
		_, err = st.DB().Exec(`INSERT INTO message_labels (message_id, label_id) SELECT ?, id FROM labels`, messageID)
		require.NoError(err)
		first, _, err := st.CreatePersonFromParticipant(firstID)
		require.NoError(err)
		second, _, err := st.CreatePersonFromParticipant(secondID)
		require.NoError(err)
		var event, note int64
		event, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "event", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar"}, RawFormat: "gcal_json", RawMIME: []byte(`{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"}}`)})
		require.NoError(err)
		note, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "note", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"}, RawFormat: "plaud_json", RawMIME: []byte(`{}`)})
		require.NoError(err)
		_, err = buildCache(path, cache, true)
		require.NoError(err)
		require.NoError(st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
		_, err = st.DB().Exec(`DELETE FROM message_labels WHERE message_id = (SELECT min(message_id) FROM message_labels)`)
		require.NoError(err)
		_, err = st.DB().Exec(`INSERT INTO messages (source_id, source_message_id, conversation_id, message_type, sent_at) SELECT source_id, 'new-mail', conversation_id, 'email', sent_at FROM messages LIMIT 1`)
		require.NoError(err)
		assert.True(cacheNeedsBuild(path, cache).HasRelatedRowDrift)
		previous := executeBuildCacheSubprocessMode
		var modes []buildCacheMode
		executeBuildCacheSubprocessMode = func(_ context.Context, mode buildCacheMode) error {
			modes = append(modes, mode)
			switch mode {
			case buildCacheModeDerived:
				_, err := buildCacheDerivedOnly(path, cache)
				return err
			case buildCacheModeFull:
				_, err := buildCache(path, cache, true)
				return err
			default:
				return fmt.Errorf("unexpected cache build mode: %v", mode)
			}
		}
		t.Cleanup(func() { executeBuildCacheSubprocessMode = previous })
		srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st, analyticsDir: cache}, Logger: slog.New(slog.DiscardHandler)})
		request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/people/%d/merge", first.ID), strings.NewReader(fmt.Sprintf(`{"absorbed_person_id":%d}`, second.ID)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("If-Match", fmt.Sprintf(`"person-%d-r%d", "person-%d-r%d"`, first.ID, first.Revision, second.ID, second.Revision))
		request.Header.Set("Idempotency-Key", "merge-related-drift")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var body struct {
			CacheState string `json:"cache_state"`
		}
		require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
		assert.Equal("ready", body.CacheState)
		assert.Equal([]buildCacheMode{buildCacheModeDerived, buildCacheModeFull}, modes)
		var pending int
		require.NoError(st.DB().QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&pending))
		assert.Zero(pending, "the full build acknowledges the meeting repair")
		assert.False(cacheNeedsBuild(path, cache).NeedsBuild)
		db, err := sql.Open("duckdb", "")
		require.NoError(err)
		var key string
		require.NoError(db.QueryRow(`SELECT meeting_key FROM read_parquet(?) WHERE message_id=?`, filepath.Join(cache, "meeting_keys", "*.parquet"), note).Scan(&key))
		assert.Equal(fmt.Sprintf("evt:%d/", event), key)
		require.NoError(db.Close())
	})
}
