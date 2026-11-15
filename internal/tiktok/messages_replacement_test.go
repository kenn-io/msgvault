package tiktok

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestStoredImageReplacementDownloadsVideo(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed snapshot", true: "unchanged historical snapshot"}[legacy], func(t *testing.T) {
			st, api, imp, source := seedPendingMedia(t, 1)
			api.fail = false
			_, err := imp.Import(t.Context(), "app-example", "account-example")
			require.NoError(t, err)
			var id, oldID int64
			var oldHash, oldPath string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id,message_id,content_hash,storage_path FROM attachments`).Scan(&oldID, &id, &oldHash, &oldPath))
			// Preserved and anonymous evidence must not count as the replacement or be rewritten.
			_, err = st.DB().ExecContext(t.Context(), `INSERT INTO source_merge_preserved_attachments(attachment_id) VALUES (?)`, oldID)
			require.NoError(t, err)
			_, err = st.DB().ExecContext(t.Context(), `INSERT INTO attachments(message_id,filename,mime_type,storage_path,content_hash,size,attachment_role) VALUES (?,'anonymous','image/png',?,'anonymous-example',1,'unknown')`, id, oldPath)
			require.NoError(t, err)
			m := pendingMessage(0)
			m.Type, m.Image, m.Video = "video", nil, &MessageMedia{ID: "video-example"}
			event := MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}
			if legacy {
				// This is the exact persisted state the old blanket completion gate left:
				// current video snapshot, previous image, no video occurrence.
				api.fail = true
				_, err = imp.Apply(t.Context(), "app-example", event, m)
				if err != nil {
					require.ErrorContains(t, err, "synthetic provider unavailable")
				}
				_, err = st.DB().ExecContext(t.Context(), `DELETE FROM attachments WHERE message_id=? AND source_part_key='tiktok:video'`, id)
				require.NoError(t, err)
			}
			api.attempts = nil
			api.fail = true
			api.open = func(_ context.Context, got BusinessMessage) error {
				assert.Equal(t, "video", got.Kind())
				var pending int
				require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM attachments WHERE message_id=? AND source_part_key='tiktok:video' AND attachment_state='pending'`, id).Scan(&pending))
				assert.Equal(t, 1, pending)
				return nil
			}
			var journal int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal`).Scan(&journal))
			_, err = imp.Apply(t.Context(), "app-example", event, m)
			require.ErrorContains(t, err, "synthetic provider unavailable")
			assert.Equal(t, []ID{m.ID}, api.attempts)
			if legacy {
				var writes int
				require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq>? AND dataset='message_facts'`, journal).Scan(&writes))
				assert.Zero(t, writes)
			}
			// No current provider window: recovery must use the committed video evidence.
			api.fail = false
			api.body = "different synthetic video bytes"
			api.attempts = nil
			sum, err := imp.Import(t.Context(), "app-example", "account-example")
			require.NoError(t, err)
			assert.Equal(t, 1, sum.MediaStored)
			assert.Equal(t, []ID{m.ID}, api.attempts)
			var hash, path, videoHash string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT content_hash,storage_path FROM attachments WHERE id=?`, oldID).Scan(&hash, &path))
			assert.Equal(t, oldHash, hash)
			assert.Equal(t, oldPath, path)
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT content_hash FROM attachments WHERE message_id=? AND source_part_key='tiktok:video'`, id).Scan(&videoHash))
			assert.NotEmpty(t, videoHash)
			assert.NotEqual(t, oldHash, videoHash)
			var anonymous int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM attachments WHERE message_id=? AND source_part_key IS NULL AND content_hash='anonymous-example'`, id).Scan(&anonymous))
			assert.Equal(t, 1, anonymous)
			pending, err := st.HasPendingTikTokMediaContext(t.Context(), source.ID)
			require.NoError(t, err)
			assert.False(t, pending)
			api.attempts = nil
			// Reject any write on the idempotent repeat, not only a second download.
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER no_repeat_attachment_write BEFORE UPDATE ON attachments BEGIN SELECT RAISE(ABORT,'unexpected repeat attachment write'); END`)
			require.NoError(t, err)
			_, err = imp.Apply(t.Context(), "app-example", event, m)
			require.NoError(t, err)
			assert.Empty(t, api.attempts)
		})
	}
}

func TestReplacementDoesNotDownloadIneligibleCurrentPart(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"preserved", `INSERT INTO source_merge_preserved_attachments(attachment_id) SELECT id FROM attachments`},
		{"foreign", `UPDATE attachments SET source_attachment_id='other:example'`},
		{"path", `UPDATE attachments SET storage_path='aa/example'`},
		{"skipped", `UPDATE attachments SET attachment_state='skipped',attachment_skip_reason='policy_scope'`},
		{"stored", `UPDATE attachments SET content_hash='stored-example',storage_path='aa/example',attachment_state='stored'`},
		{"archive only", `INSERT INTO source_merge_archive_only_messages(message_id) SELECT message_id FROM attachments`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, api, imp, _ := seedPendingMedia(t, 1)
			_, err := st.DB().ExecContext(t.Context(), `UPDATE attachments SET source_part_key='tiktok:video'`)
			require.NoError(t, err)
			_, err = st.DB().ExecContext(t.Context(), tc.sql)
			require.NoError(t, err)
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER preserve_current BEFORE UPDATE ON attachments BEGIN SELECT RAISE(ABORT,'current part changed'); END`)
			require.NoError(t, err)
			m := pendingMessage(0)
			m.Type, m.Image, m.Video = "video", nil, &MessageMedia{ID: "video-example"}
			_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, m)
			require.NoError(t, err)
			assert.Empty(t, api.attempts)
		})
	}
}

func TestReplacementCompletionPreservesChangedOccurrence(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"preserved", `INSERT INTO source_merge_preserved_attachments(attachment_id) SELECT id FROM attachments`},
		{"removed", `DELETE FROM attachments`},
		{"foreign", `UPDATE attachments SET source_attachment_id='other:example'`},
		{"stored", `UPDATE attachments SET content_hash='other-stored',storage_path='aa/example',attachment_state='stored'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, api, imp, _ := seedPendingMedia(t, 1)
			api.fail = false
			api.open = func(context.Context, BusinessMessage) error {
				_, err := st.DB().ExecContext(t.Context(), tc.sql)
				require.NoError(t, err)
				return nil
			}
			_, err := imp.Import(t.Context(), "app-example", "account-example")
			require.ErrorContains(t, err, "TikTok pending media changed before storage")
			var stored int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM attachments WHERE attachment_state='stored' AND COALESCE(content_hash,'')<>'other-stored'`).Scan(&stored))
			assert.Zero(t, stored)
		})
	}
}

func TestStoredMediaReplacementQueuesAtomically(t *testing.T) {
	st, api, imp, source := seedPendingMedia(t, 1)
	api.fail = false
	_, err := imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	var oldHash string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT content_hash FROM attachments`).Scan(&oldHash))
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER interrupt_replacement BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'synthetic replacement interruption'); END`)
	require.NoError(t, err)
	m := pendingMessage(0)
	m.Type, m.Image, m.Video = "video", nil, &MessageMedia{ID: "video-example"}
	api.attempts = nil
	_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, m)
	require.ErrorContains(t, err, "synthetic replacement interruption")
	assert.Empty(t, api.attempts)
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(m.ConversationID, m.ID)})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Contains(t, string(rows[messageKey(m.ConversationID, m.ID)].Raw), `"type":"image"`)
	var gotHash string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT content_hash FROM attachments`).Scan(&gotHash))
	assert.Equal(t, oldHash, gotHash)
}

func TestMediaRetrievalRetainsSourceExecutionLease(t *testing.T) {
	_, api, imp, _ := seedPendingMedia(t, 1)
	api.fail = false
	api.open = func(ctx context.Context, m BusinessMessage) error {
		m.Type, m.Image, m.Text = "text", nil, &MessageText{Body: "Concurrent replacement"}
		_, err := imp.Apply(ctx, "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, m)
		require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
		return nil
	}
	sum, err := imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.MediaStored)
}
