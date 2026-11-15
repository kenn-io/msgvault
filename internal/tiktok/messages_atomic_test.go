package tiktok

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMessageSnapshotRollsBackWithoutPendingMedia(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "changed"}[existing], func(t *testing.T) {
			st := testutil.NewSQLiteTestStore(t)
			api := &pendingMediaAPI{fail: true}
			imp := NewMessageImporter(st, api, MessageImportOptions{AttachmentsDir: t.TempDir()})
			source, err := imp.Register(t.Context(), "app-example", "account-example")
			require.NoError(t, err)
			event := MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}
			m := pendingMessage(0)
			var oldParent int64
			if existing {
				parent := BusinessMessage{ID: "parent-a", ConversationID: m.ConversationID, Timestamp: 1700000000000, Type: "text"}
				_, err = imp.Apply(t.Context(), "app-example", event, parent)
				require.NoError(t, err)
				parents, err := st.MessageExistsBatchContext(t.Context(), source.ID, []string{messageKey(m.ConversationID, parent.ID)})
				require.NoError(t, err)
				oldParent = parents[messageKey(m.ConversationID, parent.ID)]
				old := m
				old.Reference = &MessageReference{ID: parent.ID}
				old.Type, old.Image, old.Text = "text", nil, &MessageText{Body: "Original"}
				_, err = imp.Apply(t.Context(), "app-example", event, old)
				require.NoError(t, err)
			}
			m.Reference = &MessageReference{ID: "parent-b"}
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER interrupt_pending BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'synthetic pending failure'); END`)
			require.NoError(t, err)
			var journal int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal`).Scan(&journal))
			_, err = imp.Apply(t.Context(), "app-example", event, m)
			require.ErrorContains(t, err, "synthetic pending failure")
			rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(m.ConversationID, m.ID)})
			require.NoError(t, err)
			if existing {
				require.Len(t, rows, 1)
				row := rows[messageKey(m.ConversationID, m.ID)]
				assert.Contains(t, string(row.Raw), "Original")
				metadata, err := st.GetMessageMetadataContext(t.Context(), row.ID)
				require.NoError(t, err)
				assert.Contains(t, metadata.String, `"type":"text"`)
				stored, err := st.GetMessageContext(t.Context(), row.ID)
				require.NoError(t, err)
				assert.Equal(t, "Original", stored.Body)
				current, err := st.GetMessageReplyToMessageIDContext(t.Context(), row.ID)
				require.NoError(t, err)
				assert.Equal(t, sql.NullInt64{Int64: oldParent, Valid: true}, current)
				assert.Contains(t, metadata.String, `"referenced_message_id":"parent-a"`)
			} else {
				assert.Empty(t, rows)
			}
			var writes int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, journal).Scan(&writes))
			assert.Zero(t, writes)
			assert.Empty(t, api.attempts)
			_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER interrupt_pending`)
			require.NoError(t, err)
			_, err = imp.Apply(t.Context(), "app-example", event, m)
			require.ErrorContains(t, err, "synthetic provider unavailable")
			pending, err := st.HasPendingTikTokMediaContext(t.Context(), source.ID)
			require.NoError(t, err)
			assert.True(t, pending)
			api.fail = false
			_, err = imp.Import(t.Context(), "app-example", "account-example")
			require.NoError(t, err)
		})
	}
}

func TestMessageReplacementRetiresOnlyObsoletePendingMedia(t *testing.T) {
	for _, replacement := range []string{"video", "text"} {
		t.Run(replacement, func(t *testing.T) {
			st, api, imp, source := seedPendingMedia(t, 1)
			var id int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_id=?`, source.ID).Scan(&id))
			m := pendingMessage(0)
			m.Type, m.Image = replacement, nil
			if replacement == "video" {
				m.Video = &MessageMedia{ID: "video-example"}
			} else {
				m.Text = &MessageText{Body: "Replacement"}
			}
			_, err := imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, m)
			if replacement == "video" {
				require.ErrorContains(t, err, "synthetic provider unavailable")
			} else {
				require.NoError(t, err)
			}
			var obsolete int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM attachments WHERE message_id=? AND source_part_key='tiktok:image'`, id).Scan(&obsolete))
			assert.Zero(t, obsolete)
			api.fail = false
			api.attempts = nil
			_, err = imp.Import(t.Context(), "app-example", "account-example")
			require.NoError(t, err)
			if replacement == "video" {
				assert.Equal(t, []ID{m.ID}, api.attempts)
			} else {
				assert.Empty(t, api.attempts)
			}
			pending, err := st.HasPendingTikTokMediaContext(t.Context(), source.ID)
			require.NoError(t, err)
			assert.False(t, pending)
		})
	}
}

func TestMessageReplacementPreservesStoredMedia(t *testing.T) {
	st, api, imp, source := seedPendingMedia(t, 1)
	api.fail = false
	_, err := imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	var id int64
	var hash, path string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT message_id,content_hash,storage_path FROM attachments`).Scan(&id, &hash, &path))
	require.NotEmpty(t, hash)
	m := pendingMessage(0)
	m.Type, m.Image, m.Text = "text", nil, &MessageText{Body: "Replacement"}
	_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, m)
	require.NoError(t, err)
	var gotHash, gotPath string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT content_hash,storage_path FROM attachments WHERE message_id=?`, id).Scan(&gotHash, &gotPath))
	assert.Equal(t, hash, gotHash)
	assert.Equal(t, path, gotPath)
	stored, err := st.GetMessageContext(t.Context(), id)
	require.NoError(t, err)
	assert.True(t, stored.HasAttachments)
	pending, err := st.HasPendingTikTokMediaContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestMessageReplyReplacementSurvivesMissingParentAndShallowWindow(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	api := &pendingMediaAPI{}
	imp := NewMessageImporter(st, api, MessageImportOptions{})
	source, err := imp.Register(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	event := MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}
	m := BusinessMessage{ID: "parent-a", ConversationID: "conversation-example", Timestamp: 1700000000001, Type: "text"}
	apply := func() { _, err := imp.Apply(t.Context(), "app-example", event, m); require.NoError(t, err) }
	apply()
	m.ID, m.Reference = "child-example", &MessageReference{ID: "parent-a"}
	apply()
	m.Reference = &MessageReference{ID: "parent-b"}
	apply()
	var child int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_id=? AND source_message_id=?`, source.ID, messageKey(m.ConversationID, m.ID)).Scan(&child))
	current, err := st.GetMessageReplyToMessageIDContext(t.Context(), child)
	require.NoError(t, err)
	assert.Equal(t, sql.NullInt64{}, current)
	// Only the late parent appears in the next shallow window.
	m.ID, m.Reference = "parent-b", nil
	api.messages = []BusinessMessage{m}
	_, err = imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	current, err = st.GetMessageReplyToMessageIDContext(t.Context(), child)
	require.NoError(t, err)
	assert.True(t, current.Valid)
	parents, err := st.MessageExistsBatchContext(t.Context(), source.ID, []string{messageKey(m.ConversationID, m.ID)})
	require.NoError(t, err)
	assert.Equal(t, parents[messageKey(m.ConversationID, m.ID)], current.Int64)
	m.ID, m.Reference = "child-example", &MessageReference{}
	apply()
	m.Reference = nil
	apply()
	current, err = st.GetMessageReplyToMessageIDContext(t.Context(), child)
	require.NoError(t, err)
	assert.Equal(t, sql.NullInt64{}, current)
}

func TestMessageReplacementKeepsNonOwnedMedia(t *testing.T) {
	for _, setup := range []struct{ name, sql string }{
		{"preserved", `INSERT INTO source_merge_preserved_attachments(attachment_id) SELECT id FROM attachments`},
		{"stored path", `UPDATE attachments SET storage_path='aa/example-media'`},
		{"provider provenance", `UPDATE attachments SET source_attachment_id='other:example'`},
		{"other part", `UPDATE attachments SET source_part_key='other:image'`},
		{"skipped", `UPDATE attachments SET attachment_state='skipped', attachment_skip_reason='policy_scope'`},
	} {
		t.Run(setup.name, func(t *testing.T) {
			st, _, imp, _ := seedPendingMedia(t, 1)
			_, err := st.DB().ExecContext(t.Context(), setup.sql)
			require.NoError(t, err)
			var id int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM attachments`).Scan(&id))
			m := pendingMessage(0)
			m.Type, m.Image, m.Text = "text", nil, &MessageText{Body: "Replacement"}
			_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, m)
			require.NoError(t, err)
			var count int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM attachments WHERE id=?`, id).Scan(&count))
			assert.Equal(t, 1, count)
		})
	}
}

func TestAtomicTikTokSnapshotUsesSyncFence(t *testing.T) {
	st, _, _, source := seedPendingMedia(t, 1)
	oldID, err := st.StartSyncContext(t.Context(), source.ID, "fixture")
	require.NoError(t, err)
	stale := st.ScopedToSync(source.ID, oldID)
	require.NoError(t, st.FailSyncContext(t.Context(), oldID, "synthetic interruption"))
	currentID, err := st.StartSyncContext(t.Context(), source.ID, "fixture")
	require.NoError(t, err)
	defer func() { _ = st.FailSyncContext(context.WithoutCancel(t.Context()), currentID, "fixture complete") }()
	_, err = stale.PersistTikTokMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "new-example", MessageType: MessagesSourceType}}, store.TikTokMessageState{})
	require.ErrorIs(t, err, store.ErrSyncRunSuperseded)
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE source_id=?`, source.ID).Scan(&count))
	assert.Equal(t, 1, count)
}
