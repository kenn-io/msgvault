package carddav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPublicationReceiptRecoversProviderTimeoutWithoutReplayingPUT(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := &mutationFixture{}
	var scoped atomic.Bool
	var puts, gets atomic.Int32
	handler := fixture.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scoped.Load() {
			switch r.Method {
			case http.MethodPut:
				puts.Add(1)
			case http.MethodGet:
				gets.Add(1)
			default:
				assertions.Fail("receipt recovery must only update or observe its mapped resource")
			}
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	requirements.NoError(service.PublishPerson(t.Context(), personID))
	appendInferenceReviewNote(t, st, personID, "Synthetic timeout receipt correction")
	authorize := func(context.Context, *store.IdentityGrantSelection) error { return nil }
	preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, authorize)
	requirements.NoError(err)
	// The real HTTP handler saves the PUT, then waits for client cancellation.
	// This deadline bounds that observable transition, not runner throughput.
	fixture.mu.Lock()
	fixture.timeout = true
	fixture.mu.Unlock()
	requestTimeout := service.dav().client.requestTimeout
	service.dav().client.requestTimeout = time.Second
	scoped.Store(true)
	first, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-timeout-owner", "synthetic-timeout-key", authorize)
	service.dav().client.requestTimeout = requestTimeout
	requirements.ErrorIs(err, context.DeadlineExceeded, "actual native HTTP request must time out")
	requirements.NotNil(first)
	assertions.Equal("dispatching", first.State)
	assertions.Equal(int32(1), puts.Load())
	assertions.Zero(gets.Load())
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	requirements.NoError(err)
	requirements.NotEmpty(pending.PendingIntentID)

	saved, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-timeout-owner", "synthetic-timeout-key", authorize)
	requirements.NoError(err)
	requirements.NotNil(saved)
	assertions.Equal(first.ID, saved.ID)
	assertions.Equal("dispatching", saved.State)
	assertions.Equal(int32(1), puts.Load())
	assertions.Zero(gets.Load(), "approval retry only reads its saved outcome")

	recovered, err := service.ReconcileReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-timeout-owner", "synthetic-timeout-key", authorize)
	requirements.NoError(err)
	requirements.NotNil(recovered)
	assertions.Equal(first.ID, recovered.ID)
	assertions.Equal("verified", recovered.State)
	assertions.Equal(int32(1), puts.Load())
	assertions.Equal(int32(1), gets.Load())
	pending, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	requirements.NoError(err)
	assertions.Empty(pending.PendingIntentID)

	settled, err := service.ReconcileReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-timeout-owner", "synthetic-timeout-key", authorize)
	requirements.NoError(err)
	requirements.NotNil(settled)
	assertions.Equal(first.ID, settled.ID)
	assertions.Equal("verified", settled.State)
	assertions.Equal(int32(1), puts.Load(), "timeout recovery never replays the provider write")
	assertions.Equal(int32(1), gets.Load(), "settled recovery only reads its native receipt")
}
