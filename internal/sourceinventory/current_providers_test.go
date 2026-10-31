package sourceinventory

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCurrentProviderSchedulingAndRecovery(t *testing.T) {
	for _, archived := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			for _, kind := range []string{"pocket", "inline"} {
				t.Run(kind+map[bool]string{false: "/missing", true: "/archived"}[archived]+map[bool]string{false: "/disabled", true: "/enabled"}[enabled], func(t *testing.T) {
					assert := assertpkg.New(t)
					require := requirepkg.New(t)
					st := testutil.NewTestStore(t)
					cfg := config.NewDefaultConfig()
					identifier := "owner@example.test"
					if kind == "pocket" {
						cfg.Pocket = []config.PocketSource{{Identifier: identifier, Enabled: enabled, Schedule: "0 * * * *"}}
					} else {
						identifier = "api.inline.chat:user:123"
						cfg.Inline = config.InlineConfig{Enabled: enabled, Schedule: "0 * * * *", Accounts: []config.InlineAccount{{Identifier: identifier}}}
					}
					if archived {
						_, err := st.GetOrCreateSource(kind, identifier)
						require.NoError(err)
					}
					items, err := Build(t.Context(), cfg, st)
					require.NoError(err)
					require.Len(items, 1)
					assert.Equal(kind, items[0].SourceType)
					assert.Equal(identifier, items[0].Identifier)
					assert.Equal(map[bool]string{false: "disabled", true: "scheduled"}[enabled], items[0].Scheduling)
					assert.Equal("msgvault sync-"+kind+" "+identifier, items[0].SyncCommand)
					assert.Equal(archived, items[0].SourceID != nil)
				})
			}
		}
	}
}
