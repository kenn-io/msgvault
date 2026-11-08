package github

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

// Post kinds, as metadata.kind names them.
const (
	postIssue             = "issue"
	postPullRequest       = "pull_request"
	postIssueComment      = "issue_comment"
	postReview            = "review"
	postReviewComment     = "review_comment"
	postDiscussion        = "discussion"
	postDiscussionComment = "discussion_comment"
	postDiscussionReply   = "discussion_reply"
	postRelease           = "release"

	ghostLogin = "ghost"
	ghostURL   = "https://github.com/ghost"
)

// GraphQL fragments. A query document may only define the fragments it
// uses, so each query lists its own. {{email}} becomes the email field when
// the token may read it.
const (
	actorFragment = `fragment Actor on Actor { __typename login url ... on User { databaseId{{email}} } ... on Bot { databaseId } }`

	issueCommentFragment = `fragment IssueComment on IssueComment { id url body createdAt author { ...Actor } }`

	reviewCommentFragment = `fragment ReviewComment on PullRequestReviewComment {
  id url body path line originalLine createdAt replyTo { id } author { ...Actor }
}`

	reviewFragment = `fragment Review on PullRequestReview {
  id url body state submittedAt createdAt author { ...Actor }
  comments(first: 100) { pageInfo { hasNextPage endCursor } nodes { ...ReviewComment } }
}`

	discussionReplyFragment = `fragment DiscussionReply on DiscussionComment {
  id url body createdAt replyTo { id } author { ...Actor }
}`

	discussionCommentFragment = `fragment DiscussionComment on DiscussionComment {
  id url body createdAt isAnswer author { ...Actor }
  replies(first: 100) { pageInfo { hasNextPage endCursor } nodes { ...DiscussionReply } }
}`
)

type queryDef struct {
	name      string
	body      string
	fragments []string
}

func (q queryDef) text(emails bool) string {
	text := q.body + "\n" + strings.Join(q.fragments, "\n")
	email := ""
	if emails {
		email = " email"
	}
	return strings.ReplaceAll(text, "{{email}}", email)
}

var (
	issueThreadQuery = queryDef{"IssueThread", `query IssueThread($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issueOrPullRequest(number: $number) {
      __typename
      ... on Issue {
        id number title url body createdAt author { ...Actor }
        comments(first: 100) { pageInfo { hasNextPage endCursor } nodes { ...IssueComment } }
      }
      ... on PullRequest {
        id number title url body createdAt author { ...Actor }
        comments(first: 100) { pageInfo { hasNextPage endCursor } nodes { ...IssueComment } }
        reviews(first: 100) { pageInfo { hasNextPage endCursor } nodes { ...Review } }
      }
    }
  }
}`, []string{actorFragment, issueCommentFragment, reviewCommentFragment, reviewFragment}}

	discussionThreadQuery = queryDef{"DiscussionThread", `query DiscussionThread($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    discussion(number: $number) {
      id number title url body createdAt category { name } author { ...Actor }
      comments(first: 100) { pageInfo { hasNextPage endCursor } nodes { ...DiscussionComment } }
    }
  }
}`, []string{actorFragment, discussionReplyFragment, discussionCommentFragment}}

	// Each further page aliases its connection as "page" under node(id:).
	pageQueries = map[string]queryDef{
		connComments: {"MoreIssueComments", `query MoreIssueComments($id: ID!, $after: String) {
  node(id: $id) {
    ... on Issue { page: comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...IssueComment } } }
    ... on PullRequest { page: comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...IssueComment } } }
  }
}`, []string{actorFragment, issueCommentFragment}},
		connReviews: {"MoreReviews", `query MoreReviews($id: ID!, $after: String) {
  node(id: $id) {
    ... on PullRequest { page: reviews(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...Review } } }
  }
}`, []string{actorFragment, reviewCommentFragment, reviewFragment}},
		connReviewComments: {"MoreReviewComments", `query MoreReviewComments($id: ID!, $after: String) {
  node(id: $id) {
    ... on PullRequestReview { page: comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...ReviewComment } } }
  }
}`, []string{actorFragment, reviewCommentFragment}},
		connDiscussionComments: {"MoreDiscussionComments", `query MoreDiscussionComments($id: ID!, $after: String) {
  node(id: $id) {
    ... on Discussion { page: comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...DiscussionComment } } }
  }
}`, []string{actorFragment, discussionReplyFragment, discussionCommentFragment}},
		connReplies: {"MoreReplies", `query MoreReplies($id: ID!, $after: String) {
  node(id: $id) {
    ... on DiscussionComment { page: replies(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...DiscussionReply } } }
  }
}`, []string{actorFragment, discussionReplyFragment}},
	}
)

// Connections a thread's later pages read.
const (
	connComments           = "comments"
	connReviews            = "reviews"
	connReviewComments     = "review_comments"
	connDiscussionComments = "discussion_comments"
	connReplies            = "replies"
)

// continuation is the rest of a thread still to fetch: the connection pages
// its earlier pages said remain, in order.
type continuation struct {
	Head    forge.ThreadHead `json:"head"`
	Repo    string           `json:"repo"`
	Number  int              `json:"number"`
	Pending []pendingPage    `json:"pending"`
}

type pendingPage struct {
	Connection string `json:"connection"`
	// Node is the ID of the node whose connection this is.
	Node  string `json:"node"`
	After string `json:"after"`
}

type actor struct {
	Typename   string `json:"__typename"`
	Login      string `json:"login"`
	URL        string `json:"url"`
	DatabaseID int64  `json:"databaseId"`
	Email      string `json:"email"`
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// connection keeps each node's JSON: a post's raw evidence.
type connection struct {
	PageInfo pageInfo         `json:"pageInfo"`
	Nodes    []jsontext.Value `json:"nodes"`
}

type node struct {
	Typename     string     `json:"__typename"`
	ID           string     `json:"id"`
	Number       int        `json:"number"`
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	Body         string     `json:"body"`
	State        string     `json:"state"`
	Path         string     `json:"path"`
	Line         *int       `json:"line"`
	OriginalLine *int       `json:"originalLine"`
	IsAnswer     bool       `json:"isAnswer"`
	CreatedAt    time.Time  `json:"createdAt"`
	SubmittedAt  *time.Time `json:"submittedAt"`
	Author       *actor     `json:"author"`
	ReplyTo      *struct {
		ID string `json:"id"`
	} `json:"replyTo"`
	Category *struct {
		Name string `json:"name"`
	} `json:"category"`
	Comments connection `json:"comments"`
	Reviews  connection `json:"reviews"`
	Replies  connection `json:"replies"`
}

// optionalPaths are fields GitHub may withhold from a node without the
// thread being incomplete: who wrote a post, and what it replies to.
var optionalPaths = []string{"author", "replyTo", "email"}

// Fetch streams a thread's posts, one page at a time.
func (f *Forge) Fetch(ctx context.Context, ref forge.ThreadRef, raw []byte,
	page func(forge.ThreadHead, []forge.Post, []byte) error,
) error {
	var cont continuation
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cont); err != nil {
			return fmt.Errorf("decode GitHub continuation of %s: %w", ref.Name(), err)
		}
	} else {
		first, posts, err := f.firstPage(ctx, ref)
		if err != nil {
			return err
		}
		cont = first
		if err := page(cont.Head, posts, cont.encode()); err != nil {
			return err
		}
	}
	for len(cont.Pending) > 0 {
		posts, err := f.nextPage(ctx, &cont)
		if err != nil {
			return err
		}
		if err := page(cont.Head, posts, cont.encode()); err != nil {
			return err
		}
	}
	return nil
}

// encode is the continuation as Fetch resumes from it; nil once nothing
// remains.
func (c continuation) encode() []byte {
	if len(c.Pending) == 0 {
		return nil
	}
	data, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		panic(fmt.Sprintf("encode GitHub continuation: %v", err)) // strings and ints always encode
	}
	return data
}

// firstPage fetches a thread's opening post and the first page of each of
// its connections.
func (f *Forge) firstPage(ctx context.Context, ref forge.ThreadRef) (continuation, []forge.Post, error) {
	switch ref.Kind {
	case forge.KindRelease:
		return f.release(ctx, ref)
	case forge.KindDiscussion:
		if ref.Number == 0 {
			number, err := f.findDiscussion(ctx, ref)
			switch {
			case errors.Is(err, errDiscussionNotFound) && f.now().Sub(ref.UpdatedAt) > discussionIndexLag:
				return continuation{}, nil, fmt.Errorf("%w: no discussion titled %q in %s", forge.ErrUnavailable,
					strings.TrimPrefix(ref.Locator, "title:"), ref.Repo)
			case err != nil:
				// Search may not have indexed it yet; the thread stays queued.
				return continuation{}, nil, fmt.Errorf("find the discussion of %s: %w", ref.Name(), err)
			}
			ref.Number = number
		}
		return f.discussion(ctx, ref)
	case forge.KindIssue, forge.KindPullRequest:
		return f.issue(ctx, ref)
	default:
		return continuation{}, nil, fmt.Errorf("%w: GitHub thread kind %q", forge.ErrUnavailable, ref.Kind)
	}
}

func (f *Forge) issue(ctx context.Context, ref forge.ThreadRef) (continuation, []forge.Post, error) {
	var data struct {
		Repository *struct {
			Thread jsontext.Value `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
	owner, name, _ := strings.Cut(ref.Repo, "/")
	err := f.client.graphQL(ctx, graphQLRequest{
		OperationName: issueThreadQuery.name, Query: issueThreadQuery.text(f.emails),
		Variables: map[string]any{"owner": owner, "name": name, "number": ref.Number},
	}, &data)
	if err := f.threadError(err, ref, "repository", "repository.issueOrPullRequest"); err != nil {
		return continuation{}, nil, err
	}
	if data.Repository == nil || len(data.Repository.Thread) == 0 || data.Repository.Thread.Kind() == 'n' {
		return continuation{}, nil, fmt.Errorf("%w: %s", forge.ErrUnavailable, ref.Name())
	}
	var thread node
	if err := json.Unmarshal(data.Repository.Thread, &thread); err != nil {
		return continuation{}, nil, fmt.Errorf("decode %s: %w", ref.Name(), err)
	}
	label, kind := "#"+strconv.Itoa(thread.Number), postIssue
	if thread.Typename == "PullRequest" {
		label, kind = "PR #"+strconv.Itoa(thread.Number), postPullRequest
	}
	cont := continuation{
		Head: forge.ThreadHead{
			ConversationID: thread.ID,
			Title:          fmt.Sprintf("[%s] %s (%s)", ref.Repo, thread.Title, label),
			URL:            thread.URL,
		},
		Repo: ref.Repo, Number: thread.Number,
	}
	posts := []forge.Post{f.post(cont, thread, kind, raw(data.Repository.Thread, "comments", "reviews"), thread.Title, thread.Body, thread.CreatedAt, nil)}
	more, err := f.connectionPosts(&cont, connComments, thread.ID, thread.Comments)
	if err != nil {
		return continuation{}, nil, err
	}
	posts = append(posts, more...)
	more, err = f.connectionPosts(&cont, connReviews, thread.ID, thread.Reviews)
	if err != nil {
		return continuation{}, nil, err
	}
	return cont, append(posts, more...), nil
}

func (f *Forge) discussion(ctx context.Context, ref forge.ThreadRef) (continuation, []forge.Post, error) {
	var data struct {
		Repository *struct {
			Discussion jsontext.Value `json:"discussion"`
		} `json:"repository"`
	}
	owner, name, _ := strings.Cut(ref.Repo, "/")
	err := f.client.graphQL(ctx, graphQLRequest{
		OperationName: discussionThreadQuery.name, Query: discussionThreadQuery.text(f.emails),
		Variables: map[string]any{"owner": owner, "name": name, "number": ref.Number},
	}, &data)
	if err := f.threadError(err, ref, "repository", "repository.discussion"); err != nil {
		return continuation{}, nil, err
	}
	if data.Repository == nil || len(data.Repository.Discussion) == 0 || data.Repository.Discussion.Kind() == 'n' {
		return continuation{}, nil, fmt.Errorf("%w: %s", forge.ErrUnavailable, ref.Name())
	}
	var thread node
	if err := json.Unmarshal(data.Repository.Discussion, &thread); err != nil {
		return continuation{}, nil, fmt.Errorf("decode %s: %w", ref.Name(), err)
	}
	cont := continuation{
		Head: forge.ThreadHead{
			ConversationID: thread.ID,
			Title:          fmt.Sprintf("[%s] %s (Discussion #%d)", ref.Repo, thread.Title, thread.Number),
			URL:            thread.URL,
		},
		Repo: ref.Repo, Number: thread.Number,
	}
	var extra map[string]any
	if thread.Category != nil {
		extra = map[string]any{"category": thread.Category.Name}
	}
	posts := []forge.Post{f.post(cont, thread, postDiscussion, raw(data.Repository.Discussion, "comments"), thread.Title, thread.Body, thread.CreatedAt, extra)}
	more, err := f.connectionPosts(&cont, connDiscussionComments, thread.ID, thread.Comments)
	if err != nil {
		return continuation{}, nil, err
	}
	return cont, append(posts, more...), nil
}

// nextPage fetches the first pending connection page.
func (f *Forge) nextPage(ctx context.Context, cont *continuation) ([]forge.Post, error) {
	pending := cont.Pending[0]
	cont.Pending = cont.Pending[1:]
	query, ok := pageQueries[pending.Connection]
	if !ok {
		return nil, fmt.Errorf("unknown GitHub connection %q in a continuation", pending.Connection)
	}
	var data struct {
		Node *struct {
			Page connection `json:"page"`
		} `json:"node"`
	}
	err := f.client.graphQL(ctx, graphQLRequest{
		OperationName: query.name, Query: query.text(f.emails),
		Variables: map[string]any{"id": pending.Node, "after": pending.After},
	}, &data)
	var errs *graphQLResponseError
	switch {
	case errors.As(err, &errs) && errs.missingAt("node"):
		// The node was deleted since the earlier page; nothing more of it.
		return nil, nil
	case errors.As(err, &errs) && errs.optional():
	case err != nil:
		return nil, err
	}
	if data.Node == nil {
		return nil, nil
	}
	return f.connectionPosts(cont, pending.Connection, pending.Node, data.Node.Page)
}

// connectionPosts turns one page of a connection into posts and queues the
// pages that remain: of this connection, and of connections nested in its
// nodes.
func (f *Forge) connectionPosts(cont *continuation, conn, owner string, page connection) ([]forge.Post, error) {
	if page.PageInfo.HasNextPage {
		cont.Pending = append(cont.Pending, pendingPage{Connection: conn, Node: owner, After: page.PageInfo.EndCursor})
	}
	var posts []forge.Post
	for _, rawNode := range page.Nodes {
		var n node
		if err := json.Unmarshal(rawNode, &n); err != nil {
			return nil, fmt.Errorf("decode a %s node: %w", conn, err)
		}
		switch conn {
		case connComments:
			posts = append(posts, f.post(*cont, n, postIssueComment, raw(rawNode), "", n.Body, n.CreatedAt, nil))
		case connReviews:
			if n.State == "PENDING" {
				// A pending review is a draft only its author can see.
				continue
			}
			if body := reviewBody(n); body != "" {
				at := n.CreatedAt
				if n.SubmittedAt != nil {
					at = *n.SubmittedAt
				}
				posts = append(posts, f.post(*cont, n, postReview, raw(rawNode, "comments"), "", body, at,
					map[string]any{"review_state": strings.ToLower(n.State)}))
			}
			more, err := f.connectionPosts(cont, connReviewComments, n.ID, n.Comments)
			if err != nil {
				return nil, err
			}
			posts = append(posts, more...)
		case connReviewComments:
			posts = append(posts, f.reviewComment(*cont, n, raw(rawNode), owner))
		case connDiscussionComments:
			var extra map[string]any
			if n.IsAnswer {
				extra = map[string]any{"is_answer": true}
			}
			posts = append(posts, f.post(*cont, n, postDiscussionComment, raw(rawNode, "replies"), "", n.Body, n.CreatedAt, extra))
			more, err := f.connectionPosts(cont, connReplies, n.ID, n.Replies)
			if err != nil {
				return nil, err
			}
			posts = append(posts, more...)
		case connReplies:
			replyTo := owner
			if n.ReplyTo != nil && n.ReplyTo.ID != "" {
				replyTo = n.ReplyTo.ID
			}
			posts = append(posts, f.post(*cont, n, postDiscussionReply, raw(rawNode), "", n.Body, n.CreatedAt,
				map[string]any{"in_reply_to": replyTo}))
		}
	}
	return posts, nil
}

func (f *Forge) reviewComment(cont continuation, n node, evidence []byte, review string) forge.Post {
	extra := map[string]any{"path": n.Path, "review": review}
	line := n.Line
	if line == nil {
		line = n.OriginalLine
	}
	location := "`" + n.Path + "`"
	if line != nil {
		extra["line"] = *line
		location += " line " + strconv.Itoa(*line)
	}
	if n.ReplyTo != nil {
		extra["in_reply_to"] = n.ReplyTo.ID
	}
	return f.post(cont, n, postReviewComment, evidence, "", location+"\n\n"+n.Body, n.CreatedAt, extra)
}

// reviewBody is what a review says: its verdict, then its text. A plain
// comment review without text only groups its review comments, which are
// posts of their own.
func reviewBody(n node) string {
	verdict := ""
	switch n.State {
	case "APPROVED":
		verdict = "Approved these changes."
	case "CHANGES_REQUESTED":
		verdict = "Requested changes."
	case "DISMISSED":
		verdict = "Review dismissed."
	}
	body := strings.TrimSpace(n.Body)
	switch {
	case verdict == "":
		return body
	case body == "":
		return verdict
	default:
		return verdict + "\n\n" + body
	}
}

func (f *Forge) post(cont continuation, n node, kind string, evidence []byte, title, body string, at time.Time, extra map[string]any) forge.Post {
	metadata := map[string]any{"repository": cont.Repo}
	if cont.Number > 0 {
		metadata["number"] = cont.Number
	}
	maps.Copy(metadata, extra)
	return forge.Post{
		ID: n.ID, Kind: kind, Author: author(n.Author), Title: title, Body: body,
		URL: n.URL, CreatedAt: at, Metadata: metadata, Raw: evidence,
	}
}

// author is the account a post's author is. A deleted account is GitHub's
// ghost; an imported placeholder (a mannequin) is a migrated name.
func author(a *actor) forge.Account {
	switch {
	case a == nil || a.Login == "":
		return forge.Account{Kind: forge.AuthorUnavailable, Login: ghostLogin, URL: ghostURL}
	case a.Typename == "Mannequin":
		return forge.Account{Kind: forge.AuthorMigrated, Login: a.Login}
	case a.DatabaseID <= 0:
		return forge.Account{Kind: forge.AuthorUnavailable, Login: a.Login, URL: a.URL}
	default:
		return forge.Account{
			Kind: forge.AuthorAccount, ID: strconv.FormatInt(a.DatabaseID, 10), Login: a.Login,
			URL: a.URL, Email: a.Email, Bot: a.Typename == "Bot",
		}
	}
}

// raw is a node's JSON without its nested connections, which are posts of
// their own; the core stores it in canonical form.
func raw(value jsontext.Value, drop ...string) []byte {
	var object map[string]jsontext.Value
	if len(drop) == 0 || json.Unmarshal(value, &object) != nil {
		return value
	}
	for _, key := range drop {
		delete(object, key)
	}
	data, err := json.Marshal(object)
	if err != nil {
		return value
	}
	return data
}

// threadError reads a thread query's error: missing at one of the root
// paths, the thread is gone or forbidden; missing only in optional fields,
// the data is whole enough; otherwise the thread stays queued.
func (f *Forge) threadError(err error, ref forge.ThreadRef, roots ...string) error {
	var errs *graphQLResponseError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &errs) && errs.missingAt(roots...):
		return fmt.Errorf("%w: %s: %w", forge.ErrUnavailable, ref.Name(), err)
	case errors.As(err, &errs) && errs.optional():
		return nil
	default:
		return err
	}
}

// optional reports whether every error says a field a post may lack is
// missing or forbidden. Any other error, such as a timeout, leaves the
// thread incomplete, to be fetched again.
func (e *graphQLResponseError) optional() bool {
	for _, err := range e.errors {
		if len(err.Path) == 0 || (err.Type != "NOT_FOUND" && err.Type != "FORBIDDEN") {
			return false
		}
		last := fmt.Sprint(err.Path[len(err.Path)-1])
		found := false
		for _, field := range optionalPaths {
			if last == field {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type release struct {
	NodeID      string     `json:"node_id"`
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	Prerelease  bool       `json:"prerelease"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at"`
	Author      *struct {
		Login   string `json:"login"`
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
		Type    string `json:"type"`
	} `json:"author"`
}

// release fetches the release a notification names: one post.
func (f *Forge) release(ctx context.Context, ref forge.ThreadRef) (continuation, []forge.Post, error) {
	var evidence jsontext.Value
	if _, err := f.client.getJSON(ctx, ref.Locator, nil, nil, &evidence); err != nil {
		return continuation{}, nil, err
	}
	var r release
	if err := json.Unmarshal(evidence, &r); err != nil || r.NodeID == "" {
		return continuation{}, nil, fmt.Errorf("%w: %s", forge.ErrUnavailable, ref.Name())
	}
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = r.TagName
	}
	at := r.CreatedAt
	if r.PublishedAt != nil {
		at = *r.PublishedAt
	}
	a := &actor{Login: ghostLogin}
	if r.Author != nil {
		a = &actor{Login: r.Author.Login, URL: r.Author.HTMLURL, DatabaseID: r.Author.ID}
		if r.Author.Type == "Bot" {
			a.Typename = "Bot"
		}
	}
	cont := continuation{
		Head: forge.ThreadHead{
			ConversationID: r.NodeID,
			Title:          fmt.Sprintf("[%s] Release %s", ref.Repo, name),
			URL:            r.HTMLURL,
		},
		Repo: ref.Repo,
	}
	extra := map[string]any{"tag": r.TagName}
	if r.Prerelease {
		extra["prerelease"] = true
	}
	n := node{ID: r.NodeID, URL: r.HTMLURL, Author: a}
	return cont, []forge.Post{f.post(cont, n, postRelease, raw(evidence), name, r.Body, at, extra)}, nil
}
