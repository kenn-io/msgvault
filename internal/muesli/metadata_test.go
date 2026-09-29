package muesli

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContactsMetadataRecoversWithoutMeetingEdits(t *testing.T) {
	assert := assert.New(t)
	f := newResolveFixture(t, fixtureCard{
		uniqueID: "CARD-METADATA:ABPerson", phones: []string{"+16045550100"},
	})
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	f.addContactParticipant(t, meetingID, "contact:CARD-METADATA:ABPerson", "Alex Example")
	f.sync(t, ImportOptions{})

	f.sync(t, ImportOptions{ContactsPath: filepath.Join(t.TempDir(), "missing")})
	metadata := messageMetadata(t, f)
	assert.Equal("unavailable", metadata["contacts"])
	assert.InDelta(float64(1), metadata["contacts_carried_forward"], 0)

	recovered := f.sync(t, ImportOptions{})
	assert.Equal(ContactsComplete, recovered.ContactsState)
	metadata = messageMetadata(t, f)
	assert.Equal("complete", metadata["contacts"])
	assert.NotContains(metadata, "contacts_carried_forward")
}
