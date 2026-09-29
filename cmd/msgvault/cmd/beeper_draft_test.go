package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperDraftParser(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	created, err := parseBeeperDraftArgs([]string{"draft-beeper", "create", "--source-id=42", "--chat-id=!room:beeper.local", "--body=hello", "--json"})
	requirements.NoError(err)
	assertions.Equal(int64(42), created.SourceID)
	assertions.Equal("!room:beeper.local", created.ChatID)
	assertions.Equal("hello", created.Body)
	assertions.True(created.JSON)

	edited, err := parseBeeperDraftArgs([]string{"draft-beeper", "edit", "beeper-draft-id", "--revision=2", "--body=updated"})
	requirements.NoError(err)
	assertions.Equal(int64(2), edited.Revision)
	assertions.Equal("beeper-draft-id", edited.DraftID)
	read, err := parseBeeperDraftArgs([]string{"draft-beeper", "get", "beeper-draft-id", "--json=false"})
	requirements.NoError(err)
	assertions.Equal("beeper-draft-id", read.DraftID)
	assertions.False(read.JSON)

	_, err = parseBeeperDraftArgs([]string{"draft-beeper", "clear", "beeper-draft-id", "--revision=0"})
	requirements.Error(err)
	for _, args := range [][]string{
		{"draft-beeper", "create", "--source-id=42", "--chat-id=!room:beeper.local", "--body=hello", "--help"},
		{"draft-beeper", "get", "beeper-draft-id", "--help"},
		{"draft-beeper", "edit", "beeper-draft-id", "--revision=2", "--body=updated", "--help"},
		{"draft-beeper", "clear", "beeper-draft-id", "--revision=2", "--help"},
	} {
		_, helpErr := parseBeeperDraftArgs(args)
		requirements.ErrorContains(helpErr, "invalid_args")
	}
}

func TestBeeperDraftHelpDoesNotMutate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newBeeperDraftCommandFixture(t, "")
	sourceID := fixture.sourceID()
	var events []api.CLIRunEvent
	run := func(args ...string) error {
		events = nil
		return fixture.adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		})
	}
	requirements.ErrorContains(run("draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "hello", "--help"), "invalid_args")
	assertions.Empty(events)
	assertions.Equal(0, fixture.patches)

	requirements.NoError(fixture.run(t.Context(), "draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "hello"))
	draftID, revision := latestBeeperDraft(t, fixture.store)
	for _, args := range [][]string{
		{"draft-beeper", "get", draftID, "--help"},
		{"draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated", "--help"},
		{"draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(revision, 10), "--help"},
	} {
		requirements.ErrorContains(run(args...), "invalid_args")
		assertions.Empty(events)
	}
	assertions.Equal(1, fixture.patches)
	stored, err := fixture.store.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Equal(revision, stored.Revision)
}

func TestBeeperDraftDaemonRouting(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assertions.Equal([]string{"draft-beeper", "get", "--json", "beeper-draft-id"}, req.Args)
		assertions.Empty(req.Env)
		assertions.Empty(req.Cwd)
	}, `{"type":"stdout","data":"partial\n"}`, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	parent := &cobra.Command{Use: "draft-beeper"}
	child := newBeeperDraftGetCommand()
	parent.AddCommand(child)
	root.AddCommand(parent)
	child.SetContext(ctx)
	requirements.NoError(child.Flags().Set("json", "true"))
	var stdout, stderr bytes.Buffer
	child.SetOut(&stdout)
	child.SetErr(&stderr)
	requirements.NoError(child.RunE(child, []string{"beeper-draft-id"}))
	assertions.Equal("partial\n", stdout.String())
	assertions.Empty(stderr.String())
	assertions.Equal(int32(1), requests.Load())
}

func TestBeeperDraftAuthorization(t *testing.T) {
	requirements := require.New(t)
	source := &store.Source{ID: 42, SourceType: "beeper", Identifier: "signal"}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "signal"}}}
	requirements.NoError(authorizeBeeperDraft([]config.BeeperDraftSource{{SourceID: 42}}, source, grant, agentgrant.PermissionDraftEdit))
	requirements.ErrorContains(authorizeBeeperDraft(nil, source, nil, agentgrant.PermissionDraftEdit), "draft_disabled")
	requirements.ErrorContains(authorizeBeeperDraft([]config.BeeperDraftSource{{SourceID: 41}}, source, grant, agentgrant.PermissionDraftEdit), "draft_disabled")
	wrong := &store.Source{ID: 42, SourceType: "gmail", Identifier: "signal"}
	requirements.ErrorContains(authorizeBeeperDraft([]config.BeeperDraftSource{{SourceID: 42}}, wrong, nil, agentgrant.PermissionDraftEdit), "draft_disabled")
}

func TestBeeperDraftPolicySnapshot(t *testing.T) {
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Beeper.Drafts = []config.BeeperDraftSource{{SourceID: 42}}
	snapshot := snapshotBeeperDraftPolicy(cfg)
	cfg.Beeper.Drafts[0].SourceID = 99
	assertions.Equal(int64(42), snapshot[0].SourceID)
}

func TestBeeperDraftIdentityBoundary(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	chat := &beeper.Chat{ID: "!room:beeper.local", AccountID: "signal"}
	obs, err := validateBeeperChat(&store.Source{SourceType: "beeper", Identifier: "signal"}, chat.ID, chat)
	requirements.NoError(err)
	assertions.False(obs.Unknown)
	_, err = validateBeeperChat(&store.Source{SourceType: "beeper", Identifier: "other"}, chat.ID, chat)
	requirements.Error(err)
}

func TestBeeperDraftAgentDelegatedCapabilities(t *testing.T) {
	assertions := assert.New(t)
	draftCommand := newBeeperDraftCommand()
	for _, child := range draftCommand.Commands() {
		assertions.True(agentDelegatedCapable(child), child.Name())
	}
	unrelated := &cobra.Command{Use: "get"}
	draftCommand.AddCommand(unrelated)
	assertions.False(agentDelegatedCapable(unrelated))
}

func TestBeeperDraftDaemonSafetyBoundaries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	t.Run("occupied create", func(t *testing.T) {
		fixture := newBeeperDraftCommandFixture(t, "desktop draft")
		err := fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "candidate")
		requirements.ErrorContains(err, "occupied")
		assertions.Zero(fixture.patches)
	})
	t.Run("external edit conflict", func(t *testing.T) {
		fixture := newBeeperDraftCommandFixture(t, "")
		requirements.NoError(fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "hello"))
		draftID, revision := latestBeeperDraft(t, fixture.store)
		fixture.nativeText = "desktop edit"
		err := fixture.run(t.Context(), "draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated")
		requirements.ErrorContains(err, "conflict")
		assertions.Equal(1, fixture.patches)
	})
	t.Run("unsupported observation conflict", func(t *testing.T) {
		fixture := newBeeperDraftCommandFixture(t, "")
		requirements.NoError(fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "hello"))
		draftID, revision := latestBeeperDraft(t, fixture.store)
		fixture.rawGet = []byte(`{"id":"!room:beeper.local","accountID":"signal","draft":{"future":true}}`)
		err := fixture.run(t.Context(), "draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated")
		requirements.ErrorContains(err, "conflict")
		assertions.Equal(1, fixture.patches)
	})
	t.Run("attachment conflict", func(t *testing.T) {
		fixture := newBeeperDraftCommandFixture(t, "")
		requirements.NoError(fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "hello"))
		draftID, revision := latestBeeperDraft(t, fixture.store)
		fixture.rawGet = []byte(`{"id":"!room:beeper.local","accountID":"signal","draft":{"attachments":{"a":{"id":"a"}}}}`)
		err := fixture.run(t.Context(), "draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(revision, 10))
		requirements.ErrorContains(err, "conflict")
		assertions.Equal(1, fixture.patches)
	})
	t.Run("merged chat", func(t *testing.T) {
		fixture := newBeeperDraftCommandFixture(t, "")
		fixture.merged = true
		err := fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "candidate")
		requirements.ErrorContains(err, "ambiguous_chat")
		assertions.Zero(fixture.patches)
	})
	t.Run("grant denied before provider", func(t *testing.T) {
		fixture := newBeeperDraftCommandFixture(t, "")
		err := fixture.adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{
			Args:  []string{"draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "candidate"},
			Grant: &agentgrant.Grant{},
		}, nil)
		requirements.ErrorContains(err, "not_permitted")
		assertions.Zero(fixture.patches)
	})
	t.Run("stale revision", func(t *testing.T) {
		fixture := newBeeperDraftCommandFixture(t, "")
		requirements.NoError(fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "hello"))
		draftID, revision := latestBeeperDraft(t, fixture.store)
		err := fixture.run(t.Context(), "draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision-1, 10), "--body", "updated")
		requirements.ErrorContains(err, "revision_mismatch")
		assertions.Equal(1, fixture.patches)
	})
}

func TestBeeperDraftSetDisconnectLeavesPending(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newBeeperDraftCommandFixture(t, "")
	requirements.NoError(fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "hello"))
	draftID, revision := latestBeeperDraft(t, fixture.store)
	fixture.disconnectSet = true
	var event api.CLIRunEvent
	err := fixture.adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated",
	}, Grant: &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "signal"}}}}, func(got api.CLIRunEvent) error {
		event = got
		return nil
	})
	requirements.ErrorContains(err, "remote_unknown")
	assertions.Equal(cliStreamStderr, event.Type)
	assertions.Contains(event.Data, draftID)
	assertions.Contains(event.Data, "revision "+strconv.FormatInt(revision+1, 10))
	assertions.Contains(event.Data, "pending phase: remote_unknown")
	assertions.Contains(event.Data, "content: <withheld>")
	assertions.Contains(event.Data, "native: <withheld>")
	assertions.NotContains(event.Data, "hello")
	assertions.NotContains(event.Data, "updated")
	assertions.Equal(3, fixture.patches)
	pending, err := fixture.store.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	requirements.NotNil(pending.Pending)
	assertions.Equal(store.BeeperDraftPhaseRemoteUnknown, pending.Pending.Phase)
	assertions.Equal("updated", pending.Pending.Candidate)
	patches := fixture.patches
	err = fixture.run(t.Context(), "draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(pending.Revision, 10), "--body", "updated")
	requirements.ErrorContains(err, "pending")
	assertions.Equal(patches, fixture.patches)
}

func TestBeeperDraftPendingClearRecovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	testutil.SkipIfPostgres(t, "rollback injection uses a SQLite trigger")
	fixture := newBeeperDraftCommandFixture(t, "")
	requirements.NoError(fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "hello"))
	draftID, revision := latestBeeperDraft(t, fixture.store)
	claimed, err := fixture.store.ClaimBeeperDraftContext(t.Context(), draftID, revision, store.BeeperDraftOperationEdit, "updated")
	requirements.NoError(err)
	requirements.NoError(fixture.store.RecordBeeperDraftOutcomeContext(t.Context(), draftID, claimed.Revision, store.BeeperDraftPhaseClearDispatched, "dispatching"))
	requirements.NoError(fixture.store.RecordBeeperDraftOutcomeContext(t.Context(), draftID, claimed.Revision, store.BeeperDraftPhaseClearConfirmed, "cleared"))
	requirements.NoError(fixture.store.RecordBeeperDraftOutcomeContext(t.Context(), draftID, claimed.Revision, store.BeeperDraftPhaseSetDispatched, "dispatching"))
	requirements.NoError(fixture.store.RecordBeeperDraftOutcomeContext(t.Context(), draftID, claimed.Revision, store.BeeperDraftPhaseAcceptedLocalFailed, "remote_accepted_local_failed"))
	pending, err := fixture.store.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Equal(store.BeeperDraftPhaseAcceptedLocalFailed, pending.Pending.Phase)

	err = fixture.run(t.Context(), "draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(pending.Revision-1, 10))
	requirements.ErrorContains(err, "revision_mismatch")
	assertions.Equal(1, fixture.patches)
	err = fixture.run(t.Context(), "draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(pending.Revision, 10))
	requirements.ErrorContains(err, "conflict")
	assertions.Equal(1, fixture.patches)

	fixture.nativeText = ""
	_, err = fixture.store.DB().Exec(`
CREATE TRIGGER beeper_drafts_retire_failure
BEFORE UPDATE OF committed_text ON beeper_drafts
BEGIN
  SELECT RAISE(FAIL, 'injected retire failure');
END`)
	requirements.NoError(err)
	err = fixture.run(t.Context(), "draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(pending.Revision, 10))
	requirements.ErrorContains(err, "local_persistence_failed")
	assertions.Equal(1, fixture.patches)
	unchanged, err := fixture.store.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Equal(pending.Revision, unchanged.Revision)
	assertions.Equal(store.BeeperDraftPhaseAcceptedLocalFailed, unchanged.Pending.Phase)
	assertions.Equal("updated", unchanged.Pending.Candidate)
	_, err = fixture.store.DB().Exec(`DROP TRIGGER beeper_drafts_retire_failure`)
	requirements.NoError(err)
	requirements.NoError(fixture.run(t.Context(), "draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(pending.Revision, 10)))
	recovered, err := fixture.store.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Nil(recovered.Pending)
	assertions.Nil(recovered.CommittedText)
	assertions.Equal(pending.Revision+1, recovered.Revision)
	assertions.Equal(1, fixture.patches)
}

type beeperDraftCommandFixture struct {
	adapter       *storeAPIAdapter
	store         *store.Store
	source        *store.Source
	nativeText    string
	rawGet        []byte
	getStatus     int
	merged        bool
	disconnectSet bool
	patches       int
	gets          int
}

func newBeeperDraftCommandFixture(t *testing.T, nativeText string) *beeperDraftCommandFixture {
	t.Helper()
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)
	fixture := &beeperDraftCommandFixture{store: st, source: source, nativeText: nativeText}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fixture.gets++
			if fixture.getStatus != 0 {
				w.WriteHeader(fixture.getStatus)
				return
			}
			if fixture.merged {
				_, _ = w.Write([]byte(`{"id":"!room:beeper.local","accountID":"signal","merge":{"chatIDs":["!room:beeper.local"]},"draft":null}`))
				return
			}
			if fixture.rawGet != nil {
				_, _ = w.Write(fixture.rawGet)
				return
			}
			writeBeeperTestChat(w, fixture.nativeText)
			return
		}
		fixture.patches++
		body, readErr := io.ReadAll(r.Body)
		if !assertions.NoError(readErr) {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		var update struct {
			Draft *struct {
				Text string `json:"text"`
			} `json:"draft"`
		}
		if !assertions.NoError(json.Unmarshal(body, &update)) {
			http.Error(w, "decode request", http.StatusBadRequest)
			return
		}
		if update.Draft == nil {
			fixture.nativeText = ""
		} else {
			fixture.nativeText = "rich " + update.Draft.Text
			if fixture.disconnectSet {
				disconnectBeeperTestResponse(w)
				return
			}
		}
		writeBeeperTestChat(w, fixture.nativeText)
	}))
	t.Cleanup(server.Close)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Beeper.URL = server.URL
	cfg.Beeper.RateLimitQPS = 1000
	cfg.Beeper.Drafts = []config.BeeperDraftSource{{SourceID: source.ID}}
	requirements.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-token"))
	fixture.adapter = &storeAPIAdapter{store: st, config: cfg, beeperDraftPolicy: snapshotBeeperDraftPolicy(cfg)}
	return fixture
}

func (f *beeperDraftCommandFixture) sourceID() string {
	return strconv.FormatInt(f.source.ID, 10)
}

func (f *beeperDraftCommandFixture) run(ctx context.Context, args ...string) error {
	return f.adapter.runCLIBeeperDraft(ctx, api.CLIRunRequest{Args: args}, nil)
}

func TestBeeperDraftReloadsAfterSourceLock(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger changes state during source-lock recovery")
	for _, tc := range []struct {
		name   string
		change string
	}{{"draft", "UPDATE beeper_drafts SET committed_text = 'rich updated', revision = revision + 1"}, {"source", "UPDATE sources SET source_type = 'gmail' WHERE id = NEW.source_id"}} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fixture := newBeeperDraftCommandFixture(t, "")
			requirements.NoError(fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "hello"))
			draftID, revision := latestBeeperDraft(t, fixture.store)
			_, err := fixture.store.DB().Exec(`CREATE TRIGGER beeper_draft_lock_change AFTER UPDATE OF status ON sync_runs WHEN NEW.status = 'failed' BEGIN ` + tc.change + `; END`)
			requirements.NoError(err)
			_, err = fixture.store.DB().Exec(`INSERT INTO sync_runs (source_id, started_at, status) VALUES (?, CURRENT_TIMESTAMP, 'running')`, fixture.source.ID)
			requirements.NoError(err)
			gets, patches := fixture.gets, fixture.patches
			var event api.CLIRunEvent
			err = fixture.adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{"draft-beeper", "get", draftID, "--json"}}, func(got api.CLIRunEvent) error {
				event = got
				return nil
			})
			assertions.Equal(patches, fixture.patches)
			if tc.name == "source" {
				requirements.ErrorContains(err, "draft_disabled")
				assertions.Equal(gets, fixture.gets)
				return
			}
			requirements.NoError(err)
			var output beeperDraftOutput
			requirements.NoError(json.Unmarshal([]byte(event.Data), &output))
			assertions.Equal(revision+1, output.Revision)
			requirements.NotNil(output.CommittedText)
			assertions.Equal("rich updated", *output.CommittedText)
			assertions.Equal(gets+1, fixture.gets)
		})
	}
}

func TestBeeperDraftMissingChatKeepsBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
	}{{"active", false}, {"pending", true}} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fixture := newBeeperDraftCommandFixture(t, "")
			fixture.disconnectSet = tc.pending
			err := fixture.run(t.Context(), "draft-beeper", "create", "--source-id", fixture.sourceID(), "--chat-id", "!room:beeper.local", "--body", "hello")
			if tc.pending {
				requirements.ErrorContains(err, "remote_unknown")
			} else {
				requirements.NoError(err)
			}
			draftID, revision := latestBeeperDraft(t, fixture.store)
			patches := fixture.patches
			fixture.getStatus = http.StatusNotFound
			requirements.ErrorContains(fixture.run(t.Context(), "draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(revision, 10)), "provider_unavailable")
			kept, err := fixture.store.GetBeeperDraftContext(t.Context(), draftID)
			requirements.NoError(err)
			assertions.Equal(revision, kept.Revision)
			assertions.Equal(patches, fixture.patches)
			if tc.pending {
				requirements.NotNil(kept.Pending)
				assertions.Equal("hello", kept.Pending.Candidate)
			} else {
				requirements.NotNil(kept.CommittedText)
				assertions.Equal("rich hello", *kept.CommittedText)
			}
		})
	}
}

func TestBeeperDraftCreateEditClearUsesNativeWireAndLocalBinding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)

	nativeText := ""
	patchBodies := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, readErr := io.ReadAll(r.Body)
			if !assertions.NoError(readErr) {
				http.Error(w, "read request", http.StatusBadRequest)
				return
			}
			patchBodies = append(patchBodies, string(body))
			var update struct {
				Draft *struct {
					Text string `json:"text"`
				} `json:"draft"`
			}
			if !assertions.NoError(json.Unmarshal(body, &update)) {
				http.Error(w, "decode request", http.StatusBadRequest)
				return
			}
			if update.Draft == nil {
				nativeText = ""
			} else {
				nativeText = "rich " + update.Draft.Text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if nativeText == "" {
			_, _ = w.Write([]byte(`{"id":"!room:beeper.local","accountID":"signal","draft":null}`))
			return
		}
		data, marshalErr := json.Marshal(struct {
			ID        string `json:"id"`
			AccountID string `json:"accountID"`
			Draft     struct {
				Text string `json:"text"`
			} `json:"draft"`
		}{ID: "!room:beeper.local", AccountID: "signal", Draft: struct {
			Text string `json:"text"`
		}{Text: nativeText}})
		assertions.NoError(marshalErr)
		_, _ = w.Write(data)
	}))
	defer server.Close()

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Beeper.URL = server.URL
	cfg.Beeper.Drafts = []config.BeeperDraftSource{{SourceID: source.ID}}
	requirements.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-token"))
	adapter := &storeAPIAdapter{store: st, config: cfg, beeperDraftPolicy: snapshotBeeperDraftPolicy(cfg)}
	run := func(args ...string) error {
		return adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: args}, nil)
	}
	requirements.NoError(run("draft-beeper", "create", "--source-id", strconv.FormatInt(source.ID, 10), "--chat-id", "!room:beeper.local", "--body", "hello"))
	var draftID string
	var revision int64
	requirements.NoError(st.DB().QueryRow(`SELECT draft_id, revision FROM beeper_drafts`).Scan(&draftID, &revision))
	assertions.Equal(int64(2), revision)
	requirements.NoError(run("draft-beeper", "edit", draftID, "--revision", "2", "--body", "updated"))
	edited, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	nativeText = ""
	patchCount := len(patchBodies)
	requirements.NoError(run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(edited.Revision, 10)))
	cleared, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Nil(cleared.CommittedText)
	assertions.Len(patchBodies, patchCount)
	// A Desktop clear is observed locally and needs no second provider clear.
	requirements.NoError(run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(cleared.Revision, 10)))
	assertions.Len(patchBodies, patchCount)
	assertions.Contains(patchBodies[0], `"draft":{"text":"hello"}`)
}

func TestBeeperDraftCreateGet(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)
	nativeText := ""
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
			body, readErr := io.ReadAll(r.Body)
			assertions.NoError(readErr)
			var update struct {
				Draft struct {
					Text string `json:"text"`
				} `json:"draft"`
			}
			assertions.NoError(json.Unmarshal(body, &update))
			nativeText = "rich " + update.Draft.Text
		}
		writeBeeperTestChat(w, nativeText)
	}))
	defer server.Close()
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Beeper.URL = server.URL
	cfg.Beeper.Drafts = []config.BeeperDraftSource{{SourceID: source.ID}}
	requirements.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-token"))
	adapter := &storeAPIAdapter{store: st, config: cfg, beeperDraftPolicy: snapshotBeeperDraftPolicy(cfg)}
	sourceID := strconv.FormatInt(source.ID, 10)
	requirements.NoError(adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "hello",
	}}, nil))
	draftID, revision := latestBeeperDraft(t, st)
	var event api.CLIRunEvent
	requirements.NoError(adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "get", draftID, "--json",
	}}, func(got api.CLIRunEvent) error {
		event = got
		return nil
	}))
	assertions.Equal(cliStreamStdout, event.Type)
	var output beeperDraftOutput
	requirements.NoError(json.Unmarshal([]byte(event.Data), &output))
	assertions.Equal(draftID, output.DraftID)
	assertions.Equal(revision, output.Revision)
	requirements.NotNil(output.CommittedText)
	assertions.Equal("rich hello", *output.CommittedText)
	assertions.Equal("rich hello", output.NativeText)
	assertions.Equal(1, patches)

	nativeText = "desktop edit"
	requirements.NoError(adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "get", draftID, "--json",
	}}, func(got api.CLIRunEvent) error {
		event = got
		return nil
	}))
	requirements.NoError(json.Unmarshal([]byte(event.Data), &output))
	assertions.Equal("rich hello", *output.CommittedText)
	assertions.Equal("desktop edit", output.NativeText)
	assertions.Equal(1, patches)
	requirements.NoError(adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "get", draftID,
	}}, func(got api.CLIRunEvent) error {
		event = got
		return nil
	}))
	assertions.Equal(cliStreamStdout, event.Type)
	assertions.Contains(event.Data, "content:\nrich hello")
	assertions.Contains(event.Data, "native:\ndesktop edit")
	stored, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Equal("rich hello", *stored.CommittedText)
}

func TestBeeperDraftInterrupted(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)

	nativeText := ""
	patchMode := "accept"
	patchBodies := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeBeeperTestChat(w, nativeText)
			return
		}
		body, readErr := io.ReadAll(r.Body)
		assertions.NoError(readErr)
		patchBodies = append(patchBodies, string(body))
		var update struct {
			Draft *struct {
				Text string `json:"text"`
			} `json:"draft"`
		}
		assertions.NoError(json.Unmarshal(body, &update))
		if update.Draft == nil {
			switch patchMode {
			case "reject-clear":
				w.WriteHeader(http.StatusBadRequest)
				return
			case "disconnect-clear":
				nativeText = ""
				disconnectBeeperTestResponse(w)
				return
			}
			nativeText = ""
		} else {
			switch patchMode {
			case "reject-set":
				w.WriteHeader(http.StatusBadRequest)
				return
			case "disconnect-set":
				nativeText = "rich " + update.Draft.Text
				disconnectBeeperTestResponse(w)
				return
			}
			nativeText = "rich " + update.Draft.Text
		}
		writeBeeperTestChat(w, nativeText)
	}))
	defer server.Close()

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Beeper.URL = server.URL
	cfg.Beeper.Drafts = []config.BeeperDraftSource{{SourceID: source.ID}}
	requirements.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-token"))
	adapter := &storeAPIAdapter{store: st, config: cfg, beeperDraftPolicy: snapshotBeeperDraftPolicy(cfg)}
	run := func(args ...string) error {
		return adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: args}, nil)
	}
	sourceID := strconv.FormatInt(source.ID, 10)
	requirements.NoError(run("draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "hello"))
	draftID, revision := latestBeeperDraft(t, st)
	assertions.Equal(int64(2), revision)

	patchMode = "reject-set"
	err = run("draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated")
	requirements.ErrorContains(err, "provider_rejected")
	pending, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	requirements.NotNil(pending.Pending)
	assertions.Equal(store.BeeperDraftPhaseClearConfirmed, pending.Pending.Phase)
	assertions.Equal("updated", pending.Pending.Candidate)
	requirements.NotNil(pending.CommittedText)
	assertions.Equal("rich hello", *pending.CommittedText)

	patchMode = "accept"
	revision = pending.Revision
	requirements.NoError(run("draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated"))
	finished, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	requirements.Nil(finished.Pending)
	assertions.Equal("rich updated", *finished.CommittedText)
	revision = finished.Revision

	patchMode = "reject-clear"
	err = run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(revision, 10))
	requirements.ErrorContains(err, "provider_rejected")
	retry, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Nil(retry.Pending)
	assertions.Equal("rich updated", *retry.CommittedText)

	patchMode = "accept"
	revision = retry.Revision
	requirements.NoError(run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(revision, 10)))
	cleared, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Nil(cleared.CommittedText)

	patchMode = "accept"
	requirements.NoError(run("draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "again"))
	draftID, revision = latestBeeperDraft(t, st)
	patchMode = "disconnect-clear"
	err = run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(revision, 10))
	requirements.ErrorContains(err, "remote_unknown")
	uncertain, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	requirements.NotNil(uncertain.Pending)
	assertions.Equal(store.BeeperDraftPhaseRemoteUnknown, uncertain.Pending.Phase)
	patchCount := len(patchBodies)
	revision = uncertain.Revision
	requirements.NoError(run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(revision, 10)))
	recovered, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Nil(recovered.Pending)
	assertions.Len(patchBodies, patchCount)
}

func TestBeeperDraftCancellationPersistsRejectedEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)

	nativeText := ""
	patches := 0
	var cancelEdit atomic.Bool
	editObserved := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeBeeperTestChat(w, nativeText)
			if cancelEdit.Swap(false) {
				close(editObserved)
			}
			return
		}
		patches++
		body, readErr := io.ReadAll(r.Body)
		assertions.NoError(readErr)
		var update struct {
			Draft *struct {
				Text string `json:"text"`
			} `json:"draft"`
		}
		assertions.NoError(json.Unmarshal(body, &update))
		if update.Draft == nil {
			nativeText = ""
		} else {
			nativeText = "rich " + update.Draft.Text
		}
		writeBeeperTestChat(w, nativeText)
	}))
	defer server.Close()

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Beeper.URL = server.URL
	cfg.Beeper.Drafts = []config.BeeperDraftSource{{SourceID: source.ID}}
	cfg.Beeper.RateLimitQPS = 1000
	requirements.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-token"))
	adapter := &storeAPIAdapter{store: st, config: cfg, beeperDraftPolicy: snapshotBeeperDraftPolicy(cfg)}
	run := func(ctx context.Context, args ...string) error {
		return adapter.runCLIBeeperDraft(ctx, api.CLIRunRequest{Args: args}, nil)
	}
	sourceID := strconv.FormatInt(source.ID, 10)
	requirements.NoError(run(t.Context(), "draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "hello"))
	draftID, revision := latestBeeperDraft(t, st)
	cfg.Beeper.RateLimitQPS = 0.1
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelEdit.Store(true)
	go func() {
		<-editObserved
		time.AfterFunc(500*time.Millisecond, cancel)
	}()
	err = run(ctx, "draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated")
	requirements.ErrorContains(err, beeper.DraftWriteCodeRateLimit)
	assertions.Equal(1, patches)
	stored, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Nil(stored.Pending)
	requirements.NotNil(stored.CommittedText)
	assertions.Equal("rich hello", *stored.CommittedText)
}

func TestBeeperDraftAcceptedRemoteLocalFinishFailure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	testutil.SkipIfPostgres(t, "finish failure injection uses a SQLite trigger")
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)

	nativeText := ""
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeBeeperTestChat(w, nativeText)
			return
		}
		patches++
		body, readErr := io.ReadAll(r.Body)
		assertions.NoError(readErr)
		var update struct {
			Draft *struct {
				Text string `json:"text"`
			} `json:"draft"`
		}
		assertions.NoError(json.Unmarshal(body, &update))
		if update.Draft == nil {
			nativeText = ""
		} else {
			nativeText = "rich " + update.Draft.Text
		}
		writeBeeperTestChat(w, nativeText)
	}))
	defer server.Close()

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Beeper.URL = server.URL
	cfg.Beeper.Drafts = []config.BeeperDraftSource{{SourceID: source.ID}}
	requirements.NoError(beeper.SaveToken(cfg.TokensDir(), "synthetic-token"))
	adapter := &storeAPIAdapter{store: st, config: cfg, beeperDraftPolicy: snapshotBeeperDraftPolicy(cfg)}
	run := func(args ...string) error {
		return adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: args}, nil)
	}
	sourceID := strconv.FormatInt(source.ID, 10)
	requirements.NoError(run("draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "hello"))
	draftID, revision := latestBeeperDraft(t, st)
	assertions.Equal(int64(2), revision)

	_, err = st.DB().Exec(`
CREATE TRIGGER beeper_drafts_finish_failure
BEFORE UPDATE OF committed_text ON beeper_drafts
BEGIN
  SELECT RAISE(FAIL, 'injected finish failure');
END`)
	requirements.NoError(err)
	var failureEvent api.CLIRunEvent
	err = adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated",
	}}, func(got api.CLIRunEvent) error {
		failureEvent = got
		return nil
	})
	requirements.ErrorContains(err, "remote_accepted_local_failed")
	assertions.Equal(cliStreamStderr, failureEvent.Type)
	assertions.Contains(failureEvent.Data, draftID)
	assertions.Contains(failureEvent.Data, "revision "+strconv.FormatInt(revision+1, 10))
	assertions.Contains(failureEvent.Data, "pending phase: accepted_local_failed")
	assertions.Contains(failureEvent.Data, "candidate:\nupdated")
	assertions.Equal(3, patches)
	assertions.Equal("rich updated", nativeText)

	pending, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	requirements.NotNil(pending.Pending)
	assertions.Equal(draftID, pending.DraftID)
	assertions.Equal(revision+1, pending.Revision)
	assertions.Equal(store.BeeperDraftOperationEdit, pending.Pending.Operation)
	assertions.Equal(store.BeeperDraftPhaseAcceptedLocalFailed, pending.Pending.Phase)
	assertions.Equal("updated", pending.Pending.Candidate)
	requirements.NotNil(pending.CommittedText)
	assertions.Equal("rich hello", *pending.CommittedText)
	var pendingGetEvent api.CLIRunEvent
	requirements.NoError(adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "get", draftID,
	}}, func(got api.CLIRunEvent) error {
		pendingGetEvent = got
		return nil
	}))
	assertions.Equal(cliStreamStdout, pendingGetEvent.Type)
	assertions.Contains(pendingGetEvent.Data, "content:\nrich hello")
	assertions.Contains(pendingGetEvent.Data, "pending phase: accepted_local_failed")
	assertions.Contains(pendingGetEvent.Data, "candidate:\nupdated")
	assertions.Contains(pendingGetEvent.Data, "native:\nrich updated")
	var event api.CLIRunEvent
	createOnly := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "signal"}}}
	err = adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "again", "--json",
	}, Grant: createOnly}, func(got api.CLIRunEvent) error {
		event = got
		return nil
	})
	requirements.ErrorContains(err, "pending")
	assertions.Equal(3, patches)
	var pendingOutput beeperDraftOutput
	requirements.NoError(json.Unmarshal([]byte(event.Data), &pendingOutput))
	assertions.Equal(draftID, pendingOutput.DraftID)
	assertions.Equal(pending.Revision, pendingOutput.Revision)
	assertions.Equal(store.BeeperDraftPhaseAcceptedLocalFailed, pendingOutput.PendingPhase)
	assertions.True(pendingOutput.ContentWithheld)
	assertions.Nil(pendingOutput.CommittedText)
	assertions.Empty(pendingOutput.CandidateText)
	assertions.Empty(pendingOutput.NativeText)
	assertions.NotContains(event.Data, "rich hello")
	assertions.NotContains(event.Data, "updated")

	err = adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "again",
	}, Grant: createOnly}, func(got api.CLIRunEvent) error {
		event = got
		return nil
	})
	requirements.ErrorContains(err, "pending")
	assertions.Contains(event.Data, "content: <withheld>")
	assertions.Contains(event.Data, "native: <withheld>")
	assertions.Contains(event.Data, draftID)
	assertions.NotContains(event.Data, "rich hello")
	assertions.NotContains(event.Data, "updated")

	createRead := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftRead}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "signal"}}}
	err = adapter.runCLIBeeperDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-beeper", "create", "--source-id", sourceID, "--chat-id", "!room:beeper.local", "--body", "again", "--json",
	}, Grant: createRead}, func(got api.CLIRunEvent) error {
		event = got
		return nil
	})
	requirements.ErrorContains(err, "pending")
	requirements.NoError(json.Unmarshal([]byte(event.Data), &pendingOutput))
	assertions.False(pendingOutput.ContentWithheld)
	assertions.Equal("rich hello", *pendingOutput.CommittedText)
	assertions.Equal("updated", pendingOutput.CandidateText)

	err = run("draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(pending.Revision, 10), "--body", "updated")
	requirements.ErrorContains(err, "pending")
	assertions.Equal(3, patches)
}

func TestBeeperDraftConcurrentChange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	nativeText := "old"
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeBeeperTestChat(w, nativeText)
			nativeText = "desktop edit"
			return
		}
		patches++
		nativeText = ""
		writeBeeperTestChat(w, nativeText)
	}))
	defer server.Close()
	c := beeper.NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
	chat, err := c.GetChat(t.Context(), "!room:beeper.local")
	requirements.NoError(err)
	old, err := chat.InspectDraft()
	requirements.NoError(err)
	assertions.Equal("old", old.Text)
	cleared, err := c.UpdateDraft(t.Context(), "!room:beeper.local", nil)
	requirements.NoError(err)
	assertions.Equal(1, patches)
	clearedObservation, err := cleared.InspectDraft()
	requirements.NoError(err)
	assertions.True(clearedObservation.Empty())
	assertions.Empty(nativeText)
}

func latestBeeperDraft(t *testing.T, st *store.Store) (string, int64) {
	t.Helper()
	var draftID string
	var revision int64
	require.NoError(t, st.DB().QueryRow("SELECT draft_id, revision FROM beeper_drafts").Scan(&draftID, &revision))
	return draftID, revision
}

func writeBeeperTestChat(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	if text == "" {
		_, _ = w.Write([]byte(`{"id":"!room:beeper.local","accountID":"signal","draft":null}`))
		return
	}
	_, _ = w.Write([]byte(`{"id":"!room:beeper.local","accountID":"signal","draft":{"text":"` + text + `"}}`))
}

func disconnectBeeperTestResponse(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	connection, _, err := hijacker.Hijack()
	if err == nil {
		_ = connection.Close()
	}
}
