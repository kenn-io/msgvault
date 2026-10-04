package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/bland"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBlandSourceSelectionAndCacheRefresh(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	cfg := &config.Config{Bland: []config.BlandSource{{Identifier: "one"}, {Identifier: "two"}}}
	sources, err := resolveBlandSources(nil, false, cfg)
	requirements.NoError(err)
	assertions.Len(sources, 2)
	_, err = resolveBlandSources(nil, true, cfg)
	requirements.Error(err)
	source, err := resolveBlandSource([]string{"TWO"}, cfg)
	requirements.NoError(err)
	assertions.Equal("two", source.Identifier)
	refreshed := 0
	err = finishBlandImport("one", &bland.ImportSummary{MeetingsAdded: 1}, errors.New("retrieval failed"), func() error { refreshed++; return nil })
	requirements.Error(err)
	assertions.Equal(1, refreshed)
}
func TestBlandProbeDoesNotPrintContent(t *testing.T) {
	assertions := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/calls":
			_, _ = w.Write([]byte(`{"count":1,"calls":[{"call_id":"private-call-id"}]}`))
		case "/v1/calls/private-call-id":
			_, _ = w.Write([]byte(`{"call_id":"private-call-id","summary":"private-call-text"}`))
		case "/v1/postcall/webhooks/private-call-id":
			w.WriteHeader(http.StatusNotFound)
		default:
			assertions.Fail("unexpected route", r.URL.Path)
		}
	}))
	defer srv.Close()
	var out bytes.Buffer
	err := runBlandProbe(t.Context(), &out, bland.NewClient(srv.URL+"/v1", "synthetic-key"))
	require.NoError(t, err)
	assertions.Contains(out.String(), "available")
	assertions.NotContains(out.String(), "private-call-id")
	assertions.NotContains(out.String(), "private-call-text")
	assertions.NotContains(out.String(), "synthetic-key")
}
func TestScheduledBlandRequiresRegisteredSource(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	cfg := testConfigValue()
	ctx := testInvocationContext(context.Background(), cfg, invocationOptions{})
	err := runConfiguredBlandSync(ctx, st, config.BlandSource{Identifier: "removed", APIKey: "synthetic-key", AccountEmail: "owner@example.com"})
	requirements.Error(err)
	assertions.Contains(err.Error(), "add-bland removed")
	sources, err := st.ListSources(bland.SourceType)
	requirements.NoError(err)
	assertions.Empty(sources)
}

func TestManualBlandSyncRequiresRegisteredSource(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	cfg.Bland = []config.BlandSource{{Identifier: "removed", APIKey: "synthetic-key", AccountEmail: "owner@example.com"}}

	previousProbe, previousFull := syncBlandProbe, syncBlandFull
	previousAfter, previousLimit := syncBlandAfter, syncBlandLimit
	t.Cleanup(func() {
		syncBlandProbe, syncBlandFull = previousProbe, previousFull
		syncBlandAfter, syncBlandLimit = previousAfter, previousLimit
	})
	syncBlandProbe, syncBlandFull = false, false
	syncBlandAfter, syncBlandLimit = "", 0

	cmd := &cobra.Command{Use: syncBlandCmd.Use}
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err := syncBlandCmd.RunE(cmd, []string{"removed"})
	require.ErrorContains(t, err, `bland call source "removed" is not registered`)
	require.ErrorContains(t, err, "run msgvault add-bland removed first")
}

func TestManualBlandSyncRejectsNegativeLimitAsUsageError(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Bland = []config.BlandSource{{Identifier: "work", APIKey: "synthetic-key", AccountEmail: "owner@example.com"}}
	previousLimit := syncBlandLimit
	t.Cleanup(func() { syncBlandLimit = previousLimit })
	syncBlandLimit = -1

	cmd := &cobra.Command{Use: syncBlandCmd.Use, SilenceUsage: true}
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err := syncBlandCmd.RunE(cmd, []string{"work"})
	require.ErrorContains(t, err, "--limit must be zero or positive")
	assert.False(t, cmd.SilenceUsage, "invalid limit must show command usage")
}
