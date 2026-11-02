package cmd

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/threads"
)

type failedReplyOutput struct {
	http.ResponseWriter

	writes *atomic.Int32
}

func (w failedReplyOutput) Write([]byte) (int, error) {
	w.writes.Add(1)
	return 0, errors.New("synthetic response output failure")
}

func TestSendReplyDaemonLogsExcludePrivateBody(t *testing.T) {
	for _, afterPublish := range []bool{false, true} {
		t.Run(fmt.Sprint("after-publish=", afterPublish), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			f := newThreadsSendDaemonFixture(t)
			const body = "synthetic-private-reply-body"
			f.draft.Body = body
			parent := f.root
			if !afterPublish {
				source, err := f.adapter.store.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
				require.NoError(err)
				conv, err := f.adapter.store.EnsureConversationWithType(source.ID, "CEXAMPLE", "channel", "Synthetic")
				require.NoError(err)
				parent, err = f.adapter.store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "synthetic-parent", MessageType: "chat"})
				require.NoError(err)
			}
			var logs bytes.Buffer
			handler := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}).Router()
			var failedWrites atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if afterPublish {
					w = failedReplyOutput{ResponseWriter: w, writes: &failedWrites}
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: f.cfg.Server.APIKey, AllowInsecure: true})
			require.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"send-reply", strconv.FormatInt(parent, 10), "--body=" + body, "--json"}}, func(string, string) error { return nil })
			require.Error(err)
			assert.Contains(logs.String(), "failed to run CLI command")
			assert.Contains(logs.String(), `"command":"send-reply"`)
			assert.NotContains(logs.String(), body)
			assert.NotContains(logs.String(), "--body")
			if afterPublish {
				assert.Equal(int32(1), f.publishes.Load())
				assert.Positive(failedWrites.Load())
				receipt, receiptErr := f.adapter.store.GetThreadsReplySendContext(t.Context(), threadsDirectReplyDraftID(f.source.ID, parent, body))
				require.NoError(receiptErr)
				assert.Equal("complete", receipt.State)
				assert.Equal("500", receipt.MediaID)
			} else {
				assert.Zero(f.publishes.Load())
			}
		})
	}
}

func TestThreadsDraftReplyCLIManagedWorkflow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newThreadsSendDaemonFixture(t)
	f.cfg.Threads.Sends = nil // Drafting requires no publication opt-in.
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	cfg := *f.cfg
	cfg.Remote = config.RemoteConfig{URL: server.URL, APIKey: f.cfg.Server.APIKey, AllowInsecure: true}
	ctx := withStoreResolverConfig(t, &cfg)
	var output bytes.Buffer
	command := newDraftReplyCommand()
	command.SetContext(ctx)
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{strconv.FormatInt(f.root, 10), "--body", f.draft.Body, "--json"})
	require.NoError(command.Execute())
	var draft chatDraftOutput
	require.NoError(json.Unmarshal(output.Bytes(), &draft))
	assert.Equal("msgvault", draft.Location)
	assert.Equal("threads", draft.SourceType)
	assert.Equal("100", draft.ReplyToSourceMessageID)
	assert.Equal(f.draft.Body, draft.Body)
	assert.Equal(int64(1), draft.Revision)
	assert.Zero(f.creates.Load())
	assert.Zero(f.publishes.Load())
	f.cfg.Threads.Sends = []config.ThreadsSendConfig{{SourceID: f.source.ID}}
	output.Reset()
	send := newSendReplyCommand()
	send.SetContext(ctx)
	send.SetOut(&output)
	send.SetErr(&output)
	send.SetArgs([]string{"--draft", draft.DraftID, "--revision", "1", "--json"})
	require.NoError(send.Execute())
	var result threads.SendResult
	require.NoError(json.Unmarshal(output.Bytes(), &result))
	assert.Equal("complete", result.Status)
	assert.Equal(int32(1), f.publishes.Load())
}

func TestThreadsDraftReplyKeepsSourceAuthority(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newThreadsSendDaemonFixture(t)
	for _, flags := range [][]string{{"--from=owner@example.com"}, {"--all"}, {"--source-id=1"}, {"--account=other"}} {
		args := append([]string{"draft-reply", strconv.FormatInt(f.root, 10), "--body=synthetic reply", "--json"}, flags...)
		err := f.adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{Args: args}, nil)
		require.ErrorContains(err, "invalid_args")
	}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "threads", Identifier: "threads:123:99"}}}
	err := f.adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{Args: []string{"draft-reply", strconv.FormatInt(f.root, 10), "--body=synthetic reply"}, Grant: grant}, nil)
	require.ErrorContains(err, "not_permitted")
	assert.Zero(f.creates.Load())
	assert.Zero(f.publishes.Load())
}

func TestThreadsSendOptInRequiresDaemonRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newThreadsSendDaemonFixture(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, nil, 0o600))
	running, err := config.Load(path, f.cfg.HomeDir)
	require.NoError(err)
	f.adapter.config = running
	require.NoError(reserveThreadsSendSourceIDs(t.Context(), f.adapter.store, running))
	require.NoError(os.WriteFile(path, []byte(fmt.Sprintf("[[threads.sends]]\nsource_id=%d\n", f.source.ID)), 0o600))
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	loaded, err := running.ReloadConfigFile(snapshot)
	require.NoError(err)
	require.Len(loaded.Threads.Sends, 1)
	assert.Empty(running.Threads.Sends, "persisted settings reload must not activate a new send permission")
	_, _, err = f.run(t.Context(), f.args())
	require.ErrorContains(err, "reply_not_permitted")
	assert.Zero(f.publishes.Load())
	// A restart uses the newly loaded config only after reserving its IDs.
	require.NoError(reserveThreadsSendSourceIDs(t.Context(), f.adapter.store, loaded))
	f.adapter.config = loaded
	result, _, err := f.run(t.Context(), f.args())
	require.NoError(err)
	assert.Equal("complete", result.Status)
	assert.Equal(int32(1), f.publishes.Load())
}

func TestThreadsConfiguredFutureSourceDoesNotInheritSendOptIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newThreadsSendDaemonFixture(t)
	const configuredID int64 = 500
	f.cfg.Threads.Sends = []config.ThreadsSendConfig{{SourceID: configuredID}}
	require.NoError(reserveThreadsSendSourceIDs(t.Context(), f.adapter.store, f.cfg))
	source, err := f.adapter.store.GetOrCreateSource("threads", "threads:123:21")
	require.NoError(err)
	require.Greater(source.ID, configuredID)
	parent, err := threads.NewWriter(f.adapter.store, source.ID, source.Identifier).Apply(t.Context(), threads.Media{ID: "200", Timestamp: f.provider.clock(), Text: new("Synthetic future post")}, threads.ItemOrigin{Kind: "own_posts"})
	require.NoError(err)
	_, _, err = f.run(t.Context(), []string{"send-reply", strconv.FormatInt(parent, 10), "--body=synthetic reply", "--json"})
	require.ErrorContains(err, "reply_not_permitted")
	assert.Zero(f.opens.Load())
	assert.Zero(f.creates.Load())
	assert.Zero(f.publishes.Load())
}

func TestThreadsReplacementSourceDoesNotInheritSendOptIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newThreadsSendDaemonFixture(t)
	require.NoError(f.adapter.store.RemoveSource(f.source.ID))
	source, err := f.adapter.store.GetOrCreateSource("threads", "threads:123:21")
	require.NoError(err)
	require.NotEqual(f.source.ID, source.ID)
	parent, err := threads.NewWriter(f.adapter.store, source.ID, source.Identifier).Apply(t.Context(), threads.Media{ID: "200", Timestamp: f.provider.clock(), Text: new("Synthetic replacement post")}, threads.ItemOrigin{Kind: "own_posts"})
	require.NoError(err)
	_, _, err = f.run(t.Context(), []string{"send-reply", strconv.FormatInt(parent, 10), "--body=synthetic reply", "--json"})
	require.ErrorContains(err, "reply_not_permitted")
	assert.Zero(f.creates.Load())
	assert.Zero(f.publishes.Load())
}

func TestThreadsPreUpgradeDeletedSourceReservesStaleOptIn(t *testing.T) {
	testutil.SkipIfPostgres(t, "models deleted reusable SQLite row IDs; PostgreSQL reservation/restore sequences have separate regressions")
	assert := assert.New(t)
	require := require.New(t)

	f := newThreadsSendDaemonFixture(t)
	st := f.adapter.store
	// Model a pre-upgrade deletion: neither the source row nor a high-water
	// record survives, but the exact ID-only operator configuration does.
	_, err := st.DB().Exec(`DROP TRIGGER sources_id_high_water_insert;
DROP TRIGGER sources_id_high_water_delete;
DROP TABLE source_id_reservations;
DELETE FROM archive_metadata WHERE key='source_id_high_water';
UPDATE archive_metadata SET value='16' WHERE key='schema_version'`)
	require.NoError(err)
	require.NoError(st.RemoveSource(f.source.ID))
	require.NoError(st.InitSchemaContext(t.Context()))
	var ceiling int64
	require.NoError(st.DB().QueryRow(`SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='source_id_high_water'`).Scan(&ceiling))
	require.Zero(ceiling, "schema setup has no deleted identity history to recover")
	// This is the same reservation performed before runServe starts writers.
	require.NoError(reserveThreadsSendSourceIDs(t.Context(), st, f.cfg))
	source, err := st.GetOrCreateSource("threads", "threads:123:21")
	require.NoError(err)
	require.Greater(source.ID, f.source.ID)
	parent, err := threads.NewWriter(st, source.ID, source.Identifier).Apply(t.Context(), threads.Media{ID: "200", Timestamp: f.provider.clock(), Text: new("Synthetic post-upgrade post")}, threads.ItemOrigin{Kind: "own_posts"})
	require.NoError(err)
	_, _, err = f.run(t.Context(), []string{"send-reply", strconv.FormatInt(parent, 10), "--body=synthetic reply", "--json"})
	require.ErrorContains(err, "reply_not_permitted")
	assert.Zero(f.opens.Load())
	assert.Zero(f.creates.Load())
	assert.Zero(f.publishes.Load())
	// An explicit insertion cannot bypass the allocator's reserved-ID fence.
	_, err = st.DB().Exec(`INSERT INTO sources(id, source_type, identifier) VALUES(?, 'threads', 'threads:123:22')`, f.source.ID)
	require.ErrorContains(err, "source ID has already been allocated or reserved")
}

func TestThreadsSubsetSourceInsertionPreservesAllocationFence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newThreadsSendDaemonFixture(t, true)
	st := f.adapter.store
	_, err := st.DB().Exec(`INSERT INTO sources(id, source_type, identifier) VALUES(500, 'slack', 'TEXAMPLE:UEXAMPLE')`)
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(500, "CEXAMPLE", "channel", "Synthetic subset channel")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: 500, ConversationID: conv, SourceMessageID: "subset-parent", MessageType: "chat"})
	require.NoError(err)
	dst := t.TempDir()
	result, err := store.CopySubset(f.cfg.DatabaseDSN(), dst, 100, false)
	require.NoError(err)
	assert.Equal(int64(2), result.Sources)
	subset, err := store.OpenForTest(filepath.Join(dst, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(subset.Close()) })
	copied, err := subset.GetSourceByTypeAndIdentifier("threads", f.source.Identifier)
	require.NoError(err)
	assert.Equal(f.source.ID, copied.ID)
	require.NoError(subset.RemoveSource(500))
	_, err = subset.DB().Exec(`INSERT INTO sources(id, source_type, identifier) VALUES(500, 'threads', 'threads:123:21')`)
	require.ErrorContains(err, "source ID has already been allocated or reserved")
	next, err := subset.GetOrCreateSource("threads", "threads:123:21")
	require.NoError(err)
	assert.Greater(next.ID, int64(500))
}
