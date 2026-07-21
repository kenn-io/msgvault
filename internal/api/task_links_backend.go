package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/tasklinks"
)

const taskReverseIndexLimit = tasklinks.HardMaxTasks

type taskLinkBackend struct {
	config          config.TaskIntegrationConfig
	index           *tasklinks.Index
	lastIndexStatus tasklinks.IndexStatus
	lastIndexErr    error
	mu              sync.Mutex
}

func newTaskLinkBackend(cfg *config.Config) *taskLinkBackend {
	backend := &taskLinkBackend{
		config: cfg.Integrations.Tasks,
		index:  tasklinks.NewIndex(filepath.Join(cfg.Data.DataDir, "cache", "task-links.json"), nil),
	}
	if err := backend.index.Load(); err != nil {
		backend.lastIndexStatus = indexFailureStatus(tasklinks.IndexStatus{}, err)
		backend.lastIndexErr = err
	}
	return backend
}

func (b *taskLinkBackend) integrationConfig() taskclient.IntegrationConfig {
	return taskclient.IntegrationConfig{Enabled: b.config.Enabled, Endpoint: b.config.Endpoint,
		APIKey: b.config.APIKey, DefaultProject: b.config.DefaultProject}
}

func (b *taskLinkBackend) connect(ctx context.Context) (*taskclient.Client, taskclient.Project, error) {
	return taskclient.Connect(ctx, b.integrationConfig())
}

func (b *taskLinkBackend) projectPath() string { return strings.TrimSpace(b.config.DefaultProject) }

func (b *taskLinkBackend) cacheIdentity(identity tasklinks.MessageIdentity) tasklinks.CacheIdentity {
	return tasklinks.CacheIdentity{Project: b.projectPath(), ArchiveUID: identity.ArchiveUID, ArchiveRevision: identity.ArchiveRevision}
}

func (b *taskLinkBackend) Create(ctx context.Context, key string, create taskclient.TaskCreate, identity tasklinks.MessageIdentity, addedAt time.Time) (taskclient.Task, error) {
	client, project, err := b.connect(ctx)
	if err != nil {
		return taskclient.Task{}, err
	}
	task, err := (tasklinks.Service{Client: client, Project: b.projectPath(), Now: func() time.Time { return addedAt }}).Create(ctx, key, create, identity)
	if err == nil {
		b.refresh(ctx, client, project, identity)
	}
	return task, err
}

func (b *taskLinkBackend) Link(ctx context.Context, taskID string, identity tasklinks.MessageIdentity, addedAt time.Time) (taskclient.Task, error) {
	client, project, err := b.connect(ctx)
	if err != nil {
		return taskclient.Task{}, err
	}
	task, err := (tasklinks.Service{Client: client, Project: b.projectPath(), Now: func() time.Time { return addedAt }}).Link(ctx, taskID, identity)
	if err == nil {
		b.refresh(ctx, client, project, identity)
	}
	return task, err
}

func (b *taskLinkBackend) Unlink(ctx context.Context, taskID string, identity tasklinks.MessageIdentity) (taskclient.Task, error) {
	client, project, err := b.connect(ctx)
	if err != nil {
		return taskclient.Task{}, err
	}
	task, err := (tasklinks.Service{Client: client, Project: b.projectPath()}).Unlink(ctx, taskID, identity)
	if err == nil {
		b.refresh(ctx, client, project, identity)
	}
	return task, err
}

func (b *taskLinkBackend) Search(ctx context.Context, query string) ([]tasklinks.TaskSummary, error) {
	client, _, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	listed, err := client.SearchTasks(ctx, b.projectPath(), query, 25)
	if err != nil {
		return nil, err
	}
	result := make([]tasklinks.TaskSummary, 0, len(listed.Tasks))
	for _, task := range listed.Tasks {
		result = append(result, tasklinks.TaskSummary{ID: task.ID, Title: task.Title, Revision: task.Revision})
	}
	return result, nil
}

func (b *taskLinkBackend) refresh(ctx context.Context, client *taskclient.Client, project taskclient.Project, identity tasklinks.MessageIdentity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastIndexStatus, b.lastIndexErr = b.index.Rebuild(ctx, client, b.cacheIdentity(identity), project.Revision, taskReverseIndexLimit)
}

func (b *taskLinkBackend) Lookup(ctx context.Context, identity tasklinks.MessageIdentity) tasklinks.LookupResult {
	expected := b.cacheIdentity(identity)
	client, project, err := b.connect(ctx)
	if err != nil {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.degradedLookup(expected, identity, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	status, rebuildErr := b.index.Rebuild(ctx, client, expected, project.Revision, taskReverseIndexLimit)
	b.lastIndexStatus, b.lastIndexErr = status, rebuildErr
	result := b.index.Lookup(expected, identity, true)
	if rebuildErr != nil {
		result.IndexStatus = indexFailureStatus(status, rebuildErr)
	}
	return result
}

func (b *taskLinkBackend) degradedLookup(expected tasklinks.CacheIdentity, identity tasklinks.MessageIdentity, integrationErr error) tasklinks.LookupResult {
	authenticated := !errors.Is(integrationErr, taskclient.ErrAuthenticationRequired)
	result := b.index.Lookup(expected, identity, authenticated)
	result.Complete = false
	if b.lastIndexErr == nil {
		switch {
		case !authenticated:
			result.State, result.Reason = tasklinks.StateAuthenticationRequired, "authentication_required"
		case !b.config.Enabled:
			result.State, result.Reason = tasklinks.StateDisabled, "disabled"
		case errors.Is(integrationErr, taskclient.ErrWrongProject):
			result.State, result.Reason = tasklinks.StateWrongProject, "wrong_project"
		case errors.Is(integrationErr, taskclient.ErrNotFound):
			result.State, result.Reason = tasklinks.StateNotFound, "not_found"
		case errors.Is(integrationErr, taskclient.ErrIncompatible), errors.Is(integrationErr, taskclient.ErrInsecureDescriptor), errors.Is(integrationErr, taskclient.ErrInsecureEndpoint), errors.Is(integrationErr, taskclient.ErrInvalidResponse), errors.Is(integrationErr, taskclient.ErrResponseTooLarge), errors.Is(integrationErr, taskclient.ErrRedirect):
			result.State, result.Reason = tasklinks.StateIncompatible, "incompatible"
		default:
			result.State, result.Reason = tasklinks.StateUnavailable, tasklinks.ReasonUnavailable
		}
	} else {
		result.IndexStatus = indexFailureStatus(b.lastIndexStatus, b.lastIndexErr)
	}
	if !authenticated {
		result.Tasks = []tasklinks.TaskSummary{}
	}
	return result
}

func indexFailureStatus(status tasklinks.IndexStatus, err error) tasklinks.IndexStatus {
	status.Complete = false
	if errors.Is(err, tasklinks.ErrDiskCacheSecurityUnsupported) {
		status.State = tasklinks.StateUnavailable
		status.Reason = tasklinks.ReasonCachePersistenceUnsupported
	} else if status.Reason == "" {
		status.State = tasklinks.StateStale
		status.Reason = tasklinks.ReasonPersistenceFailure
	}
	return status
}
