package carddav

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestScopedPublicationBoundsProviderFailureEffects(t *testing.T) {
	for _, mode := range []string{"precondition", "mismatch", "settlement_revoked", "put_throttle", "get_throttle", "definitive_rejection", "unknown_timeout"} {
		t.Run(mode, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped, revoked atomic.Bool
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() && mode == "mismatch" && r.Method == http.MethodGet {
					fixture.mu.Lock()
					fixture.body = []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:person\r\nFN:Synthetic concurrent remote edit\r\nEND:VCARD\r\n")
					fixture.mu.Unlock()
				}
				baseHandler(w, r)
				if scoped.Load() && mode == "settlement_revoked" && r.Method == http.MethodGet {
					revoked.Store(true)
				}
			}))
			t.Cleanup(server.Close)
			service, st, personID, book := seededMutationServiceForServer(t, server)
			backend, ok := any(service).(scopedPublicationService)
			requirements.True(ok)
			requirements.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic authorized correction")
			denied := errors.New("synthetic settlement authority revoked")
			authorize := func(_ context.Context, _ *store.IdentityGrantSelection) error {
				if revoked.Load() {
					return denied
				}
				return nil
			}
			preview, err := backend.PreviewPublicationAuthorized(t.Context(), personID, authorize)
			requirements.NoError(err)
			href := book.CanonicalURL + "person.vcf"
			before, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
			requirements.NoError(err)
			fixture.mu.Lock()
			beforePuts, beforeGets := fixture.puts, fixture.gets
			switch mode {
			case "precondition":
				fixture.putStatus = http.StatusPreconditionFailed
			case "put_throttle":
				fixture.putStatus = http.StatusTooManyRequests
				fixture.putRetryAfter = "60"
			case "get_throttle":
				fixture.throttleGets = 1
			case "definitive_rejection":
				fixture.putStatus = http.StatusBadRequest
			case "unknown_timeout":
				fixture.timeout = true
				service.dav().client.requestTimeout = time.Second
			}
			fixture.mu.Unlock()
			scoped.Store(true)
			err = backend.PublishReviewedPersonAuthorized(t.Context(), personID, preview.ApprovalToken, authorize)
			requirements.Error(err)
			if mode == "precondition" || mode == "mismatch" {
				requirements.ErrorIs(err, ErrScopedPublicationReconciliationRequired)
			}
			if mode == "settlement_revoked" {
				requirements.ErrorIs(err, denied)
			}
			publication, loadErr := st.GetCardDAVPublicationContext(t.Context(), personID)
			requirements.NoError(loadErr)
			retained := mode != "put_throttle" && mode != "definitive_rejection"
			if retained {
				assertions.Equal(store.CardDAVMutationUpdate, publication.PendingOperation)
				assertions.NotEmpty(publication.PendingIntentID)
			} else {
				assertions.Empty(publication.PendingOperation)
				assertions.Empty(publication.PendingIntentID)
			}
			after, loadErr := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
			requirements.NoError(loadErr)
			assertions.Equal(before.RemoteETag, after.RemoteETag)
			assertions.Equal(before.RemoteBody, after.RemoteBody)
			_, loadErr = st.GetUnresolvedCardDAVConflictForMappingContext(t.Context(), book.ID, href)
			requirements.ErrorIs(loadErr, store.ErrCardDAVConflictNotFound, "scoped failures must not invoke owner conflict capture")
			account, loadErr := st.GetCardDAVAccountForBookContext(t.Context(), book.ID)
			requirements.NoError(loadErr)
			requirements.NotNil(account)
			gate, loadErr := st.GetCardDAVRetryAfterContext(t.Context(), account.ID)
			requirements.NoError(loadErr)
			if mode == "put_throttle" || mode == "get_throttle" {
				requirements.NotNil(gate)
				assertions.True(gate.After(time.Now()))
			} else {
				assertions.Nil(gate)
			}
			fixture.mu.Lock()
			assertions.Equal(beforePuts+1, fixture.puts)
			wantGets := beforeGets
			if mode == "mismatch" || mode == "settlement_revoked" || mode == "get_throttle" {
				wantGets++
			}
			assertions.Equal(wantGets, fixture.gets)
			fixture.mu.Unlock()
		})
	}
}
