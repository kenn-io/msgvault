package tasklinks

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/taskclient"
)

type listClient struct {
	pages   []taskclient.TaskList
	errAt   int
	calls   int
	limits  []int
	cursors []string
}

func (c *listClient) ListTasks(_ context.Context, _ string, limit int, cursor string) (taskclient.TaskList, error) {
	c.calls++
	c.limits = append(c.limits, limit)
	c.cursors = append(c.cursors, cursor)
	if c.errAt == c.calls {
		return taskclient.TaskList{}, errors.New("unavailable")
	}
	return c.pages[c.calls-1], nil
}

func indexedTask(id, title string, identity MessageIdentity) taskclient.Task {
	return taskclient.Task{ID: id, Project: "project", Title: title, Revision: "r1", Metadata: MetadataWithLink(nil, NewMailLink(identity, time.Now()))}
}

func testCacheIdentity() CacheIdentity {
	return CacheIdentity{Project: "project", ArchiveUID: "archive-a", ArchiveRevision: "archive-rev-1"}
}
