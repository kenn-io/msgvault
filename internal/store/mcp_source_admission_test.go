package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPSourceAdmissionUsesExactSourceLifetime(t *testing.T) {
	const family = "msgvault.source_message_archived"
	for _, mode := range []string{"valid", "conversation_reference", "other_source", "stale_source"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f := storetest.New(t)
			grantSyntheticSourceFixture(t, f)
			cfg := mcpStoreConfig()
			cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: family, SourceType: "gmail", Kinds: []string{"message"}})
			_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
			require.NoError(err)
			reference := resource.Reference
			switch mode {
			case "conversation_reference":
				conversation, err := f.Store.GetMCPResource(t.Context(), "conversation", f.ConvID)
				require.NoError(err)
				reference = conversation.Reference
			case "other_source":
				other, err := f.Store.GetOrCreateSource("gmail", "another-synthetic-source@example.net")
				require.NoError(err)
				otherResource, err := f.Store.GetMCPResource(t.Context(), "source", other.ID)
				require.NoError(err)
				reference = otherResource.Reference
			case "stale_source":
				for _, identifier := range []string{"replacement-source@example.net", f.Source.Identifier} {
					require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, identifier))
					grantSyntheticSourceFixture(t, f)
					current, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
					require.NoError(err)
					currentSource, currentBinding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), family, "source", f.Source.ID, []string{"message"}, current.Reference)
					require.NoError(err)
					assert.Equal(f.Source.ID, currentSource)
					require.NotNil(currentBinding)
				}
			}
			source, binding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), family, "source", f.Source.ID, []string{"message"}, reference)
			if mode == "valid" {
				require.NoError(err)
				assert.Equal(f.Source.ID, source)
				assert.NotNil(binding)
			} else {
				require.Error(err)
				assert.Nil(binding)
			}
		})
	}
}
