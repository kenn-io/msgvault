package chatwoot

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/chatwootapi"
)

func TestPersistCallClearsRecipientsWhenResolvedPeopleDisappear(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	api := newContractAPI(t, 3, []map[string]any{chatwootapi.Message(201, 1767225600, nil)})
	importer, source := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	chatID := contractArchivedMessageID(t, st, "201")
	agentID, err := st.EnsureParticipantByIdentifier(SourceType, "agent/7", "Example Agent")
	require.NoError(err)
	contactID, err := st.EnsureParticipantByIdentifier(SourceType, "contact/8", "Example Contact")
	require.NoError(err)
	message := Message{ID: 201, CreatedAt: 1767225600, Raw: jsontext.Value(`{"id":201}`), Call: &Call{Direction: "outgoing"}}
	metadata := map[string]any{}
	var meetingID int64
	for _, ids := range [][]int64{{agentID, contactID}, {0, contactID}, {0, 0}} {
		sum := &ImportSummary{}
		id, _, err := importer.persistCall(t.Context(), source.ID, Conversation{ID: 42}, message, chatID,
			Actor{Type: "contact", Name: "Example Contact"}, ids[1], Actor{Type: actorUser, Name: "Example Agent"}, ids[0],
			metadata, map[string]store.AttachmentRef{}, ImportOptions{InboxID: 7, NoMedia: true}, sum)
		require.NoError(err)
		if meetingID == 0 {
			meetingID = id
		}
		assert.Equal(meetingID, id)
		assert.Equal(1, sum.Meetings, "changed resolved people must rewrite the meeting")
		assert.Equal(ids[0], contractSender(t, st, id).Int64)
		to := contractRecipients(t, st, id, "to")
		if ids[1] == 0 {
			assert.Empty(to)
		} else {
			require.Len(to, 1)
			assert.Equal(ids[1], to[0].ParticipantID)
		}
	}
}

// The timeline retains historical evidence even when the live call serializer
// can no longer resolve its employee association or other optional fields.
func TestCallEvidenceMergesLiveCallOverTimeline(t *testing.T) {
	const timeline = `"content_attributes":{"data":{
		"call_id":501,"call_sid":"CA_synthetic_fallback","call_source":"twilio","call_direction":"inbound",
		"status":"completed","duration_seconds":31,"accepted_by":{"id":7,"name":"Example Historical Agent"}
	}}`
	for _, tc := range []struct {
		name, live string
		want       Call
	}{
		{"missing_live_fields", `{"id":null,"accepted_by_agent_id":7,"accepted_by_agent_name":null,"duration_seconds":null,"started_at":null,"ended_at":null}`, Call{
			Direction: "incoming", Status: "completed",
			DurationSeconds: new(float64(31)), AcceptedByAgentID: 7, AcceptedByAgentName: "Example Historical Agent",
			StartedAt: jsontext.Value(`null`), EndedAt: jsontext.Value(`null`),
		}},
		{"live_fields_and_explicit_zero_win", `{
			"id":601,"provider_call_id":"CA_synthetic_live","provider":"whatsapp","direction":"outbound","status":"rejected",
			"duration_seconds":0,"accepted_by_agent_id":8,"accepted_by_agent_name":"Example Current Agent",
			"started_at":1801526500,"ended_at":"2027-02-02T00:01:40Z",
			"from_number":"+12025550103","to_number":"+12025550104",
			"recording_url":"https://media.example.com/current.ogg","transcript":"Synthetic current transcript"
		}`, Call{
			Direction: "outgoing", Status: "rejected",
			DurationSeconds: new(float64(0)), AcceptedByAgentID: 8, AcceptedByAgentName: "Example Current Agent",
			StartedAt: jsontext.Value(`1801526500`), EndedAt: jsontext.Value(`"2027-02-02T00:01:40Z"`),
			FromNumber: "+12025550103", ToNumber: "+12025550104",
			RecordingURL: "https://media.example.com/current.ogg", Transcript: "Synthetic current transcript",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var message Message
			require.NoError(t, json.Unmarshal([]byte(`{`+timeline+`,"call":`+tc.live+`}`), &message))
			assert.Equal(t, tc.want, callEvidence(message))
		})
	}
	// A historical agent name applies only to the agent the live call names.
	for _, tc := range []struct {
		name     string
		live     *Call
		wantID   int64
		wantName string
	}{
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
