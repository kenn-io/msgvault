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

func TestImportGoogleChatSenderSearchSurvivesRebuild(t *testing.T) {
	st := testutil.NewTestStore(t)
	require.True(t, st.FTS5Available(), "probe requires real FTS")
	root := t.TempDir()
	testutil.WriteFile(t, root, "Groups/DM synthetic/messages.json", []byte(`{"messages":[{"message_id":"synthetic/topic/message","creator":{"email":"senderneedle@example.com","name":"Synthetic Sender"},"text":"ordinary body","created_date":"Monday, January 18, 2021 at 1:27:33 PM UTC"}]}`))
	write(t, root, "Groups/DM synthetic/group_info.json", `{"members":[{"email":"senderneedle@example.com"},{"email":"recipientneedle@example.com"}]}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.MessagesAdded)
	_, before, err := st.SearchMessagesContext(t.Context(), "senderneedle", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, before, "fresh imported messages must be discoverable by sender")
	_, recipientBefore, err := st.SearchMessagesContext(t.Context(), "recipientneedle", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, recipientBefore)
	n, err := st.RebuildFTSContext(t.Context(), nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, after, err := st.SearchMessagesContext(t.Context(), "senderneedle", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, before, after, "rebuild must preserve search results")
	_, recipientAfter, err := st.SearchMessagesContext(t.Context(), "recipientneedle", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, recipientBefore, recipientAfter)
	t.Logf("sender matches before=%d after=%d", before, after)
}

func TestImportGoogleChatSparseReplayKeepsKnownConversationKind(t *testing.T) {
	st := testutil.NewTestStore(t)
	full, sparse := t.TempDir(), t.TempDir()
	record := []byte(`{"messages":[{"message_id":"synthetic/topic/message","creator":{"email":"sender@example.com"},"text":"ordinary body"}]}`)
	testutil.WriteFile(t, full, "Groups/DM synthetic/messages.json", record)
	testutil.WriteFile(t, full, "Groups/DM synthetic/group_info.json", []byte(`{"members":[{"email":"owner@example.com"},{"email":"sender@example.com"}]}`))
	testutil.WriteFile(t, sparse, "Groups/DM synthetic/messages.json", record)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{full}}
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	var before string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&before))
	require.Equal(t, "direct_chat", before)
	opts.Roots = []string{sparse}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	require.Equal(t, 1, summary.Duplicates)
	var after string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&after))
	assert.Equal(t, before, after, "missing roster is no evidence that a known direct chat changed kind")
	t.Logf("conversation kind before=%s after=%s", before, after)
}

const fixtureDate = "Monday, January 18, 2021 at 1:27:33 PM UTC"

// Container rosters belong to the conversation, independently of message count.
func TestImportGoogleChatMetadataDoesNotEmbedWholeRoster(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	members := []string{`{"name":"Owner","email":"owner@example.com","user_type":"Human"}`}
	for i := 0; i < 300; i++ {
		members = append(members, fmt.Sprintf(`{"name":"Member %d","email":"member%d@example.com","user_type":"Human"}`, i, i))
	}
	testutil.WriteFile(t, root, "Groups/Space s/group_info.json", []byte(`{"name":"Big","members":[`+strings.Join(members, ",")+`]}`))
	var msgs []string
	for i := 0; i < 50; i++ {
		msgs = append(msgs, fmt.Sprintf(`{"creator":{"name":"Owner","email":"owner@example.com","user_type":"Human"},"created_date":%q,"text":"t%d","topic_id":"t","message_id":"s/t/m%d"}`, fixtureDate, i, i))
	}
	testutil.WriteFile(t, root, "Groups/Space s/messages.json", []byte(`{"messages":[`+strings.Join(msgs, ",")+`]}`))
	_, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(t, err)
	var total, rec int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT sum(length(metadata)), sum(size_estimate) FROM messages`).Scan(&total, &rec))
	t.Logf("metadata bytes=%d, record bytes=%d", total, rec)
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages WHERE source_message_id='s/t/m7'`).Scan(&metadata))
	assert.NotContains(t, metadata, "member250@example.com", "per-message metadata must not copy the full roster")
}

// Every supplied part must have a recognized layout.
func TestImportGoogleChatUnrecognisedPartIsReported(t *testing.T) {
	st := testutil.NewTestStore(t)
	good, typo := t.TempDir(), t.TempDir()
	testutil.WriteFile(t, good, "Groups/DM d/messages.json", []byte(`{"messages":[{"creator":{"email":"owner@example.com"},"created_date":"`+fixtureDate+`","text":"x","message_id":"d/t/1"}]}`))
	testutil.WriteFile(t, typo, "unrelated.txt", []byte("x"))
	_, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{good, typo}})
	assert.Error(t, err, "a supplied part with no Groups/Users layout contributes nothing and should be reported")
}

func TestImportGoogleChatRunCountsMatchCommittedMessages(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	testutil.WriteFile(t, root, "Groups/DM synthetic/messages.json", []byte(`{"messages":[{"message_id":"synthetic/topic/message","creator":{"email":"sender@example.com"},"text":"ordinary body"}]}`))
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.MessagesAdded)
	var status string
	var processed, added int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT status,messages_processed,messages_added FROM sync_runs`).Scan(&status, &processed, &added))
	require.Equal(t, "completed", status)
	assert.EqualValues(t, 1, processed, "source run must report imported coverage")
	assert.EqualValues(t, summary.MessagesAdded, added, "source run must report committed additions")
	_, err = Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(t, err)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&processed, &added))
	assert.EqualValues(t, 1, processed)
	assert.Zero(t, added)
	t.Logf("run status=%s processed=%d added=%d; CLI summary added=%d", status, processed, added, summary.MessagesAdded)
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
	var kind string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
	assert.Equal(t, "direct_chat", kind, "new messages in a sparse replay preserve known kind")
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

func TestImportGoogleChatFailedRunCountsOnlyCommittedMessages(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", one)
	write(t, root, "Groups/Space b/messages.json", `{"messages":[`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.Error(t, err)
	require.EqualValues(t, 1, summary.MessagesAdded)
	var status string
	var processed, added int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT status,messages_processed,messages_added FROM sync_runs`).Scan(&status, &processed, &added))
	assert.Equal(t, "failed", status)
	assert.EqualValues(t, 1, processed)
	assert.EqualValues(t, 1, added)
}
