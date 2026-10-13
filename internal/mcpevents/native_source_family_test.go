package mcpevents

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestNativeSourceFamilyCanonicalRequiresLifetimeReference(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	const family = "msgvault.source_message_archived"
	ref := "mcp1." + strings.Repeat("A", 43)
	a, err := canonicalArguments(family, map[string]any{"source_id": "7", "source_ref": ref})
	require.NoError(err)
	assert.Equal("source", a.scopeKind)
	assert.Equal(int64(7), a.scopeID)
	assert.Equal(ref, a.reference)
	assert.JSONEq(`{"source_id":"7","source_ref":"`+ref+`","include_from_me":false,"include_reactions":true}`, string(a.bytes))
	same, err := canonicalArguments(family, map[string]any{"source_id": "7", "source_ref": ref, "include_from_me": false, "include_reactions": true})
	require.NoError(err)
	assert.Equal(a.bytes, same.bytes)
	changed, err := canonicalArguments(family, map[string]any{"source_id": "7", "source_ref": ref, "include_from_me": true, "include_reactions": false})
	require.NoError(err)
	assert.NotEqual(a.bytes, changed.bytes)
	for _, input := range []map[string]any{
		{"source_id": "7"}, {"source_id": "7", "source_ref": nil}, {"source_id": "7", "source_ref": ""}, {"source_id": "7", "source_ref": ref, "conversation_id": "7"},
		{"source_id": "0", "source_ref": ref}, {"source_id": 7, "source_ref": ref}, {"source_id": "7", "source_ref": ref, "include_from_me": nil}, {"source_id": "7", "source_ref": ref, "include_reactions": "true"},
	} {
		_, err := canonicalArguments(family, input)
		require.Error(err)
	}
	legacy, err := canonicalArguments(messageFamily, map[string]any{"conversation_id": "7"})
	require.NoError(err)
	assert.Equal(`{"conversation_id":"7","include_from_me":false}`, string(legacy.bytes)) //nolint:testifylint // Retained subscription identity is bound to these bytes.
}

func TestNativeSourceFamilyDiscoveryUsesSourceScope(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := storetest.New(t)
	service, err := New(t.Context(), f.Store, Options{Enabled: true, Sources: []string{"gmail"}, OwnerKey: "synthetic-native-source-owner", KeyPath: filepath.Join(t.TempDir(), "events.key")})
	require.NoError(err)
	source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Known: true, MessageRead: true}))
	sources, err := service.DiscoverResources(t.Context(), service.principal, ResourceQuery{Kind: "source", SourceID: strconv.FormatInt(f.Source.ID, 10)})
	require.NoError(err)
	require.Len(sources.Resources, 1)
	assert.Equal([]string{"msgvault.source_message_archived"}, sources.Resources[0].Capabilities.Events)
	conversations, err := service.DiscoverResources(t.Context(), service.principal, ResourceQuery{Kind: "conversation", SourceID: strconv.FormatInt(f.Source.ID, 10)})
	require.NoError(err)
	require.NotEmpty(conversations.Resources)
	assert.NotContains(conversations.Resources[0].Capabilities.Events, "msgvault.source_message_archived")
	var sourceDefinition *Definition
	for _, definition := range service.Catalog().Events {
		if definition.Name == "msgvault.source_message_archived" {
			sourceDefinition = &definition
			break
		}
	}
	require.NotNil(sourceDefinition)
	assert.ElementsMatch([]string{"source_id", "source_ref"}, sourceDefinition.InputSchema["required"])
	properties, ok := sourceDefinition.InputSchema["properties"].(map[string]any)
	require.True(ok)
	assert.Contains(properties, "include_from_me")
	assert.Contains(properties, "include_reactions")
	assert.NotContains(properties, "conversation_id")
	assert.Equal(false, sourceDefinition.InputSchema["additionalProperties"])
	for _, capability := range capabilities(true, []string{"slackdump", "twilio", "gcal"}) {
		assert.NotEqual("msgvault.source_message_archived", capability.Family, "non-live producers cannot enable source subscriptions")
	}
}
