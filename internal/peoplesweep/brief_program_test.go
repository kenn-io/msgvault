package peoplesweep

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBriefProgramFingerprintStable(t *testing.T) {
	checks := assert.New(t)
	checks.Equal("msgvault-person-brief", BriefProgramID)
	checks.Equal("v1", BriefProgramVersion)
	checks.Equal("msgvault_person_brief_v1", BriefSchemaName)
	checks.Equal(2048, briefMaxOutputTokens)
	checks.Equal("6beb307435ba3ad1f068aa32c2e8a530ee98dae2cb97b7a58c716b3add1806f5", BriefProgramFingerprint())
	checks.NotEqual(ProgramFingerprint(), BriefProgramFingerprint())
}

func TestBriefJSONSchemaIsClosedAndCapped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	raw := BriefJSONSchema()
	require.NotEmpty(raw)

	var schema map[string]any
	require.NoError(json.Unmarshal(raw, &schema))
	assert.Equal(false, schema["additionalProperties"])
	properties, ok := schema["properties"].(map[string]any)
	require.True(ok)
	require.ElementsMatch(
		[]string{"last_meaningful_interaction", "highlights", "follow_ups", "appreciations",
			"uncertainties", "possible_attributes"},
		mapKeys(properties))
	require.ElementsMatch(
		[]string{"last_meaningful_interaction", "highlights", "follow_ups", "appreciations",
			"uncertainties", "possible_attributes"},
		anySliceToStrings(t, schema["required"]))

	for property, wantMax := range map[string]int{
		"highlights": 8, "follow_ups": 5, "appreciations": 3, "uncertainties": 5,
		"possible_attributes": 32,
	} {
		array, arrayOK := properties[property].(map[string]any)
		require.True(arrayOK, property)
		assert.Equal("array", array["type"], property)
		maxItems, numberOK := array["maxItems"].(float64)
		require.True(numberOK, property)
		assert.Equal(wantMax, int(maxItems), property)
	}

	// possible_attributes reuses the frozen extraction claim item schema.
	attributes, ok := properties["possible_attributes"].(map[string]any)
	require.True(ok)
	attributeItems, err := json.Marshal(attributes["items"])
	require.NoError(err)
	var extractionClaims map[string]any
	require.NoError(json.Unmarshal(ExtractionJSONSchema(), &extractionClaims))
	extractionProperties, ok := extractionClaims["properties"].(map[string]any)
	require.True(ok)
	extractionArray, ok := extractionProperties["claims"].(map[string]any)
	require.True(ok)
	extractionItems, err := json.Marshal(extractionArray["items"])
	require.NoError(err)
	assert.JSONEq(string(extractionItems), string(attributeItems))
}

func TestBriefJSONSchemaValidatesFrozenShapes(t *testing.T) {
	// require is scoped to this block: the subtests below need their own,
	// bound to each subtest's own *testing.T, and a package name shadowed in
	// an outer scope cannot be un-shadowed by a nested one.
	var schema jsonschema.Schema
	var resolved *jsonschema.Resolved
	{
		require := require.New(t)
		require.NoError(json.Unmarshal(BriefJSONSchema(), &schema))
		var err error
		resolved, err = schema.Resolve(nil)
		require.NoError(err)

		valid := `{"last_meaningful_interaction":{"evidence_id":"evidence:one","summary":"caught up on the move"},
			"highlights":[{"text":"they start a new role in September","speaker":"person",
				"evidence_ids":["evidence:one"],"observed_at":"2026-08-29","confidence_basis_points":900}],
			"follow_ups":[{"question":"how did the transition go","why":"they were mid-move",
				"highlight_index":0,"evidence_ids":["evidence:one"]}],
			"appreciations":[{"text":"how candid they were","evidence_ids":["evidence:one"]}],
			"uncertainties":[{"text":"the move may have changed","kind":"stale",
				"evidence_ids":["evidence:one"]}],
			"possible_attributes":[]}`
		var decoded any
		require.NoError(json.Unmarshal([]byte(valid), &decoded))
		require.NoError(resolved.Validate(decoded))
	}

	for name, invalid := range map[string]string{
		"unknown speaker": `{"last_meaningful_interaction":null,"highlights":[{"text":"x","speaker":"vendor",
			"evidence_ids":["evidence:one"],"observed_at":null,"confidence_basis_points":0}],
			"follow_ups":[],"appreciations":[],"uncertainties":[],"possible_attributes":[]}`,
		"unknown uncertainty kind": `{"last_meaningful_interaction":null,"highlights":[],"follow_ups":[],
			"appreciations":[],"uncertainties":[{"text":"x","kind":"vibes","evidence_ids":["evidence:one"]}],
			"possible_attributes":[]}`,
		"extra property": `{"last_meaningful_interaction":null,"highlights":[],"follow_ups":[],
			"appreciations":[],"uncertainties":[],"possible_attributes":[],"notes":"x"}`,
		"missing key": `{"highlights":[],"follow_ups":[],"appreciations":[],"uncertainties":[],
			"possible_attributes":[]}`,
		"confidence out of range": `{"last_meaningful_interaction":null,
			"highlights":[{"text":"x","speaker":"person","evidence_ids":["evidence:one"],
				"observed_at":null,"confidence_basis_points":1001}],
			"follow_ups":[],"appreciations":[],"uncertainties":[],"possible_attributes":[]}`,
		"empty evidence": `{"last_meaningful_interaction":null,
			"highlights":[{"text":"x","speaker":"person","evidence_ids":[],
				"observed_at":null,"confidence_basis_points":10}],
			"follow_ups":[],"appreciations":[],"uncertainties":[],"possible_attributes":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			var candidate any
			require.NoError(json.Unmarshal([]byte(invalid), &candidate))
			assert.Error(t, resolved.Validate(candidate))
		})
	}
}

// TestBriefOutputSerializesEmptyKindsAsArrays pins the durable shape of
// structured_json: a brief where a kind ended empty must round-trip through the
// frozen schema, which requires arrays rather than null.
func TestBriefOutputSerializesEmptyKindsAsArrays(t *testing.T) {
	require := require.New(t)

	window := briefParseWindow(t)
	parsed, err := ParseBrief(briefOutputJSON{}.raw(), window, window.Request.Profile)
	require.NoError(err)

	encoded, err := json.Marshal(parsed.Output)
	require.NoError(err)
	assert.JSONEq(t,
		`{"last_meaningful_interaction":null,"highlights":[],"follow_ups":[],`+
			`"appreciations":[],"uncertainties":[],"possible_attributes":[]}`,
		string(encoded))

	var schema jsonschema.Schema
	require.NoError(json.Unmarshal(BriefJSONSchema(), &schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(err)
	var decoded any
	require.NoError(json.Unmarshal(encoded, &decoded))
	require.NoError(resolved.Validate(decoded),
		"stored structured_json must re-validate against the frozen schema")
}

func TestBriefStructuredRequestBindsFrozenProgram(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	packet := briefTestPacket(t)
	packetJSON, err := marshalPacketEnvelope(packet)
	require.NoError(err)

	request := briefStructuredRequest(packet, packetJSON, 0)
	assert.Equal(BriefProgramID, request.ProgramID)
	assert.Equal(BriefProgramVersion, request.ProgramVersion)
	assert.Equal(BriefSchemaName, request.SchemaName)
	assert.Equal(briefMaxOutputTokens, request.MaxOutputTokens)
	// An operator may only lower the frozen cap, never raise it.
	assert.Equal(512, briefStructuredRequest(packet, packetJSON, 512).MaxOutputTokens)
	assert.Equal(briefMaxOutputTokens,
		briefStructuredRequest(packet, packetJSON, briefMaxOutputTokens+1).MaxOutputTokens)
	assert.True(request.ContainsSensitive)
	assert.JSONEq(string(BriefJSONSchema()), string(request.JSONSchema))
	assert.Equal(briefProgramText+"\n\nEvidence packet JSON:\n"+string(packetJSON), request.InputText)
	assert.NotEmpty(request.Sources)

	_, err = validateStructuredRequest(request, false)
	require.NoError(err)
}

func briefTestPacket(t *testing.T) EvidencePacket {
	t.Helper()
	packet := packetTestPacket()
	packet.ProgramID = BriefProgramID
	packet.ProgramVersion = BriefProgramVersion
	packet.Seeds = append(packet.Seeds, packet.Context...)
	packet.Context = nil
	canonical, err := canonicalPacket(packet)
	require.NoError(t, err)
	return canonical
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func anySliceToStrings(t *testing.T, value any) []string {
	t.Helper()
	raw, ok := value.([]any)
	require.True(t, ok)
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		text, textOK := entry.(string)
		require.True(t, textOK)
		out = append(out, text)
	}
	return out
}
