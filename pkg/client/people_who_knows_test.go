package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// The daemon omits an empty subject, snippet, or display name. The generated
// client must decode and validate that response, and an explicit empty string.
func TestGeneratedWhoKnowsResponseDecodesEmptyStrings(t *testing.T) {
	for name, body := range map[string]string{
		"omitted": `{"topic":"cloud","people":[{"rank":1,"person_id":7,"score":1.2,"authored_messages":1,
			"citations":[{"message_id":3,"message_type":"email"}]}],
			"unsaved_contacts":[{"rank":1,"participant_id":9,"score":0.8,"authored_messages":2,
			"citations":[{"message_id":4,"message_type":"chat","sent_at":"2026-10-01T09:00:00Z"}]}]}`,
		"explicit": `{"topic":"cloud","people":[{"rank":1,"person_id":7,"display_name":"","score":1.2,"authored_messages":1,
			"citations":[{"message_id":3,"message_type":"email","subject":"","snippet":""}]}],"unsaved_contacts":[]}`,
		"empty": `{"topic":"cloud","reason":"no_relevant_evidence","people":[],"unsaved_contacts":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var response generated.WhoKnowsResponse
			require.NoError(t, json.Unmarshal([]byte(body), &response))
			require.NoError(t, response.Validate())
			assert.Equal(t, "cloud", response.Topic)
		})
	}
}

func TestGeneratedWhoKnowsRequestSendsOnlySetFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/people/who-knows", r.URL.Path)
		assert.NoError(json.NewDecoder(r.Body).Decode(&got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"topic":"cloud","people":[],"unsaved_contacts":[]}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(err)

	limit := int64(5)
	response, err := client.FindPeopleWhoKnowWithResponse(context.Background(), &generated.FindPeopleWhoKnowRequestOptions{
		Body: &generated.FindPeopleWhoKnowBody{Topic: "cloud", Limit: &limit},
	})
	require.NoError(err)
	require.NotNil(response.JSON200)
	assert.Equal(map[string]any{"topic": "cloud", "limit": float64(5)}, got)
}
