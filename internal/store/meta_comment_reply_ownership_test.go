package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestMetaCommentReplyOwnershipRequiresSucceededSourceMatch(t *testing.T) {
	for _, scenario := range []string{"sent", "unknown", "failed", "private", "other-source", "other-identifier", "other-reply"} {
		t.Run(scenario, func(t *testing.T) {
			require := require.New(t)
			st, claim := actionStoreFixture(t)
			claim.Action = "public_reply"
			if scenario == "private" {
				claim.Action = "private_reply"
			}
			row := claimAction(t, st, claim)
			row, err := st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
			require.NoError(err)
			state := "succeeded"
			if scenario == "unknown" || scenario == "failed" {
				state = scenario
			}
			_, err = st.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: state, RemoteID: "501"})
			require.NoError(err)
			sourceID, identifier, remoteID := claim.SourceID, claim.SourceIdentifier, "501"
			switch scenario {
			case "other-source":
				sourceID++
			case "other-identifier":
				identifier = "app:9:page:101"
			case "other-reply":
				remoteID = "502"
			default:
			}
			sent, err := st.HasSucceededMetaCommentReplyContext(t.Context(), sourceID, identifier, remoteID)
			require.NoError(err)
			assert.Equal(t, scenario == "sent", sent)
		})
	}
}
