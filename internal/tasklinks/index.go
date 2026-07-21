package tasklinks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/taskclient"
)

const (
	DefaultStaleAfter = 15 * time.Minute
	HardMaxTasks      = 5000
	MaxCacheFileBytes = 8 << 20
	indexFormatV1     = 1
)

type IndexState string

const (
	StateReady                  IndexState = "ready"
	StatePartial                IndexState = "partial"
	StateStale                  IndexState = "stale"
	StateAuthenticationRequired IndexState = "authentication_required"
	StateUnavailable            IndexState = "unavailable"
	StateDisabled               IndexState = "disabled"
	StateNotFound               IndexState = "not_found"
	StateWrongProject           IndexState = "wrong_project"
	StateIncompatible           IndexState = "incompatible"
)

const (
	ReasonSafetyLimit                 = "safety_limit"
	ReasonInterrupted                 = "interrupted"
	ReasonUnavailable                 = "daemon_unavailable"
	ReasonCursorCycle                 = "cursor_cycle"
	ReasonEmptyPage                   = "empty_page_with_cursor"
	ReasonArchiveUID                  = "archive_uid_mismatch"
	ReasonArchiveRevision             = "archive_revision_mismatch"
	ReasonProjectMismatch             = "project_mismatch"
	ReasonCacheFormat                 = "cache_format_incompatible"
	ReasonPersistenceFailure          = "persistence_failure"
	ReasonCachePersistenceUnsupported = "cache_persistence_unsupported"
	ReasonIncompatibleMetadata        = "incompatible_metadata"
)

var ErrDiskCacheSecurityUnsupported = errors.New("secure task reverse-index disk persistence is unsupported on this platform")

type CacheIdentity struct {
	Project         string `json:"project"`
	ArchiveUID      string `json:"archive_uid"`
	ArchiveRevision string `json:"archive_revision"`
}

type TaskSummary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Revision string `json:"revision,omitempty"`
}

type IndexStatus struct {
	State          IndexState `json:"state"`
	Complete       bool       `json:"complete"`
	LastScan       time.Time  `json:"last_scan"`
	RemoteRevision string     `json:"remote_revision,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}

type LookupResult struct {
	IndexStatus

	Tasks []TaskSummary `json:"tasks"`
}

type indexEntry struct {
	Task  TaskSummary `json:"task"`
	Links []MailLink  `json:"links"`
}

type indexFile struct {
	FormatVersion int           `json:"format_version"`
	Identity      CacheIdentity `json:"identity"`
	Status        IndexStatus   `json:"status"`
	Entries       []indexEntry  `json:"entries"`
}

type ListClient interface {
	ListTasks(ctx context.Context, project string, limit int, cursor string) (taskclient.TaskList, error)
}

type Index struct {
	path              string
	now               func() time.Time
	persistencePolicy func() error
	data              indexFile
}

func NewIndex(path string, now func() time.Time) *Index {
	return NewIndexWithOptions(path, IndexOptions{Now: now})
}

type IndexOptions struct {
	Now               func() time.Time
	PersistencePolicy func() error
}

func NewIndexWithOptions(path string, options IndexOptions) *Index {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	policy := options.PersistencePolicy
	if policy == nil {
		policy = cachePersistencePolicy
	}
	return &Index{path: path, now: now, persistencePolicy: policy}
}

func (i *Index) Load() error {
	if err := i.persistencePolicy(); err != nil {
		return err
	}
	info, err := os.Lstat(i.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect task reverse index: %w", err)
	}
	if err := validateCacheFile(i.path, info); err != nil {
		return err
	}
	file, err := os.Open(i.path)
	if err != nil {
		return fmt.Errorf("open task reverse index: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened task reverse index: %w", err)
	}
	if err := validateCacheFile(i.path, openedInfo); err != nil {
		return err
	}
	if !os.SameFile(info, openedInfo) {
		return errors.New("task reverse index changed while it was being opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxCacheFileBytes+1))
	if err != nil {
		return fmt.Errorf("read task reverse index: %w", err)
	}
	if len(data) > MaxCacheFileBytes {
		return fmt.Errorf("task reverse index exceeds %d bytes", MaxCacheFileBytes)
	}
	var decoded indexFile
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode task reverse index: %w", err)
	}
	if decoded.FormatVersion != indexFormatV1 {
		i.data = indexFile{FormatVersion: indexFormatV1, Status: IndexStatus{State: StateIncompatible, Reason: ReasonCacheFormat}}
		return nil
	}
	i.data = decoded
	return nil
}

func (i *Index) save(data indexFile) error {
	if err := i.persistencePolicy(); err != nil {
		return err
	}
	dir := filepath.Dir(i.path)
	if err := preparePrivateCacheDir(dir); err != nil {
		return err
	}
	data.FormatVersion = indexFormatV1
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(encoded) > MaxCacheFileBytes {
		return fmt.Errorf("task reverse index exceeds %d bytes", MaxCacheFileBytes)
	}
	tmp, err := os.CreateTemp(dir, ".reverse-index-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := secureCacheFile(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, i.path)
}

func preparePrivateCacheDir(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create task cache directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("inspect task cache directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return errors.New("task cache directory must not be a symlink")
	case !info.IsDir():
		return errors.New("task cache path is not a directory")
	}
	if err := secureCacheDirectory(dir); err != nil {
		return fmt.Errorf("secure task cache directory: %w", err)
	}
	return nil
}

func (i *Index) Rebuild(ctx context.Context, client ListClient, identity CacheIdentity, remoteRevision string, maxTasks int) (IndexStatus, error) {
	if maxTasks < 1 || maxTasks > HardMaxTasks {
		maxTasks = HardMaxTasks
	}
	next := indexFile{FormatVersion: indexFormatV1, Identity: identity, Status: IndexStatus{State: StateReady, Complete: true, LastScan: i.now(), RemoteRevision: remoteRevision}, Entries: []indexEntry{}}
	cursor := ""
	seenCursors := map[string]struct{}{}
	scanned := 0
	for {
		if err := ctx.Err(); err != nil {
			status := i.retainLastGood(StateStale, ReasonInterrupted)
			return status, err
		}
		remaining := maxTasks - scanned
		if remaining < 1 {
			next.Status.State, next.Status.Complete, next.Status.Reason = StatePartial, false, ReasonSafetyLimit
			break
		}
		limit := min(500, remaining)
		page, err := client.ListTasks(ctx, identity.Project, limit, cursor)
		if err != nil {
			status := i.retainLastGood(StateUnavailable, ReasonUnavailable)
			return status, err
		}
		pageTasks := page.Tasks
		stopAfterPage := false
		if len(pageTasks) > limit {
			pageTasks = pageTasks[:limit]
			next.Status.State, next.Status.Complete, next.Status.Reason = StatePartial, false, ReasonSafetyLimit
			stopAfterPage = true
		}
		if len(pageTasks) > remaining {
			pageTasks = pageTasks[:remaining]
			next.Status.State, next.Status.Complete, next.Status.Reason = StatePartial, false, ReasonSafetyLimit
			stopAfterPage = true
		}
		scanned += len(pageTasks)
		for _, task := range pageTasks {
			links, incompatible := indexMailLinks(task.Metadata)
			if incompatible && next.Status.Reason == "" {
				next.Status.State, next.Status.Complete, next.Status.Reason = StatePartial, false, ReasonIncompatibleMetadata
			}
			if len(links) > 0 {
				next.Entries = append(next.Entries, indexEntry{Task: TaskSummary{ID: task.ID, Title: task.Title, Revision: task.Revision}, Links: links})
			}
		}
		if stopAfterPage || page.NextCursor == "" {
			break
		}
		if len(page.Tasks) == 0 {
			next.Status.State, next.Status.Complete, next.Status.Reason = StatePartial, false, ReasonEmptyPage
			break
		}
		if page.NextCursor == cursor {
			next.Status.State, next.Status.Complete, next.Status.Reason = StatePartial, false, ReasonCursorCycle
			break
		}
		if _, seen := seenCursors[page.NextCursor]; seen {
			next.Status.State, next.Status.Complete, next.Status.Reason = StatePartial, false, ReasonCursorCycle
			break
		}
		seenCursors[page.NextCursor] = struct{}{}
		if scanned >= maxTasks {
			next.Status.State, next.Status.Complete, next.Status.Reason = StatePartial, false, ReasonSafetyLimit
			break
		}
		cursor = page.NextCursor
	}
	if err := i.save(next); err != nil {
		if errors.Is(err, ErrDiskCacheSecurityUnsupported) {
			next.Status.Reason = ReasonCachePersistenceUnsupported
			i.data = next
			return next.Status, nil
		}
		return i.markLastGood(StateStale, ReasonPersistenceFailure), err
	}
	i.data = next
	return next.Status, nil
}

func (i *Index) retainLastGood(state IndexState, reason string) IndexStatus {
	status := i.markLastGood(state, reason)
	_ = i.save(i.data)
	return status
}

func (i *Index) markLastGood(state IndexState, reason string) IndexStatus {
	i.data.Status.State = state
	i.data.Status.Complete = false
	i.data.Status.Reason = reason
	if i.data.FormatVersion == 0 {
		i.data.FormatVersion = indexFormatV1
	}
	return i.data.Status
}

func (i *Index) Lookup(expected CacheIdentity, identity MessageIdentity, authenticated bool) LookupResult {
	status := i.data.Status
	if i.data.Identity.Project != expected.Project {
		status.State, status.Complete, status.Reason = StateWrongProject, false, ReasonProjectMismatch
		return LookupResult{IndexStatus: status, Tasks: []TaskSummary{}}
	}
	if i.data.Identity.ArchiveUID != expected.ArchiveUID {
		status.State, status.Complete, status.Reason = StateStale, false, ReasonArchiveUID
		return LookupResult{IndexStatus: status, Tasks: []TaskSummary{}}
	}
	if i.data.Identity.ArchiveRevision != expected.ArchiveRevision {
		status.State, status.Complete, status.Reason = StateStale, false, ReasonArchiveRevision
		return LookupResult{IndexStatus: status, Tasks: []TaskSummary{}}
	}
	if !authenticated {
		status.State, status.Complete, status.Reason = StateAuthenticationRequired, false, "authentication_required"
		return LookupResult{IndexStatus: status, Tasks: []TaskSummary{}}
	}
	if status.State != StateUnavailable && !status.LastScan.IsZero() && i.now().Sub(status.LastScan) > DefaultStaleAfter {
		status.State, status.Complete, status.Reason = StateStale, false, "refresh_overdue"
	}
	result := LookupResult{IndexStatus: status, Tasks: []TaskSummary{}}
	for _, entry := range i.data.Entries {
		if len(Resolve(entry.Links, identity)) > 0 {
			result.Tasks = append(result.Tasks, entry.Task)
		}
	}
	return result
}
