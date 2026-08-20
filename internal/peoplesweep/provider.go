package peoplesweep

import (
	"context"
	"encoding/json"
	"fmt"
)

// SourceDescriptor records the policy-relevant class and calendar date for one
// text source included in a structured request.
type SourceDescriptor struct {
	Class      SourceClass `json:"class"`
	ObservedOn string      `json:"observed_on"`
}

// StructuredRequest is the text-only provider-neutral inference contract.
type StructuredRequest struct {
	ProgramID         string             `json:"program_id"`
	ProgramVersion    string             `json:"program_version"`
	Sources           []SourceDescriptor `json:"sources"`
	ContainsSensitive bool               `json:"contains_sensitive"`
	InputText         string             `json:"input_text"`
	SchemaName        string             `json:"schema_name"`
	JSONSchema        json.RawMessage    `json:"json_schema"`
	MaxOutputTokens   int                `json:"max_output_tokens"`
}

// TokenUsage is provider-reported accounting. It is not trusted as a privacy
// or budget boundary on its own.
type TokenUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// StructuredResponse contains only parsed JSON and safe provider metadata.
type StructuredResponse struct {
	Output            json.RawMessage `json:"output"`
	ProviderRequestID string          `json:"provider_request_id,omitempty"`
	Usage             TokenUsage      `json:"usage"`
}

// StructuredTransport is the network-only adapter boundary. Callers must use
// Runner rather than invoke a transport directly outside this package.
type StructuredTransport interface {
	GenerateJSON(
		ctx context.Context,
		profile ProviderProfile,
		credential string,
		request StructuredRequest,
	) (StructuredResponse, error)
}

// StructuredRunner is the consent-gated entry point later people-sweep
// programs consume.
type StructuredRunner interface {
	RunStructured(ctx context.Context, request StructuredRequest) (StructuredResponse, error)
}

// ProviderError exposes only response status and a safe provider request ID.
// Provider response bodies are intentionally discarded.
type ProviderError struct {
	StatusCode int
	RequestID  string
}

func (e *ProviderError) Error() string {
	if e.RequestID == "" {
		return fmt.Sprintf("inference provider returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("inference provider returned HTTP %d (request_id=%s)",
		e.StatusCode, e.RequestID)
}
