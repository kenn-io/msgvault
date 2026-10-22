package msmail

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeMicrosoftStaleSyncDenialCannotRevokeNewRun(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	enableNativeMailEvents(t, st)
	graph := newFakeGraph(t)
	var client *Client
	var replace bool
	graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" {
			graph.writeJSON(w, map[string]any{"id": "synthetic-account", "mail": "me@example.com", "userPrincipalName": "me@example.com"})
			return
		}
		if replace && r.URL.Path == "/me/mailFolders" {
			replace = false
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
			if !Assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			active, err := st.GetActiveSyncReadOnly(t.Context(), source.ID)
			if !Assert.NoError(t, err) || !Assert.NotNil(t, active) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// Exercise the native terminal-run/recovery boundary while its provider
			// request is still outstanding. No SQL state or generation is fabricated.
			if !Assert.NoError(t, st.FailSync(active.ID, "synthetic replaced native run")) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, err = Import(t.Context(), st, client, Options{Email: "me@example.com"}, slog.Default())
			if !Assert.NoError(t, err, "new native sync must finish while the old provider request is outstanding") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
		graph.serve(w, r)
	})
	client = NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) { return []string{"Mail.Read"}, nil }))
	_, err := Import(t.Context(), st, client, Options{Email: "me@example.com"}, slog.Default())
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(err)
	require.NotNil(source)
	before, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	replace = true
	_, err = Import(t.Context(), st, client, Options{Email: "me@example.com"}, slog.Default())
	require.Error(err)
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.True(allowed, "a superseded sync cannot revoke the completed newer native observation")
	after, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.Equal(before.Reference, after.Reference)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count)
}
