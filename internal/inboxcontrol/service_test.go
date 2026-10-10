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
	dispatch          func(context.Context) error
	observeError      error
	onObserve         func(context.Context, int, inboxcontrol.Request) error
	verifyError       error
	previewLocation   string
	previewFolders    []inboxcontrol.Folder
	dispatchRequest   func(inboxcontrol.Request) error
	dispatchResult    inboxcontrol.DispatchResult
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
func (p *controlProvider) Preview(_ context.Context, request inboxcontrol.Request, state inboxcontrol.State) (inboxcontrol.State, error) {
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
	return p.dispatchResult, nil
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
func (p *controlProvider) Folders(context.Context, inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
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

func TestInboxControlPersistsMovedMappingBeforeReadback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	target := *f.request.Target
	target.SourceType, target.Mailbox, target.UIDValidity, target.UID = "imap", "INBOX", 7, 11
	f.request.Target = &target
	f.provider.state.Target = target
	f.service.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) {
		return f.provider, nil
	}
	destination := target
	destination.Mailbox, destination.UIDValidity, destination.UID = "Archive", 9, 4
	f.provider.dispatchResult.Target = &destination
	f.provider.onObserve = func(ctx context.Context, read int, _ inboxcontrol.Request) error {
		if read != 3 {
			return nil
		}
		var receiptID string
		if err := f.archive.Store.DB().QueryRowContext(ctx, `SELECT id FROM inbox_operation_receipts WHERE status = 'dispatching'`).Scan(&receiptID); err != nil {
			return err
		}
		receipt, err := f.archive.Store.GetInboxReceipt(ctx, receiptID)
		if err != nil {
			return err
		}
		if receipt == nil {
			assertions.Fail("dispatching receipt was not available before readback")
			return nil
		}
		if assertions.NotNil(receipt.After, "proved destination mapping must be persisted before readback") {
			assertions.Equal(destination, receipt.After.Target)
		}
		return errors.New("readback timed out")
	}

	r := f.execution(t)
	result, err := f.service.Control(t.Context(), r, f.principal, f.gate)
	assertions.Equal(3, f.provider.reads, "preview, dispatch preflight, and post-dispatch readback should each observe once")
	requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
	assertions.NotNil(result.Receipt.After)
	assertions.Equal(destination, result.Receipt.After.Target)
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

	capabilities *inboxcontrol.Capabilities
}

func (p *capabilityProvider) Capabilities(context.Context, inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
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

func TestInboxControlDispatchReceivesSignedIMAPArchiveFolder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	target := *f.request.Target
	target.SourceType, target.Mailbox, target.UIDValidity, target.UID = "imap", "INBOX", 1, 1
	f.request.Target = &target
	f.provider.state.Target = target
	folder := inboxcontrol.Folder{ID: "Archive", Name: "Archive", UIDValidity: 2}
	f.provider.previewFolders = []inboxcontrol.Folder{folder}
	f.provider.previewLocation = "Archive"
	f.service.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) { return f.provider, nil }
	f.service.ReconcileState = func(context.Context, inboxcontrol.State, inboxcontrol.State) error { return nil }
	f.provider.dispatchRequest = func(request inboxcontrol.Request) error {
		if request.ResolvedFolder == nil || *request.ResolvedFolder != folder {
			return inboxcontrol.ErrNoWrite
		}
		return nil
	}
	result, err := f.service.Control(t.Context(), f.execution(t), f.principal, f.gate)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	assertions.Nil(result.Receipt.Intent.ResolvedFolder)
	assertions.Equal([]inboxcontrol.Folder{folder}, result.Receipt.Projected.Folders)
}

// A native MOVE response can prove its destination while source disappearance
// remains unproved. The real controller and durable Store must retain unknown.
type moveProofProvider struct {
	*controlProvider

	sourceError error
}

func (p *moveProofProvider) Dispatch(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	if _, err := p.controlProvider.Dispatch(ctx, request, before); err != nil {
		return inboxcontrol.DispatchResult{}, err
	}
	target := before.Target
	target.Mailbox, target.UIDValidity, target.UID = "Archive", 2, 3
	p.state.Target = target
	return inboxcontrol.DispatchResult{Target: &target}, nil
}
func (p *moveProofProvider) VerifyMoveSource(context.Context, inboxcontrol.Target) error {
	return p.sourceError
}

func TestInboxControlMoveRequiresIndependentSourceProof(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newControlFixture(t)
	target := *f.request.Target
	target.SourceType, target.Mailbox, target.UIDValidity, target.UID = "imap", "INBOX", 1, 1
	f.request.Target = &target
	f.provider.state.Target = target
	f.provider.previewLocation = "Archive"
	provider := &moveProofProvider{controlProvider: f.provider, sourceError: inboxcontrol.ErrOutcomeUnknown}
	f.service.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) { return provider, nil }
	reconciled := false
	f.service.ReconcileState = func(context.Context, inboxcontrol.State, inboxcontrol.State) error { reconciled = true; return nil }
	result, err := f.service.Control(t.Context(), f.execution(t), f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
	assertions.False(reconciled)
	requirements.NotNil(result.Receipt.After)
	assertions.Equal("Archive", result.Receipt.After.Target.Mailbox)
	// Reconciliation repeats only source/destination reads, never MOVE.
	_, err = f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	assertions.False(reconciled)
	assertions.Equal(1, f.provider.dispatches)
	provider.sourceError = nil
	recovered, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, recovered.Receipt.Status)
	assertions.True(reconciled)
	assertions.Equal(1, f.provider.dispatches)
}
