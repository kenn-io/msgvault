package client

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestFileSuggestionsGeneratedNullTimestamp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, row := range []any{generated.FileSuggestionRow{}, generated.PersonFileSuggestionRow{}} {
		raw, err := json.Marshal(row)
		require.NoError(err)
		var fields map[string]any
		require.NoError(jsonv1.Unmarshal(raw, &fields))
		assert.Contains(fields, "occurred_at", "required nullable timestamps survive generated-client JSON")
		assert.Nil(fields["occurred_at"])
	}
}

func TestFileSuggestionsGeneratedEmptyMIME(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	empty := ""
	row := generated.FileSuggestionRow{ID: 1, MessageID: 2, ConversationID: 3, SourceID: 4, Filename: "report.pdf", MimeType: &empty, MimeFamily: "other", FilenameMatch: "exact", ContentState: "metadata_only"}
	assert.NoError(row.Validate(), "unknown MIME is represented by an empty mime_type")
	person := generated.PersonFileSuggestionRow{ID: 1, MessageID: 2, ConversationID: 3, SourceID: 4, Filename: "report.pdf", MimeType: &empty, MimeFamily: "other", FilenameMatch: "exact", ContentState: "metadata_only", PersonProvenance: generated.Provenance{ParticipantIds: []int64{5}, Roles: []generated.ProvenanceRoles{"to"}, Directions: []generated.ProvenanceDirections{"to_person"}}}
	assert.NoError(person.Validate())
	for _, value := range []any{row, person} {
		raw, err := json.Marshal(value)
		require.NoError(err)
		var fields map[string]any
		require.NoError(jsonv1.Unmarshal(raw, &fields))
		mime, ok := fields["mime_type"].(string)
		require.True(ok, "required MIME field must remain a string")
		assert.Empty(mime)
	}
	row.MimeType = nil
	person.MimeType = nil
	require.ErrorContains(row.Validate(), "MimeType is required")
	require.ErrorContains(person.Validate(), "MimeType is required")
}
