// Package threads implements the bounded Threads API surface used by the archive.
package threads

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

type API interface {
	Me(context.Context) (Owner, error)
	OwnPosts(context.Context, ListQuery) (Page, error)
	OwnReplies(context.Context, ListQuery) (Page, error)
	Conversation(context.Context, string, string) (Page, error)
	Media(context.Context, string) (Media, error)
}

type Owner struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

type ListQuery struct {
	Since, Until time.Time
	After        string
}

type Page struct {
	Items []Media
	After string
}

type MediaReference struct {
	ID string `json:"id"`
}

type MediaChild struct {
	ID        string  `json:"id"`
	MediaType *string `json:"media_type"`
	MediaURL  *string `json:"media_url"`
}

type MediaChildren struct {
	Data []MediaChild `json:"data"`
}

// Optional pointers distinguish explicit empty/false values from omissions.
// Present additionally retains explicit JSON null; Raw is the original snapshot.
type Media struct {
	ID                                                        string
	Timestamp                                                 time.Time
	Text, Username, Permalink, MediaType, MediaURL, Shortcode *string
	IsQuotePost, HasReplies, IsReply, IsReplyOwnedByMe        *bool
	Owner, QuotedPost, RepostedPost, RootPost, RepliedTo      *MediaReference
	Children                                                  *MediaChildren
	HideStatus                                                *string
	Present                                                   map[string]bool
	Raw                                                       []byte
}

var (
	ErrDecodeResponse = errors.New("threads API response could not be decoded")
	ErrRedirect       = errors.New("threads API redirects are not allowed")
	ErrPagination     = errors.New("threads pagination did not advance")
)

type APIError struct {
	StatusCode, Code int
	Subcode          int
	CursorExpired    bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("threads API: HTTP %d (Graph code %d)", e.StatusCode, e.Code)
}
func (e *APIError) CredentialInvalid() bool { return e.Code == 190 }

// PauseError asks the caller to checkpoint and release lifecycle ownership.
// A successful read can return both a usable page and a pause.
type PauseError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *PauseError) Error() string { return "threads run paused: " + e.Reason }

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return strings.Trim(id, "0") != ""
}

// UnmarshalJSON retains exact decimal IDs and the presence of optional fields.
// Decode errors are deliberately bounded: JSON diagnostics can contain bodies.
func (m *Media) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID               string          `json:"id"`
		Timestamp        string          `json:"timestamp"`
		Text             *string         `json:"text"`
		Username         *string         `json:"username"`
		Permalink        *string         `json:"permalink"`
		MediaType        *string         `json:"media_type"`
		MediaURL         *string         `json:"media_url"`
		Shortcode        *string         `json:"shortcode"`
		IsQuotePost      *bool           `json:"is_quote_post"`
		HasReplies       *bool           `json:"has_replies"`
		IsReply          *bool           `json:"is_reply"`
		IsReplyOwnedByMe *bool           `json:"is_reply_owned_by_me"`
		Owner            *MediaReference `json:"owner"`
		QuotedPost       *MediaReference `json:"quoted_post"`
		RepostedPost     *MediaReference `json:"reposted_post"`
		RootPost         *MediaReference `json:"root_post"`
		RepliedTo        *MediaReference `json:"replied_to"`
		Children         *MediaChildren  `json:"children"`
		HideStatus       *string         `json:"hide_status"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || !validID(wire.ID) {
		return ErrDecodeResponse
	}
	ts, err := time.Parse(time.RFC3339Nano, wire.Timestamp)
	if err != nil {
		ts, err = time.Parse("2006-01-02T15:04:05-0700", wire.Timestamp)
	}
	if err != nil || ts.IsZero() {
		return ErrDecodeResponse
	}
	for _, ref := range []*MediaReference{wire.Owner, wire.QuotedPost, wire.RepostedPost, wire.RootPost, wire.RepliedTo} {
		if ref != nil && !validID(ref.ID) {
			return ErrDecodeResponse
		}
	}
	if wire.Children != nil {
		for _, child := range wire.Children.Data {
			if !validID(child.ID) {
				return ErrDecodeResponse
			}
		}
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil {
		return ErrDecodeResponse
	}
	present := make(map[string]bool, len(fields))
	for key := range fields {
		present[key] = true
	}
	*m = Media{ID: wire.ID, Timestamp: ts, Text: wire.Text, Username: wire.Username, Permalink: wire.Permalink, MediaType: wire.MediaType, MediaURL: wire.MediaURL, Shortcode: wire.Shortcode, IsQuotePost: wire.IsQuotePost, HasReplies: wire.HasReplies, IsReply: wire.IsReply, IsReplyOwnedByMe: wire.IsReplyOwnedByMe, Owner: wire.Owner, QuotedPost: wire.QuotedPost, RepostedPost: wire.RepostedPost, RootPost: wire.RootPost, RepliedTo: wire.RepliedTo, Children: wire.Children, HideStatus: wire.HideStatus, Present: present, Raw: bytes.Clone(data)}
	return nil
}

// PublishAPI is the Threads-specific reply surface. Publishing has no retry.
type PublishAPI interface {
	API
	ReplyQuota(context.Context) (ReplyQuota, error)
	CreateReply(context.Context, string, string) (string, error)
	ContainerStatus(context.Context, string) (string, error)
	PublishReply(context.Context, string, string) (string, error)
}

type ReplyQuota struct {
	Usage    int64 `json:"usage"`
	Total    int64 `json:"total"`
	Duration int64 `json:"duration_seconds"`
}
