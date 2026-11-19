package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

func labelRefreshTestCommand(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{Use: syncFullCmd.Use, Args: syncFullCmd.Args, RunE: syncFullCmd.RunE}
	cmd.SetContext(ctx)
	cmd.Flags().Int64("source-id", 0, "")
	cmd.Flags().StringVar(&syncQuery, "query", "", "")
	cmd.Flags().StringVar(&syncBefore, "before", "", "")
	cmd.Flags().StringVar(&syncAfter, "after", "", "")
	cmd.Flags().IntVar(&syncLimit, "limit", 0, "")
	cmd.Flags().StringArrayVar(&syncFolders, "folder", nil, "")
	cmd.Flags().StringArrayVar(&syncSkipFolders, "skip-folder", nil, "")
	cmd.Flags().BoolVar(&syncNoResume, "noresume", false, "")
	cmd.Flags().BoolVar(&syncRefreshLabels, "refresh-labels", false, "")
	return cmd
}

func TestSyncFullLabelRefreshRejectsInvalidFlagsBeforeHTTP(t *testing.T) {
	for _, flags := range [][]string{
		{"--refresh-labels"},
		{"--refresh-labels=maybe", "reader@example.test"},
		{"--refresh-labels", "--query=", "reader@example.test"},
		{"--refresh-labels", "--before=", "reader@example.test"},
		{"--refresh-labels", "--after=2026-01-01", "reader@example.test"},
		{"--refresh-labels", "--folder=", "reader@example.test"},
		{"--refresh-labels", "--skip-folder=Trash", "reader@example.test"},
		{"--refresh-labels", "--limit=1", "reader@example.test"},
		{"--refresh-labels", "--source-id=42", "reader@example.test"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			resetSyncFullFlagsForTest(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			t.Cleanup(srv.Close)
			cmd := labelRefreshTestCommand(configureRemoteSyncTest(t, srv.URL))
			cmd.SetArgs(flags)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			require.Error(t, cmd.Execute())
			assert.Zero(t, calls.Load(), "invalid repair does no daemon or provider work")
		})
	}
}

func TestSyncFullLabelRefreshDaemonJourney(t *testing.T) {
	for _, mode := range []string{"account", "source-id", "ambiguous", "imap", "false", "false-source-id", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			resetSyncFullFlagsForTest(t)
			var syncCalls atomic.Int32
			accounts := []daemonclient.CLIAccount{{ID: 42, Type: "gmail", Email: "reader@example.test", Alias: "archive"}}
			if mode == "ambiguous" || mode == "source-id" || mode == "false-source-id" {
				accounts = append(accounts, daemonclient.CLIAccount{ID: 43, Type: "imap", Email: "reader@example.test"})
			}
			if mode == "imap" {
				accounts[0].Type = "imap"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": "5.20.0"}))
				case "/api/v1/cli/accounts":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"accounts": accounts}))
				case "/api/v1/cli/sync-full":
					syncCalls.Add(1)
					if mode == "false" || mode == "false-source-id" {
						assert.Empty(t, r.URL.Query().Get("refresh_labels"))
						assert.Equal(t, "in:inbox", r.URL.Query().Get("query"))
						if mode == "false-source-id" {
							assert.Equal(t, "43", r.URL.Query().Get("source_id"), "ordinary scalar selection retains its last value")
						}
					} else {
						assert.Equal(t, "true", r.URL.Query().Get("refresh_labels"))
						assert.Equal(t, "42", r.URL.Query().Get("source_id"))
						assert.Empty(t, r.URL.Query().Get("email"), "pin the validated source before execution")
					}
					w.Header().Set("Content-Type", "application/x-ndjson")
					if mode == "interrupted" {
						_, _ = w.Write([]byte("{\"type\":\"outcome\",\"data\":\"interrupted\"}\n{\"type\":\"error\",\"error\":\"interrupted\"}\n"))
					} else {
						_, _ = w.Write([]byte("{\"type\":\"complete\"}\n"))
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			cmd := labelRefreshTestCommand(configureRemoteSyncTest(t, srv.URL))
			args := []string{"--refresh-labels", "reader@example.test"}
			switch mode {
			case "account":
				args = []string{"--refresh-labels", "archive"}
			case "source-id":
				args = []string{"--refresh-labels", "--source-id=42"}
			case "false":
				args = []string{"--refresh-labels=false", "--query=in:inbox"}
			case "false-source-id":
				args = []string{"--refresh-labels=false", "--query=in:inbox", "--source-id=42", "--source-id=43"}
			case "interrupted":
				args = append(args, "--noresume")
			}
			var stderr bytes.Buffer
			cmd.SetArgs(args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&stderr)
			err := cmd.Execute()
			switch mode {
			case "ambiguous", "imap":
				require.ErrorContains(t, err, "exactly one configured Gmail source")
				assert.Zero(t, syncCalls.Load())
			case "interrupted":
				require.ErrorIs(t, err, ErrSyncInterrupted)
				assert.Contains(t, stderr.String(), "refreshes archived labels again")
				assert.Contains(t, stderr.String(), "--refresh-labels")
				assert.Contains(t, stderr.String(), "--noresume")
			default:
				require.NoError(t, err)
				assert.Equal(t, int32(1), syncCalls.Load())
			}
		})
	}
}

func TestCLISyncSubprocessCarriesLabelRefresh(t *testing.T) {
	t.Parallel()
	args := cliSyncSubprocessArgs(api.CLISyncRequest{Full: true, SourceID: 42, SourceIDSet: true, RefreshLabels: true})
	assert.Equal(t, []string{"sync-full", "--source-id", "42", "--refresh-labels"}, args)
}

func TestSyncFullLabelRefreshLocalRejectsOtherProviders(t *testing.T) {
	for _, provider := range []string{"imap", "msmail"} {
		for _, byID := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/source-id=%t", provider, byID), func(t *testing.T) {
				resetSyncFullFlagsForTest(t)
				cfg := lifecycleTestConfig(t.TempDir())
				st, err := store.OpenContext(t.Context(), cfg.DatabaseDSN())
				require.NoError(t, err)
				require.NoError(t, st.InitSchemaContext(t.Context()))
				source, err := st.GetOrCreateSourceContext(t.Context(), provider, "reader@example.test")
				require.NoError(t, err)
				require.NoError(t, st.Close())
				cmd := labelRefreshTestCommand(testInvocationContext(t.Context(), cfg, invocationOptions{useLocal: true}))
				args := []string{"reader@example.test"}
				flags := []string{"--refresh-labels"}
				if byID {
					args = nil
					flags = append(flags, fmt.Sprintf("--source-id=%d", source.ID))
				}
				require.NoError(t, cmd.ParseFlags(flags))
				require.ErrorContains(t, runSyncFullLocal(cmd, args), "exactly one configured Gmail source")
			})
		}
	}
}
