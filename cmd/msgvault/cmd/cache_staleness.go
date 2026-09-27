package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// cacheStaleness describes why the analytics cache needs a rebuild.
type cacheStaleness struct {
	NeedsBuild bool
	// HasUsablePublication distinguishes ordinary data drift, which may be
	// throttled after a scheduled sync, from cache recovery conditions that
	// must rebuild immediately. PublishedAt is valid only when this is true.
	HasUsablePublication bool
	PublishedAt          time.Time
	Generation           string
	PendingAdditions     int64 // positive cache addition counter delta, when known
	HasNew               bool  // new messages since last build
	HasDeleted           bool  // deletions since last build
	HasUpdated           bool  // updates or additions within the cached ID boundary require repair
	// HasIdentityDrift signals participant_links or account_identities
	// changed since the last build. Also set whenever
	// HasAccountIdentityDrift is set (AddAccountIdentity/RemoveAccountIdentity
	// bump both revisions together), so callers deciding whether the cheap
	// index-only refresh applies must check HasAccountIdentityDrift too —
	// see derivedDriftOnly in build_cache.go.
	HasIdentityDrift bool
	// HasDerivedDataDrift signals an offline repair rewrote existing message
	// or attachment facts already inside the committed cache watermark. These
	// facts require a full rebuild; neither incremental append nor the
	// identity-only refresh can replace them.
	HasDerivedDataDrift bool
	// HasConversationParticipantDrift signals conversation membership changed
	// for a conversation already represented by the committed message
	// watermark. The index-only refresh can rebuild relationship_activity and
	// its compact datasets without rewriting message facts.
	HasConversationParticipantDrift bool
	// HasConversationTypeDrift signals conversation_type or title changed for
	// a conversation already represented by the committed message watermark.
	// This metadata is baked into committed relationship_activity rows (and
	// the replaceable conversations base dataset), so like membership drift it
	// is repaired by the index-only refresh — unless new messages also arrived,
	// in which case the incremental append cannot rewrite the already-committed
	// rows and a full rebuild is forced below.
	HasConversationTypeDrift bool
	// HasParticipantIdentifierDrift signals identifier rows or their
	// service/scope classification changed since the last build. Identifiers
	// bake into the identity directory datasets
	// (participant_identifiers, relationship_people search values) but not
	// into per-row activity facts, so this drift is repaired by the
	// index-only refresh and — unlike link or conversation drift — never
	// escalates to a full rebuild when new messages coincide: incremental
	// builds re-stage participant_identifiers in full anyway.
	HasParticipantIdentifierDrift bool
	// HasParticipantDisplayNameDrift signals participant display-name changes
	// since the last build. Display names are baked into participants.parquet
	// and relationship_people, but not into message facts, so the index-only
	// refresh can repair this drift without a full rebuild.
	HasParticipantDisplayNameDrift bool
	// HasPersonDisplayNameDrift repairs curated names without rewriting message facts.
	HasPersonDisplayNameDrift bool
	// HasAccountIdentityDrift signals an identity mutation that invalidates
	// baked message data since the last build: an account identity was
	// confirmed or removed, or two participants were merged (merges repoint
	// messages.sender_id). Either changes the is_from_me flag baked into
	// message Parquet shards. Unlike plain participant-link drift, this can
	// only be repaired by a full rebuild, so it always sets FullRebuild.
	HasAccountIdentityDrift bool
	FullRebuild             bool // must rewrite all shards (not incremental)
	Reason                  string
}

// deletedSinceBuildCountSQL counts exportable messages source-deleted since
// the last cache build. It runs on every daemon start before the API server
// binds, so it must be served by idx_messages_deleted_from_source_at rather
// than a full messages scan (seconds of cold-start latency on a large
// archive); the query-plan test locks that in.
func deletedSinceBuildCountSQL() string {
	return `
		SELECT COUNT(*) FROM messages
		WHERE deleted_from_source_at IS NOT NULL
		  AND deleted_from_source_at >= ?
		  AND ` + sentCacheExportMessageWhere("")
}

// hiddenSinceBuildCountSQL counts exportable messages dedup-hidden since the
// last cache build. Same cold-start constraint as deletedSinceBuildCountSQL:
// it must be served by idx_messages_deleted_at.
func hiddenSinceBuildCountSQL() string {
	return `
		SELECT COUNT(*) FROM messages
		WHERE deleted_at IS NOT NULL
		  AND deleted_at >= ?
		  AND deleted_from_source_at IS NULL
		  AND ` + sentCacheExportMessageWhere("")
}

// cacheNeedsBuild checks if the analytics cache needs to be built or
// updated. Collects all staleness signals before returning so that
// e.g. a mixed add+delete sync correctly reports both.
//
// The Parquet cache is a SQLite-only ETL — when dbPath points at a
// PostgreSQL DSN, this returns "no build needed" rather than dispatching
// SQLite-shaped queries against pgx (which would fail on the ?
// placeholders and the sqlite_master probe).
func cacheNeedsBuild(dbPath, analyticsDir string) cacheStaleness {
	return cacheNeedsBuildContext(context.Background(), dbPath, analyticsDir)
}

func cacheNeedsBuildContext(ctx context.Context, dbPath, analyticsDir string) cacheStaleness {
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	if store.IsPostgresURL(dbPath) {
		return cacheStaleness{}
	}
	buildLock, err := acquireCacheBuildLock(ctx, analyticsDir)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot acquire cache recovery lock")
	}
	defer func() { _ = buildLock.Unlock() }()
	return cacheNeedsBuildLocked(ctx, dbPath, analyticsDir)
}

func cacheStalenessFailure(ctx context.Context, reason string) cacheStaleness {
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	return cacheStaleness{
		NeedsBuild:  true,
		FullRebuild: true,
		Reason:      reason,
	}
}

// cacheNeedsBuildForQuery inspects the committed publication without waiting
// for a builder that is staging the next generation. The shared lock excludes
// only the brief publication step and destructive cache maintenance.
func cacheNeedsBuildForQuery(ctx context.Context, dbPath, analyticsDir string) (cacheStaleness, error) {
	return inspectCacheForQuery(ctx, dbPath, analyticsDir, true)
}

// cacheNeedsBuildForServing omits the two archive-wide conversation hashes.
// The scheduled background check runs the full inspection when the minimum
// rebuild interval expires; requests still see indexed sync and revision
// signals immediately, without scanning millions of membership rows.
func cacheNeedsBuildForServing(ctx context.Context, dbPath, analyticsDir string) (cacheStaleness, error) {
	return inspectCacheForQuery(ctx, dbPath, analyticsDir, false)
}

func inspectCacheForQuery(ctx context.Context, dbPath, analyticsDir string, full bool) (cacheStaleness, error) {
	if store.IsPostgresURL(dbPath) {
		return cacheStaleness{}, nil
	}
	release, err := query.AcquireCacheReadLock(ctx, analyticsDir)
	if err != nil {
		return cacheStaleness{}, err
	}
	defer release()
	result := cacheNeedsBuildLockedWithConversationHashes(ctx, dbPath, analyticsDir, full)
	return result, ctx.Err()
}

// cacheNeedsBuildLocked performs readiness inspection while the caller holds
// either the builder lock or the publication read lock, so the committed
// marker cannot change mid-inspection. Incomplete marker-last
// publication is detected as drift and rebuilt; publication does not
// maintain a recovery journal.
func cacheNeedsBuildLocked(ctx context.Context, dbPath, analyticsDir string) cacheStaleness {
	return cacheNeedsBuildLockedWithConversationHashes(ctx, dbPath, analyticsDir, true)
}

func cacheNeedsBuildLockedWithConversationHashes(ctx context.Context, dbPath, analyticsDir string, full bool) cacheStaleness {
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	readiness, err := query.InspectCacheReadiness(analyticsDir)
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot inspect cache status")
	}
	switch readiness {
	case query.CacheAbsent:
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "no cache exists",
		}
	case query.CacheInterrupted:
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "analytics cache publication interrupted",
		}
	case query.CacheStaleSchema:
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "analytics cache schema is stale",
		}
	case query.CacheDrifted:
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "analytics cache publication drifted",
		}
	case query.CacheReady:
	}

	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	state, err := query.ReadCacheSyncState(analyticsDir)
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot read cache state")
	}

	// A cache written under a different Parquet schema layout is stale even
	// when message counts match, so bumping cacheSchemaVersion must force a
	// full rebuild. buildCache re-checks this, but the daemon only calls it
	// when this gate reports NeedsBuild.
	if state.SchemaVersion != cacheSchemaVersion {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: fmt.Sprintf("cache schema v%d != current v%d",
				state.SchemaVersion, cacheSchemaVersion),
		}
	}

	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	// Inspection must not checkpoint the WAL on close: an active export can
	// hold a read snapshot, making that checkpoint wait for the busy timeout.
	db, err := store.OpenReadOnlyContext(ctx, dbPath)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify cache status")
	}
	defer func() { _ = db.Close() }()

	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	var maxLiveID int64
	err = db.DB().QueryRowContext(ctx, `
		SELECT COALESCE(MAX(id), 0) FROM messages
		WHERE `+cacheLiveMessageWhere("")).Scan(&maxLiveID)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify cache status")
	}

	// Collect staleness signals without short-circuiting so a mixed
	// add+delete sync correctly triggers a full rebuild.
	var reasons []string
	result := cacheStaleness{
		HasUsablePublication: true,
		PublishedAt:          state.PublishedAt,
		Generation:           state.DatasetFingerprint,
	}

	if state.FullRebuildRequired {
		result.HasUpdated = true
		result.FullRebuild = true
		reasons = append(reasons, "previous build published a partial snapshot")
	}

	if maxLiveID > state.LastMessageID {
		newCount := maxLiveID - state.LastMessageID
		result.HasNew = true
		result.PendingAdditions = newCount
		reasons = append(reasons,
			fmt.Sprintf("%d new messages", newCount))
	}

	syncAtStr := state.LastSyncAt.UTC().Format("2006-01-02 15:04:05")
	var deletedSinceBuild int64
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	err = db.DB().QueryRowContext(ctx, deletedSinceBuildCountSQL(), syncAtStr).Scan(&deletedSinceBuild)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify deletion state")
	}
	if deletedSinceBuild > 0 {
		result.HasDeleted = true
		result.FullRebuild = true
		reasons = append(reasons,
			fmt.Sprintf("%d deletions", deletedSinceBuild))
	}

	// Dedup-hidden rows (deleted_at) are excluded from the messages
	// Parquet export, so a dedup run after the last cache build leaves
	// stale duplicate rows in the cache. Detect that by counting hides
	// since LastSyncAt and force a full rebuild if any are present.
	// The deleted_from_source_at IS NULL clause keeps the count
	// disjoint from the deletedSinceBuild count above so a row that is
	// both source-deleted and dedup-hidden after LastSyncAt is reported
	// once (as a deletion), not double-counted in the reason string.
	var hiddenSinceBuild int64
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	err = db.DB().QueryRowContext(ctx, hiddenSinceBuildCountSQL(), syncAtStr).Scan(&hiddenSinceBuild)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify dedup state")
	}
	if hiddenSinceBuild > 0 {
		result.HasDeleted = true
		result.FullRebuild = true
		reasons = append(reasons,
			fmt.Sprintf("%d dedup-hidden", hiddenSinceBuild))
	}

	var hasSyncRunsTable int
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	err = db.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'sync_runs'
	`).Scan(&hasSyncRunsTable)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify sync history")
	}
	if hasSyncRunsTable > 0 {
		if ctx.Err() != nil {
			return cacheStaleness{}
		}
		counters, counterErr := readCacheSyncCountersContext(ctx, db.DB())
		err = counterErr
		if err != nil {
			return cacheStalenessFailure(ctx, "cannot verify sync history")
		}
		if counters.updates != state.LastCacheUpdateCount {
			result.HasUpdated = true
			result.FullRebuild = true
			if updateDelta := counters.updates - state.LastCacheUpdateCount; updateDelta > 0 {
				reasons = append(reasons,
					fmt.Sprintf("%d updated messages", updateDelta))
			} else {
				reasons = append(reasons, fmt.Sprintf(
					"cache update watermark changed from %d to %d",
					state.LastCacheUpdateCount, counters.updates))
			}
		}
		if counters.failedRunCount != state.LastFailedSyncRunCount ||
			counters.failedRunIDSum != state.LastFailedSyncRunIDSum {
			result.HasUpdated = true
			result.FullRebuild = true
			reasons = append(reasons, fmt.Sprintf(
				"failed sync watermark changed from count=%d,sum=%d to count=%d,sum=%d",
				state.LastFailedSyncRunCount, state.LastFailedSyncRunIDSum,
				counters.failedRunCount, counters.failedRunIDSum))
		}
		if counters.additions != state.LastCacheAdditionCount {
			if delta := counters.additions - state.LastCacheAdditionCount; delta > 0 {
				result.PendingAdditions = delta
			}
			// A larger message ID gives the incremental exporter an exact lower
			// boundary for ordinary append-only syncs. If the ID boundary did not
			// move (or history moved backwards), the changed addition counter may
			// describe related rows for a parent already present in Parquet, so a
			// full rebuild is the only safe repair.
			if counters.additions < state.LastCacheAdditionCount || maxLiveID <= state.LastMessageID {
				result.HasUpdated = true
				result.FullRebuild = true
				reasons = append(reasons, fmt.Sprintf(
					"cache addition watermark changed from %d to %d within message boundary %d",
					state.LastCacheAdditionCount, counters.additions, state.LastMessageID))
			}
		}
	}

	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	derivedDataRevision, err := db.DerivedDataRevisionContext(ctx)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify derived-data revision")
	}
	if derivedDataRevision != state.DerivedDataRevision {
		result.HasDerivedDataDrift = true
		result.FullRebuild = true
		reasons = append(reasons, "derived message data changed")
	}

	// Account-identity drift covers identity mutations that invalidate baked
	// message data: confirming or removing a confirmed "me" address via
	// AddAccountIdentity/RemoveAccountIdentity, and participant merges via
	// MergeParticipants/mergeParticipant (which repoint messages.sender_id).
	// Either changes the is_from_me flag baked into every message Parquet
	// shard at export time. Unlike plain participant link/unlink drift, this
	// cannot be repaired by the lightweight identity-only refresh —
	// incremental appends can't rewrite already-exported shards — so it
	// always forces a full rebuild. Checked before HasIdentityDrift, and
	// independently of it, so derivedDriftOnly (build_cache.go) never
	// mistakes this for the cheap-refresh case even though the same
	// mutation also bumps identity_revision below.
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	accountIdentityRevision, err := db.AccountIdentityRevisionContext(ctx)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify account identity revision")
	}
	if accountIdentityRevision != state.AccountIdentityRevision {
		result.HasAccountIdentityDrift = true
		result.FullRebuild = true
		reasons = append(reasons, "identity mutations that invalidate baked message data (account identities, participant merges)")
	}

	// Identity drift (participant link/unlink/merge mutations, or
	// confirming/removing an account identity) affects only identity-derived
	// datasets, not message content, so on its own it never forces a full
	// rebuild: the index-only refresh (refreshDerivedDatasetsOnly) handles
	// it, and a full rebuild triggered by any other signal (including
	// HasAccountIdentityDrift above) refreshes it naturally.
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	identityRevision, err := db.IdentityRevisionContext(ctx)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify identity revision")
	}
	if identityRevision != state.IdentityRevision {
		result.HasIdentityDrift = true
		reasons = append(reasons, "identity revision changed")
	}

	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	participantIdentifierRevision, err := db.ParticipantIdentifierRevisionContext(ctx)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify participant identifier revision")
	}
	if participantIdentifierRevision != state.ParticipantIdentifierRevision {
		result.HasParticipantIdentifierDrift = true
		reasons = append(reasons, "participant identifiers changed")
	}

	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	participantDisplayNameRevision, err := db.ParticipantDisplayNameRevisionContext(ctx)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify participant display-name revision")
	}
	if participantDisplayNameRevision != state.ParticipantDisplayNameRevision {
		result.HasParticipantDisplayNameDrift = true
		reasons = append(reasons, "participant display names changed")
	}
	if ctx.Err() != nil {
		return cacheStaleness{}
	}
	personDisplayNameRevision, err := db.PersonDisplayNameRevisionContext(ctx)
	if err != nil {
		return cacheStalenessFailure(ctx, "cannot verify person display-name revision")
	}
	if personDisplayNameRevision != state.PersonDisplayNameRevision {
		result.HasPersonDisplayNameDrift = true
		reasons = append(reasons, "person display names changed")
	}

	if full {
		if ctx.Err() != nil {
			return cacheStaleness{}
		}
		conversationFingerprint, err := sourceConversationParticipantsFingerprint(
			ctx,
			db.DB(),
			state.LastMessageID,
		)
		if err != nil {
			return cacheStalenessFailure(ctx, "cannot verify conversation participants")
		}
		if conversationFingerprint != state.ConversationParticipantsFingerprint {
			result.HasConversationParticipantDrift = true
			reasons = append(reasons, "conversation participants changed")
		}

		if ctx.Err() != nil {
			return cacheStaleness{}
		}
		typesFingerprint, err := sourceConversationTypesFingerprint(
			ctx,
			db.DB(),
			state.LastMessageID,
		)
		if err != nil {
			return cacheStalenessFailure(ctx, "cannot verify conversation metadata")
		}
		if typesFingerprint != state.ConversationTypesFingerprint {
			result.HasConversationTypeDrift = true
			reasons = append(reasons, "conversation metadata changed")
		}
	}

	// An incremental build can append only new activity rows. If canonical
	// links, conversation membership, or conversation types also changed,
	// existing rows need to be rewritten under the new relationship
	// dimensions, so rebuild the base generation and relationship index
	// together.
	if result.HasNew &&
		(result.HasIdentityDrift || result.HasConversationParticipantDrift ||
			result.HasConversationTypeDrift) {
		result.FullRebuild = true
	}

	if len(reasons) > 0 {
		result.NeedsBuild = true
		result.Reason = strings.Join(reasons, "; ")
	}

	return result
}

// sourceConversationTypesFingerprint hashes (id, conversation_type, title) for
// conversations with exportable messages inside the committed watermark. The
// NULL normalization must match the conversations Parquet export and
// fingerprintConversationTypesFromSnapshot so an unchanged database always
// reproduces the stamped fingerprint.
func sourceConversationTypesFingerprint(
	ctx context.Context,
	db *sql.DB,
	lastMessageID int64,
) (string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.id, COALESCE(c.conversation_type, 'email_thread'),
		       COALESCE(c.title, '')
		FROM conversations c
		WHERE EXISTS (
			SELECT 1
			FROM messages m
			WHERE m.conversation_id = c.id
			  AND `+exportableMessageWhere("m")+`
			  AND m.id <= ?
		)
		ORDER BY c.id
	`, lastMessageID)
	if err != nil {
		return "", fmt.Errorf("query conversation metadata for fingerprint: %w", err)
	}
	defer func() { _ = rows.Close() }()
	fingerprint, err := identityindex.FingerprintConversationMetadata(rows)
	if rowsErr := rows.Err(); rowsErr != nil && err == nil {
		return "", fmt.Errorf("iterate conversation metadata for fingerprint: %w", rowsErr)
	}
	return fingerprint, err
}

func sourceConversationParticipantsFingerprint(
	ctx context.Context,
	db *sql.DB,
	lastMessageID int64,
) (string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT cp.conversation_id, cp.participant_id
		FROM conversation_participants cp
		WHERE EXISTS (
			SELECT 1
			FROM messages m
			WHERE m.conversation_id = cp.conversation_id
			  AND `+exportableMessageWhere("m")+`
			  AND m.id <= ?
		)
		ORDER BY cp.conversation_id, cp.participant_id
	`, lastMessageID)
	if err != nil {
		return "", fmt.Errorf("query conversation participants for fingerprint: %w", err)
	}
	defer func() { _ = rows.Close() }()
	fingerprint, err := identityindex.FingerprintConversationParticipants(rows)
	if rowsErr := rows.Err(); rowsErr != nil && err == nil {
		return "", fmt.Errorf("iterate conversation participants for fingerprint: %w", rowsErr)
	}
	return fingerprint, err
}
