package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
)

var buildCacheBeforePublicationMovesHook func() error
var cachePublicationAfterMoveHook func(string) error

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
	if err := os.WriteFile(
		filepath.Join(root, ".builder-pid"),
		[]byte(strconv.Itoa(os.Getpid())),
		0o600,
	); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("record analytics cache builder process: %w", err)
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
		root := filepath.Join(parent, entry.Name())
		pidData, readErr := os.ReadFile(filepath.Join(root, ".builder-pid"))
		if readErr == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if parseErr == nil && daemon.ProcessAlive(pid) {
				continue
			}
		}
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf(
				"remove abandoned analytics cache staging directory %s: %w",
				entry.Name(),
				err,
			)
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
	dataset     string
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
		identityindex.DatasetActivity,
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
		identityindex.DatasetPeople,
		identityindex.DatasetDomains,
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
			info, err := os.Stat(stagedDataset)
			if err != nil {
				return nil, fmt.Errorf("plan analytics cache publication for %s: %w", dataset, err)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf(
					"plan analytics cache publication for %s: staging path is not a directory",
					dataset,
				)
			}
			moves = append(moves, cachePublishMove{
				source: stagedDataset, destination: liveDataset,
				dataset: dataset, replace: true,
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
			destination := filepath.Join(
				liveDataset,
				filepath.Dir(rel),
				staging.buildID+"-"+filepath.Base(rel),
			)
			if _, err := os.Lstat(destination); err == nil {
				return fmt.Errorf(
					"analytics cache publication destination already exists: %s",
					destination,
				)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf(
					"inspect analytics cache publication destination %s: %w",
					destination,
					err,
				)
			}
			moves = append(moves, cachePublishMove{
				source: path, destination: destination, dataset: dataset,
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("plan analytics cache publication for %s: %w", dataset, err)
		}
	}
	return moves, nil
}

func publishCache(
	staging *cacheStaging,
	analyticsDir string,
	plan cachePublishPlan,
	stateData []byte,
) error {
	return publishCacheWithBeforeMarker(staging, analyticsDir, plan, stateData, nil)
}

func publishCacheWithBeforeMarker(
	staging *cacheStaging,
	analyticsDir string,
	plan cachePublishPlan,
	stateData []byte,
	beforeMarker func() error,
) error {
	moves, err := planCacheMoves(staging, analyticsDir, plan)
	if err != nil {
		return err
	}
	if buildCacheBeforePublicationMovesHook != nil {
		if err := buildCacheBeforePublicationMovesHook(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(analyticsDir, 0o755); err != nil {
		return fmt.Errorf("create analytics cache root: %w", err)
	}
	for _, move := range moves {
		if move.replace {
			if err := os.RemoveAll(move.destination); err != nil {
				return fmt.Errorf("replace cache dataset %s: %w", move.dataset, err)
			}
		} else if err := os.MkdirAll(filepath.Dir(move.destination), 0o755); err != nil {
			return fmt.Errorf("create cache append directory for %s: %w", move.dataset, err)
		}
		if err := os.Rename(move.source, move.destination); err != nil {
			return fmt.Errorf("publish cache dataset %s: %w", move.dataset, err)
		}
		if err := syncDirectory(filepath.Dir(move.destination)); err != nil {
			return fmt.Errorf("sync published cache dataset %s: %w", move.dataset, err)
		}
		if cachePublicationAfterMoveHook != nil {
			if err := cachePublicationAfterMoveHook(move.dataset); err != nil {
				return err
			}
		}
	}
	if beforeMarker != nil {
		if err := beforeMarker(); err != nil {
			return err
		}
	}
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	if err != nil {
		return fmt.Errorf("fingerprint published analytics cache: %w", err)
	}
	var state query.CacheSyncState
	if err := json.Unmarshal(stateData, &state); err != nil {
		return fmt.Errorf("decode cache sync state for publication: %w", err)
	}
	state.PublishedAt = time.Now().UTC()
	state.DatasetFingerprint = fingerprint
	stateData, err = json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode committed cache sync state: %w", err)
	}
	return commitCacheMarker(analyticsDir, staging.buildID, stateData)
}

func commitCacheMarker(analyticsDir, buildID string, stateData []byte) error {
	statePath := query.CacheStatePath(analyticsDir)
	tempPath := filepath.Join(analyticsDir, ".last-sync-"+buildID+".tmp")
	if err := buildCacheWriteStateFile(tempPath, stateData, 0o600); err != nil {
		return fmt.Errorf("write staged cache marker: %w", err)
	}
	defer func() { _ = os.Remove(tempPath) }()
	if err := syncFile(tempPath); err != nil {
		return fmt.Errorf("sync staged cache marker: %w", err)
	}
	if err := os.Rename(tempPath, statePath); err != nil {
		return fmt.Errorf("commit cache marker: %w", err)
	}
	if err := syncDirectory(analyticsDir); err != nil {
		return fmt.Errorf("sync committed cache marker: %w", err)
	}
	return nil
}
