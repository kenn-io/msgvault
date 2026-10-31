// Package x implements opt-in X API access for public posts and replies.
package x

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"encoding/json/jsontext"
)

// MaxIDDigits bounds local decoding without assuming machine-sized native IDs.
const MaxIDDigits = 128

// ValidateID accepts canonical, positive ASCII decimal native IDs.
func ValidateID(value string) error {
	if len(value) == 0 || len(value) > MaxIDDigits || value[0] == '0' {
		return errors.New("invalid X native ID")
	}
	for _, b := range []byte(value) {
		if b < '0' || b > '9' {
			return errors.New("invalid X native ID")
		}
	}
	return nil
}

// CompareID compares validated IDs numerically without integer conversion.
func CompareID(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

type Reference struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}
type Mention struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}
type User struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Protected bool   `json:"protected"`
	// ProtectedKnown distinguishes an explicit provider value from omission/null.
	ProtectedKnown bool `json:"-"`
}

// UnmarshalJSON retains the provider's protection-presence evidence. A missing
// or null field permits numeric identity/text ingestion but proves no public
// availability; callers must consult ProtectedKnown as well as Protected.
func (u *User) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Username  string `json:"username"`
		Protected *bool  `json:"protected"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*u = User{ID: wire.ID, Name: wire.Name, Username: wire.Username, ProtectedKnown: wire.Protected != nil}
	if wire.Protected != nil {
		u.Protected = *wire.Protected
	}
	return nil
}

type Post struct {
	ID             string
	AuthorID       string
	ConversationID string
	ReplyUserID    string
	Text           string
	LongText       string
	TextIncomplete bool
	CreatedAt      time.Time
	References     []Reference
	EditHistory    []string
	Mentions       []Mention
	Withheld       bool
	Raw            jsontext.Value
}

type ItemError struct {
	ID          string
	Code        string
	Unavailable bool
}
type Page struct {
	// RequestedIDs distinguishes a budget-reduced lookup from unavailable IDs.
	RequestedIDs     []string `json:"-"`
	RequestedResults int      `json:"-"`
	Posts            []Post
	Users            []User
	NextToken        string
	Errors           []ItemError
	ReservationID    string
	FetchedAt        time.Time
	PostCount        int
	UserCount        int
}
type TimelineRequest struct {
	OwnerID    string
	Mentions   bool
	SinceID    string
	After      time.Time
	Before     time.Time
	Token      string
	MaxResults int
}
type SearchRequest struct {
	Query      string
	After      time.Time
	Before     time.Time
	Token      string
	MaxResults int
}

type API interface {
	Me(context.Context) (User, error)
	Timeline(context.Context, TimelineRequest) (Page, error)
	SearchAll(context.Context, SearchRequest) (Page, error)
	Lookup(context.Context, []string) (Page, error)
	CreateReply(ctx context.Context, text, parentID string) (string, error)
}
