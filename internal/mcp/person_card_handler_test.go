package mcp

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/query/querytest"
	"testing"
)

type recordingPersonCardBackend struct {
	calls  int
	ref    string
	opts   personcard.Options
	result personcard.PersonCardResult
}

func (b *recordingPersonCardBackend) GetPersonCard(_ context.Context, ref string, opts personcard.Options) (personcard.PersonCardResult, error) {
	b.calls++
	b.ref = ref
	b.opts = opts
	return b.result, nil
}

func TestPersonCardCatalogOwnerOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		backend   PersonCardsBackend
		delegated bool
		want      bool
	}{
		{"missing", nil, false, false}, {"owner", &recordingPersonCardBackend{}, false, true}, {"delegated", &recordingPersonCardBackend{}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			opts := ServeOptions{Engine: &querytest.MockEngine{}, PersonCards: tc.backend, DelegatedOnly: tc.delegated, GrantPermissions: []string{"person.read", "message.read"}}
			var found *toolDefinition
			for _, definition := range operationCatalog(opts, &handlers{}) {
				if definition.name == ToolGetPersonCard {
					found = &definition
				}
			}
			if !tc.want {
				assertions.Nil(found)
				return
			}
			requirements.NotNil(found)
			requirements.NotNil(found.outputSchema)
			for _, property := range []string{"schema_version", "status", "ref", "as_of", "collected_at", "format", "card", "content", "content_bytes", "freshness", "gaps", "participant_id", "candidates"} {
				assertions.Contains(found.outputSchema.Properties, property)
			}
			assertions.Equal([]string{"ref"}, found.inputSchema.Required)
			assertions.True(found.annotations.ReadOnlyHint)
			_, delegated := agentReadToolPermission(ToolGetPersonCard)
			assertions.False(delegated)
		})
	}
}

func TestPersonCardMCPDelegatedHandlerRejectsBeforeBackend(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	backend := &recordingPersonCardBackend{}
	handler := &handlers{delegatedOnly: true, personCards: backend}
	result, err := handler.getPersonCard(t.Context(), toolRequest{arguments: map[string]any{"ref": "email:alex@example.com"}})
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.True(result.isError)
	assertions.Zero(backend.calls)
}

func TestPersonCardMCPBlankRefRejectsBeforeBackend(t *testing.T) {
	for _, tc := range []struct{ name, ref string }{
		{"empty", ""}, {"whitespace", " \t\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			backend := &recordingPersonCardBackend{}
			handler := &handlers{personCards: backend}
			result, err := handler.getPersonCard(t.Context(), toolRequest{arguments: map[string]any{"ref": tc.ref}})
			requirements.NoError(err)
			requirements.NotNil(result)
			assertions.True(result.isError)
			assertions.Equal("ref is required", result.text)
			assertions.Zero(backend.calls)
		})
	}
}
