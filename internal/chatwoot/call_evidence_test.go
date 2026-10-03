package chatwoot

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The timeline retains historical evidence even when the live call serializer
// can no longer resolve its employee association or other optional fields.
func TestCallEvidenceRetainsMissingLiveFields(t *testing.T) {
	var message Message
	require.NoError(t, json.Unmarshal([]byte(`{
		"content_attributes":{"data":{
			"call_id":501,"call_sid":"CA_synthetic_fallback","call_source":"twilio","call_direction":"inbound",
			"status":"completed","duration_seconds":31,"accepted_by":{"id":7,"name":"Example Historical Agent"},
			"started_at":1801526400,"ended_at":"2027-02-02T00:00:31Z",
			"from_number":"+12025550101","to_number":"+12025550102",
			"recording_url":"https://media.example.com/historical.wav","transcript":"Synthetic historical transcript"
		}},
		"call":{"id":null,"accepted_by_agent_id":7,"accepted_by_agent_name":null,"duration_seconds":null,"started_at":null,"ended_at":null}
	}`), &message))
	assert.Equal(t, Call{
		ID: 501, ProviderCallID: "CA_synthetic_fallback", Provider: "twilio", Direction: "incoming", Status: "completed",
		DurationSeconds: new(float64(31)), AcceptedByAgentID: 7, AcceptedByAgentName: "Example Historical Agent",
		StartedAt: jsontext.Value(`1801526400`), EndedAt: jsontext.Value(`"2027-02-02T00:00:31Z"`),
		FromNumber: "+12025550101", ToNumber: "+12025550102",
		RecordingURL: "https://media.example.com/historical.wav", Transcript: "Synthetic historical transcript",
	}, callEvidence(message))
}

func TestCallEvidenceAgentNameMatchesSelectedAgent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		live     *Call
		wantID   int64
		wantName string
	}{
		{"same_agent_missing_name", &Call{AcceptedByAgentID: 7}, 7, "Example Historical Agent"},
		{"different_live_agent", &Call{AcceptedByAgentID: 8}, 8, ""},
		{"missing_live_agent", &Call{}, 7, "Example Historical Agent"},
		{"live_agent_name_wins", &Call{AcceptedByAgentID: 7, AcceptedByAgentName: "Example Current Agent"}, 7, "Example Current Agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := Message{Call: tc.live, ContentAttributes: map[string]jsontext.Value{
				"data": jsontext.Value(`{"accepted_by":{"id":7,"name":"Example Historical Agent"}}`),
			}}
			got := callEvidence(message)
			assert.Equal(t, tc.wantID, got.AcceptedByAgentID)
			assert.Equal(t, tc.wantName, got.AcceptedByAgentName)
		})
	}
}

func TestCallEvidenceLiveFieldsAndExplicitZeroWin(t *testing.T) {
	var message Message
	require.NoError(t, json.Unmarshal([]byte(`{
		"content_attributes":{"data":{
			"call_id":501,"call_sid":"CA_synthetic_fallback","call_source":"twilio","call_direction":"inbound",
			"status":"completed","duration_seconds":31,"accepted_by":{"id":7,"name":"Example Historical Agent"},
			"started_at":1801526400,"ended_at":"2027-02-02T00:00:31Z",
			"from_number":"+12025550101","to_number":"+12025550102",
			"recording_url":"https://media.example.com/historical.wav","transcript":"Synthetic historical transcript"
		}},
		"call":{
			"id":601,"provider_call_id":"CA_synthetic_live","provider":"whatsapp","direction":"outbound","status":"rejected",
			"duration_seconds":0,"accepted_by_agent_id":8,"accepted_by_agent_name":"Example Current Agent",
			"started_at":1801526500,"ended_at":"2027-02-02T00:01:40Z",
			"from_number":"+12025550103","to_number":"+12025550104",
			"recording_url":"https://media.example.com/current.ogg","transcript":"Synthetic current transcript"
		}
	}`), &message))
	assert.Equal(t, Call{
		ID: 601, ProviderCallID: "CA_synthetic_live", Provider: "whatsapp", Direction: "outgoing", Status: "rejected",
		DurationSeconds: new(float64(0)), AcceptedByAgentID: 8, AcceptedByAgentName: "Example Current Agent",
		StartedAt: jsontext.Value(`1801526500`), EndedAt: jsontext.Value(`"2027-02-02T00:01:40Z"`),
		FromNumber: "+12025550103", ToNumber: "+12025550104",
		RecordingURL: "https://media.example.com/current.ogg", Transcript: "Synthetic current transcript",
	}, callEvidence(message))
}
