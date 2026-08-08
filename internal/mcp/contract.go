package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
)

type toolRequest struct {
	arguments map[string]any
}

func (r toolRequest) GetArguments() map[string]any {
	return r.arguments
}

type embeddedResource struct {
	uri      string
	mimeType string
	blob     string
}

type toolResult struct {
	text              string
	structuredContent json.RawMessage
	embeddedResource  *embeddedResource
	isError           bool
}

type internalError struct {
	operation string
	cause     error
}

func (e *internalError) Error() string {
	return fmt.Sprintf("%s: %v", e.operation, e.cause)
}

func (e *internalError) Unwrap() error {
	return e.cause
}

func newInternalError(operation string, err error) error {
	var privateErr *internalError
	if errors.As(err, &privateErr) {
		return err
	}
	return &internalError{operation: operation, cause: err}
}

func jsonResult(v any) (*toolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, &internalError{
			operation: "marshal tool result",
			cause:     err,
		}
	}

	raw := json.RawMessage(data)
	return &toolResult{
		text:              string(raw),
		structuredContent: raw,
	}, nil
}

func toolErrorResult(text string) *toolResult {
	return &toolResult{
		text:    text,
		isError: true,
	}
}
