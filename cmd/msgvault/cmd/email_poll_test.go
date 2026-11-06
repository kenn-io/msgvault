package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRapidEmailRejectsNonCanonicalTargetsBeforeCredentials(t *testing.T) {
	for _, scenario := range []string{"missing", "display name", "ambiguous providers"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = true
poll_interval = "30s"
`), 0o600))
			cfg, err := config.Load(path, dir)
			require.NoError(t, err)
			st := storetest.New(t)
			switch scenario {
			case "display name":
				src, err := st.Store.GetOrCreateSource("gmail", "other@example.test")
				require.NoError(t, err)
				require.NoError(t, st.Store.UpdateSourceDisplayName(src.ID, "sample@example.test"))
			case "ambiguous providers":
				for _, typ := range []string{"gmail", "msmail"} {
					_, err := st.Store.GetOrCreateSource(typ, "sample@example.test")
					require.NoError(t, err)
				}
			}
			count := func() int {
				t.Helper()
				var n int
				require.NoError(t, st.Store.DB().QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n))
				return n
			}
			before := count()
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			credentials := 0
			err = runScheduledSync(ctx, "sample@example.test", st.Store, func(context.Context, string) (*oauth.Manager, error) {
				credentials++
				return nil, errors.New("synthetic credentials unavailable")
			}, invocationFromContext(ctx))
			assert.Error(t, err)
			assert.Zero(t, credentials, "invalid rapid targets fail before resolving credentials")
			assert.Equal(t, before, count(), "rapid reconciliation never creates a source")
		})
	}
}

func TestRapidEmailResolverRejectsAliasAndRetiredSource(t *testing.T) {
	f := storetest.New(t)
	old, err := f.Store.GetOrCreateSource("gmail", "old@example.test")
	require.NoError(t, err)
	current, err := f.Store.GetOrCreateSource("gmail", "current@example.test")
	require.NoError(t, err)
	alias := "primary-mailbox"
	_, err = f.Store.UpdateSourceSettingsContext(t.Context(), old.ID, store.SourceSettingsUpdate{Alias: &alias})
	require.NoError(t, err)
	_, err = rapidEmailSource(f.Store, alias)
	assert.Error(t, err, "archive aliases are not canonical rapid selectors")
	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: old.ID, IntoSourceID: current.ID})
	require.NoError(t, err)
	_, err = rapidEmailSource(f.Store, old.Identifier)
	assert.Error(t, err, "retired mail sources cannot fall back to credentials or creation")
}

func TestRapidEmailResolverSelectsCurrentSourceAfterReplacement(t *testing.T) {
	f := storetest.New(t)
	old, err := f.Store.GetOrCreateSource("gmail", "sample@example.test")
	require.NoError(t, err)
	_, err = f.Store.GetOrCreateSource("gmail", "anchor@example.test")
	require.NoError(t, err)
	first, err := rapidEmailSource(f.Store, old.Identifier)
	require.NoError(t, err)
	assert.Equal(t, old.ID, first.ID)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM sources WHERE id=?`), old.ID)
	require.NoError(t, err)
	replacement, err := f.Store.GetOrCreateSource("gmail", old.Identifier)
	require.NoError(t, err)
	require.NotEqual(t, old.ID, replacement.ID)
	current, err := rapidEmailSource(f.Store, old.Identifier)
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, current.ID, "each valid pass resolves current Store state")
}
