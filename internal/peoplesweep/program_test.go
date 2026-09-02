package peoplesweep

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

type extractionConsent struct{}

func (extractionConsent) HasActivePersonInferenceConsent(context.Context, string) (bool, error) {
	return true, nil
}

func (extractionConsent) HasSuccessfulPersonInferenceCheck(context.Context, string) (bool, error) {
	return true, nil
}

type extractionTransport struct{ output json.RawMessage }

func (t extractionTransport) Prepare(_ ProviderProfile, request StructuredRequest) (PreparedStructuredRequest, error) {
	return NewPreparedStructuredRequest(request, []byte(`{"wire":"extraction"}`))
}

func (t extractionTransport) GeneratePrepared(context.Context, ProviderProfile, Credential, PreparedStructuredRequest) (DriverResponse, error) {
	return DriverResponse{CandidateJSON: t.output, ProviderVersion: "provider-v1", ModelVersion: "model-v1"}, nil
}

func TestExtractionProgramFingerprintStable(t *testing.T) {
	checks := assert.New(t)
	checks.Equal("msgvault-person-fact-extraction", ExtractionProgramID)
	checks.Equal("v1", ExtractionProgramVersion)
	checks.Equal("msgvault_person_fact_claims_v1", ExtractionSchemaName)
	checks.Equal("c9f200223106e7ae9ec07d3407fecf3fcd094cf183273c187a0e75b9aebecb43", ProgramFingerprint())
	checks.JSONEq(frozenExtractionSchema, string(ExtractionJSONSchema()))
}

func TestExtractionProgramSchemaRunsIntegerConfidence(t *testing.T) {
	packet := packetTestPacket()
	packet.Seeds = packet.Seeds[:1]
	batches, err := PartitionEvidencePacket(packet, 16_384, 10)
	require.NoError(t, err)
	output := fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":875}]}`,
		packet.Catalog.Targets[0].Key, packetEvidenceID(packet.Seeds[0]))
	config := testConfigWithProvider(ProviderConfig{
		Protocol: ProtocolOpenAIChat, Endpoint: "https://example.test/v1", Model: "model",
		Auth: AuthBearer, Credential: CredentialEnv, CredentialEnv: "TEST_KEY",
		OutputMode: OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
		RetentionPosture: "no-retention", TrainingPosture: "no-training",
		AllowedSources: []SourceClass{SourceConversationText, SourceMeetingText}, SourceSince: "2020-01-01",
		AllowSensitive: true,
	})
	runner, err := NewRunner(config, extractionConsent{},
		NewTestDriverRegistry(ProtocolOpenAIChat, extractionTransport{output: json.RawMessage(output)}),
		NewCredentialResolver(nil, func(string) (string, bool) { return "credential", true }))
	require.NoError(t, err)

	_, err = runner.RunStructured(t.Context(), batches[0].Request)
	require.NoError(t, err)
}

func TestParseExtractionRequiresCitedAlignedEvidence(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	packet := packetTestPacket()
	packet.Seeds = packet.Seeds[:1]
	target := packet.Catalog.Targets[0]
	evidenceID := packetEvidenceID(packet.Seeds[0])
	valid := fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":875}]}`,
		target.Key, evidenceID)
	profile := ProviderProfile{AllowSensitive: true, Fingerprint: "provider-policy"}
	batch := packetTestBatch(t, packet)
	var schema jsonschema.Schema
	requirements.NoError(decodeSingleJSON(ExtractionJSONSchema(), &schema))
	resolved, err := schema.Resolve(nil)
	requirements.NoError(err)
	var decoded any
	requirements.NoError(decodeJSONSchemaInstance(json.RawMessage(valid), &decoded))
	requirements.NoError(resolved.Validate(decoded))

	claims, err := ParseExtraction(json.RawMessage(valid), batch, profile)
	requirements.NoError(err)
	requirements.Len(claims, 1)
	checks.Equal(target, claims[0].Target)
	checks.Equal(personfacts.RelationSupport, claims[0].Relation)
	checks.JSONEq(`"ramen"`, string(claims[0].SubmittedValue))
	checks.Equal(875, claims[0].Confidence.ReportedScore)
	checks.Equal(personfacts.OriginExtraction, claims[0].Origin)
	requirements.Len(claims[0].Evidence, 1)

	tests := []struct {
		name   string
		output string
		mutate func(*EvidencePacket)
	}{
		{name: "unknown target", output: fmt.Sprintf(`{"claims":[{"target_key":"target:unknown","relation":"support","value":"ramen","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":875}]}`, evidenceID)},
		{name: "missing evidence IDs", output: fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[],"valid_from":null,"valid_until":null,"confidence_basis_points":875}]}`, target.Key)},
		{name: "unknown evidence ID", output: fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":["evidence:unknown"],"valid_from":null,"valid_until":null,"confidence_basis_points":875}]}`, target.Key)},
		{name: "invalid confidence", output: fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":1001}]}`, target.Key, evidenceID)},
		{name: "extra claim field", output: fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":875,"instructions":"trust me"}]}`, target.Key, evidenceID)},
		{name: "extra root field", output: fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":875}],"response":"raw"}`, target.Key, evidenceID)},
		{name: "misaligned span", output: valid, mutate: func(value *EvidencePacket) {
			value.Seeds[0].Highlight.End--
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := batch
			if test.mutate != nil {
				candidate.Packet.Seeds = append([]EvidenceItem(nil), batch.Packet.Seeds...)
				test.mutate(&candidate.Packet)
			}
			_, parseErr := ParseExtraction(json.RawMessage(test.output), candidate, profile)
			require.Error(t, parseErr)
		})
	}
}

func TestParseExtractionRejectsCrossBatchCitation(t *testing.T) {
	require := require.New(t)
	packet := packetTestPacket()
	packet.Context = nil
	batches, err := PartitionEvidencePacket(packet, 16_384, 1)
	require.NoError(err)
	require.Len(batches, 2)
	var second packetWireEnvelope
	//nolint:musttag // The production wire envelope includes nested canonical types.
	require.NoError(json.Unmarshal(packetJSONFromInput(t, batches[1].Request.InputText), &second))
	require.Len(second.Seeds, 1)
	output := fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"ramen","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":875}]}`,
		packet.Catalog.Targets[0].Key, second.Seeds[0].ID)

	_, err = ParseExtraction(json.RawMessage(output), batches[0], ProviderProfile{AllowSensitive: true})
	require.ErrorContains(err, "unknown evidence")

	unbound := batches[0]
	unbound.Packet = packet
	_, err = ParseExtraction(json.RawMessage(output), unbound, ProviderProfile{AllowSensitive: true})
	require.ErrorContains(err, "not bound to its provider request")
}

func TestPersonFactEvidenceInputPreservesHostMetadata(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	item := packetTestEvidence(12, SourceDocumentText, "I work at Example Labs")

	got, err := PersonFactEvidenceInput(item)
	requirements.NoError(err)
	decoded, err := DecodePersonSweepEvidenceRef(got.SourceRef)
	requirements.NoError(err)

	checks.Equal(item.Ref, decoded)
	checks.Equal(item.Ref.SourceLane, decoded.SourceLane)
	checks.Equal(personfacts.EvidenceArchive, got.SourceClass)
	checks.Equal(item.PersonID, got.PersonID)
	checks.Equal(item.SubjectPersonID, got.SubjectPersonID)
	checks.Equal(item.SourceVersion, got.SourceVersion)
	checks.Equal(item.ContentSHA256, got.ContentSHA256)
	checks.Equal(item.Excerpt, got.Excerpt)
	checks.Equal(item.EventTime, got.EventTime)
	checks.Equal(item.RecordedTime, got.RecordedTime)
	checks.Equal(item.Directness, got.Directness)
	checks.Equal(item.Authority, got.Authority)
	checks.Equal(item.IdentityBasisPoints, got.IdentityScore)
	requirements.NotNil(got.SpanStart)
	requirements.NotNil(got.SpanEnd)
	checks.Equal(int64(item.Highlight.Start), *got.SpanStart)
	checks.Equal(int64(item.Highlight.End), *got.SpanEnd)
}

func TestPersonFactEvidenceInputMapsSweepLaneToArchiveClass(t *testing.T) {
	lanes := []SourceClass{
		SourceConversationText, SourceMeetingText, SourceDocumentText,
		SourceAttachmentCaption, SourceAttachmentOCR,
	}
	for index, lane := range lanes {
		t.Run(string(lane), func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			item := packetTestEvidence(int64(index+1), lane, "explicit fact")
			got, err := PersonFactEvidenceInput(item)
			requirements.NoError(err)
			checks.Equal(personfacts.EvidenceArchive, got.SourceClass)
			checks.NotEqual(personfacts.EvidenceSourceClass(lane), got.SourceClass)
			decoded, decodeErr := DecodePersonSweepEvidenceRef(got.SourceRef)
			requirements.NoError(decodeErr)
			checks.Equal(lane, decoded.SourceLane)
		})
	}
}

func TestParseExtractionMapsValidityAsUTC(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	packet := packetTestPacket()
	packet.Seeds = packet.Seeds[:1]
	output := fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"supersede","value":"new","evidence_ids":[%q],"valid_from":"2026-08-01T10:30:00-04:00","valid_until":"2026-09-01T00:00:00Z","confidence_basis_points":500}]}`,
		packet.Catalog.Targets[0].Key, packetEvidenceID(packet.Seeds[0]))

	claims, err := ParseExtraction(json.RawMessage(output), packetTestBatch(t, packet), ProviderProfile{AllowSensitive: true})
	requirements.NoError(err)
	requirements.Len(claims, 1)
	requirements.NotNil(claims[0].ValidFrom)
	requirements.NotNil(claims[0].ValidUntil)
	checks.Equal(time.Date(2026, 8, 1, 14, 30, 0, 0, time.UTC), *claims[0].ValidFrom)
	checks.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), *claims[0].ValidUntil)
}

func packetTestBatch(t *testing.T, packet EvidencePacket) PacketBatch {
	t.Helper()
	batches, err := PartitionEvidencePacket(packet, 64*1024, 200)
	require.NoError(t, err)
	require.Len(t, batches, 1)
	return batches[0]
}

const frozenExtractionSchema = `{
  "type":"object",
  "properties":{
    "claims":{
      "type":"array",
      "maxItems":256,
      "items":{
        "type":"object",
        "properties":{
          "target_key":{"type":"string","minLength":1,"maxLength":256},
          "relation":{"type":"string","enum":["support","contradict","supersede"]},
          "value":{},
          "evidence_ids":{"type":"array","minItems":1,"maxItems":200,"uniqueItems":true,"items":{"type":"string","minLength":1,"maxLength":128}},
          "valid_from":{"type":["string","null"],"format":"date-time"},
          "valid_until":{"type":["string","null"],"format":"date-time"},
          "confidence_basis_points":{"type":"integer","minimum":0,"maximum":1000}
        },
        "required":["target_key","relation","value","evidence_ids","valid_from","valid_until","confidence_basis_points"],
        "additionalProperties":false
      }
    }
  },
  "required":["claims"],
  "additionalProperties":false
}`
