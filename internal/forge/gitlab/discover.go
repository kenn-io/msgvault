package gitlab

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

// cursor is how far each discovery stream came. Each stream keeps its own
// position, so a run that stops in one stream resumes there.
type cursor struct {
	Todos todoCursor `json:"todos,omitzero"`
	// Events holds each of the account's own event streams, by action.
	Events map[string]eventCursor `json:"events,omitempty"`
	// Lists holds each issue and merge request list, by listStream.key.
	Lists map[string]listCursor `json:"lists,omitempty"`
	Known knownCursor           `json:"known,omitzero"`
}

func (c cursor) encode() []byte {
	data, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		panic(fmt.Sprintf("encode GitLab cursor: %v", err)) // strings, numbers and times always encode
	}
	return data
}

type pageFunc = func([]forge.ThreadRef, []byte) error

// Discover reads the account's to-dos, its own events, the issues and
// merge requests it authored, is assigned or reviews, then checks the
// threads already archived for changes.
func (f *Forge) Discover(ctx context.Context, raw []byte, owners forge.OwnerFilter, known forge.KnownThreads, page pageFunc) error {
	state := cursor{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return fmt.Errorf("decode GitLab discovery cursor: %w", err)
		}
	}
	if state.Events == nil {
		state.Events = map[string]eventCursor{}
	}
	if state.Lists == nil {
		state.Lists = map[string]listCursor{}
	}
	if err := f.scanTodos(ctx, &state, owners, page); err != nil {
		return err
	}
	for _, action := range ownEventActions {
		if err := f.scanEvents(ctx, &state, action, owners, page); err != nil {
			return err
		}
	}
	for _, list := range listStreams {
		if err := f.scanList(ctx, &state, list, owners, page); err != nil {
			return err
		}
	}
	return f.checkKnown(ctx, &state, owners, known, page)
}

// todoCursor is how far the to-dos came. GitLab lists them newest first,
// so a scan's watermark moves only once the scan reaches the last one's.
// The newest first order is by creation: a to-do's GraphQL type has no
// update time to stop at, and a new to-do is what a mention, an assignment
// or a review request leaves.
type todoCursor struct {
	// Through is the creation time of the newest to-do the last complete
	// scan read; a scan stops at older ones.
	Through time.Time `json:"through,omitzero"`
	// Seen are the to-dos created at Through that it read.
	Seen []string `json:"seen,omitempty"`
	// Scan is a scan in progress.
	Scan *todoScan `json:"scan,omitempty"`
}

type todoScan struct {
	// After is the GraphQL cursor its next page starts after.
	After string `json:"after,omitempty"`
	// Newest is the creation time of the newest to-do it read, and
	// NewestSeen the to-dos created then.
	Newest     time.Time `json:"newest,omitzero"`
	NewestSeen []string  `json:"newest_seen,omitempty"`
}

// finish makes a complete scan's newest to-dos the watermark.
func (c *todoCursor) finish() {
	scan := c.Scan
	c.Scan = nil
	switch {
	case scan.Newest.IsZero():
	case scan.Newest.Equal(c.Through):
		c.Seen = append(c.Seen, scan.NewestSeen...)
	case scan.Newest.After(c.Through):
		c.Through, c.Seen = scan.Newest, scan.NewestSeen
	}
}

const todosQuery = `query Todos($after: String) {
  currentUser {
    todos(state: [pending, done], type: [ISSUE, MERGEREQUEST, WORKITEM], sort: CREATED_DESC, first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id
        action
        createdAt
        project { id fullPath }
        targetEntity {
          __typename
          ... on Issue { id iid updatedAt }
          ... on MergeRequest { id iid updatedAt }
          ... on WorkItem { id iid updatedAt }
        }
      }
    }
  }
}`

type todo struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	CreatedAt time.Time `json:"createdAt"`
	Project   *struct {
		ID       string `json:"id"`
		FullPath string `json:"fullPath"`
	} `json:"project"`
	Target *struct {
		Typename  string    `json:"__typename"`
		ID        string    `json:"id"`
		IID       string    `json:"iid"`
		UpdatedAt time.Time `json:"updatedAt"`
	} `json:"targetEntity"`
}

// scanTodos reads the to-dos, pending and done, created since the last
// complete scan. A to-do names its project's path, so its owner is known
// before anything else is read.
func (f *Forge) scanTodos(ctx context.Context, state *cursor, owners forge.OwnerFilter, page pageFunc) error {
	cur := &state.Todos
	if cur.Scan == nil {
		cur.Scan = &todoScan{}
	}
	for cur.Scan != nil {
		var after any
		if cur.Scan.After != "" {
			after = cur.Scan.After
		}
		var data struct {
			CurrentUser *struct {
				Todos struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []todo `json:"nodes"`
				} `json:"todos"`
			} `json:"currentUser"`
		}
		if err := f.client.graphQL(ctx, graphQLRequest{
			OperationName: "Todos", Query: todosQuery, Variables: map[string]any{"after": after},
		}, &data); err != nil {
			return fmt.Errorf("list GitLab to-dos: %w", err)
		}
		if data.CurrentUser == nil {
			return errors.New("list GitLab to-dos: GitLab returned no current user for the token")
		}
		todos := data.CurrentUser.Todos
		reached := false
		var refs []forge.ThreadRef
		for _, t := range todos.Nodes {
			if t.CreatedAt.Before(cur.Through) {
				reached = true
				break
			}
			if t.CreatedAt.Equal(cur.Through) && slices.Contains(cur.Seen, t.ID) {
				continue
			}
			switch scan := cur.Scan; {
			case t.CreatedAt.After(scan.Newest):
				scan.Newest, scan.NewestSeen = t.CreatedAt, []string{t.ID}
			case t.CreatedAt.Equal(scan.Newest) && !slices.Contains(scan.NewestSeen, t.ID):
				scan.NewestSeen = append(scan.NewestSeen, t.ID)
			}
			if ref, ok := f.todoRef(t); ok && owners.Allows(ref.Owner) {
				refs = append(refs, ref)
			}
		}
		if reached || !todos.PageInfo.HasNextPage || len(todos.Nodes) == 0 {
			cur.finish()
		} else {
			cur.Scan.After = todos.PageInfo.EndCursor
		}
		if err := page(refs, state.encode()); err != nil {
			return err
		}
	}
	return nil
}

// todoRef is the thread a to-do is about. A work item in a project is an
// issue by another name, with the issue's ID.
func (f *Forge) todoRef(t todo) (forge.ThreadRef, bool) {
	if t.Project == nil || t.Target == nil {
		return forge.ThreadRef{}, false
	}
	resource := resourceIssues
	switch t.Target.Typename {
	case "Issue", "WorkItem":
	case "MergeRequest":
		resource = resourceMergeRequests
	default:
		return forge.ThreadRef{}, false
	}
	project, ok1 := gidNumber(t.Project.ID)
	id, ok2 := gidNumber(t.Target.ID)
	iid, err := strconv.Atoi(t.Target.IID)
	if !ok1 || !ok2 || err != nil || iid <= 0 || t.Project.FullPath == "" {
		return forge.ThreadRef{}, false
	}
	f.notePath(project, t.Project.FullPath)
	ref := threadRef(resource, id, project, iid, t.Project.FullPath, t.Target.UpdatedAt)
	ref.Reason = t.Action
	return ref, true
}

// gidNumber reads the number of a global ID such as
// "gid://gitlab/Project/5".
func gidNumber(gid string) (int64, bool) {
	slash := strings.LastIndexByte(gid, '/')
	if !strings.HasPrefix(gid, "gid://gitlab/") || slash < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(gid[slash+1:], 10, 64)
	return n, err == nil && n > 0
}

// ownEventActions are the account's own actions that make it part of a
// thread: a comment, an issue or merge request it opened, an approval.
// Comments include replies inside discussions. GitLab keeps events for
// three years.
var ownEventActions = []string{"commented", "created", "approved"}

// eventCursor is how far one of the account's own event streams came.
// Events never change and come oldest first, so the newest one read is a
// position everything before it was read up to.
type eventCursor struct {
	// Through is the creation time of the newest event read.
	Through time.Time `json:"through,omitzero"`
	// LastID is that event's ID. GitLab filters events by day, so a scan
	// reads Through's day again and skips the events up to LastID, which
	// grow with creation.
	LastID int64 `json:"last_id,omitzero"`
	// Next is the next page of a scan in progress. It carries that scan's
	// query, so resuming doesn't reread the start of Through's day.
	Next string `json:"next,omitempty"`
}

type event struct {
	ID         int64     `json:"id"`
	ProjectID  int64     `json:"project_id"`
	TargetID   int64     `json:"target_id"`
	TargetIID  int       `json:"target_iid"`
	TargetType string    `json:"target_type"`
	CreatedAt  time.Time `json:"created_at"`
	Note       *struct {
		NoteableID   int64  `json:"noteable_id"`   //nolint:misspell // GitLab's field name
		NoteableIID  int    `json:"noteable_iid"`  //nolint:misspell // GitLab's field name
		NoteableType string `json:"noteable_type"` //nolint:misspell // GitLab's field name
	} `json:"note"`
}

// thread is the issue or merge request an event is about.
func (e event) thread() (resource string, id int64, iid int, ok bool) {
	kind, id, iid := e.TargetType, e.TargetID, e.TargetIID
	if e.Note != nil {
		kind, id, iid = e.Note.NoteableType, e.Note.NoteableID, e.Note.NoteableIID
	}
	switch kind {
	case "Issue", "WorkItem":
		resource = resourceIssues
	case "MergeRequest":
		resource = resourceMergeRequests
	default:
		return "", 0, 0, false
	}
	return resource, id, iid, id > 0 && iid > 0 && e.ProjectID > 0
}

// scanEvents reads the account's own events of one action since the
// stream's position, oldest first. An event names only its project's ID,
// so the project's path, and with it the owner, is looked up before the
// thread is reported.
func (f *Forge) scanEvents(ctx context.Context, state *cursor, action string, owners forge.OwnerFilter, page pageFunc) error {
	f.mu.Lock()
	accountID := f.accountID
	f.mu.Unlock()
	if accountID == "" {
		account, err := f.Account(ctx)
		if err != nil {
			return err
		}
		accountID = account.ID
	}
	cur := state.Events[action]
	query := url.Values{"action": {action}, "sort": {"asc"}, "per_page": {strconv.Itoa(perPage)}}
	if !cur.Through.IsZero() {
		// after is exclusive and counts whole days: the day before
		// Through's reads all of Through's day.
		query.Set("after", cur.Through.UTC().AddDate(0, 0, -1).Format(time.DateOnly))
	}
	next := cur.Next
	if next == "" {
		next = "/users/" + accountID + "/events"
	} else {
		query = nil // The saved next link carries the scan's original query.
	}
	for next != "" {
		var batch []event
		header, err := f.client.get(ctx, next, query, &batch)
		if err != nil {
			return fmt.Errorf("list GitLab %s events: %w", action, err)
		}
		var refs []forge.ThreadRef
		for _, e := range batch {
			if e.ID <= cur.LastID {
				continue
			}
			cur.LastID = e.ID
			if e.CreatedAt.After(cur.Through) {
				cur.Through = e.CreatedAt
			}
			resource, id, iid, ok := e.thread()
			if !ok {
				continue
			}
			path, err := f.projectPath(ctx, e.ProjectID)
			if errors.Is(err, forge.ErrUnavailable) {
				// The project is gone or closed to the token.
				continue
			}
			if err != nil {
				return fmt.Errorf("look up the project of a GitLab %s event: %w", action, err)
			}
			ref := threadRef(resource, id, e.ProjectID, iid, path, e.CreatedAt)
			if owners.Allows(ref.Owner) {
				refs = append(refs, ref)
			}
		}
		// The next link carries the query.
		next, query = forge.NextLink(header.Get("Link")), nil
		cur.Next = next
		state.Events[action] = cur
		if err := page(refs, state.encode()); err != nil {
			return err
		}
	}
	return nil
}

// listStream is one list of issues or merge requests the account takes
// part in.
type listStream struct {
	resource string
	scope    string
}

func (l listStream) key() string { return l.resource + "/" + l.scope }

var listStreams = []listStream{
	{resourceMergeRequests, "created_by_me"},
	{resourceMergeRequests, "assigned_to_me"},
	{resourceMergeRequests, "reviews_for_me"},
	{resourceIssues, "created_by_me"},
	{resourceIssues, "assigned_to_me"},
}

// listCursor is how far one list came. A list is read in order of update,
// each request asking for what was updated at or after Through, so a
// thread updated during a scan moves ahead and is read later. These lists
// have no keyset paging, so a run of more than a page of threads updated at
// the same instant is read by their creation time, which never changes,
// rather than by page number, which a thread leaving the run would shift.
type listCursor struct {
	Through time.Time `json:"through,omitzero"`
	// Seen are the threads updated at Through already read.
	Seen []string `json:"seen,omitempty"`
	// CreatedFrom is set while the threads updated at Through are read by
	// creation time: those created before it are read.
	CreatedFrom time.Time `json:"created_from,omitzero"`
}

// advance records a thread read in update order.
func (c *listCursor) advance(updated time.Time, key string) {
	switch {
	case updated.After(c.Through):
		c.Through, c.Seen = updated, []string{key}
	case updated.Equal(c.Through):
		c.Seen = append(c.Seen, key)
	}
}

// seen reports whether a thread at this update was read before.
func (c *listCursor) seen(updated time.Time, key string) bool {
	return updated.Before(c.Through) || updated.Equal(c.Through) && slices.Contains(c.Seen, key)
}

// listItem is an issue or merge request in a list.
type listItem struct {
	ID         int64     `json:"id"`
	IID        int       `json:"iid"`
	ProjectID  int64     `json:"project_id"`
	UpdatedAt  time.Time `json:"updated_at"`
	References struct {
		Full string `json:"full"`
	} `json:"references"`
}

// ref is the thread a list item is; ok is false for one without its
// project's path.
func (f *Forge) listRef(resource string, item listItem) (forge.ThreadRef, bool) {
	path := referencePath(item.References.Full)
	if path == "" || item.ID <= 0 || item.IID <= 0 || item.ProjectID <= 0 {
		return forge.ThreadRef{}, false
	}
	f.notePath(item.ProjectID, path)
	return threadRef(resource, item.ID, item.ProjectID, item.IID, path, item.UpdatedAt), true
}

// referencePath is the project path of a full reference such as
// "group/project#6" or "group/project!7".
func referencePath(full string) string {
	end := strings.LastIndexAny(full, "#!")
	if end <= 0 {
		return ""
	}
	return full[:end]
}

// updatedAfter formats a time for updated_after, to the millisecond GitLab
// keeps.
func updatedAfter(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// scanList reads one list, all states and archived projects included, in
// order of update from the list's position.
func (f *Forge) scanList(ctx context.Context, state *cursor, list listStream, owners forge.OwnerFilter, page pageFunc) error {
	for {
		cur := state.Lists[list.key()]
		if !cur.CreatedFrom.IsZero() {
			if err := f.readInstant(ctx, state, list, owners, page); err != nil {
				return err
			}
			continue
		}
		through := cur.Through
		batch, err := f.listPage(ctx, list, through, nil)
		if err != nil {
			return err
		}
		refs := f.readListItems(list, batch, &cur, owners, nil)
		full := len(batch) == perPage
		// Decide the next request before reporting, since the callback can
		// stop the scan.
		if full && !cur.Through.After(through) {
			if last := batch[len(batch)-1].UpdatedAt; last.Equal(through) {
				// The whole page was updated at through: read that instant
				// by creation time.
				cur.CreatedFrom = firstCreated
			} else {
				// No thread of the page could be read: move on to its last
				// instant, whose threads may continue on the next page.
				cur.Through, cur.Seen = last, nil
			}
		}
		state.Lists[list.key()] = cur
		if err := page(refs, state.encode()); err != nil {
			return err
		}
		if !full {
			return nil
		}
	}
}

// firstCreated is the earliest creation time a list request names.
var firstCreated = time.Unix(0, 0).UTC()

// createdRange bounds a list request by creation time, both ends included.
type createdRange struct{ from, to time.Time }

// listPage requests the first page of a list from through on, oldest update
// first, within created when it is set.
func (f *Forge) listPage(ctx context.Context, list listStream, through time.Time, created *createdRange) ([]listItem, error) {
	query := url.Values{
		"scope": {list.scope}, "state": {"all"}, "order_by": {"updated_at"}, "sort": {"asc"},
		"per_page": {strconv.Itoa(perPage)},
	}
	if list.resource == resourceIssues {
		query.Set("non_archived", "false")
	}
	if !through.IsZero() {
		query.Set("updated_after", updatedAfter(through))
	}
	if created != nil {
		query.Set("created_after", updatedAfter(created.from))
		query.Set("created_before", updatedAfter(created.to))
	}
	var batch []listItem
	if _, err := f.client.get(ctx, "/"+list.resource, query, &batch); err != nil {
		return nil, fmt.Errorf("list GitLab %s: %w", strings.ReplaceAll(list.key(), "_", " "), err)
	}
	return batch, nil
}

// readListItems records the items of a page not read before and returns
// the threads the owner filter allows. With only set, it reads only the
// items updated at that instant.
func (f *Forge) readListItems(list listStream, batch []listItem, cur *listCursor, owners forge.OwnerFilter, only *time.Time) []forge.ThreadRef {
	var refs []forge.ThreadRef
	for _, item := range batch {
		if only != nil && !item.UpdatedAt.Equal(*only) {
			continue
		}
		ref, ok := f.listRef(list.resource, item)
		if !ok || cur.seen(item.UpdatedAt, ref.Key) {
			continue
		}
		cur.advance(item.UpdatedAt, ref.Key)
		if owners.Allows(ref.Owner) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// readInstant reads the threads of a list updated at its Through, more than
// a page of them, from its CreatedFrom on, by halving their creation time
// until each range fits in a page, oldest range first. Neighbouring ranges
// share their boundary, since GitLab keeps finer times than a request
// names; a thread read twice is seen. Each range read is saved, and the
// last one moves the list past the instant: GitLab keeps update times to
// the millisecond.
func (f *Forge) readInstant(ctx context.Context, state *cursor, list listStream, owners forge.OwnerFilter, page pageFunc) error {
	at := state.Lists[list.key()].Through
	end := at.Add(time.Millisecond) // a thread is created no later than it is updated
	var bisect func(created createdRange) error
	bisect = func(created createdRange) error {
		batch, err := f.listPage(ctx, list, at, &created)
		if err != nil {
			return err
		}
		// Oldest update first, so a page that ends past the instant holds
		// all of the range's threads updated at it.
		if len(batch) == perPage && batch[len(batch)-1].UpdatedAt.Equal(at) {
			if created.to.Sub(created.from) < 2*time.Millisecond {
				return fmt.Errorf("more than %d GitLab %s were created and updated in the same millisecond, at %s; discovery can't read past them",
					perPage, strings.ReplaceAll(list.resource, "_", " "), updatedAfter(at))
			}
			middle := created.from.Add(created.to.Sub(created.from) / 2).Truncate(time.Millisecond)
			if err := bisect(createdRange{from: created.from, to: middle}); err != nil {
				return err
			}
			return bisect(createdRange{from: middle, to: created.to})
		}
		cur := state.Lists[list.key()]
		refs := f.readListItems(list, batch, &cur, owners, &at)
		cur.CreatedFrom = created.to
		if !created.to.Before(end) {
			cur = listCursor{Through: end}
		}
		state.Lists[list.key()] = cur
		return page(refs, state.encode())
	}
	return bisect(createdRange{from: state.Lists[list.key()].CreatedFrom, to: end})
}

// knownCursor is how far the check of archived threads came. A check asks
// for the threads updated since the sync of the last complete check
// started. A fetch that straddled that check's start also needs changes
// since it read its first page, before the watermark.
type knownCursor struct {
	// Through is when the sync of the last complete check started, by
	// GitLab's clock.
	Through time.Time `json:"through,omitzero"`
	// Started is when the sync of the check in progress started, and
	// Done the group of the last batch it checked.
	Started time.Time   `json:"started,omitzero"`
	Done    *knownGroup `json:"done,omitempty"`
	// LastIID is the last thread number checked within Done.
	LastIID int `json:"last_iid,omitzero"`
}

// knownGroup is the archived threads of one kind in one project.
type knownGroup struct {
	Project  int64  `json:"project"`
	Resource string `json:"resource"`
}

func (g knownGroup) compare(other knownGroup) int {
	return cmp.Or(cmp.Compare(g.Project, other.Project), strings.Compare(g.Resource, other.Resource))
}

// checkKnown asks each project which of its archived threads changed since
// the last complete check, by their numbers, and reports those updated
// after the version archived. It finds what no other stream lists, such as
// a reply in a thread the account took part in long ago, or an edit.
func (f *Forge) checkKnown(ctx context.Context, state *cursor, owners forge.OwnerFilter, known forge.KnownThreads, page pageFunc) error {
	groups := map[knownGroup]map[int]forge.KnownThread{}
	err := known.Each(ctx, func(thread forge.KnownThread) error {
		loc, ok := locatorOf(thread.Ref)
		if !ok || !owners.Allows(thread.Ref.Owner) {
			return nil
		}
		group := knownGroup{Project: loc.project, Resource: loc.resource}
		if groups[group] == nil {
			groups[group] = map[int]forge.KnownThread{}
		}
		groups[group][loc.iid] = thread
		return nil
	})
	if err != nil {
		return fmt.Errorf("read the archived GitLab threads: %w", err)
	}
	cur := &state.Known
	if cur.Started.IsZero() {
		cur.Started = f.client.startedAt()
	}
	for _, group := range slices.SortedFunc(maps.Keys(groups), knownGroup.compare) {
		if cur.Done != nil && group.compare(*cur.Done) < 0 {
			continue
		}
		if err := f.checkGroup(ctx, state, group, groups[group], owners, page); err != nil {
			return err
		}
	}
	cur.Through, cur.Started, cur.Done = cur.Started, time.Time{}, nil
	cur.LastIID = 0
	return page(nil, state.encode())
}

// checkGroup asks one project for its archived threads of one kind that
// changed, a page of numbers at a time. The project's current owner is
// checked first: a project moved to an excluded owner since its threads
// were archived has none of them read.
func (f *Forge) checkGroup(ctx context.Context, state *cursor, group knownGroup, archived map[int]forge.KnownThread,
	owners forge.OwnerFilter, page pageFunc,
) error {
	cur := &state.Known
	iids := slices.Sorted(maps.Keys(archived))
	if cur.Done != nil && group.compare(*cur.Done) == 0 {
		iids = slices.DeleteFunc(iids, func(iid int) bool { return iid <= cur.LastIID })
	}
	if len(iids) == 0 {
		return nil
	}
	owner, err := f.projectOwner(ctx, group.Project)
	if err != nil && !errors.Is(err, forge.ErrUnavailable) {
		return fmt.Errorf("look up the owner of GitLab project %d: %w", group.Project, err)
	}
	if err != nil || !owners.Allows(owner) {
		// The project is gone, closed to the token, or an excluded owner's
		// now. Its threads fail, or are dropped, on their own when fetched.
		cur.Done, cur.LastIID = &group, iids[len(iids)-1]
		return page(nil, state.encode())
	}
	for batch := range slices.Chunk(iids, perPage) {
		through := cur.Through
		for _, iid := range batch {
			thread := archived[iid]
			// A fetch completed after the last check began may have read
			// its opening post earlier, before an edit during a pause.
			if thread.ReadAt.Before(through) && !thread.CheckedAt.Before(cur.Through) {
				through = thread.ReadAt
			}
		}
		items, gone, err := f.changedSince(ctx, through, group, batch)
		if err != nil {
			return err
		}
		lastIID := batch[len(batch)-1]
		if gone {
			// The project is gone or closed to the token; its threads fail
			// on their own when fetched.
			lastIID = iids[len(iids)-1]
		}
		var refs []forge.ThreadRef
		for _, item := range items {
			ref, ok := f.listRef(group.Resource, item)
			stored, archivedOne := archived[item.IID]
			if ok && archivedOne && item.UpdatedAt.After(stored.Ref.UpdatedAt) && owners.Allows(ref.Owner) {
				refs = append(refs, ref)
			}
		}
		cur.Done, cur.LastIID = &group, lastIID
		if err := page(refs, state.encode()); err != nil {
			return err
		}
		if gone {
			break
		}
	}
	return nil
}

// changedSince lists a project's threads of one kind, by number, updated
// since through; gone reports a project the token can't read.
func (f *Forge) changedSince(ctx context.Context, through time.Time, group knownGroup, iids []int) (items []listItem, gone bool, err error) {
	query := url.Values{"state": {"all"}, "per_page": {strconv.Itoa(perPage)}}
	for _, iid := range iids {
		query.Add("iids[]", strconv.Itoa(iid))
	}
	if !through.IsZero() {
		query.Set("updated_after", updatedAfter(through))
	}
	_, err = f.client.get(ctx, "/projects/"+strconv.FormatInt(group.Project, 10)+"/"+group.Resource, query, &items)
	switch err := forbiddenIsUnavailable(err); {
	case errors.Is(err, forge.ErrUnavailable):
		return nil, true, nil
	case err != nil:
		return nil, false, fmt.Errorf("check archived GitLab %s of project %d: %w", group.Resource, group.Project, err)
	}
	return items, false, nil
}
