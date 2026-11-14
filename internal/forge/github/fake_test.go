package github

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeGitHub serves the REST and GraphQL calls a sync makes, from canned
// data, and records every request so tests can say what was fetched.
type fakeGitHub struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// scopes is the X-OAuth-Scopes header; noScopes drops it, as GitHub
	// does for fine-grained tokens.
	scopes   string
	noScopes bool
	userID   int64
	login    string

	notifications    []notification
	lastModified     string
	notificationPage int

	// repoOwners overrides a repository's current owner, by "owner/name".
	repoOwners map[string]string
	// threads and discussions are GraphQL objects by "owner/name#number".
	threads     map[string]map[string]any
	discussions map[string]map[string]any
	releases    map[string]map[string]any
	// pages answers node(id:) page queries, by "operation id after".
	pages map[string]map[string]any
	// gqlErrors adds errors to a thread query's response, by "operation key".
	gqlErrors map[string][]map[string]any
	// refuse answers a GraphQL operation with a 403 body, by operation.
	refuse map[string]string
	// fail answers a GraphQL operation with a server error, by operation.
	fail map[string]bool

	issueSearch      []searchNode
	discussionSearch []searchNode
	// reviewedSearch and reviewRequestedSearch answer the review searches.
	reviewedSearch        []searchNode
	reviewRequestedSearch []searchNode
	created               map[int]time.Time
	// discussionPages splits RecentDiscussions results into pages of this size.
	discussionPages int

	// graphQLRemaining is the GraphQL limit left; each query uses one.
	graphQLRemaining int
	graphQLReset     time.Time

	requests      []string
	searchQueries []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:                t,
		scopes:           "notifications, repo",
		userID:           1001,
		login:            "octo",
		lastModified:     "Sat, 07 Nov 2026 10:00:00 GMT",
		notificationPage: 2,
		repoOwners:       map[string]string{},
		threads:          map[string]map[string]any{},
		discussions:      map[string]map[string]any{},
		releases:         map[string]map[string]any{},
		pages:            map[string]map[string]any{},
		gqlErrors:        map[string][]map[string]any{},
		refuse:           map[string]string{},
		fail:             map[string]bool{},
		created:          map[int]time.Time{},
		discussionPages:  100,
		graphQLRemaining: 5000,
		graphQLReset:     time.Now().Add(time.Hour),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) forge() *Forge {
	base, err := url.Parse(f.server.URL)
	if err != nil {
		panic(err)
	}
	return New(base, "test-token", "msgvault-test")
}

func (f *fakeGitHub) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeGitHub) log(entry string) {
	f.requests = append(f.requests, entry)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(f.t, "Bearer test-token", r.Header.Get("Authorization"), "%s %s", r.Method, r.URL)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/user":
		f.log("GET /user")
		if !f.noScopes {
			w.Header().Set("X-Oauth-Scopes", f.scopes)
		}
		f.writeJSON(w, map[string]any{"id": f.userID, "login": f.login, "html_url": "https://github.com/" + f.login})
	case r.Method == http.MethodGet && r.URL.Path == "/notifications":
		f.serveNotifications(w, r)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases/"):
		f.log("GET " + r.URL.Path)
		rel, ok := f.releases[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		f.writeJSON(w, rel)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/"):
		f.log("GET " + r.URL.Path)
		repo := strings.TrimPrefix(r.URL.Path, "/repos/")
		owner, _, _ := strings.Cut(repo, "/")
		if moved, ok := f.repoOwners[repo]; ok {
			owner = moved
		}
		f.writeJSON(w, map[string]any{"full_name": repo, "owner": map[string]any{"login": owner}})
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		f.serveGraphQL(w, r)
	default:
		assert.Fail(f.t, "unexpected request", "%s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeGitHub) serveNotifications(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	f.log("GET /notifications?" + query.Encode())
	if since := r.Header.Get("If-Modified-Since"); since != "" && since == f.lastModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	var since time.Time
	if raw := query.Get("since"); raw != "" {
		since, _ = time.Parse(time.RFC3339, raw)
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
	updatedRange = regexp.MustCompile(`updated:(\S+)\.\.(\S+)`)
	createdRange = regexp.MustCompile(`created:(\S+)\.\.(\S+)`)
	excludedUser = regexp.MustCompile(`-user:(\S+)`)
)

func (f *fakeGitHub) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var req graphQLRequest
	if !assert.NoError(f.t, json.UnmarshalRead(r.Body, &req), "decode GraphQL request") {
		return
	}
	vars := req.Variables
	key := fmt.Sprintf("%v/%v#%v", vars["owner"], vars["name"], vars["number"])
	switch req.OperationName {
	case "IssueThread", "DiscussionThread":
		f.log(req.OperationName + " " + key)
	case "SearchThreads":
		f.log(fmt.Sprintf("%s %v", req.OperationName, vars["q"]))
	case "RecentDiscussions":
		f.log(fmt.Sprintf("%s %v/%v", req.OperationName, vars["owner"], vars["name"]))
	default:
		f.log(fmt.Sprintf("%s %v %v", req.OperationName, vars["id"], vars["after"]))
	}
	assert.NotContains(f.t, req.Query, "{{email}}", "unexpanded email placeholder in %s", req.OperationName)
	w.Header().Set("X-Ratelimit-Resource", "graphql")
	w.Header().Set("X-Ratelimit-Limit", "5000")
	w.Header().Set("X-Ratelimit-Remaining", strconv.Itoa(f.graphQLRemaining))
	w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(f.graphQLReset.Unix(), 10))
	if f.fail[req.OperationName] {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if body, ok := f.refuse[req.OperationName]; ok {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, body)
		return
	}
	if f.graphQLRemaining <= 0 {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	f.graphQLRemaining--
	errs := f.gqlErrors[req.OperationName+" "+key]

	switch req.OperationName {
	case "SearchThreads":
		f.serveSearch(w, vars)
	case "RecentDiscussions":
		f.serveRecentDiscussions(w, vars)
	case "IssueThread":
		thread, ok := f.threads[key]
		if !ok {
			f.writeGraphQL(w, map[string]any{"repository": map[string]any{"issueOrPullRequest": nil}},
				[]map[string]any{{"type": "NOT_FOUND", "message": "Could not resolve to an object.", "path": []any{"repository", "issueOrPullRequest"}}})
			return
		}
		f.writeGraphQL(w, map[string]any{"repository": map[string]any{"issueOrPullRequest": thread}}, errs)
	case "DiscussionThread":
		thread, ok := f.discussions[key]
		if !ok {
			f.writeGraphQL(w, map[string]any{"repository": map[string]any{"discussion": nil}},
				[]map[string]any{{"type": "NOT_FOUND", "message": "Could not resolve to a Discussion.", "path": []any{"repository", "discussion"}}})
			return
		}
		f.writeGraphQL(w, map[string]any{"repository": map[string]any{"discussion": thread}}, errs)
	default:
		page, ok := f.pages[fmt.Sprintf("%s %v %v", req.OperationName, vars["id"], vars["after"])]
		if !assert.True(f.t, ok, "no page for %s %v %v", req.OperationName, vars["id"], vars["after"]) {
			return
		}
		f.writeGraphQL(w, map[string]any{"node": map[string]any{"page": page}}, nil)
	}
}

func (f *fakeGitHub) serveSearch(w http.ResponseWriter, vars map[string]any) {
	q, _ := vars["q"].(string)
	f.searchQueries = append(f.searchQueries, q)
	nodes := f.issueSearch
	switch {
	case vars["type"] == string(searchDiscussions):
		nodes = f.discussionSearch
	case strings.Contains(q, "reviewed-by:"):
		nodes = f.reviewedSearch
	case strings.Contains(q, "review-requested:"):
		nodes = f.reviewRequestedSearch
	}
	parse := func(raw string) time.Time {
		at, err := time.Parse(time.RFC3339, raw)
		assert.NoError(f.t, err, "parse %q", raw)
		return at
	}
	excluded := map[string]bool{}
	for _, match := range excludedUser.FindAllStringSubmatch(q, -1) {
		excluded[match[1]] = true
	}
	var matching []searchNode
	for _, node := range nodes {
		if excluded[strings.ToLower(node.Repository.Owner.Login)] {
			continue
		}
		if match := updatedSince.FindStringSubmatch(q); match != nil && node.UpdatedAt.Before(parse(match[1])) {
			continue
		}
		if match := updatedRange.FindStringSubmatch(q); match != nil &&
			(node.UpdatedAt.Before(parse(match[1])) || node.UpdatedAt.After(parse(match[2]))) {
			continue
		}
		if match := createdRange.FindStringSubmatch(q); match != nil {
			created := f.created[node.Number]
			if created.Before(parse(match[1])) || created.After(parse(match[2])) {
				continue
			}
		}
		matching = append(matching, node)
	}
	// sort:updated-asc gives the oldest update first; without it, GitHub
	// ranks by relevance, which the fake stands in for with newest first.
	slices.SortStableFunc(matching, func(a, b searchNode) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	if !strings.Contains(q, "sort:updated-asc") {
		slices.Reverse(matching)
	}
	total := len(matching)
	// GitHub returns at most 1000 results for one search.
	matching = matching[:min(len(matching), searchWindow)]
	offset := 0
	if after, ok := vars["after"].(string); ok {
		offset, _ = strconv.Atoi(after)
	}
	end := min(offset+100, len(matching))
	page := searchPage{IssueCount: total, Nodes: matching[offset:end]}
	if vars["type"] == string(searchDiscussions) {
		page.IssueCount, page.DiscussionCount = 0, total
	}
	page.PageInfo.HasNextPage = end < len(matching)
	page.PageInfo.EndCursor = strconv.Itoa(end)
	f.writeGraphQL(w, map[string]any{"search": page}, nil)
}

func (f *fakeGitHub) serveRecentDiscussions(w http.ResponseWriter, vars map[string]any) {
	prefix := fmt.Sprintf("%v/%v#", vars["owner"], vars["name"])
	var nodes []map[string]any
	for key, d := range f.discussions {
		if strings.HasPrefix(key, prefix) {
			nodes = append(nodes, map[string]any{"number": d["number"], "title": d["title"], "updatedAt": d["updatedAt"]})
		}
	}
	slices.SortFunc(nodes, func(a, b map[string]any) int {
		return strings.Compare(fmt.Sprint(b["updatedAt"]), fmt.Sprint(a["updatedAt"]))
	})
	offset := 0
	if after, ok := vars["after"].(string); ok {
		offset, _ = strconv.Atoi(after)
	}
	end := min(offset+f.discussionPages, len(nodes))
	f.writeGraphQL(w, map[string]any{"repository": map[string]any{"discussions": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": end < len(nodes), "endCursor": strconv.Itoa(end)},
		"nodes":    nodes[offset:end],
	}}}, nil)
}

func (f *fakeGitHub) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(f.t, json.MarshalWrite(w, value), "encode response")
}

func (f *fakeGitHub) writeGraphQL(w http.ResponseWriter, data any, errs []map[string]any) {
	body := map[string]any{"data": data}
	if len(errs) > 0 {
		body["errors"] = errs
	}
	f.writeJSON(w, body)
}

// Builders for canned GitHub objects.

func ts(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func user(login string, id int64) map[string]any {
	return map[string]any{"__typename": "User", "login": login, "url": "https://github.com/" + login, "databaseId": id}
}

func conn(nodes ...map[string]any) map[string]any {
	if nodes == nil {
		nodes = []map[string]any{}
	}
	return map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": nodes}
}

func connMore(after string, nodes ...map[string]any) map[string]any {
	c := conn(nodes...)
	c["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": after}
	return c
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

func noUnexpectedRequests(t *testing.T, f *fakeGitHub, forbidden string) {
	t.Helper()
	for _, request := range f.requestLog() {
		if strings.Contains(strings.ToLower(request), forbidden) {
			assert.Contains(t, request, "-user:"+forbidden, "an excluded owner appears only as a search exclusion")
		}
	}
}
