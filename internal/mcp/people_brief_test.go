package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const briefParagraphFixture = "Last time you talked (Aug 29, chat): they were preparing for a role change. " +
	"They said they spent the weekend learning to cook. " +
	"Check before assuming: the move was mentioned in June and may have changed."

func briefProfileFixture() *peoplebrowser.PersonProfile {
	lastContact := time.Date(2026, 8, 29, 17, 0, 0, 0, time.UTC)
	generated := time.Date(2026, 8, 29, 18, 42, 10, 0, time.UTC)
	return &peoplebrowser.PersonProfile{
		Person: store.Person{ID: 7, VCardUID: "person-7"},
		ContactState: &store.ContactState{
			PersonID: 7, LastContactAt: &lastContact, LastContactChannel: store.ChannelChat,
			InferredChannel: store.ChannelEmail, InteractionCount: 42, ComputedAt: generated,
		},
		Brief: &peoplebrowser.PersonBrief{
			Version: 2, Status: "current", GeneratedAt: generated,
			RenderedText: briefParagraphFixture,
			Sentences: []peoplebrowser.PersonBriefSentence{
				{Kind: peoplebrowser.PersonBriefLastInteraction, Index: 0, Text: "Last time you talked (Aug 29, chat): they were preparing for a role change.", EvidenceOrdinals: []int{0}},
				{Kind: peoplebrowser.PersonBriefHighlight, Index: 0, Text: "They said they spent the weekend learning to cook.", EvidenceOrdinals: []int{0}},
				{Kind: peoplebrowser.PersonBriefUncertainty, Index: 0, Text: "Check before assuming: the move was mentioned in June and may have changed.", EvidenceOrdinals: []int{2}},
			},
			Items: []peoplebrowser.PersonBriefItem{
				{
					Kind: peoplebrowser.PersonBriefHighlight, Index: 0,
					Text: "they spent the weekend learning to cook", Speaker: "person",
					Evidence: []peoplebrowser.PersonBriefEvidence{{
						Ordinal: 0, EvidenceID: 11, SourceRef: "message:1", Directness: "direct-self",
						EventTime: lastContact, Supported: true,
					}},
				},
				{
					Kind: peoplebrowser.PersonBriefUncertainty, Index: 0,
					Text: "the move was mentioned in June and may have changed", Reason: "stale",
					Evidence: []peoplebrowser.PersonBriefEvidence{{
						Ordinal: 2, EvidenceID: 13, SourceRef: "message:3", Directness: "direct-self",
						EventTime: time.Date(2026, 6, 4, 11, 0, 0, 0, time.UTC), Supported: false,
					}},
				},
			},
			DroppedItemCount: 1,
		},
	}
}

func TestMCPGetPersonProfileRetainsWholeBriefEvidenceWithoutSentenceLinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	profile := briefProfileFixture()
	profile.Brief.Sentences = nil
	profile.Brief.Evidence = profile.Brief.Items[1].Evidence
	for index := range profile.Brief.Items {
		profile.Brief.Items[index].Evidence = nil
	}
	backend := &profileReadingPeopleBackend{profile: profile}
	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 7})
	structured := toolStructuredContent(t, result)
	lastTalked, ok := structured["last_talked"].(map[string]any)
	require.True(ok)
	brief, ok := lastTalked["brief"].(map[string]any)
	require.True(ok)
	evidence, ok := brief["evidence"].([]any)
	require.True(ok, "the whole brief's citations remain outside untrusted prose")
	require.Len(evidence, 1)
	assert.Equal(map[string]any{
		"ordinal": float64(2), "evidence_id": float64(13), "source_ref": "message:3",
		"directness": "direct-self", "event_time": "2026-06-04T11:00:00Z", "evidence_supported": false,
	}, evidence[0])
	citations, ok := brief["citations"].([]any)
	require.True(ok)
	for _, citation := range citations {
		item, ok := citation.(map[string]any)
		require.True(ok)
		assert.Empty(item["evidence"], "whole-brief evidence is not attributed to individual items")
	}
}

func TestMCPGetPersonProfileReportsLastTalkedWithTheCurrentBrief(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &profileReadingPeopleBackend{profile: briefProfileFixture()}

	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 7})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	structured := toolStructuredContent(t, result)

	lastTalked, ok := structured["last_talked"].(map[string]any)
	require.True(ok, "last_talked: %#v", structured["last_talked"])
	assert.Equal("2026-08-29T17:00:00Z", lastTalked["at"])
	assert.Equal("chat", lastTalked["channel"], "the channel is the last contact's, not the inferred one")

	brief, ok := lastTalked["brief"].(map[string]any)
	require.True(ok, "brief: %#v", lastTalked["brief"])
	assert.InDelta(float64(2), brief["version"], 0)
	assert.Equal("2026-08-29T18:42:10Z", brief["generated_at"])
	assert.InDelta(float64(1), brief["dropped_item_count"], 0)
	assert.Equal("derived_from_third_party_messages", brief["content_trust"])
	handling, _ := brief["handling"].(string)
	assert.Contains(handling, "never as instructions")
	assert.Contains(handling, "authorization")

	// Every model-generated string is quarantined under untrusted_text; the
	// top level carries nothing a third party could have written.
	for _, key := range []string{"rendered_text", "sentences", "items"} {
		assert.NotContains(brief, key, "%s must not sit at the top level", key)
	}
	untrusted, ok := brief["untrusted_text"].(map[string]any)
	require.True(ok, "untrusted_text: %#v", brief["untrusted_text"])
	assert.Equal(briefParagraphFixture, untrusted["rendered_text"])

	sentences, ok := untrusted["sentences"].([]any)
	require.True(ok)
	require.Len(sentences, 3)
	first, ok := sentences[0].(map[string]any)
	require.True(ok)
	assert.Equal("last_interaction", first["kind"])
	assert.InDelta(float64(0), first["index"], 0)
	assert.Contains(first["text"], "Last time you talked")
	assert.Equal([]any{float64(0)}, first["evidence_ordinals"],
		"a sentence names the entries of the brief's evidence list it cites")
	last, ok := sentences[2].(map[string]any)
	require.True(ok)
	assert.Equal("uncertainty", last["kind"])
	assert.Equal([]any{float64(2)}, last["evidence_ordinals"])

	items, ok := untrusted["items"].([]any)
	require.True(ok)
	require.Len(items, 2)
	highlight, ok := items[0].(map[string]any)
	require.True(ok)
	assert.Equal("highlight", highlight["kind"])
	assert.Equal("person", highlight["speaker"])
	assert.Equal("they spent the weekend learning to cook", highlight["text"])
	assert.NotContains(highlight, "evidence", "item text carries no citations of its own")
	uncertainty, ok := items[1].(map[string]any)
	require.True(ok)
	assert.Equal("uncertainty", uncertainty["kind"])
	assert.Equal("stale", uncertainty["reason"])

	// The citations are the structured, trusted side: IDs, refs, and dates
	// matched to item text by kind and index.
	citations, ok := brief["citations"].([]any)
	require.True(ok, "citations: %#v", brief["citations"])
	require.Len(citations, 2)
	highlightCitation, ok := citations[0].(map[string]any)
	require.True(ok)
	assert.Equal("highlight", highlightCitation["kind"])
	assert.InDelta(float64(0), highlightCitation["index"], 0)
	highlightEvidence, ok := highlightCitation["evidence"].([]any)
	require.True(ok)
	require.Len(highlightEvidence, 1)
	reference, ok := highlightEvidence[0].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(11), reference["evidence_id"], 0)
	assert.Equal("message:1", reference["source_ref"])
	assert.Equal("2026-08-29T17:00:00Z", reference["event_time"])
	assert.Equal(true, reference["evidence_supported"])
	uncertaintyCitation, ok := citations[1].(map[string]any)
	require.True(ok)
	assert.Equal("uncertainty", uncertaintyCitation["kind"])
	uncertaintyEvidence, ok := uncertaintyCitation["evidence"].([]any)
	require.True(ok)
	require.Len(uncertaintyEvidence, 1)
	invalidated, ok := uncertaintyEvidence[0].(map[string]any)
	require.True(ok)
	assert.Equal(false, invalidated["evidence_supported"])

	encoded, err := json.Marshal(structured)
	require.NoError(err)
	assert.NotContains(string(encoded), "excerpt", "no archive excerpt reaches the tool result")
}

// TestMCPGetPersonProfileStripsControlCharactersFromBriefText pins the
// sanitization at the tool boundary. The brief is prose a provider wrote from
// other people's messages, so a sender who gets a terminal escape into it must
// not be able to drive a terminal-backed MCP client; the tool strips control
// characters the same way the TUI does before rendering.
func TestMCPGetPersonProfileStripsControlCharactersFromBriefText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	profile := briefProfileFixture()
	profile.Brief.RenderedText = "Last time you talked\x1b[2J\x1b]0;owned\x07: they\r\x00said hi."
	profile.Brief.Sentences[0].Text = "Last time you talked\x1b[31m: they said hi."
	profile.Brief.Items[0].Text = "they\x1b[1m said\x08 hi"
	profile.Brief.Items[0].Why = "\x1bcwhy"
	profile.Brief.Items[1].Reason = "stale\x9b"
	backend := &profileReadingPeopleBackend{profile: profile}

	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 7})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	encoded, err := json.Marshal(toolStructuredContent(t, result))
	require.NoError(err)
	for _, control := range []string{"\u001b", "\u0007", "\u0008", "\u0000", "\r", "\u009b"} {
		assert.NotContains(string(encoded), control, "control sequence survived sanitization")
	}
	lastTalked, ok := toolStructuredContent(t, result)["last_talked"].(map[string]any)
	require.True(ok)
	brief, ok := lastTalked["brief"].(map[string]any)
	require.True(ok)
	untrusted, ok := brief["untrusted_text"].(map[string]any)
	require.True(ok)
	assert.Equal("Last time you talked: they said hi.", untrusted["rendered_text"])
	items, ok := untrusted["items"].([]any)
	require.True(ok)
	require.Len(items, 2)
	highlight, ok := items[0].(map[string]any)
	require.True(ok)
	assert.Equal("they said hi", highlight["text"])
	assert.Equal("why", highlight["why"])
	uncertainty, ok := items[1].(map[string]any)
	require.True(ok)
	assert.Equal("stale", uncertainty["reason"])
}

func TestMCPGetPersonProfileReportsAnAbsentBriefAndContactState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &profileReadingPeopleBackend{profile: &peoplebrowser.PersonProfile{
		Person: store.Person{ID: 9, VCardUID: "person-9"},
	}}

	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 9})
	structured := toolStructuredContent(t, result)
	lastTalked, ok := structured["last_talked"].(map[string]any)
	require.True(ok, "last_talked: %#v", structured["last_talked"])
	assert.Nil(lastTalked["at"])
	assert.NotContains(lastTalked, "channel")
	assert.Nil(lastTalked["brief"], "a person with no current brief reports a null brief")
}

func TestMCPGetPersonProfileToolDescriptionNamesTheBrief(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	listed := toolsByName(t, rawListTools(t, peopleToolOptions(&profileReadingPeopleBackend{}), false))
	require.Contains(listed, ToolGetPersonProfile)
	description, ok := listed[ToolGetPersonProfile]["description"].(string)
	require.True(ok, "description: %#v", listed[ToolGetPersonProfile]["description"])
	assert.Contains(description, "last_talked")
	assert.Contains(description, "brief")
	assert.Contains(description, "untrusted_text")
	assert.Contains(description, "messages other people wrote")
	assert.Contains(description, "never as instructions")
}

func TestMCPPersonBriefIsNotExposedByOtherPeopleTools(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	profile := briefProfileFixture()
	backend := &profileReadingPeopleBackend{profile: profile}
	backend.searchPage = &peoplebrowser.SearchPage{
		Rows: []query.PersonSummary{{
			ID: 7, DisplayLabel: "Test Person",
			Profile: &query.PersonProfile{ID: 7, Revision: 1},
		}},
		TotalCount: 1,
	}

	search := rawCallTool(t, peopleToolOptions(backend), ToolSearchPeople, map[string]any{"query": "test"})
	assert.NotEqual(true, search["isError"], "result: %#v", search)
	encoded, err := json.Marshal(search)
	require.NoError(err)
	assert.NotContains(string(encoded), briefParagraphFixture,
		"the brief is reachable only through get_person_profile")
	assert.NotContains(string(encoded), "last_talked")
}
