package imap

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Wrappers deliberately remove optional interface methods. Production Store,
// canonical Service, source ownership and the native IMAP provider remain real.
type inboxLedgerWithoutMapping struct{ inboxcontrol.Ledger }
type inboxProviderWithoutSourceProof struct{ inboxcontrol.Provider }
type inboxAdmissionLedger struct {
	inboxcontrol.Ledger
	mappingCalls int
}

func (l *inboxAdmissionLedger) RecordInboxReceiptMapping(context.Context, string, inboxcontrol.State) error {
	l.mappingCalls++
	return inboxcontrol.ErrInternal
}

func TestInboxIMAPServiceRequiresMoveProofBeforeClaim(t *testing.T) {
	for _, missing := range []string{"mapping ledger", "source verifier"} {
		t.Run(missing, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			native := newInboxNativeFixture(t, "native")
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("imap", native.source.SourceIdentifier)
			requirements.NoError(err)
			config, err := native.provider.client.config.ToJSON()
			requirements.NoError(err)
			requirements.NoError(st.UpdateSourceSyncConfig(source.ID, config))
			conversation, err := st.EnsureConversation(source.ID, "native-admission", "Native admission")
			requirements.NoError(err)
			item, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "synthetic-message", ConversationID: conversation, MessageType: "email"})
			requirements.NoError(err)
			native.source.SourceID = source.ID
			native.target.SourceID = source.ID
			native.target.ItemID = item
			native.provider.source = native.source
			requirements.NoError(st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: native.target.UIDValidity, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{UID: 1, CanonicalSourceMessageID: "synthetic-message", Flags: []string{}}}}}))
			withMapping := &inboxAdmissionLedger{Ledger: st}
			var ledger inboxcontrol.Ledger = inboxLedgerWithoutMapping{Ledger: st}
			var provider inboxcontrol.Provider = native.provider
			if missing == "source verifier" {
				ledger = withMapping
				provider = inboxProviderWithoutSourceProof{Provider: native.provider}
			}
			service := &inboxcontrol.Service{Ledger: ledger, Key: []byte(strings.Repeat("k", 32)), Authorize: func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, Resolve: func(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
				resolved, err := st.EmailTagTargetContext(ctx, request.Target.ItemID, request.Target.Mailbox)
				if err != nil || resolved.SourceID != native.target.SourceID || resolved.UIDValidity != native.target.UIDValidity || resolved.UID != native.target.UID {
					return nil, inboxcontrol.ErrDenied
				}
				return provider, nil
			}, AcquireSource: func(ctx context.Context, id int64) (func(), error) {
				lease, err := st.AcquireInboxExecutionContext(ctx, id)
				if err != nil {
					return nil, err
				}
				return func() { _ = lease.Release() }, nil
			}}
			gate := func(context.Context) (func(), error) { return func() {}, nil }
			principal := inboxcontrol.Principal{ID: "native-admission-principal"}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &native.target, Destination: &native.destination, DryRun: true}
			preview, err := service.Control(t.Context(), request, principal, gate)
			requirements.NoError(err)
			request.DryRun = false
			request.Expected = preview.Before
			request.PreviewToken = preview.PreviewToken
			request.IdempotencyKey = "native-admission"
			_, err = service.Control(t.Context(), request, principal, gate)
			assertions.ErrorIs(err, inboxcontrol.ErrUnavailable)
			assertions.Zero(native.moves.Load())
			assertions.Zero(native.copies.Load())
			assertions.Zero(native.expunges.Load())
			assertions.Zero(withMapping.mappingCalls)
			var claims int
			requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM inbox_operation_receipts").Scan(&claims))
			assertions.Zero(claims)
		})
	}
}
