package cacheops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // DuckDB driver (database/sql)
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const (
	datasetOwnerParticipants   = "owner_participants"
	datasetParticipantClusters = "participant_clusters"
)

// identityDatasets lists the Parquet dataset directories RefreshIdentityDatasets
// re-exports. Both are always fully replaced, matching how the full cache
// builder treats them (see replacesCacheDataset in cmd/msgvault/cmd).
var identityDatasets = []string{datasetOwnerParticipants, datasetParticipantClusters}

// ErrNoCommittedCache is returned by RefreshIdentityDatasets when
// analyticsDir has no committed cache publication (no _last_sync.json) to
// refresh. Callers should treat this like any other cache-unavailable
// condition (the API layer maps it to the standard response).
var ErrNoCommittedCache = errors.New("cacheops: no committed analytics cache to refresh")

// ErrCacheNotRefreshable is returned by RefreshIdentityDatasets when the
// committed publication fails integrity validation in a way the
// identity-only refresh cannot repair: the on-disk datasets no longer match
// the committed DatasetFingerprint (modified, truncated, or corrupted
// outside the ETL), a required non-identity dataset is missing, or the
// commit marker itself records an interrupted publication. Proceeding would
// re-fingerprint the damaged tree and stamp it as valid, hiding corruption
// that readers currently detect. The only discrepancy the refresh may
// repair is missing identity dataset(s) — regenerating those is exactly its
// job.
var ErrCacheNotRefreshable = errors.New(
	"cacheops: analytics cache failed integrity validation and an identity-only refresh would mask the damage; " +
		"rebuild the cache with 'msgvault build-cache --full-rebuild'")

// RefreshIdentityDatasets re-exports owner_participants and
// participant_clusters from st into the committed cache at analyticsDir,
// leaving every message-derived dataset (including is_from_me, baked into
// the message shards at export time) untouched, and stamps the new
// identity revision into _last_sync.json. It returns the stamped revision.
// It deliberately does NOT advance AccountIdentityRevision — see
// publishIdentityDatasets.
//
// Both datasets are derived entirely from st's own queries (never from a
// SQLite file path), so the same code path runs for SQLite- and
// PostgreSQL-backed archives alike: the owner_participants query below has
// no dialect-specific SQL, so no ATTACH or per-backend branching is needed,
// and it never crashes a PostgreSQL archive.
//
// Concurrency: callers must hold the analytics cache build lock
// (query.CacheBuildLockPath) exclusively for the duration of this call —
// the same lock a full cache build holds. This function does not acquire it
// itself: a caller that already holds it (the build-cache flow wires this
// in for identity-drift-only staleness) would self-deadlock on a second
// exclusive acquisition of the same underlying flock file. With that lock
// held, concurrent Parquet readers (which take it shared) safely wait out
// the rename below, exactly as they wait out a full build.
func RefreshIdentityDatasets(ctx context.Context, st *store.Store, analyticsDir string) (int64, error) {
	state, err := query.ReadCacheSyncState(analyticsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("%w: %s", ErrNoCommittedCache, analyticsDir)
		}
		return 0, fmt.Errorf("read cache sync state before identity refresh: %w", err)
	}

	revision, err := st.IdentityRevision()
	if err != nil {
		return 0, fmt.Errorf("read identity revision: %w", err)
	}

	// No-op short-circuit: every link/unlink API call re-runs this refresh
	// even when the mutation changed nothing (e.g. re-linking two already-
	// linked participants), and re-publishing unconditionally would rewrite
	// the Parquet datasets and advance PublishedAt/DatasetFingerprint for
	// no reason — needlessly 409-ing every active pagination cursor even
	// though the committed data is unchanged. Short-circuit only when the
	// committed revision already matches the current one AND both dataset
	// directories are actually on disk: a prior refresh that failed before
	// reaching publishIdentityDatasets leaves the committed revision
	// lagging behind the current one, so this condition is false and the
	// retry below still does the real work, satisfying the spec's
	// requirement that retry-after-stale re-attempts the refresh.
	if state.IdentityRevision == revision && identityDatasetsExist(analyticsDir) {
		return revision, nil
	}

	// Guard against legitimizing a damaged publication: publishing below
	// recomputes DatasetFingerprint over the whole tree, so refreshing on top
	// of a drifted or incomplete cache would stamp the damage as valid and
	// hide it from readers that currently detect it. The short-circuit above
	// commits nothing, so it needs no such guard.
	if err := validateCommittedPublication(analyticsDir, state); err != nil {
		return 0, err
	}

	owners, err := ownerParticipantRows(ctx, st)
	if err != nil {
		return 0, fmt.Errorf("read owner participants: %w", err)
	}
	clusters, err := st.ParticipantClusters()
	if err != nil {
		return 0, fmt.Errorf("read participant clusters: %w", err)
	}

	staging, err := newIdentityStaging(analyticsDir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = staging.cleanup() }()

	db, err := sql.Open("duckdb", "")
	if err != nil {
		return 0, fmt.Errorf("open duckdb for identity refresh: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := exportOwnerParticipants(ctx, db, owners, staging.datasetDir(datasetOwnerParticipants)); err != nil {
		return 0, err
	}
	if err := exportParticipantClusters(ctx, db, clusters, staging.datasetDir(datasetParticipantClusters)); err != nil {
		return 0, err
	}

	if err := publishIdentityDatasets(staging, analyticsDir, state, revision); err != nil {
		return 0, err
	}
	return revision, nil
}

// identityDatasetsExist reports whether every identity dataset directory
// under analyticsDir exists and has at least one file staged into it.
// exportOwnerParticipants/exportParticipantClusters always write a Parquet
// file, even for zero rows (see their comments), so an empty or missing
// directory means the dataset was never actually published — the no-op
// short-circuit in RefreshIdentityDatasets must not trust a matching
// revision alone in that case.
func identityDatasetsExist(analyticsDir string) bool {
	for _, dataset := range identityDatasets {
		entries, err := os.ReadDir(filepath.Join(analyticsDir, dataset))
		if err != nil || len(entries) == 0 {
			return false
		}
	}
	return true
}

// validateCommittedPublication checks the committed publication at
// analyticsDir against its own commit marker before an identity-only refresh
// republishes on top of it. It permits exactly one discrepancy — missing
// identity dataset(s), which the refresh regenerates from scratch — and
// returns an error wrapping ErrCacheNotRefreshable for anything else: an
// interrupted commit marker, a missing non-identity dataset, or datasets
// that no longer match the committed DatasetFingerprint.
//
// When an identity dataset is missing, its files no longer contribute the
// records the committed fingerprint was computed over, so the whole-tree
// comparison cannot match and is skipped; the remaining datasets are then
// only checked for presence. This deliberately calls
// query.CacheDatasetFingerprint directly, not the identityPublishFingerprint
// seam: fault-injection tests target the publish-time fingerprint, and
// validation must observe the real on-disk state.
func validateCommittedPublication(analyticsDir string, state query.CacheSyncState) error {
	if state.LastSyncAt.IsZero() || state.PublishedAt.IsZero() || state.DatasetFingerprint == "" {
		return fmt.Errorf("%w: %s: commit marker records an interrupted publication", ErrCacheNotRefreshable, analyticsDir)
	}
	missing, err := datasetsMissingParquet(analyticsDir)
	if err != nil {
		return fmt.Errorf("inspect committed datasets before identity refresh: %w", err)
	}
	if len(missing) > 0 {
		var nonIdentity []string
		for _, dataset := range missing {
			if !slices.Contains(identityDatasets, dataset) {
				nonIdentity = append(nonIdentity, dataset)
			}
		}
		if len(nonIdentity) > 0 {
			return fmt.Errorf("%w: %s: required dataset(s) missing beyond the identity datasets this refresh regenerates: %s",
				ErrCacheNotRefreshable, analyticsDir, strings.Join(nonIdentity, ", "))
		}
		return nil
	}
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	if err != nil {
		return fmt.Errorf("fingerprint committed analytics cache before identity refresh: %w", err)
	}
	if fingerprint != state.DatasetFingerprint {
		return fmt.Errorf("%w: %s: datasets do not match the committed fingerprint (modified, truncated, or deleted outside the ETL)",
			ErrCacheNotRefreshable, analyticsDir)
	}
	return nil
}

// datasetsMissingParquet returns the query.RequiredParquetDirs entries under
// analyticsDir that contain no Parquet file at any depth (covering the
// year=* partitioning of the messages dataset). A missing directory counts
// as missing, not as an error.
func datasetsMissingParquet(analyticsDir string) ([]string, error) {
	var missing []string
	for _, dataset := range query.RequiredParquetDirs {
		has, err := datasetHasParquetFile(filepath.Join(analyticsDir, dataset))
		if err != nil {
			return nil, fmt.Errorf("scan dataset %s: %w", dataset, err)
		}
		if !has {
			missing = append(missing, dataset)
		}
	}
	return missing, nil
}

func datasetHasParquetFile(root string) (bool, error) {
	found := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root && errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".parquet") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// ownerParticipantRow is one (source, participant) ownership edge: the
// source's confirmed account identity resolved to this participant.
type ownerParticipantRow struct {
	sourceID      int64
	participantID int64
}

// ownerParticipantsSQL matches every participant row that a confirmed
// account_identities address resolves to for its source, case-insensitively
// against either the durable participant email or an explicit
// participant_identifiers email, or verbatim against any non-email
// participant_identifiers row (phone, chat handle, etc. — identifiers are
// stored verbatim; only email comparisons fold case). Deliberately
// dialect-neutral (no bind placeholders, no dialect-specific functions) so
// it runs unchanged on SQLite and PostgreSQL. Kept byte-equivalent (apart
// from the ATTACH schema prefix) with the matching query in
// cmd/msgvault/cmd/build_cache.go.
const ownerParticipantsSQL = `
	SELECT DISTINCT ai.source_id, p.id AS participant_id
	FROM account_identities ai
	JOIN participants p
	  ON p.email_address IS NOT NULL AND lower(p.email_address) = lower(ai.address)
	UNION
	SELECT DISTINCT ai.source_id, pi.participant_id
	FROM account_identities ai
	JOIN participant_identifiers pi
	  ON (pi.identifier_type = 'email' AND lower(pi.identifier_value) = lower(ai.address))
	  OR (pi.identifier_type != 'email' AND pi.identifier_value = ai.address)
`

// ownerParticipantRows reads the current owner_participants edges through
// st's own connection.
func ownerParticipantRows(ctx context.Context, st *store.Store) ([]ownerParticipantRow, error) {
	rows, err := st.DB().QueryContext(ctx, ownerParticipantsSQL)
	if err != nil {
		return nil, fmt.Errorf("query owner participants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ownerParticipantRow
	for rows.Next() {
		var r ownerParticipantRow
		if err := rows.Scan(&r.sourceID, &r.participantID); err != nil {
			return nil, fmt.Errorf("scan owner participant row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// exportOwnerParticipants writes rows to dir/owner_participants.parquet via
// a DuckDB temp table, mirroring the participant_clusters export pattern:
// Go-computed data has no SQL source to COPY from directly, so it is
// staged through a temp table first. Always writes a file, even for zero
// rows, so the dataset directory required by query.RequiredParquetDirs
// always exists.
func exportOwnerParticipants(ctx context.Context, db *sql.DB, rows []ownerParticipantRow, dir string) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TEMP TABLE tmp_owner_participants (source_id BIGINT, participant_id BIGINT)`,
	); err != nil {
		return fmt.Errorf("create owner participants temp table: %w", err)
	}
	if len(rows) > 0 {
		values := make([]string, 0, len(rows))
		for _, r := range rows {
			values = append(values, fmt.Sprintf("(%d, %d)", r.sourceID, r.participantID))
		}
		insertSQL := "INSERT INTO tmp_owner_participants (source_id, participant_id) VALUES " +
			strings.Join(values, ", ")
		if _, err := db.ExecContext(ctx, insertSQL); err != nil {
			return fmt.Errorf("populate owner participants temp table: %w", err)
		}
	}
	escapedDir := strings.ReplaceAll(dir, "'", "''")
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
	COPY (
		SELECT source_id, participant_id FROM tmp_owner_participants
	) TO '%s/owner_participants.parquet' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, escapedDir)); err != nil {
		return fmt.Errorf("export owner participants: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE tmp_owner_participants`); err != nil {
		return fmt.Errorf("drop owner participants temp table: %w", err)
	}
	return nil
}

// exportParticipantClusters writes clusters (participant_id -> canonical
// cluster ID) to dir/participant_clusters.parquet via a DuckDB temp table,
// the same approach cmd/msgvault/cmd/build_cache.go uses for its full-build
// export: ParticipantClusters does graph traversal in Go, so it cannot be
// expressed as a COPY query directly.
func exportParticipantClusters(ctx context.Context, db *sql.DB, clusters map[int64]int64, dir string) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TEMP TABLE tmp_participant_clusters (participant_id BIGINT, canonical_id BIGINT)`,
	); err != nil {
		return fmt.Errorf("create participant clusters temp table: %w", err)
	}
	if len(clusters) > 0 {
		values := make([]string, 0, len(clusters))
		for participantID, canonicalID := range clusters {
			values = append(values, fmt.Sprintf("(%d, %d)", participantID, canonicalID))
		}
		insertSQL := "INSERT INTO tmp_participant_clusters (participant_id, canonical_id) VALUES " +
			strings.Join(values, ", ")
		if _, err := db.ExecContext(ctx, insertSQL); err != nil {
			return fmt.Errorf("populate participant clusters temp table: %w", err)
		}
	}
	escapedDir := strings.ReplaceAll(dir, "'", "''")
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
	COPY (
		SELECT participant_id, canonical_id FROM tmp_participant_clusters
	) TO '%s/participant_clusters.parquet' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, escapedDir)); err != nil {
		return fmt.Errorf("export participant clusters: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE tmp_participant_clusters`); err != nil {
		return fmt.Errorf("drop participant clusters temp table: %w", err)
	}
	return nil
}

// identityStaging is a same-parent staging directory for the two identity
// datasets, mirroring cmd/msgvault/cmd/cache_publication.go's cacheStaging
// convention (a dot-prefixed sibling of analyticsDir) without depending on
// that unexported type: cacheops cannot import cmd, since cmd imports
// cacheops.
type identityStaging struct {
	root string
}

func newIdentityStaging(analyticsDir string) (*identityStaging, error) {
	parent := filepath.Dir(filepath.Clean(analyticsDir))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create analytics cache parent: %w", err)
	}
	prefix := "." + filepath.Base(filepath.Clean(analyticsDir)) + ".identity-"
	root, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return nil, fmt.Errorf("create identity refresh staging directory: %w", err)
	}
	for _, dataset := range identityDatasets {
		if err := os.MkdirAll(filepath.Join(root, dataset), 0o755); err != nil {
			_ = os.RemoveAll(root)
			return nil, fmt.Errorf("create staged %s directory: %w", dataset, err)
		}
	}
	return &identityStaging{root: root}, nil
}

func (s *identityStaging) datasetDir(dataset string) string {
	return filepath.Join(s.root, dataset)
}

func (s *identityStaging) cleanup() error {
	if s == nil || s.root == "" {
		return nil
	}
	return os.RemoveAll(s.root)
}

// Fault-injection seams for publication tests, mirroring cmd's
// buildCacheWriteStateFile convention. Production code always leaves these
// at their os/query defaults.
var (
	identityPublishRename      = os.Rename
	identityPublishWriteFile   = os.WriteFile
	identityPublishFingerprint = query.CacheDatasetFingerprint
)

// identityDatasetBackup records where a live dataset directory was moved so
// a failed publish can restore it.
type identityDatasetBackup struct {
	live   string
	backup string
}

// publishIdentityDatasets replaces the live owner_participants and
// participant_clusters directories with their staged replacements and
// commits the updated identity revision. state is the CacheSyncState
// already read by RefreshIdentityDatasets; only IdentityRevision,
// PublishedAt, and DatasetFingerprint change.
//
// The previous committed publication stays recoverable until the very last
// step: live datasets are moved aside into staging as backups (never
// deleted), the existing _last_sync.json is left untouched while the
// datasets swap, and the new marker is staged to a sibling file and
// atomically renamed over the old one as the single commit point
// (os.Rename replaces an existing destination file on every supported
// platform, including Windows). A failure at any earlier step restores the
// backups, so the previous state — marker, datasets, and matching
// fingerprint — remains fully usable and a retry refreshes again instead
// of failing with ErrNoCommittedCache and forcing a full rebuild.
//
// Readers never observe the mid-swap window: callers hold the cache build
// lock exclusively (see RefreshIdentityDatasets) while readers acquire it
// shared. A reader that bypassed the lock would find the old marker's
// DatasetFingerprint disagreeing with the half-swapped files and classify
// the cache as drifted rather than serving mismatched data.
//
// state.AccountIdentityRevision is deliberately left untouched: this
// refresh only re-exports owner_participants/participant_clusters, never
// the message shards that bake in is_from_me, so it must not advance the
// marker that records those shards' freshness. Advancing it here would
// make a stale is_from_me look fresh and permanently suppress the full
// rebuild that HasAccountIdentityDrift is meant to force.
func publishIdentityDatasets(staging *identityStaging, analyticsDir string, state query.CacheSyncState, revision int64) error {
	backups, err := swapInStagedIdentityDatasets(staging, analyticsDir)
	if err != nil {
		return err
	}
	if err := commitIdentityState(staging, analyticsDir, state, revision); err != nil {
		return errors.Join(err, restoreIdentityBackups(backups))
	}
	return nil
}

// swapInStagedIdentityDatasets moves each live identity dataset aside into
// staging as a recoverable backup, then renames its staged replacement into
// place. On failure it restores every backup taken so far and returns the
// combined error. On success the backups stay under staging until the
// caller's deferred staging cleanup removes them after the marker commit.
func swapInStagedIdentityDatasets(staging *identityStaging, analyticsDir string) ([]identityDatasetBackup, error) {
	backupRoot := filepath.Join(staging.root, "backup")
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create identity publish backup directory: %w", err)
	}
	var backups []identityDatasetBackup
	for _, dataset := range identityDatasets {
		live := filepath.Join(analyticsDir, dataset)
		backup := filepath.Join(backupRoot, dataset)
		switch err := identityPublishRename(live, backup); {
		case err == nil:
			backups = append(backups, identityDatasetBackup{live: live, backup: backup})
		case os.IsNotExist(err):
			// A prior interrupted refresh can leave a committed marker with
			// this dataset directory missing; there is nothing to back up.
		default:
			return nil, errors.Join(
				fmt.Errorf("back up live %s dataset: %w", dataset, err),
				restoreIdentityBackups(backups),
			)
		}
		if err := identityPublishRename(staging.datasetDir(dataset), live); err != nil {
			return nil, errors.Join(
				fmt.Errorf("publish %s dataset: %w", dataset, err),
				restoreIdentityBackups(backups),
			)
		}
	}
	return backups, nil
}

// restoreIdentityBackups puts backed-up live datasets back after a failed
// publish so the previous committed publication remains fully usable. It
// calls os.Rename directly rather than the injectable seam so a test's
// injected fault cannot also break recovery.
func restoreIdentityBackups(backups []identityDatasetBackup) error {
	var errs []error
	for _, b := range backups {
		if err := os.RemoveAll(b.live); err != nil {
			errs = append(errs, fmt.Errorf("remove partially published dataset %s: %w", b.live, err))
			continue
		}
		if err := os.Rename(b.backup, b.live); err != nil {
			errs = append(errs, fmt.Errorf("restore backed-up dataset %s: %w", b.live, err))
		}
	}
	return errors.Join(errs...)
}

// commitIdentityState fingerprints the swapped-in datasets, stages the
// updated commit marker inside staging, and atomically renames it over
// _last_sync.json — the publication's single commit point. The marker is
// never absent or truncated on disk: readers see either the previous
// committed state or the new one.
func commitIdentityState(staging *identityStaging, analyticsDir string, state query.CacheSyncState, revision int64) error {
	fingerprint, err := identityPublishFingerprint(analyticsDir)
	if err != nil {
		return fmt.Errorf("fingerprint published analytics cache: %w", err)
	}
	state.IdentityRevision = revision
	state.PublishedAt = time.Now().UTC()
	state.DatasetFingerprint = fingerprint
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode cache sync state: %w", err)
	}
	stagedPath := filepath.Join(staging.root, "_last_sync.json")
	if err := identityPublishWriteFile(stagedPath, data, 0o600); err != nil {
		return fmt.Errorf("stage cache sync state: %w", err)
	}
	if err := identityPublishRename(stagedPath, query.CacheStatePath(analyticsDir)); err != nil {
		return fmt.Errorf("commit cache sync state: %w", err)
	}
	return nil
}
