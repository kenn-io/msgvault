package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestMCPEventsMessageDTOsPreserveCalendarAndSource(t *testing.T) {
	status := "cancelled"
	sequence := int64(0)
	msg := &query.MessageDetail{ID: 7, SourceID: 3, IsFromMe: true, MessageType: "calendar_event", Calendar: &store.CalendarProjection{Status: &status, Sequence: &sequence}}
	for name, dto := range map[string]any{"cli": cliMessageResponseFromQuery(msg), "detail": messageDetailFromQuery(msg)} {
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			data, err := json.Marshal(dto)
			require.NoError(err)
			var got map[string]any
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			require.NoError(decoder.Decode(&got))
			assert.Equal(json.Number("3"), got["source_id"])
			assert.Equal(true, got["is_from_me"])
			calendar, ok := got["calendar"].(map[string]any)
			require.True(ok, string(data))
			assert.Equal("cancelled", calendar["status"])
			assert.Equal(json.Number("0"), calendar["sequence"])
			assert.Contains(calendar, "all_day")
			assert.Nil(calendar["all_day"])
		})
	}
}

func TestMCPEventsOpenAPIErrorContract(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	doc := OpenAPIDocument()
	for path, operation := range doc.Paths {
		if len(path) < len(mcpEventsPath)+1 || path[:len(mcpEventsPath)+1] != mcpEventsPath+"/" {
			continue
		}
		for _, op := range []*huma.Operation{operation.Get, operation.Post} {
			if op == nil {
				continue
			}
			response := op.Responses["default"]
			require.NotNil(response, path)
			schema := response.Content[applicationJSONMediaType].Schema
			assert.Equal("#/components/schemas/MCPEventsErrorResponse", schema.Ref, path)
		}
	}
	errorSchema := doc.Components.Schemas.Map()["MCPEventsErrorResponse"]
	require.NotNil(errorSchema)
	assert.Contains(errorSchema.Properties, "code")
	assert.Contains(errorSchema.Properties, "reason")
}
