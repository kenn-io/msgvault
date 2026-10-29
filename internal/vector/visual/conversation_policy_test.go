package visual

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestVisualColdPolicyOriginalReconciliationAuthority(t *testing.T) {
	for _, duringRequest := range []bool{false, true} {
		name := "before_request"
		if duringRequest {
			name = "during_request"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)

			f, _, work := workerFixture(t, 2)
			transition := func() {
				require.NoError(t, f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Cold))
				require.NoError(t, f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Full))
			}
			provider := &fakeVisualProvider{}
			if duringRequest {
				provider.callback = func([]DocumentInput) { transition() }
			} else {
				transition()
			}
			backend := newFakeVisualBackend()
			worker := newTestVisualWorker(t, f, provider, backend)
			worker.config.MaxBatchItems = 1
			result, err := worker.Run(t.Context(), work)
			require.ErrorIs(t, err, store.ErrConversationPolicyStale)
			assertions.Zero(result.Published)
			assertions.Empty(backend.vectors)
			calls := 0
			if duringRequest {
				calls = 1
			}
			assertions.Equal(calls, provider.callCount())
			for _, item := range work {
				_, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
					GenerationID: item.Claim.GenerationID, Owner: item.Candidate.Owner,
					ProposedRevision: item.Document.Revision, LeaseOwner: "successor",
					Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: item.Claim.SourceFence,
				})
				require.NoError(t, err)
				assertions.True(acquired, "stale visual work must release current and unprocessed claims")
			}
		})
	}
}

func TestVisualColdPolicyVoyageRetryRechecksExactInput(t *testing.T) {
	f, _, work := workerFixture(t, 1)
	item := work[0]
	token, err := f.Store.PrepareVisualPublication(item.policyContext, store.PreparedVisualPublication{Claim: item.Claim, RepresentativeAttachmentID: item.Candidate.RepresentativeAttachmentID, Role: item.Candidate.Role, RoleSource: item.Candidate.RoleSource})
	require.NoError(t, err)
	ctx := store.WithConversationPolicyVisualTokens(item.policyContext, []string{token})
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			_, updateErr := f.Store.DB().ExecContext(r.Context(), f.Store.Rebind(`UPDATE messages SET subject='changed before retry' WHERE id=?`), item.Claim.Owner.MessageID)
			assert.NoError(t, updateErr)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		echoVectors(t)(w, r)
	}))
	defer server.Close()
	provider := newServerProvider(t, server, DefaultMediaPolicy())
	_, err = provider.EmbedDocuments(ctx, []DocumentInput{item.Document})
	require.Error(t, err)
	assert.Equal(t, int64(1), requests.Load(), "content edit must prevent the actual HTTP retry")
}

func TestVisualColdPolicyActivationKeepsCompletedScanAuthority(t *testing.T) {
	requirements := require.New(t)

	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy"))
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/empty-policy")
	result, err := reconciler.FullReconcile(t.Context())
	requirements.NoError(err)
	requirements.Empty(result.Work)
	requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Cold))
	requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Full))
	_, err = reconciler.Activate(t.Context())
	assert.ErrorIs(t, err, store.ErrConversationPolicyStale)
}

func TestVisualColdPolicyPromotionRestartsBoundedScan(t *testing.T) {
	requirements := require.New(t)

	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	defaultRoom := f.ConvID
	coldRoom, err := f.Store.EnsureConversation(f.Source.ID, "cold-low-id", "Cold low ID")
	requirements.NoError(err)
	f.ConvID = coldRoom
	low := testVisualCandidate(t, f, "low", strings.Repeat("a", 64))
	requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "cold-low-id", conversationpolicy.Cold))
	f.ConvID = defaultRoom
	testVisualCandidate(t, f, "middle", strings.Repeat("b", 64))
	testVisualCandidate(t, f, "high", strings.Repeat("c", 64))
	r := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/promotion-scan")
	r.config.PageSize = 1
	r.config.MaxScanPagesPerPass = 1
	first, err := r.FullReconcile(t.Context())
	requirements.NoError(err)
	requirements.Len(first.Work, 1)
	publishReconciledWork(t, f, first.Work[0])
	skipped, err := r.FullReconcile(t.Context())
	requirements.NoError(err)
	requirements.Empty(skipped.Work)
	requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "cold-low-id", conversationpolicy.Full))
	next, err := r.FullReconcile(t.Context())
	requirements.NoError(err)
	requirements.Len(next.Work, 1)
	assert.Equal(t, low, next.Work[0].Claim.Owner.MessageID)
}

func TestVisualColdPolicyReconcileDoesNotRetainCanceledPass(t *testing.T) {
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	r := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/canceled-pass")
	pass, cancel := context.WithCancel(t.Context())
	_, err := r.FullReconcile(pass)
	require.NoError(t, err)
	cancel()
	_, err = r.FullReconcile(t.Context())
	require.NoError(t, err, "only original authority, not previous pass cancellation, crosses passes")
}

func TestVisualColdPolicyNewScanCannotReplaceOldCallerAuthority(t *testing.T) {
	requirements := require.New(t)

	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy"))
	original, err := f.Store.CaptureConversationPolicyWork(t.Context())
	requirements.NoError(err)
	requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Cold))
	requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Full))
	r := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/old-caller")
	_, err = r.FullReconcile(t.Context())
	requirements.NoError(err)
	_, err = r.Activate(original)
	assert.ErrorIs(t, err, store.ErrConversationPolicyStale)
}

func TestVisualColdPolicyCanceledUncapturedWorkReleasesClaims(t *testing.T) {
	f, _, work := workerFixture(t, 1)
	// WorkItem remains a public input; callers may supply already-claimed work
	// without a reconciliation context, which the worker captures itself.
	work[0].policyContext = nil
	worker := newTestVisualWorker(t, f, &fakeVisualProvider{}, newFakeVisualBackend())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NotPanics(t, func() {
		_, err := worker.Run(ctx, work)
		assert.ErrorIs(t, err, context.Canceled)
	})
	_, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{GenerationID: work[0].Claim.GenerationID, Owner: work[0].Candidate.Owner, ProposedRevision: work[0].Document.Revision, LeaseOwner: "successor", Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: work[0].Claim.SourceFence})
	require.NoError(t, err)
	assert.True(t, acquired)
}
