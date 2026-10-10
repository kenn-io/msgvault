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

type publicationRequestAuthorityFactory interface {
	withPublicationRequestAuthority(ctx context.Context, expected store.CardDAVPublication, authorize store.PersonEditAuthorizer) context.Context
}

func TestScopedPublicationRechecksEveryHTTPDispatch(t *testing.T) {
	for _, mode := range []string{"digest_revoked", "redirect_other_book", "throttle_revoked"} {
		t.Run(mode, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped, revoked atomic.Bool
			var selectedRequests, foreignRequests atomic.Int32
			var href string
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !scoped.Load() {
					baseHandler(w, r)
					return
				}
				if r.URL.Path == "/books/other/unselected.vcf" {
					foreignRequests.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				selectedRequests.Add(1)
				assertions.Equal(http.MethodPut, r.Method)
				assertions.Equal(href, r.URL.Path)
				switch mode {
				case "digest_revoked":
					revoked.Store(true)
					w.Header().Set("WWW-Authenticate", `Digest realm="synthetic-contacts", nonce="synthetic-challenge", algorithm=MD5, qop="auth"`)
					w.WriteHeader(http.StatusUnauthorized)
				case "throttle_revoked":
					revoked.Store(true)
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
				default:
					w.Header().Set("Location", "/books/other/unselected.vcf")
					w.WriteHeader(http.StatusTemporaryRedirect)
				}
			}))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			factory, ok := any(service).(publicationRequestAuthorityFactory)
			requirements.True(ok, "scoped publication must carry immutable native authority to each actual HTTP attempt")
			requirements.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic reviewed change")
			source, plan, err := service.currentPublicationPlan(t.Context(), personID)
			requirements.NoError(err)
			fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
			pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
			requirements.NoError(err)
			requirements.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			href = "/books/personal/person.vcf"
			denied := errors.New("synthetic dispatch authority revoked")
			var authorizations atomic.Int32
			operationCtx := factory.withPublicationRequestAuthority(t.Context(), *pending, func(_ context.Context, scope *store.IdentityGrantSelection) error {
				authorizations.Add(1)
				assertions.Len(scope.Persons, 1)
				assertions.Len(scope.AddressBooks, 1)
				if revoked.Load() {
					return denied
				}
				return nil
			})
			scoped.Store(true)
			err = service.gate(operationCtx, func(ctx context.Context) error {
				return service.remote.Put(ctx, pending.Href, pending.OutgoingBody, pending.RemoteETag, false)
			})
			if mode == "digest_revoked" || mode == "throttle_revoked" {
				requirements.ErrorIs(err, denied)
				assertions.Equal(int32(2), authorizations.Load(), "retry dispatch or native retry deadline must check newly revoked authority")
			} else {
				requirements.ErrorIs(err, store.ErrCardDAVInvalidPlan)
			}
			assertions.Equal(int32(1), selectedRequests.Load())
			assertions.Zero(foreignRequests.Load(), "same-origin redirect must not dispatch to another book")
			account, err := st.GetCardDAVAccountForBookContext(t.Context(), pending.AddressBookID)
			requirements.NoError(err)
			requirements.NotNil(account)
			retryGate, err := st.GetCardDAVRetryAfterContext(t.Context(), account.ID)
			requirements.NoError(err)
			assertions.Nil(retryGate, "revoked pause must not change the native account deadline")
			after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			beforeJSON, err := json.Marshal(before)
			requirements.NoError(err)
			afterJSON, err := json.Marshal(after)
			requirements.NoError(err)
			assertions.JSONEq(string(beforeJSON), string(afterJSON), "rejected retry or retarget preserves the exact pending publication")
		})
	}
}

func TestScopedPublicationDispatchRejectsAnotherConnectionClient(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign_same_url", true: "retained_generation"}[stale], func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped atomic.Bool
			var requests atomic.Int32
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() {
					requests.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				baseHandler(w, r)
			}))
			t.Cleanup(server.Close)
			service, st, personID, book := seededMutationServiceForServer(t, server)
			requirements.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic corrected name")
			source, plan, err := service.currentPublicationPlan(t.Context(), personID)
			requirements.NoError(err)
			fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
			pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
			requirements.NoError(err)
			var want error
			if stale {
				service = service.ForConnection("default", pending.ConnectionGeneration+1)
				want = store.ErrCardDAVStalePlan
			} else {
				account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "synthetic-foreign", BaseURL: server.URL, Username: "synthetic-foreign", PrincipalURL: server.URL + "/principal/", HomeURL: server.URL + "/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: book.CanonicalURL, DisplayName: "Synthetic foreign book", CanCreate: new(true)}}})
				requirements.NoError(err)
				requirements.Len(books, 1)
				assertions.NotEqual(book.ID, books[0].ID)
				assertions.Equal(book.CanonicalURL, books[0].CanonicalURL)
				service = service.ForConnection(account.ConnectionName, account.ConnectionGeneration)
				want = ErrConnectionMismatch
			}
			factory, ok := any(service).(publicationRequestAuthorityFactory)
			requirements.True(ok)
			var authorizations atomic.Int32
			operationCtx := factory.withPublicationRequestAuthority(t.Context(), *pending, func(_ context.Context, _ *store.IdentityGrantSelection) error { authorizations.Add(1); return nil })
			scoped.Store(true)
			err = service.remote.Put(operationCtx, pending.Href, pending.OutgoingBody, pending.RemoteETag, false)
			requirements.ErrorIs(err, want)
			assertions.Zero(requests.Load())
			assertions.Zero(authorizations.Load())
		})
	}
}
