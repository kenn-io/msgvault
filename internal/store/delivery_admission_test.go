package store_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

func admissionRecipient(s *store.DeliveryPolicyState, role string) store.DeliveryRecipient {
	return store.DeliveryRecipient{PersonUID: s.PersonUID, Target: *s.Target, Role: role, PolicyRevision: s.PolicyRevision, PersonRevision: s.PersonRevision, BindingDigest: s.BindingDigest}
}

func deliveryReadOnlyURL(t *testing.T, st *store.Store) string {
	t.Helper()
	var database, schema string
	require.NoError(t, st.DB().QueryRow(`SELECT current_database(), current_schema()`).Scan(&database, &schema))
	parsed, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
	require.NoError(t, err)
	parsed.Path = "/" + database
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func TestDeliveryAdmissionReadOnlyPostgresStoreDoesNotAttemptProvider(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL read-only admission fence")
	}
	p, targets := deliveryPerson(t, f)
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(
		readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)

	readOnly, err := store.OpenReadOnly(deliveryReadOnlyURL(t, f.Store))
	requirements.NoError(err)
	t.Cleanup(func() { _ = readOnly.Close() })

	calls := 0
	err = readOnly.WithDeliveryAdmissionContext(t.Context(),
		[]store.DeliveryRecipient{admissionRecipient(&allowed.After, "to")},
		func(context.Context) error { calls++; return nil })
	var refusal *store.DeliveryAdmissionError
	requirements.ErrorAs(err, &refusal)
	assertions.Equal("admission_unavailable", refusal.Code)
	assertions.Equal("read_only_store", refusal.Reason)
	assertions.False(refusal.ProviderAttempted)
	assertions.Zero(calls)
}

func TestDeliveryAdmissionAllRecipientsAndRevocation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	calls := 0
	send := func(context.Context) error { calls++; return nil }
	first := readDelivery(t, f.Store, p.VCardUID, &targets[0])
	err := f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryRecipient{admissionRecipient(first, "to")}, send)
	var blocked *store.DeliveryAdmissionError
	requirements.ErrorAs(err, &blocked)
	assertions.Equal("draft_required", blocked.Code)
	assertions.Zero(calls)
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(first, store.DeliverySendAllowed))
	requirements.NoError(err)
	second := readDelivery(t, f.Store, p.VCardUID, &targets[1])
	for _, role := range []string{"cc", "bcc"} {
		err = f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryRecipient{admissionRecipient(&allowed.After, "to"), admissionRecipient(second, role)}, send)
		requirements.ErrorAs(err, &blocked)
		assertions.Zero(calls)
	}
	requirements.NoError(f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryRecipient{admissionRecipient(&allowed.After, "to")}, send))
	assertions.Equal(1, calls)
	denied, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(&allowed.After, store.DeliveryDraftOnly))
	requirements.NoError(err)
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryRecipient{admissionRecipient(&allowed.After, "to")}, send)
	requirements.ErrorAs(err, &blocked)
	assertions.Equal(1, calls)
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryRecipient{admissionRecipient(&denied.After, "group")}, send)
	requirements.ErrorAs(err, &blocked)
	assertions.Equal(1, calls)
}
func TestDeliveryAdmissionProviderFailureIsUncertain(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	state := readDelivery(t, f.Store, p.VCardUID, &targets[0])
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(state, store.DeliverySendAllowed))
	requirements.NoError(err)
	failure := errors.New("synthetic transport result unknown")
	calls := 0
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryRecipient{admissionRecipient(&allowed.After, "to")}, func(context.Context) error { calls++; return failure })
	var blocked *store.DeliveryAdmissionError
	requirements.ErrorAs(err, &blocked)
	assertions.Equal("delivery_uncertain", blocked.Code)
	assertions.True(blocked.ProviderAttempted)
	requirements.ErrorIs(err, failure)
	assertions.Equal(1, calls)
}

func TestDeliveryAdmissionCancelledBeforeAttemptHasDistinctReceipt(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	calls := 0
	for _, ctx := range []context.Context{cancelled, expired} {
		err = f.Store.WithDeliveryAdmissionContext(ctx, []store.DeliveryRecipient{admissionRecipient(&allowed.After, "to")}, func(context.Context) error { calls++; return nil })
		var refusal *store.DeliveryAdmissionError
		requirements.ErrorAs(err, &refusal)
		assertions.Equal("admission_cancelled", refusal.Code)
		assertions.Equal("admission_cancelled", refusal.Reason)
		requirements.ErrorIs(err, ctx.Err())
		assertions.False(refusal.ProviderAttempted)
	}
	assertions.Zero(calls)
}

func TestDeliveryAdmissionCancellationStopsWaitingForPoolConnection(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(
		readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)

	db := f.Store.DB()
	db.SetMaxOpenConns(1)
	held, err := db.Conn(t.Context())
	requirements.NoError(err)
	defer func() { _ = held.Close() }()
	baselineWaits := db.Stats().WaitCount

	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	calls := 0
	go func() {
		finished <- f.Store.WithDeliveryAdmissionContext(ctx,
			[]store.DeliveryRecipient{admissionRecipient(&allowed.After, "to")},
			func(context.Context) error { calls++; return nil })
	}()
	requirements.Eventually(func() bool {
		return db.Stats().WaitCount > baselineWaits
	}, 5*time.Second, 10*time.Millisecond, "admission did not wait for the held pool connection")
	cancel()

	select {
	case err = <-finished:
		var refusal *store.DeliveryAdmissionError
		requirements.ErrorAs(err, &refusal)
		assertions.Equal("admission_cancelled", refusal.Code)
		requirements.ErrorIs(err, context.Canceled)
		assertions.Zero(calls)
	case <-time.After(time.Second):
		// Release the sole connection so the old uncancellable acquisition can
		// finish; the assertion below records that cancellation was ignored.
		requirements.NoError(held.Close())
		err = <-finished
		requirements.Fail("pool wait returned only after the connection was released", err)
	}
}

func TestDeliveryAdmissionCancellationKeepsNativeFenceUntilAttemptExits(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseAttempt := sync.OnceFunc(func() { close(release) })
	defer releaseAttempt()
	t.Cleanup(releaseAttempt)
	finished := make(chan error, 1)
	var snapshot []store.DeliveryRecipient
	var snapshotErr error
	go func() {
		finished <- f.Store.WithDeliveryAdmissionContext(ctx, []store.DeliveryRecipient{admissionRecipient(&allowed.After, "to")}, func(providerCtx context.Context) error {
			snapshot, snapshotErr = store.AdmittedDeliveryRecipients(providerCtx)
			close(entered)
			<-release
			return snapshotErr
		})
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		requirements.FailNow("provider admission did not start")
	}
	requirements.NoError(snapshotErr)
	requirements.Len(snapshot, 1)
	assertions.Equal(targets[0], snapshot[0].Target)
	cancel()
	// A real database lock budget, not a runner throughput assertion: cancellation
	// must not roll back admission while the synchronous provider is still running.
	const nativeFenceBudget = time.Second
	writerCtx, stopWriter := context.WithTimeout(t.Context(), nativeFenceBudget)
	defer stopWriter()
	_, err = f.Store.SetDeliveryPolicyContext(writerCtx, deliveryWrite(&allowed.After, store.DeliveryDraftOnly))
	requirements.Error(err, "revocation waits for the in-flight provider boundary")
	releaseAttempt()
	requirements.NoError(<-finished)
	denied, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(&allowed.After, store.DeliveryDraftOnly))
	requirements.NoError(err)
	assertions.Equal(store.DeliveryDraftOnly, denied.After.EffectivePolicy)
}

func TestDeliveryPolicyReadDoesNotWaitForAdmissionFence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, targets := deliveryPerson(t, f)
	allowed, err := f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(
		readDelivery(t, f.Store, p.VCardUID, &targets[0]), store.DeliverySendAllowed))
	requirements.NoError(err)

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAttempt := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAttempt()
	finished := make(chan error, 1)
	go func() {
		finished <- f.Store.WithDeliveryAdmissionContext(t.Context(),
			[]store.DeliveryRecipient{admissionRecipient(&allowed.After, "to")},
			func(context.Context) error {
				close(entered)
				<-release
				return nil
			})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		requirements.FailNow("provider admission did not reach its synchronous callback")
	}

	readCtx, cancelRead := context.WithTimeout(t.Context(), 3*time.Second)
	state, readErr := f.Store.GetDeliveryPolicyContext(readCtx,
		store.DeliveryPolicyQuery{PersonUID: p.VCardUID, Target: &targets[0]})
	cancelRead()
	releaseAttempt()
	requirements.NoError(<-finished)
	requirements.NoError(readErr, "policy reads must use a read-only snapshot while admission holds the write fence")
	assertions.Equal(store.DeliverySendAllowed, state.EffectivePolicy)
}

func TestDeliveryAdmissionDuplicateNativeAnchorsAndSeparateAccounts(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, archived := deliveryParticipantTarget(t, f, "peer@EXAMPLE.TEST")
	participant := archived.ParticipantID
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "duplicate-anchor-evidence", "email_thread", "Example thread")
	requirements.NoError(err)
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: conversation, SourceMessageID: "duplicate-anchor-message", MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
	requirements.NoError(err)
	cp, err := f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "peer@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	curated := archived
	curated.Endpoint, curated.ParticipantID, curated.ContactPointID = "peer@example.test", 0, cp.Envelope.ID
	account, err := f.Store.GetOrCreateSource("gmail", "separate-account@example.test")
	requirements.NoError(err)
	separate := curated
	separate.SourceID, separate.AccountID = account.ID, account.Identifier
	for _, target := range []store.DeliveryTarget{curated, archived, separate} {
		_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(readDelivery(t, f.Store, p.VCardUID, &target), store.DeliverySendAllowed))
		requirements.NoError(err)
	}
	first := admissionRecipient(readDelivery(t, f.Store, p.VCardUID, &curated), "to")
	duplicate := admissionRecipient(readDelivery(t, f.Store, p.VCardUID, &archived), "cc")
	otherAccount := admissionRecipient(readDelivery(t, f.Store, p.VCardUID, &separate), "bcc")
	calls := 0
	send := func(context.Context) error { calls++; return nil }
	err = f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryRecipient{first, duplicate}, send)
	var blocked *store.DeliveryAdmissionError
	requirements.ErrorAs(err, &blocked)
	assertions.Equal("draft_required", blocked.Code)
	assertions.Equal("duplicate_recipient", blocked.Reason)
	assertions.False(blocked.ProviderAttempted)
	assertions.Zero(calls)
	requirements.NoError(f.Store.WithDeliveryAdmissionContext(t.Context(), []store.DeliveryRecipient{first, otherAccount}, send))
	assertions.Equal(1, calls)
}

func TestDeliveryAdmissionEquivalentAnchorRestriction(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	p, _ := deliveryPerson(t, f)
	participant := p.ParticipantIDs[0]
	contactPoint, err := f.Store.AddPersonContactPointContext(t.Context(), p.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "peer@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "equivalent-anchor-evidence", "email_thread", "Example thread")
	requirements.NoError(err)
	_, err = f.Store.UpsertMessage(&store.Message{
		SourceID: f.Source.ID, ConversationID: conversation,
		SourceMessageID: "equivalent-anchor-message", MessageType: "email",
		SenderID: sql.NullInt64{Int64: participant, Valid: true},
	})
	requirements.NoError(err)

	curated := store.DeliveryTarget{
		SourceID: f.Source.ID, SourceType: "gmail", AccountID: f.Source.Identifier,
		Network: "email", Endpoint: "peer@example.test", ContactPointID: contactPoint.Envelope.ID,
	}
	archived := curated
	archived.ContactPointID, archived.ParticipantID = 0, participant
	defaults := readDelivery(t, f.Store, p.VCardUID, nil)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(defaults, store.DeliverySendAllowed))
	requirements.NoError(err)
	_, err = f.Store.SetDeliveryPolicyContext(t.Context(), deliveryWrite(
		readDelivery(t, f.Store, p.VCardUID, &curated), store.DeliveryDraftOnly,
	))
	requirements.NoError(err)

	state := readDelivery(t, f.Store, p.VCardUID, &archived)
	calls := 0
	err = f.Store.WithDeliveryAdmissionContext(t.Context(),
		[]store.DeliveryRecipient{admissionRecipient(state, "to")},
		func(context.Context) error { calls++; return nil },
	)
	var blocked *store.DeliveryAdmissionError
	requirements.ErrorAs(err, &blocked)
	assertions.Equal("draft_required", blocked.Code)
	assertions.Zero(calls, "the equivalent native anchor must not bypass a restrictive mailbox override")
}
