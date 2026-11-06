package line_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

func TestReplyRefusalKeepsAdmissionDeadline(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newReplyFixture(t, "user", nil)
			admittedAt := f.now
			f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Status: status})
			refused, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			if status == http.StatusForbidden {
				must.ErrorIs(err, line.ErrCredentialsUnavailable)
			} else {
				must.NoError(err)
			}
			checks.Equal(store.LineReplyAdmitted, refused.Status)
			checks.Zero(f.fake.AcceptedPushes())

			f.now = admittedAt.Add(22 * time.Hour)
			f.resetClient()
			f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
			uncertain, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			must.NoError(err)
			checks.Equal(store.LineReplyRemoteUnknown, uncertain.Status)
			checks.Equal(1, f.fake.AcceptedPushes())

			f.now = admittedAt.Add(25 * time.Hour)
			expired, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			must.NoError(err)
			checks.Equal(store.LineReplyRemoteUnknownExpired, expired.Status)
			checks.Equal(1, f.fake.AcceptedPushes(), "expiry must prevent a second accepted message")
			checks.Equal(2, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
			attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
			must.NoError(err)
			checks.Equal(admittedAt.Add(23*time.Hour), attempt.RetryUntil)
		})
	}
}
