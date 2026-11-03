package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplyUnknownResolutionAfterTargetDeleted(t *testing.T) {
	for _, kind := range []ActionKind{ActionPublicReply, ActionPrivateReply} {
		for _, resolution := range []string{"sent", "not-sent"} {
			t.Run(string(kind)+"/"+resolution, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st, src, id, a, g := actionFixture(t)
				target := privateClock(t, a, id)
				g.status = 500
				unknown, err := replyRun(t, a, id, kind, "Example reply")
				_ = actionCode(t, err, "outcome_unknown")
				require.NoError(st.MarkMessageDeletedContext(t.Context(), src.ID, "100_200_300"))
				preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Body: "Example reply", DryRun: true})
				require.NoError(err)
				assert.Equal(unknown.Confirm, preview.Confirm)
				got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Body: "Example reply", Confirm: preview.Confirm, ResolveUnknown: resolution})
				require.NoError(err)
				require.NotNil(got.Receipt)
				assert.Equal(unknown.Receipt.ID, got.Receipt.ID)
				assert.Equal(1, g.count())
				_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Body: "Another reply", Confirm: ConfirmToken(target, kind, "Another reply")})
				_ = actionCode(t, err, "target_deleted")
				assert.Equal(1, g.count())
			})
		}
	}
}
