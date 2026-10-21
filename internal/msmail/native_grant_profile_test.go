package msmail

import (
	"context"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// Only provider HTTP is synthetic. Native Graph transport, MIME parser, importer,
// permission writer, and archive persistence all run unchanged.
func TestNativeMicrosoftGrantRequiresCurrentProfile(t *testing.T) {
	for _, tc := range []struct {
		name                string
		email               string
		scopes              []string
		profile             string
		proxyAddresses      []string
		status              int
		withoutEvents       bool
		wantRead, wantError bool
	}{
		{name: "known read", scopes: []string{"https://graph.microsoft.com/Mail.Read"}, profile: "me@example.com", wantRead: true},
		{name: "known read write", scopes: []string{"https://graph.microsoft.com/Mail.ReadWrite"}, profile: "me@example.com", wantRead: true},
		{name: "bare delegated scope", scopes: []string{"Mail.Read"}, profile: "me@example.com", wantRead: true},
		{name: "SMTP alias", email: "alias@example.com", scopes: []string{"https://graph.microsoft.com/Mail.Read"}, profile: "primary@example.com", proxyAddresses: []string{"SMTP:primary@example.com", "smtp:alias@example.com"}, wantRead: true},
		{name: "SMTP alias without Events", email: "alias@example.com", scopes: []string{"https://graph.microsoft.com/Mail.Read"}, profile: "primary@example.com", proxyAddresses: []string{"SMTP:primary@example.com", "smtp:alias@example.com"}, withoutEvents: true, wantRead: true},
		{name: "legacy unknown"},
		{name: "metadata only", scopes: []string{"https://graph.microsoft.com/Mail.ReadBasic"}},
		{name: "foreign profile", scopes: []string{"https://graph.microsoft.com/Mail.Read"}, profile: "other@example.net", wantError: true},
		{name: "provider denial", scopes: []string{"https://graph.microsoft.com/Mail.Read"}, status: http.StatusForbidden, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			if !tc.withoutEvents {
				enableNativeMailEvents(t, st)
			}
			graph := newFakeGraph(t)
			var profiles atomic.Int32
			graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/me" {
					profiles.Add(1)
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						return
					}
					profile := map[string]any{"id": "synthetic-account-id", "mail": tc.profile, "userPrincipalName": tc.profile}
					if strings.Contains(r.URL.Query().Get("$select"), "proxyAddresses") {
						profile["proxyAddresses"] = tc.proxyAddresses
					}
					graph.writeJSON(w, profile)
					return
				}
				graph.serve(w, r)
			})
			c := NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) { return tc.scopes, nil }))
			email := tc.email
			if email == "" {
				email = "me@example.com"
			}
			sum, err := Import(t.Context(), st, c, Options{Email: email}, slog.Default())
			if tc.wantError {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			source, err := st.GetOrCreateSource(SourceType, email)
			require.NoError(err)
			require.NotNil(source)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.Equal(tc.wantRead, allowed)
			if tc.wantRead {
				require.NotNil(sum)
				graph.put("synthetic-live", "inbox")
				_, err = Import(t.Context(), st, c, Options{Email: email}, slog.Default())
				require.NoError(err)
				if tc.withoutEvents {
					assert.Zero(nativeMailEventCount(t, st), "archive sync remains independent of Events configuration")
				} else {
					assert.Equal(1, nativeMailEventCount(t, st))
				}
			}
			if len(tc.scopes) == 0 || tc.name == "metadata only" {
				assert.Zero(profiles.Load(), "unknown or unreadable grant must not imply profile authority")
			} else {
				assert.Positive(profiles.Load())
			}
		})
	}
}
