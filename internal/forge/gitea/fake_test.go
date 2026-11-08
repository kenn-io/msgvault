package gitea

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// webBase is where the fixtures' web links point, as a server's ROOT_URL.
const webBase = "https://git.example.com/"

// fakeGitea serves the API calls a sync makes from canned objects shaped as
// Gitea's API definitions lay them out, under a sub-path as a server behind
// a reverse proxy is, and records every request so tests can say what was
// read. Gitea's API documentation has no example responses for these
// objects, so each builder below follows the definition it cites.
type fakeGitea struct {
	t      *testing.T
	server *httptest.Server
	// prefix is the path the server is mounted at.
	prefix string

	mu sync.Mutex
	// pageSize is the server's [api] MAX_RESPONSE_ITEMS.
	pageSize int
	token    string
	me       map[string]any
	// refuse answers a path with 403 and this message, as Gitea does for a
	// token without the scope the path needs.
	refuse map[string]string
	// limited answers a request whose path and query contain the key with
	// 429 and this Retry-After, once.
	limited map[string]int
	// fail answers a path with this status.
	fail map[string]int

	notifications []map[string]any
	// hits are the threads each search flag finds, by "owner/repo#n".
	hits map[string][]string
	// issues, comments and reviews are by "owner/repo#n".
	issues         map[string]map[string]any
	comments       map[string][]map[string]any
	reviews        map[string][]map[string]any
	reviewComments map[int64][]map[string]any
	activities     []map[string]any
	// repoOwners is a repository's current owner, by "owner/repo", when it
	// moved.
	repoOwners map[string]string

	requests []string
}

func newFakeGitea(t *testing.T) *fakeGitea {
	t.Helper()
	f := &fakeGitea{
		t:              t,
		prefix:         "/git",
		pageSize:       50,
		token:          "test-token",
		me:             giteaUser(1001, "octo"),
		refuse:         map[string]string{},
		limited:        map[string]int{},
		fail:           map[string]int{},
		hits:           map[string][]string{},
		issues:         map[string]map[string]any{},
		comments:       map[string][]map[string]any{},
		reviews:        map[string][]map[string]any{},
		reviewComments: map[int64][]map[string]any{},
		repoOwners:     map[string]string{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// forge is the adapter for the fake's server, opened as add-gitea and
// sync-gitea open it: from the web address the user gave.
func (f *fakeGitea) forge() *Forge {
	return f.forgeWith(f.token)
}

func (f *fakeGitea) forgeWith(token string) *Forge {
	base, err := url.Parse(f.server.URL + f.prefix + "/")
	if err != nil {
		panic(err)
	}
	gitea, err := New(base, token, "msgvault-test")
	if err != nil {
		panic(err)
	}
	return gitea
}

func (f *fakeGitea) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeGitea) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	route, ok := strings.CutPrefix(r.URL.Path, f.prefix+"/api/v1")
	if !ok || r.Method != http.MethodGet {
		assert.Fail(f.t, "unexpected request", "%s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	entry := "GET " + route
	if r.URL.RawQuery != "" {
		entry += "?" + r.URL.Query().Encode()
	}
	f.requests = append(f.requests, entry)
	if route != "/settings/api" && r.Header.Get("Authorization") != "token "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"token is required"}`)
		return
	}
	for key, retryAfter := range f.limited {
		if strings.Contains(entry, key) {
			delete(f.limited, key)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}
	if message, refused := f.refuse[route]; refused {
		f.writeError(w, http.StatusForbidden, message)
		return
	}
	if status, failing := f.fail[route]; failing {
		w.Header().Set("Retry-After", "0")
		f.writeError(w, status, http.StatusText(status))
		return
	}
	query := r.URL.Query()
	parts := strings.Split(strings.Trim(route, "/"), "/")
	scopeCheck := len(query) == 1 && query.Get("limit") == "1"
	switch {
	case route == "/settings/api":
		f.writeJSON(w, map[string]any{"max_response_items": f.pageSize})
	case scopeCheck && (route == "/notifications" || route == "/repos/issues/search"):
		f.writeJSON(w, []any{})
	case route == "/user":
		f.writeJSON(w, f.me)
	case route == "/notifications":
		f.serveNotifications(w, query)
	case route == "/repos/issues/search":
		f.serveSearch(w, query)
	case route == "/repos/search":
		f.writeJSON(w, map[string]any{"ok": true, "data": []any{}})
	case len(parts) == 4 && parts[0] == "users" && parts[2] == "activities" && parts[3] == "feeds":
		f.serveActivity(w, parts[1], query)
	case len(parts) == 3 && parts[0] == "repos":
		f.serveRepo(w, parts[1]+"/"+parts[2])
	case len(parts) >= 5 && parts[0] == "repos":
		f.serveThread(w, parts[1]+"/"+parts[2], parts[3:], query)
	default:
		f.writeError(w, http.StatusNotFound, "Not Found")
	}
}

func (f *fakeGitea) serveNotifications(w http.ResponseWriter, query url.Values) {
	if query.Get("all") != "true" {
		assert.Fail(f.t, "notifications listed without all=true", "%v", query)
	}
	since := f.since(query)
	var matching []map[string]any
	for _, n := range f.notifications {
		if !ts(n["updated_at"]).Before(since) {
			matching = append(matching, n)
		}
	}
	// Newest update first (models/activities/notification_list.go).
	slices.SortStableFunc(matching, func(a, b map[string]any) int { return ts(b["updated_at"]).Compare(ts(a["updated_at"])) })
	f.writePage(w, query, matching, true)
}

func (f *fakeGitea) serveSearch(w http.ResponseWriter, query url.Values) {
	if query.Get("state") != "all" {
		assert.Fail(f.t, "search without state=all", "%v", query)
	}
	var flags []string
	for _, flag := range searchFlags {
		if query.Get(flag) == "true" {
			flags = append(flags, flag)
		}
	}
	if len(flags) != 1 {
		assert.Fail(f.t, "search requires exactly one flag", "%v", flags)
		return
	}
	since := f.since(query)
	owner := strings.ToLower(query.Get("owner"))
	var matching []map[string]any
	for _, key := range f.hits[flags[0]] {
		issue, ok := f.issues[key]
		if !ok {
			assert.Fail(f.t, "search hit has no issue", "%s", key)
			continue
		}
		repoOwner, _ := issue["repository"].(map[string]any)["owner"].(string)
		if owner != "" && strings.ToLower(repoOwner) != owner {
			continue
		}
		if ts(issue["updated_at"]).Before(since) {
			continue
		}
		matching = append(matching, issue)
	}
	// Newest creation first (routers/api/v1/repo/issue.go:195).
	slices.SortStableFunc(matching, func(a, b map[string]any) int { return ts(b["created_at"]).Compare(ts(a["created_at"])) })
	f.writePage(w, query, matching, true)
}

func (f *fakeGitea) serveActivity(w http.ResponseWriter, login string, query url.Values) {
	if query.Get("only-performed-by") != "true" {
		assert.Fail(f.t, "activity listed without only-performed-by=true", "%v", query)
	}
	if me, _ := f.me["login"].(string); !strings.EqualFold(login, me) {
		assert.Fail(f.t, "activity is not the account's", "%s", login)
	}
	matching := slices.Clone(f.activities)
	// Newest first (models/activities/action_list.go:243).
	slices.SortStableFunc(matching, func(a, b map[string]any) int { return ts(b["created"]).Compare(ts(a["created"])) })
	f.writePage(w, query, matching, false)
}

func (f *fakeGitea) serveRepo(w http.ResponseWriter, repo string) {
	owner, name, _ := strings.Cut(repo, "/")
	if moved, ok := f.repoOwners[repo]; ok {
		owner = moved
	}
	f.writeJSON(w, map[string]any{"id": repositoryID(repo), "name": name, "full_name": owner + "/" + name, "owner": giteaUser(9000, owner)})
}

func (f *fakeGitea) serveThread(w http.ResponseWriter, repo string, rest []string, query url.Values) {
	key := repo + "#" + rest[1]
	issue, ok := f.issues[key]
	if !ok {
		f.writeError(w, http.StatusNotFound, "issue does not exist")
		return
	}
	switch {
	case len(rest) == 2 && rest[0] == "issues":
		f.writeJSON(w, issue)
	case len(rest) == 3 && rest[0] == "issues" && rest[2] == "comments":
		// Every plain comment, unpaged, oldest first
		// (routers/api/v1/repo/issue_comment.go:64-111).
		since := f.since(query)
		var matching []map[string]any
		for _, c := range f.comments[key] {
			if !ts(c["updated_at"]).Before(since) {
				matching = append(matching, c)
			}
		}
		w.Header().Set("X-Total-Count", strconv.Itoa(len(matching)))
		f.writeJSON(w, nonNil(matching))
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "reviews":
		// Gitea counts and pages before hiding other users' pending reviews
		// (routers/api/v1/repo/pull_review.go:80-103,
		// services/convert/pull_review.go:68-72).
		reviews := f.page(w, query, f.reviews[key], false)
		visible := make([]map[string]any, 0, len(reviews))
		for _, review := range reviews {
			if review["state"] == "PENDING" && f.me["is_admin"] != true {
				reviewer, ok := review["user"].(map[string]any)
				if !assert.True(f.t, ok, "pending review has no user") || reviewer["id"] != f.me["id"] {
					continue
				}
			}
			visible = append(visible, review)
		}
		f.writeJSON(w, visible)
	case len(rest) == 5 && rest[0] == "pulls" && rest[2] == "reviews" && rest[4] == "comments":
		id, _ := strconv.ParseInt(rest[3], 10, 64)
		comments, ok := f.reviewComments[id]
		if !ok {
			f.writeError(w, http.StatusNotFound, "review does not exist")
			return
		}
		f.writeJSON(w, comments)
	default:
		f.writeError(w, http.StatusNotFound, "Not Found")
	}
}

func (f *fakeGitea) since(query url.Values) time.Time {
	raw := query.Get("since")
	if raw == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		assert.Fail(f.t, "invalid since timestamp", "%q: %v", raw, err)
	}
	return at
}

// writePage pages a list as Gitea does: page and limit, the limit clamped
// to the server's MAX_RESPONSE_ITEMS, the total in X-Total-Count, and, for
// the lists that set them, Link headers to the next, last, first and
// previous pages under the server's ROOT_URL (routers/api/v1/utils/page.go,
// services/convert/utils.go, services/context/api.go:140-188).
func (f *fakeGitea) writePage(w http.ResponseWriter, query url.Values, items []map[string]any, links bool) {
	f.writeJSON(w, f.page(w, query, items, links))
}

func (f *fakeGitea) page(w http.ResponseWriter, query url.Values, items []map[string]any, links bool) []map[string]any {
	page := max(1, atoi(query.Get("page")))
	limit := atoi(query.Get("limit"))
	if limit <= 0 || limit > f.pageSize {
		limit = f.pageSize
	}
	start := min((page-1)*limit, len(items))
	end := min(start+limit, len(items))
	w.Header().Set("X-Total-Count", strconv.Itoa(len(items)))
	if pages := (len(items) + limit - 1) / limit; links && pages > 1 {
		link := func(rel string, to int) string {
			return fmt.Sprintf(`<%sapi/v1/notifications?page=%d>; rel="%s"`, webBase, to, rel)
		}
		var set []string
		if page < pages {
			set = append(set, link("next", page+1), link("last", pages))
		}
		if page > 1 {
			set = append(set, link("first", 1), link("prev", page-1))
		}
		w.Header().Set("Link", strings.Join(set, ","))
	}
	return nonNil(items[start:end])
}

func (f *fakeGitea) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, value); err != nil {
		assert.Fail(f.t, "encode response", "%v", err)
	}
}

// writeError answers as Gitea's APIError does, with a message and a URL.
func (f *fakeGitea) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	f.writeJSON(w, map[string]any{"message": message, "url": webBase + "api/swagger"})
}

func atoi(raw string) int {
	n, _ := strconv.Atoi(raw)
	return n
}

func nonNil(items []map[string]any) []map[string]any {
	if items == nil {
		return []map[string]any{}
	}
	return items
}

func ts(value any) time.Time {
	raw, _ := value.(string)
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		panic(fmt.Sprintf("time %q: %v", raw, err))
	}
	return at
}

// Builders for canned objects. Each follows the definition in Gitea's
// templates/swagger/v1-swagger.generated.json (GT) or Forgejo's
// templates/swagger/v1_json.tmpl (FJ).

// giteaUser is a User (GT :33347; modules/structs/user.go:15-60). Its email
// is the placeholder Gitea shows other accounts.
func giteaUser(id int64, login string) map[string]any {
	return map[string]any{
		"id": id, "login": login, "username": login, "login_name": "", "source_id": 0,
		"full_name": "", "email": strings.ToLower(login) + "@noreply.git.example.com",
		"avatar_url": webBase + "avatars/" + login, "html_url": webBase + login, "language": "",
		"is_admin": false, "last_login": "0001-01-01T00:00:00Z", "created": "2024-01-01T00:00:00Z",
		"restricted": false, "active": false, "prohibit_login": false, "location": "", "website": "",
		"description": "", "visibility": "public", "followers_count": 0, "following_count": 0,
		"starred_repos_count": 0, "type": "User",
	}
}

// giteaBot is a bot account: a User of type Bot (modules/structs/user_type.go).
func giteaBot(id int64, login string) map[string]any {
	u := giteaUser(id, login)
	u["type"] = "Bot"
	return u
}

// ghost is Gitea's stand-in for a deleted account
// (models/user/user_system.go:14-26).
func ghost() map[string]any {
	return giteaUser(-1, "Ghost")
}

// migratedAuthor is how Gitea shows a post a migration carried over: the
// ghost's ID with the original author's name (services/convert/user.go:53-58).
func migratedAuthor(name string) map[string]any {
	return map[string]any{"id": -1, "login": name, "username": name, "type": "User", "visibility": "public", "avatar_url": webBase + "assets/img/avatar_default.png"}
}

// forgejoUser is a Forgejo User (FJ :32370), which has pronouns and no type.
func forgejoUser(id int64, login string) map[string]any {
	u := giteaUser(id, login)
	delete(u, "type")
	u["pronouns"] = ""
	return u
}

// repo is a Repository (GT :32283), with the fields a notification or
// activity carries that the sync reads, and enough beside them to be one.
func repo(owner, name string) map[string]any {
	return map[string]any{
		"id": repositoryID(owner + "/" + name), "owner": giteaUser(9000, owner), "name": name, "full_name": owner + "/" + name,
		"description": "", "empty": false, "private": false, "fork": false,
		"html_url": webBase + owner + "/" + name, "default_branch": "main",
	}
}

var fixtureRepositories = struct {
	sync.Mutex

	ids map[string]int64
}{ids: make(map[string]int64)}

// repositoryID makes every builder agree on a repository's numeric ID.
func repositoryID(name string) int64 {
	fixtureRepositories.Lock()
	defer fixtureRepositories.Unlock()
	if id, ok := fixtureRepositories.ids[name]; ok {
		return id
	}
	id := int64(len(fixtureRepositories.ids) + 1)
	fixtureRepositories.ids[name] = id
	return id
}

// notified is a NotificationThread (GT :30728; modules/structs/notifications.go:11-44)
// for an issue or pull request, whose subject URL is the issue's API URL for
// both (models/issues/issue.go:403-411).
func notified(id int64, kind, owner, name string, number int, title, updated string) map[string]any {
	apiRepo := webBase + "api/v1/repos/" + owner + "/" + name
	path := "issues"
	if kind == "Pull" {
		path = "pulls"
	}
	return map[string]any{
		"id": id, "repository": repo(owner, name), "unread": true, "pinned": false, "updated_at": updated,
		"url": webBase + "api/v1/notifications/threads/" + strconv.FormatInt(id, 10),
		"subject": map[string]any{
			"title": title, "url": apiRepo + "/issues/" + strconv.Itoa(number),
			"html_url":           webBase + owner + "/" + name + "/" + path + "/" + strconv.Itoa(number),
			"latest_comment_url": "", "latest_comment_html_url": "", "type": kind, "state": "open",
		},
	}
}

// issue is an Issue (GT :29807; modules/structs/issue.go:42-82); a pull
// request is an issue with pull_request set.
func issue(id int64, owner, name string, number int, title, body string, author map[string]any, created, updated string) map[string]any {
	return map[string]any{
		"id": id, "url": webBase + "api/v1/repos/" + owner + "/" + name + "/issues/" + strconv.Itoa(number),
		"html_url": webBase + owner + "/" + name + "/issues/" + strconv.Itoa(number),
		"number":   number, "user": author, "title": title, "body": body, "ref": "",
		"assets": []any{}, "labels": []any{map[string]any{"id": 5, "name": "bug", "color": "ee0701"}},
		"milestone": nil, "assignee": nil, "assignees": nil, "state": "open", "is_locked": false,
		"comments": 0, "created_at": created, "updated_at": updated, "closed_at": nil, "due_date": nil,
		"pull_request": nil, "pin_order": 0,
		"repository": map[string]any{"id": repositoryID(owner + "/" + name), "name": name, "owner": owner, "full_name": owner + "/" + name},
	}
}

// pull is a pull request in the fixtures' acme organization.
func pull(id int64, name string, number int, title, body string, author map[string]any, created, updated string) map[string]any {
	const owner = "acme"
	p := issue(id, owner, name, number, title, body, author, created, updated)
	p["html_url"] = webBase + owner + "/" + name + "/pulls/" + strconv.Itoa(number)
	p["pull_request"] = map[string]any{"merged": false, "merged_at": nil, "draft": false, "html_url": p["html_url"]}
	return p
}

// comment is a Comment (GT :25686; modules/structs/issue_comment.go:11-29).
func comment(id int64, threadURL, body string, author map[string]any, created string) map[string]any {
	return map[string]any{
		"id": id, "html_url": threadURL + "#issuecomment-" + strconv.FormatInt(id, 10),
		"pull_request_url": "", "issue_url": threadURL, "user": author, "body": body,
		"assets": []any{}, "created_at": created, "updated_at": created,
	}
}

// review is a PullReview (GT :31799; modules/structs/pull_review.go:30-51).
func review(id int64, state, body string, author map[string]any, commentsCount int, submitted string) map[string]any {
	return map[string]any{
		"id": id, "user": author, "team": nil, "state": state, "body": body, "commit_id": "0f1e2d",
		"stale": false, "official": state == "APPROVED", "dismissed": false, "comments_count": commentsCount,
		"submitted_at": submitted, "updated_at": submitted,
		"html_url":         webBase + "acme/widgets/pulls/7#issuecomment-" + strconv.FormatInt(id, 10),
		"pull_request_url": webBase + "acme/widgets/pulls/7",
	}
}

// reviewComment is a PullReviewComment (GT :31874; modules/structs/pull_review.go:55-76).
func reviewComment(id, reviewID int64, path string, position, originalPosition int, body string, author, resolver map[string]any, created string) map[string]any {
	var resolvedBy any // null until someone resolves the conversation
	if resolver != nil {
		resolvedBy = resolver
	}
	return map[string]any{
		"id": id, "body": body, "user": author, "resolver": resolvedBy, "pull_request_review_id": reviewID,
		"created_at": created, "updated_at": created, "path": path, "commit_id": "0f1e2d",
		"original_commit_id": "0f1e2d", "diff_hunk": "@@ -1,3 +1,4 @@", "position": position,
		"original_position": originalPosition,
		"html_url":          webBase + "acme/widgets/pulls/7/files#issuecomment-" + strconv.FormatInt(id, 10),
		"pull_request_url":  webBase + "acme/widgets/pulls/7",
	}
}

// commented is an Activity of a comment (GT :24892; modules/structs/activity.go:8-36),
// whose content Gitea writes as "<index>|<excerpt>" (services/feed/notifier.go:111).
func commented(id int64, opType, owner, name string, number int, excerpt string, actor map[string]any, created string) map[string]any {
	return map[string]any{
		"id": id, "user_id": actor["id"], "op_type": opType, "act_user_id": actor["id"], "act_user": actor,
		"repo_id": repositoryID(owner + "/" + name), "repo": repo(owner, name), "comment_id": id * 10, "comment": nil, "ref_name": "",
		"is_private": false, "content": strconv.Itoa(number) + "|" + excerpt, "created": created,
	}
}
