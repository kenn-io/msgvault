package tiktok

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMessageReplayKeepsAnalyticsValid(t *testing.T) {
	for _, mode := range []string{"pull", "webhook"} {
		t.Run(mode, func(t *testing.T) {
			st := testutil.NewSQLiteTestStore(t)
			api := &pendingMediaAPI{}
			imp := NewMessageImporter(st, api, MessageImportOptions{AttachmentsDir: t.TempDir()})
			_, err := imp.Register(t.Context(), "app-example", "account-example")
			require.NoError(t, err)
			parent := BusinessMessage{ID: "parent-example", ConversationID: "conversation-example", Timestamp: 1700000000001, Type: "text", Text: &MessageText{Body: "Parent"}}
			child := parent
			child.ID, child.Reference = "child-example", &MessageReference{ID: parent.ID}
			child.FromUser = &MessageActor{ID: "actor-example", DisplayName: "Example sender"}
			child.ToUser = &MessageActor{ID: "account-example", DisplayName: "Example recipient"}
			child.Text = &MessageText{Body: "Reply"}
			run := func() {
				if mode == "pull" {
					api.messages = []BusinessMessage{parent, child}
					_, err = imp.Import(t.Context(), "app-example", "account-example")
					require.NoError(t, err)
				} else {
					for _, message := range []BusinessMessage{parent, child} {
						_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, message)
						require.NoError(t, err)
					}
				}
			}
			run()
			var baseline int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal`).Scan(&baseline))
			run()
			var invalidations int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, baseline).Scan(&invalidations))
			assert.Zero(t, invalidations, "unchanged message snapshots must not invalidate analytics")
		})
	}
}

func TestMessageReplayPreservesUpdates(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	api := &pendingMediaAPI{}
	imp := NewMessageImporter(st, api, MessageImportOptions{AttachmentsDir: t.TempDir()})
	source, err := imp.Register(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	event := MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}
	message := BusinessMessage{ID: "message-example", ConversationID: "conversation-example", Timestamp: 1700000000001, Type: "text", Text: &MessageText{Body: "Original"}, FromUser: &MessageActor{ID: "actor-example", DisplayName: "Example sender"}}
	apply := func(m BusinessMessage) {
		_, err := imp.Apply(t.Context(), "app-example", event, m)
		require.NoError(t, err)
	}
	apply(message)
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(message.ConversationID, message.ID)})
	require.NoError(t, err)
	id := rows[messageKey(message.ConversationID, message.ID)].ID
	require.NotZero(t, id)
	// A repaired body/recipient projection must not be mistaken for a no-op
	// just because the archived provider evidence already matches.
	_, err = st.DB().ExecContext(t.Context(), `UPDATE message_bodies SET body_text = 'stale projection' WHERE message_id = ?`, id)
	require.NoError(t, err)
	apply(message)
	stored, err := st.GetMessageContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "Original", stored.Body)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE message_recipients SET display_name = 'stale name' WHERE message_id = ?`, id)
	require.NoError(t, err)
	apply(message)
	var name string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT display_name FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, id).Scan(&name))
	assert.Equal(t, "Example sender", name)
	message.Text.Body = "Updated"
	message.FromUser.DisplayName = "Updated sender"
	apply(message)
	stored, err = st.GetMessageContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "Updated", stored.Body)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT display_name FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, id).Scan(&name))
	assert.Equal(t, "Updated sender", name)
	// The event is projected metadata, independent of native payload bytes.
	api.messages = []BusinessMessage{message}
	_, err = imp.Import(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	metadata, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(t, err)
	var meta messageMetadata
	require.NoError(t, json.Unmarshal([]byte(metadata.String), &meta))
	assert.Empty(t, meta.Event)
	// Explicit replacement and later sparse replay retain the new parent.
	for _, parentID := range []ID{"parent-a", "parent-b"} {
		parent := message
		parent.ID = parentID
		apply(parent)
		message.Reference = &MessageReference{ID: parentID}
		apply(message)
		parents, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(message.ConversationID, parentID)})
		require.NoError(t, err)
		var reply sql.NullInt64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT reply_to_message_id FROM messages WHERE id = ?`, id).Scan(&reply))
		assert.Equal(t, sql.NullInt64{Int64: parents[messageKey(message.ConversationID, parentID)].ID, Valid: true}, reply)
	}
	message.Reference = nil
	apply(message)
	apply(message)
	metadata, err = st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(metadata.String), &meta))
	assert.Equal(t, ID("parent-b"), meta.ReplyTo)
}

func TestMessageReplayPreservesRawNumericLexemes(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	imp := NewMessageImporter(st, &pendingMediaAPI{}, MessageImportOptions{})
	source, err := imp.Register(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	for _, number := range []string{"9007199254740992", "9007199254740993", "1.0000000000000000001", "1.0000000000000000002", "1e0", "1.0"} {
		raw := []byte(`{"message_id":"message-example","conversation_id":"conversation-example","timestamp":1700000000001,"type":"text","from_user":{"id":"actor-example","display_name":"Example sender"},"text":{"body":"Same body","provider_number":` + number + `}}`)
		var message BusinessMessage
		require.NoError(t, json.Unmarshal(raw, &message))
		_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, message)
		require.NoError(t, err)
		rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(message.ConversationID, message.ID)})
		require.NoError(t, err)
		assert.Equal(t, raw, rows[messageKey(message.ConversationID, message.ID)].Raw)
		// Layout-only changes in real provider JSON must not rewrite the snapshot,
		// while the exact numeric lexeme and original archived bytes remain intact.
		reordered := jsontext.Value(raw).Clone()
		require.NoError(t, reordered.Canonicalize(jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false)))
		require.NoError(t, reordered.Indent())
		require.NotEqual(t, raw, []byte(reordered))
		id := rows[messageKey(message.ConversationID, message.ID)].ID
		metadata, err := st.GetMessageMetadataContext(t.Context(), id)
		require.NoError(t, err)
		formattedMetadata := jsontext.Value(metadata.String).Clone()
		require.NoError(t, formattedMetadata.Indent())
		_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET metadata = ? WHERE id = ?`, string(formattedMetadata), id)
		require.NoError(t, err)
		var baseline int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal`).Scan(&baseline))
		require.NoError(t, json.Unmarshal(reordered, &message))
		_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, message)
		require.NoError(t, err)
		storedRaw, err := st.GetMessageRawContext(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, raw, storedRaw)
		var invalidations int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, baseline).Scan(&invalidations))
		assert.Zero(t, invalidations)
	}
}

func TestUnchangedMediaReplayStillRetries(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	api := &pendingMediaAPI{fail: true}
	imp := NewMessageImporter(st, api, MessageImportOptions{AttachmentsDir: t.TempDir()})
	_, err := imp.Register(t.Context(), "app-example", "account-example")
	require.NoError(t, err)
	event := MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}
	message := pendingMessage(0)
	message.FromUser = &MessageActor{ID: "actor-example", DisplayName: "Example sender"}
	for i := 1; i <= 2; i++ {
		sum, err := imp.Apply(t.Context(), "app-example", event, message)
		require.Error(t, err)
		assert.True(t, sum.Partial)
		assert.Len(t, api.attempts, i)
	}
	api.fail = false
	sum, err := imp.Apply(t.Context(), "app-example", event, message)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.MediaStored)
	var baseline int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal`).Scan(&baseline))
	sum, err = imp.Apply(t.Context(), "app-example", event, message)
	require.NoError(t, err)
	assert.Zero(t, sum.MediaStored)
	assert.Len(t, api.attempts, 3)
	var invalidations int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, baseline).Scan(&invalidations))
	assert.Zero(t, invalidations)
}

// A failed reply update rolls back its metadata too. A successful retry and
// later sparse replays keep the new link without redundant reply writes.
func TestMessageReplayRollsBackInterruptedReplyReplacement(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_parent_%t", missing), func(t *testing.T) {
			st := testutil.NewSQLiteTestStore(t)
			imp := NewMessageImporter(st, &pendingMediaAPI{}, MessageImportOptions{})
			source, err := imp.Register(t.Context(), "app-example", "account-example")
			require.NoError(t, err)
			event := MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}
			message := BusinessMessage{ID: "parent-a", ConversationID: "conversation-example", Timestamp: 1700000000001, Type: "text", FromUser: &MessageActor{ID: "actor-example"}}
			apply := func() { _, err := imp.Apply(t.Context(), "app-example", event, message); require.NoError(t, err) }
			apply()
			if !missing {
				message.ID = "parent-b"
				apply()
			}
			message.ID, message.Reference = "child-example", &MessageReference{ID: "parent-a"}
			apply()
			rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(message.ConversationID, message.ID), messageKey(message.ConversationID, "parent-a"), messageKey(message.ConversationID, "parent-b")})
			require.NoError(t, err)
			childID := rows[messageKey(message.ConversationID, message.ID)].ID
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER interrupt_reply BEFORE UPDATE OF reply_to_message_id ON messages BEGIN SELECT RAISE(ABORT, 'synthetic interrupted reply write'); END`)
			require.NoError(t, err)
			message.Reference = &MessageReference{ID: "parent-b"}
			sum, err := imp.Apply(t.Context(), "app-example", event, message)
			require.ErrorContains(t, err, "synthetic interrupted reply write")
			assert.True(t, sum.Partial)
			metadata, err := st.GetMessageMetadataContext(t.Context(), childID)
			require.NoError(t, err)
			assert.Contains(t, metadata.String, `"referenced_message_id":"parent-a"`)
			current, err := st.GetMessageReplyToMessageIDContext(t.Context(), childID)
			require.NoError(t, err)
			require.Equal(t, sql.NullInt64{Int64: rows[messageKey(message.ConversationID, "parent-a")].ID, Valid: true}, current)
			_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER interrupt_reply; CREATE TABLE replay_reply_writes (id INTEGER); CREATE TRIGGER record_replay_reply AFTER UPDATE OF reply_to_message_id ON messages BEGIN INSERT INTO replay_reply_writes VALUES (NEW.id); END`)
			require.NoError(t, err)
			apply()
			current, err = st.GetMessageReplyToMessageIDContext(t.Context(), childID)
			require.NoError(t, err)
			assert.Equal(t, sql.NullInt64{Int64: rows[messageKey(message.ConversationID, "parent-b")].ID, Valid: !missing}, current)
			apply()
			// Sparse evidence must retain the new reference, not infer no parent.
			message.Reference = nil
			apply()
			apply()
			var writes int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM replay_reply_writes`).Scan(&writes))
			assert.Equal(t, 1, writes, "only the changed link needs a write")
		})
	}
}
