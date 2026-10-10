package carddav

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type scopedPublicationService interface {
	PreviewPublicationAuthorized(ctx context.Context, personID int64, authorize store.PersonEditAuthorizer) (*PublicationPreview, error)
	PublishReviewedPersonAuthorized(ctx context.Context, personID int64, token string, authorize store.PersonEditAuthorizer) error
	RecoverPublicationAuthorized(ctx context.Context, expected store.CardDAVPublication, authorize store.PersonEditAuthorizer) error
}

func TestScopedReviewedPublicationUsesCurrentAuthorityForEachEffect(t *testing.T) {
	for _, mode := range []string{"allowed", "denied", "post_put_revoked", "empty_token"} {
		t.Run(mode, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped, revoked atomic.Bool
			var puts, gets atomic.Int32
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() {
					switch r.Method {
					case http.MethodPut:
						puts.Add(1)
					case http.MethodGet:
						gets.Add(1)
					default:
						assertions.Fail("scoped publication must send only PUT and GET")
					}
				}
				baseHandler(w, r)
				if scoped.Load() && mode == "post_put_revoked" && r.Method == http.MethodPut {
					revoked.Store(true)
				}
			}))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			backend, ok := any(service).(scopedPublicationService)
			requirements.True(ok, "mapped reviewed publication requires a separate authorized Service path")
			requirements.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic reviewed contact correction")
			denied := errors.New("synthetic publication grant revoked")
			authorize := func(_ context.Context, scope *store.IdentityGrantSelection) error {
				assertions.Len(scope.Persons, 1)
				assertions.Len(scope.AddressBooks, 1)
				if revoked.Load() {
					return denied
				}
				return nil
			}
			beforePreview, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			scoped.Store(true)
			preview, err := backend.PreviewPublicationAuthorized(t.Context(), personID, authorize)
			requirements.NoError(err)
			requirements.NotNil(preview)
			requirements.NotEmpty(preview.ApprovalToken)
			requirements.Equal(PublicationReviewCurrent, preview.Kind)
			assertions.Zero(puts.Load(), "preview must not send a PUT")
			assertions.Zero(gets.Load(), "preview must not send a GET")
			afterPreview, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			beforePreviewJSON, err := json.Marshal(beforePreview)
			requirements.NoError(err)
			afterPreviewJSON, err := json.Marshal(afterPreview)
			requirements.NoError(err)
			assertions.JSONEq(string(beforePreviewJSON), string(afterPreviewJSON), "preview must not change any native rendering or publication evidence")
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			if mode == "denied" {
				revoked.Store(true)
			}
			token := preview.ApprovalToken
			if mode == "empty_token" {
				token = ""
			}
			scoped.Store(true)
			err = backend.PublishReviewedPersonAuthorized(t.Context(), personID, token, authorize)
			switch mode {
			case "allowed":
				requirements.NoError(err)
				assertions.Equal(int32(1), puts.Load())
				assertions.Equal(int32(1), gets.Load())
				publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
				requirements.NoError(err)
				assertions.Empty(publication.PendingOperation)
				assertions.Empty(publication.PendingIntentID)
			case "post_put_revoked":
				requirements.ErrorIs(err, denied)
				assertions.Equal(int32(1), puts.Load())
				assertions.Zero(gets.Load(), "canonical observation requires fresh authority after PUT")
				pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
				requirements.NoError(err)
				requirements.NotEmpty(pending.PendingIntentID)
				requirements.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)
				revoked.Store(false)
				requirements.NoError(backend.RecoverPublicationAuthorized(t.Context(), *pending, authorize))
				assertions.Equal(int32(1), puts.Load(), "recovery must never replay the ambiguous mapped PUT")
				assertions.Equal(int32(1), gets.Load())
				requirements.ErrorIs(backend.RecoverPublicationAuthorized(t.Context(), *pending, authorize), store.ErrCardDAVStalePlan)
				assertions.Equal(int32(1), puts.Load())
				assertions.Equal(int32(1), gets.Load())
			default:
				if mode == "denied" {
					requirements.ErrorIs(err, denied)
				} else {
					requirements.Error(err)
				}
				assertions.Zero(puts.Load())
				assertions.Zero(gets.Load())
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				requirements.NoError(err)
				beforeJSON, err := json.Marshal(before)
				requirements.NoError(err)
				afterJSON, err := json.Marshal(after)
				requirements.NoError(err)
				assertions.JSONEq(string(beforeJSON), string(afterJSON))
			}
		})
	}
}
