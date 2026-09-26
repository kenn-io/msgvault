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
