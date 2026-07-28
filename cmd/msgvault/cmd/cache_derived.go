package cmd

import (
	"context"
	"encoding/json"
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

func refreshDerivedDatasetsOnly(
	ctx context.Context,
	dbPath, analyticsDir string,
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
	clusters, err := st.ParticipantClusters()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read participant clusters: %w", err)
	}
	if err := st.Close(); err != nil {
		return nil, fmt.Errorf("close identity store: %w", err)
	}

	spillDir := filepath.Join(staging.root, "duckdb-tmp")
	duckDB, err := duckdbutil.Open(ctx, duckdbutil.BuilderPolicy(spillDir))
	if err != nil {
		return nil, fmt.Errorf("open bounded DuckDB for derived refresh: %w", err)
	}
	defer func() { _ = duckDB.Close() }()

	sourceSnapshot, err := openCacheSourceSnapshot(duckDB, dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sourceSnapshot.Close() }()
	if err := sourceSnapshot.PrepareDatasets(
		tableMessages,
		tableConversationParticipants,
		"account_identities",
		tableParticipants,
		tableParticipantIdentifiers,
	); err != nil {
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

	if err := exportDerivedOwnerParticipants(ctx, exportDB, staging.root); err != nil {
		return nil, err
	}
	if err := exportDerivedParticipantClusters(ctx, exportDB, clusters, staging.root); err != nil {
		return nil, err
	}
	conversationChanged :=
		conversationFingerprint != state.ConversationParticipantsFingerprint
	if conversationChanged {
		if err := exportDerivedConversationParticipants(
			ctx,
			exportDB,
			state.LastMessageID,
			staging.root,
		); err != nil {
			return nil, err
		}
	}

	derived, err := identityindex.Build(ctx, exportDB, identityindex.BuildOptions{
		Mode:           identityindex.ModeIndexOnly,
		CommittedRoot:  analyticsDir,
		StagedBaseRoot: staging.root,
		OutputRoot:     staging.root,
		Progress:       reportIdentityBuildProgress,
	})
	if err != nil {
		return nil, fmt.Errorf("build derived identity index: %w", err)
	}
	if derived.ConversationParticipantsFingerprint != conversationFingerprint {
		return nil, errors.New("derived conversation participant fingerprint changed during build")
	}
	if err := sourceSnapshot.Close(); err != nil {
		return nil, fmt.Errorf("close SQLite derived-refresh snapshot: %w", err)
	}

	state.IdentityRevision = identityRevision
	state.ConversationParticipantsFingerprint = conversationFingerprint
	// Stats describe the unchanged committed raw snapshot. Preserve them
	// byte-for-byte instead of scanning Parquet again.
	plan := derivedCachePublishPlan(conversationChanged)
	if err := publishDerivedCache(staging, analyticsDir, plan, state); err != nil {
		return nil, err
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

func exportDerivedOwnerParticipants(
	ctx context.Context,
	db sqlRunner,
	stagingRoot string,
) error {
	dir := filepath.Join(stagingRoot, tableOwnerParticipants)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create derived owner participants directory: %w", err)
	}
	path := filepath.Join(dir, "owner_participants.parquet")
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		COPY (
			SELECT DISTINCT ai.source_id, p.id AS participant_id
			FROM sqlite_db.account_identities ai
			JOIN sqlite_db.participants p
			  ON p.email_address IS NOT NULL
			 AND lower(p.email_address) = lower(ai.address)
			UNION
			SELECT DISTINCT ai.source_id, pi.participant_id
			FROM sqlite_db.account_identities ai
			JOIN sqlite_db.participant_identifiers pi
			  ON (pi.identifier_type = 'email'
			      AND lower(pi.identifier_value) = lower(ai.address))
			  OR (pi.identifier_type != 'email'
			      AND pi.identifier_value = ai.address)
		) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')
	`, quoteCacheSQL(path)))
	if err != nil {
		return fmt.Errorf("export derived owner participants: %w", err)
	}
	return nil
}

func exportDerivedParticipantClusters(
	ctx context.Context,
	db sqlRunner,
	clusters map[int64]int64,
	stagingRoot string,
) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TEMP TABLE tmp_derived_participant_clusters (
			participant_id BIGINT,
			canonical_id BIGINT
		)
	`); err != nil {
		return fmt.Errorf("create derived participant clusters table: %w", err)
	}
	if len(clusters) > 0 {
		values := make([]string, 0, len(clusters))
		for participantID, canonicalID := range clusters {
			values = append(values, fmt.Sprintf("(%d,%d)", participantID, canonicalID))
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO tmp_derived_participant_clusters
			VALUES `+strings.Join(values, ",")); err != nil {
			return fmt.Errorf("populate derived participant clusters: %w", err)
		}
	}
	dir := filepath.Join(stagingRoot, tableParticipantClusters)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create derived participant clusters directory: %w", err)
	}
	path := filepath.Join(dir, "participant_clusters.parquet")
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
		COPY (
			SELECT participant_id, canonical_id
			FROM tmp_derived_participant_clusters
		) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')
	`, quoteCacheSQL(path))); err != nil {
		return fmt.Errorf("export derived participant clusters: %w", err)
	}
	return nil
}

func exportDerivedConversationParticipants(
	ctx context.Context,
	db sqlRunner,
	lastMessageID int64,
	stagingRoot string,
) error {
	dir := filepath.Join(stagingRoot, tableConversationParticipants)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create derived conversation participants directory: %w", err)
	}
	path := filepath.Join(dir, "conversation_participants.parquet")
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		COPY (
			SELECT cp.conversation_id, cp.participant_id
			FROM sqlite_db.conversation_participants cp
			WHERE EXISTS (
				SELECT 1
				FROM sqlite_db.messages m
				WHERE m.conversation_id = cp.conversation_id
				  AND %s
				  AND TRY_CAST(m.id AS BIGINT) <= %d
			)
		) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')
	`, exportableMessageWhere("m"), lastMessageID, quoteCacheSQL(path)))
	if err != nil {
		return fmt.Errorf("export derived conversation participants: %w", err)
	}
	return nil
}

func quoteCacheSQL(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func derivedCachePublishPlan(includeConversationParticipants bool) cachePublishPlan {
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
	} {
		plan.Replace[dataset] = true
	}
	if includeConversationParticipants {
		plan.Replace[tableConversationParticipants] = true
	}
	return plan
}

func publishDerivedCache(
	staging *cacheStaging,
	analyticsDir string,
	plan cachePublishPlan,
	state query.CacheSyncState,
) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode derived cache marker: %w", err)
	}
	return publishCacheWithBeforeMarker(
		staging,
		analyticsDir,
		plan,
		data,
		derivedPublishBeforeMarkerHook,
	)
}
