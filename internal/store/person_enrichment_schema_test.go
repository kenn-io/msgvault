package store_test

import (
	"bytes"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonEnrichmentBackendParityMatrix(t *testing.T) {
	t.Run("exact consent and suppression survives deletion", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		st := testutil.NewTestStore(t)
		profile := enrichmentTestProfile(t)
		_, err := st.EnsurePersonEnrichmentProfile(t.Context(), profile)
		requirements.NoError(err)
		consent, created, err := st.GrantPersonEnrichmentConsent(
			t.Context(), profile.Fingerprint, "parity-test")
		requirements.NoError(err)
		requirements.True(created)
		checks.Equal(profile.Fingerprint, consent.ProfileFingerprint)

		hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x5a}, 32))
		requirements.NoError(err)
		digest := hasher.Digest(profile.ProviderNamespace,
			personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
			"parity-person@example.test")
		input := store.PersonEnrichmentSuppressionInput{
			ProviderNamespace: digest.ProviderNamespace,
			IdentifierClass:   digest.IdentifierClass, NormalizationVersion: digest.NormalizationVersion,
			KeyID: digest.KeyID, Digest: digest.Digest,
			Reason: store.PersonEnrichmentSuppressionDeletion, Actor: "parity-test",
		}
		requirements.NoError(st.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{input}))
		requirements.NoError(st.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{input}),
			"the exact tuple and immutable audit metadata are idempotent")

		participantID, err := st.EnsureParticipant(
			"parity-person@example.test", "Parity Person", "example.test")
		requirements.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
		requirements.NoError(err)
		keyID, err := hasher.KeyID()
		requirements.NoError(err)
		requirements.NoError(st.DeletePersonWithEnrichmentSuppressionsContext(
			t.Context(), store.DeletePersonEnrichmentInput{
				PersonID: person.ID, ExpectedRevision: person.Revision,
				ConfiguredKeyID: keyID, Actor: "parity-test",
				Reason:             store.PersonEnrichmentSuppressionDeletion,
				CurrentIdentifiers: []store.PersonEnrichmentSuppressionInput{input},
			}))
		found, err := st.HasPersonEnrichmentSuppressionContext(t.Context(), store.PersonEnrichmentSuppressionLookup{
			ProviderNamespace: input.ProviderNamespace,
			IdentifierClass:   input.IdentifierClass, NormalizationVersion: input.NormalizationVersion,
			KeyID: input.KeyID, Digest: input.Digest,
		})
		requirements.NoError(err)
		checks.True(found)
	})

	t.Run("manual run is immutably target bound", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newEnrichmentTriggerFixture(t, 1)
		f.grant(t, 0)
		_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
		requirements.NoError(err)
		first, created, err := f.store.StartManualPersonEnrichmentRunContext(
			t.Context(), f.person.ID, f.profiles[0].Fingerprint, "parity-manual", f.now)
		requirements.NoError(err)
		requirements.True(created)
		again, created, err := f.store.StartManualPersonEnrichmentRunContext(
			t.Context(), f.person.ID, f.profiles[0].Fingerprint, "parity-manual", f.now.Add(time.Minute))
		requirements.NoError(err)
		checks.False(created)
		checks.Equal(first.ID, again.ID)
		work := f.work(t, 0)
		requirements.Len(work, 1)
		requirements.NotNil(work[0].RunID)
		checks.Equal(first.ID, *work[0].RunID)
		checks.Equal("manual:parity-manual", work[0].TriggerGeneration)
	})

	t.Run("scheduled attempt resumes exact binding and fences stale lease", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newEnrichmentWorkFixture(t)
		run := f.startRun(t, "parity-scheduled")
		again, created, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
			Kind: "scheduled", RequestedBy: "parity-scheduled", RequestedAt: f.now.Add(time.Minute),
		})
		requirements.NoError(err)
		checks.False(created)
		checks.Equal(run.ID, again.ID)

		f.enqueue(t)
		lease := f.claim(t, run.ID, "parity-worker-a")
		checks.Equal(run.ID, lease.RunID)
		start := testAttemptStart(&f, run.ID, "e")
		attempt, created, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
		requirements.NoError(err)
		requirements.True(created)
		replayed, created, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
		requirements.NoError(err)
		checks.False(created)
		checks.Equal(attempt.ID, replayed.ID)

		requirements.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
		requirements.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
			State: personenrichment.AttemptPending, RequestID: "parity-request", JobID: "parity-job",
			StartedAt: f.now, AdapterVersion: "parity-adapter-v1", SchemaVersion: "parity-wire-v1",
			ProgramFingerprint: strings.Repeat("f", 64),
		}))
		requirements.NoError(f.store.SchedulePoll(t.Context(), attempt.Token, personenrichment.Result{
			State: personenrichment.ResultPending, RequestID: "parity-request", JobID: "parity-job",
			PollAfter: time.Minute, AdapterVersion: "parity-adapter-v1", SchemaVersion: "parity-wire-v1",
		}))
		err = f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
			State: "succeeded", CompletedAt: f.now,
		})
		requirements.ErrorIs(err, store.ErrRunNotTerminal)
		running, err := f.store.ListRunningRuns(t.Context(), personenrichment.RunningRunFilter{Limit: 10})
		requirements.NoError(err)
		requirements.Len(running, 1)
		checks.Equal(run.ID, running[0].ID)

		f.setNow(f.now.Add(time.Minute + time.Nanosecond))
		reclaimed := f.claim(t, run.ID, "parity-worker-b")
		requirements.NotNil(reclaimed.ActiveAttempt)
		checks.Equal(attempt.ID, reclaimed.ActiveAttempt.ID)
		checks.Equal("parity-job", reclaimed.ActiveAttempt.JobID)
		checks.Equal(attempt.ID, reclaimed.Token.AttemptID)
		err = f.store.MarkTerminal(t.Context(), lease.Token, personenrichment.SafeFailure{
			Class: personenrichment.FailureTerminal, Message: "stale parity worker",
		})
		requirements.ErrorIs(err, store.ErrStaleLease)
		requirements.NoError(f.store.MarkTerminal(t.Context(), reclaimed.Token, personenrichment.SafeFailure{
			Class: personenrichment.FailureTerminal, Message: "synthetic terminal result",
		}))
		requirements.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
			State: "failed", CompletedAt: f.now,
		}))
	})
}

func TestPersonEnrichmentSchemaEnforcesConsentAuditState(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	profile := enrichmentTestProfile(t)
	_, err := st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_enrichment_consents
			(profile_fingerprint, granted_by, revoked_by)
		VALUES (?, 'cli', 'cli')`), profile.Fingerprint)
	require.Error(err, "revocation actor without timestamp must fail")

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_enrichment_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'cli')`), profile.Fingerprint)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_enrichment_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'second')`), profile.Fingerprint)
	require.Error(err, "only one active consent is allowed")

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_enrichment_consents
		SET revoked_by = 'cli', revoked_at = CURRENT_TIMESTAMP
		WHERE profile_fingerprint = ? AND revoked_at IS NULL`), profile.Fingerprint)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_enrichment_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'second')`), profile.Fingerprint)
	require.NoError(err, "a revoked consent must not block regrant")
}

func TestPersonEnrichmentSchemaSQLiteForeignKeysAndIndexes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	columns := pragmaTextColumn(t, st.DB(), `PRAGMA table_info(person_enrichment_suppressions)`, 1)
	assert.ElementsMatch([]string{
		"id", "provider_namespace", "identifier_class", "normalization_version",
		"key_id", "digest", "reason", "actor", "created_at",
	}, columns)
	assert.NotContains(columns, "person_id")
	assert.NotContains(columns, "raw_value")
	assert.NotContains(columns, "normalized_value")
	assert.NotContains(columns, "credential")

	suppressionFKs := pragmaTextColumn(
		t, st.DB(), `PRAGMA foreign_key_list(person_enrichment_suppressions)`, 2)
	assert.Empty(suppressionFKs, "suppressions must survive person deletion")
	consentFKs := pragmaTextColumn(
		t, st.DB(), `PRAGMA foreign_key_list(person_enrichment_consents)`, 2)
	assert.Equal([]string{"person_enrichment_profiles"}, consentFKs)

	indexes := pragmaIndexes(t, st.DB(), "person_enrichment_consents")
	active, ok := indexes["person_enrichment_consents_active"]
	require.True(ok)
	assert.True(active.unique)
	assert.True(active.partial)
	assert.Equal([]string{"profile_fingerprint"}, pragmaIndexColumns(
		t, st.DB(), "person_enrichment_consents_active"))

	indexes = pragmaIndexes(t, st.DB(), "person_enrichment_suppressions")
	wantUniqueColumns := []string{
		"provider_namespace", "identifier_class", "normalization_version", "key_id", "digest",
	}
	foundUniqueTuple := false
	for name, index := range indexes {
		if index.unique && slices.Equal(
			wantUniqueColumns, pragmaIndexColumns(t, st.DB(), name)) {
			foundUniqueTuple = true
		}
	}
	assert.True(foundUniqueTuple, "suppression exact tuple needs a unique index")
}

func TestPersonEnrichmentSchemaWorkAttemptPointerAndIndexes(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite PRAGMA shape is covered here; portable behavior runs on both backends")
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	workColumns := pragmaTextColumn(t, st.DB(), `PRAGMA table_info(person_enrichment_work)`, 1)
	assert.Contains(workColumns, "active_attempt_id")
	assert.Contains(workColumns, "run_id")
	assert.Contains(workColumns, "has_fresh_trigger")
	attemptColumns := pragmaTextColumn(t, st.DB(), `PRAGMA table_info(person_enrichment_attempts)`, 1)
	assert.Contains(attemptColumns, "run_id")
	assert.Contains(attemptColumns, "provider_job_id")
	assert.Contains(attemptColumns, "program_fingerprint")
	assert.Contains(attemptColumns, "targets_json")
	assert.Contains(attemptColumns, "provider_started_at")
	assert.Contains(attemptColumns, "dispatch_authorized_at")

	workIndexes := pragmaIndexes(t, st.DB(), "person_enrichment_work")
	foundUniquePointer := false
	for name, index := range workIndexes {
		if index.unique && slices.Equal([]string{"active_attempt_id"}, pragmaIndexColumns(t, st.DB(), name)) {
			foundUniquePointer = true
		}
	}
	assert.True(foundUniquePointer, "active attempt pointer must be unique and nullable")

	attemptIndexes := pragmaIndexes(t, st.DB(), "person_enrichment_attempts")
	job, ok := attemptIndexes["person_enrichment_attempts_provider_job"]
	require.True(ok)
	assert.True(job.unique)
	assert.True(job.partial)
}

func TestPersonEnrichmentSchemaManualRunTargetBinding(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite PRAGMA shape is covered here; portable behavior runs on both backends")
	st := testutil.NewSQLiteTestStore(t)
	columns := pragmaTextColumn(t, st.DB(),
		`PRAGMA table_info(person_enrichment_manual_run_targets)`, 1)
	assert.ElementsMatch(t, []string{
		"run_id", "person_id", "profile_fingerprint", "created_at",
	}, columns)
	assert.ElementsMatch(t, []string{
		"person_enrichment_runs", "person_enrichment_profiles",
	}, pragmaTextColumn(t, st.DB(),
		`PRAGMA foreign_key_list(person_enrichment_manual_run_targets)`, 2))
}

func TestPersonEnrichmentAttemptProviderStartedAtLegacyMigrationParity(t *testing.T) {
	for name, migrations := range map[string][]store.ColumnMigration{
		"sqlite":   (&store.SQLiteDialect{}).LegacyColumnMigrations(),
		"postgres": (&store.PostgreSQLDialect{}).LegacyColumnMigrations(),
	} {
		t.Run(name, func(t *testing.T) {
			matches := 0
			for _, migration := range migrations {
				if migration.Desc == "person_enrichment_attempts.provider_started_at" {
					matches++
					assert.Contains(t, migration.SQL, "ADD COLUMN")
					assert.Contains(t, migration.SQL, "provider_started_at")
				}
			}
			assert.Equal(t, 1, matches)
		})
	}
}

func TestPersonEnrichmentAttemptDispatchAuthorizationLegacyMigrationParity(t *testing.T) {
	for name, migrations := range map[string][]store.ColumnMigration{
		"sqlite":   (&store.SQLiteDialect{}).LegacyColumnMigrations(),
		"postgres": (&store.PostgreSQLDialect{}).LegacyColumnMigrations(),
	} {
		t.Run(name, func(t *testing.T) {
			matches := 0
			for _, migration := range migrations {
				if migration.Desc == "person_enrichment_attempts.dispatch_authorized_at" {
					matches++
					assert.Contains(t, migration.SQL, "ADD COLUMN")
					assert.Contains(t, migration.SQL, "dispatch_authorized_at")
				}
			}
			assert.Equal(t, 1, matches)
		})
	}
}

func TestPersonEnrichmentAttemptTargetsLegacyMigrationParity(t *testing.T) {
	for name, migrations := range map[string][]store.ColumnMigration{
		"sqlite":   (&store.SQLiteDialect{}).LegacyColumnMigrations(),
		"postgres": (&store.PostgreSQLDialect{}).LegacyColumnMigrations(),
	} {
		t.Run(name, func(t *testing.T) {
			matches := 0
			for _, migration := range migrations {
				if migration.Desc == "person_enrichment_attempts.targets_json" {
					matches++
					assert.Contains(t, migration.SQL, "ADD COLUMN")
					assert.Contains(t, migration.SQL, "targets_json TEXT")
				}
			}
			assert.Equal(t, 1, matches)
		})
	}
}

func TestPersonEnrichmentFreshTriggerLegacyMigrationParity(t *testing.T) {
	for name, migrations := range map[string][]store.ColumnMigration{
		"sqlite":   (&store.SQLiteDialect{}).LegacyColumnMigrations(),
		"postgres": (&store.PostgreSQLDialect{}).LegacyColumnMigrations(),
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			matches := 0
			for _, migration := range migrations {
				if migration.Desc == "person_enrichment_work.has_fresh_trigger" {
					matches++
					assert.Contains(migration.SQL, "ADD COLUMN")
					assert.Contains(migration.SQL, "has_fresh_trigger")
				}
			}
			assert.Equal(1, matches)
		})
	}
}

func TestInitSchemaAddsDurableAttemptTargetsToLegacySchema(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	drop := `ALTER TABLE person_enrichment_attempts DROP COLUMN targets_json`
	if st.IsPostgreSQL() {
		drop += ` CASCADE`
	}
	_, err := st.DB().Exec(drop)
	requirements.NoError(err)

	requirements.NoError(st.InitSchema())
	var columnCount int
	query := `SELECT COUNT(*) FROM pragma_table_info('person_enrichment_attempts') WHERE name = 'targets_json'`
	if st.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM information_schema.columns
		         WHERE table_schema = current_schema()
		           AND table_name = 'person_enrichment_attempts' AND column_name = 'targets_json'`
	}
	requirements.NoError(st.DB().QueryRow(query).Scan(&columnCount))
	assert.Equal(t, 1, columnCount)
}

func TestInitSchemaAddsProviderStartedAtToLegacySchema(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	drop := `ALTER TABLE person_enrichment_attempts DROP COLUMN provider_started_at`
	if st.IsPostgreSQL() {
		drop += ` CASCADE`
	}
	_, err := st.DB().Exec(drop)
	requirements.NoError(err)

	requirements.NoError(st.InitSchema())
	var columnCount int
	query := `SELECT COUNT(*) FROM pragma_table_info('person_enrichment_attempts') WHERE name = 'provider_started_at'`
	if st.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM information_schema.columns
		         WHERE table_schema = current_schema()
		           AND table_name = 'person_enrichment_attempts' AND column_name = 'provider_started_at'`
	}
	requirements.NoError(st.DB().QueryRow(query).Scan(&columnCount))
	assert.Equal(t, 1, columnCount)
}

func TestInitSchemaAddsFreshTriggerMarkerToLegacySchema(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	drop := `ALTER TABLE person_enrichment_work DROP COLUMN has_fresh_trigger`
	if st.IsPostgreSQL() {
		drop += ` CASCADE`
	}
	_, err := st.DB().Exec(drop)
	require.NoError(err)

	require.NoError(st.InitSchema())
	var columnCount int
	query := `SELECT COUNT(*) FROM pragma_table_info('person_enrichment_work') WHERE name = 'has_fresh_trigger'`
	if st.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM information_schema.columns
		         WHERE table_schema = current_schema()
		           AND table_name = 'person_enrichment_work' AND column_name = 'has_fresh_trigger'`
	}
	require.NoError(st.DB().QueryRow(query).Scan(&columnCount))
	assert.Equal(1, columnCount)
}

func TestPersonEnrichmentSchemaResultMetadataPrivacyIdentityAndCitationIndexes(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite PRAGMA shape is covered here; portable behavior runs on both backends")
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	identifierColumns := pragmaTextColumn(t, st.DB(),
		`PRAGMA table_info(person_enrichment_attempt_identifiers)`, 1)
	assert.ElementsMatch([]string{
		"attempt_id", "provider_namespace", "identifier_class",
		"normalization_version", "key_id", "digest",
	}, identifierColumns)
	for _, forbidden := range []string{"person_id", "raw_value", "normalized_value", "credential", "key"} {
		assert.NotContains(identifierColumns, forbidden)
	}
	assert.Equal([]string{"person_enrichment_attempts"}, pragmaTextColumn(t, st.DB(),
		`PRAGMA foreign_key_list(person_enrichment_attempt_identifiers)`, 2))

	identityColumns := pragmaTextColumn(t, st.DB(),
		`PRAGMA table_info(person_enrichment_provider_identities)`, 1)
	assert.ElementsMatch([]string{
		"person_id", "provider_namespace", "provider_person_id", "confidence", "verified_at",
	}, identityColumns)
	indexes := pragmaIndexes(t, st.DB(), "person_enrichment_provider_identities")
	identityIndex, ok := indexes["person_enrichment_provider_identity_unique"]
	require.True(ok)
	assert.True(identityIndex.unique)
	assert.Equal([]string{"provider_namespace", "provider_person_id"}, pragmaIndexColumns(
		t, st.DB(), "person_enrichment_provider_identity_unique"))

	citationColumns := pragmaTextColumn(t, st.DB(), `PRAGMA table_info(person_enrichment_citations)`, 1)
	assert.ElementsMatch([]string{
		"id", "person_id", "citation_key", "canonical_url", "title", "publisher",
		"excerpt", "published_at", "retrieved_at", "created_at",
	}, citationColumns)
	assert.Equal([]string{"persons"}, pragmaTextColumn(t, st.DB(),
		`PRAGMA foreign_key_list(person_enrichment_citations)`, 2))
	assert.ElementsMatch([]string{"person_enrichment_citations", "person_enrichment_attempts"},
		pragmaTextColumn(t, st.DB(), `PRAGMA foreign_key_list(person_enrichment_attempt_citations)`, 2))
	assert.Equal([]string{"person_enrichment_attempts"}, pragmaTextColumn(t, st.DB(),
		`PRAGMA foreign_key_list(person_enrichment_attempt_sources)`, 2))
}

type sqliteIndexShape struct {
	unique  bool
	partial bool
}

func pragmaIndexes(t *testing.T, db interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, table string) map[string]sqliteIndexShape {
	t.Helper()
	rows, err := db.Query(`PRAGMA index_list(` + table + `)`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	result := make(map[string]sqliteIndexShape)
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		require.NoError(t, rows.Scan(&sequence, &name, &unique, &origin, &partial))
		result[name] = sqliteIndexShape{unique: unique == 1, partial: partial == 1}
		_ = sequence
		_ = origin
	}
	require.NoError(t, rows.Err())
	return result
}

func pragmaIndexColumns(t *testing.T, db interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, index string) []string {
	t.Helper()
	return pragmaTextColumn(t, db, `PRAGMA index_info(`+index+`)`, 2)
}

func pragmaTextColumn(t *testing.T, db interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, query string, target int) []string {
	t.Helper()
	rows, err := db.Query(query)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	columnTypes, err := rows.ColumnTypes()
	require.NoError(t, err)
	values := make([]string, 0)
	for rows.Next() {
		dest := make([]any, len(columnTypes))
		holders := make([]any, len(columnTypes))
		for i := range dest {
			dest[i] = &holders[i]
		}
		require.NoError(t, rows.Scan(dest...))
		value, ok := holders[target].(string)
		if !ok {
			if bytes, bytesOK := holders[target].([]byte); bytesOK {
				value, ok = string(bytes), true
			}
		}
		require.True(t, ok)
		values = append(values, value)
	}
	require.NoError(t, rows.Err())
	return values
}
