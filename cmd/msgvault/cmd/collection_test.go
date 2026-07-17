package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestCollectionListUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := collectionHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: "list", RunE: runCollectionList}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(err, "collection list")

	assert.Equal(1, int(requests.Load()), "collection endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Contains(stdout.String(), "NAME", "table header")
	assert.Contains(stdout.String(), "Team", "collection row")
	assert.Contains(stdout.String(), "2", "source count")
	assert.Contains(stdout.String(), "1,234", "message count")
}

func TestCollectionShowUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := collectionHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "show <name>",
		Args: collectionShowCmd.Args,
		RunE: runCollectionShow,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"Team"})

	err := cmd.Execute()
	require.NoError(err, "collection show")

	assert.Equal(1, int(requests.Load()), "collection endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Contains(stdout.String(), "Collection: Team")
	assert.Contains(stdout.String(), "Description: Team mail")
	assert.Contains(stdout.String(), "Sources: 2")
	assert.Contains(stdout.String(), "Messages: 1,234")
	assert.Contains(stdout.String(), "Created: 2024-01-02 03:04")
	assert.Contains(stdout.String(), "Personal (id 7)")
	assert.Contains(stdout.String(), "bob@example.com (id 8)")
}

func TestCollectionCreateUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := collectionHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedAccounts := collectionCreateAccounts
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		collectionCreateAccounts = savedAccounts
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	collectionCreateAccounts = "alice@example.com,bob@example.com"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "create <name>",
		Args: collectionCreateCmd.Args,
		RunE: runCollectionCreate,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"Team"})

	err := cmd.Execute()
	require.NoError(err, "collection create")

	assert.Equal(1, int(requests.Load()), "collection endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Equal("Created collection \"Team\" with 2 source(s).\n", stdout.String())
}

func TestCollectionAddUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := collectionHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedAccounts := collectionAddAccounts
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		collectionAddAccounts = savedAccounts
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	collectionAddAccounts = "alice@example.com,bob@example.com"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "add <name>",
		Args: collectionAddCmd.Args,
		RunE: runCollectionAdd,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"Team"})

	err := cmd.Execute()
	require.NoError(err, "collection add")

	assert.Equal(1, int(requests.Load()), "collection endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Equal("Added 2 source(s) to \"Team\".\n", stdout.String())
}

func TestCollectionRemoveUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := collectionHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedAccounts := collectionRemoveAccounts
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		collectionRemoveAccounts = savedAccounts
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	collectionRemoveAccounts = "alice@example.com,bob@example.com"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "remove <name>",
		Args: collectionRemoveCmd.Args,
		RunE: runCollectionRemove,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"Team"})

	err := cmd.Execute()
	require.NoError(err, "collection remove")

	assert.Equal(1, int(requests.Load()), "collection endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Equal("Removed 2 source(s) from \"Team\".\n", stdout.String())
}

func TestCollectionDeleteUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := collectionHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "delete <name>",
		Args: collectionDeleteCmd.Args,
		RunE: runCollectionDelete,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"Team"})

	err := cmd.Execute()
	require.NoError(err, "collection delete")

	assert.Equal(1, int(requests.Load()), "collection endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Equal("Deleted collection \"Team\".\n", stdout.String())
}

func collectionHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/collections", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"collections": [{
					"id": 10,
					"name": "Team",
					"description": "Team mail",
					"created_at": "2024-01-02T03:04:05Z",
					"source_ids": [7, 8],
					"message_count": 1234,
					"sources": [{
						"id": 7,
						"identifier": "alice@example.com",
						"display_name": "Personal"
					}, {
						"id": 8,
						"identifier": "bob@example.com"
					}]
				}]
			}`))
		case http.MethodPost:
			var req struct {
				Name     string   `json:"name"`
				Accounts []string `json:"accounts"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode collection create") {
				http.Error(w, "bad create request", http.StatusBadRequest)
				return
			}
			assert.Equal(t, "Team", req.Name, "create name")
			assert.Equal(t, []string{"alice@example.com", "bob@example.com"}, req.Accounts, "create accounts")
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"Team","source_count":2}`))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	})
	mux.HandleFunc("/api/v1/cli/collections/Team", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"Team"}`))
	})
	mux.HandleFunc("/api/v1/cli/collections/Team/sources", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch, http.MethodDelete:
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Accounts []string `json:"accounts"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode collection accounts") {
			http.Error(w, "bad accounts request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, []string{"alice@example.com", "bob@example.com"}, req.Accounts, "accounts")
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"Team","source_count":2}`))
	})
	mux.HandleFunc("/api/v1/cli/collection", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("name") != "Team" {
			http.Error(w, "missing collection name", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"collection": {
				"id": 10,
				"name": "Team",
				"description": "Team mail",
				"created_at": "2024-01-02T03:04:05Z",
				"source_ids": [7, 8],
				"message_count": 1234,
				"sources": [{
					"id": 7,
					"identifier": "alice@example.com",
					"display_name": "Personal"
				}, {
					"id": 8,
					"identifier": "bob@example.com"
				}]
			}
		}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func TestCollectionShowPrintsReadableSourceNames(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	}()

	tmpDir := t.TempDir()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")
	t.Cleanup(func() { _ = st.Close() })

	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create alice source")
	require.NoError(st.UpdateSourceDisplayName(alice.ID, "Personal"), "set display name")
	bob, err := st.GetOrCreateSource("imap", "bob@example.com")
	require.NoError(err, "create bob source")
	_, err = st.CreateCollection("team", "", []int64{alice.ID, bob.ID})
	require.NoError(err, "create collection")
	startStoreAPIDaemon(t, tmpDir, st, nil)

	done := captureStdout(t)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	require.NoError(runCollectionShow(cmd, []string{"team"}), "runCollectionShow")
	out := done()

	assert.Contains(out, "Personal (id ", "missing display name in output")
	assert.Contains(out, "bob@example.com (id ", "missing identifier in output")
}

func TestResolveAccountListRejectsMissingNumericID(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()
	st, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = st.Close() }()
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")

	ids, err := collectionops.ResolveAccountList(st, []string{strconv.FormatInt(src.ID, 10)})
	require.NoError(err, "resolveAccountList(existing id)")
	require.Equal([]int64{src.ID}, ids, "resolveAccountList(existing id)")

	// "999999" is neither an existing source ID nor an existing
	// identifier/display name, so resolveAccountList errors via the
	// final ResolveAccountFlag fall-through. Iter12 codex flagged that
	// the prior shape errored *before* the fall-through, so a numeric
	// identifier (e.g. unprefixed phone "15551234567") that wasn't a
	// source ID would never get a chance to match by identifier. The
	// test below asserts the fall-through path is reachable.
	_, err = collectionops.ResolveAccountList(st, []string{"999999"})
	require.Error(err, "expected error for missing numeric source ID")
}

// TestResolveAccountListNumericFallthroughResolvesIdentifier verifies
// that a plain-digit token that does NOT match a source ID falls
// through to identifier resolution. Regression test for iter12 codex
// Low: previously, a numeric identifier (e.g. an unprefixed phone
// number) that happened to not match a source ID would error
// immediately instead of being looked up by identifier.
func TestResolveAccountListNumericFallthroughResolvesIdentifier(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()
	st, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = st.Close() }()
	require.NoError(st.InitSchema(), "init schema")

	// Create a source with a numeric identifier that is unlikely to
	// collide with the auto-assigned source ID. Use a 12-digit string
	// (way past any plausible primary-key value) so the test stays
	// stable.
	phoneIdentifier := "987654321098"
	src, err := st.GetOrCreateSource("whatsapp", phoneIdentifier)
	require.NoError(err, "create source")
	require.NotEqual(phoneIdentifier, strconv.FormatInt(src.ID, 10),
		"test assumption broken: source id %d collides with identifier", src.ID)

	ids, err := collectionops.ResolveAccountList(st, []string{phoneIdentifier})
	require.NoError(err, "resolveAccountList(numeric identifier)")
	require.Equal([]int64{src.ID}, ids, "resolveAccountList(numeric identifier)")
}
