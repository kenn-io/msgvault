package vcardmap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestNativeProfileProjectionEndToEndPreservesResidueAndVersionViews(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	participantID, err := st.EnsureParticipant(
		"alice@example.com", "Alice Example", "example.com",
	)
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	_, err = st.AddPersonNameContext(ctx, person.ID, store.PersonNameInput{
		NameKind: store.PersonNameStructured, FamilyName: new("Example"),
		GivenName: new("Alice"), SecondarySurname: new("Sample"),
		OriginalValue: "Example;Alice;;;;Sample;",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	group := "item1"
	_, err = st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "new@example.com",
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceUser, TypeTokens: []string{"home"},
			VCard: store.VCardIdentity{Property: "EMAIL", Group: &group},
		},
	})
	require.NoError(err)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Imported Name\r\n" +
		"item1.EMAIL;TYPE=home;X-KEEP=opaque:old@example.com\r\n" +
		"item1.X-ABLABEL:Home\r\n" +
		"SOCIALPROFILE:https://social.example/alice\r\n" +
		"X-VENDOR;X-FUTURE=yes:untouched\r\nEND:VCARD\r\n")
	envelope, err := vcard.ParseResourceEnvelope(raw)
	require.NoError(err)
	envelope.SourceRef = "address-book"
	envelope.SourceResourceUID = "alice-source"
	envelope.CanonicalPersonUID = person.VCardUID
	created, err := st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
		PersonID: person.ID, Envelope: envelope,
	})
	require.NoError(err)

	snapshot, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	prepared, err := ProjectPersonEnvelope(*snapshot, created.ResourceEnvelope)
	require.NoError(err)
	committed, err := st.CommitVCardResourceEnvelopeContext(
		ctx, "address-book", "alice-source", created.Revision,
		snapshot.Fingerprint, prepared,
	)
	require.NoError(err)
	assert.Equal(raw, committed.OriginalRawBytes)
	assert.Contains(string(committed.StoredBody),
		"item1.EMAIL;TYPE=home;X-KEEP=opaque:new@example.com")
	assert.Contains(string(committed.StoredBody), "item1.X-ABLABEL:Home")
	assert.Contains(string(committed.StoredBody), "X-VENDOR;X-FUTURE=yes:untouched")
	assert.Contains(string(committed.StoredBody), "N:Example;Alice;;;;Sample;")
	assert.Contains(string(committed.StoredBody), "FN:Imported Name\r\n",
		"the imported FN stands; no derived FN is added beside it")
	assert.NotContains(string(committed.StoredBody), "DERIVED")

	v3, err := st.RenderVCardResourceViewContext(
		ctx, "address-book", "alice-source", vcard.Version30,
	)
	require.NoError(err)
	assert.NotContains(string(v3), "SOCIALPROFILE")
	assert.Contains(string(v3), "X-VENDOR;X-FUTURE=yes:untouched")
	v4, err := st.RenderVCardResourceViewContext(
		ctx, "address-book", "alice-source", vcard.Version40,
	)
	require.NoError(err)
	assert.Equal(committed.StoredBody, v4)
	assert.Contains(string(v4), "SOCIALPROFILE:https://social.example/alice")
}

// TestNativeProfileProjectionOfUnchangedSnapshotIsANoOpCommit pins the
// idempotence of re-projection: rendering the same semantic snapshot over an
// envelope it already produced must change nothing a reader can see — not the
// body, not the render revision, and not the store revision or ETag.
func TestNativeProfileProjectionOfUnchangedSnapshotIsANoOpCommit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	participantID, err := st.EnsureParticipant(
		"alice@example.com", "Alice Example", "example.com",
	)
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	_, err = st.AddPersonNameContext(ctx, person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
		OriginalValue: "Alice Example",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	envelope, err := vcard.ParseResourceEnvelope(
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Imported Name\r\nEND:VCARD\r\n"),
	)
	require.NoError(err)
	envelope.SourceRef = "address-book"
	envelope.SourceResourceUID = "alice-source"
	created, err := st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
		PersonID: person.ID, Envelope: envelope,
	})
	require.NoError(err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)

	prepared, err := ProjectPersonEnvelope(*snapshot, created.ResourceEnvelope)
	require.NoError(err)
	first, err := st.CommitVCardResourceEnvelopeContext(
		ctx, "address-book", "alice-source", created.Revision,
		snapshot.Fingerprint, prepared,
	)
	require.NoError(err)
	assert.Contains(string(first.StoredBody), "FN:Alice Example")

	again, err := ProjectPersonEnvelope(*snapshot, first.ResourceEnvelope)
	require.NoError(err)
	second, err := st.CommitVCardResourceEnvelopeContext(
		ctx, "address-book", "alice-source", first.Revision,
		snapshot.Fingerprint, again,
	)
	require.NoError(err)
	assert.Equal(first.Revision, second.Revision)
	assert.Equal(first.RenderMetadata.Revision, second.RenderMetadata.Revision)
	assert.Equal(first.StoredBody, second.StoredBody)
	assert.Equal(first.ETag, second.ETag)
	assert.Equal(first.UpdatedAt, second.UpdatedAt)
}
