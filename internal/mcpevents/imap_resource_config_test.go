package mcpevents

import (
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
)

func TestIMAPResourceLifetimeTracksRoutingConfig(t *testing.T) {
	for _, tc := range []struct {
		name           string
		change         func(*imap.Config)
		restore, valid bool
	}{
		{name: "host", change: func(c *imap.Config) { c.Host = "replacement.example.net" }},
		{name: "username", change: func(c *imap.Config) { c.Username = "replacement@example.net" }},
		{name: "auth", change: func(c *imap.Config) { c.AuthMethod = imap.AuthXOAuth2 }},
		{name: "host_away_back", change: func(c *imap.Config) { c.Host = "replacement.example.net" }, restore: true},
		{name: "effective_defaults", change: func(c *imap.Config) { c.Port = 993; c.AuthMethod = imap.AuthPassword }, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			original, fixture, request := eventService(t)
			opts := original.opts
			opts.Sources = append(opts.Sources, "imap")
			service, err := New(t.Context(), fixture.Store, opts)
			require.NoError(err)
			service.webhook = original.webhook
			config := imap.Config{Host: "imap.example.net", Username: "owner@example.net", TLS: true}
			source, err := fixture.Store.GetOrCreateSource("imap", config.Identifier())
			require.NoError(err)
			before, err := config.ToJSON()
			require.NoError(err)
			require.NoError(fixture.Store.UpdateSourceSyncConfig(source.ID, before))
			// This unit isolates routing lifetimes from native credential probes.
			// Give each selected account lifetime an explicit synthetic grant;
			// native IMAP authentication/capture is exercised by the SDK fixture.
			grantCurrentRoute := func() {
				resource, err := fixture.Store.GetMCPResource(t.Context(), "source", source.ID)
				Require.NoError(t, err)
				Require.NoError(t, fixture.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "imap", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
			}
			grantCurrentRoute()
			conversation, err := fixture.Store.EnsureConversation(source.ID, "synthetic-thread", "Synthetic thread")
			require.NoError(err)
			request.Arguments = map[string]any{"conversation_id": strconv.FormatInt(conversation, 10)}
			first, err := service.Subscribe(t.Context(), service.principal, request)
			require.NoError(err)
			tc.change(&config)
			after, err := config.ToJSON()
			require.NoError(err)
			require.NoError(fixture.Store.UpdateSourceSyncConfig(source.ID, after))
			if tc.restore {
				require.NoError(fixture.Store.UpdateSourceSyncConfig(source.ID, before))
			}
			grantCurrentRoute()
			result, err := service.Subscribe(t.Context(), service.principal, request)
			if tc.valid {
				require.NoError(err)
				assert.Equal(first.ID, result.ID)
			} else {
				require.Error(err, "IMAP routing replacement cannot retain subscription authority")
			}
		})
	}
}
