package client

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMeetingPersonOmitsEmptyEmail(t *testing.T) {
	phone := "+16045550100"

	encoded, err := json.Marshal(generated.MeetingPerson{Phone: &phone})

	require.NoError(t, err)
	assert.JSONEq(t, `{"phone":"+16045550100"}`, string(encoded),
		"a phone-only attendee must not send an empty email that violates format: email")
}

func TestMeetingPersonRequiresIdentity(t *testing.T) {
	t.Parallel()
	phone := "+16045550100"
	for _, tt := range []struct {
		name    string
		person  generated.MeetingPerson
		wantErr bool
	}{
		{name: "missing identity", wantErr: true},
		{name: "email only", person: generated.MeetingPerson{Email: "attendee@example.com"}},
		{name: "phone only", person: generated.MeetingPerson{Phone: &phone}},
		{name: "both", person: generated.MeetingPerson{Email: "attendee@example.com", Phone: &phone}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.person.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
