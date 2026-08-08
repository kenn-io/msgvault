package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonMediaStoresInlineBytesWithHashAndSize(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	payload := bytes.Repeat([]byte{0x89, 0x50, 0x4e, 0x47}, 64)
	digest := sha256.Sum256(payload)
	stored, err := st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, MediaType: new("image/png"),
		Data: payload, OriginalValue: "data:image/png;base64,<elided>",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceCardDAVImport,
			SourceRef: new("resource-1"), VCard: store.VCardIdentity{
				Property: "PHOTO", PropID: new("p1"),
			}},
	})
	require.NoError(err)
	assert.True(stored.HasData)
	assert.Equal(int64(len(payload)), *stored.ByteSize)
	assert.Equal(hex.EncodeToString(digest[:]), *stored.ContentHash)
	data, mediaType, err := st.ReadPersonMediaDataContext(ctx, personID, stored.Envelope.ID)
	require.NoError(err)
	assert.Equal(payload, data)
	assert.Equal("image/png", mediaType)
}

func TestPersonMediaStoresURIReferenceWithoutBytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	stored, err := st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, URI: new("https://example.com/alice.png"),
		MediaType: new("image/png"),
		Envelope:  store.ValueEnvelope{Source: store.ProvenanceVCardImport},
	})
	require.NoError(err)
	assert.False(stored.HasData)
	assert.Nil(stored.ByteSize)
	_, _, err = st.ReadPersonMediaDataContext(ctx, personID, stored.Envelope.ID)
	require.ErrorIs(err, store.ErrPersonMediaNoData)
}

func TestPersonMediaHoldsAllFourVCardMediaKinds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	for _, kind := range []store.PersonMediaKind{
		store.PersonMediaPhoto, store.PersonMediaLogo, store.PersonMediaSound, store.PersonMediaKey,
	} {
		_, err := st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
			MediaKind: kind, Data: []byte("synthetic-" + string(kind)),
			OriginalValue: "data:application/octet-stream;base64,<elided>",
			Envelope:      store.ValueEnvelope{Source: store.ProvenanceVCardImport},
		})
		require.NoError(err)
	}
	media, err := st.ListPersonMediaContext(ctx, personID, true)
	require.NoError(err)
	require.Len(media, 4)
	for _, row := range media {
		assert.True(row.HasData)
		assert.NotNil(row.ContentHash)
	}
}

func TestPersonMediaValidation(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	personID := newTestPerson(t, st)
	ctx := context.Background()
	_, err := st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: "avatar", Data: []byte("synthetic"),
		Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrInvalidPersonMediaKind)
	_, err = st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto,
		Envelope:  store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonMediaEmpty)
	_, err = st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, Data: make([]byte, store.MaxPersonMediaBytes+1),
		Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonMediaTooLarge)
}
