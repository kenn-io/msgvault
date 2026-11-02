package sourceinventory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCurrentProviderSchedulingAndRecovery(t *testing.T) {
	for _, archived := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			for _, kind := range []string{"pocket", "inline"} {
				t.Run(kind+map[bool]string{false: "/missing", true: "/archived"}[archived]+map[bool]string{false: "/disabled", true: "/enabled"}[enabled], func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
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
						requirements.NoError(err)
					}
					items, err := Build(t.Context(), cfg, st)
					requirements.NoError(err)
					requirements.Len(items, 1)
					assertions.Equal(kind, items[0].SourceType)
					assertions.Equal(identifier, items[0].Identifier)
					assertions.Equal(map[bool]string{false: "disabled", true: "scheduled"}[enabled], items[0].Scheduling)
					assertions.Equal("msgvault sync-"+kind+" "+identifier, items[0].SyncCommand)
					assertions.Equal(archived, items[0].SourceID != nil)
				})
			}
		}
	}
}
