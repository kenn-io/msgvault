package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
)

var buildCacheBeforePublicationMovesHook func() error
var cachePublicationCheckpointHook func(string)

type cacheStaging struct {
	root    string
	buildID string
}

func cacheStagingPrefix(analyticsDir string) string {
	return "." + filepath.Base(filepath.Clean(analyticsDir)) + ".build-"
}

func newCacheStaging(analyticsDir string) (*cacheStaging, error) {
	parent := filepath.Dir(filepath.Clean(analyticsDir))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create analytics cache parent: %w", err)
	}
	prefix := cacheStagingPrefix(analyticsDir)
	root, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return nil, fmt.Errorf("create analytics cache staging directory: %w", err)
	}
	buildID := strings.TrimPrefix(filepath.Base(root), prefix)
	if buildID == "" {
		// #nosec G703 -- root is the exact private directory returned by os.MkdirTemp above.
		_ = os.RemoveAll(root)
		return nil, errors.New("create analytics cache staging directory: empty build ID")
	}
	return &cacheStaging{root: root, buildID: buildID}, nil
}

func cleanupStaleCacheStaging(analyticsDir string) error {
	parent := filepath.Dir(filepath.Clean(analyticsDir))
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list analytics cache staging directories: %w", err)
	}
	prefix := cacheStagingPrefix(analyticsDir)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(parent, entry.Name())); err != nil {
			return fmt.Errorf("remove abandoned analytics cache staging directory %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (s *cacheStaging) cleanup() error {
	if s == nil || s.root == "" {
		return nil
	}
	return os.RemoveAll(s.root)
}

type cachePublishMove struct {
	source      string
	destination string
	replace     bool
}

type cachePublishPlan struct {
	Append  map[string]bool
	Replace map[string]bool
}

func cachePublishPlanForMode(replaceAll bool) cachePublishPlan {
	plan := cachePublishPlan{
		Append:  make(map[string]bool),
		Replace: make(map[string]bool),
	}
	if replaceAll {
		for _, dataset := range query.RequiredParquetDirs {
			plan.Replace[dataset] = true
		}
		return plan
	}
	for _, dataset := range []string{
		tableMessages,
		"message_recipients",
		"message_labels",
		tableAttachments,
		identityindex.DatasetEntryFacts,
		identityindex.DatasetDirectEdges,
	} {
		plan.Append[dataset] = true
	}
	for _, dataset := range []string{
		tableParticipants,
		tableParticipantIdentifiers,
		tableLabels,
		"sources",
		tableConversations,
		tableConversationParticipants,
		tableOwnerParticipants,
		tableParticipantClusters,
		identityindex.DatasetConversationEdges,
		identityindex.DatasetDirectory,
		identityindex.DatasetRollups,
		identityindex.DatasetDomainRollups,
		identityindex.DatasetRelationships,
		identityindex.DatasetRelationshipDaily,
	} {
		plan.Replace[dataset] = true
	}
	return plan
}

func (p cachePublishPlan) datasets() []string {
	datasets := make([]string, 0, len(p.Append)+len(p.Replace))
	for _, dataset := range query.RequiredParquetDirs {
		if p.Append[dataset] || p.Replace[dataset] {
			datasets = append(datasets, dataset)
		}
	}
	return datasets
}

func planCacheMoves(
	staging *cacheStaging,
	analyticsDir string,
	plan cachePublishPlan,
) ([]cachePublishMove, error) {
	if staging == nil || staging.root == "" || staging.buildID == "" {
		return nil, errors.New("plan analytics cache publication: invalid staging directory")
	}
	var moves []cachePublishMove
	for _, dataset := range plan.datasets() {
		stagedDataset := filepath.Join(staging.root, dataset)
		liveDataset := filepath.Join(analyticsDir, dataset)
		if plan.Replace[dataset] {
			if info, err := os.Stat(stagedDataset); err != nil {
				return nil, fmt.Errorf("plan analytics cache publication for %s: %w", dataset, err)
			} else if !info.IsDir() {
				return nil, fmt.Errorf("plan analytics cache publication for %s: staging path is not a directory", dataset)
			}
			moves = append(moves, cachePublishMove{
				source: stagedDataset, destination: liveDataset, replace: true,
			})
			continue
		}

		if _, err := os.Stat(stagedDataset); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("inspect staged analytics cache dataset %s: %w", dataset, err)
		}
		err := filepath.WalkDir(stagedDataset, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".parquet") {
				return nil
			}
			rel, err := filepath.Rel(stagedDataset, path)
			if err != nil {
				return err
			}
			destination := filepath.Join(liveDataset, filepath.Dir(rel),
				staging.buildID+"-"+filepath.Base(rel))
			// #nosec G703 -- destination is rooted in analyticsDir and rel came from WalkDir under stagedDataset.
			if _, err := os.Lstat(destination); err == nil {
				return fmt.Errorf("analytics cache publication destination already exists: %s", destination)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect analytics cache publication destination %s: %w", destination, err)
			}
			moves = append(moves, cachePublishMove{
				source: path, destination: destination,
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("plan analytics cache publication for %s: %w", dataset, err)
		}
	}
	return moves, nil
}

const cachePublicationJournalVersion = 1

type durableCachePublishMove struct {
	Source         string `json:"source"`
	Destination    string `json:"destination"`
	Backup         string `json:"backup,omitempty"`
	Replace        bool   `json:"replace"`
	HadDestination bool   `json:"had_destination"`
}

type cachePublicationJournal struct {
	Version          int                       `json:"version"`
	AnalyticsDir     string                    `json:"analytics_dir"`
	StagingRoot      string                    `json:"staging_root"`
	Phase            string                    `json:"phase"`
	AppliedMoves     int                       `json:"applied_moves"`
	Moves            []durableCachePublishMove `json:"moves"`
	OldMarkerPresent bool                      `json:"old_marker_present"`
	OldMarkerDigest  string                    `json:"old_marker_digest,omitempty"`
	NewMarkerDigest  string                    `json:"new_marker_digest,omitempty"`
}

type cachePublicationTransaction struct {
	root    string
	journal cachePublicationJournal
}

func cachePublicationTransactionRoot(analyticsDir string) string {
	clean := filepath.Clean(analyticsDir)
	return filepath.Join(
		filepath.Dir(clean),
		"."+filepath.Base(clean)+".publication",
	)
}

func beginCachePublicationTransaction(
	staging *cacheStaging,
	analyticsDir string,
	moves []cachePublishMove,
) (*cachePublicationTransaction, error) {
	root := cachePublicationTransactionRoot(analyticsDir)
	if err := os.Mkdir(root, 0o700); err != nil {
		return nil, fmt.Errorf("create cache publication backup: %w", err)
	}
	if err := syncDirectory(filepath.Dir(root)); err != nil {
		return nil, fmt.Errorf("sync cache publication parent: %w", err)
	}
	backupRoot := filepath.Join(root, "backup")
	if err := os.Mkdir(backupRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create cache publication backup: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		return nil, fmt.Errorf("sync cache publication transaction: %w", err)
	}
	transaction := &cachePublicationTransaction{
		root: root,
		journal: cachePublicationJournal{
			Version:      cachePublicationJournalVersion,
			AnalyticsDir: filepath.Clean(analyticsDir),
			StagingRoot:  filepath.Clean(staging.root),
			Phase:        "prepared",
			Moves:        make([]durableCachePublishMove, 0, len(moves)),
		},
	}
	for index, move := range moves {
		durableMove := durableCachePublishMove{
			Source:      filepath.Clean(move.source),
			Destination: filepath.Clean(move.destination),
			Replace:     move.replace,
		}
		if move.replace {
			durableMove.Backup = filepath.Join(
				backupRoot,
				fmt.Sprintf("%04d-%s", index, filepath.Base(move.destination)),
			)
			if _, err := os.Lstat(move.destination); err == nil {
				durableMove.HadDestination = true
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf(
					"inspect live cache dataset %s: %w",
					filepath.Base(move.destination),
					err,
				)
			}
		}
		transaction.journal.Moves = append(transaction.journal.Moves, durableMove)
	}
	oldMarker := query.CacheStatePath(analyticsDir)
	// #nosec G703 -- oldMarker is the fixed cache marker below the configured analytics directory.
	if data, err := os.ReadFile(oldMarker); err == nil {
		transaction.journal.OldMarkerPresent = true
		transaction.journal.OldMarkerDigest = cachePublicationDigest(data)
		if err := writeDurableFile(
			filepath.Join(root, "old-marker.json"),
			data,
			0o600,
		); err != nil {
			return nil, fmt.Errorf("preserve old cache publication marker: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read old cache publication marker: %w", err)
	}
	if err := transaction.writeJournal(); err != nil {
		return nil, err
	}
	cachePublicationCheckpoint("journal-prepared")
	return transaction, nil
}

func (t *cachePublicationTransaction) apply() error {
	for index := range t.journal.Moves {
		move := &t.journal.Moves[index]
		if move.Replace {
			if move.HadDestination {
				// #nosec G703 -- beginCachePublicationTransaction constructed both paths inside configured roots.
				if err := os.Rename(move.Destination, move.Backup); err != nil {
					return fmt.Errorf(
						"back up live cache dataset %s: %w",
						filepath.Base(move.Destination),
						err,
					)
				}
				if err := syncRenameDirectories(move.Destination, move.Backup); err != nil {
					return err
				}
				cachePublicationCheckpoint("backup-moved")
			}
		}
		// #nosec G703 -- move.Destination was constructed below the configured analytics root.
		if err := os.MkdirAll(filepath.Dir(move.Destination), 0o755); err != nil {
			return fmt.Errorf("create cache publication directory: %w", err)
		}
		// #nosec G703 -- beginCachePublicationTransaction constructed both paths inside configured roots.
		if err := os.Rename(move.Source, move.Destination); err != nil {
			return fmt.Errorf("publish cache path %s: %w", move.Destination, err)
		}
		if err := syncRenameDirectories(move.Source, move.Destination); err != nil {
			return err
		}
		cachePublicationCheckpoint("data-moved")
		t.journal.AppliedMoves = index + 1
		t.journal.Phase = "moving"
		if err := t.writeJournal(); err != nil {
			return err
		}
	}
	t.journal.Phase = "data-published"
	if err := t.writeJournal(); err != nil {
		return err
	}
	return nil
}

func (t *cachePublicationTransaction) rollback() error {
	return recoverInterruptedCachePublication(t.journal.AnalyticsDir)
}

func (t *cachePublicationTransaction) commitMarker(data []byte) error {
	newMarker := filepath.Join(t.root, "new-marker.json")
	if err := buildCacheWriteStateFile(newMarker, data, 0o600); err != nil {
		return fmt.Errorf("save cache sync state to staged marker: %w", err)
	}
	if err := syncFile(newMarker); err != nil {
		return fmt.Errorf("sync staged cache marker: %w", err)
	}
	if err := syncDirectory(t.root); err != nil {
		return fmt.Errorf("sync staged cache marker directory: %w", err)
	}
	t.journal.NewMarkerDigest = cachePublicationDigest(data)
	t.journal.Phase = "marker-prepared"
	if err := t.writeJournal(); err != nil {
		return err
	}
	cachePublicationCheckpoint("marker-prepared")
	statePath := query.CacheStatePath(t.journal.AnalyticsDir)
	// #nosec G703 -- both paths are fixed marker names inside validated transaction/cache roots.
	if err := os.Rename(newMarker, statePath); err != nil {
		return fmt.Errorf("commit cache sync state to %s: %w", statePath, err)
	}
	if err := syncRenameDirectories(newMarker, statePath); err != nil {
		return err
	}
	cachePublicationCheckpoint("marker-committed")
	t.journal.Phase = "marker-committed"
	if err := t.writeJournal(); err != nil {
		return err
	}
	return t.finalize()
}

func (t *cachePublicationTransaction) writeJournal() error {
	data, err := json.Marshal(t.journal)
	if err != nil {
		return fmt.Errorf("encode cache publication journal: %w", err)
	}
	path := filepath.Join(t.root, "journal.json")
	tempPath := filepath.Join(t.root, "journal.tmp")
	if err := writeDurableFile(tempPath, data, 0o600); err != nil {
		return fmt.Errorf("write cache publication journal: %w", err)
	}
	// #nosec G703 -- both paths are fixed journal names inside the private transaction root.
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("commit cache publication journal: %w", err)
	}
	if err := syncDirectory(t.root); err != nil {
		return fmt.Errorf("sync cache publication journal: %w", err)
	}
	return nil
}

func (t *cachePublicationTransaction) finalize() error {
	parent := filepath.Dir(t.root)
	for _, path := range []string{
		filepath.Join(t.root, "backup"),
		filepath.Join(t.root, "old-marker.json"),
		filepath.Join(t.root, "new-marker.json"),
		filepath.Join(t.root, "journal.tmp"),
	} {
		// #nosec G703 -- every path is a fixed child of the private transaction root.
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove completed cache publication artifact: %w", err)
		}
	}
	if err := syncDirectory(t.root); err != nil {
		return fmt.Errorf("sync completed cache publication transaction: %w", err)
	}
	// #nosec G703 -- journal.json is a fixed child of the private transaction root.
	if err := os.Remove(filepath.Join(t.root, "journal.json")); err != nil &&
		!os.IsNotExist(err) {
		return fmt.Errorf("remove completed cache publication journal: %w", err)
	}
	if err := syncDirectory(t.root); err != nil {
		return fmt.Errorf("sync removed cache publication journal: %w", err)
	}
	// #nosec G703 -- t.root is the deterministic private transaction directory.
	if err := os.Remove(t.root); err != nil {
		return fmt.Errorf("remove completed cache publication transaction: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync completed cache publication parent: %w", err)
	}
	return nil
}

func publishCache(
	staging *cacheStaging,
	analyticsDir string,
	plan cachePublishPlan,
	stateData []byte,
) error {
	if err := recoverInterruptedCachePublication(analyticsDir); err != nil {
		return err
	}
	moves, err := planCacheMoves(staging, analyticsDir, plan)
	if err != nil {
		return err
	}
	transaction, err := beginCachePublicationTransaction(staging, analyticsDir, moves)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		return errors.Join(err, transaction.rollback())
	}
	if buildCacheBeforePublicationMovesHook != nil {
		if err := buildCacheBeforePublicationMovesHook(); err != nil {
			return fail(err)
		}
	}
	if err := transaction.apply(); err != nil {
		return fail(err)
	}
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	if err != nil {
		return fail(fmt.Errorf("fingerprint published analytics cache: %w", err))
	}
	var state query.CacheSyncState
	if err := json.Unmarshal(stateData, &state); err != nil {
		return fail(fmt.Errorf("decode cache sync state for publication: %w", err))
	}
	state.PublishedAt = time.Now().UTC()
	state.DatasetFingerprint = fingerprint
	stateData, err = json.Marshal(state)
	if err != nil {
		return fail(fmt.Errorf("encode committed cache sync state: %w", err))
	}
	if err := transaction.commitMarker(stateData); err != nil {
		return fail(err)
	}
	return nil
}

func recoverInterruptedCachePublication(analyticsDir string) error {
	root := cachePublicationTransactionRoot(analyticsDir)
	data, err := os.ReadFile(filepath.Join(root, "journal.json"))
	if os.IsNotExist(err) {
		if _, statErr := os.Stat(root); os.IsNotExist(statErr) {
			return nil
		} else if statErr != nil {
			return fmt.Errorf("inspect cache publication transaction: %w", statErr)
		}
		backupEntries, readErr := os.ReadDir(filepath.Join(root, "backup"))
		if readErr == nil && len(backupEntries) > 0 {
			return errors.New("recover cache publication: backups exist without a durable journal")
		}
		if readErr != nil && !os.IsNotExist(readErr) {
			return fmt.Errorf("inspect unjournaled cache publication backup: %w", readErr)
		}
		if removeErr := os.RemoveAll(root); removeErr != nil {
			return fmt.Errorf("remove unjournaled cache publication transaction: %w", removeErr)
		}
		return syncDirectory(filepath.Dir(root))
	}
	if err != nil {
		return fmt.Errorf("read cache publication journal: %w", err)
	}
	var journal cachePublicationJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return fmt.Errorf("decode cache publication journal: %w", err)
	}
	if journal.Version != cachePublicationJournalVersion ||
		filepath.Clean(journal.AnalyticsDir) != filepath.Clean(analyticsDir) {
		return errors.New("recover cache publication: invalid journal identity")
	}
	for _, move := range journal.Moves {
		if !pathWithin(journal.StagingRoot, move.Source) ||
			!pathWithin(analyticsDir, move.Destination) ||
			(move.Replace && !pathWithin(filepath.Join(root, "backup"), move.Backup)) {
			return errors.New("recover cache publication: journal move escapes transaction roots")
		}
	}
	transaction := &cachePublicationTransaction{root: root, journal: journal}
	if journal.NewMarkerDigest != "" {
		// #nosec G703 -- analyticsDir was checked against the durable journal identity above.
		liveMarker, markerErr := os.ReadFile(query.CacheStatePath(analyticsDir))
		if markerErr == nil &&
			cachePublicationDigest(liveMarker) == journal.NewMarkerDigest {
			return transaction.finalize()
		}
		if markerErr != nil && !os.IsNotExist(markerErr) {
			return fmt.Errorf("read live cache marker during recovery: %w", markerErr)
		}
	}

	var recoveryErrs []error
	for _, v := range slices.Backward(journal.Moves) {
		move := v
		if move.Replace {
			_, backupErr := os.Lstat(move.Backup)
			switch {
			case backupErr == nil:
				if err := os.RemoveAll(move.Destination); err != nil {
					recoveryErrs = append(recoveryErrs, fmt.Errorf(
						"remove uncommitted cache dataset %s: %w",
						filepath.Base(move.Destination),
						err,
					))
					continue
				}
				if err := os.Rename(move.Backup, move.Destination); err != nil {
					recoveryErrs = append(recoveryErrs, fmt.Errorf(
						"restore cache dataset %s: %w",
						filepath.Base(move.Destination),
						err,
					))
					continue
				}
				if err := syncRenameDirectories(move.Backup, move.Destination); err != nil {
					recoveryErrs = append(recoveryErrs, err)
				}
			case os.IsNotExist(backupErr):
				if !move.HadDestination {
					if err := os.RemoveAll(move.Destination); err != nil {
						recoveryErrs = append(recoveryErrs, err)
					}
				}
			default:
				recoveryErrs = append(recoveryErrs, backupErr)
			}
			continue
		}
		if err := os.Remove(move.Destination); err != nil && !os.IsNotExist(err) {
			recoveryErrs = append(recoveryErrs, fmt.Errorf(
				"remove uncommitted cache append %s: %w",
				move.Destination,
				err,
			))
		} else if err == nil {
			if syncErr := syncDirectory(filepath.Dir(move.Destination)); syncErr != nil {
				recoveryErrs = append(recoveryErrs, syncErr)
			}
		}
	}
	if err := errors.Join(recoveryErrs...); err != nil {
		return err
	}
	if journal.OldMarkerPresent {
		oldMarker, err := os.ReadFile(filepath.Join(root, "old-marker.json"))
		if err != nil {
			return fmt.Errorf("read preserved cache marker: %w", err)
		}
		if cachePublicationDigest(oldMarker) != journal.OldMarkerDigest {
			return errors.New("recover cache publication: preserved marker digest mismatch")
		}
		if err := writeDurableFile(
			filepath.Join(analyticsDir, ".last-sync-recovery.tmp"),
			oldMarker,
			0o600,
		); err != nil {
			return err
		}
		tempMarker := filepath.Join(analyticsDir, ".last-sync-recovery.tmp")
		// #nosec G703 -- both paths are fixed marker names inside the validated analytics root.
		if err := os.Rename(tempMarker, query.CacheStatePath(analyticsDir)); err != nil {
			return fmt.Errorf("restore cache publication marker: %w", err)
		}
		if err := syncRenameDirectories(
			tempMarker,
			query.CacheStatePath(analyticsDir),
		); err != nil {
			return err
		}
		// #nosec G703 -- the marker is a fixed filename inside the validated analytics root.
	} else if err := os.Remove(query.CacheStatePath(analyticsDir)); err != nil &&
		!os.IsNotExist(err) {
		return fmt.Errorf("remove uncommitted cache marker: %w", err)
	}
	return transaction.finalize()
}

func cachePublicationCheckpoint(phase string) {
	if cachePublicationCheckpointHook != nil {
		cachePublicationCheckpointHook(phase)
	}
}

func cachePublicationDigest(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func writeDurableFile(path string, data []byte, mode os.FileMode) error {
	// #nosec G703 -- callers construct path from fixed filenames below private transaction/cache roots.
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	if err := syncFile(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncFile(path string) error {
	// #nosec G703 -- callers pass fixed files inside private transaction/cache roots.
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}

func syncDirectory(path string) error {
	// #nosec G703 -- callers pass configured cache parents or validated transaction paths.
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func syncRenameDirectories(oldPath, newPath string) error {
	oldDir := filepath.Dir(oldPath)
	newDir := filepath.Dir(newPath)
	if err := syncDirectory(oldDir); err != nil {
		return fmt.Errorf("sync cache publication source directory: %w", err)
	}
	if newDir != oldDir {
		if err := syncDirectory(newDir); err != nil {
			return fmt.Errorf("sync cache publication destination directory: %w", err)
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil &&
		relative != "." &&
		relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
