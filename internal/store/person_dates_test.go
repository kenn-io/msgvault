package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonDatesRoundTripPartialPrecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	tests := []struct {
		kind     store.PersonDateKind
		date     store.PartialDate
		property string
		raw      string
	}{
		{store.PersonDateBirthday, partialDate(0, 4, 12), "BDAY", "--0412"},
		{store.PersonDateBirthday, partialDate(0, 4, 0), "BDAY", "--04"},
		{store.PersonDateAnniversary, partialDate(2014, 6, 0), "ANNIVERSARY", "2014-06"},
		{store.PersonDateDeath, partialDate(2024, 0, 0), "DEATHDATE", "2024"},
		{store.PersonDateCustom, partialDate(2019, 9, 30), "X-DATE", "20190930"},
	}
	for _, test := range tests {
		stored, err := st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
			DateKind: test.kind, Date: test.date, OriginalValue: test.raw,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport,
				VCard: store.VCardIdentity{Property: test.property}},
		})
		require.NoError(err)
		assert.Equal(test.date, stored.Date)
	}
	dates, err := st.ListPersonDatesContext(ctx, personID, true)
	require.NoError(err)
	assert.Len(dates, 5)
}

func TestPersonDateDayOnlyRoundTrips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	dayOnly := partialDate(0, 0, 12)

	stored, err := st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind:      store.PersonDateBirthday,
		Date:          dayOnly,
		OriginalValue: "---12",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport},
	})
	require.NoError(err)
	assert.Equal(dayOnly, stored.Date)
	assert.Equal("---12", stored.Date.String())

	dates, err := st.ListPersonDatesContext(ctx, personID, true)
	require.NoError(err)
	require.Len(dates, 1)
	assert.Equal(dayOnly, dates[0].Date)
}

func TestPersonDateAcceptsTextValueAndCalendarScale(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	personID := newTestPerson(t, st)
	stored, err := st.AddPersonDateContext(context.Background(), personID, store.PersonDateInput{
		DateKind: store.PersonDateBirthday, DateText: new("circa 1800"),
		CalendarScale: new("gregorian"), OriginalValue: "circa 1800",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport},
	})
	require.NoError(err)
	assert.True(stored.Date.IsZero())
	assert.Equal("circa 1800", *stored.DateText)
}

func TestPersonDateValidation(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	_, err := st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: "graduation", Date: partialDate(2001, 5, 1),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrInvalidPersonDateKind)
	_, err = st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: store.PersonDateBirthday,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonDateValueMissing)
	_, err = st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: store.PersonDateBirthday, Date: partialDate(1985, 2, 29),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrInvalidPartialDate)
}

func TestPersonDateComponentChecksAreEnforcedBySQL(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	insert := "INSERT INTO person_dates " +
		"(person_id, date_kind, date_year, date_month, date_day, original_value, source, confidence) VALUES "
	for _, values := range []string{
		"(?, 'birthday', 0, 4, 12, 'x', 'user', NULL)",
		"(?, 'birthday', 1985, 13, 1, 'x', 'user', NULL)",
		"(?, 'birthday', 1985, NULL, 12, 'x', 'user', NULL)",
		"(?, 'birthday', 1985, 4, 12, 'x', 'user', 0.5)",
		"(?, 'birthday', 1985, 4, 12, 'x', 'extraction', 1.5)",
	} {
		_, err := st.DB().ExecContext(ctx, st.Rebind(insert+values), personID)
		require.Error(err, values)
	}
	for _, values := range []string{
		"(?, 'birthday', NULL, 4, 12, '--0412', 'user', NULL)",
		"(?, 'birthday', NULL, 4, NULL, '--04', 'user', NULL)",
		"(?, 'birthday', NULL, NULL, 12, '---12', 'user', NULL)",
		"(?, 'death', 2024, NULL, NULL, '2024', 'user', NULL)",
		"(?, 'birthday', 1985, 4, 12, 'x', 'system', 0.5)",
	} {
		_, err := st.DB().ExecContext(ctx, st.Rebind(insert+values), personID)
		require.NoError(err, values)
	}
}
