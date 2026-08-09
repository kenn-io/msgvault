package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAutomaticProfileOrdinalsNeverReuseHistoricalSlots(t *testing.T) {
	t.Run("kind-scoped values", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := storetest.New(t).Store
		personID := newTestPerson(t, st)

		var points []*store.PersonContactPoint
		for _, email := range []string{"first@example.org", "second@example.org"} {
			point, err := st.AddPersonContactPointContext(
				t.Context(), personID, store.PersonContactPointInput{
					AddressKind: store.ContactAddressEmail, OriginalValue: email,
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				},
			)
			require.NoError(err)
			points = append(points, point)
		}
		assert.Equal(0, points[0].Envelope.Ordinal)
		assert.Equal(1, points[1].Envelope.Ordinal)
		require.NoError(st.SupersedePersonContactPointContext(
			t.Context(), personID, points[1].Envelope.ID, nil,
		))

		appended, err := st.AddPersonContactPointContext(
			t.Context(), personID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "third@example.org",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			},
		)
		require.NoError(err)
		assert.Equal(2, appended.Envelope.Ordinal,
			"append must not adopt a superseded value's history slot")
	})

	t.Run("owner-scoped values", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := storetest.New(t).Store
		personID := newTestPerson(t, st)

		var categories []*store.PersonCategory
		for _, category := range []string{"First", "Second"} {
			value, err := st.AddPersonCategoryContext(
				t.Context(), personID, store.PersonCategoryInput{
					OriginalValue: category,
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				},
			)
			require.NoError(err)
			categories = append(categories, value)
		}
		assert.Equal(0, categories[0].Envelope.Ordinal)
		assert.Equal(1, categories[1].Envelope.Ordinal)
		require.NoError(st.SupersedePersonCategoryContext(
			t.Context(), personID, categories[1].Envelope.ID, nil,
		))

		appended, err := st.AddPersonCategoryContext(
			t.Context(), personID, store.PersonCategoryInput{
				OriginalValue: "Third",
				Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			},
		)
		require.NoError(err)
		assert.Equal(2, appended.Envelope.Ordinal,
			"append must not adopt a superseded value's history slot")
	})
}
