package peoplesweep

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderBriefMatchesTheFrozenRenderedForm(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	assert.Equal("person-brief-render-v1", BriefRendererPolicyV1)

	archive := &briefFakeArchive{
		items: []EvidenceItem{
			briefWindowItem(10, SourceConversationText, 10, "the person wrote this"),
			briefWindowItem(11, SourceConversationText, 12, "the person wrote this too"),
			briefWindowItem(12, SourceConversationText, 29, "the person wrote this later"),
		},
		lastContact:    BriefLastContact{MessageID: 12, SourceID: 2, Channel: "chat"},
		hasLastContact: true,
	}
	request := briefWindowRequest(t)
	request.MaxItems = 10
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)

	self := briefEvidenceID(t, window, 10)
	middle := briefEvidenceID(t, window, 11)
	later := briefEvidenceID(t, window, 12)

	output := briefOutputJSON{
		LastInteraction: fmt.Sprintf(
			`{"evidence_id":%q,"summary":"they were preparing for a role change and spending weekends learning to cook"}`,
			later),
		Highlights: []string{
			briefHighlightJSON("they start the new role in September", BriefSpeakerPerson, "", middle),
			briefHighlightJSON("they were candid about the decision", BriefSpeakerPerson, "", self),
		},
		FollowUps: []string{fmt.Sprintf(
			`{"question":"how the transition went and whether the autumn trip is still happening",`+
				`"why":"the role change was still open","highlight_index":0,"evidence_ids":[%q]}`, middle)},
		Uncertainties: []string{fmt.Sprintf(
			`{"text":"the move to a new city was mentioned in June and may have changed",`+
				`"kind":"stale","evidence_ids":[%q]}`, later)},
	}.raw()

	parsed, err := ParseBrief(output, window, window.Request.Profile)
	require.NoError(err)
	require.Zero(parsed.DroppedItemCount)

	rendered, err := RenderBrief(parsed, window)
	require.NoError(err)
	assert.Equal(BriefRendererPolicyV1, rendered.Policy)
	assert.Equal(
		"Last time you talked (Aug 29, chat): they were preparing for a role change and "+
			"spending weekends learning to cook. "+
			"They said they start the new role in September. "+
			"They said they were candid about the decision. "+
			"You may want to ask how the transition went and whether the autumn trip is still happening. "+
			"Check before assuming: the move to a new city was mentioned in June and may have changed.",
		rendered.Text)
	assert.Equal([]RenderedSentence{
		{Kind: BriefSentenceLastInteraction, Index: 0, Text: "Last time you talked (Aug 29, chat): " +
			"they were preparing for a role change and spending weekends learning to cook."},
		{Kind: BriefSentenceHighlight, Index: 0, Text: "They said they start the new role in September."},
		{Kind: BriefSentenceHighlight, Index: 1, Text: "They said they were candid about the decision."},
		{Kind: BriefSentenceFollowUp, Index: 0, Text: "You may want to ask how the transition went " +
			"and whether the autumn trip is still happening."},
		{Kind: BriefSentenceUncertainty, Index: 0,
			Text: "Check before assuming: the move to a new city was mentioned in June and may have changed."},
	}, rendered.Sentences)
}

// TestRenderBriefRendersEveryFrozenSentenceKind pins the wording of the owner and
// third-party sentences. Ruling R7 keeps those kinds in the frozen v1 schema but
// makes them unreachable through ParseBrief, so the structure is built directly:
// widening the evidence window is a v2 program, and this policy must render what
// it already promised to render.
func TestRenderBriefRendersEveryFrozenSentenceKind(t *testing.T) {
	rendered, err := RenderBrief(ParsedBrief{Output: BriefOutput{
		Highlights: []BriefHighlight{
			{Text: "they start the new role in September", Speaker: BriefSpeakerPerson},
			{Text: "you offered to help with the move", Speaker: BriefSpeakerOwner},
			{Text: "their partner started at a new lab", Speaker: BriefSpeakerOther},
		},
		Appreciations: []BriefAppreciation{{Text: "how candid they were about the decision"}},
	}}, BriefWindow{})
	require.NoError(t, err)
	assert.Equal(t,
		"They said they start the new role in September. "+
			"You said you offered to help with the move. "+
			"Someone else said their partner started at a new lab. "+
			"You said you appreciated how candid they were about the decision.",
		rendered.Text)
	assert.Equal(t, []RenderedSentence{
		{Kind: BriefSentenceHighlight, Index: 0, Text: "They said they start the new role in September."},
		{Kind: BriefSentenceHighlight, Index: 1, Text: "You said you offered to help with the move."},
		{Kind: BriefSentenceHighlight, Index: 2,
			Text: "Someone else said their partner started at a new lab."},
		{Kind: BriefSentenceAppreciation, Index: 0,
			Text: "You said you appreciated how candid they were about the decision."},
	}, rendered.Sentences)
}

func TestRenderBriefOmitsAnUnknownChannelAndKeepsExistingPunctuation(t *testing.T) {
	require := require.New(t)

	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(12, SourceConversationText, 29, "the person wrote this"),
	}}
	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(err)
	later := briefEvidenceID(t, window, 12)

	output := briefOutputJSON{
		LastInteraction: fmt.Sprintf(`{"evidence_id":%q,"summary":"you compared notes."}`, later),
		Uncertainties: []string{fmt.Sprintf(
			`{"text":"is the trip still on?","kind":"ambiguous","evidence_ids":[%q]}`, later)},
	}.raw()
	parsed, err := ParseBrief(output, window, window.Request.Profile)
	require.NoError(err)

	rendered, err := RenderBrief(parsed, window)
	require.NoError(err)
	assert.Equal(t,
		"Last time you talked (Aug 29): you compared notes. "+
			"Check before assuming: is the trip still on?",
		rendered.Text)
}

func TestRenderBriefWithoutALastInteractionStartsAtTheHighlights(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(11, SourceConversationText, 12, "the person wrote this too"),
	}}
	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(err)
	middle := briefEvidenceID(t, window, 11)

	parsed, err := ParseBrief(briefOutputJSON{Highlights: []string{
		briefHighlightJSON("they are hiring again", BriefSpeakerPerson, "", middle),
	}}.raw(), window, window.Request.Profile)
	require.NoError(err)

	rendered, err := RenderBrief(parsed, window)
	require.NoError(err)
	assert.Equal("They said they are hiring again.", rendered.Text)
	require.Len(rendered.Sentences, 1)
	assert.Equal(BriefSentenceHighlight, rendered.Sentences[0].Kind)
}

func TestRenderBriefRefusesAnEmptyBrief(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(11, SourceConversationText, 12, "the person wrote this too"),
	}}
	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(t, err)

	parsed, err := ParseBrief(briefOutputJSON{}.raw(), window, window.Request.Profile)
	require.NoError(t, err)

	_, err = RenderBrief(parsed, window)
	require.ErrorIs(t, err, ErrEmptyBrief)
}

func TestRenderBriefFallsBackToTheCitedItemWhenContactStateIsMissing(t *testing.T) {
	require := require.New(t)

	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "the person wrote this"),
		briefWindowItem(12, SourceConversationText, 29, "the person wrote this later"),
	}}
	request := briefWindowRequest(t)
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	require.False(window.LastContact.Included)

	parsed, err := ParseBrief(briefOutputJSON{LastInteraction: fmt.Sprintf(
		`{"evidence_id":%q,"summary":"you compared notes"}`, briefEvidenceID(t, window, 10)),
	}.raw(), window, window.Request.Profile)
	require.NoError(err)

	rendered, err := RenderBrief(parsed, window)
	require.NoError(err)
	assert.Equal(t, "Last time you talked (Aug 10): you compared notes.", rendered.Text)
}

func TestRenderBriefOmitsTheChannelOfAnExcludedLastContact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	archive := &briefFakeArchive{
		items: []EvidenceItem{
			briefWindowItem(10, SourceConversationText, 10, "I start a new role"),
			briefWindowItem(12, SourceMeetingText, 14, "meeting transcript"),
		},
		lastContact:    BriefLastContact{MessageID: 12, SourceID: 2, Channel: "meeting"},
		hasLastContact: true,
	}
	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(err)
	require.False(window.LastContact.Included)
	parsed, err := ParseBrief(briefOutputJSON{LastInteraction: fmt.Sprintf(
		`{"evidence_id":%q,"summary":"they start a new role"}`, briefEvidenceID(t, window, 10)),
	}.raw(), window, window.Request.Profile)
	require.NoError(err)
	rendered, err := RenderBrief(parsed, window)
	require.NoError(err)
	assert.Equal("Last time you talked (Aug 10): they start a new role.", rendered.Text)
}

// briefCitationOutput parses one brief that cites three archive items from
// every structured kind, including an appreciation that ruling R7 drops and a
// proposed attribute that cites an item a sentence already cited.
func briefCitationOutput(t *testing.T) ParsedBrief {
	t.Helper()
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)
	middle := briefEvidenceID(t, window, 11)
	later := briefEvidenceID(t, window, 12)
	target := window.Batch.Packet.Catalog.Targets[0].Key

	output := briefOutputJSON{
		LastInteraction: fmt.Sprintf(`{"evidence_id":%q,"summary":"caught up on the move"}`, later),
		Highlights: []string{
			briefHighlightJSON("they are mid-move", BriefSpeakerPerson, "2026-08-12", middle),
			briefHighlightJSON("they packed the kitchen first", BriefSpeakerPerson, "", self),
		},
		FollowUps: []string{fmt.Sprintf(
			`{"question":"how did the move go","why":"they were mid-move",`+
				`"highlight_index":0,"evidence_ids":[%q]}`, middle)},
		Appreciations: []string{fmt.Sprintf(
			`{"text":"how candid they were about the move","evidence_ids":[%q]}`, self)},
		Uncertainties: []string{fmt.Sprintf(
			`{"text":"the new city may have changed","kind":"stale","evidence_ids":[%q]}`, later)},
		Attributes: []string{fmt.Sprintf(
			`{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],`+
				`"valid_from":null,"valid_until":null,"confidence_basis_points":900}`, target, self)},
	}.raw()

	parsed, err := ParseBrief(output, window, window.Request.Profile)
	require.NoError(t, err)
	require.Equal(t, 1, parsed.DroppedItemCount, "the appreciation is dropped by ruling R7")
	return parsed
}

// TestBriefCitationOrdinalsMapsEachItemToItsStoredPointers pins the join key the
// API emits: the ordinals are positions in ParseBrief's citation order, which is
// exactly the order the store writes person_brief_evidence rows in.
func TestBriefCitationOrdinalsMapsEachItemToItsStoredPointers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	parsed := briefCitationOutput(t)

	refs := make([]int64, 0, len(parsed.Evidence))
	for _, evidence := range parsed.Evidence {
		ref, err := DecodePersonSweepEvidenceRef(evidence.SourceRef)
		require.NoError(err)
		refs = append(refs, ref.MessageID)
	}
	require.Equal([]int64{12, 11, 10}, refs,
		"pointer ordinals are positions in this list")

	ordinals, ok := BriefCitationOrdinals(parsed.Output, len(parsed.Evidence))
	require.True(ok)
	assert.Equal(map[BriefSentenceKey][]int{
		{Kind: BriefSentenceLastInteraction, Index: 0}: {0},
		{Kind: BriefSentenceHighlight, Index: 0}:       {1},
		{Kind: BriefSentenceHighlight, Index: 1}:       {2},
		{Kind: BriefSentenceFollowUp, Index: 0}:        {1},
		{Kind: BriefSentenceUncertainty, Index: 0}:     {0},
	}, ordinals, "a dropped appreciation takes no ordinal and an attribute takes no sentence")

	rendered, err := RenderBrief(parsed, BriefWindow{})
	require.NoError(err)
	for _, sentence := range rendered.Sentences {
		assert.Contains(ordinals, BriefSentenceKey{Kind: sentence.Kind, Index: sentence.Index},
			"every rendered sentence has a citation entry")
	}
}

func TestBriefCitationOrdinalsFailsClosedOnAPointerCountMismatch(t *testing.T) {
	assert := assert.New(t)

	parsed := briefCitationOutput(t)

	ordinals, ok := BriefCitationOrdinals(parsed.Output, len(parsed.Evidence)-1)
	assert.False(ok, "a dropped pointer makes the walk unusable")
	assert.Nil(ordinals)

	ordinals, ok = BriefCitationOrdinals(parsed.Output, len(parsed.Evidence)+1)
	assert.False(ok)
	assert.Nil(ordinals)

	ordinals, ok = BriefCitationOrdinals(BriefOutput{}, 0)
	assert.True(ok, "a brief that cites nothing maps nothing and still aligns")
	assert.Empty(ordinals)
}
