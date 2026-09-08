package store_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonSweepLastContactReportsContactStateCoordinates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f, personID := activityProjectionFixture(t)

	_, present, err := f.Store.PersonSweepLastContact(t.Context(), personID)
	require.NoError(err)
	assert.False(present, "a person with no contact state row has no last contact")

	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)

	var newestMessageID int64
	for index, occurredAt := range []time.Time{
		time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
	} {
		messageID := f.NewMessage().
			WithSourceMessageID("brief-last-contact-"+strconv.Itoa(index)).
			WithSentAt(occurredAt).
			Create(t, f.Store)
		_, err = f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
			activityProjectionForMessage(t, f, messageID, personID, occurredAt),
		})
		require.NoError(err)
		newestMessageID = messageID
	}

	state, err := f.Store.ContactStateContext(
		t.Context(), personID, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.NotNil(state.LastContactAt)
	require.Equal("message:"+strconv.FormatInt(newestMessageID, 10), state.LastContactRef)

	lastContact, present, err := f.Store.PersonSweepLastContact(t.Context(), personID)
	require.NoError(err)
	require.True(present)
	assert.Equal(newestMessageID, lastContact.MessageID,
		"the brief window resolves the same message the contact projection did")
	assert.Equal(string(state.LastContactChannel), lastContact.Channel)
	if state.LastContactSourceID != nil {
		assert.Equal(*state.LastContactSourceID, lastContact.SourceID)
	}
}

func TestPersonSweepLastContactRejectsInvalidPerson(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, _, err := st.PersonSweepLastContact(t.Context(), 0)
	require.Error(t, err)
}
