package store_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func dailyNotePerson(t *testing.T, f *storetest.Fixture, suffix string) *store.Person {
	t.Helper()
	participantID := f.EnsureParticipant(
		suffix+"@example.invalid", "Test Person", "example.invalid",
	)
	person, _, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	return person
}

func TestDailyNoteValidationAndCalendarDates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	for _, test := range []struct {
		name  string
		input store.DailyNoteEntryInput
		want  error
	}{
		{"missing date", store.DailyNoteEntryInput{Body: "note"}, store.ErrDailyNoteDateRequired},
		{"malformed date", store.DailyNoteEntryInput{LocalDate: "2026-7-30", Body: "note"}, store.ErrDailyNoteDateRequired},
		{"impossible date", store.DailyNoteEntryInput{LocalDate: "2026-02-29", Body: "note"}, store.ErrDailyNoteDateRequired},
		{"blank body", store.DailyNoteEntryInput{LocalDate: "2026-07-30", Body: " \t\n "}, store.ErrDailyNoteBodyRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.Store.CreateDailyNoteEntryContext(t.Context(), test.input)
			require.ErrorIs(err, test.want)
		})
	}

	assert.True(store.IsValidLocalDate("2024-02-29"))
	assert.False(store.IsValidLocalDate("2026-02-29"))
	assert.False(store.IsValidLocalDate("２０２６-07-30"))

	_, err := f.Store.ListDailyNoteEntriesContext(t.Context(), "today", 1, 0)
	require.ErrorIs(err, store.ErrDailyNoteDateRequired)
	_, err = f.Store.ListDailyNoteEntriesContext(t.Context(), "2026-07-30", 1, -1)
	require.Error(err)
}

func TestDailyNoteDatabaseDateChecksAreASCIIOnly(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	for _, table := range []string{"daily_note_entries", "daily_note_day_sequences"} {
		var statement string
		if table == "daily_note_entries" {
			statement = `INSERT INTO daily_note_entries
				(local_date, ordinal, body, author, source) VALUES (?, 1, 'body', '', 'user')`
		} else {
			statement = `INSERT INTO daily_note_day_sequences (local_date, last_ordinal) VALUES (?, 1)`
		}
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(statement), "２０２６-07-30")
		require.Error(err, table)
	}
	_, err := f.Store.DB().ExecContext(t.Context(),
		`INSERT INTO daily_note_day_sequences (local_date, last_ordinal) VALUES (NULL, 1)`)
	require.Error(err, "allocator dates must have SQLite/PostgreSQL NOT NULL parity")
}

func TestDailyNoteOrderingTargetsPaginationAndDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	firstPerson := dailyNotePerson(t, f, "first")
	secondPerson := dailyNotePerson(t, f, "second")

	first, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-07-30", Body: "  morning  ", Author: "tester",
		PersonIDs: []int64{secondPerson.ID, firstPerson.ID, secondPerson.ID},
	})
	require.NoError(err)
	second, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-07-30", Body: "afternoon",
	})
	require.NoError(err)
	otherDay, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-07-31", Body: "other",
	})
	require.NoError(err)

	assert.Equal(int64(1), first.Ordinal)
	assert.Equal(int64(2), second.Ordinal)
	assert.Equal(int64(1), otherDay.Ordinal)
	assert.Equal("morning", first.Body)
	assert.Equal("user", first.Source)
	assert.Equal([]int64{firstPerson.ID, secondPerson.ID}, first.PersonIDs)

	page, err := f.Store.ListDailyNoteEntriesContext(t.Context(), "2026-07-30", 1, 1)
	require.NoError(err)
	require.Len(page, 1)
	assert.Equal(second.ID, page[0].ID)

	forPerson, err := f.Store.ListDailyNoteEntriesForPersonContext(
		t.Context(), firstPerson.ID, "", 0, 0,
	)
	require.NoError(err)
	require.Len(forPerson, 1)
	assert.Equal([]int64{firstPerson.ID, secondPerson.ID}, forPerson[0].PersonIDs)

	require.NoError(f.Store.DeletePerson(firstPerson.ID, firstPerson.Revision))
	kept, err := f.Store.ListDailyNoteEntriesContext(t.Context(), "2026-07-30", 10, 0)
	require.NoError(err)
	require.Len(kept, 2)
	assert.Equal([]int64{secondPerson.ID}, kept[0].PersonIDs)

	require.NoError(f.Store.DeleteDailyNoteEntryContext(t.Context(), second.ID))
	require.ErrorIs(
		f.Store.DeleteDailyNoteEntryContext(t.Context(), second.ID),
		store.ErrDailyNoteEntryNotFound,
	)
	replacement, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-07-30", Body: "replacement",
	})
	require.NoError(err)
	assert.Equal(int64(3), replacement.Ordinal)

	entries, err := f.Store.ListDailyNoteEntriesContext(t.Context(), "2026-07-30", 10, 0)
	require.NoError(err)
	require.Len(entries, 2)
	assert.Equal([]int64{1, 3}, []int64{entries[0].Ordinal, entries[1].Ordinal})

	require.NoError(f.Store.DeleteDailyNoteEntryContext(t.Context(), first.ID))
	var targetRows int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(),
		f.Store.Rebind(`SELECT COUNT(*) FROM daily_note_entry_persons WHERE entry_id = ?`),
		first.ID).Scan(&targetRows))
	assert.Zero(targetRows)
}

func TestDailyNoteInvalidTargetsRollbackEntryAndAllocator(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-07-30", Body: "invalid zero", PersonIDs: []int64{0},
	})
	require.ErrorIs(err, store.ErrPersonNotFound)
	assert.Equal(store.ErrPersonNotFound.Error(), err.Error())

	_, err = f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-07-30", Body: "missing", PersonIDs: []int64{999999},
	})
	require.ErrorIs(err, store.ErrPersonNotFound)
	assert.Equal(store.ErrPersonNotFound.Error(), err.Error())

	entry, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-07-30", Body: "first committed",
	})
	require.NoError(err)
	assert.Equal(int64(1), entry.Ordinal)
}

func TestDailyNoteTargetInsertFailureRollsBackAllocator(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person := dailyNotePerson(t, f, "rollback")
	var dropStatements []string
	if f.Store.IsPostgreSQL() {
		_, err := f.Store.DB().ExecContext(t.Context(), `
			CREATE FUNCTION fail_daily_note_target() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'injected daily note target failure';
			END
			$$
		`)
		require.NoError(err)
		_, err = f.Store.DB().ExecContext(t.Context(), `
			CREATE TRIGGER fail_daily_note_target
			BEFORE INSERT ON daily_note_entry_persons
			FOR EACH ROW EXECUTE FUNCTION fail_daily_note_target()
		`)
		require.NoError(err)
		dropStatements = []string{
			`DROP TRIGGER fail_daily_note_target ON daily_note_entry_persons`,
			`DROP FUNCTION fail_daily_note_target()`,
		}
	} else {
		_, err := f.Store.DB().ExecContext(t.Context(), `
			CREATE TRIGGER fail_daily_note_target
			BEFORE INSERT ON daily_note_entry_persons
			BEGIN
				SELECT RAISE(ABORT, 'injected daily note target failure');
			END
		`)
		require.NoError(err)
		dropStatements = []string{`DROP TRIGGER fail_daily_note_target`}
	}

	_, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-08-05", Body: "must roll back", PersonIDs: []int64{person.ID},
	})
	require.Error(err)
	for _, statement := range dropStatements {
		_, err = f.Store.DB().ExecContext(t.Context(), statement)
		require.NoError(err)
	}

	var entries, sequence int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM daily_note_entries WHERE local_date = '2026-08-05'`).Scan(&entries))
	require.NoError(f.Store.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM daily_note_day_sequences WHERE local_date = '2026-08-05'`).Scan(&sequence))
	assert.Zero(entries)
	assert.Zero(sequence)

	committed, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-08-05", Body: "first committed",
	})
	require.NoError(err)
	assert.Equal(int64(1), committed.Ordinal)
}

func TestDailyNotePaginationDefaultsAndCaps(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO daily_note_day_sequences (local_date, last_ordinal) VALUES (?, ?)
	`), "2026-08-06", 505)
	require.NoError(err)
	for ordinal := 1; ordinal <= 505; ordinal++ {
		_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO daily_note_entries (local_date, ordinal, body, author, source)
			VALUES (?, ?, 'page fixture', '', 'user')
		`), "2026-08-06", ordinal)
		require.NoError(err)
	}
	require.NoError(tx.Commit())

	defaultPage, err := f.Store.ListDailyNoteEntriesContext(
		t.Context(), "2026-08-06", 0, 0,
	)
	require.NoError(err)
	assert.Len(defaultPage, store.DailyNoteDefaultLimit)
	assert.Equal(int64(1), defaultPage[0].Ordinal)

	cappedPage, err := f.Store.ListDailyNoteEntriesContext(
		t.Context(), "2026-08-06", 10_000, 0,
	)
	require.NoError(err)
	assert.Len(cappedPage, store.DailyNoteMaxLimit)
	assert.Equal(int64(500), cappedPage[len(cappedPage)-1].Ordinal)

	tail, err := f.Store.ListDailyNoteEntriesContext(
		t.Context(), "2026-08-06", 10, 500,
	)
	require.NoError(err)
	require.Len(tail, 5)
	assert.Equal(int64(501), tail[0].Ordinal)
	assert.Equal(int64(505), tail[4].Ordinal)
}

func TestDailyNoteConcurrentAppendsUseConsecutiveOrdinals(t *testing.T) {
	f := storetest.New(t)
	n := 24
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		n = 48
	}

	start := make(chan struct{})
	ordinals := make(chan int64, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			entry, err := f.Store.CreateDailyNoteEntryContext(context.Background(), store.DailyNoteEntryInput{
				LocalDate: "2026-08-01", Body: fmt.Sprintf("note %d", i),
			})
			if err != nil {
				errs <- err
				return
			}
			ordinals <- entry.Ordinal
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(ordinals)
	for err := range errs {
		require.NoError(t, err)
	}
	got := make([]int64, 0, n)
	for ordinal := range ordinals {
		got = append(got, ordinal)
	}
	slices.Sort(got)
	want := make([]int64, n)
	for i := range n {
		want[i] = int64(i + 1)
	}
	assert.Equal(t, want, got)
}

func TestDailyNoteConcurrentDifferentDays(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			date := "2026-08-02"
			if i%2 == 1 {
				date = "2026-08-03"
			}
			_, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
				LocalDate: date, Body: fmt.Sprintf("note %d", i),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(err)
	}
	for _, date := range []string{"2026-08-02", "2026-08-03"} {
		entries, err := f.Store.ListDailyNoteEntriesContext(t.Context(), date, 100, 0)
		require.NoError(err)
		require.Len(entries, 8)
		assert.Equal(int64(1), entries[0].Ordinal)
		assert.Equal(int64(8), entries[7].Ordinal)
	}
}

func TestDailyNoteAuthoringDoesNotTouchComputedActivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person := dailyNotePerson(t, f, "separate")
	tables := []string{
		"activity_events", "activity_event_persons", "activity_projection_queue",
		"person_contact_state",
	}
	before := make(map[string]int)
	for _, table := range tables {
		var count int
		require.NoError(f.Store.DB().QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM "+table).Scan(&count))
		before[table] = count
	}

	_, err := f.Store.CreateDailyNoteEntry(store.DailyNoteEntryInput{
		LocalDate: "2026-08-04", Body: "human note", PersonIDs: []int64{person.ID},
	})
	require.NoError(err)
	for _, table := range tables {
		var after int
		require.NoError(f.Store.DB().QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM "+table).Scan(&after))
		assert.Equal(before[table], after, table)
	}
}

func TestDailyNoteSchemaReinitializationIsIdempotent(t *testing.T) {
	f := storetest.New(t)
	require.NoError(t, f.Store.InitSchema())
	require.NoError(t, f.Store.InitSchema())
}
