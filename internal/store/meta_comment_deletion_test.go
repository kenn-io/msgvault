package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetaCommentDeletionRequiresSucceededSourceMatch(t *testing.T) {
	for _, scenario := range []string{"deleted", "unknown", "failed", "hidden", "other-source", "other-identifier", "other-comment"} {
		t.Run(scenario, func(t *testing.T) {
			require := require.New(t)
			st, claim := actionStoreFixture(t)
			if scenario == "hidden" {
				claim.Action = "hide"
			}
			state := "succeeded"
			if scenario == "unknown" || scenario == "failed" {
				state = scenario
			}
			finishAction(t, st, claimAction(t, st, claim), state)
			sourceID, identifier, commentID := claim.SourceID, claim.SourceIdentifier, claim.TargetCommentID
			switch scenario {
			case "other-source":
				sourceID++
			case "other-identifier":
				identifier = "app:9:page:101"
			case "other-comment":
				commentID = "100_200_301"
			default:
			}
			deleted, err := st.HasSucceededMetaCommentDeletionContext(t.Context(), sourceID, identifier, commentID)
			require.NoError(err)
			assert.Equal(t, scenario == "deleted", deleted)
		})
	}
}
