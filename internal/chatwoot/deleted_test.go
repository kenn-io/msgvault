package chatwoot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/chatwootapi"
)

func deletedFromSource(t *testing.T, st *store.Store, messageID int64) bool {
	t.Helper()
	var deleted bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageID).Scan(&deleted))
	return deleted
}

// Chatwoot keeps a deleted message's row but replaces its content and removes
// its files. Neither the artifact refresh nor --full may overwrite the archive.
func TestDeletedMessageKeepsArchivedContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		full bool
	}{{"artifact_refresh", false}, {"full", true}} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			message := chatwootapi.Message(901, now().Unix(), nil)
			message["content"] = "deletedquartz original words"
			message["content_type"] = "voice_call"
			message["call"] = map[string]any{"status": "completed", "transcript": "deletedquartz call transcript"}
			api := newContractAPI(t, 20, []map[string]any{message})
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
			require.NoError(err)
			require.Contains(savedState(t, st, source).Conversations["42"].Artifacts, "901")

			api.Mu.Lock()
			message["content"] = "This message was deleted"
			message["content_type"] = "text"
			message["content_attributes"] = map[string]any{"deleted": true}
			delete(message, "call")
			api.Mu.Unlock()
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, Full: tc.full})
			require.NoError(err)

			for providerID, want := range map[string]string{
				"901":      "deletedquartz original words",
				"call:901": "deletedquartz call transcript",
			} {
				id := contractArchivedMessageID(t, st, providerID)
				body, err := st.GetMessageBodyText(id)
				require.NoError(err)
				assert.Contains(body, want, providerID)
				assert.NotContains(body, "This message was deleted", providerID)
				assert.True(deletedFromSource(t, st, id), providerID)
			}
			assert.NotContains(savedState(t, st, source).Conversations, "42", "a deleted message leaves the refresh list")
		})
	}
}

func TestMessageDeletedBeforeFirstSyncIsNotArchived(t *testing.T) {
	deleted := chatwootapi.Message(901, 1767225600, nil)
	deleted["content"] = "This message was deleted"
	deleted["content_attributes"] = map[string]any{"deleted": true}
	api := newContractAPI(t, 20, []map[string]any{deleted, chatwootapi.Message(902, 1767225601, nil)})
	st := testutil.NewTestStore(t)
	imp, _ := contractRegister(t, st, api)
	_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(t, err)
	assert.Equal(t, []int64{902}, contractMessageIDs(t, st))
}
