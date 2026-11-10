package googlechat

import (
	"context"
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

func TestImportGoogleChatMetadataDoesNotEmbedWholeRoster(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)
	var recipients, membersCount int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='to'`).Scan(&recipients))
	assert.Zero(recipients)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_participants`).Scan(&membersCount))
	assert.Equal(301, membersCount)
	var metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages WHERE source_message_id='s/t/m7'`).Scan(&metadata))
	assert.NotContains(metadata, "member250@example.com", "per-message metadata must not copy the full roster")
}

func TestImportGoogleChatContainerEvidenceSurvivesSparseReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	full, partial := t.TempDir(), t.TempDir()
	write(t, full, "Groups/DM a/messages.json", fixtureMessages(t))
	groupRaw := `{ "members":[{"email":"owner@example.com"},{"email":"peer@example.com"}], "unknown_group":{"retained":true}, "number":1e3 }`
	write(t, full, "Groups/DM a/group_info.json", groupRaw)
	profileRaw := `{ "user":{"email":"owner@example.com"}, "unknown_profile":{"retained":true} }`
	write(t, full, "Users/User synthetic/user_info.json", profileRaw)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{full}}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	_, err = Import(t.Context(), st, opts)
	require.NoError(err, "repeated full export retains one copy of each container snapshot")
	write(t, partial, "Groups/DM a/messages.json", strings.Replace(fixtureMessages(t), "group/topic/message", "group/topic/new", 1))
	opts.Roots = []string{partial}
	_, err = Import(t.Context(), st, opts)
	require.NoError(err)
	var group, profile sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM conversations`).Scan(&group))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sync_config FROM sources`).Scan(&profile))
	var evidence struct {
		Files []provenance `json:"group_files"`
	}
	require.NoError(json.Unmarshal([]byte(group.String), &evidence))
	assert.ElementsMatch([]provenance{
		{Path: "Groups/DM a/group_info.json", Data: groupRaw},
		{Path: "Groups/DM a/messages.json"},
	}, evidence.Files, "relative container provenance is retained once")
	assert.JSONEq(`{"owner_profile_path":"Users/User synthetic/user_info.json"}`, profile.String)
	assert.NotContains(profile.String, "unknown_profile", "owner profile JSON is validated without being retained")
	assert.NotContains(group.String, full)
	assert.NotContains(profile.String, full)
	var metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages ORDER BY id LIMIT 1`).Scan(&metadata))
	for _, key := range []string{"unknown_group", "unknown_profile", "provider_record", "group_info.json", "user_info.json", "messages.json"} {
		assert.NotContains(metadata, key)
	}
}

func TestImportGoogleChatRunAccounting(t *testing.T) {
	for _, mode := range []string{"completed", "failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
			if mode == "failed" {
				write(t, root, "Groups/Space b/messages.json", `{"messages":[`)
			}
			if mode == "cancelled" {
				write(t, root, "Groups/Space b/messages.json", strings.ReplaceAll(fixtureMessages(t), "group/topic/message", "group/topic/second"))
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
				require.NoError(err)
				statusWant = "completed"
			} else {
				require.Error(err)
			}
			require.EqualValues(1, summary.MessagesAdded)
			var status string
			var processed, added int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status,messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &processed, &added))
			assert.Equal(statusWant, status)
			assert.EqualValues(1, processed)
			assert.Equal(summary.MessagesAdded, added)
			if mode == "cancelled" {
				require.ErrorIs(err, context.Canceled)
				replay, replayErr := Import(t.Context(), st, opts)
				require.NoError(replayErr)
				assert.EqualValues(2, replay.MessagesAdded+int64(replay.Duplicates))
				assert.Positive(replay.Duplicates)
			}
			if mode == "completed" {
				_, err = Import(t.Context(), st, opts)
				require.NoError(err)
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&processed, &added))
				assert.EqualValues(1, processed)
				assert.Zero(added)
			}
		})
	}
}
