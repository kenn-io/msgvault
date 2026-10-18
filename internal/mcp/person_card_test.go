package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestPersonCardMCPPassesEveryStatus(t *testing.T) {
	for _, status := range []string{"ok", "observed_only", "unknown", "ambiguous"} {
		t.Run(status, func(t *testing.T) {
			assertions := assert.New(t)
			fixture := personcard.PersonCardResult{SchemaVersion: 1, Status: status, Ref: "email:alex@example.com", Format: personcard.FormatJSON, Candidates: []personcard.Candidate{}, Gaps: []string{}}
			switch status {
			case "ok":
				fixture.Card = &personcard.Card{Person: personcard.Person{PersonID: 7}}
			case "observed_only":
				fixture.ParticipantID = new(int64(7))
				fixture.Gaps = []string{"not_a_saved_profile"}
			case "unknown":
				fixture.Gaps = []string{"not_in_archive"}
			case "ambiguous":
				fixture.Candidates = []personcard.Candidate{{PersonID: 7, ContentTrust: "source_controlled", UntrustedText: personcard.CandidateText{DisplayName: "Alex Example"}}}
			}
			backend := &recordingPersonCardBackend{result: fixture}
			response := rawModernCall(t, ServeOptions{Engine: &querytest.MockEngine{}, PersonCards: backend}, HTTPOptions{}, "tools/call", map[string]any{"name": ToolGetPersonCard, "arguments": map[string]any{"ref": "email:alex@example.com", "as_of": "2026-10-17T15:00:00Z", "max_items": 4, "max_bytes": 8192, "format": "json", "affiliation": "external"}})
			require.NotEqual(t, true, response.Result["isError"])
			content := toolStructuredContent(t, response.Result)
			assertions.Equal(status, content["status"])
			assertions.Equal("email:alex@example.com", backend.ref)
			assertions.Equal(personcard.Options{AsOf: time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC), MaxItems: 4, MaxBytes: 8192, Format: personcard.FormatJSON, Affiliation: "external"}, backend.opts)
		})
	}
}

func TestPersonCardMCPRejectsInvalidArgumentsBeforeReading(t *testing.T) {
	for _, args := range []map[string]any{
		{"ref": "person:7", "extra": true}, {"ref": "person:7", "max_items": 0}, {"ref": "person:7", "max_items": 11},
		{"ref": "person:7", "max_bytes": 2047}, {"ref": "person:7", "max_bytes": 65537},
		{"ref": "person:7", "format": "xml"}, {"ref": "person:7", "affiliation": "friend"}, {"ref": "person:7", "as_of": "yesterday"},
	} {
		backend := &recordingPersonCardBackend{}
		response := rawModernCall(t, ServeOptions{Engine: &querytest.MockEngine{}, PersonCards: backend}, HTTPOptions{}, "tools/call", map[string]any{"name": ToolGetPersonCard, "arguments": args})
		assert.Zero(t, backend.calls)
		assert.True(t, response.Error != nil || response.Result["isError"] == true)
	}
}

func TestPersonCardMCPDefaults(t *testing.T) {
	backend := &recordingPersonCardBackend{result: personcard.PersonCardResult{SchemaVersion: 1, Status: "unknown", Ref: "email:missing@example.com", Format: personcard.FormatJSON, Gaps: []string{"not_in_archive"}}}
	response := rawModernCall(t, ServeOptions{Engine: &querytest.MockEngine{}, PersonCards: backend}, HTTPOptions{}, "tools/call", map[string]any{"name": ToolGetPersonCard, "arguments": map[string]any{"ref": "email:missing@example.com"}})
	require.NotEqual(t, true, response.Result["isError"])
	assert.Equal(t, personcard.Options{MaxItems: 3, MaxBytes: 16384, Format: personcard.FormatJSON}, backend.opts)
}
