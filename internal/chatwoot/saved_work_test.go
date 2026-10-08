package chatwoot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// --limit bounds history only. Capping refreshes would recheck the lowest
// artifact IDs every run and let later recordings leave their window unread.
func TestLimitBoundsHistoryNotArtifactRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	messages := []map[string]any{}
	for id := int64(901); id <= 902; id++ {
		message := contractMessage(id, 1767225600+id, nil)
		message["attachments"] = []any{map[string]any{
			"id": id + 1000, "file_type": "audio", "content_type": "audio/ogg",
			"data_url": fmt.Sprintf("https://chatwoot.example.com/audio-%d.ogg", id),
		}}
		messages = append(messages, message)
	}
	api := newContractAPI(t, 2, messages)
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7}
	initial, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	require.False(initial.Partial)
	opts.Limit = 1
	limited, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(2, limited.MessagesProcessed, "--limit bounds history, so every pending artifact is rechecked")
	assert.False(limited.Partial)
	conversation := savedState(t, st, source).Conversations["42"]
	require.NotNil(conversation)
	assert.Empty(conversation.Pending)
	assert.Len(conversation.Artifacts, 2)
}

func TestSavedWorkAccessFailuresKeepHealthyProgress(t *testing.T) {
	for _, tc := range []struct {
		name           string
		count, budget  int
		messages, full bool
	}{
		{"detail", 1, 100, false, false}, {"messages", 1, 100, true, false},
		{"budget_full", 1, 7, false, true}, {"failed_batch", 100, 1000, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			api := newContractAPI(t, 1000, nil)
			for id := int64(1); id <= int64(tc.count+1); id++ {
				api.AddMessage(id, id*10+1, now())
				api.AddMessage(id, id*10+2, now().Add(time.Second))
			}
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7, Limit: 1})
			require.NoError(err)
			api.Hidden, api.DeniedDetails, api.DeniedMessages = map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
			for id := int64(1); id <= int64(tc.count); id++ {
				api.Hidden[id] = !tc.messages
				api.DeniedDetails[id] = !tc.messages
				api.DeniedMessages[id] = tc.messages
			}
			api.AddMessage(int64(tc.count+2), 9001, now().Add(2*time.Second))
			opts := ImportOptions{InboxID: 7, Full: tc.full}
			api.TakeRequests()
			for attempt := range 3 {
				restarted := NewImporter(st, api.client(t))
				restarted.requestBudget = tc.budget
				if attempt > 0 {
					restarted.requestBudget = 1000
				}
				sum, err := restarted.Import(t.Context(), opts)
				if attempt > 0 || err != nil {
					require.ErrorContains(err, "HTTP 401")
				}
				assert.True(sum.Partial)
				assert.LessOrEqual(len(api.TakeRequests()), restarted.requestBudget+1)
			}
			archived := contractMessageIDs(t, st)
			assert.Contains(archived, int64((tc.count+1)*10+2), "healthy saved history progresses")
			assert.Contains(archived, int64(9001), "later listing pages progress")
			state, err := NewImporter(st, api.client(t)).resumeState(source.ID, source.Identifier)
			require.NoError(err)
			require.Contains(state.Conversations, "1")
			assert.NotEmpty(state.Conversations["1"].Pending)
			assert.Empty(state.Walk, "attempted failures retain history without holding completed discovery open")
			assert.Equal(now().UTC(), state.ReconciledAt)
			api.Hidden, api.DeniedDetails, api.DeniedMessages = nil, nil, nil
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7})
			require.NoError(err)
			assert.Len(contractMessageIDs(t, st), (tc.count+1)*2+1)
		})
	}
}

func TestSavedWorkFatalErrorsStopReads(t *testing.T) {
	for _, kind := range []string{"checkpoint", "database", "filesystem", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			media := newMediaRefreshServer(t)
			router := newChatwootMediaRouter(t, media.server)
			api := newContractAPI(t, 1000, nil)
			api.mediaRouter = router
			for id := int64(1); id <= 3; id++ {
				api.AddMessage(id, id*10, now())
			}
			api.DeniedMessages = map[int64]bool{1: true}
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			opts := mediaRefreshOptions(t)
			if kind == "filesystem" {
				blocked := filepath.Join(opts.AttachmentsDir, "blocked")
				require.NoError(os.WriteFile(blocked, []byte("file"), 0600))
				opts.AttachmentsDir = blocked
				api.Conversations[2][0]["attachments"] = []any{map[string]any{"id": int64(2001), "file_type": "image", "data_url": router.url(t, media.server, "/recording-a.ogg")}}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var setupErr error
			api.OnRequest = func(path string) {
				if path != "/conversations/2/messages" && (kind != "checkpoint" || path != "/conversations/1/messages") {
					return
				}
				api.OnRequest = nil
				switch kind {
				case "checkpoint":
					trigger := `CREATE TRIGGER reject_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs BEGIN SELECT CASE WHEN NEW.status = 'failed' THEN RAISE(ABORT, 'synthetic failed checkpoint') ELSE RAISE(ABORT, 'synthetic checkpoint failure') END; END`
					if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
						trigger = `CREATE FUNCTION reject_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status = 'failed' THEN RAISE EXCEPTION 'synthetic failed checkpoint'; END IF; RAISE EXCEPTION 'synthetic checkpoint failure'; END $$; CREATE TRIGGER reject_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs FOR EACH ROW EXECUTE FUNCTION reject_checkpoint()`
					}
					_, setupErr = st.DB().Exec(trigger)
				case "database":
					_, setupErr = st.DB().Exec(`DROP TABLE message_bodies`)
				case "cancellation":
					cancel()
				}
			}
			_, err := imp.Import(ctx, opts)
			requests := api.TakeRequests()
			require.NoError(setupErr)
			require.Error(err)
			assert.Contains(err.Error(), "conversation 1", "earlier provider errors survive a fatal error")
			for _, request := range requests {
				assert.False(strings.HasPrefix(request, "messages 3 "), "fatal errors stop later reads")
			}
			if kind == "checkpoint" {
				assert.Contains(err.Error(), "synthetic checkpoint failure")
				assert.Contains(err.Error(), "synthetic failed checkpoint", "failure-checkpoint errors remain visible")
			} else {
				if kind == "cancellation" {
					require.ErrorIs(err, context.Canceled)
				}
				state, stateErr := NewImporter(st, api.client(t)).resumeState(source.ID, source.Identifier)
				require.NoError(stateErr)
				assert.NotEmpty(state.Conversations["2"].Pending)
			}
		})
	}
}

func TestSavedArtifactCheckpointRetiresPrivateMessageWhenExcluded(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "private_excluded"
		if moved {
			name = "moved_inbox"
		}
		t.Run(name, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			message := contractMessage(901, 1767225600, nil)
			message["private"] = !moved
			message["attachments"] = []any{map[string]any{"id": 2001, "file_type": "audio", "content_type": "audio/ogg", "data_url": "https://chatwoot.example.com/audio.ogg"}}
			api := newContractAPI(t, 2, []map[string]any{message})
			st := testutil.NewTestStore(t)
			importer, source := contractRegister(t, st, api)
			opts := ImportOptions{InboxID: 7, IncludePrivate: true}
			_, err := importer.Import(t.Context(), opts)
			must.NoError(err)
			must.Contains(savedState(t, st, source).Conversations["42"].Artifacts, "901")
			if moved {
				api.Mu.Lock()
				api.ConversationInboxID = 8
				for _, message := range api.Conversations[42] {
					message["inbox_id"] = int64(8)
				}
				api.Mu.Unlock()
			} else {
				opts.IncludePrivate = false
			}
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
			must.NoError(err)
			checks.NotContains(savedState(t, st, source).Conversations, "42")
			if !moved {
				api.TakeRequests()
				_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				must.NoError(err)
				for _, request := range api.TakeRequests() {
					checks.NotContains(request, "messages ", "later syncs must not refetch excluded private artifacts")
				}
				return
			}
			old, err := st.MessageExistsBatch(source.ID, []string{"901"})
			must.NoError(err)
			checks.Contains(old, "901", "archived rows in the former inbox remain available")
			importer8 := NewImporter(st, api.client(t))
			sources8, err := importer8.Register(t.Context(), []Inbox{{ID: 8}})
			must.NoError(err)
			must.Len(sources8, 1)
			_, err = importer8.Import(t.Context(), ImportOptions{InboxID: 8, IncludePrivate: true})
			must.NoError(err)
			archived, err := st.MessageExistsBatch(sources8[0].ID, []string{"901"})
			must.NoError(err)
			checks.Contains(archived, "901", "the new inbox source archives the moved message")
		})
	}
}
