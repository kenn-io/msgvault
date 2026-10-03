package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenericAuthoritativeCallDuration(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want *float64
	}{
		{"positive", `{"duration_seconds":45}`, new(float64(45))},
		{"zero", `{"duration_seconds":0}`, new(float64(0))},
		{"precedence", `{"duration_seconds":45,"started_at":"2026-10-03T10:00:00Z","ended_at":"2026-10-03T10:01:00Z"}`, new(float64(45))},
		{"missing", `{}`, nil},
		{"null", `{"duration_seconds":null}`, nil},
		{"negative", `{"duration_seconds":-1}`, nil},
		{"nonfinite", `{"duration_seconds":"Infinity"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			content := Decode("meeting_json", []byte(tc.raw), nil)
			if tc.want == nil {
				assert.Nil(content.DurationSeconds)
				return
			}
			require.NotNil(content.DurationSeconds)
			assert.InDelta(*tc.want, *content.DurationSeconds, 1e-9)
			assert.Equal(DurationProvider, content.DurationBasis)
		})
	}
}

func TestProviderPhoneAndIDParticipantsMergeByIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	content := Decode("meeting_json", []byte(`{"organizer":{"participant_id":201,"name":"Example Agent"},"attendees":[{"name":"Example Contact","phone":"+12025550101"},{"name":"Same Name"},{"name":"Same Name"}]}`), nil)
	require.Len(content.SourceParticipants, 4)
	assert.Equal("+12025550101", content.SourceParticipants[1].Phone)
	entry := normalizeEntry(Entry{
		Participants: []Participant{
			{ParticipantID: new(int64(201)), Name: "Example Agent", Role: "from"},
			{ParticipantID: new(int64(101)), Name: "Example Contact", Phone: "+12025550101", Role: "to"},
		},
		Content: content,
	})
	assert.Len(entry.Participants, 4, "resolved and source identities merge; name-only duplicates stay separate")
}

func TestProviderParticipantIDsDoNotMergeDifferentPeopleOnSharedPhone(t *testing.T) {
	assert := assert.New(t)

	const sharedPhone = "+12025550101"
	archived := []Participant{
		{ParticipantID: new(int64(101)), Name: "First Example", Phone: sharedPhone, Role: "to"},
		{ParticipantID: new(int64(102)), Name: "Second Example", Phone: sharedPhone, Role: "to"},
	}
	entry := normalizeEntry(Entry{
		Participants: archived,
		Content: Content{SourceParticipants: []Participant{
			{ParticipantID: new(int64(103)), Name: "Third Example", Phone: sharedPhone, Role: "to"},
			{Name: "Unresolved Example", Phone: sharedPhone, Role: "to"},
		}},
	})
	assert.Len(entry.Participants, 4, "shared phone cannot select one of several durable people")
	assert.Contains(entry.Participants, Participant{Name: "Unresolved Example", Phone: sharedPhone, Role: "to"})
}

func TestProviderInvalidParticipantIDsAreNotPresentedAsArchiveIdentities(t *testing.T) {
	assert := assert.New(t)

	content := Decode("meeting_json", []byte(`{"organizer":{"participant_id":-1},"attendees":[{"participant_id":0},{"participant_id":-2,"name":"Example Guest"}]}`), nil)
	assert.Equal([]Participant{{Name: "Example Guest", Role: "to"}}, content.SourceParticipants)
}
