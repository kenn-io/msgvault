package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// GraphQL fragments. A query document may only define the fragments it
// uses, so each query lists its own.
const (
	actorFragment = `fragment Actor on Actor { login url ... on User { databaseId } ... on Bot { databaseId } }`

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

var (
	issueThreadQuery = `query IssueThread($owner: String!, $name: String!, $number: Int!) {
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
}
` + strings.Join([]string{actorFragment, issueCommentFragment, reviewCommentFragment, reviewFragment}, "\n")

	discussionThreadQuery = `query DiscussionThread($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    discussion(number: $number) {
      id number title url body createdAt category { name } author { ...Actor }
      comments(first: 100) { pageInfo { hasNextPage endCursor } nodes { ...DiscussionComment } }
    }
  }
}
` + strings.Join([]string{actorFragment, discussionReplyFragment, discussionCommentFragment}, "\n")

	moreIssueCommentsQuery = `query MoreIssueComments($id: ID!, $after: String) {
  node(id: $id) {
    ... on Issue { page: comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...IssueComment } } }
    ... on PullRequest { page: comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...IssueComment } } }
  }
}
` + strings.Join([]string{actorFragment, issueCommentFragment}, "\n")

	moreReviewsQuery = `query MoreReviews($id: ID!, $after: String) {
  node(id: $id) {
    ... on PullRequest { page: reviews(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...Review } } }
  }
}
` + strings.Join([]string{actorFragment, reviewCommentFragment, reviewFragment}, "\n")

	moreReviewCommentsQuery = `query MoreReviewComments($id: ID!, $after: String) {
  node(id: $id) {
    ... on PullRequestReview { page: comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...ReviewComment } } }
  }
}
` + strings.Join([]string{actorFragment, reviewCommentFragment}, "\n")

	moreDiscussionCommentsQuery = `query MoreDiscussionComments($id: ID!, $after: String) {
  node(id: $id) {
    ... on Discussion { page: comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...DiscussionComment } } }
  }
}
` + strings.Join([]string{actorFragment, discussionReplyFragment, discussionCommentFragment}, "\n")

	moreRepliesQuery = `query MoreReplies($id: ID!, $after: String) {
  node(id: $id) {
    ... on DiscussionComment { page: replies(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...DiscussionReply } } }
  }
}
` + strings.Join([]string{actorFragment, discussionReplyFragment}, "\n")
)

type actor struct {
	Login      string `json:"login"`
	URL        string `json:"url"`
	DatabaseID int64  `json:"databaseId"`
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type connection[T any] struct {
	PageInfo pageInfo `json:"pageInfo"`
	Nodes    []T      `json:"nodes"`
}

type nodeRef struct {
	ID string `json:"id"`
}

type issueComment struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Author    *actor    `json:"author"`
}

type reviewComment struct {
	ID           string    `json:"id"`
	URL          string    `json:"url"`
	Body         string    `json:"body"`
	Path         string    `json:"path"`
	Line         *int      `json:"line"`
	OriginalLine *int      `json:"originalLine"`
	CreatedAt    time.Time `json:"createdAt"`
	ReplyTo      *nodeRef  `json:"replyTo"`
	Author       *actor    `json:"author"`
}

type review struct {
	ID          string                    `json:"id"`
	URL         string                    `json:"url"`
	Body        string                    `json:"body"`
	State       string                    `json:"state"`
	SubmittedAt *time.Time                `json:"submittedAt"`
	CreatedAt   time.Time                 `json:"createdAt"`
	Author      *actor                    `json:"author"`
	Comments    connection[reviewComment] `json:"comments"`
}

type issueThread struct {
	Typename  string                   `json:"__typename"`
	ID        string                   `json:"id"`
	Number    int                      `json:"number"`
	Title     string                   `json:"title"`
	URL       string                   `json:"url"`
	Body      string                   `json:"body"`
	CreatedAt time.Time                `json:"createdAt"`
	Author    *actor                   `json:"author"`
	Comments  connection[issueComment] `json:"comments"`
	Reviews   connection[review]       `json:"reviews"`
}

type discussionReply struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	ReplyTo   *nodeRef  `json:"replyTo"`
	Author    *actor    `json:"author"`
}

type discussionComment struct {
	ID        string                      `json:"id"`
	URL       string                      `json:"url"`
	Body      string                      `json:"body"`
	CreatedAt time.Time                   `json:"createdAt"`
	IsAnswer  bool                        `json:"isAnswer"`
	Author    *actor                      `json:"author"`
	Replies   connection[discussionReply] `json:"replies"`
}

type discussionThread struct {
	ID        string    `json:"id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Category  struct {
		Name string `json:"name"`
	} `json:"category"`
	Author   *actor                        `json:"author"`
	Comments connection[discussionComment] `json:"comments"`
}

type releaseAuthor struct {
	Login   string `json:"login"`
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

type release struct {
	NodeID      string         `json:"node_id"`
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	Body        string         `json:"body"`
	HTMLURL     string         `json:"html_url"`
	Prerelease  bool           `json:"prerelease"`
	CreatedAt   time.Time      `json:"created_at"`
	PublishedAt *time.Time     `json:"published_at"`
	Author      *releaseAuthor `json:"author"`
}

// fetchIssueThread returns an issue or pull request with every comment,
// review and review comment.
func (c *Client) fetchIssueThread(ctx context.Context, t Thread) (issueThread, error) {
	var data struct {
		Repository *struct {
			IssueOrPullRequest *issueThread `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
	if err := c.graphQL(ctx, graphQLRequest{
		OperationName: "IssueThread", Query: issueThreadQuery,
		Variables: map[string]any{"owner": t.Owner, "name": t.Repo, "number": t.Number},
	}, &data); err != nil {
		return issueThread{}, err
	}
	if data.Repository == nil || data.Repository.IssueOrPullRequest == nil {
		return issueThread{}, fmt.Errorf("%w: %s", ErrUnavailable, t.Name())
	}
	thread := *data.Repository.IssueOrPullRequest
	var err error
	if thread.Comments.Nodes, err = morePages(ctx, c, "MoreIssueComments", moreIssueCommentsQuery, thread.ID, thread.Comments); err != nil {
		return issueThread{}, err
	}
	if thread.Reviews.Nodes, err = morePages(ctx, c, "MoreReviews", moreReviewsQuery, thread.ID, thread.Reviews); err != nil {
		return issueThread{}, err
	}
	for i := range thread.Reviews.Nodes {
		r := &thread.Reviews.Nodes[i]
		if r.Comments.Nodes, err = morePages(ctx, c, "MoreReviewComments", moreReviewCommentsQuery, r.ID, r.Comments); err != nil {
			return issueThread{}, err
		}
	}
	return thread, nil
}

// fetchDiscussionThread returns a discussion with every comment and reply.
func (c *Client) fetchDiscussionThread(ctx context.Context, t Thread) (discussionThread, error) {
	var data struct {
		Repository *struct {
			Discussion *discussionThread `json:"discussion"`
		} `json:"repository"`
	}
	if err := c.graphQL(ctx, graphQLRequest{
		OperationName: "DiscussionThread", Query: discussionThreadQuery,
		Variables: map[string]any{"owner": t.Owner, "name": t.Repo, "number": t.Number},
	}, &data); err != nil {
		return discussionThread{}, err
	}
	if data.Repository == nil || data.Repository.Discussion == nil {
		return discussionThread{}, fmt.Errorf("%w: %s", ErrUnavailable, t.Name())
	}
	thread := *data.Repository.Discussion
	var err error
	if thread.Comments.Nodes, err = morePages(ctx, c, "MoreDiscussionComments", moreDiscussionCommentsQuery, thread.ID, thread.Comments); err != nil {
		return discussionThread{}, err
	}
	for i := range thread.Comments.Nodes {
		comment := &thread.Comments.Nodes[i]
		if comment.Replies.Nodes, err = morePages(ctx, c, "MoreReplies", moreRepliesQuery, comment.ID, comment.Replies); err != nil {
			return discussionThread{}, err
		}
	}
	return thread, nil
}

// fetchRelease returns the release a notification names.
func (c *Client) fetchRelease(ctx context.Context, t Thread) (release, error) {
	var r release
	if _, err := c.getJSON(ctx, t.ReleaseURL, nil, nil, &r); err != nil {
		return release{}, err
	}
	if r.NodeID == "" {
		return release{}, fmt.Errorf("%w: %s", ErrUnavailable, t.Name())
	}
	return r, nil
}

// morePages reads the pages of a connection after its first one. Each
// query aliases the connection as "page" under node(id:).
func morePages[T any](ctx context.Context, c *Client, operation, query, id string, conn connection[T]) ([]T, error) {
	nodes := conn.Nodes
	for conn.PageInfo.HasNextPage {
		var data struct {
			Node *struct {
				Page connection[T] `json:"page"`
			} `json:"node"`
		}
		if err := c.graphQL(ctx, graphQLRequest{
			OperationName: operation, Query: query,
			Variables: map[string]any{"id": id, "after": conn.PageInfo.EndCursor},
		}, &data); err != nil {
			return nil, err
		}
		if data.Node == nil {
			return nil, fmt.Errorf("%w: %s", ErrUnavailable, id)
		}
		conn = data.Node.Page
		nodes = append(nodes, conn.Nodes...)
		if len(conn.Nodes) == 0 {
			break
		}
	}
	return nodes, nil
}
