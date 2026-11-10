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
			write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
			opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}}
			_, err := Import(t.Context(), st, opts)
			require.NoError(err)
			var before []byte
			var id int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&id))
			before, err = st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			if mode == "conversation" {
				write(t, root, "Groups/DM a/messages.json", strings.ReplaceAll(fixtureMessages(t), "group/topic/message", "group/topic/new"))
				write(t, root, "Groups/Space b/messages.json", fixtureMessages(t))
				write(t, root, "Groups/Space c/messages.json", strings.ReplaceAll(fixtureMessages(t), "group/topic/message", "group/topic/later"))
			} else {
				_, err = st.DB().ExecContext(t.Context(), `UPDATE message_raw SET raw_format='future_google_chat'`)
				require.NoError(err)
			}
			summary, err := Import(t.Context(), st, opts)
			wantStatus := "failed"
			if mode == "conversation" {
				require.NoError(err)
				assert.EqualValues(2, summary.MessagesAdded)
				assert.Equal(1, summary.Unsupported)
				replayed, err := Import(t.Context(), st, opts)
				require.NoError(err)
				assert.Zero(replayed.MessagesAdded)
				assert.Equal(2, replayed.Duplicates)
				assert.Equal(1, replayed.Unsupported)
				wantStatus = "completed"
			} else {
				assert.Zero(summary.MessagesAdded)
				require.ErrorContains(err, "stored raw format")
			}
			raw, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(before, raw)
			var status string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
			assert.Equal(wantStatus, status)
		})
	}
}

func TestImportGoogleChatRootAliasesKeepProfilesAndMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Takeout/Google Chat/Groups/DM a/messages.json", fixtureMessages(t))
	write(t, root, "Takeout/Google Chat/Groups/DM a/File-example.txt", "synthetic")
	write(t, root, "Takeout/Google Chat/Users/User a/user_info.json", `{"user":{"email":"owner@example.com"}}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{filepath.Join(root, "Takeout/Google Chat/Groups"), root, filepath.Join(root, "Takeout/Google Chat")}, AttachmentsDir: t.TempDir()})
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	var config string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sync_config FROM sources`).Scan(&config))
	assert.Contains(config, "owner_profile_path")
}

func TestImportGoogleChatReportsUnknownRosterMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
	write(t, root, "Groups/DM a/group_info.json", `{"members":[{"name":"Synthetic bot","user_type":"Bot"}]}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(err)
	assert.Equal(1, summary.UnknownMembers)
	assert.Zero(summary.UnknownSenders)
	assert.Zero(summary.Unsupported)
	var processed int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT messages_processed FROM sync_runs`).Scan(&processed))
	assert.EqualValues(1, processed)
}

func TestImportGoogleChatDoesNotInferRecipientsFromRoster(t *testing.T) {
	for _, tc := range []struct {
		name, creatorEmail string
		fromCount          int
		creatorInRoster    bool
	}{
		{"missing creator email", "", 0, false},
		{"creator outside roster", "senderneedle@example.com", 1, false},
		{"creator in roster", "senderneedle@example.com", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			require.True(st.FTS5Available(), "regression requires real FTS")
			root := t.TempDir()
			data := strings.Replace(fixtureMessages(t), `"email":"owner@example.com",`, "", 1)
			if tc.creatorEmail != "" {
				data = strings.Replace(fixtureMessages(t), "owner@example.com", tc.creatorEmail, 1)
			}
			write(t, root, "Groups/DM synthetic/messages.json", data)
			roster := `{"members":[{"email":"recipientneedle1@example.com"},{"email":"recipientneedle2@example.com"}]}`
			if tc.creatorInRoster {
				roster = strings.Replace(roster, "recipientneedle1@example.com", tc.creatorEmail, 1)
			}
			write(t, root, "Groups/DM synthetic/group_info.json", roster)
			summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}, AttachmentsDir: t.TempDir()})
			require.NoError(err)
			require.EqualValues(1, summary.MessagesAdded)
			var toCount, fromCount int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='to'`).Scan(&toCount))
			assert.Zero(toCount, "membership alone cannot establish delivery recipients")
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='from'`).Scan(&fromCount))
			assert.Equal(tc.fromCount, fromCount)
			rows, err := st.DB().QueryContext(t.Context(), `SELECT p.email_address FROM conversation_participants cp JOIN participants p ON p.id=cp.participant_id`)
			require.NoError(err)
			defer func() { _ = rows.Close() }()
			var members []string
			for rows.Next() {
				var email string
				require.NoError(rows.Scan(&email))
				members = append(members, email)
			}
			require.NoError(rows.Err())
			require.NoError(rows.Close())
			wantMembers := []string{"recipientneedle1@example.com", "recipientneedle2@example.com"}
			if tc.creatorInRoster {
				wantMembers[0] = tc.creatorEmail
			} else if tc.creatorEmail != "" {
				wantMembers = append(wantMembers, tc.creatorEmail)
			}
			assert.ElementsMatch(wantMembers, members, "observed roster and sender membership remain")
			for _, rebuilt := range []bool{false, true} {
				if rebuilt {
					n, err := st.RebuildFTSContext(t.Context(), nil)
					require.NoError(err)
					require.EqualValues(1, n)
				}
				for _, search := range []struct {
					term string
					want int
				}{{"recipientneedle1", 0}, {"recipientneedle2", 0}, {"Bodyneedle", 1}, {"senderneedle", tc.fromCount}} {
					_, count, err := st.SearchMessagesContext(t.Context(), search.term, 0, 10)
					require.NoError(err)
					assert.EqualValues(search.want, count, "term=%s rebuilt=%t", search.term, rebuilt)
				}
			}
		})
	}
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
	if r.Message != "sql tx commit" || h.canceled {
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

// Observe the existing synchronous post-commit log, then verify durable rows.
// This needs no production hook, polling or timing threshold. Slow logging stays
// disabled so it cannot replace the commit event on a loaded runner.
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
