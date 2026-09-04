package peoplesweep

import "slices"

// ProtocolCapability is the immutable protocol metadata shared by provider
// validation, capability negotiation, driver registration, and catalog setup.
// Slice fields are copied by ProtocolCapabilityFor before they leave this
// package so callers cannot mutate the declarations.
type ProtocolCapability struct {
	Protocol                    Protocol
	DriverVersion               string
	AuthSchemes                 []AuthScheme
	RequiredAuth                AuthScheme
	OutputModes                 []OutputMode
	TokenParameters             []string
	SupportsReasoningEffort     bool
	SupportsCustomReasoningMode bool
	CatalogAuthSchemes          []AuthScheme
	CatalogDefaultAuth          AuthScheme
	ModelsDevShapes             []string
}

var httpProtocolCapabilities = []ProtocolCapability{
	{
		Protocol:                    ProtocolOpenAIChat,
		DriverVersion:               OpenAIChatProviderVersion,
		AuthSchemes:                 []AuthScheme{AuthBearer, AuthXAPIKey, AuthGoogleAPIKey, AuthNone},
		OutputModes:                 []OutputMode{OutputModeNativeJSONSchema, OutputModeJSONObject, OutputModePromptJSON},
		TokenParameters:             []string{"max_completion_tokens", "max_tokens"},
		SupportsReasoningEffort:     true,
		SupportsCustomReasoningMode: true,
		CatalogAuthSchemes:          []AuthScheme{AuthBearer, AuthXAPIKey},
		CatalogDefaultAuth:          AuthBearer,
		ModelsDevShapes:             []string{"@ai-sdk/openai-compatible", "@ai-sdk/openai"},
	},
	{
		Protocol:                ProtocolOpenAIResponses,
		DriverVersion:           "openai-responses-v1",
		AuthSchemes:             []AuthScheme{AuthBearer, AuthXAPIKey, AuthGoogleAPIKey, AuthNone},
		OutputModes:             []OutputMode{OutputModeNativeJSONSchema, OutputModeJSONObject, OutputModePromptJSON},
		TokenParameters:         []string{""},
		SupportsReasoningEffort: true,
		CatalogAuthSchemes:      []AuthScheme{AuthBearer, AuthXAPIKey},
		CatalogDefaultAuth:      AuthBearer,
		ModelsDevShapes:         []string{"@ai-sdk/openai"},
	},
	{
		Protocol:           ProtocolAnthropicMessages,
		DriverVersion:      "anthropic-messages-v1",
		AuthSchemes:        []AuthScheme{AuthXAPIKey},
		RequiredAuth:       AuthXAPIKey,
		OutputModes:        []OutputMode{OutputModeNativeJSONSchema, OutputModePromptJSON},
		TokenParameters:    []string{""},
		CatalogAuthSchemes: []AuthScheme{AuthXAPIKey},
		CatalogDefaultAuth: AuthXAPIKey,
		ModelsDevShapes:    []string{"@ai-sdk/anthropic"},
	},
	{
		Protocol:           ProtocolGoogleGenerateContent,
		DriverVersion:      "google-generate-content-v1",
		AuthSchemes:        []AuthScheme{AuthGoogleAPIKey},
		RequiredAuth:       AuthGoogleAPIKey,
		OutputModes:        []OutputMode{OutputModeNativeJSONSchema, OutputModePromptJSON},
		TokenParameters:    []string{""},
		CatalogAuthSchemes: []AuthScheme{AuthGoogleAPIKey},
		CatalogDefaultAuth: AuthGoogleAPIKey,
		ModelsDevShapes:    []string{"@ai-sdk/google"},
	},
}

// ProtocolCapabilityFor returns a copy of the HTTP capability declaration for
// protocol. Codex is intentionally absent because it uses process transport.
func ProtocolCapabilityFor(protocol Protocol) (ProtocolCapability, bool) {
	for _, capability := range httpProtocolCapabilities {
		if capability.Protocol == protocol {
			return cloneProtocolCapability(capability), true
		}
	}
	return ProtocolCapability{}, false
}

func cloneProtocolCapability(capability ProtocolCapability) ProtocolCapability {
	capability.AuthSchemes = slices.Clone(capability.AuthSchemes)
	capability.OutputModes = slices.Clone(capability.OutputModes)
	capability.TokenParameters = slices.Clone(capability.TokenParameters)
	capability.CatalogAuthSchemes = slices.Clone(capability.CatalogAuthSchemes)
	capability.ModelsDevShapes = slices.Clone(capability.ModelsDevShapes)
	return capability
}

func protocolsForModelsDevShape(shape string) []Protocol {
	var protocols []Protocol
	for _, capability := range httpProtocolCapabilities {
		if slices.Contains(capability.ModelsDevShapes, shape) {
			protocols = append(protocols, capability.Protocol)
		}
	}
	return protocols
}
