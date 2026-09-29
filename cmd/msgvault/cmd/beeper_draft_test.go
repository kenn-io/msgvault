package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

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

	_, err = parseBeeperDraftArgs([]string{"draft-beeper", "clear", "beeper-draft-id", "--revision=0"})
	assertions.Error(err)
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
	assertions := assert.New(t)
	requirements := require.New(t)
	source := &store.Source{ID: 42, SourceType: "beeper", Identifier: "signal"}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "signal"}}}
	requirements.NoError(authorizeBeeperDraft([]config.BeeperDraftSource{{SourceID: 42}}, source, grant, agentgrant.PermissionDraftEdit))
	assertions.ErrorContains(authorizeBeeperDraft(nil, source, nil, agentgrant.PermissionDraftEdit), "draft_disabled")
	assertions.ErrorContains(authorizeBeeperDraft([]config.BeeperDraftSource{{SourceID: 41}}, source, grant, agentgrant.PermissionDraftEdit), "draft_disabled")
	wrong := &store.Source{ID: 42, SourceType: "gmail", Identifier: "signal"}
	assertions.ErrorContains(authorizeBeeperDraft([]config.BeeperDraftSource{{SourceID: 42}}, wrong, nil, agentgrant.PermissionDraftEdit), "draft_disabled")
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
	assertions.Error(err)
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
			requirements.NoError(readErr)
			patchBodies = append(patchBodies, string(body))
			var update struct {
				Draft *struct {
					Text string `json:"text"`
				} `json:"draft"`
			}
			requirements.NoError(json.Unmarshal(body, &update))
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
		requirements.NoError(marshalErr)
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
	drafts, err := st.DB().Query(`SELECT draft_id, revision FROM beeper_drafts`)
	requirements.NoError(err)
	requirements.True(drafts.Next())
	var draftID string
	var revision int64
	requirements.NoError(drafts.Scan(&draftID, &revision))
	requirements.NoError(drafts.Close())
	assertions.Equal(int64(2), revision)
	requirements.NoError(run("draft-beeper", "edit", draftID, "--revision", "2", "--body", "updated"))
	edited, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	requirements.NoError(run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(edited.Revision, 10)))
	cleared, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Nil(cleared.CommittedText)
	patchCount := len(patchBodies)
	// A Desktop clear is observed locally and needs no second provider clear.
	requirements.NoError(run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(cleared.Revision, 10)))
	assertions.Equal(patchCount, len(patchBodies))
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
			requirements.NoError(readErr)
			var update struct {
				Draft struct {
					Text string `json:"text"`
				} `json:"draft"`
			}
			requirements.NoError(json.Unmarshal(body, &update))
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
		requirements.NoError(readErr)
		patchBodies = append(patchBodies, string(body))
		var update struct {
			Draft *struct {
				Text string `json:"text"`
			} `json:"draft"`
		}
		requirements.NoError(json.Unmarshal(body, &update))
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
	assertions.ErrorContains(err, "provider_rejected")
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
	assertions.ErrorContains(err, "provider_rejected")
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
	assertions.ErrorContains(err, "remote_unknown")
	uncertain, err := st.GetBeeperDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	requirements.NotNil(uncertain.Pending)
	assertions.Equal(store.BeeperDraftPhaseRemoteUnknown, uncertain.Pending.Phase)
	patchCount := len(patchBodies)
	revision = uncertain.Revision
	err = run("draft-beeper", "clear", draftID, "--revision", strconv.FormatInt(revision, 10))
	assertions.ErrorContains(err, "pending")
	assertions.Equal(patchCount, len(patchBodies))
}

func TestBeeperDraftAcceptedRemoteLocalFinishFailure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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
		requirements.NoError(readErr)
		var update struct {
			Draft *struct {
				Text string `json:"text"`
			} `json:"draft"`
		}
		requirements.NoError(json.Unmarshal(body, &update))
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
	err = run("draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(revision, 10), "--body", "updated")
	assertions.ErrorContains(err, "remote_accepted_local_failed")
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

	err = run("draft-beeper", "edit", draftID, "--revision", strconv.FormatInt(pending.Revision, 10), "--body", "updated")
	assertions.ErrorContains(err, "pending")
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
	assertions.Equal("", nativeText)
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
