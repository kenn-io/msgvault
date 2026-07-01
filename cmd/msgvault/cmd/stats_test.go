package cmd

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestStatsCommand_AccountAndCollectionMutuallyExclusive confirms that passing
// both --account and --collection to the stats command is rejected by cobra.
func TestStatsCommand_AccountAndCollectionMutuallyExclusive(t *testing.T) {
	var a, b string
	cmd := &cobra.Command{Use: "stats-test", SilenceErrors: true}
	sub := &cobra.Command{Use: "stats", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	sub.Flags().StringVar(&a, "account", "", "")
	sub.Flags().StringVar(&b, "collection", "", "")
	sub.MarkFlagsMutuallyExclusive("account", "collection")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"stats", "--account", "foo@example.com", "--collection", "bar"})

	err := cmd.Execute()
	require.Error(t, err, "expected error when both --account and --collection are set")
	msg := err.Error()
	assert.Contains(t, msg, "account", "error should mention account flag name")
	assert.Contains(t, msg, "collection", "error should mention collection flag name")
	_ = a
	_ = b
}

// TestStatsCommand_EmptyCollectionRejected verifies that
// `stats --collection <name>` errors out when the named collection
// has zero member sources, instead of silently falling through to
// archive-wide stats. Regression test for iter13 codex Medium:
// previously, an empty collection produced a non-IsEmpty Scope but
// SourceIDs() returned an empty slice, and GetStatsForScope treats
// an empty slice as unscoped/global.
func TestStatsCommand_EmptyCollectionRejected(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	_, err = st.CreateCollection("empty", "test", []int64{src.ID})
	require.NoError(err, "create collection")
	require.NoError(st.RemoveSourcesFromCollection("empty", []int64{src.ID}), "remove source from collection")
	startStoreAPIDaemon(t, dataDir, st, nil)

	savedCfg := cfg
	savedLogger := logger
	savedUseLocal := useLocal
	savedStatsAccount := statsAccount
	savedStatsCollection := statsCollection
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		useLocal = savedUseLocal
		statsAccount = savedStatsAccount
		statsCollection = savedStatsCollection
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	useLocal = true
	statsCollection = "empty"

	testCmd := &cobra.Command{Use: "stats", RunE: statsCmd.RunE}
	testCmd.Flags().StringVar(&statsAccount, "account", "", "")
	testCmd.Flags().StringVar(&statsCollection, "collection", "empty", "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"stats", "--collection", "empty"})

	err = root.Execute()
	require.Error(err, "expected error for empty collection")
	assert.Contains(t, err.Error(), "no member accounts")
}

func TestStatsCommand_ScopedUsesLocalDaemonHTTPAndPreservesLocalOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	testCfg := &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	server, statsRequests := statsHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedLogger := logger
	savedUseLocal := useLocal
	savedStatsAccount := statsAccount
	savedStatsCollection := statsCollection
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		useLocal = savedUseLocal
		statsAccount = savedStatsAccount
		statsCollection = savedStatsCollection
	}()

	cfg = testCfg
	logger = slog.New(slog.DiscardHandler)
	useLocal = true
	statsAccount = ""
	statsCollection = "Important"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: "stats", RunE: runStats}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(err, "stats command")

	assert.Equal(1, int(statsRequests.Load()), "stats endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Equal(`Stats for collection "Important" (2 accounts):
  Messages:    8
  Threads:     6
  Attachments: 3
  Labels:      9
  Accounts:    2
  Size:        2.00 MB

Note: Size is global (not scoped).
`, stdout.String())
}

func TestStatsCommand_UnscopedUsesLocalDaemonHTTPAndPreservesLocalOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	testCfg := &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	server, statsRequests := statsHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedLogger := logger
	savedUseLocal := useLocal
	savedStatsAccount := statsAccount
	savedStatsCollection := statsCollection
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		useLocal = savedUseLocal
		statsAccount = savedStatsAccount
		statsCollection = savedStatsCollection
	}()

	cfg = testCfg
	logger = slog.New(slog.DiscardHandler)
	useLocal = true
	statsAccount = ""
	statsCollection = ""

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: "stats", RunE: runStats}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(err, "stats command")

	assert.Equal(1, int(statsRequests.Load()), "stats endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Equal("Database: "+testCfg.DatabaseDSN()+`
  Messages:    3
  Threads:     2
  Attachments: 5
  Labels:      4
  Accounts:    1
  Size:        1.00 MB
`, stdout.String())
}

func statsHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	statsRequests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		statsRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"total_messages": 3,
			"total_threads": 2,
			"total_accounts": 1,
			"total_labels": 4,
			"total_attachments": 5,
			"database_size_bytes": 1048576
		}`))
	})
	mux.HandleFunc("/api/v1/cli/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("collection") != "Important" {
			http.Error(w, "missing collection", http.StatusBadRequest)
			return
		}
		statsRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"stats": {
				"total_messages": 8,
				"total_threads": 6,
				"total_accounts": 2,
				"total_labels": 9,
				"total_attachments": 3,
				"database_size_bytes": 2097152
			},
			"scope_label": "Important",
			"scope_source_count": 2
		}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, statsRequests
}

func writeStatsHTTPDaemonRuntime(t *testing.T, dataDir string, server *httptest.Server) {
	t.Helper()
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err, "split listener address")
	_, err = strconv.Atoi(portText)
	require.NoError(t, err, "parse listener port")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:       host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(t, err, "write daemon runtime")
}
