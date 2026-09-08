package peoplesweep

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

// briefParseWindow builds a real window over three items:
//
//	self    conversation_text, direct-self, 2026-08-10
//	middle  conversation_text, direct-self, 2026-08-12
//	later   conversation_text, direct-self, 2026-08-14
func briefParseWindow(t *testing.T) BriefWindow {
	t.Helper()
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "the person wrote this"),
		briefWindowItem(11, SourceConversationText, 12, "the person wrote this too"),
		briefWindowItem(12, SourceConversationText, 14, "the person wrote this later"),
	}}
	request := briefWindowRequest(t)
	request.MaxItems = 10
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(t, err)
	require.Len(t, window.Items, 3)
	return window
}

func briefEvidenceID(t *testing.T, window BriefWindow, messageID int64) string {
	t.Helper()
	for _, item := range window.Items {
		if item.Ref.MessageID == messageID {
			return packetEvidenceID(item)
		}
	}
	require.FailNow(t, "window has no item for message", "%d", messageID)
	return ""
}

type briefOutputJSON struct {
	LastInteraction string
	Highlights      []string
	FollowUps       []string
	Appreciations   []string
	Uncertainties   []string
	Attributes      []string
}

func (b briefOutputJSON) raw() json.RawMessage {
	interaction := "null"
	if b.LastInteraction != "" {
		interaction = b.LastInteraction
	}
	return json.RawMessage(fmt.Sprintf(
		`{"last_meaningful_interaction":%s,"highlights":[%s],"follow_ups":[%s],`+
			`"appreciations":[%s],"uncertainties":[%s],"possible_attributes":[%s]}`,
		interaction, strings.Join(b.Highlights, ","), strings.Join(b.FollowUps, ","),
		strings.Join(b.Appreciations, ","), strings.Join(b.Uncertainties, ","),
		strings.Join(b.Attributes, ",")))
}

func briefHighlightJSON(text string, speaker BriefSpeaker, observedAt string, ids ...string) string {
	observed := "null"
	if observedAt != "" {
		observed = `"` + observedAt + `"`
	}
	return fmt.Sprintf(
		`{"text":%q,"speaker":%q,"evidence_ids":[%s],"observed_at":%s,"confidence_basis_points":800}`,
		text, string(speaker), quotedList(ids), observed)
}

func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, `"`+value+`"`)
	}
	return strings.Join(quoted, ",")
}

func TestParseBriefKeepsAWellEvidencedBrief(t *testing.T) {
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
			`{"question":"how did the move go","why":"they were mid-move","highlight_index":0,"evidence_ids":[%q]}`,
			middle)},
		Uncertainties: []string{fmt.Sprintf(
			`{"text":"the new city may have changed","kind":"stale","evidence_ids":[%q]}`, later)},
		Attributes: []string{fmt.Sprintf(
			`{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],`+
				`"valid_from":null,"valid_until":null,"confidence_basis_points":900}`, target, later)},
	}.raw()

	require := require.New(t)
	assert := assert.New(t)

	parsed, err := ParseBrief(output, window, window.Request.Profile)
	require.NoError(err)

	assert.Zero(parsed.DroppedItemCount)
	require.NotNil(parsed.Output.LastMeaningfulInteraction)
	assert.Equal(later, parsed.Output.LastMeaningfulInteraction.EvidenceID)
	assert.Len(parsed.Output.Highlights, 2)
	assert.Len(parsed.Output.FollowUps, 1)
	assert.Empty(parsed.Output.Appreciations)
	assert.Len(parsed.Output.Uncertainties, 1)

	require.Len(parsed.Claims, 1)
	assert.Equal(personfacts.OriginBrief, parsed.Claims[0].Origin)
	assert.Equal(personfacts.RelationSupport, parsed.Claims[0].Relation)
	assert.JSONEq(`"ramen"`, string(parsed.Claims[0].SubmittedValue))

	// Evidence follows citation order and is deduplicated: the last interaction
	// cites `later` first, then the highlights, follow-up, and uncertainty.
	refs := make([]string, 0, len(parsed.Evidence))
	for _, evidence := range parsed.Evidence {
		ref, err := DecodePersonSweepEvidenceRef(evidence.SourceRef)
		require.NoError(err)
		refs = append(refs, strconv.FormatInt(ref.MessageID, 10))
	}
	assert.Equal([]string{"12", "11", "10"}, refs)
}

func TestParseBriefDropsItemsCitingUnknownEvidence(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)
	unknown := "evidence:" + strings.Repeat("0", 64)

	output := briefOutputJSON{
		LastInteraction: fmt.Sprintf(`{"evidence_id":%q,"summary":"invented"}`, unknown),
		Highlights: []string{
			briefHighlightJSON("kept", BriefSpeakerPerson, "", self),
			briefHighlightJSON("invented", BriefSpeakerPerson, "", unknown),
			briefHighlightJSON("half invented", BriefSpeakerPerson, "", self, unknown),
		},
		FollowUps: []string{fmt.Sprintf(
			`{"question":"invented","why":"invented","highlight_index":0,"evidence_ids":[%q]}`, unknown)},
		Appreciations: []string{fmt.Sprintf(`{"text":"invented","evidence_ids":[%q]}`, unknown)},
		Uncertainties: []string{fmt.Sprintf(
			`{"text":"invented","kind":"ambiguous","evidence_ids":[%q]}`, unknown)},
	}.raw()

	require := require.New(t)
	assert := assert.New(t)

	parsed, err := ParseBrief(output, window, window.Request.Profile)
	require.NoError(err)

	assert.Nil(parsed.Output.LastMeaningfulInteraction)
	require.Len(parsed.Output.Highlights, 1)
	assert.Equal("kept", parsed.Output.Highlights[0].Text)
	assert.Empty(parsed.Output.FollowUps)
	assert.Empty(parsed.Output.Appreciations)
	assert.Empty(parsed.Output.Uncertainties)
	assert.Equal(6, parsed.DroppedItemCount)
}

// TestParseBriefEnforcesSpeakerGates pins ruling R7: a v1 packet holds only the
// person's own messages, so a person highlight is supported by any packet item,
// an owner highlight can never be evidenced, and a third-party highlight needs a
// subject that is not the person, which no packet item can carry today.
func TestParseBriefEnforcesSpeakerGates(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)
	middle := briefEvidenceID(t, window, 11)

	for name, testCase := range map[string]struct {
		highlight string
		kept      bool
	}{
		"person cites a chat message":   {briefHighlightJSON("kept", BriefSpeakerPerson, "", self), true},
		"person cites a second message": {briefHighlightJSON("kept", BriefSpeakerPerson, "", middle), true},
		"person cites both":             {briefHighlightJSON("kept", BriefSpeakerPerson, "", self, middle), true},
		"owner cites a chat message":    {briefHighlightJSON("dropped", BriefSpeakerOwner, "", self), false},
		"owner cites a second message":  {briefHighlightJSON("dropped", BriefSpeakerOwner, "", middle), false},
		"third party cites the person":  {briefHighlightJSON("dropped", BriefSpeakerOther, "", self), false},
		"third party cites a second message": {
			briefHighlightJSON("dropped", BriefSpeakerOther, "", middle), false},
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			output := briefOutputJSON{Highlights: []string{testCase.highlight}}.raw()
			parsed, err := ParseBrief(output, window, window.Request.Profile)
			require.NoError(err)
			if testCase.kept {
				require.Len(parsed.Output.Highlights, 1)
				assert.Zero(parsed.DroppedItemCount)
				return
			}
			assert.Empty(parsed.Output.Highlights)
			assert.Equal(1, parsed.DroppedItemCount)
		})
	}
}

// TestParseBriefDropsEveryAppreciation pins ruling R7: an appreciation is
// something the owner said, and no owner-authored evidence can reach a v1
// packet, so every appreciation is unevidenced regardless of what it cites.
func TestParseBriefDropsEveryAppreciation(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)
	middle := briefEvidenceID(t, window, 11)

	for name, cited := range map[string]string{
		"cites a chat message":   self,
		"cites a second message": middle,
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := ParseBrief(briefOutputJSON{Appreciations: []string{
				fmt.Sprintf(`{"text":"how candid they were","evidence_ids":[%q]}`, cited),
			}}.raw(), window, window.Request.Profile)
			require.NoError(t, err)
			assert.Empty(t, parsed.Output.Appreciations)
			assert.Equal(t, 1, parsed.DroppedItemCount)
		})
	}
}

func TestParseBriefReindexesFollowUpsAfterHighlightDrops(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)
	unknown := "evidence:" + strings.Repeat("0", 64)

	output := briefOutputJSON{
		Highlights: []string{
			briefHighlightJSON("dropped", BriefSpeakerPerson, "", unknown),
			briefHighlightJSON("kept", BriefSpeakerPerson, "", self),
		},
		FollowUps: []string{
			fmt.Sprintf(`{"question":"about the kept highlight","why":"it is open",`+
				`"highlight_index":1,"evidence_ids":[%q]}`, self),
			fmt.Sprintf(`{"question":"about the dropped highlight","why":"it is gone",`+
				`"highlight_index":0,"evidence_ids":[%q]}`, self),
		},
	}.raw()

	require := require.New(t)
	assert := assert.New(t)

	parsed, err := ParseBrief(output, window, window.Request.Profile)
	require.NoError(err)
	require.Len(parsed.Output.Highlights, 1)
	require.Len(parsed.Output.FollowUps, 1)
	assert.Equal("about the kept highlight", parsed.Output.FollowUps[0].Question)
	assert.Equal(0, parsed.Output.FollowUps[0].HighlightIndex,
		"a kept follow-up is re-indexed onto the kept highlight")
	assert.Equal(2, parsed.DroppedItemCount)
}

func TestParseBriefDropsObservationsOutsideTheWindow(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)

	for name, testCase := range map[string]struct {
		observedAt string
		kept       bool
	}{
		"first day of the window": {"2026-08-10", true},
		"last day of the window":  {"2026-08-14", true},
		"before the window":       {"2026-08-09", false},
		"after the window":        {"2026-08-15", false},
	} {
		t.Run(name, func(t *testing.T) {
			output := briefOutputJSON{Highlights: []string{
				briefHighlightJSON("observed", BriefSpeakerPerson, testCase.observedAt, self),
			}}.raw()
			parsed, err := ParseBrief(output, window, window.Request.Profile)
			require.NoError(t, err)
			assert.Len(t, parsed.Output.Highlights, map[bool]int{true: 1, false: 0}[testCase.kept])
		})
	}
}

func TestParseBriefTreatsSchemaViolationsAsValidationFailures(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)

	for name, output := range map[string]string{
		"unknown speaker": `{"last_meaningful_interaction":null,"highlights":[{"text":"x",` +
			`"speaker":"vendor","evidence_ids":["` + self + `"],"observed_at":null,` +
			`"confidence_basis_points":0}],"follow_ups":[],"appreciations":[],` +
			`"uncertainties":[],"possible_attributes":[]}`,
		"missing key": `{"highlights":[],"follow_ups":[],"appreciations":[],"uncertainties":[],"possible_attributes":[]}`,
		"trailing json": `{"last_meaningful_interaction":null,"highlights":[],"follow_ups":[],` +
			`"appreciations":[],"uncertainties":[],"possible_attributes":[]} {}`,
		"not json": `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBrief(json.RawMessage(output), window, window.Request.Profile)
			require.Error(t, err)
		})
	}
}

func TestParseBriefRejectsUnboundBatchesAndBadAttributes(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)
	empty := briefOutputJSON{}.raw()

	require := require.New(t)

	unbound := window
	unbound.Batch.InputHash = strings.Repeat("f", 64)
	_, err := ParseBrief(empty, unbound, window.Request.Profile)
	require.ErrorContains(err, "not bound to its provider request")

	extraction := window
	extraction.Batch.Request = extractionStructuredRequest(window.Batch.Packet, []byte(`{}`))
	_, err = ParseBrief(empty, extraction, window.Request.Profile)
	require.Error(err)

	unknownTarget := briefOutputJSON{Attributes: []string{fmt.Sprintf(
		`{"target_key":"target:invented","relation":"support","value":"x","evidence_ids":[%q],`+
			`"valid_from":null,"valid_until":null,"confidence_basis_points":900}`, self)}}.raw()
	_, err = ParseBrief(unknownTarget, window, window.Request.Profile)
	require.ErrorContains(err, "unknown target")

	unknownEvidence := briefOutputJSON{Attributes: []string{fmt.Sprintf(
		`{"target_key":%q,"relation":"support","value":"x","evidence_ids":[%q],`+
			`"valid_from":null,"valid_until":null,"confidence_basis_points":900}`,
		window.Batch.Packet.Catalog.Targets[0].Key, "evidence:"+strings.Repeat("0", 64))}}.raw()
	_, err = ParseBrief(unknownEvidence, window, window.Request.Profile)
	require.ErrorContains(err, "unknown evidence")
}

func TestParseBriefRejectsSensitiveAttributesForADisallowingProfile(t *testing.T) {
	archive := &briefFakeArchive{items: []EvidenceItem{
		briefWindowItem(10, SourceConversationText, 10, "the person wrote this"),
	}}
	request := briefWindowRequest(t)
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(t, err)
	sensitive := ""
	for _, target := range window.Batch.Packet.Catalog.Targets {
		if target.Sensitive {
			sensitive = target.Key
		}
	}
	require.NotEmpty(t, sensitive)

	output := briefOutputJSON{Attributes: []string{fmt.Sprintf(
		`{"target_key":%q,"relation":"support","value":"x","evidence_ids":[%q],`+
			`"valid_from":null,"valid_until":null,"confidence_basis_points":900}`,
		sensitive, packetEvidenceID(window.Items[0]))}}.raw()

	strict := request.Profile
	strict.AllowSensitive = false
	_, err = ParseBrief(output, window, strict)
	require.ErrorContains(t, err, "policy-disabled sensitive target")
}

// TestParseBriefKeepsEvidenceCitedOnlyByAProposedAttribute covers the case the
// structured loops cannot: an attribute is the only thing citing an item, so
// without an explicit keep the stored brief would carry a claim whose evidence
// has no pointer back to the archive.
func TestParseBriefKeepsEvidenceCitedOnlyByAProposedAttribute(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)
	target := window.Batch.Packet.Catalog.Targets[0].Key

	output := briefOutputJSON{Attributes: []string{fmt.Sprintf(
		`{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],`+
			`"valid_from":null,"valid_until":null,"confidence_basis_points":900}`,
		target, self)}}.raw()

	require := require.New(t)
	assert := assert.New(t)

	parsed, err := ParseBrief(output, window, window.Request.Profile)
	require.NoError(err)

	assert.Zero(parsed.DroppedItemCount)
	require.Len(parsed.Claims, 1)
	assert.Equal(personfacts.OriginBrief, parsed.Claims[0].Origin)
	require.Len(parsed.Evidence, 1,
		"an attribute-only citation still needs an evidence pointer")
	ref, err := DecodePersonSweepEvidenceRef(parsed.Evidence[0].SourceRef)
	require.NoError(err)
	assert.Equal(int64(10), ref.MessageID)
	assert.Equal(parsed.Claims[0].Evidence[0].SourceRef, parsed.Evidence[0].SourceRef)
}

// TestParseBriefOrdersAttributeEvidenceAfterTheStructuredItems keeps the
// citation order a reader expands sentences in: everything the paragraph cites
// first, then anything only a proposed attribute cites.
func TestParseBriefOrdersAttributeEvidenceAfterTheStructuredItems(t *testing.T) {
	window := briefParseWindow(t)
	self := briefEvidenceID(t, window, 10)
	later := briefEvidenceID(t, window, 12)
	target := window.Batch.Packet.Catalog.Targets[0].Key

	output := briefOutputJSON{
		LastInteraction: fmt.Sprintf(`{"evidence_id":%q,"summary":"caught up on the move"}`, later),
		Attributes: []string{fmt.Sprintf(
			`{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],`+
				`"valid_from":null,"valid_until":null,"confidence_basis_points":900}`,
			target, self)},
	}.raw()

	parsed, err := ParseBrief(output, window, window.Request.Profile)
	require.NoError(t, err)

	refs := make([]string, 0, len(parsed.Evidence))
	for _, evidence := range parsed.Evidence {
		ref, refErr := DecodePersonSweepEvidenceRef(evidence.SourceRef)
		require.NoError(t, refErr)
		refs = append(refs, strconv.FormatInt(ref.MessageID, 10))
	}
	assert.Equal(t, []string{"12", "10"}, refs)
}
