package store_test

import (
	"database/sql"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocumentVectorChunkLifecycleSQLiteContract(t *testing.T) {
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite contract runs without MSGVAULT_TEST_DB")
	}
	runDocumentVectorChunkLifecycleContract(t)
}

func TestDocumentVectorGenerationLifecycleSQLiteContract(t *testing.T) {
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite contract runs without MSGVAULT_TEST_DB")
	}
	runDocumentVectorGenerationLifecycleContract(t)
}

func TestDocumentVectorOperationLockSerializesPostgresWriters(t *testing.T) {
	requirements := require.New(t)
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL-only cross-process writer lock")
	}
	f := storetest.New(t)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- f.Store.WithDocumentVectorOperationLock(t.Context(), func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- f.Store.WithDocumentVectorOperationLock(t.Context(), func() error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		requirements.FailNow("second document-vector writer entered while the first held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	requirements.NoError(<-firstDone)
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		requirements.FailNow("second document-vector writer did not acquire the released lock")
	}
	requirements.NoError(<-secondDone)
}

func TestDocumentVectorOperationLockSerializesSQLiteWriters(t *testing.T) {
	requirements := require.New(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite-only process-local writer lock")
	}
	f := storetest.New(t)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- f.Store.WithDocumentVectorOperationLock(t.Context(), func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- f.Store.WithDocumentVectorOperationLock(t.Context(), func() error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		requirements.FailNow("second document-vector writer entered while the first held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	requirements.NoError(<-firstDone)
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		requirements.FailNow("second document-vector writer did not acquire the released lock")
	}
	requirements.NoError(<-secondDone)
}

func runDocumentVectorGenerationLifecycleContract(t *testing.T) {
	t.Helper()
	t.Run("consent usage progress and same-policy rebuild", testDocumentVectorOperationsState)
	t.Run("coverage activation and rollback", testDocumentVectorCoverageAndActivation)
	t.Run("atomic generation swap and retirement", testDocumentVectorActivationSwapAndRetirement)
	t.Run("live token ordering and invalidation", testDocumentVectorLiveResolution)
	t.Run("derivative garbage collection preserves cleanup token", testDocumentVectorDerivativeGarbageCollection)
	t.Run("derivative purge preserves cleanup token", testDocumentVectorDerivativePurge)
	t.Run("retired token cleanup and purge", testDocumentVectorCleanupAndPurge)
	t.Run("obsolete cleanup rechecks live snapshot", testDocumentVectorObsoleteCleanup)
}

func testDocumentVectorOperationsState(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 12, 34, 56, 987654321, time.FixedZone("consent", 2*60*60))
	require.NoError(t, f.Store.InitSchema(), "the document-vector base schema is replay-safe")
	if f.Store.IsPostgreSQL() {
		var indexDefinition string
		require.NoError(t, f.Store.DB().QueryRow(`
			SELECT indexdef FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = 'idx_document_vector_generations_live_fingerprint'`).Scan(&indexDefinition))
		assert.NotContains(t, indexDefinition, "UNIQUE")
	} else {
		rows, err := f.Store.DB().Query(`PRAGMA index_list(document_vector_generations)`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		found := false
		for rows.Next() {
			var unique int
			var sequence, partial int
			var name, origin string
			require.NoError(t, rows.Scan(&sequence, &name, &unique, &origin, &partial))
			if name == "idx_document_vector_generations_live_fingerprint" {
				found = true
				assert.Zero(t, unique)
				break
			}
		}
		require.NoError(t, rows.Err())
		require.True(t, found, "document vector generation fingerprint index")
	}

	egressFingerprint := strings.Repeat("e", 64)
	consentSpec := store.DocumentVectorConsentSpec{
		DocumentVectorGenerationSpec: generation.DocumentVectorGenerationSpec,
		EgressFingerprint:            egressFingerprint,
		Purpose:                      "document_embedding",
	}
	consent, created, err := f.Store.RecordDocumentVectorConsent(t.Context(), consentSpec, now)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, generation.DocumentVectorGenerationSpec, consent.DocumentVectorGenerationSpec)
	assert.Equal(t, now.UTC().Truncate(time.Millisecond), consent.ConsentedAt)

	gotConsent, err := f.Store.GetDocumentVectorConsent(t.Context(), egressFingerprint)
	require.NoError(t, err)
	require.NotNil(t, gotConsent)
	assert.Equal(t, consent, *gotConsent)

	collision := consentSpec
	collision.Model = "different-model"
	_, _, err = f.Store.RecordDocumentVectorConsent(t.Context(), collision, now)
	require.ErrorContains(t, err, "fingerprint")

	err = f.Store.CheckpointDocumentVectorBuild(t.Context(), generation.ID, 42, false, store.DocumentVectorUsageDelta{
		ProviderCalls: 1, ProviderDocuments: 2, ProviderChunks: 3, ProviderInputChars: 123,
	}, now)
	require.NoError(t, err)
	cursor, err := f.Store.GetDocumentVectorBuildCursor(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(42), cursor)
	usage, err := f.Store.GetDocumentVectorProviderUsage(t.Context(), generation.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, int64(1), usage.ProviderCalls)
	assert.Equal(t, int64(2), usage.ProviderDocuments)
	assert.Equal(t, int64(3), usage.ProviderChunks)
	assert.Equal(t, int64(123), usage.ProviderInputChars)
	queryEgressFingerprint := strings.Repeat("d", 64)
	operations, err := f.Store.GetDocumentVectorOperationsStatus(t.Context(), generation.DocumentVectorGenerationSpec, egressFingerprint, queryEgressFingerprint, 0, "", 10)
	require.NoError(t, err)
	require.NotNil(t, operations.DocumentConsent)
	assert.Nil(t, operations.QueryConsent)
	require.NotNil(t, operations.Coverage)
	assert.Equal(t, store.DocumentVectorCoverage{Required: 1, Ready: 0}, *operations.Coverage)

	require.NoError(t, f.Store.CheckpointDocumentVectorBuild(t.Context(), generation.ID, 0, true, store.DocumentVectorUsageDelta{}, now.Add(time.Second)))
	cursor, err = f.Store.GetDocumentVectorBuildCursor(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.Zero(t, cursor)
	require.ErrorContains(t, f.Store.CheckpointDocumentVectorBuild(t.Context(), generation.ID, 1, false, store.DocumentVectorUsageDelta{}, time.Time{}), "time")

	readyAllDocumentVectorChunks(t, f, generation, now)
	require.NoError(t, f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))
	err = f.Store.CheckpointDocumentVectorBuild(t.Context(), generation.ID, 99, false, store.DocumentVectorUsageDelta{
		ProviderCalls: 2, ProviderDocuments: 3, ProviderChunks: 4, ProviderInputChars: 456,
	}, now.Add(1500*time.Millisecond))
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)
	usage, err = f.Store.GetDocumentVectorProviderUsage(t.Context(), generation.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, int64(3), usage.ProviderCalls, "completed worker usage survives concurrent activation")
	assert.Equal(t, int64(5), usage.ProviderDocuments)
	assert.Equal(t, int64(7), usage.ProviderChunks)
	assert.Equal(t, int64(579), usage.ProviderInputChars)
	cursor, err = f.Store.GetDocumentVectorBuildCursor(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.Zero(t, cursor, "an active generation cursor is not advanced")
	_, err = f.Store.StartDocumentVectorRebuild(t.Context(), generation.ID, generation.DocumentVectorGenerationSpec, time.Time{})
	require.ErrorContains(t, err, "time")
	rebuild, err := f.Store.StartDocumentVectorRebuild(t.Context(), generation.ID, generation.DocumentVectorGenerationSpec, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorGenerationBuilding, rebuild.State)
	assert.Equal(t, generation.Fingerprint, rebuild.Fingerprint)
	assert.Equal(t, now.Add(2*time.Second).UTC().Truncate(time.Millisecond), rebuild.CreatedAt)
	resumed, created, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), generation.DocumentVectorGenerationSpec)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, rebuild.ID, resumed.ID)
	active, err := f.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, generation.ID, active.ID)

	t.Run("configured target rotation builds beside the old active generation", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		rotatedFixture, old := seedDocumentVectorGenerationWithChunks(t, 1)
		readyAllDocumentVectorChunks(t, rotatedFixture, old, now.Add(-time.Second))
		require.NoError(rotatedFixture.Store.ActivateDocumentVectorGeneration(t.Context(), old.ID, now))
		profile := rotatedDocumentVectorProfile()
		_, err := rotatedFixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
		require.NoError(err)
		require.NoError(rotatedFixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
			ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
			RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
		}))
		_, err = rotatedFixture.Store.DB().Exec(rotatedFixture.Store.Rebind(`
			UPDATE document_index_state SET target_profile_id = ? WHERE singleton = 1`), profile.ID)
		require.NoError(err)
		desired := old.DocumentVectorGenerationSpec
		desired.Fingerprint = strings.Repeat("3", 64)
		desired.TargetExtractionProfileID = profile.ID
		operations, err := rotatedFixture.Store.GetDocumentVectorOperationsStatus(t.Context(), desired, strings.Repeat("d", 64), strings.Repeat("e", 64), 0, "", 10)
		require.NoError(err)
		require.NotNil(operations.Selected)
		assert.Equal(old.ID, operations.Selected.GenerationID)
		assert.Nil(operations.Coverage, "coverage is undefined for the old target")
		building, err := rotatedFixture.Store.StartDocumentVectorRebuild(t.Context(), old.ID, desired, now.Add(time.Second))
		require.NoError(err)
		assert.Equal(desired, building.DocumentVectorGenerationSpec)
		stillActive, err := rotatedFixture.Store.GetActiveDocumentVectorGeneration(t.Context())
		require.NoError(err)
		require.NotNil(stillActive)
		assert.Equal(old.ID, stillActive.ID)
	})

	t.Run("observed usage survives target rotation and retirement without advancing cursor", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		rotatedFixture, building := seedDocumentVectorGenerationWithChunks(t, 1)
		require.NoError(rotatedFixture.Store.CheckpointDocumentVectorBuild(
			t.Context(), building.ID, 7, false, store.DocumentVectorUsageDelta{}, now))
		profile := rotatedDocumentVectorProfile()
		_, err := rotatedFixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
		require.NoError(err)
		_, err = rotatedFixture.Store.DB().Exec(rotatedFixture.Store.Rebind(
			`UPDATE document_index_state SET target_profile_id = ? WHERE singleton = 1`), profile.ID)
		require.NoError(err)
		err = rotatedFixture.Store.CheckpointDocumentVectorBuild(t.Context(), building.ID, 99, false,
			store.DocumentVectorUsageDelta{ProviderCalls: 1, ProviderChunks: 2, ProviderInputChars: 30}, now.Add(time.Second))
		require.ErrorIs(err, store.ErrDocumentVectorInvalidGenerationState)
		cursor, err := rotatedFixture.Store.GetDocumentVectorBuildCursor(t.Context(), building.ID)
		require.NoError(err)
		assert.Equal(int64(7), cursor)
		retired, err := rotatedFixture.Store.RetireDocumentVectorGeneration(t.Context(), building.ID, now.Add(2*time.Second))
		require.NoError(err)
		assert.True(retired)
		err = rotatedFixture.Store.CheckpointDocumentVectorBuild(t.Context(), building.ID, 101, false,
			store.DocumentVectorUsageDelta{ProviderCalls: 2, ProviderDocuments: 1, ProviderInputChars: 40}, now.Add(3*time.Second))
		require.ErrorIs(err, store.ErrDocumentVectorInvalidGenerationState)
		usage, err := rotatedFixture.Store.GetDocumentVectorProviderUsage(t.Context(), building.Fingerprint)
		require.NoError(err)
		assert.Equal(int64(3), usage.ProviderCalls)
		assert.Equal(int64(1), usage.ProviderDocuments)
		assert.Equal(int64(2), usage.ProviderChunks)
		assert.Equal(int64(70), usage.ProviderInputChars)
		cursor, err = rotatedFixture.Store.GetDocumentVectorBuildCursor(t.Context(), building.ID)
		require.NoError(err)
		assert.Equal(int64(7), cursor)
		purged, err := rotatedFixture.Store.PurgeRetiredDocumentVectorGeneration(t.Context(), building.ID)
		require.NoError(err)
		assert.True(purged)
		err = rotatedFixture.Store.CheckpointDocumentVectorBuildForFingerprint(t.Context(), building.ID, building.Fingerprint,
			0, true, store.DocumentVectorUsageDelta{ProviderCalls: 1, ProviderInputChars: 5}, now.Add(4*time.Second))
		require.ErrorIs(err, store.ErrDocumentVectorInvalidGenerationState)
		usage, err = rotatedFixture.Store.GetDocumentVectorProviderUsage(t.Context(), building.Fingerprint)
		require.NoError(err)
		assert.Equal(int64(4), usage.ProviderCalls, "purged-generation completion retains observed usage by fingerprint")
		assert.Equal(int64(75), usage.ProviderInputChars)
	})
}

func testDocumentVectorObsoleteCleanup(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 23, 15, 0, 654321000, time.FixedZone("cleanup-offset", -2*60*60))
	claims := readyAllDocumentVectorChunks(t, f, generation, now)
	require.Len(t, claims, 1)
	claim := claims[0]
	var messageID int64
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT message_id FROM document_occurrences WHERE canonical_blob_hash = ?`), claim.CanonicalBlobHash).Scan(&messageID))
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`), now, messageID)
	require.NoError(t, err)
	page, err := f.Store.ParkObsoleteDocumentVectorTokens(t.Context(), generation.ID, "", 10, now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, []store.DocumentVectorCleanupToken{{GenerationID: generation.ID, Token: claim.Token}}, page.Tokens)
	assert.True(t, page.Exhausted)
	assert.Zero(t, page.AfterGenerationID)
	assert.Empty(t, page.AfterToken)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = NULL WHERE id = ?`), messageID)
	require.NoError(t, err)
	status, err := f.Store.GetDocumentVectorGenerationStatus(t.Context(), generation.ID, "", 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.Obsolete)
	assert.Equal(t, int64(1), status.CleanupPending)
	assert.Zero(t, status.ReadyLive, "a parked row cannot become live when authority returns")
	replayed, err := f.Store.ParkObsoleteDocumentVectorTokens(t.Context(), generation.ID, "", 10, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, page.Tokens, replayed.Tokens, "a crash after parking must replay the durable token")

	finalized, err := f.Store.FinalizeObsoleteDocumentVectorToken(t.Context(), generation.ID, claim.Token, now.Add(3*time.Second))
	require.NoError(t, err)
	assert.True(t, finalized)
	finalized, err = f.Store.FinalizeObsoleteDocumentVectorToken(t.Context(), generation.ID, claim.Token, now.Add(4*time.Second))
	require.NoError(t, err)
	assert.False(t, finalized)
	reclaimed, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "rebuild-worker", now.Add(5*time.Second), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	assert.Equal(t, claim.Token, reclaimed.Token)
	assert.Equal(t, 1, reclaimed.AttemptCount)

	t.Run("retired makes every token obsolete", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
		claims := readyAllDocumentVectorChunks(t, f, generation, now)
		require.Len(claims, 1)
		retired, err := f.Store.RetireDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second))
		require.NoError(err)
		require.True(retired)
		page, err := f.Store.ParkObsoleteDocumentVectorTokens(t.Context(), generation.ID, "", 10, now.Add(2*time.Second))
		require.NoError(err)
		assert.Equal([]store.DocumentVectorCleanupToken{{GenerationID: generation.ID, Token: claims[0].Token}}, page.Tokens)
		finalized, err := f.Store.FinalizeObsoleteDocumentVectorToken(t.Context(), generation.ID, claims[0].Token, now.Add(3*time.Second))
		require.NoError(err)
		assert.True(finalized)
		var cleanedAt sql.NullTime
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
			SELECT backend_cleaned_at FROM document_vector_publications
			WHERE generation_id = ? AND token = ?`), generation.ID, claims[0].Token).Scan(&cleanedAt))
		assert.True(cleanedAt.Valid, "retired finalization preserves the ledger for purge")
	})

	t.Run("retired waits for a live publication lease", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
		claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "retiring-worker", now, time.Minute)
		require.NoError(err)
		require.NotNil(claim)
		renewedUntil, err := f.Store.RenewDocumentVectorChunkClaim(
			t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence,
			now.Add(59*time.Second), time.Minute,
		)
		require.NoError(err)
		assert.Equal(now.Add(119*time.Second).UTC().Truncate(time.Millisecond), renewedUntil)
		retired, err := f.Store.RetireDocumentVectorGeneration(t.Context(), generation.ID, now.Add(60*time.Second))
		require.NoError(err)
		require.True(retired)
		beforeExpiry, err := f.Store.ParkObsoleteDocumentVectorTokens(t.Context(), generation.ID, "", 10, renewedUntil.Add(-time.Millisecond))
		require.NoError(err)
		assert.Empty(beforeExpiry.Tokens)
		atExpiry, err := f.Store.ParkObsoleteDocumentVectorTokens(t.Context(), generation.ID, "", 10, renewedUntil)
		require.NoError(err)
		assert.Equal([]store.DocumentVectorCleanupToken{{GenerationID: generation.ID, Token: claim.Token}}, atExpiry.Tokens)
	})

	t.Run("active generation parks before delete", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
		claims := readyAllDocumentVectorChunks(t, f, generation, now)
		require.Len(claims, 1)
		require.NoError(f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))
		var messageID int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
			SELECT message_id FROM document_occurrences WHERE canonical_blob_hash = ?`), claims[0].CanonicalBlobHash).Scan(&messageID))
		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`), now, messageID)
		require.NoError(err)
		page, err := f.Store.ParkObsoleteDocumentVectorTokens(t.Context(), generation.ID, "", 10, now.Add(2*time.Second))
		require.NoError(err)
		assert.Equal([]store.DocumentVectorCleanupToken{{GenerationID: generation.ID, Token: claims[0].Token}}, page.Tokens)
		_, err = f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET deleted_from_source_at = NULL WHERE id = ?`), messageID)
		require.NoError(err)
		live, err := f.Store.ResolveLiveDocumentVectorPublications(t.Context(), generation.ID, []string{claims[0].Token})
		require.NoError(err)
		assert.Empty(live)
		finalized, err := f.Store.FinalizeObsoleteDocumentVectorToken(t.Context(), generation.ID, claims[0].Token, now.Add(3*time.Second))
		require.NoError(err)
		assert.True(finalized)
		var publications int
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
			SELECT COUNT(*) FROM document_vector_publications WHERE generation_id = ?`), generation.ID).Scan(&publications))
		assert.Zero(publications)
	})
}

func testDocumentVectorDerivativeGarbageCollection(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 22, 0, 0, 0, time.UTC)
	claims := readyAllDocumentVectorChunks(t, f, generation, now)
	require.Len(t, claims, 1)
	require.NoError(t, f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))
	claim := claims[0]

	var attachmentID, messageID int64
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT attachment_id, message_id FROM document_occurrences
		WHERE canonical_blob_hash = ?`), claim.CanonicalBlobHash).Scan(&attachmentID, &messageID))
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(t, err)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, claim.SourceSequence+1)
	require.NoError(t, err)
	assert.False(t, eligible)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE document_extractions SET updated_at = ? WHERE id = ?`), now.Add(-48*time.Hour), claim.ExtractionID)
	require.NoError(t, err)

	result, err := f.Store.GarbageCollectDocumentDerivatives(t.Context(), now.Add(-24*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentDerivativeGCResult{ExtractionsRemoved: 1, CurrentHeadsRemoved: 1}, result)
	assertDocumentVectorPublicationSurvivesSourceRemoval(t, f, generation.ID, claim.Token)
}

func testDocumentVectorDerivativePurge(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 22, 30, 0, 0, time.UTC)
	claims := readyAllDocumentVectorChunks(t, f, generation, now)
	require.Len(t, claims, 1)
	require.NoError(t, f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))
	claim := claims[0]

	result, err := f.Store.PurgeDocumentDerivedByHash(t.Context(), claim.CanonicalBlobHash)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentDerivedPurgeResult{ExtractionsRemoved: 1, HeadsRemoved: 1}, result)
	assertDocumentVectorPublicationSurvivesSourceRemoval(t, f, generation.ID, claim.Token)
}

func assertDocumentVectorPublicationSurvivesSourceRemoval(
	t *testing.T, f *storetest.Fixture, generationID int64, token string,
) {
	t.Helper()
	var storedToken, state string
	var cleanedAt sql.NullTime
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT token, state, backend_cleaned_at
		FROM document_vector_publications
		WHERE generation_id = ? AND token = ?`), generationID, token).Scan(
		&storedToken, &state, &cleanedAt))
	assert.Equal(t, token, storedToken)
	assert.Equal(t, "ready", state)
	assert.False(t, cleanedAt.Valid)
	resolved, err := f.Store.ResolveLiveDocumentVectorPublications(t.Context(), generationID, []string{token})
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

func testDocumentVectorCoverageAndActivation(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 2)
	now := time.Date(2026, time.August, 20, 18, 19, 20, 456789000, time.FixedZone("coverage-offset", -3*60*60))
	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "coverage-worker", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.NoError(t, f.Store.CommitDocumentVectorPublication(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now.Add(time.Second)))

	coverage, err := f.Store.GetDocumentVectorCoverage(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorCoverage{Required: 2, Ready: 1}, coverage)
	assert.False(t, coverage.Complete())
	revisionBefore, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	err = f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(2*time.Second))
	require.ErrorIs(t, err, store.ErrDocumentVectorCoverageIncomplete)
	afterFailedActivation, err := f.Store.GetDocumentVectorGeneration(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorGenerationBuilding, afterFailedActivation.State)
	assert.Nil(t, afterFailedActivation.ActivatedAt)
	revisionAfterFailure, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	assert.Equal(t, revisionBefore, revisionAfterFailure)

	remaining := readyAllDocumentVectorChunks(t, f, generation, now.Add(3*time.Second))
	require.Len(t, remaining, 1)
	coverage, err = f.Store.GetDocumentVectorCoverage(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorCoverage{Required: 2, Ready: 2}, coverage)
	assert.True(t, coverage.Complete())

	activationTime := now.Add(4 * time.Second)
	require.NoError(t, f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, activationTime))
	active, err := f.Store.GetDocumentVectorGeneration(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorGenerationActive, active.State)
	require.NotNil(t, active.ActivatedAt)
	assert.Equal(t, activationTime.UTC().Truncate(time.Millisecond), active.ActivatedAt.UTC())
	revisionAfterActivation, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	assert.Equal(t, revisionBefore+1, revisionAfterActivation)

	empty := storetest.New(t)
	profile, _ := seedDocumentPublicationAuthority(t, empty)
	emptyGeneration, _, err := empty.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("0", 64), TargetExtractionProfileID: profile.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768,
	})
	require.NoError(t, err)
	emptyCoverage, err := empty.Store.GetDocumentVectorCoverage(t.Context(), emptyGeneration.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorCoverage{}, emptyCoverage)
	assert.True(t, emptyCoverage.Complete())
	require.NoError(t, empty.Store.ActivateDocumentVectorGeneration(t.Context(), emptyGeneration.ID, activationTime))
}

func testDocumentVectorActivationSwapAndRetirement(t *testing.T) {
	f, oldGeneration := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 19, 20, 21, 987654000, time.UTC)
	oldClaims := readyAllDocumentVectorChunks(t, f, oldGeneration, now)
	require.Len(t, oldClaims, 1)
	require.NoError(t, f.Store.ActivateDocumentVectorGeneration(t.Context(), oldGeneration.ID, now.Add(time.Second)))

	newSpec := oldGeneration.DocumentVectorGenerationSpec
	newSpec.Fingerprint = strings.Repeat("1", 64)
	newSpec.Model = "embed-v2"
	newGeneration, created, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), newSpec)
	require.NoError(t, err)
	require.True(t, created)
	err = f.Store.ActivateDocumentVectorGeneration(t.Context(), newGeneration.ID, now.Add(2*time.Second))
	require.ErrorIs(t, err, store.ErrDocumentVectorCoverageIncomplete)
	stillActive, err := f.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(t, err)
	require.NotNil(t, stillActive)
	assert.Equal(t, oldGeneration.ID, stillActive.ID, "incomplete activation must not retire the serving generation")
	newClaims := readyAllDocumentVectorChunks(t, f, newGeneration, now.Add(2*time.Second))
	require.Len(t, newClaims, 1)
	revisionBeforeSwap, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	swapTime := now.Add(3 * time.Second)
	require.NoError(t, f.Store.ActivateDocumentVectorGeneration(t.Context(), newGeneration.ID, swapTime))

	oldAfter, err := f.Store.GetDocumentVectorGeneration(t.Context(), oldGeneration.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorGenerationRetired, oldAfter.State)
	require.NotNil(t, oldAfter.RetiredAt)
	assert.Equal(t, swapTime.UTC().Truncate(time.Millisecond), oldAfter.RetiredAt.UTC())
	newAfter, err := f.Store.GetDocumentVectorGeneration(t.Context(), newGeneration.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorGenerationActive, newAfter.State)
	revisionAfterSwap, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	assert.Equal(t, revisionBeforeSwap+1, revisionAfterSwap)

	resolved, err := f.Store.ResolveLiveDocumentVectorPublications(t.Context(), oldGeneration.ID, []string{oldClaims[0].Token})
	require.NoError(t, err)
	assert.Empty(t, resolved)
	resolved, err = f.Store.ResolveLiveDocumentVectorPublications(t.Context(), newGeneration.ID, []string{newClaims[0].Token})
	require.NoError(t, err)
	require.Len(t, resolved, 1)

	revisionBeforeRetire, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	retired, err := f.Store.RetireDocumentVectorGeneration(t.Context(), newGeneration.ID, now.Add(4*time.Second))
	require.NoError(t, err)
	assert.True(t, retired)
	revisionAfterRetire, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	assert.Equal(t, revisionBeforeRetire+1, revisionAfterRetire)
	retired, err = f.Store.RetireDocumentVectorGeneration(t.Context(), newGeneration.ID, now.Add(5*time.Second))
	require.NoError(t, err)
	assert.False(t, retired)
	revisionAfterRepeat, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	assert.Equal(t, revisionAfterRetire, revisionAfterRepeat)
	resolved, err = f.Store.ResolveLiveDocumentVectorPublications(t.Context(), newGeneration.ID, []string{newClaims[0].Token})
	require.NoError(t, err)
	assert.Empty(t, resolved)
	_, err = f.Store.GetDocumentVectorCoverage(t.Context(), newGeneration.ID)
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)
}

func testDocumentVectorLiveResolution(t *testing.T) {
	t.Run("input ordering dedupe and bounds", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f, generation := seedDocumentVectorGenerationWithChunks(t, 2)
		now := time.Date(2026, time.August, 20, 20, 21, 22, 0, time.UTC)
		claims := readyAllDocumentVectorChunks(t, f, generation, now)
		require.Len(claims, 2)
		require.NoError(f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))
		resolved, err := f.Store.ResolveLiveDocumentVectorPublications(t.Context(), generation.ID, []string{strings.Repeat("8", 64), claims[1].Token, claims[0].Token, claims[1].Token})
		require.NoError(err)
		require.Len(resolved, 2)
		assert.Equal(claims[1].Token, resolved[0].Token)
		assert.Equal(claims[0].Token, resolved[1].Token)
		assert.Equal(claims[1].DocumentVectorChunkCandidate, resolved[0].DocumentVectorChunkCandidate)
		_, err = f.Store.ResolveLiveDocumentVectorPublications(t.Context(), generation.ID, []string{"invalid"})
		require.Error(err)
		_, err = f.Store.ResolveLiveDocumentVectorPublications(t.Context(), generation.ID, make([]string, 1001))
		require.Error(err)
	})

	for _, mutation := range []string{"attachment replacement", "occurrence deletion", "role change", "message deletion", "target rotation"} {
		t.Run(mutation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
			now := time.Date(2026, time.August, 20, 21, 22, 23, 0, time.UTC)
			claims := readyAllDocumentVectorChunks(t, f, generation, now)
			require.Len(claims, 1)
			require.NoError(f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))
			attachmentID := documentVectorAttachmentID(t, f, claims[0].CanonicalBlobHash)
			file, err := f.Store.GetFileMetadata(t.Context(), attachmentID)
			require.NoError(err)
			require.NotNil(file)

			switch mutation {
			case "attachment replacement":
				newHash := strings.Repeat("c", 64)
				require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), file.MessageID, store.AttachmentWrite{
					Filename: file.Filename, MIMEType: file.MimeType, Size: file.Size,
					StoragePath: newHash[:2] + "/" + newHash, ContentHash: newHash,
					Role: file.AttachmentRole, RoleSource: file.RoleSource, SourcePartKey: file.SourcePartKey,
				}))
			case "occurrence deletion":
				_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM document_occurrences WHERE attachment_id = ?`), attachmentID)
				require.NoError(err)
			case "role change":
				require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), file.MessageID, store.AttachmentWrite{
					Filename: file.Filename, MIMEType: file.MimeType, Size: file.Size,
					StoragePath: file.StoragePath, ContentHash: file.ContentHash,
					Role: store.AttachmentRoleInline, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
					SourcePartKey: file.SourcePartKey,
				}))
			case "message deletion":
				require.NoError(f.Store.MarkMessageDeleted(file.SourceID, file.SourceMessageID))
			case "target rotation":
				profile := rotatedDocumentVectorProfile()
				_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
				require.NoError(err)
				require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
					ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
					RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
				}))
			}

			resolved, err := f.Store.ResolveLiveDocumentVectorPublications(t.Context(), generation.ID, []string{claims[0].Token})
			require.NoError(err)
			assert.Empty(resolved)
			var state string
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT state FROM document_vector_publications WHERE generation_id = ? AND token = ?`), generation.ID, claims[0].Token).Scan(&state))
			assert.Equal("ready", state, "source invalidation must hide without deleting the ledger row")
		})
	}
}

func testDocumentVectorCleanupAndPurge(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 3)
	now := time.Date(2026, time.August, 20, 22, 23, 24, 654321000, time.FixedZone("cleanup-offset", 4*60*60))
	ready, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "cleanup-worker", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, ready)
	require.NoError(t, f.Store.CommitDocumentVectorPublication(t.Context(), generation.ID, ready.Token, ready.LeaseOwner, ready.LeaseFence, now.Add(time.Second)))
	pending, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, ready.ChunkID, 1, "cleanup-worker", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, pending)
	failed, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, pending.ChunkID, 1, "cleanup-worker", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, failed)
	require.NoError(t, f.Store.FailDocumentVectorChunk(t.Context(), generation.ID, failed.Token, failed.LeaseOwner, failed.LeaseFence, now.Add(time.Second), nil, true, "provider_rejected"))

	_, err = f.Store.ListDocumentVectorCleanupTokens(t.Context(), generation.ID, "", 2)
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)
	changed, err := f.Store.MarkDocumentVectorTokenCleaned(t.Context(), generation.ID, ready.Token, now)
	assert.False(t, changed)
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)
	_, err = f.Store.PurgeRetiredDocumentVectorGeneration(t.Context(), generation.ID)
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)
	revisionBefore, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	retired, err := f.Store.RetireDocumentVectorGeneration(t.Context(), generation.ID, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.True(t, retired)
	retiredGeneration, err := f.Store.GetDocumentVectorGeneration(t.Context(), generation.ID)
	require.NoError(t, err)
	require.NotNil(t, retiredGeneration.RetiredAt)
	assert.Equal(t, now.Add(2*time.Second).UTC().Truncate(time.Millisecond), retiredGeneration.RetiredAt.UTC())
	revisionAfter, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(t, err)
	assert.Equal(t, revisionBefore, revisionAfter, "retiring a building generation need not invalidate search")

	wantTokens := []string{ready.Token, pending.Token, failed.Token}
	sort.Strings(wantTokens)
	first, err := f.Store.ListDocumentVectorCleanupTokens(t.Context(), generation.ID, "", 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	gotTokens := []string{first[0].Token, first[1].Token}
	assert.Equal(t, wantTokens[:2], gotTokens)
	second, err := f.Store.ListDocumentVectorCleanupTokens(t.Context(), generation.ID, first[1].Token, 2)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, wantTokens[2], second[0].Token)
	_, err = f.Store.ListDocumentVectorCleanupTokens(t.Context(), generation.ID, "", 0)
	require.ErrorContains(t, err, "limit")
	_, err = f.Store.ListDocumentVectorCleanupTokens(t.Context(), generation.ID, "", 1001)
	require.ErrorContains(t, err, "limit")
	_, err = f.Store.ListDocumentVectorCleanupTokens(t.Context(), generation.ID, "not-a-token", 1)
	require.ErrorContains(t, err, "cursor")

	_, err = f.Store.PurgeRetiredDocumentVectorGeneration(t.Context(), generation.ID)
	require.ErrorIs(t, err, store.ErrDocumentVectorCleanupIncomplete)
	var generationCount, publicationCount int
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM document_vector_generations WHERE id = ?`), generation.ID).Scan(&generationCount))
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM document_vector_publications WHERE generation_id = ?`), generation.ID).Scan(&publicationCount))
	assert.Equal(t, 1, generationCount)
	assert.Equal(t, 3, publicationCount)

	cleanTime := now.Add(3 * time.Second)
	for _, token := range wantTokens {
		changed, err := f.Store.MarkDocumentVectorTokenCleaned(t.Context(), generation.ID, token, cleanTime)
		require.NoError(t, err)
		assert.True(t, changed)
	}
	var cleanedAt time.Time
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT backend_cleaned_at FROM document_vector_publications WHERE generation_id = ? AND token = ?`), generation.ID, wantTokens[0]).Scan(&cleanedAt))
	assert.Equal(t, cleanTime.UTC().Truncate(time.Millisecond), cleanedAt.UTC())
	changed, err = f.Store.MarkDocumentVectorTokenCleaned(t.Context(), generation.ID, wantTokens[0], cleanTime.Add(time.Second))
	require.NoError(t, err)
	assert.False(t, changed)
	changed, err = f.Store.MarkDocumentVectorTokenCleaned(t.Context(), generation.ID, strings.Repeat("9", 64), cleanTime)
	require.NoError(t, err)
	assert.False(t, changed)
	remaining, err := f.Store.ListDocumentVectorCleanupTokens(t.Context(), generation.ID, "", 10)
	require.NoError(t, err)
	assert.Empty(t, remaining)

	purged, err := f.Store.PurgeRetiredDocumentVectorGeneration(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.True(t, purged)
	purged, err = f.Store.PurgeRetiredDocumentVectorGeneration(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.False(t, purged)
}

func runDocumentVectorChunkLifecycleContract(t *testing.T) {
	t.Helper()
	t.Run("claim resume bounds and stable token", testDocumentVectorClaimResumeBounds)
	t.Run("renew and stale fence", testDocumentVectorRenewAndStaleFence)
	t.Run("retry and terminal failure", testDocumentVectorFailureLifecycle)
	t.Run("failure status and terminal reset", testDocumentVectorFailureStatusAndReset)
	t.Run("source changed failure stays parked", testDocumentVectorSourceChangedFailureStaysParked)
	t.Run("commit idempotence and source race", testDocumentVectorCommitLifecycle)
	t.Run("invalid generation state", testDocumentVectorInvalidGenerationState)
	t.Run("timestamp precision parity", testDocumentVectorTimestampPrecisionParity)
	t.Run("ineffective sub-millisecond boundaries", testDocumentVectorRejectsIneffectiveTimeBoundaries)
}

func testDocumentVectorFailureStatusAndReset(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 2)
	now := time.Date(2026, time.August, 20, 12, 30, 0, 456789000, time.FixedZone("reset-offset", 2*60*60))
	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.NoError(t, f.Store.FailDocumentVectorChunk(
		t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence,
		now.Add(time.Second), nil, true, "provider_rejected",
	))
	retryable, err := f.Store.ClaimDocumentVectorChunk(
		t.Context(), generation.ID, claim.ChunkID, 1, "worker-a", now, time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, retryable)
	retryAt := now.Add(time.Hour).UTC().Truncate(time.Millisecond)
	require.NoError(t, f.Store.FailDocumentVectorChunk(
		t.Context(), generation.ID, retryable.Token, retryable.LeaseOwner, retryable.LeaseFence,
		now.Add(time.Second), &retryAt, false, "provider_busy",
	))
	firstPage, err := f.Store.GetDocumentVectorGenerationStatus(t.Context(), generation.ID, "", 1)
	require.NoError(t, err)
	require.Len(t, firstPage.Failures, 1)
	assert.False(t, firstPage.FailuresExhausted)
	assert.Equal(t, generation.ID, firstPage.FailureAfterGenerationID)
	assert.NotEmpty(t, firstPage.FailureAfterToken)
	secondPage, err := f.Store.GetDocumentVectorGenerationStatus(
		t.Context(), generation.ID, firstPage.FailureAfterToken, 1,
	)
	require.NoError(t, err)
	require.Len(t, secondPage.Failures, 1)
	assert.False(t, secondPage.FailuresExhausted)
	lastPage, err := f.Store.GetDocumentVectorGenerationStatus(
		t.Context(), generation.ID, secondPage.FailureAfterToken, 1,
	)
	require.NoError(t, err)
	assert.Empty(t, lastPage.Failures)
	assert.True(t, lastPage.FailuresExhausted)
	assert.Zero(t, lastPage.FailureAfterGenerationID)
	assert.Empty(t, lastPage.FailureAfterToken)

	status, err := f.Store.GetDocumentVectorGenerationStatus(t.Context(), generation.ID, "", 10)
	require.NoError(t, err)
	assert.True(t, status.Blocked)
	assert.Equal(t, int64(1), status.Terminal)
	assert.Zero(t, status.Pending)
	assert.Equal(t, int64(1), status.Retryable)
	assert.Zero(t, status.ReadyLive)
	assert.Zero(t, status.Obsolete)
	assert.Zero(t, status.CleanupPending)
	require.Len(t, status.Failures, 2)
	diagnostics := make(map[string]store.DocumentVectorFailureDiagnostic, len(status.Failures))
	for _, diagnostic := range status.Failures {
		diagnostics[diagnostic.Token] = diagnostic
	}
	assert.Equal(t, store.DocumentVectorFailureDiagnostic{
		Token: claim.Token, AttemptCount: 1, Terminal: true, ErrorCode: "provider_rejected",
	}, diagnostics[claim.Token])
	assert.Equal(t, store.DocumentVectorFailureDiagnostic{
		Token: retryable.Token, AttemptCount: 1, NextRetryAt: &retryAt, ErrorCode: "provider_busy",
	}, diagnostics[retryable.Token])
	assert.True(t, status.FailuresExhausted)
	assert.Empty(t, status.FailureAfterToken)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE document_vector_publications SET error_code = ?
		WHERE generation_id = ? AND token = ?`), "Provider returned private response text", generation.ID, retryable.Token)
	require.NoError(t, err)
	sanitized, err := f.Store.GetDocumentVectorGenerationStatus(t.Context(), generation.ID, "", 10)
	require.NoError(t, err)
	for _, diagnostic := range sanitized.Failures {
		if diagnostic.Token == retryable.Token {
			assert.Equal(t, "unknown", diagnostic.ErrorCode)
		}
	}

	reset, err := f.Store.ResetDocumentVectorFailures(t.Context(), generation.ID, "", 1, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, reset.Scanned)
	assert.Equal(t, 1, reset.Reset)
	assert.False(t, reset.Exhausted)
	assert.Equal(t, generation.ID, reset.AfterGenerationID)
	assert.NotEmpty(t, reset.AfterToken)
	secondReset, err := f.Store.ResetDocumentVectorFailures(
		t.Context(), generation.ID, reset.AfterToken, 1, now.Add(2*time.Second),
	)
	require.NoError(t, err)
	assert.Equal(t, 1, secondReset.Scanned)
	assert.Equal(t, 1, secondReset.Reset)
	assert.False(t, secondReset.Exhausted)
	finalReset, err := f.Store.ResetDocumentVectorFailures(
		t.Context(), generation.ID, secondReset.AfterToken, 1, now.Add(2*time.Second),
	)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorFailureResetResult{Exhausted: true}, finalReset)
	resetStatus, err := f.Store.GetDocumentVectorGenerationStatus(t.Context(), generation.ID, "", 10)
	require.NoError(t, err)
	assert.False(t, resetStatus.Blocked)

	reclaimed, err := f.Store.ClaimDocumentVectorChunk(
		t.Context(), generation.ID, 0, 1, "worker-b", now.Add(3*time.Second), time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	assert.Equal(t, claim.Token, reclaimed.Token)
	assert.Equal(t, claim.LeaseFence+1, reclaimed.LeaseFence)
	assert.Equal(t, 1, reclaimed.AttemptCount)
	_, err = f.Store.GetDocumentVectorGenerationStatus(t.Context(), generation.ID, "", 0)
	require.ErrorContains(t, err, "limit")
	_, err = f.Store.GetDocumentVectorGenerationStatus(t.Context(), generation.ID, "not-a-token", 1)
	require.ErrorContains(t, err, "cursor")
	_, err = f.Store.ResetDocumentVectorFailures(t.Context(), generation.ID, "", 1001, now)
	require.ErrorContains(t, err, "limit")
}

func testDocumentVectorSourceChangedFailureStaysParked(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 12, 45, 0, 0, time.UTC)
	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`DELETE FROM document_occurrences WHERE canonical_blob_hash = ?`), claim.CanonicalBlobHash)
	require.NoError(t, err)
	err = f.Store.CommitDocumentVectorPublication(
		t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now.Add(time.Second),
	)
	require.ErrorIs(t, err, store.ErrDocumentVectorSourceChanged)

	reset, err := f.Store.ResetDocumentVectorFailures(t.Context(), generation.ID, "", 10, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorFailureResetResult{Scanned: 1, Exhausted: true}, reset)
	status, err := f.Store.GetDocumentVectorGenerationStatus(t.Context(), generation.ID, "", 10)
	require.NoError(t, err)
	assert.Zero(t, status.Terminal, "obsolete failures are counted in Obsolete instead")
	assert.Equal(t, int64(1), status.Obsolete)
	assert.Equal(t, int64(1), status.CleanupPending)
	require.Len(t, status.Failures, 1)
	assert.Equal(t, "source_changed", status.Failures[0].ErrorCode)
}

func testDocumentVectorClaimResumeBounds(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 3)
	now := time.Date(2026, time.August, 20, 10, 11, 12, 0, time.UTC)

	first, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, 2*time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, int64(1), first.LeaseFence)
	assert.Equal(t, 1, first.AttemptCount)
	assert.Len(t, first.Token, 64)
	assert.Equal(t, now.Add(2*time.Minute), first.LeaseUntil)

	observed, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now.Add(time.Second), 2*time.Minute)
	require.NoError(t, err)
	require.NotNil(t, observed)
	assert.Equal(t, first.Token, observed.Token)
	assert.Equal(t, first.LeaseFence, observed.LeaseFence)
	assert.Equal(t, first.AttemptCount, observed.AttemptCount)
	assert.Equal(t, first.LeaseUntil, observed.LeaseUntil)

	blocked, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-b", now.Add(time.Second), 2*time.Minute)
	require.NoError(t, err)
	assert.Nil(t, blocked, "bounded scan must not jump past its first busy candidate")

	second, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 2, "worker-b", now.Add(time.Second), 2*time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Greater(t, second.ChunkID, first.ChunkID)

	takeover, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-b", first.LeaseUntil, 2*time.Minute)
	require.NoError(t, err)
	require.NotNil(t, takeover)
	assert.Equal(t, first.Token, takeover.Token)
	assert.Equal(t, first.LeaseFence+1, takeover.LeaseFence)
	assert.Equal(t, first.AttemptCount+1, takeover.AttemptCount)
}

func testDocumentVectorRenewAndStaleFence(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 11, 12, 13, 0, time.UTC)
	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)

	renewedUntil, err := f.Store.RenewDocumentVectorChunkClaim(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now.Add(30*time.Second), 3*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, now.Add(3*time.Minute+30*time.Second), renewedUntil)

	takeover, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", renewedUntil, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, takeover)
	_, err = f.Store.RenewDocumentVectorChunkClaim(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, renewedUntil, time.Minute)
	require.ErrorIs(t, err, store.ErrDocumentVectorClaimLost)
	assert.NotEqual(t, claim.LeaseFence, takeover.LeaseFence)
}

func testDocumentVectorFailureLifecycle(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 2)
	now := time.Date(2026, time.August, 20, 12, 13, 14, 0, time.UTC)
	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	retryAt := now.Add(5 * time.Minute)
	require.NoError(t, f.Store.FailDocumentVectorChunk(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now.Add(time.Second), &retryAt, false, "provider_busy"))

	beforeRetry, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-b", retryAt.Add(-time.Second), time.Minute)
	require.NoError(t, err)
	assert.Nil(t, beforeRetry)
	retry, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-b", retryAt, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, retry)
	assert.Equal(t, claim.Token, retry.Token)
	assert.Equal(t, claim.LeaseFence+1, retry.LeaseFence)
	assert.Equal(t, claim.AttemptCount+1, retry.AttemptCount)

	require.Error(t, f.Store.FailDocumentVectorChunk(t.Context(), generation.ID, retry.Token, retry.LeaseOwner, retry.LeaseFence, retryAt, nil, false, "provider_busy"))
	require.Error(t, f.Store.FailDocumentVectorChunk(t.Context(), generation.ID, retry.Token, retry.LeaseOwner, retry.LeaseFence, retryAt, &retryAt, true, "provider_busy"))
	require.Error(t, f.Store.FailDocumentVectorChunk(t.Context(), generation.ID, retry.Token, retry.LeaseOwner, retry.LeaseFence, retryAt, nil, true, strings.Repeat("x", 65)))
	require.NoError(t, f.Store.FailDocumentVectorChunk(t.Context(), generation.ID, retry.Token, retry.LeaseOwner, retry.LeaseFence, retryAt, nil, true, "provider_rejected"))

	terminal, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-c", retryAt.Add(time.Hour), time.Minute)
	require.NoError(t, err)
	assert.Nil(t, terminal)
}

func testDocumentVectorCommitLifecycle(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 2)
	now := time.Date(2026, time.August, 20, 13, 14, 15, 0, time.UTC)
	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.NoError(t, f.Store.CommitDocumentVectorPublication(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now.Add(time.Second)))
	require.NoError(t, f.Store.CommitDocumentVectorPublication(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now.Add(2*time.Second)))
	err = f.Store.CommitDocumentVectorPublication(t.Context(), generation.ID, claim.Token, "stale-worker", claim.LeaseFence, now.Add(2*time.Second))
	require.ErrorIs(t, err, store.ErrDocumentVectorClaimLost)
	err = f.Store.CommitDocumentVectorPublication(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence+1, now.Add(2*time.Second))
	require.ErrorIs(t, err, store.ErrDocumentVectorClaimLost)

	tracing, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, claim.ChunkID, 1, "worker-b", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, tracing)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM document_occurrences WHERE canonical_blob_hash = ?`), tracing.CanonicalBlobHash)
	require.NoError(t, err)
	err = f.Store.CommitDocumentVectorPublication(t.Context(), generation.ID, tracing.Token, tracing.LeaseOwner, tracing.LeaseFence, now.Add(time.Second))
	require.ErrorIs(t, err, store.ErrDocumentVectorSourceChanged)

	var state, errorCode string
	var owner, leaseUntil, cleanedAt sql.NullString
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT state, error_code, lease_owner, lease_until, backend_cleaned_at FROM document_vector_publications WHERE generation_id = ? AND token = ?`), generation.ID, tracing.Token).Scan(&state, &errorCode, &owner, &leaseUntil, &cleanedAt))
	assert.Equal(t, "failed", state)
	assert.Equal(t, "source_changed", errorCode)
	assert.False(t, owner.Valid)
	assert.False(t, leaseUntil.Valid)
	assert.False(t, cleanedAt.Valid)
}

func testDocumentVectorInvalidGenerationState(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 14, 15, 16, 0, time.UTC)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE document_vector_generations SET state = 'active' WHERE id = ?`), generation.ID)
	require.NoError(t, err)
	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, time.Minute)
	assert.Nil(t, claim)
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)

	_, err = f.Store.RenewDocumentVectorChunkClaim(t.Context(), generation.ID, strings.Repeat("0", 64), "worker-a", 1, now, time.Minute)
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)

	f, generation = seedDocumentVectorGenerationWithChunks(t, 1)
	_, err = f.Store.DB().Exec(`UPDATE document_index_state SET target_profile_id = NULL WHERE singleton = 1`)
	require.NoError(t, err)
	claim, err = f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, time.Minute)
	assert.Nil(t, claim)
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)
}

func testDocumentVectorTimestampPrecisionParity(t *testing.T) {
	t.Run("claim", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
		rawNow := time.Date(2026, time.August, 20, 15, 16, 17, 123456789, time.FixedZone("test-offset", 2*60*60))
		leaseDuration := 2*time.Second + 400*time.Microsecond
		wantUntil := rawNow.Add(leaseDuration).UTC().Truncate(time.Millisecond)

		claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", rawNow, leaseDuration)
		require.NoError(err)
		require.NotNil(claim)
		assert.Equal(wantUntil, claim.LeaseUntil)
		assert.Equal(wantUntil, documentVectorLeaseUntil(t, f, generation.ID, claim.Token))

		before, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-b", wantUntil.Add(-time.Nanosecond), time.Second)
		require.NoError(err)
		assert.Nil(before)
		atDeadline, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-b", wantUntil, time.Second)
		require.NoError(err)
		require.NotNil(atDeadline)
		assert.Equal(claim.Token, atDeadline.Token)
		assert.Equal(claim.LeaseFence+1, atDeadline.LeaseFence)
	})

	t.Run("renew", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
		claimNow := time.Date(2026, time.August, 20, 16, 17, 18, 0, time.UTC)
		claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", claimNow, time.Minute)
		require.NoError(err)
		require.NotNil(claim)

		rawNow := claimNow.Add(5*time.Second + 456789*time.Nanosecond)
		leaseDuration := 2*time.Second + 400*time.Microsecond
		wantUntil := rawNow.Add(leaseDuration).UTC().Truncate(time.Millisecond)
		renewedUntil, err := f.Store.RenewDocumentVectorChunkClaim(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, rawNow, leaseDuration)
		require.NoError(err)
		assert.Equal(wantUntil, renewedUntil)
		assert.Equal(wantUntil, documentVectorLeaseUntil(t, f, generation.ID, claim.Token))

		before, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-b", wantUntil.Add(-time.Nanosecond), time.Second)
		require.NoError(err)
		assert.Nil(before)
		atDeadline, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-b", wantUntil, time.Second)
		require.NoError(err)
		require.NotNil(atDeadline)
		assert.Equal(claim.Token, atDeadline.Token)
	})
}

func testDocumentVectorRejectsIneffectiveTimeBoundaries(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	now := time.Date(2026, time.August, 20, 17, 18, 19, 123000000, time.UTC)

	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, 500*time.Microsecond)
	assert.Nil(t, claim)
	require.ErrorContains(t, err, "lease duration")

	claim, err = f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "worker-a", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	_, err = f.Store.RenewDocumentVectorChunkClaim(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now, 500*time.Microsecond)
	require.ErrorContains(t, err, "lease duration")

	retryAt := now.Add(500 * time.Microsecond)
	err = f.Store.FailDocumentVectorChunk(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now, &retryAt, false, "provider_busy")
	require.ErrorContains(t, err, "future retry time")
}

func documentVectorLeaseUntil(t *testing.T, f *storetest.Fixture, generationID int64, token string) time.Time {
	t.Helper()
	var leaseUntil time.Time
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT lease_until FROM document_vector_publications WHERE generation_id = ? AND token = ?`), generationID, token).Scan(&leaseUntil))
	return leaseUntil.UTC()
}

func readyAllDocumentVectorChunks(
	t *testing.T,
	f *storetest.Fixture,
	generation store.DocumentVectorGeneration,
	now time.Time,
) []store.DocumentVectorChunkClaim {
	t.Helper()
	var claims []store.DocumentVectorChunkClaim
	for len(claims) < 1000 {
		claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1000, "ready-worker", now, time.Minute)
		require.NoError(t, err)
		if claim == nil {
			return claims
		}
		require.NoError(t, f.Store.CommitDocumentVectorPublication(t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now.Add(time.Second)))
		claims = append(claims, *claim)
	}
	require.Fail(t, "document vector ready helper exceeded its safety bound")
	return nil
}

func documentVectorAttachmentID(t *testing.T, f *storetest.Fixture, hash string) int64 {
	t.Helper()
	var attachmentID int64
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM attachments WHERE content_hash = ?`), hash).Scan(&attachmentID))
	return attachmentID
}

func rotatedDocumentVectorProfile() store.DocumentExtractionProfile {
	fingerprint := strings.Repeat("2", 64)
	return store.DocumentExtractionProfile{
		ID: "profile-" + fingerprint, Fingerprint: fingerprint,
		Provider: "mistral", Endpoint: "https://api.mistral.ai/v1/ocr",
		Region: "eu", Model: "mistral-ocr-5-0",
		RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"application/pdf"}, PolicyJSON: []byte(`{"policy":2}`),
	}
}

func seedDocumentVectorGenerationWithChunks(t *testing.T, chunkCount int) (*storetest.Fixture, store.DocumentVectorGeneration) {
	t.Helper()
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishManySearchChunks(t, f, profile, hash, chunkCount)
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("f", 64), TargetExtractionProfileID: profile.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768,
	})
	require.NoError(t, err)
	return f, generation
}

func TestDocumentVectorGenerationCreateResumeCollisionAndBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, _ := seedDocumentPublicationAuthority(t, f)
	spec := store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("1", 64), TargetExtractionProfileID: profile.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768,
	}

	first, created, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(err)
	assert.True(created)
	assert.Equal(store.DocumentVectorGenerationBuilding, first.State)
	assert.NotZero(first.ID)

	second, created, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(err)
	assert.False(created)
	assert.Equal(first.ID, second.ID)

	collision := spec
	collision.Model = "embed-v2"
	_, _, err = f.Store.EnsureDocumentVectorGeneration(t.Context(), collision)
	require.ErrorContains(err, "fingerprint")

	other := spec
	other.Fingerprint = strings.Repeat("2", 64)
	_, _, err = f.Store.EnsureDocumentVectorGeneration(t.Context(), other)
	require.ErrorContains(err, "building")

	invalid := spec
	invalid.Fingerprint = strings.Repeat("3", 64)
	invalid.Dimension = 0
	_, _, err = f.Store.EnsureDocumentVectorGeneration(t.Context(), invalid)
	require.ErrorContains(err, "dimension")
}

func TestDocumentVectorGenerationRejectsNonCurrentTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	current, _ := seedDocumentPublicationAuthority(t, f)
	nonCurrent := rotatedDocumentVectorProfile()
	_, err := f.Store.EnsureDocumentExtractionProfile(t.Context(), nonCurrent)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: nonCurrent.ID, ProfileFingerprint: nonCurrent.Fingerprint,
		RetentionPosture: nonCurrent.RetentionPosture, TrainingPosture: nonCurrent.TrainingPosture,
	}))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE document_index_state SET target_profile_id = ? WHERE singleton = 1`), current.ID)
	require.NoError(err)

	_, _, err = f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("3", 64), TargetExtractionProfileID: nonCurrent.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768,
	})
	require.ErrorIs(err, store.ErrDocumentVectorInvalidGenerationState)
	var rows int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*) FROM document_vector_generations WHERE target_extraction_profile_id = ?`),
		nonCurrent.ID).Scan(&rows))
	assert.Zero(rows)

	generation, created, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("4", 64), TargetExtractionProfileID: current.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768,
	})
	require.NoError(err)
	assert.True(created)
	assert.Equal(current.ID, generation.TargetExtractionProfileID)
}

func TestDocumentVectorBaseSchemaPreservesCleanupAuthority(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		rows, err := f.Store.DB().Query(`
			SELECT confrelid::regclass::text
			FROM pg_constraint
			WHERE conrelid = 'document_vector_publications'::regclass
			  AND contype = 'f'
			ORDER BY confrelid::regclass::text`)
		require.NoError(err)
		defer func() { _ = rows.Close() }()
		var references []string
		for rows.Next() {
			var table string
			require.NoError(rows.Scan(&table))
			references = append(references, table)
		}
		require.NoError(rows.Err())
		assert.Equal([]string{"document_vector_generations"}, references)
		var definition string
		require.NoError(f.Store.DB().QueryRow(`
			SELECT pg_get_indexdef('idx_document_vector_publications_cleanup'::regclass)`).Scan(&definition))
		assert.Contains(definition, "(generation_id, backend_cleaned_at, token)")
		return
	}

	rows, err := f.Store.DB().Query(`PRAGMA foreign_key_list('document_vector_publications')`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	references := map[string]struct{}{}
	for rows.Next() {
		var id, sequence int
		var table, from, to, onUpdate, onDelete, match string
		require.NoError(rows.Scan(&id, &sequence, &table, &from, &to, &onUpdate, &onDelete, &match))
		references[table] = struct{}{}
	}
	require.NoError(rows.Err())
	assert.Equal(map[string]struct{}{"document_vector_generations": {}}, references)
	indexRows, err := f.Store.DB().Query(`PRAGMA index_info('idx_document_vector_publications_cleanup')`)
	require.NoError(err)
	defer func() { _ = indexRows.Close() }()
	var columns []string
	for indexRows.Next() {
		var sequence, columnID int
		var column string
		require.NoError(indexRows.Scan(&sequence, &columnID, &column))
		columns = append(columns, column)
	}
	require.NoError(indexRows.Err())
	assert.Equal([]string{"generation_id", "backend_cleaned_at", "token"}, columns)
}

func TestDocumentVectorGenerationRetirementAndActiveConstraints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, _ := seedDocumentPublicationAuthority(t, f)
	spec := store.DocumentVectorGenerationSpec{Fingerprint: strings.Repeat("4", 64), TargetExtractionProfileID: profile.ID, EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768}
	first, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE document_vector_generations SET state = 'retired', retired_at = CURRENT_TIMESTAMP WHERE id = ?`), first.ID)
	require.NoError(err)
	fresh, created, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(err)
	assert.True(created)
	assert.NotEqual(first.ID, fresh.ID)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE document_vector_generations SET state = 'active' WHERE id = ?`), fresh.ID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO document_vector_generations (fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, state) VALUES (?, ?, ?, ?, ?, 'active')`), strings.Repeat("5", 64), profile.ID, "vector.embeddings", "embed-v1", 768)
	require.Error(err)
}

func TestDocumentVectorGenerationRejectsHistoricalSpecCollision(t *testing.T) {
	f := storetest.New(t)
	profile, _ := seedDocumentPublicationAuthority(t, f)
	spec := store.DocumentVectorGenerationSpec{Fingerprint: strings.Repeat("c", 64), TargetExtractionProfileID: profile.ID, EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO document_vector_generations (fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, state, retired_at) VALUES (?, ?, ?, ?, ?, 'retired', CURRENT_TIMESTAMP)`), spec.Fingerprint, profile.ID, spec.EmbeddingProfile, "embed-old", spec.Dimension)
	require.NoError(t, err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO document_vector_generations (fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, state) VALUES (?, ?, ?, ?, ?, 'building')`), spec.Fingerprint, profile.ID, spec.EmbeddingProfile, spec.Model, spec.Dimension)
	require.NoError(t, err)
	_, _, err = f.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.ErrorContains(t, err, "collides")
}

func TestDocumentVectorGenerationRejectsInvalidFingerprintAndProfile(t *testing.T) {
	f := storetest.New(t)
	profile, _ := seedDocumentPublicationAuthority(t, f)
	for _, spec := range []store.DocumentVectorGenerationSpec{
		{Fingerprint: "not-a-digest", TargetExtractionProfileID: profile.ID, EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768},
		{Fingerprint: strings.Repeat("6", 64), TargetExtractionProfileID: profile.ID, EmbeddingProfile: "other.embeddings", Model: "embed-v1", Dimension: 768},
	} {
		_, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
		require.Error(t, err)
	}
}

func TestDocumentVectorTargetRejectsLegacyNormalizedIdentity(t *testing.T) {
	requirements := require.New(t)
	f, _ := seedDocumentVectorGenerationWithChunks(t, 1)
	var extractionID string
	requirements.NoError(f.Store.DB().QueryRow(`SELECT extraction_id FROM document_extraction_heads LIMIT 1`).Scan(&extractionID))
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE document_extractions
		SET normalization_version = NULL, document_family = NULL, unit_kind = NULL
		WHERE id = ?`), extractionID)
	requirements.NoError(err)

	_, err = f.Store.GetDocumentVectorTargetProfileID(t.Context())
	requirements.ErrorIs(err, store.ErrDocumentNormalizedIdentityUnavailable)
	requirements.ErrorContains(err, "documents build --full-rebuild")
	_, err = f.Store.LoadNormalizedDocument(t.Context(), extractionID)
	requirements.ErrorIs(err, store.ErrDocumentNormalizedIdentityUnavailable)
	requirements.ErrorContains(err, "documents build --full-rebuild")
}

func TestDocumentVectorTargetIgnoresDeadLegacyNormalizedIdentity(t *testing.T) {
	requirements := require.New(t)
	f, _ := seedDocumentVectorGenerationWithChunks(t, 1)
	var extractionID, profileID, hash string
	requirements.NoError(f.Store.DB().QueryRow(`
		SELECT extraction_id, profile_id, canonical_blob_hash
		FROM document_extraction_heads LIMIT 1`).Scan(&extractionID, &profileID, &hash))
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE document_extractions
		SET normalization_version = NULL, document_family = NULL, unit_kind = NULL
		WHERE id = ?`), extractionID)
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		DELETE FROM document_occurrences WHERE canonical_blob_hash = ?`), hash)
	requirements.NoError(err)

	target, err := f.Store.GetDocumentVectorTargetProfileID(t.Context())
	requirements.NoError(err)
	requirements.Equal(profileID, target)
}

func TestDocumentVectorChunkCandidatesUseCurrentLiveAuthority(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "vector candidate evidence", "vector-candidate")
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("7", 64), TargetExtractionProfileID: profile.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768,
	})
	require.NoError(err)

	candidates, err := f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(generation.ID, candidates[0].GenerationID)
	assert.Equal(profile.ID, candidates[0].ExtractionProfileID)
	assert.Equal(hash, candidates[0].CanonicalBlobHash)
	assert.Equal("vector candidate evidence", candidates[0].Text)
	assert.Equal("vector-candidate", candidates[0].ExtractionID)
	assert.NotEmpty(candidates[0].ChunkKey)
	assert.NotEmpty(candidates[0].ChunkChecksum)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM document_occurrences WHERE canonical_blob_hash = ?`), hash)
	require.NoError(err)
	candidates, err = f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 10)
	require.NoError(err)
	assert.Empty(candidates)
}

func TestDocumentVectorChunkCandidatesHideAfterTargetProfileRotation(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "rotation candidate evidence", "vector-rotation")
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{Fingerprint: strings.Repeat("8", 64), TargetExtractionProfileID: profile.ID, EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768})
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE document_index_state SET target_profile_id = ? WHERE singleton = 1`), "rotated-profile")
	require.NoError(err)
	candidates, err := f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 10)
	require.NoError(err)
	assert.Empty(t, candidates)
}

func TestDocumentVectorPublicationCommitRejectsInconsistentImmutableSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "integrity candidate evidence", "vector-integrity")
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{Fingerprint: strings.Repeat("9", 64), TargetExtractionProfileID: profile.ID, EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768})
	require.NoError(err)
	now := time.Date(2026, time.August, 20, 23, 0, 0, 0, time.UTC)
	claim, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "integrity-worker", now, time.Minute)
	require.NoError(err)
	require.NotNil(claim)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE document_vector_publications SET canonical_blob_hash = ?
		WHERE generation_id = ? AND token = ?`), strings.Repeat("f", 64), generation.ID, claim.Token)
	require.NoError(err)
	err = f.Store.CommitDocumentVectorPublication(
		t.Context(), generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, now.Add(time.Second))
	require.ErrorIs(err, store.ErrDocumentVectorSourceChanged)
	var state, errorCode string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT state, error_code FROM document_vector_publications
		WHERE generation_id = ? AND token = ?`), generation.ID, claim.Token).Scan(&state, &errorCode))
	assert.Equal("failed", state)
	assert.Equal("source_changed", errorCode)
}

func TestDocumentVectorGenerationCannotDeletePublicationBeforeBackendCleanup(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "retained token evidence", "vector-retained")
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{Fingerprint: strings.Repeat("d", 64), TargetExtractionProfileID: profile.ID, EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768})
	require.NoError(err)
	candidates, err := f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 1)
	require.NoError(err)
	require.Len(candidates, 1)
	candidate := candidates[0]
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO document_vector_publications (generation_id, extraction_id, extraction_profile_id, canonical_blob_hash, extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`), generation.ID, candidate.ExtractionID, candidate.ExtractionProfileID, candidate.CanonicalBlobHash, candidate.ExtractionInputKey, candidate.ChunkID, candidate.ChunkKey, candidate.ChunkChecksum, candidate.SourceSequence, "opaque-retained-token")
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM document_vector_generations WHERE id = ?`), generation.ID)
	require.Error(err)
}

func TestDocumentVectorChunkCandidatesRequireConfiguredExtractionProfile(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "fallback vector evidence", "vector-fallback")

	target := profile
	target.Fingerprint = strings.Repeat("e", 64)
	target.ID = "profile-" + target.Fingerprint
	target.Model = "mistral-ocr-5-0"
	target.PolicyJSON = []byte(`{"policy":2}`)
	_, err := f.Store.EnsureDocumentExtractionProfile(t.Context(), target)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: target.ID, ProfileFingerprint: target.Fingerprint,
		RetentionPosture: target.RetentionPosture, TrainingPosture: target.TrainingPosture,
	}))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE document_index_state SET target_profile_id = ? WHERE singleton = 1`), target.ID)
	require.NoError(err)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE document_vector_generations SET state = 'active' WHERE state = 'building'`))
	require.NoError(err)
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("a", 64), TargetExtractionProfileID: target.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768,
	})
	require.NoError(err)

	candidates, err := f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 10)
	require.NoError(err)
	assert.Empty(t, candidates, "an older fallback extraction does not satisfy the configured Docbank identity")

	publishSearchDocument(t, f, target, hash, "target vector evidence", "vector-target")
	candidates, err = f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(t, target.ID, candidates[0].ExtractionProfileID)
}

func TestDocumentVectorChunkCandidatesHaveStableBoundedPagination(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishManySearchChunks(t, f, profile, hash, 3)
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("b", 64), TargetExtractionProfileID: profile.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768,
	})
	require.NoError(err)

	first, err := f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 2)
	require.NoError(err)
	require.Len(first, 2)
	second, err := f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, first[1].ChunkID, 2)
	require.NoError(err)
	require.Len(second, 1)
	assert.Greater(t, second[0].ChunkID, first[1].ChunkID)

	_, err = f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 0)
	require.ErrorContains(err, "limit")
	_, err = f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 1001)
	require.ErrorContains(err, "limit")
	_, err = f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, -1, 1)
	require.ErrorContains(err, "after")
}
