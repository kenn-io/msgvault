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
	// maxTitleQueryRunes keeps a discussion title within GitHub's search
	// query limit; the exact title is compared afterwards.
	maxTitleQueryRunes = 120
)

// cursor is how far each discovery stream came.
type cursor struct {
	// Notifications advance only after a scan read every page.
	Notifications notificationCursor `json:"notifications,omitzero"`
	// Each search's through is the latest update it has read. Results come
	// oldest first, so everything older was read too, and the next page or
	// sync continues from there.
	IssuesThrough          time.Time `json:"issues_through,omitzero"`
	DiscussionsThrough     time.Time `json:"discussions_through,omitzero"`
	ReviewedThrough        time.Time `json:"reviewed_through,omitzero"`
	ReviewRequestedThrough time.Time `json:"review_requested_through,omitzero"`
}

type notificationCursor struct {
	// LastModified is the list's Last-Modified header, sent back as
	// If-Modified-Since, which GitHub doesn't count against the rate limit.
	LastModified string `json:"last_modified,omitempty"`
	// Since is the latest thread update the last complete scan showed.
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
	if err := f.scanNotifications(ctx, &state, owners, page); err != nil {
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
// the threads it found are already queued.
func (f *Forge) scanNotifications(ctx context.Context, state *cursor, owners forge.OwnerFilter,
	page func([]forge.ThreadRef, []byte) error,
) error {
	query := url.Values{"all": {"true"}, "per_page": {strconv.Itoa(notificationsPerPage)}}
	if !state.Notifications.Since.IsZero() {
		query.Set("since", state.Notifications.Since.UTC().Format(time.RFC3339))
	}
	header := http.Header{}
	if state.Notifications.LastModified != "" {
		header.Set("If-Modified-Since", state.Notifications.LastModified)
	}
	latest := state.Notifications.Since
	lastModified := ""
	next := "/notifications"
	for first := true; next != ""; first = false {
		var batch []notification
		resp, err := f.client.getJSON(ctx, next, query, header, &batch)
		if err != nil {
			return fmt.Errorf("list GitHub notifications: %w", err)
		}
		if first {
			if resp.Status == http.StatusNotModified {
				return nil
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
				if ref, err = f.resolveDiscussion(ctx, ref); err != nil {
					return err
				}
			}
			refs = append(refs, ref)
		}
		// The next link carries the query; only the first page is conditional.
		next, query, header = forge.NextLink(resp.Header.Get("Link")), nil, nil
		if next == "" {
			state.Notifications = notificationCursor{LastModified: lastModified, Since: latest}
		}
		if err := page(refs, state.encode()); err != nil {
			return err
		}
	}
	return nil
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
		// The number is resolved from the title, see resolveDiscussion.
		ref.Kind, ref.Locator = forge.KindDiscussion, "title:"+n.Subject.Title
		ref.Key = strings.ToLower(ref.Repo) + "#title:" + n.Subject.Title
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
// after *through, oldest first. GitHub returns at most 1,000 results per
// search, so a longer history is read in windows, each starting at the last
// update the previous one read. *through advances after every page.
func (f *Forge) searchThreads(ctx context.Context, kind searchKind, query string, state *cursor, through *time.Time,
	owners forge.OwnerFilter, page func([]forge.ThreadRef, []byte) error,
) error {
	base := query + ownerQualifiers(owners)
	for {
		windowStart := *through
		q := base + " sort:updated-asc"
		if !windowStart.IsZero() {
			q += " updated:>=" + windowStart.UTC().Format(time.RFC3339)
		}
		read, total, err := f.readSearch(ctx, kind, q, func(nodes []searchNode) error {
			refs := searchRefs(nodes, owners)
			for _, node := range nodes {
				if node.UpdatedAt.After(*through) {
					*through = node.UpdatedAt
				}
			}
			return page(refs, state.encode())
		})
		if err != nil {
			return err
		}
		if read < searchWindow || total <= read {
			return nil
		}
		if !through.After(windowStart) {
			// The whole window was updated in one second: read that second
			// by creation time, then move past it.
			if err := f.exhaustSecond(ctx, kind, base, windowStart, owners, state, page); err != nil {
				return err
			}
			*through = windowStart.Add(time.Second)
		}
	}
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

// discussionIndexLag is how long a new or renamed discussion may take to
// appear in GitHub's search; a title not found for longer names no
// discussion any more.
const discussionIndexLag = 24 * time.Hour

var errDiscussionNotFound = errors.New("no discussion with that title")

// resolveDiscussion gives a notified discussion its number, so it is the
// same thread a search finds. One not found yet keeps its title: Fetch
// tries again.
func (f *Forge) resolveDiscussion(ctx context.Context, ref forge.ThreadRef) (forge.ThreadRef, error) {
	number, err := f.findDiscussion(ctx, ref)
	switch {
	case errors.Is(err, errDiscussionNotFound):
		return ref, nil
	case err != nil:
		return forge.ThreadRef{}, err
	}
	ref.Number, ref.Locator = number, ""
	ref.Key = threadKey(ref.Repo, number)
	return ref, nil
}

const findDiscussionQuery = `query FindDiscussion($q: String!, $after: String) {
  search(type: DISCUSSION, query: $q, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { ... on Discussion { number title } }
  }
}`

// findDiscussion returns the number of the discussion a notification names,
// by its exact title among the repository's discussions updated since the
// notification's day.
func (f *Forge) findDiscussion(ctx context.Context, ref forge.ThreadRef) (int, error) {
	title := strings.TrimPrefix(ref.Locator, "title:")
	words := strings.ReplaceAll(title, `"`, " ")
	if runes := []rune(words); len(runes) > maxTitleQueryRunes {
		words = string(runes[:maxTitleQueryRunes])
	}
	q := fmt.Sprintf(`repo:%s in:title "%s" updated:>=%s`,
		ref.Repo, strings.TrimSpace(words), ref.UpdatedAt.UTC().Format(time.DateOnly))
	var after any
	for {
		var data struct {
			Search struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []struct {
					Number int    `json:"number"`
					Title  string `json:"title"`
				} `json:"nodes"`
			} `json:"search"`
		}
		if err := f.client.graphQL(ctx, graphQLRequest{
			OperationName: "FindDiscussion", Query: findDiscussionQuery,
			Variables: map[string]any{"q": q, "after": after},
		}, &data); err != nil {
			return 0, err
		}
		for _, node := range data.Search.Nodes {
			if node.Title == title && node.Number > 0 {
				return node.Number, nil
			}
		}
		if !data.Search.PageInfo.HasNextPage {
			return 0, errDiscussionNotFound
		}
		after = data.Search.PageInfo.EndCursor
	}
}
