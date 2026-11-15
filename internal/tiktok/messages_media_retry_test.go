package tiktok

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The fake provider never opens a network connection. Successful reads are
// small synthetic streams through the production attachment writer.
type pendingMediaAPI struct {
	messages   []BusinessMessage
	attempts   []ID
	fail       bool
	body       string
	accountErr error
	open       func(context.Context, BusinessMessage) error
}

func (a *pendingMediaAPI) Account(context.Context, string) (Account, error) {
	return Account{Username: "fixture"}, a.accountErr
}
func (a *pendingMediaAPI) Conversations(_ context.Context, _, kind, _ string, _ int) (ConversationPage, error) {
	page := ConversationPage{}
	if kind == "SINGLE" {
		seen := map[ID]bool{}
		for _, message := range a.messages {
			if !seen[message.ConversationID] {
				page.Conversations = append(page.Conversations, BusinessConversation{ID: message.ConversationID})
				seen[message.ConversationID] = true
			}
		}
	}
	return page, nil
}
func (a *pendingMediaAPI) Messages(_ context.Context, _, conversation string) (MessagePage, error) {
	page := MessagePage{}
	for _, message := range a.messages {
		if string(message.ConversationID) == conversation {
			page.Messages = append(page.Messages, message)
		}
	}
	return page, nil
}
func (a *pendingMediaAPI) OpenMessageMedia(ctx context.Context, _ string, m BusinessMessage, _ int64) (*MessageMediaResponse, error) {
	a.attempts = append(a.attempts, m.ID)
	if a.open != nil {
		if err := a.open(ctx, m); err != nil {
			return nil, err
		}
	}
	if a.fail {
		return nil, errors.New("synthetic provider unavailable")
	}
	body := a.body
	if body == "" {
		body = "synthetic media"
	}
	return &MessageMediaResponse{Body: io.NopCloser(strings.NewReader(body)), ContentType: "image/png"}, nil
}
func pendingMessage(i int) BusinessMessage {
	return BusinessMessage{ID: ID(fmt.Sprintf("media-%03d", i)), ConversationID: "conversation-example", Timestamp: 1700000000001, Type: "image", Image: &MessageMedia{ID: ID(fmt.Sprintf("image-%03d", i))}}
}
func seedPendingMedia(t *testing.T, count int) (*store.Store, *pendingMediaAPI, *MessageImporter, *store.Source) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	api := &pendingMediaAPI{fail: true}
	for i := 0; i < count; i++ {
		message := pendingMessage(i)
		// Respect the real pull projection: no more than 20 messages per conversation.
		if i >= 20 {
			message.ConversationID = ID(fmt.Sprintf("conversation-%02d-example", i/20))
		}
		api.messages = append(api.messages, message)
	}
	imp := NewMessageImporter(st, api, MessageImportOptions{AttachmentsDir: t.TempDir()})
	source, err := imp.Register(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	sum, err := imp.Import(t.Context(), "app-example", "account-example")
	require.Error(t, err)
	require.Equal(t, count, sum.MessagesAdded)
	api.messages = nil
	api.attempts = nil
	return st, api, imp, source
}

func TestPendingMediaRecoversAfterEmptyPull(t *testing.T) {
	st, api, imp, source := seedPendingMedia(t, 1)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_id = ?`, source.ID).Scan(&id))
	// The pull client's typed projection fills this field but native JSON need
	// not contain it. Preserve the archived bytes while rebuilding the request.
	raw := []byte(`{"message_id":"media-000","timestamp":1700000000001,"type":"image","image":{"media_id":"image-000"}}`)
	require.NoError(t, st.UpsertMessageRawWithFormatContext(t.Context(), id, raw, messageFormat))
	sum, err := imp.Import(t.Context(), "app-example", "account-example")
	require.Error(t, err)
	assert.True(t, sum.Partial)
	var status string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`, source.ID).Scan(&status))
	assert.Equal(t, store.SyncStatusFailed, status)
	assert.Equal(t, []ID{"media-000"}, api.attempts)
	api.attempts = nil
	api.fail = false
	api.open = func(_ context.Context, m BusinessMessage) error {
		assert.Equal(t, ID("conversation-example"), m.ConversationID)
		return nil
	}
	sum, err = imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	assert.False(t, sum.Partial)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`, source.ID).Scan(&status))
	assert.Equal(t, store.SyncStatusCompleted, status)
	assert.Zero(t, sum.MessagesProcessed)
	assert.Equal(t, 1, sum.MediaStored)
	assert.Equal(t, []ID{"media-000"}, api.attempts)
	got, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, raw, got)
	pending, err := st.HasPendingTikTokMediaContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.False(t, pending)
	var state, hash string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT attachment_state, content_hash FROM attachments WHERE message_id = ?`, id).Scan(&state, &hash))
	assert.Equal(t, "stored", state)
	assert.NotEmpty(t, hash)
	// Completed sync clears the ordinary source cursor, but not retry progress.
	var marker string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, fmt.Sprintf("tiktok.media_sweep:%d", source.ID)).Scan(&marker))
	assert.Contains(t, marker, `"through":`)
	api.attempts = nil
	_, err = imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	assert.Empty(t, api.attempts)
}

func TestPendingMediaBudgetFairnessAndWebhookIsolation(t *testing.T) {
	st, api, imp, source := seedPendingMedia(t, pendingMediaBudget+2)
	sum, err := imp.Import(t.Context(), "app-example", "account-example")
	require.Error(t, err)
	assert.True(t, sum.Partial)
	assert.Len(t, api.attempts, pendingMediaBudget)
	assert.Equal(t, ID("media-019"), api.attempts[pendingMediaBudget-1])
	var before string
	key := fmt.Sprintf("tiktok.media_sweep:%d", source.ID)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, key).Scan(&before))
	// A new webhook message is not a historical retry, and does not move the
	// finite pass's upper bound even when it appends a new pending item.
	api.attempts = nil
	_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, pendingMessage(99))
	require.Error(t, err)
	assert.Equal(t, []ID{"media-099"}, api.attempts)
	var after string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, key).Scan(&after))
	assert.Equal(t, before, after)
	api.attempts = nil
	api.fail = false
	sum, err = imp.Import(t.Context(), "app-example", "account-example")
	require.ErrorIs(t, err, errPendingMessageMedia)
	assert.True(t, sum.Partial)
	assert.Equal(t, 2, sum.MediaStored)
	assert.Equal(t, []ID{"media-020", "media-021"}, api.attempts)
	api.attempts = nil
	sum, err = imp.Import(t.Context(), "app-example", "account-example")
	require.ErrorIs(t, err, errPendingMessageMedia)
	assert.Equal(t, pendingMediaBudget, sum.MediaStored)
	api.attempts = nil
	sum, err = imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	assert.Equal(t, []ID{"media-099"}, api.attempts)
	assert.False(t, sum.Partial)
}

func TestPendingMediaPreflightAndCancellation(t *testing.T) {
	st, api, imp, source := seedPendingMedia(t, 1)
	api.accountErr = errors.New("synthetic account unavailable")
	_, err := imp.Import(t.Context(), "app-example", "account-example")
	require.Error(t, err)
	assert.Empty(t, api.attempts)
	api.accountErr = nil
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	api.open = func(context.Context, BusinessMessage) error { cancel(); return ctx.Err() }
	sum, err := imp.Import(ctx, "app-example", "account-example")
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, sum.Partial)
	state, err := st.LoadTikTokMediaSweepContext(t.Context(), source.ID, source.Identifier)
	require.NoError(t, err)
	assert.Zero(t, state.After)
	api.open = nil
	api.fail = false
	api.attempts = nil
	_, err = imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	assert.Equal(t, []ID{"media-000"}, api.attempts)
}

func TestPendingMediaRejectsIneligibleArchiveEvidence(t *testing.T) {
	tests := []struct {
		name, query string
		args        []any
	}{
		{"metadata app", `UPDATE messages SET metadata = REPLACE(metadata, 'app-example', 'other-app')`, nil},
		{"source key", `UPDATE messages SET source_message_id = 'wrong-key'`, nil},
		{"conversation", `UPDATE conversations SET source_conversation_id = 'wrong-conversation'`, nil},
		{"part", `UPDATE attachments SET source_part_key = 'tiktok:video'`, nil},
		{"raw identity", `UPDATE message_raw SET raw_data = ?, compression = ''`, []any{[]byte(`{"message_id":"other-message","type":"image"}`)}},
		{"raw format", `UPDATE message_raw SET raw_format = 'other-json'`, nil},
		{"raw compression", `UPDATE message_raw SET compression = 'other'`, nil},
		{"raw size", `UPDATE message_raw SET raw_data = ?, compression = ''`, []any{[]byte(`{"message_id":"media-000","conversation_id":"conversation-example","timestamp":1700000000001,"type":"image","image":{"media_id":"image-000"},"padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, api, imp, source := seedPendingMedia(t, 1)
			_, err := st.DB().ExecContext(t.Context(), tt.query, tt.args...)
			require.NoError(t, err)
			api.fail = false
			sum, err := imp.Import(t.Context(), "app-example", "account-example")
			require.Error(t, err)
			assert.True(t, sum.Partial)
			assert.Empty(t, api.attempts)
			state, err := st.LoadTikTokMediaSweepContext(t.Context(), source.ID, source.Identifier)
			require.NoError(t, err)
			// A finished pass is restarted on load, rather than stuck on an invalid row.
			assert.Zero(t, state.After)
		})
	}
}

func TestPendingMediaProvenanceAndSourceIsolation(t *testing.T) {
	for _, tt := range []struct{ name, query string }{
		{"preserved attachment", `INSERT INTO source_merge_preserved_attachments(attachment_id) SELECT id FROM attachments`},
		{"archive-only message", `INSERT INTO source_merge_archive_only_messages(message_id) SELECT id FROM messages`},
		{"unavailable", `UPDATE attachments SET attachment_state = 'unavailable'`},
		{"other provider part", `UPDATE attachments SET source_part_key = 'other:image'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, api, imp, source := seedPendingMedia(t, 1)
			_, err := st.DB().ExecContext(t.Context(), tt.query)
			require.NoError(t, err)
			api.fail = false
			sum, err := imp.Import(t.Context(), "app-example", "account-example")
			require.NoError(t, err)
			assert.False(t, sum.Partial)
			assert.Empty(t, api.attempts)
			_, err = st.ReadTikTokMediaCandidateContext(t.Context(), source.ID, 1)
			require.Error(t, err)
		})
	}
	t.Run("other source", func(t *testing.T) {
		st, api, imp, source := seedPendingMedia(t, 1)
		other, err := imp.Register(t.Context(), "other-app", "account-example")
		require.NoError(t, err)
		api.fail = false
		_, err = imp.Import(t.Context(), "other-app", "account-example")
		require.NoError(t, err)
		assert.Empty(t, api.attempts)
		var id int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_id = ?`, source.ID).Scan(&id))
		_, err = st.ReadTikTokMediaCandidateContext(t.Context(), other.ID, id)
		require.Error(t, err)
	})
}

func TestPendingMediaProgressFenceAndVersion(t *testing.T) {
	st, _, _, source := seedPendingMedia(t, 1)
	oldID, err := st.StartSyncContext(t.Context(), source.ID, "fixture")
	require.NoError(t, err)
	stale := st.ScopedToSync(source.ID, oldID)
	state := store.TikTokMediaSweep{Version: 1, Source: source.Identifier, Through: 1}
	require.NoError(t, stale.SaveTikTokMediaSweepContext(t.Context(), source.ID, state))
	require.NoError(t, st.FailSyncContext(t.Context(), oldID, "synthetic interruption"))
	currentID, err := st.StartSyncContext(t.Context(), source.ID, "fixture")
	require.NoError(t, err)
	defer func() { _ = st.FailSyncContext(context.WithoutCancel(t.Context()), currentID, "fixture complete") }()
	state.After = 1
	require.ErrorIs(t, stale.SaveTikTokMediaSweepContext(t.Context(), source.ID, state), store.ErrSyncRunSuperseded)
	got, err := st.LoadTikTokMediaSweepContext(t.Context(), source.ID, source.Identifier)
	require.NoError(t, err)
	assert.Zero(t, got.After)
	_, err = st.LoadTikTokMediaSweepContext(t.Context(), source.ID, "other-identity")
	require.Error(t, err)
	state.Version = 2
	require.NoError(t, st.ScopedToSync(source.ID, currentID).SaveTikTokMediaSweepContext(t.Context(), source.ID, state))
	_, err = st.LoadTikTokMediaSweepContext(t.Context(), source.ID, source.Identifier)
	require.Error(t, err)
}

func TestPendingMediaBoundedDecodeAndFinalization(t *testing.T) {
	t.Run("decoded raw bound", func(t *testing.T) {
		st, api, imp, source := seedPendingMedia(t, 1)
		var id int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_id = ?`, source.ID).Scan(&id))
		require.NoError(t, st.UpsertMessageRawWithFormatContext(t.Context(), id, []byte(strings.Repeat("x", (1<<20)+1)), messageFormat))
		api.fail = false
		sum, err := imp.Import(t.Context(), "app-example", "account-example")
		require.ErrorContains(t, err, "decode limit")
		assert.True(t, sum.Partial)
		assert.Empty(t, api.attempts)
	})
	t.Run("completion error", func(t *testing.T) {
		st, api, imp, _ := seedPendingMedia(t, 1)
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fixture_fail_completion BEFORE UPDATE OF status ON sync_runs WHEN NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'synthetic completion failure'); END`)
		require.NoError(t, err)
		api.fail = false
		sum, err := imp.Import(t.Context(), "app-example", "account-example")
		require.ErrorContains(t, err, "synthetic completion failure")
		assert.Equal(t, 1, sum.MediaStored)
		assert.True(t, sum.Partial, "finalization failures must not report complete after attachment-only writes")
	})
	t.Run("existing byte limit", func(t *testing.T) {
		_, api, imp, _ := seedPendingMedia(t, 1)
		api.fail = false
		imp.options.MaxMediaBytes = 3
		sum, err := imp.Import(t.Context(), "app-example", "account-example")
		require.Error(t, err)
		assert.True(t, sum.Partial)
		assert.Zero(t, sum.MediaStored)
	})
}

func TestPendingMediaOverlapAttemptsEachOccurrenceOnce(t *testing.T) {
	st, api, imp, source := seedPendingMedia(t, 1)
	latest := pendingMessage(0)
	latest.Text = &MessageText{Body: "updated current window text"}
	api.messages = []BusinessMessage{latest}
	api.fail = false
	api.open = func(context.Context, BusinessMessage) error {
		if len(api.attempts) == 1 {
			return errors.New("synthetic first retrieval failure")
		}
		return nil
	}
	sum, err := imp.Import(t.Context(), "app-example", "account-example")
	require.ErrorContains(t, err, "synthetic first retrieval failure")
	assert.True(t, sum.Partial)
	assert.Equal(t, []ID{"media-000"}, api.attempts, "the shallow window must not retry this occurrence a second time")
	assert.Zero(t, sum.MediaStored)
	assert.Equal(t, 1, sum.MessagesProcessed, "current window persistence is independent of the media attempt")
	var id int64
	var snippet string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id, snippet FROM messages WHERE source_id = ?`, source.ID).Scan(&id, &snippet))
	assert.Equal(t, latest.Text.Body, snippet)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	var saved BusinessMessage
	require.NoError(t, json.Unmarshal(raw, &saved))
	assert.Equal(t, latest.Text, saved.Text)
	api.messages = nil
	api.attempts = nil
	api.open = nil
	sum, err = imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	assert.False(t, sum.Partial)
	assert.Equal(t, []ID{"media-000"}, api.attempts)
	assert.Equal(t, 1, sum.MediaStored)
}

func TestPendingMediaOverlapKeepsDistinctAndUndispatchedEvidence(t *testing.T) {
	t.Run("different part kind", func(t *testing.T) {
		_, api, imp, _ := seedPendingMedia(t, 1)
		latest := pendingMessage(0)
		latest.Type, latest.Image, latest.Video = "video", nil, &MessageMedia{ID: "video-example"}
		api.messages = []BusinessMessage{latest}
		api.fail = false
		var kinds []string
		api.open = func(_ context.Context, m BusinessMessage) error {
			kinds = append(kinds, m.Kind())
			if m.Kind() == "image" {
				return errors.New("synthetic old image failure")
			}
			return nil
		}
		sum, err := imp.Import(t.Context(), "app-example", "account-example")
		require.Error(t, err)
		assert.Equal(t, []string{"image", "video"}, kinds)
		assert.Equal(t, 1, sum.MediaStored)
		assert.Equal(t, 1, sum.MessagesProcessed)
	})
	t.Run("invalid historical raw never dispatched", func(t *testing.T) {
		st, api, imp, source := seedPendingMedia(t, 1)
		var id int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_id = ?`, source.ID).Scan(&id))
		require.NoError(t, st.UpsertMessageRawWithFormatContext(t.Context(), id, []byte(`{"message_id":"wrong-message","type":"image"}`), messageFormat))
		api.messages = []BusinessMessage{pendingMessage(0)}
		api.fail = false
		sum, err := imp.Import(t.Context(), "app-example", "account-example")
		require.ErrorContains(t, err, "identity conflicts")
		assert.Equal(t, []ID{"media-000"}, api.attempts)
		assert.Equal(t, 1, sum.MediaStored)
		assert.Equal(t, 1, sum.MessagesProcessed)
	})
}
