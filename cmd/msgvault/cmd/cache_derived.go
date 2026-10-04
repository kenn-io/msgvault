package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// ErrDerivedRefreshRequiresFullBuild means the committed fact snapshot cannot
// safely support a derived-only refresh. Callers should run a full rebuild.
var ErrDerivedRefreshRequiresFullBuild = errors.New(
	"derived cache refresh requires a full rebuild",
)

var derivedPublishBeforeMarkerHook func() error

// derivedRefreshBeforeSnapshotHook is a deterministic test seam for writes
// that commit after a staleness check selects a refresh but before the refresh
// pins its source snapshot.
var derivedRefreshBeforeSnapshotHook func()

func refreshDerivedDatasetsOnly(
	ctx context.Context,
	dbPath, analyticsDir string,
	locking cachePublishLocking,
	repairRelated bool,
	builderOverrides ...duckdbutil.BuilderOverrides,
) (*buildResult, error) {
	readiness, err := query.InspectCacheReadiness(analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect committed cache: %w",
			ErrDerivedRefreshRequiresFullBuild, err)
	}
	if readiness != query.CacheReady {
		return nil, fmt.Errorf("%w: cache is %s",
			ErrDerivedRefreshRequiresFullBuild, readiness)
	}
	state, err := query.ReadCacheSyncState(analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("%w: read committed marker: %w",
			ErrDerivedRefreshRequiresFullBuild, err)
	}
	if state.SchemaVersion != query.CacheSchemaVersion {
		return nil, fmt.Errorf("%w: cache schema v%d, need v%d",
			ErrDerivedRefreshRequiresFullBuild,
			state.SchemaVersion,
			query.CacheSchemaVersion,
		)
	}

	if err := cleanupStaleCacheStaging(analyticsDir); err != nil {
		return nil, err
	}
	staging, err := newCacheStaging(analyticsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = staging.cleanup() }()

	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store for derived cache refresh: %w", err)
	}
	identityRevision, err := st.IdentityRevision()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read identity revision: %w", err)
	}
	derivedDataRevision, err := st.DerivedDataRevision()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read derived-data revision: %w", err)
	}
	if derivedDataRevision != state.DerivedDataRevision {
		relatedOnly, relatedErr := st.RelatedDerivedRevisionsOnly(ctx,
			state.DerivedDataRevision, derivedDataRevision)
		if relatedErr != nil {
			_ = st.Close()
			return nil, fmt.Errorf("classify derived-data revision: %w", relatedErr)
		}
		if !repairRelated || !relatedOnly {
			_ = st.Close()
			return nil, fmt.Errorf("%w: derived-data revision changed",
				ErrDerivedRefreshRequiresFullBuild)
		}
	}
	accountIdentityRevision, err := st.AccountIdentityRevision()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read account identity revision: %w", err)
	}
	if accountIdentityRevision != state.AccountIdentityRevision {
		_ = st.Close()
		return nil, fmt.Errorf("%w: account identity revision changed",
			ErrDerivedRefreshRequiresFullBuild)
	}
	participantIdentifierRevision, err := st.ParticipantIdentifierRevision()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read participant identifier revision: %w", err)
	}
	participantDisplayNameRevision, err := st.ParticipantDisplayNameRevision()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read participant display-name revision: %w", err)
	}
	personDisplayNameRevision, err := st.PersonDisplayNameRevision()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read person display-name revision: %w", err)
	}
	clusters, err := st.ParticipantClusters()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read participant clusters: %w", err)
	}
	if err := st.Close(); err != nil {
		return nil, fmt.Errorf("close identity store: %w", err)
	}

	spillDir := filepath.Join(staging.root, "duckdb-tmp")
	duckDB, err := duckdbutil.Open(ctx, duckdbutil.BuilderPolicyWithOverrides(
		spillDir,
		firstBuilderOverrides(builderOverrides),
	))
	if err != nil {
		return nil, fmt.Errorf("open bounded DuckDB for derived refresh: %w", err)
	}
	defer func() { _ = duckDB.Close() }()
	textRepairs := &cacheTextRepairs{}
	if err := registerCacheTextFunctions(ctx, duckDB, textRepairs); err != nil {
		return nil, err
	}

	if derivedRefreshBeforeSnapshotHook != nil {
		derivedRefreshBeforeSnapshotHook()
	}
	sourceSnapshot, err := openCacheSourceSnapshot(duckDB, dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sourceSnapshot.Close() }()
	datasets := []string{
		tableMessages,
		tableConversations,
		tableConversationParticipants,
		"account_identities",
		tableParticipants,
		tableParticipantIdentifiers,
		"persons", "person_participants",
	}
	var relatedChangeSeq int64
	var relatedKinds relatedChangeKinds
	if repairRelated {
		// A late terminal sync can add children to an already-exported parent.
		// Acknowledge its addition counter only from the same snapshot used to
		// repair those children. Changed facts or failed runs remain full repairs.
		counters, err := readCacheSyncCounters(sourceSnapshot)
		if err != nil {
			return nil, fmt.Errorf("read related-refresh sync counters: %w", err)
		}
		if counters.updates != state.LastCacheUpdateCount ||
			counters.failedRunCount != state.LastFailedSyncRunCount ||
			counters.failedRunIDSum != state.LastFailedSyncRunIDSum ||
			counters.additions < state.LastCacheAdditionCount {
			return nil, fmt.Errorf("%w: sync counters require a full repair",
				ErrDerivedRefreshRequiresFullBuild)
		}
		if counters.additions != state.LastCacheAdditionCount {
			if err := verifyRelatedOnlyAdditions(sourceSnapshot, state); err != nil {
				return nil, err
			}
		}
		state.LastCacheAdditionCount = counters.additions
		if err := sourceSnapshot.QueryRow(`SELECT COALESCE((SELECT seq FROM sqlite_sequence
			WHERE name = 'cache_related_change_journal'), 0)`).Scan(&relatedChangeSeq); err != nil {
			return nil, fmt.Errorf("read related-change boundary: %w", err)
		}
		if err := inspectRelatedSnapshotColumns(sourceSnapshot); err != nil {
			return nil, err
		}
		// The CSV fallback closes its SQLite transaction during preparation.
		// Read journal metadata while that snapshot is still available.
		relatedKinds, err = inspectRelatedChangeKinds(sourceSnapshot,
			state.LastRelatedChangeSeq, relatedChangeSeq, state.LastMessageID)
		if err != nil {
			return nil, err
		}
		if relatedKinds.other {
			return nil, fmt.Errorf("%w: unsupported related-row journal dataset",
				ErrDerivedRefreshRequiresFullBuild)
		}
	}
	relatedExports := make(map[string]int64)
	relatedReplacements := make(map[string]bool)
	for dataset, changed := range relatedKinds.datasets() {
		if changed {
			relatedExports[dataset] = 0
			datasets = append(datasets, dataset)
			relatedReplacements[dataset] = true
		}
	}
	if err := sourceSnapshot.PrepareDatasets(datasets...); err != nil {
		return nil, err
	}
	exportDB := sourceSnapshot.DuckDB()

	conversationFingerprint, err := fingerprintConversationParticipantsFromSnapshot(
		ctx,
		exportDB,
		state.LastMessageID,
	)
	if err != nil {
		return nil, err
	}
	typesFingerprint, err := fingerprintConversationTypesFromSnapshot(
		ctx,
		exportDB,
		state.LastMessageID,
	)
	if err != nil {
		return nil, err
	}

	if !repairRelated && identityRevision == state.IdentityRevision &&
		participantIdentifierRevision == state.ParticipantIdentifierRevision &&
		participantDisplayNameRevision == state.ParticipantDisplayNameRevision &&
		personDisplayNameRevision == state.PersonDisplayNameRevision &&
		conversationFingerprint == state.ConversationParticipantsFingerprint &&
		typesFingerprint == state.ConversationTypesFingerprint {
		// Nothing the derived datasets read has changed (the account-identity
		// revision was already verified equal above). Republishing would only
		// advance PublishedAt, invalidating readers' cache revision — and with
		// it active pagination cursors — for no analytical difference.
		return &buildResult{OutputDir: analyticsDir, IdentityOnly: true, Skipped: true}, nil
	}
	if repairRelated && !relatedKinds.recipients &&
		identityRevision == state.IdentityRevision &&
		participantIdentifierRevision == state.ParticipantIdentifierRevision &&
		participantDisplayNameRevision == state.ParticipantDisplayNameRevision &&
		personDisplayNameRevision == state.PersonDisplayNameRevision &&
		conversationFingerprint == state.ConversationParticipantsFingerprint &&
		typesFingerprint == state.ConversationTypesFingerprint {
		// Labels and attachment metadata do not enter relationship_activity.
		// Publish their child rows and marker directly, avoiding a scan of the
		// expanded relationship population for a small metadata correction.
		if err := exportRelatedDatasets(ctx, exportDB, sourceSnapshot,
			state.LastMessageID, staging.root, relatedExports); err != nil {
			return nil, err
		}
		if err := refreshRelatedCacheStats(ctx, exportDB, &state, relatedKinds); err != nil {
			return nil, err
		}
		if err := sourceSnapshot.Close(); err != nil {
			return nil, fmt.Errorf("close SQLite related-refresh snapshot: %w", err)
		}
		state.DerivedDataRevision = derivedDataRevision
		state.LastRelatedChangeSeq = relatedChangeSeq
		plan := cachePublishPlan{
			Append:  map[string]bool{},
			Replace: relatedReplacements,
		}
		if err := publishDerivedCache(staging, analyticsDir, plan, state, locking); err != nil {
			return nil, err
		}
		reportCacheTextRepairs(os.Stderr, textRepairs)
		warnRelatedChangePrune(dbPath, relatedChangeSeq, derivedDataRevision)
		return &buildResult{OutputDir: analyticsDir, IdentityOnly: true}, nil
	}

	derivedCopy := func(table, selectSQL string) error {
		return copyParquet(ctx, exportDB, filepath.Join(staging.root, table), table+".parquet", selectSQL)
	}
	if err := derivedCopy(tableOwnerParticipants, ownerParticipantsSelectSQL(
		sourceSnapshot.identityPresenceSQL("email_address", "primary_email_present"))); err != nil {
		return nil, fmt.Errorf("export derived owner participants: %w", err)
	}
	if err := stageParticipantClusters(ctx, exportDB, clusters); err != nil {
		return nil, err
	}
	if err := derivedCopy(tableParticipantClusters, participantClustersSelectSQL); err != nil {
		return nil, fmt.Errorf("export derived participant clusters: %w", err)
	}
	conversationChanged :=
		conversationFingerprint != state.ConversationParticipantsFingerprint
	if conversationChanged {
		if err := derivedCopy(tableConversationParticipants, conversationParticipantsSelectSQL(state.LastMessageID)); err != nil {
			return nil, fmt.Errorf("export derived conversation participants: %w", err)
		}
	}
	identifiersChanged :=
		participantIdentifierRevision != state.ParticipantIdentifierRevision
	if identifiersChanged {
		// The directory rebuild reads identifier values from the
		// participant_identifiers base dataset (relationship_people search
		// values and label fallbacks), so a changed mapping must be re-staged
		// and republished alongside the derived index.
		if err := derivedCopy(tableParticipantIdentifiers, sourceSnapshot.participantIdentifiersExportSelectSQL()); err != nil {
			return nil, fmt.Errorf("export derived participant identifiers: %w", err)
		}
	}
	displayNamesChanged :=
		participantDisplayNameRevision != state.ParticipantDisplayNameRevision
	if identifiersChanged || displayNamesChanged {
		// Participant identifiers can create participant rows, and display-name
		// mutations change the row already present in participants.parquet. Both
		// changes must replace that base dataset before rebuilding the directory.
		if err := derivedCopy(tableParticipants, sourceSnapshot.participantsExportSelectSQL()); err != nil {
			return nil, fmt.Errorf("export derived participants: %w", err)
		}
	}
	personDisplayNamesChanged := personDisplayNameRevision != state.PersonDisplayNameRevision
	identityChanged := identityRevision != state.IdentityRevision
	if personDisplayNamesChanged || identityChanged {
		if err := derivedCopy(tablePersonDisplayNames, sourceSnapshot.personDisplayNamesExportSelectSQL()); err != nil {
			return nil, fmt.Errorf("export derived person_display_names: %w", err)
		}
	}
	typesChanged := typesFingerprint != state.ConversationTypesFingerprint
	if typesChanged {
		// The index rebuild reads conversation_type from the conversations
		// base dataset, and the analytical view joins it live — both must
		// see the current types, so the dataset is re-staged and republished
		// alongside the derived index.
		if err := derivedCopy(tableConversations, sourceSnapshot.conversationsExportSelectSQL(state.LastMessageID)); err != nil {
			return nil, fmt.Errorf("export derived conversations: %w", err)
		}
	}
	if repairRelated {
		if err := exportRelatedDatasets(ctx, exportDB, sourceSnapshot, state.LastMessageID, staging.root, relatedExports); err != nil {
			return nil, err
		}
		if err := refreshRelatedCacheStats(ctx, exportDB, &state, relatedKinds); err != nil {
			return nil, err
		}
	}

	derived, err := identityindex.Build(ctx, exportDB, identityindex.BuildOptions{
		Mode:           identityindex.ModeIndexOnly,
		CommittedRoot:  analyticsDir,
		StagedBaseRoot: staging.root,
		OutputRoot:     staging.root,
		EffectiveAt:    state.LastSyncAt,
		Progress:       reportIdentityBuildProgress,
	})
	if err != nil {
		return nil, fmt.Errorf("build derived identity index: %w", err)
	}
	reportRelationshipActivityStats(derived.Activity)
	if derived.ConversationParticipantsFingerprint != conversationFingerprint {
		return nil, errors.New("derived conversation participant fingerprint changed during build")
	}
	if err := sourceSnapshot.Close(); err != nil {
		return nil, fmt.Errorf("close SQLite derived-refresh snapshot: %w", err)
	}

	state.IdentityRevision = identityRevision
	if repairRelated {
		state.DerivedDataRevision = derivedDataRevision
	}
	state.ParticipantIdentifierRevision = participantIdentifierRevision
	state.ParticipantDisplayNameRevision = participantDisplayNameRevision
	state.PersonDisplayNameRevision = personDisplayNameRevision
	state.ConversationParticipantsFingerprint = conversationFingerprint
	state.ConversationTypesFingerprint = typesFingerprint
	if repairRelated {
		state.LastRelatedChangeSeq = relatedChangeSeq
	}
	// The message snapshot remains unchanged; child-row statistics were
	// refreshed above when their source rows changed.
	plan := derivedCachePublishPlan(
		conversationChanged,
		typesChanged,
		identifiersChanged,
		identifiersChanged || displayNamesChanged,
		personDisplayNamesChanged || identityChanged,
	)
	if repairRelated {
		for dataset := range relatedReplacements {
			plan.Replace[dataset] = true
		}
	}
	if err := publishDerivedCache(staging, analyticsDir, plan, state, locking); err != nil {
		return nil, err
	}
	reportCacheTextRepairs(os.Stderr, textRepairs)
	if repairRelated {
		warnRelatedChangePrune(dbPath, relatedChangeSeq, derivedDataRevision)
	}
	return &buildResult{OutputDir: analyticsDir, IdentityOnly: true}, nil
}

func fingerprintConversationParticipantsFromSnapshot(
	ctx context.Context,
	db sqlRunner,
	lastMessageID int64,
) (string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT cp.conversation_id::BIGINT, cp.participant_id::BIGINT
		FROM sqlite_db.conversation_participants cp
		WHERE EXISTS (
			SELECT 1
			FROM sqlite_db.messages m
			WHERE m.conversation_id = cp.conversation_id
			  AND %s
			  AND TRY_CAST(m.id AS BIGINT) <= ?
		)
		ORDER BY cp.conversation_id, cp.participant_id
	`, exportableMessageWhere("m")), lastMessageID)
	if err != nil {
		return "", fmt.Errorf("query conversation participants from source snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	fingerprint, err := identityindex.FingerprintConversationParticipants(rows)
	if rowsErr := rows.Err(); rowsErr != nil && err == nil {
		return "", fmt.Errorf("iterate source conversation participants: %w", rowsErr)
	}
	return fingerprint, err
}

// fingerprintConversationTypesFromSnapshot mirrors
// sourceConversationTypesFingerprint over the export snapshot, so the stamp
// written at publish time describes exactly the type/title metadata the staged
// datasets baked. The query applies the same NULL defaults as the staleness
// query. FingerprintConversationMetadata repairs invalid UTF-8 on both paths
// before hashing.
func fingerprintConversationTypesFromSnapshot(
	ctx context.Context,
	db sqlRunner,
	lastMessageID int64,
) (string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT c.id::BIGINT,
		       COALESCE(TRY_CAST(c.conversation_type AS VARCHAR), 'email_thread'),
		       COALESCE(TRY_CAST(c.title AS VARCHAR), '')
		FROM sqlite_db.conversations c
		WHERE EXISTS (
			SELECT 1
			FROM sqlite_db.messages m
			WHERE m.conversation_id = c.id
			  AND %s
			  AND TRY_CAST(m.id AS BIGINT) <= ?
		)
		ORDER BY c.id
	`, exportableMessageWhere("m")), lastMessageID)
	if err != nil {
		return "", fmt.Errorf("query conversation metadata from source snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	fingerprint, err := identityindex.FingerprintConversationMetadata(rows)
	if rowsErr := rows.Err(); rowsErr != nil && err == nil {
		return "", fmt.Errorf("iterate source conversation metadata: %w", rowsErr)
	}
	return fingerprint, err
}

// stageParticipantClusters loads the Go-computed clusters into a DuckDB temp
// table that participantClustersSelectSQL reads.
func stageParticipantClusters(ctx context.Context, db sqlRunner, clusters map[int64]int64) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TEMP TABLE tmp_participant_clusters (participant_id BIGINT, canonical_id BIGINT)`); err != nil {
		return fmt.Errorf("create participant clusters temp table: %w", err)
	}
	if len(clusters) == 0 {
		return nil
	}
	values := make([]string, 0, len(clusters))
	for participantID, canonicalID := range clusters {
		values = append(values, fmt.Sprintf("(%d,%d)", participantID, canonicalID))
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tmp_participant_clusters VALUES `+strings.Join(values, ",")); err != nil {
		return fmt.Errorf("populate participant clusters temp table: %w", err)
	}
	return nil
}

const participantClustersSelectSQL = `SELECT participant_id, canonical_id FROM tmp_participant_clusters`

func conversationParticipantsSelectSQL(maxMessageID int64) string {
	return fmt.Sprintf(`SELECT cp.conversation_id, cp.participant_id
		FROM sqlite_db.conversation_participants cp
		WHERE EXISTS (
			SELECT 1
			FROM sqlite_db.messages m
			WHERE m.conversation_id = cp.conversation_id
			  AND %s
			  AND TRY_CAST(m.id AS BIGINT) <= %d
		)`, exportableMessageWhere("m"), maxMessageID)
}

// copyParquet writes selectSQL to dir/file as zstd Parquet, creating dir.
func copyParquet(ctx context.Context, db sqlRunner, dir, file, selectSQL string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`COPY (%s) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')`,
		selectSQL, quoteCacheSQL(filepath.Join(dir, file))))
	return err
}

func quoteCacheSQL(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func derivedCachePublishPlan(
	includeConversationParticipants, includeConversations,
	includeParticipantIdentifiers, includeParticipants, includePersonDisplayNames bool,
) cachePublishPlan {
	plan := cachePublishPlan{
		Append:  make(map[string]bool),
		Replace: make(map[string]bool),
	}
	for _, dataset := range []string{
		tableOwnerParticipants,
		tableParticipantClusters,
		identityindex.DatasetActivity,
		identityindex.DatasetPeople,
		identityindex.DatasetDomains,
		identityindex.DatasetRelationshipDaily,
		identityindex.DatasetLogicalContributions,
		identityindex.DatasetTemperatureContributions,
	} {
		plan.Replace[dataset] = true
	}
	if includeConversationParticipants {
		plan.Replace[tableConversationParticipants] = true
	}
	if includeConversations {
		plan.Replace[tableConversations] = true
	}
	if includeParticipantIdentifiers {
		plan.Replace[tableParticipantIdentifiers] = true
	}
	if includeParticipants {
		plan.Replace[tableParticipants] = true
	}
	if includePersonDisplayNames {
		plan.Replace[tablePersonDisplayNames] = true
	}
	return plan
}

func publishDerivedCache(
	staging *cacheStaging,
	analyticsDir string,
	plan cachePublishPlan,
	state query.CacheSyncState,
	locking cachePublishLocking,
) error {
	data, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode derived cache marker: %w", err)
	}
	return publishCacheWithBeforeMarker(
		staging,
		analyticsDir,
		plan,
		data,
		derivedPublishBeforeMarkerHook,
		locking,
	)
}

// verifyRelatedOnlyAdditions confirms that new sync additions changed only
// child rows of cached messages. A refresh may then record the new addition
// count. Any change to the message population needs a full build instead:
// an old message that became exportable inside the cached boundary, or an
// exportable message above it. The staleness check ignores a message deleted
// at the source when it looks for new messages, so such a message above the
// boundary would otherwise stay out of the cache.
func verifyRelatedOnlyAdditions(snapshot *cacheSourceSnapshot, state syncState) error {
	var coveredCount int64
	if err := snapshot.QueryRow(coveredCacheMessageCountSQL(), state.LastMessageID).
		Scan(&coveredCount); err != nil {
		return fmt.Errorf("check related-refresh message population: %w", err)
	}
	if coveredCount != state.Stats.TotalMessages {
		return fmt.Errorf("%w: cached message population changed",
			ErrDerivedRefreshRequiresFullBuild)
	}
	var uncachedExportable bool
	if err := snapshot.QueryRow(`SELECT EXISTS (SELECT 1 FROM messages WHERE id > ? AND `+
		exportableMessageWhere("")+`)`, state.LastMessageID).Scan(&uncachedExportable); err != nil {
		return fmt.Errorf("check related-refresh message boundary: %w", err)
	}
	if uncachedExportable {
		return fmt.Errorf("%w: exportable messages above the cached boundary",
			ErrDerivedRefreshRequiresFullBuild)
	}
	return nil
}
