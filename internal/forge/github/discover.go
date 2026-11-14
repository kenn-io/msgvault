package github

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

const (
	// notificationsPerPage is the most GitHub's notifications API allows.
	notificationsPerPage = 50
	// searchWindow is the most results one GitHub search returns; a longer
	// history is read in windows of update time.
	searchWindow = 1000
	// searchOverlap is how far before its latest read update a search
	// starts again: GitHub's search index can show an update after a newer
	// one it already showed.
	searchOverlap = 5 * time.Minute
)

// cursor is how far each discovery stream came.
type cursor struct {
	// Notifications advance only after a scan read every page.
	Notifications notificationCursor `json:"notifications,omitzero"`
	// Each search's through is the latest update it has read. Results come
	// oldest first, so everything older was read too, except an update the
	// index shows late: the next page continues from there, and the next
	// sync from searchOverlap before it.
	IssuesThrough          time.Time `json:"issues_through,omitzero"`
	DiscussionsThrough     time.Time `json:"discussions_through,omitzero"`
	ReviewedThrough        time.Time `json:"reviewed_through,omitzero"`
	ReviewRequestedThrough time.Time `json:"review_requested_through,omitzero"`
}

type notificationCursor struct {
	// LastModified is the list's Last-Modified header, sent back as
	// If-Modified-Since, which GitHub doesn't count against the rate limit.
	LastModified string `json:"last_modified,omitempty"`
	// Since is the latest thread update the last complete scan showed. The
	// next scan reads back from a second before it: GitHub's times are whole
	// seconds, and a thread can be updated later in Since's second. A thread
	// read again unchanged is already archived, so it isn't fetched again.
	Since time.Time `json:"since,omitzero"`
}

func (c cursor) encode() []byte {
	data, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		panic(fmt.Sprintf("encode GitHub cursor: %v", err)) // a struct of strings and times always encodes
	}
	return data
}

// Discover reads the notifications, then searches for the issues, pull
// requests and discussions the account authored, commented on, was
// assigned or mentioned in, and the pull requests it reviewed or was asked
// to review.
func (f *Forge) Discover(ctx context.Context, raw []byte, owners forge.OwnerFilter, _ forge.KnownThreads,
	page func([]forge.ThreadRef, []byte) error,
) error {
	var state cursor
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return fmt.Errorf("decode GitHub discovery cursor: %w", err)
		}
	}
	login, err := f.login(ctx)
	if err != nil {
		return err
	}
	missed, err := f.scanNotifications(ctx, &state, owners, page)
	if err != nil {
		return err
	}
	for _, search := range []struct {
		kind    searchKind
		query   string
		through *time.Time
	}{
		{searchIssues, "involves:" + login, &state.IssuesThrough},
		{searchDiscussions, "involves:" + login, &state.DiscussionsThrough},
		// involves: leaves out reviews; a pull request the account only
		// reviewed or was asked to review is found here.
		{searchIssues, "is:pr reviewed-by:" + login, &state.ReviewedThrough},
		{searchIssues, "is:pr review-requested:" + login, &state.ReviewRequestedThrough},
	} {
		if err := f.searchThreads(ctx, search.kind, search.query, &state, search.through, owners, page); err != nil {
			return err
		}
	}
	if len(missed) > 0 {
		return &forge.IncompleteError{Missed: missed}
	}
	return nil
}

// notification is one thread of GET /notifications.
type notification struct {
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	Subject   struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Type  string `json:"type"`
	} `json:"subject"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// scanNotifications lists every notification thread updated after the last
// complete scan, read or not. A scan that stops early starts over next time;
// the threads it found are already queued. It returns the notified
// discussions it couldn't look up; they hold the cursor, so the next scan
// tries them again.
func (f *Forge) scanNotifications(ctx context.Context, state *cursor, owners forge.OwnerFilter,
	page func([]forge.ThreadRef, []byte) error,
) ([]forge.Missed, error) {
	query := url.Values{"all": {"true"}, "per_page": {strconv.Itoa(notificationsPerPage)}}
	if since := state.Notifications.Since; !since.IsZero() {
		query.Set("since", since.Add(-time.Second).UTC().Format(time.RFC3339))
	}
	header := http.Header{}
	if state.Notifications.LastModified != "" {
		header.Set("If-Modified-Since", state.Notifications.LastModified)
	}
	latest := state.Notifications.Since
	lastModified := ""
	// missed holds the notifications that couldn't be turned into threads;
	// any of them holds the cursor, so the next scan tries them again.
	var missed []forge.Missed
	next := "/notifications"
	for first := true; next != ""; first = false {
		var batch []notification
		resp, err := f.client.getJSON(ctx, next, query, header, &batch)
		if err != nil {
			return nil, fmt.Errorf("list GitHub notifications: %w", err)
		}
		if first {
			if resp.Status == http.StatusNotModified {
				return nil, nil
			}
			lastModified = resp.Header.Get("Last-Modified")
		}
		refs := make([]forge.ThreadRef, 0, len(batch))
		for _, n := range batch {
			if n.UpdatedAt.After(latest) {
				latest = n.UpdatedAt
			}
			ref, ok := notificationRef(n)
			if !ok || !owners.Allows(ref.Owner) {
				continue
			}
			if ref.Kind == forge.KindDiscussion {
				numbers, err := f.findDiscussions(ctx, ref.Repo, n.Subject.Title, n.UpdatedAt)
				switch {
				case errors.Is(err, errDiscussionNotFound):
					// No such discussion any more: nothing to archive.
					continue
				case forge.IsRateLimit(err):
					return nil, err
				case err != nil:
					missed = append(missed, forge.Missed{
						Name: fmt.Sprintf("%s discussion %q", ref.Repo, n.Subject.Title), Err: err,
					})
					continue
				}
				for _, number := range numbers {
					found := ref
					found.Number, found.Key = number, threadKey(ref.Repo, number)
					refs = append(refs, found)
				}
				continue
			}
			refs = append(refs, ref)
		}
		// The next link carries the query; only the first page is conditional.
		next, query, header = forge.NextLink(resp.Header.Get("Link")), nil, nil
		if next == "" && len(missed) == 0 {
			state.Notifications = notificationCursor{LastModified: lastModified, Since: latest}
		}
		if err := page(refs, state.encode()); err != nil {
			return nil, err
		}
	}
	return missed, nil
}

// notificationRef is the thread a notification points at. A discussion's
// notification names no number; its ref carries the title, and Fetch finds
// the number.
func notificationRef(n notification) (forge.ThreadRef, bool) {
	owner, repo := n.Repository.Owner.Login, n.Repository.Name
	ref := forge.ThreadRef{Owner: owner, Repo: owner + "/" + repo, UpdatedAt: n.UpdatedAt, Reason: n.Reason}
	switch n.Subject.Type {
	case "Issue", "PullRequest":
		number, ok := subjectNumber(n.Subject.URL)
		if !ok {
			return forge.ThreadRef{}, false
		}
		ref.Kind, ref.Number = forge.KindIssue, number
		if n.Subject.Type == "PullRequest" {
			ref.Kind = forge.KindPullRequest
		}
		ref.Key = threadKey(ref.Repo, number)
	case "Release":
		if n.Subject.URL == "" {
			return forge.ThreadRef{}, false
		}
		ref.Kind, ref.Locator = forge.KindRelease, n.Subject.URL
		ref.Key = strings.ToLower(ref.Repo) + "@" + n.Subject.URL
	case "Discussion":
		// A discussion's notification names no number; scanNotifications
		// finds it by title.
		ref.Kind = forge.KindDiscussion
	default:
		// CI runs, security alerts, commits: no conversation to archive.
		return forge.ThreadRef{}, false
	}
	return ref, true
}

// threadKey is an issue's, pull request's or discussion's key: they share
// one number sequence per repository.
func threadKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// subjectNumber reads the number from an issue or pull request API URL,
// .../repos/{owner}/{repo}/issues/{number} or .../pulls/{number}.
func subjectNumber(subjectURL string) (int, bool) {
	parsed, err := url.Parse(subjectURL)
	if err != nil {
		return 0, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || (parts[len(parts)-2] != "issues" && parts[len(parts)-2] != "pulls") {
		return 0, false
	}
	number, err := strconv.Atoi(parts[len(parts)-1])
	return number, err == nil && number > 0
}

// searchKind is a GitHub search type that finds threads.
type searchKind string

const (
	searchIssues      searchKind = "ISSUE"
	searchDiscussions searchKind = "DISCUSSION"
)

const searchThreadsQuery = `query SearchThreads($q: String!, $type: SearchType!, $after: String) {
  search(type: $type, query: $q, first: 100, after: $after) {
    issueCount
    discussionCount
    pageInfo { hasNextPage endCursor }
    nodes {
      __typename
      ... on Issue { number updatedAt repository { name owner { login } } }
      ... on PullRequest { number updatedAt repository { name owner { login } } }
      ... on Discussion { number updatedAt repository { name owner { login } } }
    }
  }
}`

type searchNode struct {
	Typename   string    `json:"__typename"`
	Number     int       `json:"number"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

type searchPage struct {
	IssueCount      int `json:"issueCount"`
	DiscussionCount int `json:"discussionCount"`
	PageInfo        struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []searchNode `json:"nodes"`
}

// searchThreads finds the threads of kind that match query, updated at or
// after searchOverlap before *through, oldest first. A thread read again
// unchanged is already archived, and the core skips it. GitHub returns at
// most 1,000 results per search, so a longer history is read in windows,
// each starting at the last update the previous one read, which may be
// earlier than *through while the search reads back. *through advances
// after every page.
func (f *Forge) searchThreads(ctx context.Context, kind searchKind, query string, state *cursor, through *time.Time,
	owners forge.OwnerFilter, page func([]forge.ThreadRef, []byte) error,
) error {
	base := query + ownerQualifiers(owners)
	windowStart := *through
	if !windowStart.IsZero() {
		windowStart = windowStart.Add(-searchOverlap)
	}
	for {
		q := base + " sort:updated-asc"
		if !windowStart.IsZero() {
			q += " updated:>=" + windowStart.UTC().Format(time.RFC3339)
		}
		lastRead := windowStart
		read, total, err := f.readSearch(ctx, kind, q, func(nodes []searchNode) error {
			refs := searchRefs(nodes, owners)
			for _, node := range nodes {
				lastRead = later(lastRead, node.UpdatedAt)
				*through = later(*through, node.UpdatedAt)
			}
			return page(refs, state.encode())
		})
		if err != nil {
			return err
		}
		if read < searchWindow || total <= read {
			return nil
		}
		if !lastRead.After(windowStart) {
			// The whole window was updated in one second: read that second
			// by creation time, then move past it.
			if err := f.exhaustSecond(ctx, kind, base, windowStart, owners, state, page); err != nil {
				return err
			}
			lastRead = windowStart.Add(time.Second)
			*through = later(*through, lastRead)
		}
		windowStart = lastRead
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// readSearch reads every page of one search, at most searchWindow results,
// and reports how many it read and how many match.
func (f *Forge) readSearch(ctx context.Context, kind searchKind, q string, each func([]searchNode) error) (int, int, error) {
	read, total := 0, 0
	var after any
	for {
		var data struct {
			Search searchPage `json:"search"`
		}
		if err := f.client.graphQL(ctx, graphQLRequest{
			OperationName: "SearchThreads", Query: searchThreadsQuery,
			Variables: map[string]any{"q": q, "type": string(kind), "after": after},
		}, &data); err != nil {
			return read, total, fmt.Errorf("search GitHub %s threads: %w", strings.ToLower(string(kind)), err)
		}
		total = max(data.Search.IssueCount, data.Search.DiscussionCount)
		read += len(data.Search.Nodes)
		if err := each(data.Search.Nodes); err != nil {
			return read, total, err
		}
		if !data.Search.PageInfo.HasNextPage || len(data.Search.Nodes) == 0 {
			return read, total, nil
		}
		after = data.Search.PageInfo.EndCursor
	}
}

// exhaustSecond reads every thread updated in one second by bisecting
// their creation time until each range fits in one search.
func (f *Forge) exhaustSecond(ctx context.Context, kind searchKind, base string, second time.Time,
	owners forge.OwnerFilter, state *cursor, page func([]forge.ThreadRef, []byte) error,
) error {
	at := second.UTC().Format(time.RFC3339)
	var bisect func(from, to time.Time) error
	bisect = func(from, to time.Time) error {
		q := fmt.Sprintf("%s updated:%s..%s created:%s..%s", base, at, at,
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
		read, total, err := f.readSearch(ctx, kind, q, func(nodes []searchNode) error {
			return page(searchRefs(nodes, owners), state.encode())
		})
		if err != nil || read >= total {
			return err
		}
		if !to.After(from) {
			return fmt.Errorf("more than %d GitHub threads were created and updated at %s; discovery can't read past them", searchWindow, at)
		}
		middle := from.Add(to.Sub(from) / 2).Truncate(time.Second)
		if err := bisect(from, middle); err != nil {
			return err
		}
		return bisect(middle.Add(time.Second), to)
	}
	return bisect(time.Unix(0, 0), second)
}

// ownerQualifiers restricts a search to the allowed owners, so an excluded
// owner's threads never reach msgvault. GitHub ORs repeated user:
// qualifiers.
func ownerQualifiers(owners forge.OwnerFilter) string {
	var b strings.Builder
	for _, owner := range owners.Include() {
		b.WriteString(" user:" + owner)
	}
	for _, owner := range owners.Exclude() {
		b.WriteString(" -user:" + owner)
	}
	return b.String()
}

func searchRefs(nodes []searchNode, owners forge.OwnerFilter) []forge.ThreadRef {
	refs := make([]forge.ThreadRef, 0, len(nodes))
	for _, node := range nodes {
		owner := node.Repository.Owner.Login
		if node.Number <= 0 || !owners.Allows(owner) {
			continue
		}
		ref := forge.ThreadRef{
			Owner: owner, Repo: owner + "/" + node.Repository.Name, Number: node.Number, UpdatedAt: node.UpdatedAt,
		}
		switch node.Typename {
		case "Issue":
			ref.Kind = forge.KindIssue
		case "PullRequest":
			ref.Kind = forge.KindPullRequest
		case "Discussion":
			ref.Kind = forge.KindDiscussion
		default:
			continue
		}
		ref.Key = threadKey(ref.Repo, ref.Number)
		refs = append(refs, ref)
	}
	return refs
}

// discussionWindow is how long before its notification a discussion may
// have been updated last; the list is read back that far.
const discussionWindow = 30 * 24 * time.Hour

var errDiscussionNotFound = errors.New("no discussion with that title")

const recentDiscussionsQuery = `query RecentDiscussions($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    discussions(first: 100, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { number title updatedAt }
    }
  }
}`

// findDiscussions returns the numbers of the discussions a notification may
// name: those with its exact title among the repository's discussions, read
// most recently updated first, back to discussionWindow before the
// notification. A notification names only a title, so when two discussions
// share it, both are returned rather than one left out. The list, unlike
// search, has no indexing lag, so a title it doesn't hold names no
// discussion.
func (f *Forge) findDiscussions(ctx context.Context, repo, title string, updated time.Time) ([]int, error) {
	owner, name, _ := strings.Cut(repo, "/")
	floor := updated.Add(-discussionWindow)
	var numbers []int
	found := func() ([]int, error) {
		if len(numbers) == 0 {
			return nil, errDiscussionNotFound
		}
		return numbers, nil
	}
	var after any
	for {
		var data struct {
			Repository *struct {
				Discussions struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						Number    int       `json:"number"`
						Title     string    `json:"title"`
						UpdatedAt time.Time `json:"updatedAt"`
					} `json:"nodes"`
				} `json:"discussions"`
			} `json:"repository"`
		}
		err := f.client.graphQL(ctx, graphQLRequest{
			OperationName: "RecentDiscussions", Query: recentDiscussionsQuery,
			Variables: map[string]any{"owner": owner, "name": name, "after": after},
		}, &data)
		var errs *graphQLResponseError
		switch {
		case errors.As(err, &errs) && errs.missingAt("repository"), errors.Is(err, forge.ErrUnavailable):
			return nil, errDiscussionNotFound
		case err != nil:
			return nil, err
		}
		if data.Repository == nil {
			return nil, errDiscussionNotFound
		}
		list := data.Repository.Discussions
		for _, node := range list.Nodes {
			if node.Title == title && node.Number > 0 {
				numbers = append(numbers, node.Number)
			}
			if node.UpdatedAt.Before(floor) {
				return found()
			}
		}
		if !list.PageInfo.HasNextPage {
			return found()
		}
		after = list.PageInfo.EndCursor
	}
}
