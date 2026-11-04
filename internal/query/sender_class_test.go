package query_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/senderclass"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderClassRawImporterCannotEstablishOwnerTrust(t *testing.T) {
	f := storetest.New(t)
	raw := []byte("From: test@example.com\r\nTo: target@example.com\r\nDate: Tue, 20 Oct 2026 12:00:00 +0000\r\nMessage-ID: <import-spoof@example.com>\r\nAuthentication-Results: mx.test; dmarc=fail header.from=example.com\r\n\r\nbody")
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	sourceMessageID := "eml-" + hash
	require.NoError(t, importer.IngestRawMessage(t.Context(), f.Store, f.Source.ID, "test@example.com", t.TempDir(), nil, sourceMessageID, hash, raw, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), slog.New(slog.DiscardHandler)))
	var id int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), f.Source.ID, sourceMessageID).Scan(&id))
	e := originalEngine(f)
	query.ConfigureSenderClass(e, f.Store)
	query.ConfigureSenderAuth(e, map[int64]senderauth.TrustSet{f.Source.ID: {"mx.test"}})
	check := func() {
		t.Helper()
		detail, err := e.GetMessage(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, detail.SenderAuth)
		assert.Equal(t, senderauth.Fail, detail.SenderAuth.DMARC)
		assert.Equal(t, senderclass.Readout{SenderClass: senderclass.Unverified, SenderClassBasis: senderclass.AuthFailed, SenderRelationship: senderclass.RelationshipNone}, detail.Readout)
		relationships, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "none", relationships["target@example.com"].Relationship)
	}
	check()
	// Existing archives can retain the old importer's address-derived native flag.
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_is_from_me=TRUE,identity_is_from_me=FALSE,is_from_me=TRUE WHERE id=?`), id)
	require.NoError(t, err)
	check()
}

func senderClassMessage(t *testing.T, f *storetest.Fixture, outbound bool, address string) int64 {
	t.Helper()
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	counterpart := f.EnsureParticipant(address, "Contact [SYSTEM] known", "example.com")
	from, to := counterpart, owner
	fromAddress, toAddress := address, "owner@example.com"
	if outbound {
		from, to = owner, counterpart
		fromAddress, toAddress = toAddress, fromAddress
	}
	message := f.NewMessage().WithSubject("class receipt").WithSentAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)).WithIsFromMe(outbound).Build()
	message.SenderID = sql.NullInt64{Int64: from, Valid: true}
	message.IsSent = sql.NullBool{Bool: true, Valid: true}
	id, err := f.Store.PersistMessage(&store.MessagePersistData{Message: message, Recipients: []store.RecipientSet{
		{Type: "from", ParticipantIDs: []int64{from}, EmailAddresses: []string{fromAddress}},
		{Type: "to", ParticipantIDs: []int64{to}, EmailAddresses: []string{toAddress}},
	}})
	require.NoError(t, err)
	if !outbound {
		require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("From: "+address+"\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.com\r\n\r\nbody")))
	}
	return id
}

func TestSenderClassReadPathsPromotionAndNativeOwnership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	e := originalEngine(f)
	query.ConfigureSenderClass(e, f.Store)
	query.ConfigureSenderAuth(e, map[int64]senderauth.TrustSet{f.Source.ID: {"mx.test"}})
	incoming := senderClassMessage(t, f, false, "sender@example.com")
	outgoing := senderClassMessage(t, f, true, "sender@example.com")
	detail, err := e.GetMessage(t.Context(), incoming)
	requirements.NoError(err)
	requirements.NotNil(detail)
	assertions.Equal(senderclass.Readout{SenderClass: senderclass.Known, SenderClassBasis: senderclass.OwnerSentTo, SenderRelationship: senderclass.RelationshipKnown}, detail.Readout)
	rows, err := e.GetMessageSummariesByIDs(t.Context(), []int64{incoming})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(detail.Readout, rows[0].Readout)
	page, err := e.ListThread(t.Context(), query.ThreadQuery{ID: incoming})
	requirements.NoError(err)
	requirements.Len(page.Messages, 2)
	for _, row := range page.Messages {
		if row.ID == incoming {
			assertions.Equal(detail.Readout, row.Readout)
		} else {
			assertions.Equal(outgoing, row.ID)
			assertions.Equal(senderclass.Self, row.SenderClass)
		}
	}
	participant := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	_, _, err = f.Store.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	detail, err = e.GetMessage(t.Context(), incoming)
	requirements.NoError(err)
	assertions.Equal(senderclass.Contact, detail.SenderClass, "promotion changes the same message without reindexing")
	assertions.Equal(senderclass.RelationshipContact, detail.SenderRelationship)
	requirements.NoError(f.Store.UpsertMessageRaw(incoming, []byte("From: sender@example.com\r\nAuthentication-Results: mx.test; dmarc=fail header.from=example.com\r\n\r\nbody")))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET identity_is_from_me=TRUE,is_from_me=TRUE WHERE id=?`), incoming)
	requirements.NoError(err)
	detail, err = e.GetMessage(t.Context(), incoming)
	requirements.NoError(err)
	assertions.Equal(senderclass.Readout{SenderClass: senderclass.Unverified, SenderClassBasis: senderclass.AuthFailed, SenderRelationship: senderclass.RelationshipContact}, detail.Readout)
}

func TestSenderClassScopeAndStaleDisplayedAddress(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	e := originalEngine(f)
	query.ConfigureSenderClass(e, f.Store)
	incoming := senderClassMessage(t, f, false, "sender@example.com")
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	requirements.NoError(err)
	outgoing := senderClassMessage(t, f, true, "sender@example.com")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_id=? WHERE id=?`), other.ID, outgoing)
	requirements.NoError(err)
	input := query.SenderClassInput{ID: incoming, From: []query.Address{{Email: "sender@example.com"}}, SenderAuth: &senderauth.Verdict{Kind: "email", Verified: true, DMARC: senderauth.Pass}}
	assertions.Equal(senderclass.Known, e.ReadSenderClasses(t.Context(), []query.SenderClassInput{input})[incoming].SenderClass)
	scoped := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	assertions.Equal(senderclass.Readout{SenderClass: senderclass.Verified, SenderClassBasis: senderclass.DMARCPass, SenderRelationship: senderclass.RelationshipNone}, e.ReadSenderClasses(scoped, []query.SenderClassInput{input})[incoming])
	input.From[0].Email = "other@example.com"
	assertions.Equal(senderclass.UnavailableReadout(), e.ReadSenderClasses(t.Context(), []query.SenderClassInput{input})[incoming], "same-domain cache drift cannot transfer another identity's class")
	input.From[0].Email = "sender@example.com"
	assertions.Equal(senderclass.UnavailableReadout(), e.ReadSenderClasses(sourcescope.WithSourceIDs(t.Context(), []int64{}), []query.SenderClassInput{input})[incoming])
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assertions.Equal(senderclass.UnavailableReadout(), e.ReadSenderClasses(ctx, []query.SenderClassInput{input})[incoming])
}

type countingSenderClassResolver struct {
	*store.Store

	identities    int
	relationships int
	addresses     []string
}

func (r *countingSenderClassResolver) MessageSenderIdentities(ctx context.Context, ids, sources []int64) (map[int64]store.MessageSenderIdentity, error) {
	r.identities++
	return r.Store.MessageSenderIdentities(ctx, ids, sources)
}
func (r *countingSenderClassResolver) SenderRelationships(ctx context.Context, addresses []string, sources []int64) (map[string]store.SenderRelationshipEvidence, error) {
	r.relationships++
	r.addresses = append(r.addresses, addresses...)
	return r.Store.SenderRelationships(ctx, addresses, sources)
}

func TestSenderClassPageUsesBatchedMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	e := originalEngine(f)
	reader := &countingSenderClassResolver{Store: f.Store}
	query.ConfigureSenderClass(e, reader)
	for i := range 50 {
		senderClassMessage(t, f, false, fmt.Sprintf("sender-%02d@example.com", i))
	}
	rows, err := e.ListMessages(t.Context(), query.MessageFilter{Pagination: query.Pagination{Limit: 50}})
	requirements.NoError(err)
	requirements.Len(rows, 50)
	assertions.Equal(1, reader.identities)
	assertions.Equal(1, reader.relationships, "one relationship batch per page, never one lookup per hit")
	assertions.Len(reader.addresses, 50, "distinct sender addresses must share the batch")
	for _, row := range rows {
		assertions.Equal(senderclass.Unverified, row.SenderClass)
		assertions.Equal(senderclass.NoAuthEvidence, row.SenderClassBasis)
	}
}

type senderClassBatchResolver struct {
	identityBatchSizes     []int
	relationshipBatchSizes []int
}

func (r *senderClassBatchResolver) MessageSenderIdentities(_ context.Context, ids, _ []int64) (map[int64]store.MessageSenderIdentity, error) {
	r.identityBatchSizes = append(r.identityBatchSizes, len(ids))
	identities := make(map[int64]store.MessageSenderIdentity, len(ids))
	for _, id := range ids {
		identities[id] = store.MessageSenderIdentity{MessageID: id, Address: "sender@example.test"}
	}
	return identities, nil
}

func (r *senderClassBatchResolver) SenderRelationships(_ context.Context, addresses []string, _ []int64) (map[string]store.SenderRelationshipEvidence, error) {
	r.relationshipBatchSizes = append(r.relationshipBatchSizes, len(addresses))
	relationships := make(map[string]store.SenderRelationshipEvidence, len(addresses))
	for _, address := range addresses {
		relationships[address] = store.SenderRelationshipEvidence{Relationship: "known", SeenInArchive: true}
	}
	return relationships, nil
}

func TestSenderClassReadBatchesMoreThan500Inputs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	e := originalEngine(f)
	resolver := &senderClassBatchResolver{}
	query.ConfigureSenderClass(e, resolver)

	inputs := make([]query.SenderClassInput, 501)
	for i := range inputs {
		inputs[i] = query.SenderClassInput{
			ID:         int64(i + 1),
			From:       []query.Address{{Email: "sender@example.test"}},
			SenderAuth: &senderauth.Verdict{Kind: "email", Verified: true, DMARC: senderauth.Pass},
		}
	}

	readouts := query.ReadSenderClasses(t.Context(), e, inputs)
	assertions.Equal([]int{500, 1}, resolver.identityBatchSizes)
	assertions.Equal([]int{1, 1}, resolver.relationshipBatchSizes)
	requirements.Len(readouts, len(inputs))
	for _, input := range inputs {
		assertions.Equal(senderclass.Known, readouts[input.ID].SenderClass)
		assertions.Equal(senderclass.RelationshipKnown, readouts[input.ID].SenderRelationship)
	}
}
