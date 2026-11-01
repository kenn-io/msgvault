package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
)

func TestStatsJSONFlagRegistered(t *testing.T) {
	flag := statsCmd.Flags().Lookup("json")
	require.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)
}

func TestStatsJSON(t *testing.T) {
	for _, tc := range []struct {
		name, mode, account, collection, label, kind, wantLabel string
		active, deleted, total, sources, scopeSources, size     int64
	}{
		{name: "local archive", mode: "local", kind: "archive", active: 8, deleted: 3, total: 11, sources: 4, size: 2097152},
		{name: "remote archive", mode: "remote", kind: "archive", active: 8, deleted: 3, total: 11, sources: 4},
		{name: "account with calendar", mode: "local", account: "reader@example.test", label: "Resolved account", kind: "account", wantLabel: "Resolved account", active: 8, deleted: 3, total: 11, sources: 2, scopeSources: 2},
		{name: "collection", mode: "remote", collection: "team", label: "Resolved team", kind: "collection", wantLabel: "Resolved team", active: 8, deleted: 3, total: 11, sources: 2, scopeSources: 2, size: 1048576},
		{name: "account label fallback", mode: "remote", account: "reader@example.test", kind: "account", wantLabel: "reader@example.test", sources: 2, scopeSources: 2},
		{name: "collection label fallback", mode: "local", collection: "team", kind: "collection", wantLabel: "team", sources: 2, scopeSources: 2},
		{name: "empty archive", mode: "local", kind: "archive"},
		{name: "deleted only", mode: "remote", kind: "archive", deleted: 3, total: 3, sources: 1},
		{name: "grant", mode: "delegated", label: "agent grant", kind: "agent_grant", wantLabel: "agent grant", active: 8, deleted: 3, total: 11, sources: 2, scopeSources: 2},
		{name: "empty grant", mode: "delegated", label: "agent grant", kind: "agent_grant", wantLabel: "agent grant"},
		{name: "delegated account", mode: "delegated", account: "reader@example.test", label: "Resolved account", kind: "account", wantLabel: "Resolved account", sources: 2, scopeSources: 2},
		{name: "delegated collection", mode: "delegated", collection: "team", label: "team", kind: "collection", wantLabel: "team", deleted: 3, total: 3, sources: 2, scopeSources: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			// HTTP total_messages remains active-only; the CLI must add the
			// separate source-deleted count. Empty fixtures include no live rows.
			var threads, attachments, labels int64
			if tc.active > 0 {
				threads, attachments, labels = 6, 3, 9
			}
			sizeField := fmt.Sprintf(`,"database_size_bytes":%d`, tc.size)
			if tc.mode == "delegated" {
				sizeField = ""
			}
			body := fmt.Sprintf(`{"stats":{"total_messages":%d,"source_deleted_messages":%d,"total_threads":%d,"total_attachments":%d,"total_labels":%d,"total_accounts":%d%s},"scope_label":%q,"scope_source_count":%d}`,
				tc.active, tc.deleted, threads, attachments, labels, tc.sources, sizeField, tc.label, tc.scopeSources)
			cmd, output, requests := statsJSONFixture(t, tc.mode, body, tc.account, tc.collection)
			cmd.SetArgs([]string{"--json"})
			require.NoError(cmd.Execute())
			assert.Equal(int32(1), requests.Load())

			decoder := json.NewDecoder(output)
			decoder.UseNumber()
			var got map[string]any
			require.NoError(decoder.Decode(&got))
			var trailing any
			require.ErrorIs(decoder.Decode(&trailing), io.EOF, "exactly one JSON object")
			want := map[string]any{
				"total_messages": json.Number(strconv.FormatInt(tc.total, 10)), "active_messages": json.Number(strconv.FormatInt(tc.active, 10)),
				"source_deleted_messages": json.Number(strconv.FormatInt(tc.deleted, 10)), "total_accounts": json.Number(strconv.FormatInt(tc.sources, 10)),
				"total_threads": json.Number(strconv.FormatInt(threads, 10)), "total_attachments": json.Number(strconv.FormatInt(attachments, 10)),
				"total_labels": json.Number(strconv.FormatInt(labels, 10)),
			}
			scope := map[string]any{"kind": tc.kind, "source_count": json.Number(strconv.FormatInt(tc.sources, 10))}
			if tc.wantLabel != "" {
				scope["label"] = tc.wantLabel
			}
			want["scope"] = scope
			if tc.mode != "delegated" {
				want["database_size_bytes"] = json.Number(strconv.FormatInt(tc.size, 10))
				want["database_size_scope"] = "global"
			}
			assert.Equal(want, got, "complete JSON contract, including zero fields and no endpoint metadata")
		})
	}
}

func TestStatsJSONCommandLocalFlagAndTextFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// A template flag set by another invocation must not control this command.
	flag := statsCmd.Flags().Lookup("json")
	if flag != nil {
		previous := flag.Value.String()
		require.NoError(flag.Value.Set("true"))
		t.Cleanup(func() { require.NoError(flag.Value.Set(previous)) })
	}
	for _, args := range [][]string{nil, {"--json=false"}} {
		cmd, output, _ := statsJSONFixture(t, "remote", `{"stats":{"total_messages":8,"source_deleted_messages":3,"total_threads":6,"total_attachments":3,"total_labels":9,"total_accounts":2,"database_size_bytes":1048576},"scope_source_count":2}`, "", "team")
		cmd.SetArgs(args)
		require.NoError(cmd.Execute())
		assert.Contains(output.String(), "Stats for collection \"team\" (2 accounts):\n")
		assert.Contains(output.String(), "\n  Messages:    11 (8 active, 3 deleted from source)\n")
	}
}

func TestStatsJSONWriterError(t *testing.T) {
	cmd, _, requests := statsJSONFixture(t, "remote", `{"stats":{}}`, "", "")
	cmd.SetArgs([]string{"--json"})
	cmd.SetOut(failingSearchWriter{})
	require.ErrorIs(t, cmd.Execute(), io.ErrClosedPipe)
	assert.Equal(t, int32(1), requests.Load())
}

// statsJSONFixture exercises the production HTTP client and response conversion
// without starting a daemon or opening an archive.
func statsJSONFixture(t *testing.T, mode, body, account, collection string) (*cobra.Command, *bytes.Buffer, *atomic.Int32) {
	t.Helper()
	previousAccount, previousCollection := statsAccount, statsCollection
	t.Cleanup(func() { statsAccount, statsCollection = previousAccount, previousCollection })
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, api.APISchemaVersion)
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-stats-grant", r.Header.Get(apiprotocol.AgentTokenHeader))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"authenticated":true,"auth_mode":"delegated"}`)
	})
	mux.HandleFunc("/api/v1/cli/stats", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, account, r.URL.Query().Get("account"))
		assert.Equal(t, collection, r.URL.Query().Get("collection"))
		if mode == "delegated" {
			assert.Equal(t, "synthetic-stats-grant", r.Header.Get(apiprotocol.AgentTokenHeader))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	dataDir := t.TempDir()
	cfg := &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}, Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}}
	options := invocationOptions{}
	if mode == "local" {
		writeStatsHTTPDaemonRuntime(t, dataDir, server)
		options.useLocal = true
	}
	if mode == "delegated" {
		tokenFile := filepath.Join(dataDir, "agent.token")
		require.NoError(t, os.WriteFile(tokenFile, []byte("synthetic-stats-grant"), 0o600))
		options = invocationOptions{agentURL: server.URL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true}
	}
	ctx := testInvocationContext(t.Context(), cfg, options)
	invocationFromContext(ctx).logger = slog.New(slog.DiscardHandler)
	cmd := &cobra.Command{Use: "stats", RunE: runStats, SilenceErrors: true, SilenceUsage: true}
	cmd.Flags().StringVar(&statsAccount, "account", account, "")
	cmd.Flags().StringVar(&statsCollection, "collection", collection, "")
	cmd.Flags().Bool("json", false, "Output as JSON")
	cmd.SetContext(ctx)
	output := &bytes.Buffer{}
	cmd.SetOut(output)
	cmd.SetErr(io.Discard)
	return cmd, output, requests
}
