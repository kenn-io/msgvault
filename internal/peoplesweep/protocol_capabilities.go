package peoplesweep

import "slices"

// ProtocolCapability is the immutable protocol metadata shared by provider
// validation, capability negotiation, driver registration, and catalog setup.
// Slice fields are copied by ProtocolCapabilityFor before they leave this
// package so callers cannot mutate the declarations.
type ProtocolCapability struct {
	Protocol      Protocol
	DriverVersion string
	AuthSchemes   []AuthScheme
	OutputModes   []OutputMode
	// TokenParameters lists negotiation attempts in preference order. An empty
	// string means use the driver's fixed token field; an empty slice offers no attempts.
	TokenParameters             []string
	SupportsReasoningEffort     bool
	SupportsCustomReasoningMode bool
	CatalogAuthSchemes          []AuthScheme
	CatalogDefaultAuth          AuthScheme
	// CatalogTrustedHost is compiled into the binary, independent of catalog input.
	CatalogTrustedHost string
	ModelsDevShapes    []string
}

var httpProtocolCapabilities = []ProtocolCapability{
	{
		Protocol:                    ProtocolOpenAIChat,
		CatalogTrustedHost:          "api.openai.com",
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
		CatalogTrustedHost:      "api.openai.com",
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
		CatalogTrustedHost: "api.anthropic.com",
		DriverVersion:      "anthropic-messages-v1",
		AuthSchemes:        []AuthScheme{AuthXAPIKey},
		OutputModes:        []OutputMode{OutputModeNativeJSONSchema, OutputModePromptJSON},
		TokenParameters:    []string{""},
		CatalogAuthSchemes: []AuthScheme{AuthXAPIKey},
		CatalogDefaultAuth: AuthXAPIKey,
		ModelsDevShapes:    []string{"@ai-sdk/anthropic"},
	},
	{
		Protocol:           ProtocolGoogleGenerateContent,
		CatalogTrustedHost: "generativelanguage.googleapis.com",
		DriverVersion:      "google-generate-content-v1",
		AuthSchemes:        []AuthScheme{AuthGoogleAPIKey},
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
