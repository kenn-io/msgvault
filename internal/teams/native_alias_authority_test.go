package teams

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
)

func TestNativeTeamsSMTPAliasSyncWithAndWithoutEvents(t *testing.T) {
	for _, eventsEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "events disabled", true: "events enabled"}[eventsEnabled], func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f, imp, opts := newNativeTeamsInboxFeed(t)
			f.profile = "primary@example.test"
			f.proxyAddresses = []string{"SMTP:primary@example.test", "smtp:alias@example.test"}
			opts.Email = "alias@example.test"
			if !eventsEnabled {
				_, err := imp.store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{})
				require.NoError(err)
			}

			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), sum.SourceID)
			require.NoError(err)
			assert.True(allowed, "the configured SMTP alias belongs to the verified Graph mailbox")

			f.messages["event-chat"] = []map[string]any{nativeTeamsEventMessage("alias-live", "2026-01-02T00:00:00Z", "Synthetic alias sync", "sender@example.test")}
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err, "archive sync must accept an authorized SMTP alias")
			var messages, events int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(1, messages)
			if eventsEnabled {
				assert.Equal(3, events, "the fixture captures conversation, source, and inbox event families")
			} else {
				assert.Zero(events)
			}
		})
	}
}
