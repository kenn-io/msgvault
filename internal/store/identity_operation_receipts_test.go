package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

func receiptRequest(t *testing.T, st *Store, operation identitycontrol.Operation, target identitycontrol.IdentityTarget, key string) IdentityOperationRequest {
	t.Helper()
	preview, err := st.IdentityOperationPreviewContext(t.Context(), operation, target)
	require.NoError(t, err)
	return IdentityOperationRequest{Principal: "synthetic-principal", IdempotencyKey: key, Operation: operation, Target: target, ExpectedFingerprint: preview.Fingerprint}
}

func TestIdentityOperationReceiptCommitsExplicitBindingAndDetach(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	target := identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID}
	request := receiptRequest(t, st, identitycontrol.OperationPersonLink, target, "synthetic-attach-key")
	receipt, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	requirements.NotNil(receipt)
	assertions.True(receipt.Changed)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.ElementsMatch([]int64{a, b}, current.ParticipantIDs)
	assertions.Greater(current.Revision, person.Revision)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Empty(edges, "direct binding must preserve the independent participant graph")
	request = receiptRequest(t, st, identitycontrol.OperationPersonUnlink, target, "synthetic-detach-key")
	detached, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	assertions.True(detached.Changed)
	current, err = st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal([]int64{b}, current.ParticipantIDs)
}

func TestIdentityOperationReceiptExactRetryReturnsCommittedReceipt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, target, "synthetic-retry-key")
	first, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	second, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	assertions.Equal(first, second)
	revision, err := st.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(first.AfterRevision, revision)
	changed := request
	changed.Operation = identitycontrol.OperationGraphUnlink
	_, err = st.ApplyIdentityOperationContext(t.Context(), changed, nil)
	requirements.ErrorIs(err, ErrIdentityOperationIdempotency)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Len(edges, 1)
}

func TestIdentityOperationReceiptDenialAndStaleEvidenceDoNotMutate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, target, "synthetic-denied-key")
	denied := errors.New("synthetic current grant revoked")
	calls := 0
	_, err := st.ApplyIdentityOperationContext(t.Context(), request, func(ctx context.Context, snapshot *IdentitySnapshot) error { calls++; return denied })
	requirements.ErrorIs(err, denied)
	assertions.Equal(1, calls)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Empty(edges)
	_, err = st.EnsureParticipant("identity-a@example.test", "Updated Synthetic A", "example.test")
	requirements.NoError(err)
	// Native identifier evidence changes without creating an edge.
	err = st.SetParticipantIdentifier(a, "email", "changed@example.test")
	requirements.NoError(err)
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.ErrorIs(err, ErrIdentityOperationStale)
	edges, err = st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Empty(edges)
}

func TestIdentityOperationReceiptInsertionFailureRollsBackGraphMutation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, target, "synthetic-rollback-key")
	before, err := st.IdentityRevision()
	requirements.NoError(err)
	// Reject only the real receipt INSERT, so the preceding receipt lookup and
	// native mutation execute normally before the transaction must roll back.
	if st.IsPostgreSQL() {
		_, err = st.db.Exec(`ALTER TABLE identity_operation_receipts ADD CONSTRAINT synthetic_receipt_rejection CHECK (FALSE)`)
	} else {
		_, err = st.db.Exec(`CREATE TRIGGER synthetic_receipt_rejection BEFORE INSERT ON identity_operation_receipts BEGIN SELECT RAISE(ABORT, 'synthetic receipt insertion denied'); END`)
	}
	requirements.NoError(err)
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.Error(err)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Empty(edges)
	after, err := st.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(before, after)
}

func TestIdentityOperationReceiptRetryRechecksCurrentAuthorization(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, target, "synthetic-revoked-retry-key")
	first, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	denied := errors.New("synthetic principal revoked")
	calls := 0
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, func(ctx context.Context, snapshot *IdentitySnapshot) error { calls++; return denied })
	requirements.ErrorIs(err, denied)
	assertions.Equal(1, calls)
	revision, err := st.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(first.AfterRevision, revision)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Len(edges, 1)
}

func TestIdentityOperationReceiptGraphUnlinkKeepsDurableBinding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	_, err := st.LinkParticipants(a, b)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	request := receiptRequest(t, st, identitycontrol.OperationGraphUnlink, target, "synthetic-graph-unlink-key")
	receipt, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	assertions.True(receipt.Changed)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Empty(edges)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.ElementsMatch([]int64{a, b}, current.ParticipantIDs)
	inverse := receiptRequest(t, st, identitycontrol.OperationGraphLink, target, "synthetic-inverse-link-key")
	restored, err := st.ApplyIdentityOperationContext(t.Context(), inverse, nil)
	requirements.NoError(err)
	assertions.True(restored.Changed)
	edges, err = st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Len(edges, 1)
}

func TestIdentityOperationReceiptConcurrentDifferentKeysCommitOnce(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	first := receiptRequest(t, st, identitycontrol.OperationGraphLink, target, "synthetic-concurrent-one")
	second := first
	second.IdempotencyKey = "synthetic-concurrent-two"
	type outcome struct {
		receipt *IdentityReceipt
		err     error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, request := range []IdentityOperationRequest{first, second} {
		go func() {
			<-start
			receipt, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
			results <- outcome{receipt, err}
		}()
	}
	close(start)
	successes, stale := 0, 0
	for range 2 {
		result := <-results
		if result.err == nil {
			successes++
			requirements.NotNil(result.receipt)
			assertions.True(result.receipt.Changed)
		} else {
			requirements.ErrorIs(result.err, ErrIdentityOperationStale)
			stale++
		}
	}
	assertions.Equal(1, successes)
	assertions.Equal(1, stale)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Len(edges, 1)
}

func TestIdentityOperationReceiptBindingPreservesMessageAuthorship(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("gmail", "synthetic-authorship@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "synthetic-authorship-thread", "Synthetic Authorship")
	requirements.NoError(err)
	message, err := st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "synthetic-authorship-message", MessageType: "email", SenderID: sql.NullInt64{Int64: a, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(message, "to", []int64{b}, []string{"Synthetic B"}))
	person, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationPersonLink, identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID}, "synthetic-authorship-binding")
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	var sender int64
	requirements.NoError(st.db.QueryRow(`SELECT sender_id FROM messages WHERE id=?`, message).Scan(&sender))
	assertions.Equal(a, sender)
	var recipient int64
	requirements.NoError(st.db.QueryRow(`SELECT participant_id FROM message_recipients WHERE message_id=?`, message).Scan(&recipient))
	assertions.Equal(b, recipient)
}

func TestIdentityOperationReceiptCancelledBeforeMutation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-cancelled-operation")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt, err := st.ApplyIdentityOperationContext(ctx, request, nil)
	requirements.ErrorIs(err, context.Canceled)
	assertions.Nil(receipt)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Empty(edges)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM identity_operation_receipts`).Scan(&count))
	assertions.Zero(count)
}
