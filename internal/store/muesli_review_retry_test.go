package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMuesliReviewedAcceptanceRetry(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "new evidence"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := storetest.New(t).Store
			phoneID, err := st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
			require.NoError(err)
			imp := muesli.NewImporter(st)
			source := meetingimport.Source{Identifier: "recorder", AccountEmail: "owner@example.com"}
			_, err = imp.ImportRemote(t.Context(), muesli.RemoteRequest{Action: "register", Source: source})
			require.NoError(err)
			meeting := muesli.Meeting{ID: 42, Title: "Planning", CreatedAt: "2026-09-01 14:00:03", StartTime: "2026-09-01T14:00:00Z", Status: "completed", RawTranscript: "Synthetic transcript", ContactsState: muesli.ContactsComplete, Participants: []muesli.Participant{{Email: "attendee@example.com", Identifier: "email:attendee@example.com", ContactReviewPhones: []string{"+16045550100"}}}}
			request := muesli.RemoteRequest{Action: "upsert", Source: source, Meeting: muesli.NewRemoteMeeting(meeting)}
			_, err = imp.ImportRemote(t.Context(), request)
			require.NoError(err)
			matches, err := st.ListIdentityMatchReviewsContext(t.Context(), nil, 10, 0)
			require.NoError(err)
			require.Len(matches, 1)
			reviewed := matches[0]
			decisionContext, cancel := context.WithCancel(t.Context())
			reset := st.SetIdentityMatchReviewAfterDecisionHookForTest(cancel)
			_, _, err = st.DecideIdentityMatchReviewedContext(decisionContext, reviewed.ID, reviewed.ReviewToken, store.IdentityMatchStateAccepted, nil)
			reset()
			require.ErrorIs(err, context.Canceled)
			pending, err := st.GetIdentityMatchReviewContext(t.Context(), reviewed.ID)
			require.NoError(err)
			require.True(pending.ApplicationPending)
			if changed {
				meeting.ID = 43
				request.Meeting = muesli.NewRemoteMeeting(meeting)
			}
			imported, err := imp.ImportRemote(t.Context(), request)
			require.NoError(err)
			if !changed {
				assert.False(imported.Changed)
			}
			stillPending, err := st.GetIdentityMatchReviewContext(t.Context(), reviewed.ID)
			require.NoError(err)
			assert.True(stillPending.ApplicationPending, "imports must not resume owner decisions")
			if changed {
				applied, err := st.ApplyAcceptedIdentityMatchesContext(t.Context(), 10)
				require.NoError(err)
				assert.Zero(applied)
				current, err := st.GetIdentityMatchReviewContext(t.Context(), reviewed.ID)
				require.NoError(err)
				assert.Equal(store.IdentityMatchStateConflict, current.State)
				_, _, err = st.DecideIdentityMatchReviewedContext(t.Context(), reviewed.ID, reviewed.ReviewToken, store.IdentityMatchStateAccepted, nil)
				require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
				return
			}
			assert.Equal(pending.ReviewToken, stillPending.ReviewToken)
			_, revision, err := st.DecideIdentityMatchReviewedContext(t.Context(), reviewed.ID, reviewed.ReviewToken, store.IdentityMatchStateAccepted, nil)
			require.NoError(err)
			_, again, err := st.DecideIdentityMatchReviewedContext(t.Context(), reviewed.ID, reviewed.ReviewToken, store.IdentityMatchStateAccepted, nil)
			require.NoError(err)
			assert.Equal(revision, again)
			emailID, err := st.EnsureParticipant("attendee@example.com", "", "example.com")
			require.NoError(err)
			members, err := st.ClusterMembers(phoneID)
			require.NoError(err)
			assert.Contains(members, emailID)
		})
	}
}
