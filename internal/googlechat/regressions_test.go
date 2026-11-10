package googlechat

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

const fixtureDate = "Monday, January 18, 2021 at 1:27:33 PM UTC"

func TestImportGoogleChatSenderSearchSurvivesRebuild(t *testing.T) {
	st := testutil.NewTestStore(t)
	require.True(t, st.FTS5Available(), "probe requires real FTS")
	root := t.TempDir()
	testutil.WriteFile(t, root, "Groups/DM synthetic/messages.json", []byte(`{"messages":[{"message_id":"synthetic/topic/message","creator":{"email":"senderneedle@example.com","name":"Synthetic Sender"},"text":"bodyneedle ordinary body","created_date":"Monday, January 18, 2021 at 1:27:33 PM UTC"}]}`))
	write(t, root, "Groups/DM synthetic/group_info.json", `{"members":[{"email":"senderneedle@example.com"},{"email":"recipientneedle@example.com"}]}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.MessagesAdded)
	_, bodyBefore, err := st.SearchMessagesContext(t.Context(), "bodyneedle", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, bodyBefore)
	_, before, err := st.SearchMessagesContext(t.Context(), "senderneedle", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, before, "fresh imported messages must be discoverable by sender")
	_, recipientBefore, err := st.SearchMessagesContext(t.Context(), "recipientneedle", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, recipientBefore)
	n, err := st.RebuildFTSContext(t.Context(), nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, bodyAfter, err := st.SearchMessagesContext(t.Context(), "bodyneedle", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, bodyBefore, bodyAfter)
	_, after, err := st.SearchMessagesContext(t.Context(), "senderneedle", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, before, after, "rebuild must preserve search results")
	_, recipientAfter, err := st.SearchMessagesContext(t.Context(), "recipientneedle", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, recipientBefore, recipientAfter)
}

func TestImportGoogleChatMetadataDoesNotEmbedWholeRoster(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	members := []string{`{"name":"Owner","email":"owner@example.com","user_type":"Human"}`}
	for i := range 300 {
		members = append(members, fmt.Sprintf(`{"name":"Member %d","email":"member%d@example.com","user_type":"Human"}`, i, i))
	}
	testutil.WriteFile(t, root, "Groups/Space s/group_info.json", []byte(`{"name":"Big","members":[`+strings.Join(members, ",")+`]}`))
	var msgs []string
	for i := range 50 {
		msgs = append(msgs, fmt.Sprintf(`{"creator":{"name":"Owner","email":"owner@example.com","user_type":"Human"},"created_date":%q,"text":"t%d","topic_id":"t","message_id":"s/t/m%d"}`, fixtureDate, i, i))
	}
	testutil.WriteFile(t, root, "Groups/Space s/messages.json", []byte(`{"messages":[`+strings.Join(msgs, ",")+`]}`))
	_, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(t, err)
	var recipients, membersCount int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='to'`).Scan(&recipients))
	assert.Zero(t, recipients)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_participants`).Scan(&membersCount))
	assert.Equal(t, 301, membersCount)
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages WHERE source_message_id='s/t/m7'`).Scan(&metadata))
	assert.NotContains(t, metadata, "member250@example.com", "per-message metadata must not copy the full roster")
}

func TestImportGoogleChatContainerEvidenceSurvivesSparseReplay(t *testing.T) {
	st := testutil.NewTestStore(t)
	full, partial := t.TempDir(), t.TempDir()
	write(t, full, "Groups/DM a/messages.json", one)
	groupRaw := `{ "members":[{"email":"owner@example.com"},{"email":"peer@example.com"}], "unknown_group":{"retained":true}, "number":1e3 }`
	write(t, full, "Groups/DM a/group_info.json", groupRaw)
	profileRaw := `{ "user":{"email":"owner@example.com"}, "unknown_profile":{"retained":true} }`
	write(t, full, "Users/User synthetic/user_info.json", profileRaw)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{full}}
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	_, err = Import(t.Context(), st, opts)
	require.NoError(t, err, "repeated full export retains one copy of each container snapshot")
	write(t, partial, "Groups/DM a/messages.json", strings.Replace(one, "group/topic/message", "group/topic/new", 1))
	opts.Roots = []string{partial}
	_, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	var group, profile sql.NullString
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM conversations`).Scan(&group))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT sync_config FROM sources`).Scan(&profile))
	for _, tc := range []struct{ metadata, container, unknown, raw string }{
		{group.String, "group_files", "unknown_group", groupRaw},
		{profile.String, "owner_profiles", "unknown_profile", profileRaw},
	} {
		var container map[string][]provenance
		require.NoError(t, json.Unmarshal([]byte(tc.metadata), &container))
		require.Len(t, container[tc.container], 1)
		assert.Equal(t, tc.raw, container[tc.container][0].Data, "container source bytes survive database normalization")
		var data map[string]any
		require.NoError(t, json.Unmarshal([]byte(container[tc.container][0].Data), &data))
		assert.Equal(t, map[string]any{"retained": true}, data[tc.unknown])
	}
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages ORDER BY id LIMIT 1`).Scan(&metadata))
	assert.NotContains(t, metadata, "unknown_group")
	assert.NotContains(t, metadata, "unknown_profile")
	assert.NotContains(t, metadata, "provider_record")
	assert.Contains(t, metadata, "group_info.json")
	assert.Contains(t, metadata, "user_info.json")
}

func TestImportGoogleChatRunAccounting(t *testing.T) {
	for _, mode := range []string{"completed", "failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			write(t, root, "Groups/DM a/messages.json", one)
			if mode == "failed" {
				write(t, root, "Groups/Space b/messages.json", `{"messages":[`)
			}
			if mode == "cancelled" {
				write(t, root, "Groups/Space b/messages.json", strings.ReplaceAll(one, "group/topic/message", "group/topic/second"))
			}
			opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}}
			var summary *ImportSummary
			var err error
			if mode == "cancelled" {
				summary, err = cancelAfterCommittedMessage(t, st, opts)
			} else {
				summary, err = Import(t.Context(), st, opts)
			}
			statusWant := "failed"
			if mode == "completed" {
				require.NoError(t, err)
				statusWant = "completed"
			} else {
				require.Error(t, err)
			}
			require.EqualValues(t, 1, summary.MessagesAdded)
			var status string
			var processed, added int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT status,messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &processed, &added))
			assert.Equal(t, statusWant, status)
			assert.EqualValues(t, 1, processed)
			assert.Equal(t, summary.MessagesAdded, added)
			if mode == "completed" {
				_, err = Import(t.Context(), st, opts)
				require.NoError(t, err)
				require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&processed, &added))
				assert.EqualValues(t, 1, processed)
				assert.Zero(t, added)
			}
		})
	}
}
