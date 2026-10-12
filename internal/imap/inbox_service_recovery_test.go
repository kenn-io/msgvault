package imap

import (
	"context"
	"strings"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type inboxNativeServiceFixture struct {
	native    *inboxNativeFixture
	store     *store.Store
	service   *inboxcontrol.Service
	principal inboxcontrol.Principal
	gate      func(context.Context) (func(), error)
	resolves  int
}

// This observer checks the storage boundary immediately before delegating to
// the real provider's destination FETCH; no protocol command is replaced.
type inboxMappingReadbackObserver struct {
	*InboxProvider
	beforeRead func(inboxcontrol.Request) error
}

func (p *inboxMappingReadbackObserver) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	if request.Target != nil && request.Target.Mailbox == "Archive" {
		if err := p.beforeRead(request); err != nil {
			return inboxcontrol.State{}, err
		}
	}
	return p.InboxProvider.Observe(ctx, request)
}

func newInboxNativeServiceFixture(t *testing.T) *inboxNativeServiceFixture {
	return newInboxNativeServiceFixtureWithFault(t, "native")
}

func newInboxNativeServiceFixtureWithFault(t *testing.T, fault string) *inboxNativeServiceFixture {
	t.Helper()
	return newInboxNativeServiceFixtureWithProvider(t, newInboxNativeFixture(t, fault), []string{})
}

func newInboxNativeServiceFixtureWithProvider(t *testing.T, provider *inboxNativeFixture, flags []string) *inboxNativeServiceFixture {
	t.Helper()
	f := &inboxNativeServiceFixture{native: provider, store: testutil.NewTestStore(t), principal: inboxcontrol.Principal{ID: "native-recovery-principal"}}
	native, st := f.native, f.store
	source, err := st.GetOrCreateSource("imap", native.source.SourceIdentifier)
	require.NoError(t, err)
	config, err := native.provider.client.config.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, config))
	conversation, err := st.EnsureConversation(source.ID, "native-recovery", "Synthetic native recovery")
	require.NoError(t, err)
	item, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "synthetic-message", ConversationID: conversation, MessageType: "email"})
	require.NoError(t, err)
	native.source.SourceID, native.target.SourceID, native.target.ItemID = source.ID, source.ID, item
	native.provider.source = native.source
	deltas := []store.IMAPMailboxDelta{
		{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: native.target.UIDValidity, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{UID: 1, CanonicalSourceMessageID: "synthetic-message", Flags: flags}}},
	}
	if native.destination.UIDValidity != 0 {
		deltas = append(deltas, store.IMAPMailboxDelta{Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: native.destination.UIDValidity, UIDNext: 2}})
	}
	require.NoError(t, st.ApplyIMAPMailboxDeltas(source.ID, deltas))
	f.gate = func(context.Context) (func(), error) { return func() {}, nil }
	f.service = &inboxcontrol.Service{Ledger: st, Key: []byte(strings.Repeat("k", 32)), Authorize: func(ctx context.Context, principal inboxcontrol.Principal, request inboxcontrol.Request) error {
		if principal != f.principal || request.Target == nil {
			return inboxcontrol.ErrDenied
		}
		if original, ok := inboxcontrol.ReceiptAuthorityTarget(ctx); ok && original == *request.Target {
			return f.store.ValidateInboxItemIdentityContext(ctx, original)
		}
		return f.store.ValidateInboxTargetContext(ctx, *request.Target)
	}, Resolve: func(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
		f.resolves++
		if request.Target == nil {
			return nil, inboxcontrol.ErrDenied
		}
		if mapped, ok := inboxcontrol.ReceiptMoveTarget(ctx); ok && mapped == *request.Target {
			if err := f.store.ValidateInboxItemIdentityContext(ctx, mapped); err != nil {
				return nil, err
			}
		} else if err := f.store.ValidateInboxTargetContext(ctx, *request.Target); err != nil {
			return nil, err
		}
		return native.provider, nil
	}, AcquireSource: func(ctx context.Context, id int64) (func(), error) {
		lease, err := f.store.AcquireInboxExecutionContext(ctx, id)
		if err != nil {
			return nil, err
		}
		return func() { _ = lease.Release() }, nil
	}, ReconcileState: func(ctx context.Context, before, after inboxcontrol.State) error {
		return f.store.ReconcileInboxProviderState(ctx, before.Target, before, after)
	}}
	return f
}

func TestInboxIMAPServiceRecoversLostStoreReplyWithoutSessionRecent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	wire := &inboxWireServer{flags: []string{`\Recent`}, modSeq: 10, lostReply: true}
	_, provider, target := newInboxWireServer(t, wire)
	f := newInboxNativeServiceFixtureWithProvider(t, &inboxNativeFixture{provider: provider, source: provider.source, target: target}, []string{`\Recent`})
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &f.native.target, DryRun: true}
	preview, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.NoError(err)
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-recent-recovery"
	result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
	requirements.NotNil(result)
	assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
	wire.mu.Lock()
	wire.flags = []string{`\Seen`}
	wire.mu.Unlock()
	// Load the durable receipt through the canonical recovery path after the
	// session-only marker disappears. No operation is submitted again.
	recovered, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, recovered.Receipt.ID)
	assertions.Equal(inboxcontrol.StatusVerified, recovered.Receipt.Status)
	requirements.NotNil(recovered.Receipt.Projected)
	requirements.NotNil(recovered.Receipt.After)
	assertions.Equal([]string{`\Recent`}, recovered.Receipt.Before.Flags)
	assertions.ElementsMatch([]string{`\Recent`, `\Seen`}, recovered.Receipt.Projected.Flags)
	assertions.Equal([]string{`\Seen`}, recovered.Receipt.After.Flags)
	state, err := f.store.GetInboxProviderState(t.Context(), f.native.target)
	requirements.NoError(err)
	requirements.NotNil(state)
	assertions.Equal([]string{`\Seen`}, state.Flags)
	wire.mu.Lock()
	defer wire.mu.Unlock()
	assertions.Len(wire.stores, 1)
	assertions.Equal(1, wire.writes)
}

func (f *inboxNativeServiceFixture) plannedMove(t *testing.T) inboxcontrol.Request {
	t.Helper()
	request := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &f.native.target, Destination: &f.native.destination, DryRun: true}
	preview, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	require.NoError(t, err)
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-recovery"
	return request
}

func TestInboxIMAPServiceRecoversAfterReconcileAndLostReceiptFinish(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newInboxNativeServiceFixture(t)
	request := f.plannedMove(t)
	// Exercise real storage failure at the receipt boundary after native MOVE
	// and local membership reconciliation, without replacing primary commands.
	var err error
	if f.store.IsPostgreSQL() {
		_, err = f.store.DB().Exec(`CREATE FUNCTION fail_native_receipt_finish() RETURNS trigger AS $$ BEGIN IF NEW.status IN ('verified','reconcile-only') THEN RAISE EXCEPTION 'synthetic receipt finalization failure'; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql`)
		requirements.NoError(err)
		_, err = f.store.DB().Exec(`CREATE TRIGGER fail_native_receipt_finish BEFORE UPDATE OF status ON inbox_operation_receipts FOR EACH ROW EXECUTE FUNCTION fail_native_receipt_finish()`)
	} else {
		_, err = f.store.DB().Exec(`CREATE TRIGGER fail_native_receipt_finish BEFORE UPDATE OF status ON inbox_operation_receipts WHEN NEW.status IN ('verified','reconcile-only') BEGIN SELECT RAISE(ABORT, 'synthetic receipt finalization failure'); END`)
	}
	requirements.NoError(err)
	result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
	requirements.NotNil(result)
	requirements.NotNil(result.Receipt.After)
	assertions.Equal(inboxcontrol.StatusDispatching, result.Receipt.Status)
	assertions.Equal("Archive", result.Receipt.After.Target.Mailbox)
	assertions.Equal(uint32(2), result.Receipt.After.Target.UID)
	assertions.Equal(int64(1), f.native.moves.Load())
	assertions.Zero(f.native.copies.Load())
	assertions.Zero(f.native.expunges.Load())
	var origins int
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND mailbox='INBOX'`), f.native.source.SourceID).Scan(&origins))
	assertions.Zero(origins)
	requirements.NoError(f.store.RecoverInboxReceipts(t.Context()))
	resolves := f.resolves
	replay, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(resolves, f.resolves, "same-key replay performs no provider resolution")
	_, err = f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(resolves, f.resolves, "receipt-get performs no provider resolution")
	drop := "DROP TRIGGER fail_native_receipt_finish"
	if f.store.IsPostgreSQL() {
		drop += " ON inbox_operation_receipts"
	}
	_, err = f.store.DB().Exec(drop)
	requirements.NoError(err)
	settled, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, settled.Receipt.Status)
	assertions.Equal(int64(1), f.native.moves.Load(), "receipt recovery never redispatches MOVE")
}

func TestInboxIMAPServiceRecoversMappedProviderTombstone(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newInboxNativeServiceFixture(t)
	request := f.plannedMove(t)
	reconcile := f.service.ReconcileState
	f.service.ReconcileState = func(ctx context.Context, before, after inboxcontrol.State) error {
		// Simulate a sync after remote MOVE but before interrupted local
		// reconciliation: origin is gone, destination has not been imported.
		err := f.store.ApplyIMAPMailboxDeltas(f.native.source.SourceID, []store.IMAPMailboxDelta{
			{Mailbox: "INBOX", Reset: true, State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: f.native.target.UIDValidity, UIDNext: 2}},
			{Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: f.native.destination.UIDValidity, UIDNext: 3}},
		})
		if err != nil {
			return err
		}
		return reconcile(ctx, before, after)
	}
	result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	assertions.Equal(int64(1), f.native.moves.Load())
	var memberships int
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND mailbox='Archive' AND message_id=?`), f.native.source.SourceID, f.native.target.ItemID).Scan(&memberships))
	assertions.Equal(1, memberships)
	var tombstones int
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT COUNT(*) FROM messages WHERE id=? AND deleted_from_source_at IS NOT NULL`), f.native.target.ItemID).Scan(&tombstones))
	assertions.Zero(tombstones)
}

func TestInboxIMAPServiceNeverRevivesLocallyDeletedItem(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newInboxNativeServiceFixture(t)
	request := f.plannedMove(t)
	reconcile := f.service.ReconcileState
	f.service.ReconcileState = func(ctx context.Context, before, after inboxcontrol.State) error {
		_, err := f.store.DB().Exec(f.store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), f.native.target.ItemID)
		if err != nil {
			return err
		}
		return reconcile(ctx, before, after)
	}
	result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
	assertions.Equal(inboxcontrol.StatusReconcileOnly, result.Receipt.Status)
	var live, destinations int
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT COUNT(*) FROM messages WHERE id=? AND deleted_at IS NULL`), f.native.target.ItemID).Scan(&live))
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND mailbox='Archive' AND message_id=?`), f.native.source.SourceID, f.native.target.ItemID).Scan(&destinations))
	assertions.Zero(live)
	assertions.Zero(destinations)
}

func TestInboxIMAPServicePersistsMappingBeforeDestinationRead(t *testing.T) {
	for _, failMapping := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable mapping", true: "mapping storage failure"}[failMapping], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newInboxNativeServiceFixture(t)
			request := f.plannedMove(t)
			reads := 0
			resolve := f.service.Resolve
			f.service.Resolve = func(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
				provider, err := resolve(ctx, request)
				if err != nil {
					return nil, err
				}
				return &inboxMappingReadbackObserver{InboxProvider: provider.(*InboxProvider), beforeRead: func(read inboxcontrol.Request) error {
					reads++
					stored, err := f.store.LookupInboxReceipt(t.Context(), f.principal.ID, f.native.source.SourceID, "native-recovery")
					requirements.NoError(err)
					requirements.NotNil(stored)
					requirements.NotNil(stored.After)
					assertions.Equal(inboxcontrol.StatusDispatching, stored.Status)
					assertions.Equal(*read.Target, stored.After.Target)
					assertions.Nil(stored.After.Read, "COPYUID mapping is not fabricated readback evidence")
					return nil
				}}, nil
			}
			if failMapping {
				var err error
				if f.store.IsPostgreSQL() {
					_, err = f.store.DB().Exec(`CREATE FUNCTION fail_native_mapping() RETURNS trigger AS $$ BEGIN IF OLD.after_json IS NULL AND NEW.after_json IS NOT NULL THEN RAISE EXCEPTION 'synthetic mapping storage failure'; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql`)
					requirements.NoError(err)
					_, err = f.store.DB().Exec(`CREATE TRIGGER fail_native_mapping BEFORE UPDATE OF after_json ON inbox_operation_receipts FOR EACH ROW EXECUTE FUNCTION fail_native_mapping()`)
				} else {
					_, err = f.store.DB().Exec(`CREATE TRIGGER fail_native_mapping BEFORE UPDATE OF after_json ON inbox_operation_receipts WHEN OLD.after_json IS NULL AND NEW.after_json IS NOT NULL BEGIN SELECT RAISE(ABORT, 'synthetic mapping storage failure'); END`)
				}
				requirements.NoError(err)
			}
			result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
			requirements.NotNil(result)
			assertions.Equal(int64(1), f.native.moves.Load())
			if failMapping {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				assertions.Zero(reads, "ambiguous mapping storage prevents destination FETCH")
				assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
				assertions.Nil(result.Receipt.After)
				replay, err := f.service.Control(t.Context(), request, f.principal, f.gate)
				requirements.NoError(err)
				assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
				assertions.Zero(reads)
				assertions.Equal(int64(1), f.native.moves.Load())
			} else {
				requirements.NoError(err)
				assertions.Equal(1, reads)
				assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
			}
		})
	}
}

func TestInboxIMAPServiceReplaysWhenMoveCompletesDuringOwnershipWait(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newInboxNativeServiceFixture(t)
	request := f.plannedMove(t)
	resolves := f.resolves
	var first *inboxcontrol.Result
	// The second call has passed its initial lookup and authorization. Another
	// operation completes while its gate acquisition is pending, retiring the
	// old membership before the second call obtains actual Store ownership.
	waitingGate := func(ctx context.Context) (func(), error) {
		var err error
		first, err = f.service.Control(ctx, request, f.principal, f.gate)
		if err != nil {
			return nil, err
		}
		return func() {}, nil
	}
	second, err := f.service.Control(t.Context(), request, f.principal, waitingGate)
	requirements.NoError(err)
	requirements.NotNil(first)
	assertions.Equal(first.Receipt.ID, second.Receipt.ID)
	assertions.Equal(inboxcontrol.StatusVerified, second.Receipt.Status)
	assertions.Equal(resolves+1, f.resolves, "the waiting replay resolves no second provider")
	assertions.Equal(int64(1), f.native.moves.Load())
}

func TestInboxIMAPServiceReauthorizesMappedReceiptBeforeConflictDetails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newInboxNativeServiceFixture(t)
	request := f.plannedMove(t)
	result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.NoError(err)
	resolves := f.resolves
	changed := request
	changed.Operation, changed.Destination = inboxcontrol.OpArchive, nil
	_, err = f.service.Control(t.Context(), changed, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrConflict)
	_, err = f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.Receipt.ID}, inboxcontrol.Principal{ID: "foreign-principal"}, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	f.service.Authorize = func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error {
		return inboxcontrol.ErrDenied
	}
	denied, err := f.service.Control(t.Context(), changed, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Nil(denied, "revoked authority discloses neither stored receipt nor conflict details")
	_, err = f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Equal(resolves, f.resolves)
	assertions.Equal(int64(1), f.native.moves.Load())
}

func TestInboxIMAPStoreRequiresPrivateReceiptProofForRelocation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newInboxNativeServiceFixture(t)
	request := f.plannedMove(t)
	reconcile := f.service.ReconcileState
	f.service.ReconcileState = func(_ context.Context, before, after inboxcontrol.State) error {
		// Publicly supplied target/state values cannot substitute for Service's
		// private durable-receipt context, even with a real MOVE on the server.
		return f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after)
	}
	result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
	requirements.NotNil(result.Receipt.After)
	var destinations int
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND mailbox='Archive' AND message_id=?`), f.native.source.SourceID, f.native.target.ItemID).Scan(&destinations))
	assertions.Zero(destinations)
	f.service.ReconcileState = reconcile
	settled, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, settled.Receipt.Status)
	assertions.Equal(int64(1), f.native.moves.Load())
}

func TestInboxIMAPServiceCreateFolderRetainsExactEvidence(t *testing.T) {
	for _, fault := range []string{"native", "missing created epoch", "lost CREATE reply"} {
		t.Run(fault, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newInboxNativeServiceFixtureWithFault(t, fault)
			f.service.Authorize = func(ctx context.Context, principal inboxcontrol.Principal, request inboxcontrol.Request) error {
				if principal != f.principal || request.Source == nil || *request.Source != f.native.source {
					return inboxcontrol.ErrDenied
				}
				source, err := f.store.GetSourceByIDContext(ctx, request.Source.SourceID)
				if err != nil || source.Identifier != request.Source.SourceIdentifier {
					return inboxcontrol.ErrDenied
				}
				return nil
			}
			f.service.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) {
				return f.native.provider, nil
			}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &f.native.source, Destination: &inboxcontrol.Folder{Name: "Triage"}, DryRun: true}
			preview, err := f.service.Control(t.Context(), request, f.principal, f.gate)
			requirements.NoError(err)
			assertions.Zero(f.native.creates.Load())
			request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-create-once"
			result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
			requirements.NotNil(result)
			assertions.Equal(int64(1), f.native.creates.Load())
			if fault == "native" {
				requirements.NoError(err)
				assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
				requirements.NotNil(result.Receipt.After.ProvisionedFolder)
				folder := *result.Receipt.After.ProvisionedFolder
				assertions.Equal("Triage", folder.ID)
				assertions.NotZero(folder.UIDValidity)
				var epoch uint32
				requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT uidvalidity FROM imap_folder_state WHERE source_id=? AND mailbox='Triage'`), f.native.source.SourceID).Scan(&epoch))
				assertions.Equal(folder.UIDValidity, epoch)
			} else {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
				assertions.Nil(result.Receipt.After)
				_, err = f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
				requirements.ErrorIs(err, inboxcontrol.ErrUnavailable, "a same-name mailbox is not dispatch identity")
			}
			replay, err := f.service.Control(t.Context(), request, f.principal, f.gate)
			requirements.NoError(err)
			assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
			assertions.Equal(int64(1), f.native.creates.Load())
		})
	}
}

// SQLite reopens the actual archive file. The same native durable mapping and
// receipt recovery paths above also run against the configured PostgreSQL Store.
func TestInboxIMAPServiceReopensMappedMoveWithoutRedispatch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	testutil.SkipIfPostgres(t, "SQLite archive file reopen; native mapping/recovery is backend-neutral")
	f := newInboxNativeServiceFixture(t)
	request := f.plannedMove(t)
	reconcile := f.service.ReconcileState
	f.service.ReconcileState = func(context.Context, inboxcontrol.State, inboxcontrol.State) error { return inboxcontrol.ErrInternal }
	result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
	requirements.NotNil(result.Receipt.After)
	assertions.Equal("Archive", result.Receipt.After.Target.Mailbox)
	var sequence int
	var name, path string
	requirements.NoError(f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
	requirements.NoError(f.store.Close())
	reopened, err := store.OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	f.store, f.service.Ledger = reopened, reopened
	requirements.NoError(reopened.RecoverInboxReceipts(t.Context()))
	resolves := f.resolves
	replay, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(result.Receipt.After.Target, replay.Receipt.After.Target)
	_, err = f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(resolves, f.resolves)
	f.service.ReconcileState = reconcile
	settled, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, settled.Receipt.Status)
	assertions.Equal(int64(1), f.native.moves.Load())
	assertions.Zero(f.native.copies.Load())
	assertions.Zero(f.native.expunges.Load())
}

func TestInboxIMAPServiceRecoversCanceledMappedReadback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newInboxNativeServiceFixture(t)
	request := f.plannedMove(t)
	resolve := f.service.Resolve
	entered := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	f.service.Resolve = func(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
		provider, err := resolve(ctx, request)
		if err != nil {
			return nil, err
		}
		return &inboxMappingReadbackObserver{InboxProvider: provider.(*InboxProvider), beforeRead: func(read inboxcontrol.Request) error {
			stored, err := f.store.LookupInboxReceipt(context.WithoutCancel(ctx), f.principal.ID, f.native.source.SourceID, request.IdempotencyKey)
			if err != nil || stored == nil || stored.After == nil || stored.After.Target != *read.Target {
				return inboxcontrol.ErrInternal
			}
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	type outcome struct {
		result *inboxcontrol.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := f.service.Control(ctx, request, f.principal, f.gate)
		done <- outcome{result, err}
	}()
	select {
	case <-entered:
		cancel()
	case <-ctx.Done():
		requirements.FailNow("native destination readback did not reach durable mapping boundary")
	}
	finished := <-done
	requirements.ErrorIs(finished.err, inboxcontrol.ErrOutcomeUnknown)
	requirements.NotNil(finished.result)
	requirements.NotNil(finished.result.Receipt.After)
	assertions.Equal(inboxcontrol.StatusUnknown, finished.result.Receipt.Status)
	assertions.Equal("Archive", finished.result.Receipt.After.Target.Mailbox)
	assertions.Nil(finished.result.Receipt.After.Read, "canceled readback retains identity without inventing state")
	requirements.NoError(f.store.RecoverInboxReceipts(t.Context()))
	resolves := f.resolves
	replay, err := f.service.Control(t.Context(), request, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(finished.result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(resolves, f.resolves)
	f.service.Resolve = resolve
	settled, err := f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: replay.Receipt.ID}, f.principal, f.gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, settled.Receipt.Status)
	assertions.Equal(int64(1), f.native.moves.Load())
	assertions.Zero(f.native.copies.Load())
	assertions.Zero(f.native.expunges.Load())
}

func TestInboxIMAPServiceReconcileConflictsPreserveArchive(t *testing.T) {
	for _, conflict := range []string{"destination collision", "destination epoch", "newer origin observation"} {
		t.Run(conflict, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newInboxNativeServiceFixture(t)
			request := f.plannedMove(t)
			state, err := f.native.provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &f.native.target})
			requirements.NoError(err)
			_, err = f.store.ObserveInboxState(t.Context(), state)
			requirements.NoError(err)
			originDelta := store.IMAPMailboxDelta{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: f.native.target.UIDValidity, UIDNext: 2}}
			var collisionID int64
			var collisionUID uint32
			switch conflict {
			case "destination collision":
				requirements.NoError(f.native.provider.client.withDraftConn(t.Context(), func(conn *imapclient.Client) error {
					status, err := conn.Status("Archive", &imapapi.StatusOptions{UIDNext: true}).Wait()
					if err == nil && status != nil {
						collisionUID = uint32(status.UIDNext)
					}
					return err
				}))
				requirements.NotZero(collisionUID)
				conversation, err := f.store.EnsureConversation(f.native.source.SourceID, "native-collision", "Synthetic collision")
				requirements.NoError(err)
				collisionID, err = f.store.UpsertMessage(&store.Message{SourceID: f.native.source.SourceID, SourceMessageID: "synthetic-collision", ConversationID: conversation, MessageType: "email"})
				requirements.NoError(err)
				requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.native.source.SourceID, []store.IMAPMailboxDelta{originDelta, {Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: f.native.destination.UIDValidity, UIDNext: collisionUID + 1}, Memberships: []store.IMAPMembershipObservation{{UID: collisionUID, CanonicalSourceMessageID: "synthetic-collision", Flags: []string{}}}}}))
			case "destination epoch":
				requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.native.source.SourceID, []store.IMAPMailboxDelta{originDelta, {Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: f.native.destination.UIDValidity + 1, UIDNext: 2}}}))
			case "newer origin observation":
				// A prior observation with a later clock must not be overwritten.
				state.ObservedAt = state.ObservedAt.Add(time.Hour)
				changed, err := f.store.ObserveInboxState(t.Context(), state)
				requirements.NoError(err)
				requirements.True(changed)
			}
			before, err := f.store.GetInboxProviderState(t.Context(), f.native.target)
			requirements.NoError(err)
			requirements.NotNil(before)
			result, err := f.service.Control(t.Context(), request, f.principal, f.gate)
			requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
			requirements.NotNil(result.Receipt)
			assertions.Equal(inboxcontrol.StatusReconcileOnly, result.Receipt.Status)
			requirements.NotNil(result.Receipt.After)
			assertions.Equal("Archive", result.Receipt.After.Target.Mailbox)
			_, err = f.service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, f.principal, f.gate)
			requirements.ErrorIs(err, inboxcontrol.ErrReconcileOnly)
			assertions.Equal(int64(1), f.native.moves.Load(), "local conflict recovery never redispatches native MOVE")
			origin, err := f.store.EmailTagTargetContext(t.Context(), f.native.target.ItemID, "INBOX")
			requirements.NoError(err)
			assertions.Equal(f.native.target.UID, origin.UID)
			after, err := f.store.GetInboxProviderState(t.Context(), f.native.target)
			requirements.NoError(err)
			assertions.Equal(before, after, "failed reconciliation rolls back provider-state retirement")
			var adopted int
			requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND mailbox='Archive' AND message_id=?`), f.native.source.SourceID, f.native.target.ItemID).Scan(&adopted))
			assertions.Zero(adopted)
			if collisionID != 0 {
				var preserved int64
				requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id=? AND mailbox='Archive' AND uidvalidity=? AND uid=?`), f.native.source.SourceID, f.native.destination.UIDValidity, collisionUID).Scan(&preserved))
				assertions.Equal(collisionID, preserved)
			}
			assertions.Zero(f.native.copies.Load())
			assertions.Zero(f.native.expunges.Load())
		})
	}
}

func TestInboxIMAPServiceRejectsChangedOrExpiredPreviewBeforeClaim(t *testing.T) {
	for _, fault := range []string{"expired", "forged", "changed expected"} {
		t.Run(fault, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newInboxNativeServiceFixture(t)
			now := time.Now().UTC()
			f.service.Now = func() time.Time { return now }
			request := f.plannedMove(t)
			switch fault {
			case "expired":
				now = now.Add(24 * time.Hour)
			case "forged":
				request.PreviewToken += "invalid"
			case "changed expected":
				changed := *request.Expected
				changed.Flags = append([]string{}, changed.Flags...)
				changed.Flags = append(changed.Flags, `\Seen`)
				request.Expected = &changed
			}
			_, err := f.service.Control(t.Context(), request, f.principal, f.gate)
			if fault == "changed expected" {
				requirements.ErrorIs(err, inboxcontrol.ErrPlanChanged)
			} else {
				requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
			}
			assertions.Zero(f.native.moves.Load())
			assertions.Zero(f.native.copies.Load())
			assertions.Zero(f.native.expunges.Load())
			assertions.Zero(f.native.creates.Load())
			var claims int
			requirements.NoError(f.store.DB().QueryRow("SELECT COUNT(*) FROM inbox_operation_receipts").Scan(&claims))
			assertions.Zero(claims)
		})
	}
}
