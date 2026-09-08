package store

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgreSQLPersonBriefKeepsOneCurrentVersionUnderConcurrentRegeneration
// checks that racing regenerations leave exactly one current version. If the
// transactions overlap, a unique constraint may reject one; if they run in
// sequence, both may commit successive versions.
func TestPostgreSQLPersonBriefKeepsOneCurrentVersionUnderConcurrentRegeneration(t *testing.T) {
	skipUnlessPostgresInternal(t)
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefFixture(t, "pg-concurrent")
	f.applyBrief(t, f.briefInsert(1000, personBriefNow))

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for index := range 2 {
		go func() {
			ready.Done()
			<-start
			input := f.briefInsert(int64(2000+index), personBriefNow.Add(time.Duration(index)*time.Hour))
			errs <- f.store.withTxContext(t.Context(), func(tx *loggedTx) error {
				_, applyErr := f.store.applyPersonBriefTx(t.Context(), tx, input)
				return applyErr
			})
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-errs, <-errs
	successes := 0
	for _, err := range []error{first, second} {
		if err == nil {
			successes++
		} else {
			checks.True(isPgError(err, "23505"), "expected a unique constraint conflict, got %v", err)
		}
	}
	checks.GreaterOrEqual(successes, 1, "at least one regeneration must commit")

	var currentRows int
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_briefs WHERE person_id = ? AND status = ?`),
		f.personID, PersonBriefStatusCurrent).Scan(&currentRows))
	checks.Equal(1, currentRows)
	versions, err := f.store.ListPersonBriefVersionsContext(t.Context(), f.personID, 10)
	requirements.NoError(err)
	checks.Len(versions, 1+successes, "only committed regenerations add a version")
}

// TestPostgreSQLPersonBriefStoresCanonicalJSONB proves the JSONB columns
// round-trip through the store's JSON bind expression: PostgreSQL parses and
// re-serializes JSONB, so a reader must still get the same document back, and a
// malformed document must be refused by the column type rather than stored.
func TestPostgreSQLPersonBriefStoresCanonicalJSONB(t *testing.T) {
	skipUnlessPostgresInternal(t)
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefFixture(t, "pg-jsonb")
	input := f.briefInsert(184233, personBriefNow)
	input.Structured = json.RawMessage(
		`{"highlights":[{"text":"synthetic","evidence_ids":["a","b"]}],"follow_ups":[]}`)
	brief := f.applyBrief(t, input)

	stored, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	requirements.NoError(err)
	checks.JSONEq(string(input.Structured), string(stored.Structured))
	checks.JSONEq(string(input.Boundary), string(stored.Boundary))

	var columnType string
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'person_briefs'
		  AND column_name = 'structured_json'`).Scan(&columnType))
	checks.Equal("jsonb", columnType)

	_, err = f.store.db.ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_briefs SET boundary_json = ?::jsonb WHERE id = ?`),
		`{"broken"`, brief.ID)
	requirements.Error(err, "PostgreSQL must refuse a malformed boundary document")
}

// TestPostgreSQLBriefEligibilityReadsBoundarySequence covers the eligibility
// read against JSONB, where boundary_json comes back as PostgreSQL's own
// serialization rather than the bytes the writer supplied.
func TestPostgreSQLBriefEligibilityReadsBoundarySequence(t *testing.T) {
	skipUnlessPostgresInternal(t)
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefFixture(t, "pg-eligibility")
	_, err := f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, true, "owner", false)
	requirements.NoError(err)
	f.applyBrief(t, f.briefInsert(184233, personBriefNow))

	eligible, err := f.store.ListBriefEligiblePeopleContext(t.Context(), 0, 10)
	requirements.NoError(err)
	requirements.Len(eligible, 1)
	checks.Equal(f.personID, eligible[0].PersonID)
	checks.Equal(int64(184233), eligible[0].ThroughSequence)
	checks.Equal(PersonBriefStatusCurrent, eligible[0].Status)
}
