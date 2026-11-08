package github

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/messageimport"
)

const (
	SourceType = "github"
	// MessageType is the type of every message a GitHub source archives;
	// metadata.kind says which kind of post it is.
	MessageType = "github"
	// ParticipantIdentifierType names GitHub account IDs among participant
	// identifiers: one GitHub account is one participant in every source.
	ParticipantIdentifierType = "github"

	ghostLogin = "ghost"
	ghostURL   = "https://github.com/ghost"
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
)

// threadMessages is one thread as msgvault archives it: a conversation and
// its messages, oldest post first.
type threadMessages struct {
	ConversationID string
	Title          string
	URL            string
	Messages       []messageimport.ImportedMessage
}

// conversationTitle names a thread as GitHub's notification emails do, so
// a thread reads the same whichever way it was archived.
func conversationTitle(t Thread, title string, number int, kind Kind) string {
	label := "#" + strconv.Itoa(number)
	switch kind {
	case KindPullRequest:
		label = "PR " + label
	case KindDiscussion:
		label = "Discussion " + label
	case KindIssue, KindRelease:
	}
	return fmt.Sprintf("[%s/%s] %s (%s)", t.Owner, t.Repo, title, label)
}

func issueMessages(t Thread, thread issueThread) threadMessages {
	kind, rootKind := KindIssue, postIssue
	if thread.Typename == "PullRequest" {
		kind, rootKind = KindPullRequest, postPullRequest
	}
	out := threadMessages{
		ConversationID: thread.ID,
		Title:          conversationTitle(t, thread.Title, thread.Number, kind),
		URL:            thread.URL,
	}
	base := baseMetadata(t, thread.Number)
	out.add(post{
		id: thread.ID, kind: rootKind, author: thread.Author, title: thread.Title,
		body: thread.Body, url: thread.URL, at: thread.CreatedAt,
	}, base, nil)
	for _, comment := range thread.Comments.Nodes {
		out.add(post{
			id: comment.ID, kind: postIssueComment, author: comment.Author,
			body: comment.Body, url: comment.URL, at: comment.CreatedAt,
		}, base, nil)
	}
	for _, r := range thread.Reviews.Nodes {
		if r.State == "PENDING" {
			// A pending review is a draft only its author can see.
			continue
		}
		if body := reviewBody(r); body != "" {
			at := r.CreatedAt
			if r.SubmittedAt != nil {
				at = *r.SubmittedAt
			}
			out.add(post{
				id: r.ID, kind: postReview, author: r.Author, body: body, url: r.URL, at: at,
			}, base, map[string]any{"review_state": strings.ToLower(r.State)})
		}
		for _, comment := range r.Comments.Nodes {
			extra := map[string]any{"path": comment.Path, "review": r.ID}
			line := comment.Line
			if line == nil {
				line = comment.OriginalLine
			}
			location := "`" + comment.Path + "`"
			if line != nil {
				extra["line"] = *line
				location += " line " + strconv.Itoa(*line)
			}
			if comment.ReplyTo != nil {
				extra["in_reply_to"] = comment.ReplyTo.ID
			}
			out.add(post{
				id: comment.ID, kind: postReviewComment, author: comment.Author,
				body: location + "\n\n" + comment.Body, url: comment.URL, at: comment.CreatedAt,
			}, base, extra)
		}
	}
	return out
}

// reviewBody is what a review says: its verdict, then its text. A plain
// comment review without text only groups its review comments, which are
// messages of their own.
func reviewBody(r review) string {
	verdict := ""
	switch r.State {
	case "APPROVED":
		verdict = "Approved these changes."
	case "CHANGES_REQUESTED":
		verdict = "Requested changes."
	case "DISMISSED":
		verdict = "Review dismissed."
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

func discussionMessages(t Thread, thread discussionThread) threadMessages {
	out := threadMessages{
		ConversationID: thread.ID,
		Title:          conversationTitle(t, thread.Title, thread.Number, KindDiscussion),
		URL:            thread.URL,
	}
	base := baseMetadata(t, thread.Number)
	out.add(post{
		id: thread.ID, kind: postDiscussion, author: thread.Author, title: thread.Title,
		body: thread.Body, url: thread.URL, at: thread.CreatedAt,
	}, base, map[string]any{"category": thread.Category.Name})
	for _, comment := range thread.Comments.Nodes {
		var extra map[string]any
		if comment.IsAnswer {
			extra = map[string]any{"is_answer": true}
		}
		out.add(post{
			id: comment.ID, kind: postDiscussionComment, author: comment.Author,
			body: comment.Body, url: comment.URL, at: comment.CreatedAt,
		}, base, extra)
		for _, reply := range comment.Replies.Nodes {
			replyTo := comment.ID
			if reply.ReplyTo != nil && reply.ReplyTo.ID != "" {
				replyTo = reply.ReplyTo.ID
			}
			out.add(post{
				id: reply.ID, kind: postDiscussionReply, author: reply.Author,
				body: reply.Body, url: reply.URL, at: reply.CreatedAt,
			}, base, map[string]any{"in_reply_to": replyTo})
		}
	}
	return out
}

func releaseMessages(t Thread, r release) threadMessages {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = r.TagName
	}
	at := r.CreatedAt
	if r.PublishedAt != nil {
		at = *r.PublishedAt
	}
	var author *actor
	if r.Author != nil {
		author = &actor{Login: r.Author.Login, URL: r.Author.HTMLURL, DatabaseID: r.Author.ID}
	}
	out := threadMessages{
		ConversationID: r.NodeID,
		Title:          fmt.Sprintf("[%s/%s] Release %s", t.Owner, t.Repo, name),
		URL:            r.HTMLURL,
	}
	extra := map[string]any{"tag": r.TagName}
	if r.Prerelease {
		extra["prerelease"] = true
	}
	out.add(post{
		id: r.NodeID, kind: postRelease, author: author, title: name,
		body: r.Body, url: r.HTMLURL, at: at,
	}, map[string]any{"repository": t.Owner + "/" + t.Repo}, extra)
	return out
}

func baseMetadata(t Thread, number int) map[string]any {
	return map[string]any{"repository": t.Owner + "/" + t.Repo, "number": number}
}

// post is one message of a thread before it becomes an import message.
type post struct {
	id, kind, title, body, url string
	author                     *actor
	at                         time.Time
}

func (m *threadMessages) add(p post, base, extra map[string]any) {
	metadata := make(map[string]any, len(base)+len(extra)+1)
	for key, value := range base {
		metadata[key] = value
	}
	for key, value := range extra {
		if text, ok := value.(string); ok {
			value = clean(text)
		}
		metadata[key] = value
	}
	metadata["kind"] = p.kind
	conversation := messageimport.MessageImportConversation{ExternalID: m.ConversationID}
	if len(m.Messages) == 0 {
		// Only the opening post names the conversation, so retitling a
		// thread rewrites that one post rather than every reply.
		conversation.Title = clean(m.Title)
	}
	m.Messages = append(m.Messages, messageimport.ImportedMessage{
		ExternalID:   p.id,
		MessageType:  MessageType,
		Conversation: conversation,
		Sender:       sender(p.author),
		Title:        clean(p.title),
		URL:          p.url,
		SentAt:       p.at.UTC().Format(time.RFC3339),
		BodyMarkdown: clean(p.body),
		Metadata:     metadata,
	})
}

// sender is the participant a post's author becomes: the GitHub account,
// keyed by its ID and named by its login. A deleted account is GitHub's
// ghost.
func sender(a *actor) messageimport.MessageImportSender {
	if a == nil || a.Login == "" {
		return messageimport.MessageImportSender{Name: ghostLogin, URL: ghostURL}
	}
	s := messageimport.MessageImportSender{Name: a.Login, URL: a.URL}
	if a.DatabaseID > 0 {
		s.ID = strconv.FormatInt(a.DatabaseID, 10)
	}
	return s
}

// clean drops NUL characters, which GitHub text may hold and the archive
// can't store.
func clean(text string) string {
	return strings.ReplaceAll(text, "\x00", "")
}
