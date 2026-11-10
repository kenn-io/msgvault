package gitlab

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

// Post kinds, as metadata.kind names them.
const (
	postIssue        = "issue"
	postMergeRequest = "merge_request"
	// postNote is a comment of its own; postDiscussionNote a comment in a
	// discussion thread; postDiffNote a comment on a line of the diff.
	postNote           = "note"
	postDiscussionNote = "discussion_note"
	postDiffNote       = "diff_note"
	postApproval       = "approval"
)

// Pages a thread fetches after its opening post.
const (
	pageDiscussions = "discussions"
	pageApprovals   = "approvals"
)

// continuation is the rest of a thread still to fetch: the pages its
// earlier pages said remain, in order.
type continuation struct {
	Head   forge.ThreadHead `json:"head"`
	Repo   string           `json:"repo"`
	Number int              `json:"number"`
	// Opened is when the thread was opened, which dates an approval
	// GitLab gives no time.
	Opened  time.Time     `json:"opened"`
	Pending []pendingPage `json:"pending"`
}

type pendingPage struct {
	// Resource is pageDiscussions or pageApprovals.
	Resource string `json:"resource"`
	// Next is the page's URL from GitLab's Link header; empty for the
	// first page.
	Next string `json:"next,omitempty"`
}

// encode is the continuation as Fetch resumes from it; nil once nothing
// remains.
func (c continuation) encode() []byte {
	if len(c.Pending) == 0 {
		return nil
	}
	data, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		panic(fmt.Sprintf("encode GitLab continuation: %v", err)) // strings, ints and times always encode
	}
	return data
}

// Fetch streams a thread's posts: the issue or merge request, then its
// discussions a page at a time, then a merge request's approvals.
func (f *Forge) Fetch(ctx context.Context, ref forge.ThreadRef, raw []byte,
	page func(forge.ThreadHead, []forge.Post, []byte) error,
) error {
	loc, err := parseLocator(ref)
	if err != nil {
		return err
	}
	var cont continuation
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cont); err != nil {
			return fmt.Errorf("decode GitLab continuation of %s: %w", ref.Name(), err)
		}
	} else {
		first, posts, err := f.opening(ctx, ref, loc)
		if err != nil {
			return err
		}
		cont = first
		if err := page(cont.Head, posts, cont.encode()); err != nil {
			return err
		}
	}
	for len(cont.Pending) > 0 {
		posts, err := f.nextPage(ctx, loc, &cont)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", ref.Name(), err)
		}
		if err := page(cont.Head, posts, cont.encode()); err != nil {
			return err
		}
	}
	return nil
}

// thread is an issue or merge request as GitLab returns it alone.
type thread struct {
	ID          int64     `json:"id"`
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	WebURL      string    `json:"web_url"`
	CreatedAt   time.Time `json:"created_at"`
	Author      *user     `json:"author"`
	IssueType   string    `json:"issue_type"`
	References  struct {
		Full string `json:"full"`
	} `json:"references"`
}

// opening fetches the issue or merge request: the thread's first post,
// which names its conversation as GitLab's notification emails do.
func (f *Forge) opening(ctx context.Context, ref forge.ThreadRef, loc locator) (continuation, []forge.Post, error) {
	var evidence jsontext.Value
	if _, err := f.client.get(ctx, loc.path(), nil, &evidence); err != nil {
		return continuation{}, nil, fmt.Errorf("fetch %s: %w", ref.Name(), forbiddenIsUnavailable(err))
	}
	var t thread
	if err := json.Unmarshal(evidence, &t); err != nil {
		return continuation{}, nil, fmt.Errorf("decode %s: %w", ref.Name(), err)
	}
	if t.ID <= 0 || t.IID <= 0 {
		return continuation{}, nil, fmt.Errorf("GitLab returned no thread for %s", ref.Name())
	}
	path := referencePath(t.References.Full)
	if path == "" {
		path = ref.Repo
	}
	key, label, kind := issueGID, "#", postIssue
	if loc.resource == resourceMergeRequests {
		key, label, kind = mergeRequestGID, "!", postMergeRequest
	}
	cont := continuation{
		Head: forge.ThreadHead{
			ConversationID: key + strconv.FormatInt(t.ID, 10),
			Title:          fmt.Sprintf("[%s] %s (%s%d)", path, t.Title, label, t.IID),
			URL:            t.WebURL,
		},
		Repo: path, Number: t.IID, Opened: t.CreatedAt,
		Pending: []pendingPage{{Resource: pageDiscussions}},
	}
	if loc.resource == resourceMergeRequests {
		cont.Pending = append(cont.Pending, pendingPage{Resource: pageApprovals})
	}
	if err := f.resolveAuthors(ctx, []*user{t.Author}); err != nil {
		return continuation{}, nil, err
	}
	var extra map[string]any
	if t.IssueType != "" && t.IssueType != "issue" {
		extra = map[string]any{"issue_type": t.IssueType}
	}
	post := f.post(cont, cont.Head.ConversationID, kind, t.Author, t.Title, t.Description, t.WebURL, t.CreatedAt, extra,
		sorted(evidence, "id", "iid", "project_id", "title", "description", "author", "created_at", "web_url",
			"references", "issue_type", "source_branch", "target_branch"))
	return cont, []forge.Post{post}, nil
}

// nextPage fetches the first pending page.
func (f *Forge) nextPage(ctx context.Context, loc locator, cont *continuation) ([]forge.Post, error) {
	pending := cont.Pending[0]
	cont.Pending = cont.Pending[1:]
	switch pending.Resource {
	case pageDiscussions:
		return f.discussions(ctx, loc, cont, pending.Next)
	case pageApprovals:
		return f.approvals(ctx, loc, cont)
	default:
		return nil, fmt.Errorf("unknown GitLab page %q in a continuation", pending.Resource)
	}
}

// discussion is one discussion: a comment of its own (individual_note) or
// a thread of comments, the first one starting it.
type discussion struct {
	ID             string           `json:"id"`
	IndividualNote bool             `json:"individual_note"`
	Notes          []jsontext.Value `json:"notes"`
}

type note struct {
	ID         int64     `json:"id"`
	Type       string    `json:"type"`
	Body       string    `json:"body"`
	Author     *user     `json:"author"`
	CreatedAt  time.Time `json:"created_at"`
	System     bool      `json:"system"`
	Resolvable bool      `json:"resolvable"`
	Resolved   bool      `json:"resolved"`
	Position   *struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
		OldLine *int   `json:"old_line"`
		NewLine *int   `json:"new_line"`
	} `json:"position"`
}

// discussions fetches one page of the thread's discussions and queues the
// next. System notes, GitLab's record of changes such as a new label, are
// not posts.
func (f *Forge) discussions(ctx context.Context, loc locator, cont *continuation, next string) ([]forge.Post, error) {
	path, query := next, url.Values(nil)
	if path == "" {
		path, query = loc.path()+"/discussions", url.Values{"per_page": {strconv.Itoa(perPage)}}
	}
	var page []discussion
	header, err := f.client.get(ctx, path, query, &page)
	if err != nil {
		return nil, forbiddenIsUnavailable(err)
	}
	if link := forge.NextLink(header.Get("Link")); link != "" {
		cont.Pending = append([]pendingPage{{Resource: pageDiscussions, Next: link}}, cont.Pending...)
	}
	type entry struct {
		discussion discussion
		note       note
		raw        jsontext.Value
	}
	var entries []entry
	var authors []*user
	for _, d := range page {
		for _, raw := range d.Notes {
			var n note
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, fmt.Errorf("decode a note of discussion %s: %w", d.ID, err)
			}
			if n.System || n.ID <= 0 {
				continue
			}
			entries = append(entries, entry{discussion: d, note: n, raw: raw})
			authors = append(authors, n.Author)
		}
	}
	if err := f.resolveAuthors(ctx, authors); err != nil {
		return nil, err
	}
	posts := make([]forge.Post, 0, len(entries))
	first := map[string]string{}
	for _, e := range entries {
		n, id := e.note, noteGID(e.note.ID)
		kind, extra := postNote, map[string]any{}
		if !e.discussion.IndividualNote {
			kind = postDiscussionNote
			extra["discussion"] = e.discussion.ID
			if start, ok := first[e.discussion.ID]; ok {
				extra["in_reply_to"] = start
			} else {
				first[e.discussion.ID] = id
			}
		}
		if n.Resolvable {
			extra["resolved"] = n.Resolved
		}
		body := n.Body
		if n.Type == "DiffNote" {
			kind = postDiffNote
			body = diffLocation(n, extra) + n.Body
		}
		posts = append(posts, f.post(*cont, id, kind, n.Author, "", body, cont.Head.URL+"#note_"+strconv.FormatInt(n.ID, 10),
			n.CreatedAt, extra, sorted(e.raw)))
	}
	return posts, nil
}

// noteGID is a note's global ID.
func noteGID(id int64) string { return "gid://gitlab/Note/" + strconv.FormatInt(id, 10) }

// diffLocation records where a diff note sits, and returns that as the
// first line of its body.
func diffLocation(n note, extra map[string]any) string {
	if n.Position == nil {
		return ""
	}
	path, line := n.Position.NewPath, n.Position.NewLine
	if path == "" {
		path = n.Position.OldPath
	}
	if line == nil {
		line = n.Position.OldLine
	}
	extra["path"] = path
	location := "`" + path + "`"
	if line != nil {
		extra["line"] = *line
		location += " line " + strconv.Itoa(*line)
	}
	return location + "\n\n"
}

// approvals fetches who approved a merge request. GitLab gives an approval
// no ID of its own; a user approves a merge request once at a time, so the
// merge request and the user name it. A merge request whose approvals the
// token may not read has none to archive.
func (f *Forge) approvals(ctx context.Context, loc locator, cont *continuation) ([]forge.Post, error) {
	var state struct {
		ApprovedBy []jsontext.Value `json:"approved_by"`
	}
	_, err := f.client.get(ctx, loc.path()+"/approvals", nil, &state)
	err = forbiddenIsUnavailable(err)
	if errors.Is(err, forge.ErrUnavailable) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type approval struct {
		User       *user      `json:"user"`
		ApprovedAt *time.Time `json:"approved_at"`
	}
	approvals := make([]approval, 0, len(state.ApprovedBy))
	var authors []*user
	for _, raw := range state.ApprovedBy {
		var a approval
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("decode an approval: %w", err)
		}
		approvals = append(approvals, a)
		authors = append(authors, a.User)
	}
	if err := f.resolveAuthors(ctx, authors); err != nil {
		return nil, err
	}
	posts := make([]forge.Post, 0, len(approvals))
	for i, a := range approvals {
		if a.User == nil || a.User.ID <= 0 {
			continue
		}
		at := cont.Opened
		if a.ApprovedAt != nil {
			at = *a.ApprovedAt
		}
		id := cont.Head.ConversationID + "/approval/" + strconv.FormatInt(a.User.ID, 10)
		posts = append(posts, f.post(*cont, id, postApproval, a.User, "", "Approved this merge request.", cont.Head.URL, at, nil,
			sorted(state.ApprovedBy[i])))
	}
	return posts, nil
}

func (f *Forge) post(cont continuation, id, kind string, author *user, title, body, link string, at time.Time,
	extra map[string]any, evidence []byte,
) forge.Post {
	metadata := map[string]any{"repository": cont.Repo, "number": cont.Number}
	maps.Copy(metadata, extra)
	// What GraphQL said of the author is outside the post's REST evidence.
	// The metadata keeps it, so a change to it rewrites the post and links
	// its sender anew.
	t := f.resolved(author)
	metadata["author"] = map[string]any{"type": t.userType, "bot": t.bot, "public_email": t.email}
	return forge.Post{
		ID: id, Kind: kind, Author: f.author(author), Title: title, Body: body,
		URL: link, CreatedAt: at, Metadata: metadata, Raw: evidence,
	}
}

// sorted is a post's JSON, limited to keep when supplied, in canonical form
// (RFC 8785: keys sorted at every level), so unchanged evidence is
// byte-identical across syncs.
func sorted(value jsontext.Value, keep ...string) []byte {
	var object map[string]jsontext.Value
	if len(keep) > 0 && json.Unmarshal(value, &object) == nil {
		selected := make(map[string]jsontext.Value, len(keep))
		for _, key := range keep {
			if field, ok := object[key]; ok {
				selected[key] = field
			}
		}
		if data, err := json.Marshal(selected); err == nil {
			value = data
		}
	}
	canonical := value.Clone()
	if err := canonical.Canonicalize(); err != nil {
		return value
	}
	return canonical
}

// authorType is what kind of account a user is, and the email it shows.
// GitLab's REST responses don't say what kind; its GraphQL API does. REST
// embeds an author's public email only from GitLab 18.0, while GraphQL has
// given it since 13.7.
type authorType struct {
	kind forge.AuthorKind
	// userType, bot and email are what GraphQL returned.
	userType string
	bot      bool
	email    string
}

const usersQuery = `query Users($ids: [ID!], $first: Int) {
  users(ids: $ids, first: $first) { nodes { id type bot publicEmail } }
}`

// resolveAuthors learns what kind of account each new author is, and its
// public email, once per sync. A user GitLab doesn't return is taken for an
// ordinary account.
func (f *Forge) resolveAuthors(ctx context.Context, users []*user) error {
	f.mu.Lock()
	var ids []string
	asked := map[int64]bool{}
	for _, u := range users {
		if u == nil || u.ID <= 0 || asked[u.ID] {
			continue
		}
		if _, known := f.authors[u.ID]; !known {
			asked[u.ID] = true
			ids = append(ids, "gid://gitlab/User/"+strconv.FormatInt(u.ID, 10))
		}
	}
	f.mu.Unlock()
	for start := 0; start < len(ids); start += perPage {
		batch := ids[start:min(start+perPage, len(ids))]
		var data struct {
			Users struct {
				Nodes []struct {
					ID          string `json:"id"`
					Type        string `json:"type"`
					Bot         bool   `json:"bot"`
					PublicEmail string `json:"publicEmail"`
				} `json:"nodes"`
			} `json:"users"`
		}
		if err := f.client.graphQL(ctx, graphQLRequest{
			OperationName: "Users", Query: usersQuery, Variables: map[string]any{"ids": batch, "first": len(batch)},
		}, &data); err != nil {
			return fmt.Errorf("look up GitLab authors: %w", err)
		}
		f.mu.Lock()
		for _, gid := range batch {
			id, _ := gidNumber(gid)
			f.authors[id] = authorType{kind: forge.AuthorAccount}
		}
		for _, node := range data.Users.Nodes {
			if id, ok := gidNumber(node.ID); ok {
				f.authors[id] = authorType{
					kind: userKind(node.Type), userType: node.Type, bot: node.Bot, email: strings.TrimSpace(node.PublicEmail),
				}
			}
		}
		f.mu.Unlock()
	}
	return nil
}

// userKind is how GitLab's user type attributes a post. The ghost user
// holds a deleted account's posts and the import user posts no author was
// found for; a placeholder stands for one author of an import until it is
// matched to an account.
func userKind(userType string) forge.AuthorKind {
	switch userType {
	case "GHOST", "IMPORT_USER":
		return forge.AuthorUnavailable
	case "PLACEHOLDER":
		return forge.AuthorMigrated
	default:
		return forge.AuthorAccount
	}
}

// resolved is what this sync's GraphQL lookup said of a user: zero for one
// it didn't return.
func (f *Forge) resolved(u *user) authorType {
	if u == nil || u.ID <= 0 {
		return authorType{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authors[u.ID]
}

// author is the account a post's author is. A human account's public
// email joins the person with that address in the archive.
func (f *Forge) author(u *user) forge.Account {
	if u == nil || u.ID <= 0 {
		return forge.Account{Kind: forge.AuthorUnavailable, Login: "ghost"}
	}
	t := f.resolved(u)
	switch t.kind {
	case forge.AuthorUnavailable:
		return forge.Account{Kind: forge.AuthorUnavailable, Login: u.Username, URL: u.WebURL}
	case forge.AuthorMigrated:
		return forge.Account{Kind: forge.AuthorMigrated, Login: cmp.Or(strings.TrimSpace(u.Name), u.Username)}
	default:
		email := ""
		if !t.bot {
			email = cmp.Or(t.email, strings.TrimSpace(u.PublicEmail))
		}
		return forge.Account{
			Kind: forge.AuthorAccount, ID: strconv.FormatInt(u.ID, 10), Login: u.Username, URL: u.WebURL, Bot: t.bot, Email: email,
		}
	}
}
