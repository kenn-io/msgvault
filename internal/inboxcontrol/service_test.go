package inboxcontrol_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// This provider implements the external dispatch contract. The controller,
// archive resolution, source lease and durable ledger below are production code.
type controlProvider struct {
	state             inboxcontrol.State
	dispatches, reads int
	previews          int
	dispatch          func(context.Context) error
	observeError      error
	onObserve         func(context.Context, int, inboxcontrol.Request) error
	onPreview         func(context.Context, inboxcontrol.Request) error
	onFolders         func(context.Context, inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error)
	verifyError       error
	previewLocation   string
	previewFolders    []inboxcontrol.Folder
	dispatchRequest   func(inboxcontrol.Request) error
}

func (p *controlProvider) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	p.reads++
	if p.onObserve != nil {
		if err := p.onObserve(ctx, p.reads, request); err != nil {
			return inboxcontrol.State{}, err
		}
	}
	if p.observeError != nil {
		return inboxcontrol.State{}, p.observeError
	}
	return p.state, ctx.Err()
}
func (p *controlProvider) Preview(ctx context.Context, request inboxcontrol.Request, state inboxcontrol.State) (inboxcontrol.State, error) {
	p.previews++
	if p.onPreview != nil {
		if err := p.onPreview(ctx, request); err != nil {
			return inboxcontrol.State{}, err
		}
	}
	if request.Operation != inboxcontrol.OpArchive || state.Inbox == nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	state.Inbox = new(false)
	state.Location = p.previewLocation
	state.Folders = p.previewFolders
	return state, nil
}
func (p *controlProvider) Dispatch(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	p.dispatches++
	if p.dispatchRequest != nil {
		if err := p.dispatchRequest(request); err != nil {
			return inboxcontrol.DispatchResult{}, err
		}
	}
	if p.dispatch != nil {
		if err := p.dispatch(ctx); err != nil {
			return inboxcontrol.DispatchResult{}, err
		}
	}
	p.state.Inbox = new(false)
	return inboxcontrol.DispatchResult{}, nil
}
func (p *controlProvider) Verify(_ inboxcontrol.Request, before, projected, after inboxcontrol.State) error {
	if p.verifyError != nil {
		return p.verifyError
	}
	if after.Inbox == nil || *after.Inbox {
		return inboxcontrol.ErrPlanChanged
	}
	return nil
}
func (p *controlProvider) Folders(ctx context.Context, source inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	if p.onFolders != nil {
		return p.onFolders(ctx, source)
	}
	return nil, nil
}

type controlFixture struct {
	archive   *storetest.Fixture
	service   *inboxcontrol.Service
	provider  *controlProvider
	principal inboxcontrol.Principal
	request   inboxcontrol.Request
	now       time.Time
	denied    bool
	gateCalls int
	gate      func(context.Context) (func(), error)
}

func TestInboxControlRejectsPreFixPreviewWithCollapsedEmptyEvidence(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newControlFixture(t)
	// The previous serializer omitted known-empty Tags/Flags, producing the
	// same signed representation as nil. Model that historical provider contract
	// through the real Service and Store; never accept both fingerprints.
	preview, err := f.service.Control(t.Context(), f.request, f.principal, f.gate)
	requirements.NoError(err)
	f.request.DryRun = false
	f.request.Expected = preview.Before
	f.request.PreviewToken = preview.PreviewToken
	f.request.IdempotencyKey = "historical-empty-preview"
	f.provider.state.Tags = []string{}
	f.provider.state.Flags = []string{}
	_, err = f.service.Control(t.Context(), f.request, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrPlanChanged)
	assertions.Zero(f.provider.dispatches)
	receipt, err := f.archive.Store.LookupInboxReceipt(t.Context(), f.principal.ID, f.request.Target.SourceID, f.request.IdempotencyKey)
	requirements.NoError(err)
	assertions.Nil(receipt, "a stale signed preview never obtains a dispatch claim")
}

func newControlFixture(t *testing.T) *controlFixture {
	t.Helper()
	a := storetest.New(t)
	target := inboxcontrol.Target{SourceID: a.Source.ID, SourceType: "gmail", SourceIdentifier: a.Source.Identifier, AccountID: a.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: a.CreateMessage("controller-message"), ProviderID: "controller-message"}
	f := &controlFixture{archive: a, principal: inboxcontrol.Principal{ID: "delegate-fixture"}, now: time.Now().UTC(), request: inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}}
	f.provider = &controlProvider{state: inboxcontrol.State{Target: target, Inbox: new(true), ObservedAt: f.now}}
	f.service = &inboxcontrol.Service{
		Ledger: a.Store, Key: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return f.now },
		Authorize: func(ctx context.Context, principal inboxcontrol.Principal, request inboxcontrol.Request) error {
			if f.denied || principal.ID != f.principal.ID {
				return inboxcontrol.ErrDenied
			}
			return nil
		},
		Resolve: func(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
			bound, err := a.Store.EmailTagTargetContext(ctx, request.Target.ItemID, request.Target.Mailbox)
			if err != nil {
				return nil, err
			}
			if bound.SourceID != request.Target.SourceID || bound.SourceMessageID != request.Target.ProviderID || request.Target.SourceIdentifier != a.Source.Identifier || request.Target.AccountID != a.Source.Identifier {
				return nil, inboxcontrol.ErrDenied
			}
			return f.provider, nil
		},
		AcquireSource: func(ctx context.Context, sourceID int64) (func(), error) {
			execution, err := a.Store.AcquireSyncExecutionContext(ctx, sourceID)
			if err != nil {
				return nil, err
			}
			return func() { _ = execution.Release() }, nil
		},
		ReconcileState: func(ctx context.Context, before, after inboxcontrol.State) error {
			return a.Store.ReconcileInboxProviderState(ctx, before.Target, before, after)
		},
	}
	f.gate = func(context.Context) (func(), error) { f.gateCalls++; return func() {}, nil }
	return f
}

func (f *controlFixture) execution(t *testing.T) inboxcontrol.Request {
	t.Helper()
	preview, err := f.service.Control(t.Context(), f.request, f.principal, f.gate)
	require.NoError(t, err)
	require.NotEmpty(t, preview.PreviewToken)
	r := f.request
	r.DryRun, r.Expected, r.PreviewToken, r.IdempotencyKey = false, preview.Before, preview.PreviewToken, "operation-fixture"
	return r
}

func TestInboxControlPreviewDoesNotWrite(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	result, err := f.service.Control(t.Context(), f.request, f.principal, f.gate)
	requirements.NoError(err)
	requirements.NotNil(result.Before)
	requirements.NotNil(result.Projected)
	assertions.True(*result.Before.Inbox)
	assertions.False(*result.Projected.Inbox)
	assertions.Equal(f.now.Add(5*time.Minute), result.ExpiresAt)
	assertions.Equal(0, f.gateCalls)
	assertions.Equal(0, f.provider.dispatches)
	var receipts, observations int
	requirements.NoError(f.archive.Store.DB().QueryRow("SELECT COUNT(*) FROM inbox_operation_receipts").Scan(&receipts))
	requirements.NoError(f.archive.Store.DB().QueryRow("SELECT COUNT(*) FROM inbox_provider_states").Scan(&observations))
	assertions.Equal(0, receipts)
	assertions.Equal(0, observations)
}

func TestInboxControlReauthorizesAfterObservationBeforeReturningMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func(*controlFixture) inboxcontrol.Request
	}{
		{"get-state", func(f *controlFixture) inboxcontrol.Request {
			return inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: f.request.Target}
		}},
		{"preview", func(f *controlFixture) inboxcontrol.Request { return f.request }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newControlFixture(t)
			f.provider.onObserve = func(context.Context, int, inboxcontrol.Request) error {
				f.denied = true
				return nil
			}

			result, err := f.service.Control(t.Context(), tc.request(f), f.principal, f.gate)
			requirements.ErrorIs(err, inboxcontrol.ErrDenied)
			assertions.Nil(result)
			assertions.Zero(f.provider.dispatches)
			if tc.name == "preview" {
				assertions.Zero(f.provider.previews, "a revoked read grant stops before preview I/O")
			}
		})
	}
}

func TestInboxControlReauthorizesAfterProviderPreviewBeforeSigning(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newControlFixture(t)
	f.provider.onPreview = func(context.Context, inboxcontrol.Request) error {
		f.denied = true
		return nil
	}

	result, err := f.service.Control(t.Context(), f.request, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Nil(result)
	assertions.Equal(1, f.provider.previews)
}

func TestInboxControlReauthorizesAfterFolderRead(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newControlFixture(t)
	target := *f.request.Target
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	f.service.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) { return f.provider, nil }
	f.provider.onFolders = func(context.Context, inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
		f.denied = true
		return []inboxcontrol.Folder{{ID: "private-folder", Name: "Private folder"}}, nil
	}

	result, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpListFolders, Source: &source}, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Nil(result)
}

func TestInboxControlReauthorizesAfterCapabilitiesRead(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newControlFixture(t)
	target := *f.request.Target
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	cp := &capabilityProvider{controlProvider: f.provider, capabilities: &inboxcontrol.Capabilities{
		Source: source, LocationModel: "labels", ObservedAt: f.now,
		Operations: []inboxcontrol.Capability{{Operation: inboxcontrol.OpGetState, Status: inboxcontrol.CapabilitySupported}},
	}}
	cp.onCapabilities = func(context.Context, inboxcontrol.Request) error {
		f.denied = true
		return nil
	}
	f.service.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) { return cp, nil }

	result, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Nil(result)
}

func TestInboxControlVerifiedReplayBeforeExpiryAndChangedState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	assertions.Equal(1, f.gateCalls)
	f.now = f.now.Add(time.Hour)
	f.provider.state.Inbox = new(true)
	reads := f.provider.reads
	replay, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(reads, f.provider.reads)
	assertions.Equal(1, f.provider.dispatches)
	f.denied = true
	_, err = f.service.Control(t.Context(), r, f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrDenied)
	assertions.Equal(1, f.provider.dispatches)
}

func TestInboxControlChangedIntentCannotReuseKey(t *testing.T) {
	f := newControlFixture(t)
	r := f.execution(t)
	_, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.NoError(t, err)
	r.Operation = inboxcontrol.OpSetUnread
	_, err = f.service.Control(t.Context(), r, f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrConflict)
	assert.Equal(t, 1, f.provider.dispatches)
}

func TestInboxControlStaleOrForgedPreviewNeverDispatches(t *testing.T) {
	for _, change := range []string{"state", "token", "expiry", "account", "expected"} {
		t.Run(change, func(t *testing.T) {
			f := newControlFixture(t)
			r := f.execution(t)
			switch change {
			case "state":
				f.provider.state.Revision = "changed"
			case "token":
				r.PreviewToken += "x"
			case "expiry":
				f.now = f.now.Add(5 * time.Minute)
			case "account":
				target := *r.Target
				target.AccountID = "other@example.test"
				r.Target = &target
				expected := *r.Expected
				expected.Target = target
				r.Expected = &expected
			case "expected":
				expected := *r.Expected
				expected.Inbox = new(false)
				r.Expected = &expected
			}
			_, err := f.service.Control(t.Context(), r, f.principal, f.gate)
			require.Error(t, err)
			assert.Equal(t, 0, f.provider.dispatches)
		})
	}
}

func TestInboxControlSourceLeaseExcludesSync(t *testing.T) {
	f := newControlFixture(t)
	r := f.execution(t)
	execution, err := f.archive.Store.AcquireSyncExecutionContext(t.Context(), r.Target.SourceID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = execution.Release() })
	_, err = f.service.Control(t.Context(), r, f.principal, f.gate)
	require.Error(t, err)
	assert.Equal(t, 0, f.provider.dispatches)
}

func TestInboxControlLostResponseNeverRetriesAndSanitizesError(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	f.provider.dispatch = func(context.Context) error { return errors.New("Bearer secret-fixture https://personal.example.test") }
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	assertions.NotContains(err.Error(), "secret-fixture")
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
	assertions.Equal("dispatch-unverified", result.Receipt.FailureCode)
	_, err = f.service.Control(t.Context(), r, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(1, f.provider.dispatches)
}

func TestInboxControlReauthorizesAfterDispatchBeforeReturningResults(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dispatchErr error
		wantStatus  inboxcontrol.Status
	}{
		{name: "verified dispatch", wantStatus: inboxcontrol.StatusVerified},
		{name: "unknown dispatch", dispatchErr: errors.New("fixture lost dispatch response"), wantStatus: inboxcontrol.StatusUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newControlFixture(t)
			r := f.execution(t)
			originalAuthorize := f.service.Authorize
			var reauthorizedIntent inboxcontrol.Request
			var reauthorizedReceiptID string
			f.service.Authorize = func(ctx context.Context, principal inboxcontrol.Principal, request inboxcontrol.Request) error {
				if f.denied {
					reauthorizedIntent = request
					reauthorizedReceiptID, _ = inboxcontrol.ReceiptAuthorityID(ctx)
				}
				return originalAuthorize(ctx, principal, request)
			}
			f.provider.dispatch = func(context.Context) error {
				f.denied = true
				return tc.dispatchErr
			}

			result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
			require.ErrorIs(t, err, inboxcontrol.ErrDenied)
			assert.Nil(t, result)
			stored, err := f.archive.Store.LookupInboxReceipt(t.Context(), f.principal.ID, r.Target.SourceID, r.IdempotencyKey)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, tc.wantStatus, stored.Status)
			assert.Equal(t, stored.ID, reauthorizedReceiptID)
			assert.Equal(t, inboxcontrol.OpArchive, reauthorizedIntent.Operation)
			assert.Equal(t, *r.Target, *reauthorizedIntent.Target)
			assert.True(t, reauthorizedIntent.DryRun)
			assert.Nil(t, reauthorizedIntent.Expected)
			assert.Empty(t, reauthorizedIntent.PreviewToken)
			assert.Empty(t, reauthorizedIntent.IdempotencyKey)
			assert.Equal(t, 1, f.provider.dispatches)
		})
	}
}

func TestInboxControlCanceledDispatchFinalizesReceipt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	f.provider.dispatch = func(context.Context) error { cancel(); return context.Canceled }
	result, err := f.service.Control(ctx, r, f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	requirements.NotNil(result.Receipt)
	stored, err := f.archive.Store.GetInboxReceipt(t.Context(), result.Receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusUnknown, stored.Status)
	assertions.NotNil(stored.FinishedAt)
}

func TestInboxControlReadbackDoesNotConsumeCompletionDeadline(t *testing.T) {
	f := newControlFixture(t)
	r := f.execution(t)
	f.provider.onObserve = func(ctx context.Context, read int, _ inboxcontrol.Request) error {
		if read == 3 {
			select {
			case <-time.After(5100 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	f.service.ReconcileState = func(ctx context.Context, _, _ inboxcontrol.State) error {
		return ctx.Err()
	}

	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.NoError(t, err)
	require.NotNil(t, result.Receipt)
	assert.Equal(t, inboxcontrol.StatusVerified, result.Receipt.Status)
}

func TestInboxControlLocalFailureReconcilesWithoutDispatch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	reconcile := f.service.ReconcileState
	f.service.ReconcileState = func(context.Context, inboxcontrol.State, inboxcontrol.State) error {
		return errors.New("fixture local failure")
	}
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrReconcileOnly)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusReconcileOnly, result.Receipt.Status)
	f.service.ReconcileState = reconcile
	result, err = f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	assertions.Equal(1, f.provider.dispatches)
}

func TestInboxControlReceiptReauthorizesAfterRecoveryObservation(t *testing.T) {
	t.Run("settles before suppressing result", func(t *testing.T) {
		f := newControlFixture(t)
		r := f.execution(t)
		reconcile := f.service.ReconcileState
		f.service.ReconcileState = func(context.Context, inboxcontrol.State, inboxcontrol.State) error {
			return errors.New("fixture local failure")
		}
		pending, err := f.service.Control(t.Context(), r, f.principal, f.gate)
		require.ErrorIs(t, err, inboxcontrol.ErrReconcileOnly)
		require.NotNil(t, pending)
		f.service.ReconcileState = reconcile
		f.provider.onObserve = func(context.Context, int, inboxcontrol.Request) error {
			f.denied = true
			return nil
		}

		result, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: pending.Receipt.ID}, f.principal, f.gate)
		require.ErrorIs(t, err, inboxcontrol.ErrDenied)
		assert.Nil(t, result)
		stored, err := f.archive.Store.GetInboxReceipt(t.Context(), pending.Receipt.ID)
		require.NoError(t, err)
		assert.Equal(t, inboxcontrol.StatusVerified, stored.Status)
		assert.Equal(t, 1, f.provider.dispatches)
	})

	t.Run("suppresses readback error", func(t *testing.T) {
		f := newControlFixture(t)
		r := f.execution(t)
		f.service.ReconcileState = func(context.Context, inboxcontrol.State, inboxcontrol.State) error {
			return errors.New("fixture local failure")
		}
		pending, err := f.service.Control(t.Context(), r, f.principal, f.gate)
		require.ErrorIs(t, err, inboxcontrol.ErrReconcileOnly)
		require.NotNil(t, pending)
		f.provider.onObserve = func(context.Context, int, inboxcontrol.Request) error {
			f.denied = true
			return nil
		}
		f.provider.observeError = errors.New("fixture readback failure")

		result, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: pending.Receipt.ID}, f.principal, f.gate)
		require.ErrorIs(t, err, inboxcontrol.ErrDenied)
		assert.Nil(t, result)
		stored, err := f.archive.Store.GetInboxReceipt(t.Context(), pending.Receipt.ID)
		require.NoError(t, err)
		assert.Equal(t, inboxcontrol.StatusReconcileOnly, stored.Status)
		assert.Equal(t, 1, f.provider.dispatches)
	})
}

func TestInboxControlReceiptRequiresCurrentOriginalAuthority(t *testing.T) {
	f := newControlFixture(t)
	r := f.execution(t)
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.NoError(t, err)
	read := inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.Receipt.ID}
	other := inboxcontrol.Principal{ID: "other-delegate"}
	_, err = f.service.Control(t.Context(), read, other, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrDenied)
	f.denied = true
	_, err = f.service.Control(t.Context(), read, f.principal, f.gate)
	assert.ErrorIs(t, err, inboxcontrol.ErrDenied)
}

func TestInboxControlReadbackIdentityMismatchRemainsUnknown(t *testing.T) {
	assertions := assert.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	f.provider.dispatch = func(context.Context) error { f.provider.state.Target.ProviderID = "different-message"; return nil }
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	require.NotNil(t, result.Receipt)
	assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
	assertions.Nil(result.Receipt.After)
}

func TestInboxControlChangedProjectionInvalidatesPreview(t *testing.T) {
	f := newControlFixture(t)
	r := f.execution(t)
	f.provider.previewLocation = "changed-destination"
	_, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.Error(t, err)
	assert.Equal(t, 0, f.provider.dispatches)
}

type finishFailureLedger struct{ inboxcontrol.Ledger }

func (l finishFailureLedger) FinishInboxReceipt(context.Context, string, inboxcontrol.Status, *inboxcontrol.State, string) error {
	return errors.New("fixture storage unavailable")
}

func TestInboxControlFinalizationFailureRetainsDispatchEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	f.service.Ledger = finishFailureLedger{f.archive.Store}
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrReconcileOnly)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusDispatching, result.Receipt.Status)
	assertions.NotNil(result.Receipt.DispatchedAt)
	_, err = f.service.Control(t.Context(), r, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(1, f.provider.dispatches)
	f.service.Ledger = f.archive.Store
	reconciled, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, reconciled.Receipt.Status)
	assertions.Equal(1, f.provider.dispatches)
}

func TestInboxControlDispatchRequiresCommittedReceiptAndSourceLease(t *testing.T) {
	f := newControlFixture(t)
	r := f.execution(t)
	f.provider.dispatch = func(ctx context.Context) error {
		receipt, err := f.archive.Store.LookupInboxReceipt(ctx, f.principal.ID, r.Target.SourceID, r.IdempotencyKey)
		require.NoError(t, err)
		require.NotNil(t, receipt)
		assert.Equal(t, inboxcontrol.StatusDispatching, receipt.Status)
		assert.NotNil(t, receipt.DispatchedAt)
		lease, err := f.archive.Store.AcquireSyncExecutionContext(ctx, r.Target.SourceID)
		assert.Error(t, err)
		if lease != nil {
			_ = lease.Release()
		}
		return nil
	}
	_, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	require.NoError(t, err)
}

func TestInboxControlReceiptRetainsAuthenticatedProjection(t *testing.T) {
	requirements := require.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	requirements.NoError(err)
	stored, err := f.archive.Store.GetInboxReceipt(t.Context(), result.Receipt.ID)
	requirements.NoError(err)
	encoded, err := json.Marshal(stored)
	requirements.NoError(err)
	var durable struct {
		Projected *inboxcontrol.State `json:"projected"`
	}
	requirements.NoError(json.Unmarshal(encoded, &durable))
	requirements.NotNil(durable.Projected, "reconciliation must retain the preview's exact projected destination and delta")
	assert.Equal(t, result.Projected, durable.Projected)
}

type capabilityProvider struct {
	*controlProvider

	capabilities   *inboxcontrol.Capabilities
	onCapabilities func(context.Context, inboxcontrol.Request) error
}

func (p *capabilityProvider) Capabilities(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
	if p.onCapabilities != nil {
		if err := p.onCapabilities(ctx, request); err != nil {
			return nil, err
		}
	}
	return p.capabilities, nil
}

func TestInboxControlCapabilitiesUseCurrentSourceGrantsWithoutWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	target := *f.request.Target
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	cp := &capabilityProvider{controlProvider: f.provider, capabilities: &inboxcontrol.Capabilities{Source: source, LocationModel: "labels", ObservedAt: f.now, Operations: []inboxcontrol.Capability{{Operation: inboxcontrol.OpGetState, Status: inboxcontrol.CapabilitySupported}, {Operation: inboxcontrol.OpTags, Status: inboxcontrol.CapabilitySupported}, {Operation: inboxcontrol.OpMove, Status: inboxcontrol.CapabilityUnsupported, Reason: "native limitation"}}}}
	f.service.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) { return cp, nil }
	f.service.Authorize = func(_ context.Context, _ inboxcontrol.Principal, request inboxcontrol.Request) error {
		if f.denied || request.Operation == inboxcontrol.OpTags {
			return inboxcontrol.ErrDenied
		}
		return nil
	}
	request := inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}
	result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.NoError(err)
	requirements.NotNil(result.Capabilities)
	requirements.Len(result.Capabilities.Operations, 3)
	assertions.Equal(inboxcontrol.CapabilitySupported, result.Capabilities.Operations[0].Status)
	assertions.Equal(inboxcontrol.CapabilityPermissionRequired, result.Capabilities.Operations[1].Status)
	assertions.Equal(inboxcontrol.CapabilityUnsupported, result.Capabilities.Operations[2].Status)
	assertions.Equal(inboxcontrol.CapabilitySupported, cp.capabilities.Operations[1].Status, "authorization filtering must not change provider evidence")
	assertions.Equal(0, f.gateCalls)
	assertions.Equal(0, f.provider.dispatches)
	var receipts int
	requirements.NoError(f.archive.Store.DB().QueryRow("SELECT COUNT(*) FROM inbox_operation_receipts").Scan(&receipts))
	assertions.Equal(0, receipts)
	f.denied = true
	_, err = f.service.Control(t.Context(), request, f.principal, f.gate)
	assertions.ErrorIs(err, inboxcontrol.ErrDenied)
}

func TestInboxControlCapabilitiesRejectUnprovedMetadata(t *testing.T) {
	for _, invalid := range []string{"source", "time", "status", "operation", "duplicate"} {
		t.Run(invalid, func(t *testing.T) {
			f := newControlFixture(t)
			target := *f.request.Target
			source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
			caps := &inboxcontrol.Capabilities{Source: source, LocationModel: "labels", ObservedAt: f.now, Operations: []inboxcontrol.Capability{{Operation: inboxcontrol.OpGetState, Status: inboxcontrol.CapabilitySupported}}}
			switch invalid {
			case "source":
				caps.Source.AccountID = "other@example.com"
			case "time":
				caps.ObservedAt = time.Time{}
			case "status":
				caps.Operations[0].Status = "unknown"
			case "operation":
				caps.Operations[0].Operation = "send"
			case "duplicate":
				caps.Operations = append(caps.Operations, caps.Operations[0])
			}
			cp := &capabilityProvider{controlProvider: f.provider, capabilities: caps}
			f.service.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) { return cp, nil }
			_, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}, f.principal, f.gate)
			require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			assert.Equal(t, 0, f.gateCalls)
		})
	}
}

func TestInboxControlReconcileDoesNotConsumeReceiptSettlementDeadline(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	f.service.ReconcileState = func(ctx context.Context, _, _ inboxcontrol.State) error {
		<-ctx.Done()
		return ctx.Err()
	}
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusReconcileOnly, result.Receipt.Status)
	assertions.NotNil(result.Receipt.DispatchedAt)
	assertions.NotNil(result.Receipt.FinishedAt)
	assertions.Equal(1, f.provider.dispatches)
	// An expired preview is still an authorized replay of durable evidence.
	f.now = f.now.Add(time.Hour)
	replay, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(1, f.provider.dispatches)
}

func TestInboxReceiptReconcileHasSeparateSettlementDeadline(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newControlFixture(t)
	r := f.execution(t)
	f.service.ReconcileState = func(context.Context, inboxcontrol.State, inboxcontrol.State) error {
		return errors.New("synthetic local failure")
	}
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
	requirements.NotNil(result.Receipt)
	f.service.ReconcileState = func(ctx context.Context, _, _ inboxcontrol.State) error {
		<-ctx.Done()
		return ctx.Err()
	}
	request := inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}
	result, err = f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusReconcileOnly, result.Receipt.Status)
	assertions.Equal("local-reconcile-failed", result.Receipt.FailureCode)
	assertions.Equal(1, f.provider.dispatches)
}
