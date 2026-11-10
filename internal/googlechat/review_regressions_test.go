package googlechat

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A known email can exist without a name before Takeout supplies that name.
// Use public Store setup and the real importer; no provider/user data is read.
func TestImportGoogleChatFillsMissingParticipantName(t *testing.T) {
	for _, tc := range []struct{ name, existing, want string }{
		{"nameless", "", "Synthetic Sender"},
		{"curated", "Kept Name", "Kept Name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			pid, err := st.EnsureParticipantContext(t.Context(), "sender@example.com", tc.existing, "example.com")
			require.NoError(err)
			root := t.TempDir()
			write(t, root, "Groups/DM synthetic/group_info.json", `{"members":[{"email":"sender@example.com"}]}`)
			testutil.WriteFile(t, root, "Groups/DM synthetic/messages.json", []byte(`{"messages":[{"message_id":"synthetic/topic/message","creator":{"email":"sender@example.com","name":"Synthetic Sender","user_type":"Human"},"text":"synthetic body","created_date":"Monday, January 18, 2021 at 1:27:33 PM UTC"}]}`))
			summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
			require.NoError(err)
			require.EqualValues(1, summary.MessagesAdded)
			var name string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(display_name,'') FROM participants WHERE id = ?`), pid).Scan(&name))
			assert.Equal(t, tc.want, name, "Takeout should fill a missing name and preserve an existing one")
		})
	}
}

func TestImportGoogleChatReplayIdentityFence(t *testing.T) {
	for _, mode := range []string{"conversation", "format"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			write(t, root, "Groups/DM a/messages.json", one)
			opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}}
			_, err := Import(t.Context(), st, opts)
			require.NoError(err)
			var before []byte
			var id int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&id))
			before, err = st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			if mode == "conversation" {
				write(t, root, "Groups/DM a/messages.json", strings.ReplaceAll(one, "group/topic/message", "group/topic/new"))
				write(t, root, "Groups/Space b/messages.json", one)
			} else {
				_, err = st.DB().ExecContext(t.Context(), `UPDATE message_raw SET raw_format='future_google_chat'`)
				require.NoError(err)
			}
			summary, err := Import(t.Context(), st, opts)
			require.Error(err)
			if mode == "conversation" {
				assert.EqualValues(1, summary.MessagesAdded)
				require.ErrorContains(err, "conflicting message ID")
			} else {
				assert.Zero(summary.MessagesAdded)
				require.ErrorContains(err, "stored raw format")
			}
			raw, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(before, raw)
			var status string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
			assert.Equal("failed", status)
		})
	}
}

func TestImportGoogleChatRootAliasesKeepProfilesAndMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Takeout/Google Chat/Groups/DM a/messages.json", one)
	write(t, root, "Takeout/Google Chat/Groups/DM a/File-example.txt", "synthetic")
	write(t, root, "Takeout/Google Chat/Users/User a/user_info.json", `{"user":{"email":"owner@example.com"}}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{filepath.Join(root, "Takeout/Google Chat/Groups"), root, filepath.Join(root, "Takeout/Google Chat")}, AttachmentsDir: t.TempDir()})
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	var config string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sync_config FROM sources`).Scan(&config))
	assert.Contains(config, "owner_profiles")
}

func TestImportGoogleChatReportsUnknownRosterMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", one)
	write(t, root, "Groups/DM a/group_info.json", `{"members":[{"email":"not an email"}]}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(err)
	assert.Equal(1, summary.UnknownMembers)
	assert.Zero(summary.UnknownSenders)
	assert.Zero(summary.Unsupported)
	var processed int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT messages_processed FROM sync_runs`).Scan(&processed))
	assert.EqualValues(1, processed)
}

type cancelOnCommittedMessage struct {
	slog.Handler

	cancel   context.CancelFunc
	st       *store.Store
	ctx      context.Context
	canceled bool
}

func (*cancelOnCommittedMessage) Enabled(context.Context, slog.Level) bool { return true }
func (h *cancelOnCommittedMessage) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "sql tx begin" || h.canceled {
		return nil
	}
	var n int
	if err := h.st.DB().QueryRowContext(h.ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		h.canceled = true
		h.cancel()
	}
	return nil
}
func cancelAfterCommittedMessage(t *testing.T, st *store.Store, opts ImportOptions) (*ImportSummary, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := &cancelOnCommittedMessage{Handler: slog.DiscardHandler, cancel: cancel, st: st, ctx: t.Context()}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)
	store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
	defer store.ConfigureSQLLogging(store.SQLLogOptions{})
	return Import(ctx, st, opts)
}
