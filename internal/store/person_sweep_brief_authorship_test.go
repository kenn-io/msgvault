package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPersonSweepWorkerBriefExcludesOwnerAuthoredLastContact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "brief-outbound-last-contact")
	st := f.journal.store
	personID := f.journal.alicePersonID
	ownerID, err := st.EnsureParticipant("owner@example.test", "Owner", "example.test")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(f.journal.sourceID, "owner@example.test", "manual"))
	for _, participantID := range []int64{ownerID, f.journal.aliceID} {
		require.NoError(st.EnsureConversationParticipant(f.journal.conversationID, participantID, "member"))
	}
	const outboundText = "I am planning a surprise for another friend"
	outboundID := f.journal.insertMessage(t, "brief-outbound-last-contact", "chat", ownerID,
		time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	addSweepBody(t, f.journal, outboundID, outboundText)
	require.NoError(st.ReplaceMessageRecipients(outboundID, "to", []int64{f.journal.aliceID}, []string{"Alice"}))

	// Let the production activity projection select the latest outbound
	// message, then prove hydration makes it available without authorship.
	projector, err := activity.NewProjector(st, activity.Options{})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	lastContact, present, err := st.PersonSweepLastContact(t.Context(), personID)
	require.NoError(err)
	require.True(present)
	require.Equal(outboundID, lastContact.MessageID)
	hydrated, err := st.HydratePersonSweepMessages(t.Context(), personID, []int64{outboundID}, 0)
	require.NoError(err)
	require.Len(hydrated, 1)
	require.Nil(hydrated[0].SubjectPersonID)
	require.Contains(hydrated[0].Excerpt, outboundText)

	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeForce,
	})
	require.NoError(err)
	require.Len(f.runner.briefPackets, 1)
	packet, err := json.Marshal(f.runner.briefPackets[0])
	require.NoError(err)
	assert.NotContains(string(packet), outboundText,
		"the provider must not receive owner-authored last-contact text")
	assert.Contains(string(packet), "I start the new role in September",
		"the provider still receives the person's authenticated messages")
	require.Len(result.People, 1)
	assert.Equal(1, result.People[0].BriefVersion)
	assert.Empty(result.People[0].BriefFailureClass)
}
