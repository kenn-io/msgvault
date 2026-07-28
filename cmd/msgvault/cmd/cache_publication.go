package cmd

import (
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

type appliedCacheMove struct {
	move      cachePublishMove
	backup    string
	hadBackup bool
}

type cachePublicationTransaction struct {
	backupRoot string
	applied    []appliedCacheMove
}

func beginCachePublicationTransaction(staging *cacheStaging) (*cachePublicationTransaction, error) {
	backupRoot := filepath.Join(staging.root, "publication-backup")
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create cache publication backup: %w", err)
	}
	return &cachePublicationTransaction{backupRoot: backupRoot}, nil
}

func (t *cachePublicationTransaction) apply(moves []cachePublishMove) error {
	for i, move := range moves {
		applied := appliedCacheMove{move: move}
		if move.replace {
			applied.backup = filepath.Join(
				t.backupRoot,
				fmt.Sprintf("%04d-%s", i, filepath.Base(move.destination)),
			)
			if _, err := os.Lstat(move.destination); err == nil {
				if err := os.Rename(move.destination, applied.backup); err != nil {
					return fmt.Errorf(
						"back up live cache dataset %s: %w",
						filepath.Base(move.destination),
						err,
					)
				}
				applied.hadBackup = true
			} else if !os.IsNotExist(err) {
				return fmt.Errorf(
					"inspect live cache dataset %s: %w",
					filepath.Base(move.destination),
					err,
				)
			}
			t.applied = append(t.applied, applied)
		}
		if err := os.MkdirAll(filepath.Dir(move.destination), 0o755); err != nil {
			return fmt.Errorf("create cache publication directory: %w", err)
		}
		if err := os.Rename(move.source, move.destination); err != nil {
			return fmt.Errorf("publish cache path %s: %w", move.destination, err)
		}
		if !move.replace {
			t.applied = append(t.applied, applied)
		}
	}
	return nil
}

func (t *cachePublicationTransaction) rollback() error {
	var rollbackErrs []error
	for _, applied := range slices.Backward(t.applied) {
		if applied.move.replace {
			if err := os.RemoveAll(applied.move.destination); err != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf(
					"remove uncommitted cache dataset %s: %w",
					filepath.Base(applied.move.destination),
					err,
				))
				continue
			}
			if applied.hadBackup {
				if err := os.Rename(applied.backup, applied.move.destination); err != nil {
					rollbackErrs = append(rollbackErrs, fmt.Errorf(
						"restore cache dataset %s: %w",
						filepath.Base(applied.move.destination),
						err,
					))
				}
			}
			continue
		}
		if err := os.Remove(applied.move.destination); err != nil && !os.IsNotExist(err) {
			rollbackErrs = append(rollbackErrs, fmt.Errorf(
				"remove uncommitted cache append %s: %w",
				applied.move.destination,
				err,
			))
		}
	}
	return errors.Join(rollbackErrs...)
}

func publishCache(
	staging *cacheStaging,
	analyticsDir string,
	plan cachePublishPlan,
	stateData []byte,
) error {
	moves, err := planCacheMoves(staging, analyticsDir, plan)
	if err != nil {
		return err
	}
	transaction, err := beginCachePublicationTransaction(staging)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		return errors.Join(err, transaction.rollback())
	}
	if buildCacheBeforePublicationMovesHook != nil {
		if err := buildCacheBeforePublicationMovesHook(); err != nil {
			return err
		}
	}
	if err := transaction.apply(moves); err != nil {
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
	stagedStatePath := filepath.Join(staging.root, "_last_sync.json")
	if err := buildCacheWriteStateFile(stagedStatePath, stateData, 0o600); err != nil {
		return fail(fmt.Errorf("save cache sync state to staged marker: %w", err))
	}
	statePath := query.CacheStatePath(analyticsDir)
	if err := os.Rename(stagedStatePath, statePath); err != nil {
		return fail(fmt.Errorf("commit cache sync state to %s: %w", statePath, err))
	}
	return nil
}
