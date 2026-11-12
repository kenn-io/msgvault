package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestListAccountsUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	cfg := testConfigValue()
	useLocal := false

	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, accountRequests := accountsHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedJSON := listAccountsJSON
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		listAccountsJSON = savedJSON
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	_ = testCtx
	useLocal = true
	invocationFromContext(testCtx).options.useLocal = true
	listAccountsJSON = false

	done := captureStdout(t)
	cmd := &cobra.Command{Use: "list-accounts", RunE: listAccountsCmd.RunE}
	cmd.SetContext(testCtx)

	err := cmd.Execute()
	out := done()
	require.NoError(err, "list-accounts")

	assert.Equal(1, int(accountRequests.Load()), "accounts endpoint calls")
	assert.Contains(out, "ID  ACCOUNT", "table header")
	assert.Regexp(`ALIAS\s+STATE`, out, "source lifecycle columns")
	assert.Regexp(`(?m)^7\s+alice@example\.com\s+gmail\s+Alice\s+1,234\s+2024-01-02 03:04\s+active$`, out,
		"Gmail account and source type")
	assert.Regexp(`(?m)^8\s+imaps://bob@imap\.example\.com:993\s+imap\s+Bob\s+42\s+-\s+active$`, out,
		"IMAP account and source type")
	assert.Contains(out, "Alice", "display name")
	assert.Contains(out, "1,234", "message count formatting")
	assert.Contains(out, "2024-01-02 03:04", "last sync formatting")
}

func accountsHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"accounts": [{
				"id": 7,
				"email": "alice@example.com",
				"type": "gmail",
				"display_name": "Alice",
				"message_count": 1234,
				"last_sync": "2024-01-02T03:04:05Z"
			}, {
				"id": 8,
				"email": "imaps://bob@imap.example.com:993",
				"type": "imap",
				"display_name": "Bob",
				"message_count": 42,
				"last_sync": null
			}]
		}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func TestListAccountsPendingCountsRemainExplicit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	stats := daemonAccountsToStats([]daemonclient.CLIAccount{{ID: 7, Email: "counts@example.test", Type: "gmail"}}, true)
	require.Len(stats, 1)
	assert.Equal("pending", formatMessagesCell(stats[0]))
	done := captureStdout(t)
	require.NoError(outputAccountsJSON(stats))
	output := done()
	assert.Contains(output, `"counts_pending": true`)
	assert.NotContains(output, "message_count", "unknown counts are omitted rather than zero")
	assert.Equal("0", formatMessagesCell(accountStats{}), "known empty accounts still show zero")
}

func TestListAccountsStaleCountAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		asOf := time.Date(1999, time.December, 31, 18, 56, 59, 123000000, time.FixedZone("test", -5*60*60))
		account := daemonclient.CLIAccount{ID: 7, Email: "counts@example.test", Type: "gmail", MessageCount: 1234, SourceDeletedCount: 2, Stale: true, AsOf: &asOf}
		stats := daemonAccountsToStats([]daemonclient.CLIAccount{account}, false)
		done := captureStdout(t)
		outputAccountsTable(stats)
		assert.Contains(done(), "1,234 (+2 deleted from source) (counted 3 minutes ago)")
		done = captureStdout(t)
		require.NoError(outputAccountsJSON(stats))
		var rows []map[string]any
		require.NoError(json.Unmarshal([]byte(done()), &rows))
		require.Len(rows, 1)
		assert.InDelta(1234, rows[0]["message_count"], 0)
		assert.Equal(true, rows[0]["stale"])
		assert.Equal("1999-12-31T18:56:59.123-05:00", rows[0]["as_of"])

		account.Stale = false
		stats = daemonAccountsToStats([]daemonclient.CLIAccount{account}, false)
		done = captureStdout(t)
		outputAccountsTable(stats)
		freshTable := done()
		assert.Contains(freshTable, "1,234 (+2 deleted from source)")
		assert.NotContains(freshTable, "counted")
		done = captureStdout(t)
		require.NoError(outputAccountsJSON(stats))
		var freshRows []map[string]any
		require.NoError(json.Unmarshal([]byte(done()), &freshRows))
		require.Len(freshRows, 1)
		assert.NotContains(freshRows[0], "stale")
		assert.NotContains(freshRows[0], "as_of")

		futureAsOf := time.Date(2000, time.January, 1, 0, 2, 0, 0, time.UTC)
		done = captureStdout(t)
		outputAccountsTable([]accountStats{{MessageCount: 5, Stale: true, AsOf: &futureAsOf}})
		assert.Contains(done(), "5 (counted now)", "a remote clock ahead of the CLI cannot count messages in the future")
	})
}
