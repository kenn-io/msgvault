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
		{"zero", `{"duration_seconds":0}`, new(float64(0))},
		{"precedence", `{"duration_seconds":45,"started_at":"2026-10-03T10:00:00Z","ended_at":"2026-10-03T10:01:00Z"}`, new(float64(45))},
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

// A durable ID outranks an address: a phone shared by several archived people
// selects none of them, and an ID absorbed by a participant merge is dropped.
func TestProviderParticipantIDsMatchOnlyTheirOwnPerson(t *testing.T) {
	const sharedPhone = "+12025550101"
	for _, tc := range []struct {
		name     string
		archived []Participant
		source   []Participant
		want     []Participant
	}{
		{
			"shared_phone",
			[]Participant{
				{ParticipantID: new(int64(101)), Name: "First Example", Phone: sharedPhone, Role: "to"},
				{ParticipantID: new(int64(102)), Name: "Second Example", Phone: sharedPhone, Role: "to"},
			},
			[]Participant{
				{ParticipantID: new(int64(103)), Name: "Stale Example", Phone: sharedPhone, Role: "to"},
				{Name: "Unresolved Example", Phone: sharedPhone, Role: "to"},
			},
			[]Participant{
				{ParticipantID: new(int64(101)), Name: "First Example", Phone: sharedPhone, Role: "to"},
				{ParticipantID: new(int64(102)), Name: "Second Example", Phone: sharedPhone, Role: "to"},
				{Name: "Unresolved Example", Phone: sharedPhone, Role: "to"},
			},
		},
		{
			// The snapshot keeps the absorbed participant's ID; recipients hold the survivor.
			"merged_away",
			[]Participant{{ParticipantID: new(int64(202)), Name: "Example Agent", Role: "from"}},
			[]Participant{{ParticipantID: new(int64(201)), Name: "Example Agent", Role: "from"}},
			[]Participant{{ParticipantID: new(int64(202)), Name: "Example Agent", Role: "from"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := normalizeEntry(Entry{Participants: tc.archived, Content: Content{SourceParticipants: tc.source}})
			assert.Equal(t, tc.want, entry.Participants)
		})
	}
}
