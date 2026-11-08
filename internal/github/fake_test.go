package github

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeGitHub serves the REST and GraphQL calls a sync makes, from canned
// threads, and records every request so tests can say what was fetched.
type fakeGitHub struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// scopes is the X-OAuth-Scopes header; noScopes drops it, as GitHub
	// does for fine-grained tokens.
	scopes   string
	noScopes bool
	viewer   Viewer

	notifications    []notification
	lastModified     string
	notificationPage int

	threads     map[string]issueThread
	discussions map[string]discussionThread
	releases    map[string]release
	// pages answers node(id:) page queries, keyed by operation, node ID and
	// cursor.
	pages map[string]any

	issueSearch      []searchNode
	discussionSearch []searchNode

	// graphQLRemaining is the GraphQL rate limit left; each query uses one.
	graphQLRemaining int
	graphQLReset     time.Time

	requests      []string
	searchQueries []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{
		t:                t,
		scopes:           "notifications, repo",
		viewer:           Viewer{ID: 1001, Login: "octo"},
		lastModified:     "Sat, 07 Nov 2026 10:00:00 GMT",
		notificationPage: 2,
		threads:          map[string]issueThread{},
		discussions:      map[string]discussionThread{},
		releases:         map[string]release{},
		pages:            map[string]any{},
		graphQLRemaining: 5000,
		graphQLReset:     time.Now().Add(time.Hour),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) client() *Client {
	return NewClient(f.server.URL, "test-token", "msgvault-test")
}

func (f *fakeGitHub) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(f.t, "Bearer test-token", r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/user":
		f.requests = append(f.requests, "GET /user")
		if !f.noScopes {
			w.Header().Set("X-OAuth-Scopes", f.scopes)
		}
		f.writeJSON(w, f.viewer)
	case r.Method == http.MethodGet && r.URL.Path == "/notifications":
		f.serveNotifications(w, r)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases/"):
		f.requests = append(f.requests, "GET "+r.URL.Path)
		rel, ok := f.releases[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		f.writeJSON(w, rel)
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		f.serveGraphQL(w, r)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeGitHub) serveNotifications(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	f.requests = append(f.requests, "GET /notifications?"+query.Encode())
	require.Equal(f.t, "true", query.Get("all"))
	if since := r.Header.Get("If-Modified-Since"); since != "" && since == f.lastModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	var since time.Time
	if raw := query.Get("since"); raw != "" {
		var err error
		since, err = time.Parse(time.RFC3339, raw)
		require.NoError(f.t, err)
	}
	var matching []notification
	for _, n := range f.notifications {
		if n.UpdatedAt.After(since) {
			matching = append(matching, n)
		}
	}
	page := 1
	if raw := query.Get("page"); raw != "" {
		page, _ = strconv.Atoi(raw)
	}
	start := min((page-1)*f.notificationPage, len(matching))
	end := min(start+f.notificationPage, len(matching))
	if end < len(matching) {
		next := *r.URL
		q := next.Query()
		q.Set("page", strconv.Itoa(page+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, f.server.URL, next.RequestURI()))
	}
	w.Header().Set("Last-Modified", f.lastModified)
	f.writeJSON(w, matching[start:end])
}

var (
	updatedSince = regexp.MustCompile(`updated:>=(\S+)`)
	excludedUser = regexp.MustCompile(`-user:(\S+)`)
	repoTitle    = regexp.MustCompile(`repo:(\S+)/(\S+) in:title "([^"]*)"`)
)

func (f *fakeGitHub) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var req graphQLRequest
	require.NoError(f.t, json.UnmarshalRead(r.Body, &req))
	vars := req.Variables
	name := fmt.Sprintf("%s/%s#%v", vars["owner"], vars["name"], vars["number"])
	switch req.OperationName {
	case "IssueThread", "DiscussionThread":
		f.requests = append(f.requests, req.OperationName+" "+name)
	case "SearchThreads", "FindDiscussion":
		f.requests = append(f.requests, fmt.Sprintf("%s %v", req.OperationName, vars["q"]))
	default:
		f.requests = append(f.requests, fmt.Sprintf("%s %v %v", req.OperationName, vars["id"], vars["after"]))
	}
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(f.graphQLRemaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.graphQLReset.Unix(), 10))
	if f.graphQLRemaining <= 0 {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	f.graphQLRemaining--

	switch req.OperationName {
	case "SearchThreads":
		f.serveSearch(w, vars)
	case "FindDiscussion":
		q, _ := vars["q"].(string)
		match := repoTitle.FindStringSubmatch(q)
		require.NotNil(f.t, match, q)
		var nodes []map[string]any
		for key, d := range f.discussions {
			if strings.HasPrefix(key, match[1]+"/"+match[2]+"#") && strings.Contains(d.Title, strings.TrimSpace(match[3])) {
				nodes = append(nodes, map[string]any{"number": d.Number, "title": d.Title})
			}
		}
		f.writeData(w, map[string]any{"search": map[string]any{"nodes": nodes}})
	case "IssueThread":
		thread, ok := f.threads[name]
		if !ok {
			f.writeNotFound(w, map[string]any{"repository": map[string]any{"issueOrPullRequest": nil}})
			return
		}
		f.writeData(w, map[string]any{"repository": map[string]any{"issueOrPullRequest": thread}})
	case "DiscussionThread":
		thread, ok := f.discussions[name]
		if !ok {
			f.writeNotFound(w, map[string]any{"repository": map[string]any{"discussion": nil}})
			return
		}
		f.writeData(w, map[string]any{"repository": map[string]any{"discussion": thread}})
	default:
		key := fmt.Sprintf("%s %v %v", req.OperationName, vars["id"], vars["after"])
		page, ok := f.pages[key]
		require.True(f.t, ok, "no page for %s", key)
		f.writeData(w, map[string]any{"node": map[string]any{"page": page}})
	}
}

func (f *fakeGitHub) serveSearch(w http.ResponseWriter, vars map[string]any) {
	q, _ := vars["q"].(string)
	f.searchQueries = append(f.searchQueries, q)
	nodes := f.issueSearch
	if vars["type"] == string(searchDiscussions) {
		nodes = f.discussionSearch
	}
	var since time.Time
	if match := updatedSince.FindStringSubmatch(q); match != nil {
		var err error
		since, err = time.Parse(time.RFC3339, match[1])
		require.NoError(f.t, err)
	}
	excluded := map[string]bool{}
	for _, match := range excludedUser.FindAllStringSubmatch(q, -1) {
		excluded[match[1]] = true
	}
	var matching []searchNode
	for _, node := range nodes {
		if !node.UpdatedAt.Before(since) && !excluded[strings.ToLower(node.Repository.Owner.Login)] {
			matching = append(matching, node)
		}
	}
	slices.SortStableFunc(matching, func(a, b searchNode) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	total := len(matching)
	// GitHub returns at most 1000 results for one search.
	matching = matching[:min(len(matching), searchWindow)]
	offset := 0
	if after, ok := vars["after"].(string); ok {
		offset, _ = strconv.Atoi(after)
	}
	end := min(offset+searchPerPage, len(matching))
	page := searchPage{IssueCount: total, Nodes: matching[offset:end]}
	if vars["type"] == string(searchDiscussions) {
		page.IssueCount, page.DiscussionCount = 0, total
	}
	page.PageInfo.HasNextPage = end < len(matching)
	page.PageInfo.EndCursor = strconv.Itoa(end)
	f.writeData(w, map[string]any{"search": page})
}

func (f *fakeGitHub) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	require.NoError(f.t, json.MarshalWrite(w, value))
}

func (f *fakeGitHub) writeData(w http.ResponseWriter, data any) {
	f.writeJSON(w, map[string]any{"data": data})
}

func (f *fakeGitHub) writeNotFound(w http.ResponseWriter, data any) {
	f.writeJSON(w, map[string]any{
		"data":   data,
		"errors": []map[string]any{{"type": "NOT_FOUND", "message": "Could not resolve to an object."}},
	})
}

// Builders for canned GitHub objects.

func ts(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func user(login string, id int64) *actor {
	return &actor{Login: login, URL: "https://github.com/" + login, DatabaseID: id}
}

func notified(kind, owner, repo, title, subjectURL, reason, updated string) notification {
	var n notification
	n.Reason = reason
	n.UpdatedAt = ts(updated)
	n.Subject.Title = title
	n.Subject.URL = subjectURL
	n.Subject.Type = kind
	n.Repository.Name = repo
	n.Repository.Owner.Login = owner
	return n
}

func found(typename, owner, repo string, number int, updated string) searchNode {
	node := searchNode{Typename: typename, Number: number, UpdatedAt: ts(updated)}
	node.Repository.Name = repo
	node.Repository.Owner.Login = owner
	return node
}

func comments(nodes ...issueComment) connection[issueComment] {
	return connection[issueComment]{Nodes: nodes}
}
