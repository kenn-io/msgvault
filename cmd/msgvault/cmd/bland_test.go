package cmd

import (
	"bytes"
	"context"
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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBlandSourceSelection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	cfg := &config.Config{Bland: []config.BlandSource{{Identifier: "one"}, {Identifier: "two"}}}
	_, err := resolveBlandSources(nil, true, cfg)
	requirements.Error(err)
	source, err := resolveBlandSource([]string{"TWO"}, cfg)
	requirements.NoError(err)
	assertions.Equal("two", source.Identifier)
}
func TestBlandProbeDoesNotPrintContent(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		detailStatus, postcallStatus int
		want                         string
	}{
		{"postcall missing", 0, http.StatusNotFound, "Retained postcall history unavailable"},
		{"postcall refused", 0, http.StatusForbidden, "Retained postcall history refused for sampled call (HTTP 403)"},
		{"details deleted", http.StatusNotFound, http.StatusNotFound, "Call details unavailable for sampled call"},
		{"details refused", http.StatusBadRequest, http.StatusNotFound, "Call details refused for sampled call (HTTP 400)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/calls":
					assertions.Equal("false", r.URL.Query().Get("ascending"), "the probe samples the newest call")
					_, _ = w.Write([]byte(`{"count":1,"calls":[{"call_id":"private-call-id"}]}`))
				case "/v1/calls/private-call-id":
					if tc.detailStatus != 0 {
						w.WriteHeader(tc.detailStatus)
						return
					}
					_, _ = w.Write([]byte(`{"call_id":"private-call-id","created_at":"2026-10-01T11:00:00Z","summary":"private-call-text"}`))
				case "/v1/postcall/webhooks/private-call-id":
					w.WriteHeader(tc.postcallStatus)
				default:
					assertions.Fail("unexpected route", r.URL.Path)
				}
			}))
			defer srv.Close()
			var out bytes.Buffer
			err := runBlandProbe(t.Context(), &out, bland.NewClient(srv.URL+"/v1", "synthetic-key"))
			require.NoError(t, err, "a missing or refused read doesn't fail registration")
			assertions.Contains(out.String(), tc.want)
			assertions.NotContains(out.String(), "private-call-id")
			assertions.NotContains(out.String(), "private-call-text")
			assertions.NotContains(out.String(), "synthetic-key")
		})
	}
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

// An unregistered source, or one missing its API key, fails alone with the
// fix it needs; the other sources still sync, and a limited one prints the
// command that resumes it.
func TestBlandSyncContinuesPastBrokenSources(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	cfg.Bland = []config.BlandSource{
		{Identifier: "removed", AccountEmail: "owner@example.com"},
		{Identifier: "nokey", AccountEmail: "owner@example.com"},
		{Identifier: "work", APIKey: "synthetic-key", AccountEmail: "owner@example.com"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/calls":
			_, _ = w.Write([]byte(`{"count":2,"total_count":2,"calls":[{"call_id":"call-1"},{"call_id":"call-2"}]}`))
		case "/v1/calls/call-1":
			_, _ = w.Write([]byte(`{"call_id":"call-1","completed":true,"status":"no-answer","created_at":"2026-10-01T11:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	previousClient, previousLimit := newBlandClient, syncBlandLimit
	t.Cleanup(func() { newBlandClient, syncBlandLimit = previousClient, previousLimit })
	newBlandClient = func(_, token string) *bland.Client { return bland.NewClient(srv.URL+"/v1", token) }
	syncBlandLimit = 1
	st, err := store.Open(cfg.DatabaseDSN())
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	for _, id := range []string{"nokey", "work"} {
		_, err = st.GetOrCreateSource(bland.SourceType, id)
		requirements.NoError(err)
	}
	requirements.NoError(st.Close())

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err = syncBlandCmd.RunE(cmd, nil)
	requirements.ErrorContains(err, `bland call source "removed" is not registered; run msgvault add-bland removed first`)
	requirements.ErrorContains(err, `[[bland]] entry "nokey" has no API key`)
	assertions.Contains(out.String(), "Syncing Bland calls for work")
	assertions.Contains(out.String(), "Run: msgvault sync-bland work --limit 1")
}
