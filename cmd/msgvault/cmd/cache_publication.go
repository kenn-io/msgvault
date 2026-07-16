package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/query"
)

var buildCacheAfterStateInvalidationHook func() error

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

func replacesCacheDataset(dataset string, replaceAll bool) bool {
	if replaceAll {
		return true
	}
	switch dataset {
	case "participants", "labels", "sources", "conversations":
		return true
	default:
		return false
	}
}

func planCacheMoves(
	staging *cacheStaging,
	analyticsDir string,
	replaceAll bool,
) ([]cachePublishMove, error) {
	if staging == nil || staging.root == "" || staging.buildID == "" {
		return nil, errors.New("plan analytics cache publication: invalid staging directory")
	}
	var moves []cachePublishMove
	for _, dataset := range query.RequiredParquetDirs {
		stagedDataset := filepath.Join(staging.root, dataset)
		liveDataset := filepath.Join(analyticsDir, dataset)
		if replacesCacheDataset(dataset, replaceAll) {
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

func publishCache(staging *cacheStaging, analyticsDir string, replaceAll bool, stateData []byte) error {
	moves, err := planCacheMoves(staging, analyticsDir, replaceAll)
	if err != nil {
		return err
	}
	statePath := query.CacheStatePath(analyticsDir)
	if err := invalidateSyncStateFile(statePath); err != nil {
		return err
	}
	if buildCacheAfterStateInvalidationHook != nil {
		if err := buildCacheAfterStateInvalidationHook(); err != nil {
			return err
		}
	}
	for _, move := range moves {
		if move.replace {
			if err := os.RemoveAll(move.destination); err != nil {
				return fmt.Errorf("remove live cache dataset %s: %w", move.destination, err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(move.destination), 0o755); err != nil {
			return fmt.Errorf("create cache publication directory: %w", err)
		}
		if err := os.Rename(move.source, move.destination); err != nil {
			return fmt.Errorf("publish cache path %s: %w", move.destination, err)
		}
	}
	if err := buildCacheWriteStateFile(statePath, stateData, 0o600); err != nil {
		return fmt.Errorf("save cache sync state to %s: %w", statePath, err)
	}
	return nil
}
