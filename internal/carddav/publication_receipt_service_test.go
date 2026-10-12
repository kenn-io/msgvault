package carddav

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPublicationServiceReturnsOriginalReceiptBeforePreview(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite receipt abort trigger; focused Store SQL contracts run on both PostgreSQL builds")
	for _, mode := range []string{"verified", "without_remote", "dispatching", "rejected", "wrong_token", "revoked", "noop"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
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
						assert.Fail("reviewed publication only updates and observes its mapped resource")
					}
				}
				baseHandler(w, r)
			}))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			if mode != "noop" {
				appendInferenceReviewNote(t, st, personID, "Synthetic receipt service correction")
			}
			denied := errors.New("synthetic receipt service grant revoked")
			authorize := func(context.Context, *store.IdentityGrantSelection) error {
				if revoked.Load() {
					return denied
				}
				return nil
			}
			preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, authorize)
			require.NoError(err)
			if mode == "dispatching" {
				_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_service_receipt_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic service receipt failure'); END`)
				require.NoError(err)
			}
			if mode == "rejected" {
				fixture.mu.Lock()
				fixture.putStatus = http.StatusBadRequest
				fixture.mu.Unlock()
			}
			scoped.Store(true)
			first, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-service-key", authorize)
			switch mode {
			case "dispatching":
				require.ErrorContains(err, "synthetic service receipt failure")
			case "rejected":
				require.Error(err)
			default:
				require.NoError(err)
			}
			require.NotNil(first)
			wantState := "verified"
			if mode == "dispatching" {
				wantState = "dispatching"
			}
			if mode == "rejected" {
				wantState = "rejected"
			}
			assert.Equal(wantState, first.State)
			if mode == "noop" {
				assert.True(first.Noop)
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
			} else {
				assert.Equal(int32(1), puts.Load())
				wantGets := int32(1)
				if mode == "rejected" {
					wantGets = 0
				}
				assert.Equal(wantGets, gets.Load())
			}
			beforePuts, beforeGets := puts.Load(), gets.Load()
			token := preview.ApprovalToken
			if mode == "verified" {
				appendInferenceReviewNote(t, st, personID, "Synthetic later local correction")
			}
			if mode == "without_remote" {
				service.remote = nil
			}
			if mode == "wrong_token" {
				token = "different-original-token"
			}
			if mode == "revoked" {
				revoked.Store(true)
			}
			retry, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, token, "synthetic-owner", "synthetic-service-key", authorize)
			switch mode {
			case "wrong_token":
				require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
				assert.Nil(retry)
			case "revoked":
				require.ErrorIs(err, denied)
				assert.Nil(retry)
			default:
				require.NoError(err)
				require.NotNil(retry)
				assert.Equal(first.ID, retry.ID)
				assert.Equal(wantState, retry.State)
			}
			assert.Equal(beforePuts, puts.Load(), "receipt retries never repeat provider writes")
			assert.Equal(beforeGets, gets.Load(), "receipt retries do not start automatic recovery")
		})
	}
}

func TestPublicationServiceConcurrentRequestsShareOneReceipt(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	fixture := &mutationFixture{}
	var scoped atomic.Bool
	var calls, puts, gets atomic.Int32
	bothCalling := make(chan struct{})
	baseHandler := fixture.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scoped.Load() {
			switch r.Method {
			case http.MethodPut:
				<-bothCalling
				puts.Add(1)
			case http.MethodGet:
				gets.Add(1)
			default:
				assert.Fail("reviewed publication only updates and observes its mapped resource")
			}
		}
		baseHandler(w, r)
	}))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	appendInferenceReviewNote(t, st, personID, "Synthetic concurrent receipt correction")
	allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
	preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, allow)
	require.NoError(err)
	type result struct {
		receipt *store.CardDAVPublicationReceipt
		err     error
	}
	done := make(chan result, 2)
	scoped.Store(true)
	for range 2 {
		go func() {
			if calls.Add(1) == 2 {
				close(bothCalling)
			}
			receipt, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-concurrent-key", allow)
			done <- result{receipt, err}
		}()
	}
	first, second := <-done, <-done
	require.NoError(first.err)
	require.NoError(second.err)
	require.NotNil(first.receipt)
	require.NotNil(second.receipt)
	assert.Equal(first.receipt.ID, second.receipt.ID)
	assert.Equal("verified", first.receipt.State)
	assert.Equal("verified", second.receipt.State)
	assert.Equal(int32(1), puts.Load())
	assert.Equal(int32(1), gets.Load())
}
