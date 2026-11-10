package gitlab

import (
	"cmp"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
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
	"github.com/stretchr/testify/require"
)

// The fixtures below are the example responses in GitLab's API
// documentation (gitlab-docs a8f1a2df, doc/api/), trimmed to the fields a
// sync reads and a few beside them. Real people's names in the examples
// are replaced with synthetic ones; tests override IDs and times.

// userExample is GET /user: doc/api/users.md:589-634.
const userExample = `{
  "id": 1,
  "username": "john_smith",
  "email": "john@example.com",
  "name": "John Smith",
  "state": "active",
  "locked": false,
  "avatar_url": "http://localhost:3000/uploads/user/avatar/1/index.jpg",
  "web_url": "http://localhost:3000/john_smith",
  "created_at": "2012-05-23T08:00:58Z",
  "public_email": "john@example.com",
  "bot": false
}`

// tokenExample is GET /personal_access_tokens/self, whose attributes are
// the token list's: doc/api/personal_access_tokens.md:69-93 (one item),
// :119-120.
const tokenExample = `{
  "id": 4,
  "name": "Test Token",
  "revoked": false,
  "created_at": "2020-07-23T14:31:47.729Z",
  "description": "Test Token description",
  "scopes": ["api"],
  "user_id": 3,
  "last_used_at": "2021-10-06T17:58:37.550Z",
  "active": true,
  "expires_at": null
}`

// authorExample is a user embedded in a response: the author of
// doc/api/issues.md:930-937.
const authorExample = `{
  "state": "active",
  "web_url": "https://gitlab.example.com/root",
  "avatar_url": null,
  "username": "root",
  "id": 1,
  "name": "Administrator"
}`

// issueExample is GET /projects/:id/issues/:issue_iid:
// doc/api/issues.md:916-1000. The list example, :113-201, has the same
// fields; the fake serves this object to both.
const issueExample = `{
  "project_id": 4,
  "author": {"state": "active", "web_url": "https://gitlab.example.com/root", "avatar_url": null, "username": "root", "id": 1, "name": "Administrator"},
  "description": "Omnis vero earum sunt corporis dolor et placeat.",
  "state": "closed",
  "iid": 1,
  "assignees": [{"avatar_url": null, "web_url": "https://gitlab.example.com/lennie", "state": "active", "username": "lennie", "id": 9, "name": "Dr. Luella Kovacek"}],
  "type": "ISSUE",
  "labels": [],
  "upvotes": 4,
  "downvotes": 0,
  "merge_requests_count": 0,
  "id": 41,
  "title": "Ut commodi ullam eos dolores perferendis nihil sunt.",
  "updated_at": "2016-01-04T15:31:46.176Z",
  "created_at": "2016-01-04T15:31:46.176Z",
  "closed_at": null,
  "closed_by": null,
  "subscribed": false,
  "user_notes_count": 1,
  "web_url": "http://gitlab.example.com/my-group/my-project/issues/1",
  "references": {"short": "#1", "relative": "#1", "full": "my-group/my-project#1"},
  "confidential": false,
  "discussion_locked": false,
  "issue_type": "issue",
  "severity": "UNKNOWN"
}`

// mergeRequestExample is an item of GET /merge_requests:
// doc/api/merge_requests.md:252-373; the fake serves it alone too.
const mergeRequestExample = `{
  "id": 1,
  "iid": 1,
  "project_id": 3,
  "title": "test1",
  "description": "fixed login page css paddings",
  "state": "merged",
  "imported": false,
  "imported_from": "none",
  "merged_at": "2018-09-07T11:16:17.520Z",
  "created_at": "2017-04-29T08:46:00Z",
  "updated_at": "2017-04-29T08:46:00Z",
  "target_branch": "main",
  "source_branch": "test1",
  "upvotes": 0,
  "downvotes": 0,
  "author": {"bot": false, "id": 1, "name": "Administrator", "username": "admin", "state": "active", "avatar_url": null, "web_url": "https://gitlab.example.com/admin"},
  "reviewers": [{"id": 2, "name": "Sam Bauch", "username": "kenyatta_oconnell", "state": "active", "web_url": "http://gitlab.example.com//kenyatta_oconnell"}],
  "source_project_id": 2,
  "target_project_id": 3,
  "labels": ["Community contribution", "Manage"],
  "draft": false,
  "merge_status": "can_be_merged",
  "detailed_merge_status": "not_open",
  "sha": "8888888888888888888888888888888888888888",
  "user_notes_count": 1,
  "web_url": "http://gitlab.example.com/my-group/my-project/merge_requests/1",
  "references": {"short": "!1", "relative": "my-group/my-project!1", "full": "my-group/my-project!1"},
  "squash": false
}`

// discussionsExample is GET .../merge_requests/:iid/discussions:
// doc/api/discussions.md:913-999; the issue example, :93-171, differs
// only in noteable_type and resolvable.
//
//nolint:misspell // GitLab's own field names, as documented
const discussionsExample = `[
  {
    "id": "6a9c1750b37d513a43987b574953fceb50b03ce7",
    "individual_note": false,
    "notes": [
      {"id": 1126, "type": "DiscussionNote", "body": "discussion text", "attachment": null,
       "author": {"id": 1, "name": "root", "username": "root", "state": "active", "avatar_url": "https://www.gravatar.com/avatar/00afb8fb6ab07c3ee3e9c1f38777e2f4?s=80&d=identicon", "web_url": "http://localhost:3000/root"},
       "created_at": "2018-03-03T21:54:39.668Z", "updated_at": "2018-03-03T21:54:39.668Z", "system": false,
       "noteable_id": 3, "noteable_type": "MergeRequest", "project_id": 5, "noteable_iid": null,
       "resolved": false, "resolvable": true, "resolved_by": null, "resolved_at": null},
      {"id": 1129, "type": "DiscussionNote", "body": "reply to the discussion", "attachment": null,
       "author": {"id": 1, "name": "root", "username": "root", "state": "active", "avatar_url": "https://www.gravatar.com/avatar/00afb8fb6ab07c3ee3e9c1f38777e2f4?s=80&d=identicon", "web_url": "http://localhost:3000/root"},
       "created_at": "2018-03-04T13:38:02.127Z", "updated_at": "2018-03-04T13:38:02.127Z", "system": false,
       "noteable_id": 3, "noteable_type": "MergeRequest", "project_id": 5, "noteable_iid": null,
       "resolved": false, "resolvable": true, "resolved_by": null}
    ]
  },
  {
    "id": "87805b7c09016a7058e91bdbe7b29d1f284a39e6",
    "individual_note": true,
    "notes": [
      {"id": 1128, "type": null, "body": "a single comment", "attachment": null,
       "author": {"id": 1, "name": "root", "username": "root", "state": "active", "avatar_url": "https://www.gravatar.com/avatar/00afb8fb6ab07c3ee3e9c1f38777e2f4?s=80&d=identicon", "web_url": "http://localhost:3000/root"},
       "created_at": "2018-03-04T09:17:22.520Z", "updated_at": "2018-03-04T09:17:22.520Z", "system": false,
       "noteable_id": 3, "noteable_type": "MergeRequest", "project_id": 5, "noteable_iid": null,
       "resolved": false, "resolvable": true, "resolved_by": null}
    ]
  }
]`

// diffDiscussionExample is a diff comment's discussion:
// doc/api/discussions.md:1005-1072.
//
//nolint:misspell // GitLab's own field names, as documented
const diffDiscussionExample = `{
  "id": "87805b7c09016a7058e91bdbe7b29d1f284a39e6",
  "individual_note": false,
  "notes": [
    {"id": 1128, "type": "DiffNote", "body": "diff comment", "attachment": null,
     "author": {"id": 1, "name": "root", "username": "root", "state": "active", "avatar_url": "https://www.gravatar.com/avatar/00afb8fb6ab07c3ee3e9c1f38777e2f4?s=80&d=identicon", "web_url": "http://localhost:3000/root"},
     "created_at": "2018-03-04T09:17:22.520Z", "updated_at": "2018-03-04T09:17:22.520Z", "system": false,
     "noteable_id": 3, "noteable_type": "MergeRequest", "project_id": 5, "noteable_iid": null,
     "commit_id": "4803c71e6b1833ca72b8b26ef2ecd5adc8a38031",
     "position": {"base_sha": "b5d6e7b1613fca24d250fa8e5bc7bcc3dd6002ef", "start_sha": "7c9c2ead8a320fb7ba0b4e234bd9529a2614e306",
       "head_sha": "4803c71e6b1833ca72b8b26ef2ecd5adc8a38031", "old_path": "package.json", "new_path": "package.json",
       "position_type": "text", "old_line": 27, "new_line": 27,
       "line_range": {"start": {"line_code": "588440f66559714280628a4f9799f0c4eb880a4a_10_10", "type": "new", "old_line": null, "new_line": 10},
         "end": {"line_code": "588440f66559714280628a4f9799f0c4eb880a4a_11_11", "type": "old", "old_line": 11, "new_line": 11}}},
     "resolved": false, "resolvable": true, "resolved_by": null,
     "suggestions": [{"id": 1, "from_line": 27, "to_line": 27, "appliable": true, "applied": false, "from_content": "x", "to_content": "b"}]}
  ]
}`

// approvalsExample is GET .../merge_requests/:iid/approvals from GitLab
// Community Edition: doc/api/merge_request_approvals.md:1012-1031.
const approvalsExample = `{
  "user_has_approved": false,
  "user_can_approve": true,
  "approved": true,
  "approved_by": [
    {"user": {"id": 1, "username": "root", "public_email": "", "name": "Administrator", "state": "active", "locked": false,
      "avatar_url": "http://www.gravatar.com/avatar/e64c7d89f26bd1972efa854d13d7dd61?s=80&d=identicon", "web_url": "http://localhost:3000/root"},
     "approved_at": "2016-06-09T01:45:21.720Z"}
  ]
}`

// projectExample is GET /projects/:id: doc/api/projects.md:381-559.
const projectExample = `{
  "id": 3,
  "description": "Lorem ipsum dolor sit amet, consectetur adipiscing elit.",
  "default_branch": "main",
  "visibility": "private",
  "web_url": "http://example.com/diaspora/diaspora-project-site",
  "name": "Diaspora Project Site",
  "name_with_namespace": "Diaspora / Diaspora Project Site",
  "path": "diaspora-project-site",
  "path_with_namespace": "diaspora/diaspora-project-site",
  "namespace": {"id": 3, "name": "Diaspora", "path": "diaspora", "kind": "group", "full_path": "diaspora"},
  "archived": false
}`

// commentEventExample is an event of GET /events?action=commented, with
// the note it made: doc/api/events.md:288-326, :61.
//
//nolint:misspell // GitLab's own field names, as documented
const commentEventExample = `{
  "id": 7,
  "title": null,
  "project_id": 15,
  "action_name": "commented on",
  "target_id": 1312,
  "target_iid": 61,
  "target_type": "Note",
  "author_id": 1,
  "target_title": null,
  "created_at": "2015-12-04T10:33:58.089Z",
  "note": {
    "id": 1312, "body": "What an awesome day!", "attachment": null,
    "author": {"name": "Ada Example", "username": "root", "id": 1, "state": "active", "avatar_url": "http://localhost:3000/uploads/user/avatar/1/fox_avatar.png", "web_url": "http://localhost:3000/root"},
    "created_at": "2015-12-04T10:33:56.698Z", "system": false, "noteable_id": 377, "noteable_type": "Issue", "noteable_iid": 377
  },
  "author": {"name": "Ada Example", "username": "root", "id": 1, "state": "active", "avatar_url": "http://localhost:3000/uploads/user/avatar/1/fox_avatar.png", "web_url": "http://localhost:3000/root"},
  "author_username": "root",
  "imported": false,
  "imported_from": "none"
}`

// openedEventExample is an event of GET /events?action=created:
// doc/api/events.md:78-101.
const openedEventExample = `{
  "id": 1,
  "title": null,
  "project_id": 1,
  "action_name": "opened",
  "target_id": 160,
  "target_iid": 53,
  "target_type": "Issue",
  "author_id": 25,
  "target_title": "Qui natus eos odio tempore et quaerat consequuntur ducimus cupiditate quis.",
  "created_at": "2017-02-09T10:43:19.667Z",
  "author": {"name": "User 3", "username": "user3", "id": 25, "state": "active", "avatar_url": "http://www.gravatar.com/avatar/97d6d9441ff85fdc730e02a6068d267b?s=80&d=identicon", "web_url": "https://gitlab.example.com/user3"},
  "author_username": "user3",
  "imported": false,
  "imported_from": "none"
}`

// GraphQL has no example responses for these; they follow the type
// reference: Todo, doc/api/graphql/reference/objects.md:20102-20126, and
// UserCore, :20427-20500 (id, type, bot).

// fakeGitLab serves the REST and GraphQL calls a sync makes and records
// every request, so tests can say what was read.
type fakeGitLab struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	me map[string]any
	// scopes is the token's scopes; noScopes answers 404, as GitLab does
	// for a token it doesn't describe.
	scopes   []string
	noScopes bool
	// todos are GraphQL Todo nodes, newest first; todoPage is how many a
	// page holds.
	todos    []map[string]any
	todoPage int
	events   []fakeEvent
	projects map[int64]map[string]any
	paths    map[int64]string
	threads  []*fakeThread
	// userTypes are GraphQL UserCore nodes, by user ID; a user without one
	// is not returned, as for a user the token may not see.
	userTypes map[int64]map[string]any
	// discussionPage is how many discussions a page holds.
	discussionPage int
	// limitPath answers 429, resetting in an hour, to the requests for it
	// after the first limitAfter.
	limitPath  string
	limitAfter int
	// failPath answers 400 after the first failAfter requests for it.
	failPath  string
	failAfter int

	requests []string
	served   map[string]int
}

type fakeEvent struct {
	// action is the action filter that lists the event.
	action string
	object map[string]any
}

type fakeThread struct {
	resource string
	object   map[string]any
	// scopes are the list scopes that list the thread.
	scopes      []string
	discussions []*discussionDoc
	approvals   []map[string]any
}

// discussionDoc is a discussion as the fake serves it.
type discussionDoc struct {
	ID             string           `json:"id"`
	IndividualNote bool             `json:"individual_note"`
	Notes          []map[string]any `json:"notes"`
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{
		t:              t,
		me:             fixture(t, userExample, nil),
		scopes:         []string{"read_api", "read_user"},
		todoPage:       100,
		projects:       map[int64]map[string]any{},
		paths:          map[int64]string{},
		userTypes:      map[int64]map[string]any{},
		discussionPage: 100,
		served:         map[string]int{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// forge is GitLab at the fake, for one sync.
func (f *fakeGitLab) forge() *Forge {
	g, err := New(f.server.URL, "test-token", "msgvault-test")
	require.NoError(f.t, err)
	return g
}

func (f *fakeGitLab) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeGitLab) clearLog() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

var projectRoute = regexp.MustCompile(`^/api/v4/projects/(\d+)(?:/(issues|merge_requests)(?:/(\d+)(?:/(discussions|approvals))?)?)?$`)

// RoundTrip serves the same API without sockets, so a synctest clock can
// advance between syncs without idle network goroutines keeping it busy.
func (f *fakeGitLab) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	f.serve(w, r)
	return w.Result(), nil
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(f.t, "Bearer test-token", r.Header.Get("Authorization"))
	path := r.URL.Path
	query := r.URL.Query()
	if r.Method == http.MethodPost && path == "/api/graphql" {
		f.serveGraphQL(w, r)
		return
	}
	f.requests = append(f.requests, r.Method+" "+path+"?"+query.Encode())
	f.served[path]++
	w.Header().Set("Ratelimit-Limit", "2000")
	w.Header().Set("Ratelimit-Remaining", "1999")
	w.Header().Set("Ratelimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	if path == f.limitPath && f.served[path] > f.limitAfter {
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("Ratelimit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"429 Too Many Requests"}`)
		return
	}
	if path == f.failPath && f.served[path] > f.failAfter {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"400 Bad request"}`)
		return
	}
	switch path {
	case "/api/v4/user":
		f.writeJSON(w, f.me)
	case "/api/v4/personal_access_tokens/self":
		if f.noScopes {
			f.notFound(w)
			return
		}
		f.writeJSON(w, fixture(f.t, tokenExample, map[string]any{"scopes": f.scopes}))
	case "/api/v4/users/" + strconv.FormatInt(number(f.me["id"]), 10) + "/events":
		f.serveEvents(w, r)
	case "/api/v4/issues", "/api/v4/merge_requests":
		assert.Equal(f.t, "updated_at", query.Get("order_by"))
		assert.Equal(f.t, "asc", query.Get("sort"))
		assert.Equal(f.t, "all", query.Get("state"))
		if path == "/api/v4/issues" {
			assert.Equal(f.t, "false", query.Get("non_archived"), "issues of archived projects are listed")
		}
		resource := strings.TrimPrefix(path, "/api/v4/")
		f.serveList(w, r, func(thread *fakeThread) bool {
			return thread.resource == resource && slices.Contains(thread.scopes, query.Get("scope"))
		})
	default:
		f.serveProject(w, r)
	}
}

// serveProject serves a project and what is under it.
func (f *fakeGitLab) serveProject(w http.ResponseWriter, r *http.Request) {
	match := projectRoute.FindStringSubmatch(r.URL.Path)
	if match == nil {
		assert.Failf(f.t, "unexpected request", "%s %s", r.Method, r.URL)
		f.notFound(w)
		return
	}
	project, _ := strconv.ParseInt(match[1], 10, 64)
	resource, iid, part := match[2], match[3], match[4]
	if resource == "" {
		if p, ok := f.projects[project]; ok {
			f.writeJSON(w, p)
			return
		}
		f.notFound(w)
		return
	}
	if iid == "" {
		iids := r.URL.Query()["iids[]"]
		f.serveList(w, r, func(thread *fakeThread) bool {
			return thread.resource == resource && number(thread.object["project_id"]) == project &&
				slices.Contains(iids, strconv.FormatInt(number(thread.object["iid"]), 10))
		})
		return
	}
	thread := f.thread(project, resource, iid)
	switch {
	case thread == nil:
		f.notFound(w)
	case part == "":
		f.writeJSON(w, thread.object)
	case part == "discussions":
		writePage(f, w, r, thread.discussions, f.discussionPage)
	case part == "approvals":
		f.writeJSON(w, map[string]any{
			"user_has_approved": false, "user_can_approve": true,
			"approved": len(thread.approvals) > 0, "approved_by": thread.approvals,
		})
	}
}

func (f *fakeGitLab) thread(project int64, resource, iid string) *fakeThread {
	for _, thread := range f.threads {
		if thread.resource == resource && number(thread.object["project_id"]) == project &&
			strconv.FormatInt(number(thread.object["iid"]), 10) == iid {
			return thread
		}
	}
	return nil
}

// serveList serves the threads keep selects, oldest update first, from
// updated_after on, a page at a time.
func (f *fakeGitLab) serveList(w http.ResponseWriter, r *http.Request, keep func(*fakeThread) bool) {
	var after time.Time
	if raw := r.URL.Query().Get("updated_after"); raw != "" {
		var err error
		after, err = time.Parse(time.RFC3339Nano, raw)
		assert.NoError(f.t, err)
	}
	createdIn := func(thread *fakeThread) bool {
		created := f.stamp(thread.object["created_at"])
		for param, outside := range map[string]func(time.Time) bool{"created_after": created.Before, "created_before": created.After} {
			if raw := r.URL.Query().Get(param); raw != "" {
				bound, err := time.Parse(time.RFC3339Nano, raw)
				assert.NoError(f.t, err)
				if outside(bound) {
					return false
				}
			}
		}
		return true
	}
	var items []map[string]any
	for _, thread := range f.threads {
		if keep(thread) && !f.stamp(thread.object["updated_at"]).Before(after) && createdIn(thread) {
			items = append(items, thread.object)
		}
	}
	slices.SortStableFunc(items, func(a, b map[string]any) int {
		return cmp.Or(f.stamp(a["updated_at"]).Compare(f.stamp(b["updated_at"])), cmp.Compare(number(a["id"]), number(b["id"])))
	})
	writePage(f, w, r, items, perPage)
}

// serveEvents serves the account's events of one action, oldest first.
// after counts whole days: an event is listed from the day after it.
func (f *fakeGitLab) serveEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	assert.Equal(f.t, "asc", query.Get("sort"))
	var after time.Time
	if raw := query.Get("after"); raw != "" {
		day, err := time.Parse(time.DateOnly, raw)
		assert.NoError(f.t, err)
		after = day.AddDate(0, 0, 1)
	}
	var items []map[string]any
	for _, e := range f.events {
		if e.action == query.Get("action") && !f.stamp(e.object["created_at"]).Before(after) {
			items = append(items, e.object)
		}
	}
	writePage(f, w, r, items, perPage)
}

// writePage writes one page of items, with a Link header to the next.
func writePage[T any](f *fakeGitLab, w http.ResponseWriter, r *http.Request, items []T, most int) {
	query := r.URL.Query()
	size, _ := strconv.Atoi(query.Get("per_page"))
	size = min(cmp.Or(size, 20), most)
	page, _ := strconv.Atoi(query.Get("page"))
	page = max(page, 1)
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	if end < len(items) {
		next := *r.URL
		q := next.Query()
		q.Set("page", strconv.Itoa(page+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, f.server.URL, next.RequestURI()))
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	}
	f.writeJSON(w, items[start:end])
}

func (f *fakeGitLab) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var req graphQLRequest
	if !assert.NoError(f.t, json.UnmarshalRead(r.Body, &req)) {
		return
	}
	switch req.OperationName {
	case "Todos":
		f.requests = append(f.requests, fmt.Sprintf("POST /api/graphql Todos after=%v", req.Variables["after"]))
		states := todoStates.FindStringSubmatch(req.Query)
		if !assert.Len(f.t, states, 2, "the query must name the to-do states") {
			return
		}
		allowed := strings.Split(states[1], ",")
		for i := range allowed {
			allowed[i] = strings.TrimSpace(allowed[i])
		}
		var todos []map[string]any
		for _, node := range f.todos {
			if slices.Contains(allowed, fmt.Sprint(node["state"])) {
				todos = append(todos, node)
			}
		}
		start := 0
		if after, ok := req.Variables["after"].(string); ok {
			start, _ = strconv.Atoi(after)
		}
		end := min(start+f.todoPage, len(todos))
		f.writeJSON(w, map[string]any{"data": map[string]any{"currentUser": map[string]any{"todos": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end < len(todos), "endCursor": strconv.Itoa(end)},
			"nodes":    todos[start:end],
		}}}})
	case "Users":
		ids, _ := req.Variables["ids"].([]any)
		f.requests = append(f.requests, fmt.Sprintf("POST /api/graphql Users %v", ids))
		nodes := []map[string]any{}
		for _, id := range ids {
			n, ok := gidNumber(fmt.Sprint(id))
			assert.True(f.t, ok, id)
			if node, ok := f.userTypes[n]; ok {
				nodes = append(nodes, node)
			}
		}
		f.writeJSON(w, map[string]any{"data": map[string]any{"users": map[string]any{"nodes": nodes}}})
	default:
		assert.Failf(f.t, "unexpected GraphQL operation", "%s", req.OperationName)
	}
}

var todoStates = regexp.MustCompile(`todos\(state:\s*\[([^]]*)\]`)

func (f *fakeGitLab) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(f.t, json.MarshalWrite(w, value))
}

func (f *fakeGitLab) notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"404 Not Found"}`)
}

func (f *fakeGitLab) stamp(value any) time.Time {
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(value))
	assert.NoError(f.t, err)
	return at
}

// fixture decodes a documented example and sets overrides on it.
func fixture(t *testing.T, example string, overrides map[string]any) map[string]any {
	t.Helper()
	var object map[string]any
	require.NoError(t, json.Unmarshal([]byte(example), &object))
	maps.Copy(object, overrides)
	return object
}

func number(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	default:
		return 0
	}
}

// Builders for a sync's canned GitLab objects.

func (f *fakeGitLab) project(id int64, path string) {
	name := path[strings.LastIndexByte(path, '/')+1:]
	f.paths[id] = path
	f.projects[id] = fixture(f.t, projectExample, map[string]any{
		"id": id, "path": name, "path_with_namespace": path, "web_url": "https://gitlab.example.com/" + path,
	})
}

// person is a user as responses embed one, built from the documented
// author.
func person(t *testing.T, id int64, username, name string) map[string]any {
	t.Helper()
	return fixture(t, authorExample, map[string]any{
		"id": id, "username": username, "name": name, "web_url": "https://gitlab.example.com/" + username,
	})
}

// issue adds an issue to project, built from the documented issue.
func (f *fakeGitLab) issue(project int64, id int64, iid int, title string, author map[string]any, updated string, scopes ...string) *fakeThread {
	path := f.paths[project]
	thread := &fakeThread{resource: resourceIssues, scopes: scopes, object: fixture(f.t, issueExample, map[string]any{
		"id": id, "iid": iid, "project_id": project, "title": title, "author": author,
		"created_at": "2016-01-04T15:31:46.176Z", "updated_at": updated,
		"web_url":    fmt.Sprintf("https://gitlab.example.com/%s/-/issues/%d", path, iid),
		"references": map[string]any{"short": fmt.Sprintf("#%d", iid), "relative": fmt.Sprintf("#%d", iid), "full": fmt.Sprintf("%s#%d", path, iid)},
	})}
	f.threads = append(f.threads, thread)
	return thread
}

// mergeRequest adds a merge request to project, built from the documented
// merge request.
func (f *fakeGitLab) mergeRequest(project int64, id int64, iid int, title string, author map[string]any, updated string, scopes ...string) *fakeThread {
	path := f.paths[project]
	thread := &fakeThread{resource: resourceMergeRequests, scopes: scopes, object: fixture(f.t, mergeRequestExample, map[string]any{
		"id": id, "iid": iid, "project_id": project, "title": title, "author": author,
		"source_project_id": project, "target_project_id": project, "updated_at": updated,
		"web_url":    fmt.Sprintf("https://gitlab.example.com/%s/-/merge_requests/%d", path, iid),
		"references": map[string]any{"short": fmt.Sprintf("!%d", iid), "relative": fmt.Sprintf("!%d", iid), "full": fmt.Sprintf("%s!%d", path, iid)},
	})}
	f.threads = append(f.threads, thread)
	return thread
}

// fakeNote is a note built from the documented discussion note.
func fakeNote(t *testing.T, id int64, noteType any, body string, author map[string]any, created string) map[string]any {
	t.Helper()
	var docs []discussionDoc
	require.NoError(t, json.Unmarshal([]byte(discussionsExample), &docs))
	out := docs[0].Notes[0]
	maps.Copy(out, map[string]any{"id": id, "type": noteType, "body": body, "author": author, "created_at": created, "updated_at": created})
	return out
}

// fakeDiffNote is a diff note built from the documented diff discussion.
func fakeDiffNote(t *testing.T, id int64, body string, author map[string]any) map[string]any {
	t.Helper()
	var doc discussionDoc
	require.NoError(t, json.Unmarshal([]byte(diffDiscussionExample), &doc))
	out := doc.Notes[0]
	maps.Copy(out, map[string]any{"id": id, "body": body, "author": author})
	return out
}

func fakeDiscussion(id string, individual bool, notes ...map[string]any) *discussionDoc {
	return &discussionDoc{ID: id, IndividualNote: individual, Notes: notes}
}

// fakeTodo is a GraphQL to-do about a thread.
func fakeTodo(id int64, action, created string, project int64, path string, thread *fakeThread) map[string]any {
	class := "Issue"
	if thread.resource == resourceMergeRequests {
		class = "MergeRequest"
	}
	return map[string]any{
		"id": fmt.Sprintf("gid://gitlab/Todo/%d", id), "action": action, "createdAt": created,
		"state":   "pending",
		"project": map[string]any{"id": fmt.Sprintf("gid://gitlab/Project/%d", project), "fullPath": path},
		"targetEntity": map[string]any{
			"__typename": class, "id": fmt.Sprintf("gid://gitlab/%s/%d", class, number(thread.object["id"])),
			"iid": strconv.FormatInt(number(thread.object["iid"]), 10), "updatedAt": thread.object["updated_at"],
		},
	}
}

// commented is the account's comment event on a thread.
//
//nolint:misspell // GitLab's own field names, as documented
func (f *fakeGitLab) commented(id int64, created string, thread *fakeThread) {
	class := "Issue"
	if thread.resource == resourceMergeRequests {
		class = "MergeRequest"
	}
	var example struct {
		Note map[string]any `json:"note"`
	}
	require.NoError(f.t, json.Unmarshal([]byte(commentEventExample), &example))
	maps.Copy(example.Note, map[string]any{"noteable_id": thread.object["id"], "noteable_iid": thread.object["iid"], "noteable_type": class})
	f.events = append(f.events, fakeEvent{action: "commented", object: fixture(f.t, commentEventExample, map[string]any{
		"id": id, "project_id": thread.object["project_id"], "created_at": created, "note": example.Note,
	})})
}

// event is the account's event of action on a thread itself, such as
// opening or approving it.
func (f *fakeGitLab) event(action string, id int64, created string, thread *fakeThread) {
	target := "Issue"
	if thread.resource == resourceMergeRequests {
		target = "MergeRequest"
	}
	f.events = append(f.events, fakeEvent{action: action, object: fixture(f.t, openedEventExample, map[string]any{
		"id": id, "project_id": thread.object["project_id"], "action_name": action, "target_type": target,
		"target_id": thread.object["id"], "target_iid": thread.object["iid"], "created_at": created,
	})})
}

func approval(t *testing.T, approver map[string]any, at string) map[string]any {
	t.Helper()
	var state struct {
		ApprovedBy []map[string]any `json:"approved_by"`
	}
	require.NoError(t, json.Unmarshal([]byte(approvalsExample), &state))
	out := state.ApprovedBy[0]
	out["user"], out["approved_at"] = approver, at
	return out
}

func userType(id int64, kind string, bot bool) map[string]any {
	return map[string]any{"id": fmt.Sprintf("gid://gitlab/User/%d", id), "type": kind, "bot": bot}
}

// requestsFor are the logged requests whose path starts with prefix.
func requestsFor(log []string, prefix string) []string {
	var out []string
	for _, request := range log {
		if _, rest, ok := strings.Cut(request, " "); ok && strings.HasPrefix(rest, prefix) {
			out = append(out, request)
		}
	}
	return out
}

// queryOf is a logged request's query.
func queryOf(t *testing.T, request string) url.Values {
	t.Helper()
	_, raw, _ := strings.Cut(request, "?")
	query, err := url.ParseQuery(raw)
	require.NoError(t, err)
	return query
}
