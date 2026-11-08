package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// notificationsPerPage is the most GitHub's notifications API allows.
	notificationsPerPage = 50
	searchPerPage        = 100
	// searchWindow is the most results one GitHub search returns; a longer
	// history is searched in windows of updated time.
	searchWindow = 1000
	// maxTitleQueryRunes keeps a discussion title within GitHub's search
	// query limit; the exact title is compared afterwards.
	maxTitleQueryRunes = 120
)

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

// notificationScan is what one pass over the notifications found.
type notificationScan struct {
	Threads []Thread
	// Discussions are discussion threads, whose notifications name no
	// number; lookupDiscussions finds it by title.
	Discussions []discussionNotification
	// LastModified and Latest advance the notification cursor.
	LastModified string
	Latest       time.Time
	NotModified  bool
	// Excluded counts threads of excluded owners; Unsupported counts
	// threads of kinds with no conversation to archive, such as CI runs.
	Excluded    int
	Unsupported int
}

type discussionNotification struct {
	Owner, Repo, Title string
	UpdatedAt          time.Time
	Reason             string
}

// scanNotifications lists every notification thread updated after since,
// read or not. lastModified makes an unchanged list a 304, which GitHub
// doesn't count against the rate limit.
func (c *Client) scanNotifications(ctx context.Context, since time.Time, lastModified string, owners OwnerFilter) (notificationScan, error) {
	query := url.Values{"all": {"true"}, "per_page": {strconv.Itoa(notificationsPerPage)}}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	header := http.Header{}
	if lastModified != "" {
		header.Set("If-Modified-Since", lastModified)
	}
	scan := notificationScan{Latest: since}
	next := "/notifications"
	for page := 0; next != ""; page++ {
		var batch []notification
		resp, err := c.getJSON(ctx, next, query, header, &batch)
		if err != nil {
			return notificationScan{}, fmt.Errorf("list GitHub notifications: %w", err)
		}
		if page == 0 {
			if resp.StatusCode == http.StatusNotModified {
				scan.NotModified = true
				scan.LastModified = lastModified
				return scan, nil
			}
			scan.LastModified = resp.Header.Get("Last-Modified")
		}
		for _, n := range batch {
			if n.UpdatedAt.After(scan.Latest) {
				scan.Latest = n.UpdatedAt
			}
			scan.add(n, owners)
		}
		// The next link carries the query; only the first page is conditional.
		next, query, header = nextLink(resp.Header.Get("Link")), nil, nil
	}
	return scan, nil
}

func (s *notificationScan) add(n notification, owners OwnerFilter) {
	owner, repo := n.Repository.Owner.Login, n.Repository.Name
	if !owners.Allows(owner) {
		s.Excluded++
		return
	}
	thread := Thread{Owner: owner, Repo: repo, UpdatedAt: n.UpdatedAt, Reason: n.Reason}
	switch n.Subject.Type {
	case "Issue", "PullRequest":
		number, ok := subjectNumber(n.Subject.URL)
		if !ok {
			s.Unsupported++
			return
		}
		thread.Kind, thread.Number = KindIssue, number
		if n.Subject.Type == "PullRequest" {
			thread.Kind = KindPullRequest
		}
	case "Release":
		if n.Subject.URL == "" {
			s.Unsupported++
			return
		}
		thread.Kind, thread.ReleaseURL = KindRelease, n.Subject.URL
	case "Discussion":
		s.Discussions = append(s.Discussions, discussionNotification{
			Owner: owner, Repo: repo, Title: n.Subject.Title, UpdatedAt: n.UpdatedAt, Reason: n.Reason,
		})
		return
	default:
		s.Unsupported++
		return
	}
	s.Threads = append(s.Threads, thread)
}

// subjectNumber reads the number from an issue or pull request API URL,
// .../repos/{owner}/{repo}/issues/{number} or .../pulls/{number}.
func subjectNumber(subjectURL string) (int, bool) {
	parsed, err := url.Parse(subjectURL)
	if err != nil {
		return 0, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return 0, false
	}
	switch parts[len(parts)-2] {
	case "issues", "pulls":
	default:
		return 0, false
	}
	number, err := strconv.Atoi(parts[len(parts)-1])
	return number, err == nil && number > 0
}

// nextLink returns the rel="next" URL of a Link header, or "".
func nextLink(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

const findDiscussionQuery = `query FindDiscussion($q: String!) {
  search(type: DISCUSSION, query: $q, first: 20) {
    nodes { ... on Discussion { number title } }
  }
}`

// findDiscussion returns the number of the discussion a notification
// names, found by its exact title among the repository's discussions
// updated since the notification's day.
func (c *Client) findDiscussion(ctx context.Context, d discussionNotification) (int, error) {
	title := strings.ReplaceAll(d.Title, `"`, " ")
	if runes := []rune(title); len(runes) > maxTitleQueryRunes {
		title = string(runes[:maxTitleQueryRunes])
	}
	q := fmt.Sprintf(`repo:%s/%s in:title "%s" updated:>=%s`,
		d.Owner, d.Repo, strings.TrimSpace(title), d.UpdatedAt.UTC().Format(time.DateOnly))
	var data struct {
		Search struct {
			Nodes []struct {
				Number int    `json:"number"`
				Title  string `json:"title"`
			} `json:"nodes"`
		} `json:"search"`
	}
	if err := c.graphQL(ctx, graphQLRequest{
		OperationName: "FindDiscussion", Query: findDiscussionQuery, Variables: map[string]any{"q": q},
	}, &data); err != nil {
		return 0, err
	}
	for _, node := range data.Search.Nodes {
		if node.Title == d.Title && node.Number > 0 {
			return node.Number, nil
		}
	}
	return 0, fmt.Errorf("%w: no discussion titled %q in %s/%s", ErrUnavailable, d.Title, d.Owner, d.Repo)
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

// searchScan is what one involvement search found.
type searchScan struct {
	Threads []Thread
	// Through is the latest update the search covered.
	Through  time.Time
	Excluded int
}

// searchInvolvement finds the threads of kind that login authored,
// commented on, was assigned or was mentioned in, updated at or after
// from. GitHub returns at most 1000 results per search, so a longer
// history is read in windows that each start where the last one ended.
func (c *Client) searchInvolvement(ctx context.Context, kind searchKind, login string, from time.Time, owners OwnerFilter) (searchScan, error) {
	base := "involves:" + login + owners.searchQualifiers() + " sort:updated-asc"
	scan := searchScan{Through: from}
	seen := map[string]bool{}
	for {
		q := base
		if !scan.Through.IsZero() {
			q += " updated:>=" + scan.Through.UTC().Format(time.RFC3339)
		}
		windowStart := scan.Through
		read, total := 0, 0
		var after any
		for {
			var data struct {
				Search searchPage `json:"search"`
			}
			if err := c.graphQL(ctx, graphQLRequest{
				OperationName: "SearchThreads", Query: searchThreadsQuery,
				Variables: map[string]any{"q": q, "type": string(kind), "after": after},
			}, &data); err != nil {
				return scan, fmt.Errorf("search GitHub %s threads: %w", strings.ToLower(string(kind)), err)
			}
			total = max(data.Search.IssueCount, data.Search.DiscussionCount)
			for _, node := range data.Search.Nodes {
				read++
				scan.addSearchNode(node, owners, seen)
			}
			if !data.Search.PageInfo.HasNextPage || len(data.Search.Nodes) == 0 {
				break
			}
			after = data.Search.PageInfo.EndCursor
		}
		if total <= read || read < searchWindow {
			return scan, nil
		}
		if !scan.Through.After(windowStart) {
			// A whole window updated in one second: step past it rather
			// than read the same window forever.
			scan.Through = windowStart.Add(time.Second)
		}
	}
}

func (s *searchScan) addSearchNode(node searchNode, owners OwnerFilter, seen map[string]bool) {
	if node.UpdatedAt.After(s.Through) {
		s.Through = node.UpdatedAt
	}
	owner := node.Repository.Owner.Login
	if !owners.Allows(owner) {
		s.Excluded++
		return
	}
	thread := Thread{Owner: owner, Repo: node.Repository.Name, Number: node.Number, UpdatedAt: node.UpdatedAt}
	switch node.Typename {
	case "Issue":
		thread.Kind = KindIssue
	case "PullRequest":
		thread.Kind = KindPullRequest
	case "Discussion":
		thread.Kind = KindDiscussion
	default:
		return
	}
	if thread.Number <= 0 || seen[thread.Key()] {
		return
	}
	seen[thread.Key()] = true
	s.Threads = append(s.Threads, thread)
}

// isRateLimit reports whether err is a rate limit the run should wait out.
func isRateLimit(err error) bool {
	var limited *RateLimitError
	return errors.As(err, &limited)
}
