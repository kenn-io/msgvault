// Package meta archives comments returned by Meta's official Graph API.
package meta

import "context"

const SourceType = "facebook_comments"

// PostEdge limits discovery to the supported Page surfaces. It is never an
// arbitrary path or a URL supplied by configuration or provider pagination.
type PostEdge string

const (
	Posts        PostEdge = "posts"
	VisitorPosts PostEdge = "visitor_posts"
	AdsPosts     PostEdge = "ads_posts"
)

var postEdges = [...]PostEdge{Posts, VisitorPosts, AdsPosts}

func (e PostEdge) valid() bool { return e == Posts || e == VisitorPosts || e == AdsPosts }

type Page struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

// Object preserves native IDs verbatim, including composite post/comment IDs.
// Parent and From are optional provider evidence, never inferred from a name.
type Object struct {
	ID               string     `json:"id"`
	Message          string     `json:"message"`
	CreatedTime      string     `json:"created_time"`
	From             *Actor     `json:"from,omitempty"`
	Parent           *Actor     `json:"parent,omitempty"`
	InstagramMedia   *IGMedia   `json:"instagram_media,omitempty"`
	InstagramComment *IGComment `json:"instagram_comment,omitempty"`
}

type Actor struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type ObjectPage struct {
	Data  []Object
	After string
}

// IGUser identifies the linked account or a media owner, not a comment author.
type IGUser struct {
	ID       string `json:"id"`
	Username string `json:"username,omitempty"`
}

// IGCommentFromUser preserves the distinct author identifier family. Neither
// ID nor SelfIGScopedID is established as comparable with IGUser.ID.
type IGCommentFromUser struct {
	ID             string `json:"id"`
	Username       string `json:"username,omitempty"`
	SelfIGScopedID string `json:"self_ig_scoped_id,omitempty"`
}

type IGMedia struct {
	ID        string  `json:"id"`
	Caption   string  `json:"caption"`
	Timestamp string  `json:"timestamp"`
	Permalink string  `json:"permalink"`
	Owner     *IGUser `json:"owner,omitempty"`
}

type IGComment struct {
	ID        string             `json:"id"`
	Text      string             `json:"text"`
	Timestamp string             `json:"timestamp"`
	From      *IGCommentFromUser `json:"from,omitempty"`
	ParentID  string             `json:"parent_id,omitempty"`
}

type IGMediaPage struct {
	Data  []IGMedia
	After string
}

type IGCommentPage struct {
	Data  []IGComment
	After string
}

// API reads one page per call. Import's budget covers every call, including
// identity validation. Implementations must never follow arbitrary paging URLs.
type API interface {
	VerifyPage(ctx context.Context, pageID string) (Page, error)
	Posts(ctx context.Context, pageID string, edge PostEdge, after string) (ObjectPage, error)
	Comments(ctx context.Context, parentID string, after string) (ObjectPage, error)
}
