package beeper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/personmatchpolicy"
	"go.kenn.io/msgvault/internal/personmatchworker"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func addMatchingNameEnvelope(t *testing.T, st *store.Store, source, room int64, key string, ids []int64, names []string) {
	t.Helper()
	message, err := st.UpsertMessage(&store.Message{SourceID: source, ConversationID: room, SourceMessageID: key, MessageType: "chat"})
	require.NoError(t, err)
	require.NoError(t, st.ReplaceMessageRecipients(message, "to", ids, names))
}

func TestMatcherNameEvidenceRequiresExactFullEnvelopeThroughScoring(t *testing.T) {
	for _, scenario := range []string{"cold-with-blank-full", "cold-with-different-full", "demote-only-name-room", "demote-with-exact-full"} {
		t.Run(scenario, func(t *testing.T) {
			requirements := require.New(t)

			ctx := t.Context()
			st := testutil.NewTestStore(t)
			st.DB().SetMaxOpenConns(1)
			leftSource := newBeeperTestSource(t, st, "name-left")
			rightSource := newBeeperTestSource(t, st, "name-right")
			left, err := st.EnsureParticipantByIdentifier(participantIdentifierType, "@name-left:beeper.local", "Shared Example")
			requirements.NoError(err)
			right, err := st.EnsureParticipantByIdentifier(participantIdentifierType, "@name-right:beeper.local", "Shared Example")
			requirements.NoError(err)
			ids := []int64{left, right}
			nameRoom, err := st.EnsureConversation(leftSource, "name-origin", "Name origin")
			requirements.NoError(err)
			initialFull := scenario == "demote-only-name-room" || scenario == "demote-with-exact-full"
			tier := conversationpolicy.Cold
			if initialFull {
				tier = conversationpolicy.Full
			}
			requirements.NoError(st.SetConversationPolicyOverride(ctx, leftSource, "name-origin", tier))
			addMatchingNameEnvelope(t, st, leftSource, nameRoom, "origin-names", ids, []string{"Shared Example", "Shared Example"})

			// Real address capture comes from Full rooms with blank or different
			// envelope names. Those addresses cannot attest to the raw shared name.
			fullName := ""
			switch scenario {
			case "cold-with-different-full":
				fullName = "Different Example"
			case "demote-with-exact-full":
				fullName = "Shared Example"
			}
			leftRoom := fullBeeperObservationRoom(t, st, leftSource)
			rightRoom := fullBeeperObservationRoom(t, st, rightSource)
			addMatchingNameEnvelope(t, st, leftSource, leftRoom, "left-full-envelope", []int64{left}, []string{fullName})
			addMatchingNameEnvelope(t, st, rightSource, rightRoom, "right-full-envelope", []int64{right}, []string{fullName})
			recorder := newObservationRecorder(st)
			matcher := newIdentityMatcher(st)
			captureAndMatch(t, recorder, matcher, left, &User{ID: "@name-left:beeper.local", Username: "shared-user", Email: "shared@example.test"}, captureContext{SourceID: leftSource, ConversationID: leftRoom, AccountID: "name-left", Network: "Slack"})
			outcome := captureAndMatch(t, recorder, matcher, right, &User{ID: "@name-right:beeper.local", Username: "shared-user", Email: "shared@example.test"}, captureContext{SourceID: rightSource, ConversationID: rightRoom, AccountID: "name-right", Network: "Slack"})
			requirements.NotEmpty(outcome.Suggested)
			candidateID := outcome.Suggested[0]

			checkSnapshot := func(wantName, score bool) {
				t.Helper()
				candidate, err := st.GetIdentityMatchCandidateContext(ctx, candidateID)
				require.NoError(t, err)
				kinds := map[string]bool{}
				for _, evidence := range candidate.Evidence {
					kinds[evidence.EvidenceKind] = true
				}
				assert.True(t, kinds[evidenceEmail], "Full email evidence stays independently justified")
				assert.True(t, kinds[evidenceUsername], "Full username candidate remains explained")
				assert.Equal(t, wantName, kinds[evidenceName], "candidate detail must exclude unsupported automatic names")
				if !score {
					return
				}
				lease, err := st.ClaimNextIdentityMatchJudgmentContext(ctx, "name_policy", time.Minute)
				require.NoError(t, err)
				require.NotNil(t, lease)
				assert.Equal(t, candidateID, lease.CandidateID)
				packet, signals, err := personmatchworker.BuildPairPacket(&lease.Candidate)
				require.NoError(t, err)
				nameEvidence, emailEvidence, nameSignal, emailSignal := false, false, false, false
				for _, evidence := range packet.Evidence {
					nameEvidence = nameEvidence || evidence.Class == string(personmatchpolicy.SignalDisplayName)
					emailEvidence = emailEvidence || evidence.Class == string(personmatchpolicy.SignalEmail)
				}
				for _, signal := range signals {
					nameSignal = nameSignal || signal.Class == personmatchpolicy.SignalDisplayName
					emailSignal = emailSignal || signal.Class == personmatchpolicy.SignalEmail
				}
				assert.Equal(t, wantName, nameEvidence, "the real scoring snapshot must not leak a name evidence class")
				assert.Equal(t, wantName, nameSignal, "the real scoring snapshot must not leak a name policy signal")
				assert.True(t, emailEvidence)
				assert.True(t, emailSignal)
			}
			if initialFull {
				checkSnapshot(true, false)
				requirements.NoError(st.SetConversationPolicyOverride(ctx, leftSource, "name-origin", conversationpolicy.Cold))
			}
			checkSnapshot(scenario == "demote-with-exact-full", true)
		})
	}
}
