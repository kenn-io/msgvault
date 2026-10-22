package msmail

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// A successful MIME/lookup response can use an access token refreshed after
// the run's first grant observation. Reduced native authority must be applied
// before that message's Events publication, even if ordinary archiving works.
func TestNativeMicrosoftCaptureRechecksCurrentGrant(t *testing.T) {
	for _, tc := range []struct {
		name          string
		scopes        []string
		credentialErr error
		retire        bool
	}{
		{"narrowed", []string{"User.Read", "Mail.ReadBasic"}, nil, true},
		{"unknown", nil, nil, false},
		{"credential unavailable", nil, errors.New("synthetic credential unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			enableNativeMailEvents(t, st)
			graph := newFakeGraph(t)
			var change, changed atomic.Bool
			graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/me" {
					graph.writeJSON(w, map[string]any{"id": "synthetic-account", "mail": "me@example.com", "userPrincipalName": "me@example.com"})
					return
				}
				if change.Load() && strings.HasSuffix(r.URL.Path, "/$value") {
					changed.Store(true)
				}
				graph.serve(w, r)
			})
			client := NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) {
				if changed.Load() {
					return tc.scopes, tc.credentialErr
				}
				return []string{"Mail.Read"}, nil
			}))
			_, err := Import(t.Context(), st, client, Options{Email: "me@example.com"}, slog.Default())
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
			require.NoError(err)
			before, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			graph.put("synthetic-grant-change-arrival", "inbox")
			change.Store(true)
			_, err = Import(t.Context(), st, client, Options{Email: "me@example.com"}, slog.Default())
			require.NoError(err, "ordinary archive sync remains usable")
			assert.True(changed.Load(), "native MIME capture must cross the grant transition")
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed)
			assert.Zero(nativeMailEventCount(t, st))
			var archived int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id=?`), source.ID, "synthetic-grant-change-arrival").Scan(&archived))
			assert.Equal(1, archived)
			after, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			if tc.retire {
				assert.NotEqual(before.Reference, after.Reference)
			} else {
				assert.Equal(before.Reference, after.Reference)
			}
		})
	}
}
