package peoplesweep

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProtocolCapabilityLookupCopiesSlices(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	capability, ok := ProtocolCapabilityFor(ProtocolOpenAIChat)
	require.True(ok)
	capability.AuthSchemes[0] = AuthNone
	capability.OutputModes[0] = OutputModePromptJSON
	capability.TokenParameters[0] = "changed"
	capability.CatalogAuthSchemes[0] = AuthGoogleAPIKey
	capability.ModelsDevShapes[0] = "changed"

	unchanged, ok := ProtocolCapabilityFor(ProtocolOpenAIChat)
	require.True(ok)
	assert.Equal(AuthBearer, unchanged.AuthSchemes[0])
	assert.Equal(OutputModeNativeJSONSchema, unchanged.OutputModes[0])
	assert.Equal("max_completion_tokens", unchanged.TokenParameters[0])
	assert.Equal(AuthBearer, unchanged.CatalogAuthSchemes[0])
	assert.Equal("@ai-sdk/openai-compatible", unchanged.ModelsDevShapes[0])
}

func TestCapabilityDriverSelectsHTTPDriversAndExcludesCodex(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	registry, err := NewDriverRegistry(http.DefaultClient, nil, nil)
	require.NoError(err)

	for _, protocol := range []Protocol{
		ProtocolOpenAIChat, ProtocolOpenAIResponses,
		ProtocolAnthropicMessages, ProtocolGoogleGenerateContent,
	} {
		capability, ok := ProtocolCapabilityFor(protocol)
		require.True(ok)
		driver, err := registry.capabilityDriver(protocol)
		require.NoError(err)
		profile, err := capabilityProfile(ProviderConfig{
			Protocol: protocol, Endpoint: "https://api.example.com", Model: "test-model",
			Auth: capability.AuthSchemes[0],
		}, capabilityOutputModes(protocol)[0], capabilityTokenParameters(protocol)[0], false)
		require.NoError(err)
		_, err = driver.Prepare(profile, capabilitySyntheticRequest())
		require.NoError(err)
	}
	_, err = registry.capabilityDriver(ProtocolCodexAppServer)
	assert.Error(err)
}
