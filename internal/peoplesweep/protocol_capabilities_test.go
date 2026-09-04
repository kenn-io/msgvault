package peoplesweep

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProtocolCapabilities(t *testing.T) {
	tests := []struct {
		protocol                    Protocol
		driverVersion               string
		authSchemes                 []AuthScheme
		requiredAuth                AuthScheme
		outputModes                 []OutputMode
		tokenParameters             []string
		supportsReasoningEffort     bool
		supportsCustomReasoningMode bool
		catalogAuthSchemes          []AuthScheme
		catalogDefaultAuth          AuthScheme
		modelsDevShapes             []string
	}{
		{
			protocol: ProtocolOpenAIChat, driverVersion: OpenAIChatProviderVersion,
			authSchemes:             []AuthScheme{AuthBearer, AuthXAPIKey, AuthGoogleAPIKey, AuthNone},
			outputModes:             []OutputMode{OutputModeNativeJSONSchema, OutputModeJSONObject, OutputModePromptJSON},
			tokenParameters:         []string{"max_completion_tokens", "max_tokens"},
			supportsReasoningEffort: true, supportsCustomReasoningMode: true,
			catalogAuthSchemes: []AuthScheme{AuthBearer, AuthXAPIKey}, catalogDefaultAuth: AuthBearer,
			modelsDevShapes: []string{"@ai-sdk/openai-compatible", "@ai-sdk/openai"},
		},
		{
			protocol: ProtocolOpenAIResponses, driverVersion: "openai-responses-v1",
			authSchemes:     []AuthScheme{AuthBearer, AuthXAPIKey, AuthGoogleAPIKey, AuthNone},
			outputModes:     []OutputMode{OutputModeNativeJSONSchema, OutputModeJSONObject, OutputModePromptJSON},
			tokenParameters: []string{""}, supportsReasoningEffort: true,
			catalogAuthSchemes: []AuthScheme{AuthBearer, AuthXAPIKey}, catalogDefaultAuth: AuthBearer,
			modelsDevShapes: []string{"@ai-sdk/openai"},
		},
		{
			protocol: ProtocolAnthropicMessages, driverVersion: "anthropic-messages-v1",
			authSchemes: []AuthScheme{AuthXAPIKey}, requiredAuth: AuthXAPIKey,
			outputModes:     []OutputMode{OutputModeNativeJSONSchema, OutputModePromptJSON},
			tokenParameters: []string{""}, catalogAuthSchemes: []AuthScheme{AuthXAPIKey},
			catalogDefaultAuth: AuthXAPIKey, modelsDevShapes: []string{"@ai-sdk/anthropic"},
		},
		{
			protocol: ProtocolGoogleGenerateContent, driverVersion: "google-generate-content-v1",
			authSchemes: []AuthScheme{AuthGoogleAPIKey}, requiredAuth: AuthGoogleAPIKey,
			outputModes:     []OutputMode{OutputModeNativeJSONSchema, OutputModePromptJSON},
			tokenParameters: []string{""}, catalogAuthSchemes: []AuthScheme{AuthGoogleAPIKey},
			catalogDefaultAuth: AuthGoogleAPIKey, modelsDevShapes: []string{"@ai-sdk/google"},
		},
	}

	for _, test := range tests {
		t.Run(string(test.protocol), func(t *testing.T) {
			capability, ok := ProtocolCapabilityFor(test.protocol)
			require.True(t, ok)
			assert.Equal(t, test.protocol, capability.Protocol)
			assert.Equal(t, test.driverVersion, capability.DriverVersion)
			assert.Equal(t, test.authSchemes, capability.AuthSchemes)
			assert.Equal(t, test.requiredAuth, capability.RequiredAuth)
			assert.Equal(t, test.outputModes, capability.OutputModes)
			assert.Equal(t, test.tokenParameters, capability.TokenParameters)
			assert.Equal(t, test.supportsReasoningEffort, capability.SupportsReasoningEffort)
			assert.Equal(t, test.supportsCustomReasoningMode, capability.SupportsCustomReasoningMode)
			assert.Equal(t, test.catalogAuthSchemes, capability.CatalogAuthSchemes)
			assert.Equal(t, test.catalogDefaultAuth, capability.CatalogDefaultAuth)
			assert.Equal(t, test.modelsDevShapes, capability.ModelsDevShapes)
		})
	}

	assert.False(t, func() bool {
		_, ok := ProtocolCapabilityFor(ProtocolCodexAppServer)
		return ok
	}())
}

func TestProtocolCapabilityLookupCopiesSlices(t *testing.T) {
	capability, ok := ProtocolCapabilityFor(ProtocolOpenAIChat)
	require.True(t, ok)
	capability.AuthSchemes[0] = AuthNone
	capability.OutputModes[0] = OutputModePromptJSON
	capability.TokenParameters[0] = "changed"
	capability.CatalogAuthSchemes[0] = AuthGoogleAPIKey
	capability.ModelsDevShapes[0] = "changed"

	unchanged, ok := ProtocolCapabilityFor(ProtocolOpenAIChat)
	require.True(t, ok)
	assert.Equal(t, AuthBearer, unchanged.AuthSchemes[0])
	assert.Equal(t, OutputModeNativeJSONSchema, unchanged.OutputModes[0])
	assert.Equal(t, "max_completion_tokens", unchanged.TokenParameters[0])
	assert.Equal(t, AuthBearer, unchanged.CatalogAuthSchemes[0])
	assert.Equal(t, "@ai-sdk/openai-compatible", unchanged.ModelsDevShapes[0])
}

func TestProtocolCapabilityRegistrationPairsHTTPDriversAndExcludesCodex(t *testing.T) {
	registry, err := NewDriverRegistry(http.DefaultClient, nil, nil)
	require.NoError(t, err)

	for _, protocol := range []Protocol{
		ProtocolOpenAIChat, ProtocolOpenAIResponses,
		ProtocolAnthropicMessages, ProtocolGoogleGenerateContent,
	} {
		registration, ok := registry.registrations[protocol]
		require.True(t, ok)
		assert.Equal(t, protocol, registration.capability.Protocol)
		assert.NotNil(t, registration.driver)
		_, err := registry.capabilityDriver(protocol)
		assert.NoError(t, err)
	}
	_, err = registry.capabilityDriver(ProtocolCodexAppServer)
	assert.Error(t, err)
}
