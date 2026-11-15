package gitea

import (
	"cmp"
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
	postIssue         = "issue"
	postPullRequest   = "pull_request"
	postIssueComment  = "issue_comment"
	postReview        = "review"
	postReviewComment = "review_comment"
)

// Review states a sync leaves out: a draft only its author sees, and a
// request for a review, which is no post (modules/structs/pull_review.go).
const (
	reviewPending   = "PENDING"
	reviewRequested = "REQUEST_REVIEW"
)

// issueActivity are an issue's or pull request's fields that change with
// the rest of its thread, not with the opening post: its evidence leaves
// them out, so a reply doesn't rewrite it. A pinned issue's place moves
// when another is unpinned.
var issueActivity = []string{"updated_at", "comments", "pin_order"}

// reviewActivity are a review's fields that change with the pull request,
// not with the review: a push makes it stale, and a reply to one of its code
// comments changes its comment count.
var reviewActivity = []string{"stale", "updated_at", "comments_count"}

// continuation is the rest of a thread still to fetch: the pages its
// earlier pages said remain, in order.
type continuation struct {
	Head    forge.ThreadHead `json:"head"`
	Repo    string           `json:"repo"`
	Number  int              `json:"number"`
	Pending []pendingPage    `json:"pending"`
}

// pendingPage is a page a thread still owes: a page of a pull request's
// reviews, or one review's comments.
type pendingPage struct {
	Reviews int   `json:"reviews,omitzero"`
	Review  int64 `json:"review,omitzero"`
}

// encode is the continuation as Fetch resumes from it; nil once nothing
// remains.
func (c continuation) encode() []byte {
	if len(c.Pending) == 0 {
		return nil
	}
	data, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		panic(fmt.Sprintf("encode Gitea continuation: %v", err)) // strings and ints always encode
	}
	return data
}

// issueObject is what a sync reads of Gitea's Issue, which also stands for
// a pull request (modules/structs/issue.go). Forgejo still names a migrated
// author in original_author.
type issueObject struct {
	ID             int64     `json:"id"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	HTMLURL        string    `json:"html_url"`
	CreatedAt      time.Time `json:"created_at"`
	User           *user     `json:"user"`
	OriginalAuthor string    `json:"original_author"`
	PullRequest    *struct{} `json:"pull_request"`
	Repository     *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// commentObject is what a sync reads of Gitea's Comment
// (modules/structs/issue_comment.go).
type commentObject struct {
	ID             int64     `json:"id"`
	Body           string    `json:"body"`
	HTMLURL        string    `json:"html_url"`
	CreatedAt      time.Time `json:"created_at"`
	User           *user     `json:"user"`
	OriginalAuthor string    `json:"original_author"`
}

// reviewObject is what a sync reads of Gitea's PullReview
// (modules/structs/pull_review.go).
type reviewObject struct {
	ID            int64     `json:"id"`
	User          *user     `json:"user"`
	State         string    `json:"state"`
	Body          string    `json:"body"`
	Dismissed     bool      `json:"dismissed"`
	CommentsCount int       `json:"comments_count"`
	SubmittedAt   time.Time `json:"submitted_at"`
	HTMLURL       string    `json:"html_url"`
}

// reviewCommentObject is what a sync reads of Gitea's PullReviewComment. A
// comment on a changed line has its position; one on a removed line, its
// original position (services/convert/pull_review.go).
type reviewCommentObject struct {
	ID               int64     `json:"id"`
	Body             string    `json:"body"`
	HTMLURL          string    `json:"html_url"`
	CreatedAt        time.Time `json:"created_at"`
	User             *user     `json:"user"`
	Resolver         *user     `json:"resolver"`
	Path             string    `json:"path"`
	Position         int       `json:"position"`
	OriginalPosition int       `json:"original_position"`
}

// Fetch streams a thread's posts, one page at a time: the issue or pull
// request with its comments, then a pull request's reviews page by page,
// each followed by its review comments.
func (f *Forge) Fetch(ctx context.Context, ref forge.ThreadRef, raw []byte,
	page func(forge.ThreadHead, []forge.Post, []byte) error,
) error {
	var cont continuation
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cont); err != nil {
			return fmt.Errorf("decode Gitea continuation of %s: %w", ref.Name(), err)
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

// firstPage fetches the issue or pull request and its comments, which
// Gitea returns in one unpaged list.
func (f *Forge) firstPage(ctx context.Context, ref forge.ThreadRef) (continuation, []forge.Post, error) {
	if ref.Number <= 0 {
		return continuation{}, nil, fmt.Errorf("%w: %s has no number", forge.ErrUnavailable, ref.Name())
	}
	var rawIssue jsontext.Value
	if _, err := f.get(ctx, issuePath(ref.Repo, ref.Number), nil, &rawIssue); err != nil {
		return continuation{}, nil, fmt.Errorf("fetch %s: %w", ref.Name(), err)
	}
	var issue issueObject
	if err := json.Unmarshal(rawIssue, &issue); err != nil {
		return continuation{}, nil, fmt.Errorf("decode %s: %w", ref.Name(), err)
	}
	if issue.ID <= 0 || issue.Number <= 0 {
		return continuation{}, nil, fmt.Errorf("%w: %s", forge.ErrUnavailable, ref.Name())
	}
	// The issue names its repository as it is now, after any move.
	repo := ref.Repo
	if issue.Repository != nil && issue.Repository.FullName != "" {
		repo = issue.Repository.FullName
	}
	kind := postIssue
	if issue.PullRequest != nil {
		kind = postPullRequest
	}
	cont := continuation{
		Head: forge.ThreadHead{
			ConversationID: postID(postIssue, issue.ID),
			// Gitea's notification emails name issues and pull requests
			// alike (services/mailer/mail_issue_common.go).
			Title: fmt.Sprintf("[%s] %s (#%d)", repo, issue.Title, issue.Number),
			URL:   issue.HTMLURL,
		},
		Repo: repo, Number: issue.Number,
	}
	posts := []forge.Post{cont.post(postID(postIssue, issue.ID), kind, author(issue.User, issue.OriginalAuthor),
		issue.Title, issue.Body, issue.HTMLURL, issue.CreatedAt, evidence(rawIssue, issueActivity...), nil)}

	var comments []jsontext.Value
	if _, err := f.get(ctx, issuePath(repo, issue.Number)+"/comments", nil, &comments); err != nil {
		return continuation{}, nil, fmt.Errorf("fetch the comments of %s: %w", ref.Name(), err)
	}
	for _, rawComment := range comments {
		var c commentObject
		if err := json.Unmarshal(rawComment, &c); err != nil {
			return continuation{}, nil, fmt.Errorf("decode a comment of %s: %w", ref.Name(), err)
		}
		posts = append(posts, cont.post(postID("comment", c.ID), postIssueComment, author(c.User, c.OriginalAuthor),
			"", c.Body, c.HTMLURL, c.CreatedAt, evidence(rawComment), nil))
	}
	if issue.PullRequest != nil {
		cont.Pending = []pendingPage{{Reviews: 1}}
	}
	return cont, posts, nil
}

// nextPage fetches the first page the thread still owes.
func (f *Forge) nextPage(ctx context.Context, cont *continuation) ([]forge.Post, error) {
	next := cont.Pending[0]
	cont.Pending = cont.Pending[1:]
	pull := repoPath(cont.Repo) + "/pulls/" + strconv.Itoa(cont.Number)
	if next.Reviews > 0 {
		return f.reviews(ctx, cont, pull, next.Reviews)
	}
	return f.reviewComments(ctx, cont, pull, next.Review)
}

// reviews fetches one page of a pull request's reviews. Each review's
// comments, then the next page of reviews, come next.
func (f *Forge) reviews(ctx context.Context, cont *continuation, pull string, page int) ([]forge.Post, error) {
	limit, err := f.pageLimit(ctx)
	if err != nil {
		return nil, err
	}
	var batch []jsontext.Value
	header, err := f.get(ctx, pull+"/reviews", pageQuery(page, limit), &batch)
	if err != nil {
		return nil, fmt.Errorf("fetch the reviews of %s#%d: %w", cont.Repo, cont.Number, err)
	}
	var posts []forge.Post
	var owed []pendingPage
	for _, rawReview := range batch {
		var r reviewObject
		if err := json.Unmarshal(rawReview, &r); err != nil {
			return nil, fmt.Errorf("decode a review of %s#%d: %w", cont.Repo, cont.Number, err)
		}
		if r.State == reviewPending || r.State == reviewRequested {
			continue
		}
		if body := reviewBody(r); body != "" {
			extra := map[string]any{"review_state": strings.ToLower(r.State)}
			if r.Dismissed {
				extra["dismissed"] = true
			}
			posts = append(posts, cont.post(postID(postReview, r.ID), postReview, author(r.User, ""),
				"", body, r.HTMLURL, r.SubmittedAt, evidence(rawReview, reviewActivity...), extra))
		}
		if r.CommentsCount > 0 {
			owed = append(owed, pendingPage{Review: r.ID})
		}
	}
	if hasMore(header, page, len(batch), limit) {
		owed = append(owed, pendingPage{Reviews: page + 1})
	}
	cont.Pending = append(owed, cont.Pending...)
	return posts, nil
}

// reviewComments fetches one review's comments on the changed code.
func (f *Forge) reviewComments(ctx context.Context, cont *continuation, pull string, review int64) ([]forge.Post, error) {
	var batch []jsontext.Value
	_, err := f.get(ctx, pull+"/reviews/"+strconv.FormatInt(review, 10)+"/comments", nil, &batch)
	switch {
	case errors.Is(err, forge.ErrUnavailable):
		// The review was deleted since its page was read.
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("fetch the comments of review %d on %s#%d: %w", review, cont.Repo, cont.Number, err)
	}
	posts := make([]forge.Post, 0, len(batch))
	for _, rawComment := range batch {
		var c reviewCommentObject
		if err := json.Unmarshal(rawComment, &c); err != nil {
			return nil, fmt.Errorf("decode a comment of review %d on %s#%d: %w", review, cont.Repo, cont.Number, err)
		}
		extra := map[string]any{"path": c.Path, "review": postID(postReview, review), "resolved": c.Resolver != nil && c.Resolver.Login != ""}
		location := "`" + c.Path + "`"
		if line := cmp.Or(c.Position, c.OriginalPosition); line > 0 {
			extra["line"] = line
			location += " line " + strconv.Itoa(line)
		}
		posts = append(posts, cont.post(postID("comment", c.ID), postReviewComment, author(c.User, ""),
			"", location+"\n\n"+c.Body, c.HTMLURL, c.CreatedAt, evidence(rawComment), extra))
	}
	return posts, nil
}

// reviewBody is what a review says: its verdict, then its text. A plain
// comment review without text only groups its review comments, which are
// posts of their own.
func reviewBody(r reviewObject) string {
	verdict := ""
	switch r.State {
	case "APPROVED":
		verdict = "Approved these changes."
	case "REQUEST_CHANGES":
		verdict = "Requested changes."
	}
	body := strings.TrimSpace(r.Body)
	switch {
	case verdict == "":
		return body
	case body == "":
		return verdict
	default:
		return verdict + "\n\n" + body
	}
}

// postID is a post's ID on its instance. Issues, comments and reviews are
// numbered separately; plain and review comments share one sequence.
func postID(table string, id int64) string {
	return table + ":" + strconv.FormatInt(id, 10)
}

func (c continuation) post(id, kind string, by forge.Account, title, body, url string, at time.Time,
	raw []byte, extra map[string]any,
) forge.Post {
	metadata := map[string]any{"repository": c.Repo, "number": c.Number}
	maps.Copy(metadata, extra)
	return forge.Post{
		ID: id, Kind: kind, Author: by, Title: title, Body: body,
		URL: url, CreatedAt: at, Metadata: metadata, Raw: raw,
	}
}
