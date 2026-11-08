// Package forge archives the threads an account takes part in on a code
// forge: issues, pull or merge requests with their reviews, and, where the
// forge has them, discussions and releases. Each forge account is one
// source. A forge package finds changed threads and fetches their posts;
// this package runs the sync around it: the run, its resume state and
// queue, the owner filter, rate limits, and writing every post through the
// provider-neutral message import, one conversation per thread.
package forge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
	"time"
)

// Forge is what one forge provides to a sync. Every method uses the token
// of the account being synced.
type Forge interface {
	// Name is the forge's source type, message type and participant
	// identifier type, such as "github".
	Name() string
	// Instance is the forge server the account lives on.
	Instance() Instance
	// Account returns the token's account and checks the token's scopes.
	Account(ctx context.Context) (Account, error)
	// Discover reports the threads that changed since cursor, one page at a
	// time. cursor is the forge's own state from the last page it reported,
	// or nil at first. known reads the threads this source has archived, for
	// forges that must check them again. Discover drops threads owners does
	// not allow before fetching anything more about them. After each page
	// the core queues the threads and checkpoints next; a non-nil error from
	// page ends Discover with that error.
	Discover(ctx context.Context, cursor []byte, owners OwnerFilter, known KnownThreads,
		page func(threads []ThreadRef, next []byte) error) error
	// Owner returns the current owner of a thread's repository or project,
	// without fetching the thread's content. The core calls it before every
	// Fetch, because a repository can move to an excluded owner after it
	// was discovered.
	Owner(ctx context.Context, ref ThreadRef) (string, error)
	// Fetch streams a thread's posts, oldest first, starting from
	// continuation (nil at first). After each page the core writes the posts
	// and keeps next with the queued thread; next is nil after the last
	// page. A non-nil error from page ends Fetch with that error.
	Fetch(ctx context.Context, ref ThreadRef, continuation []byte,
		page func(head ThreadHead, posts []Post, next []byte) error) error
}

// Kind is the kind of a thread.
type Kind string

const (
	KindIssue        Kind = "issue"
	KindPullRequest  Kind = "pull_request"
	KindMergeRequest Kind = "merge_request"
	KindDiscussion   Kind = "discussion"
	KindRelease      Kind = "release"
)

// ThreadRef names one thread a sync found changed and has yet to archive.
type ThreadRef struct {
	Kind Kind `json:"kind"`
	// Key is the thread's stable ID on its instance; one key is one thread.
	Key string `json:"key"`
	// Owner is the account or group that owns the thread's repository or
	// project, as the owner filter compares it.
	Owner string `json:"owner"`
	// Repo names the repository or project, such as "octo/widgets".
	Repo string `json:"repo"`
	// Number is the thread's number in its repository; 0 for releases.
	Number int `json:"number,omitzero"`
	// Locator is the forge's own way to fetch the thread, such as an API URL.
	Locator   string    `json:"locator,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// Reason is why the forge notified the account, when it says.
	Reason string `json:"reason,omitempty"`
}

// Name is how errors and logs name the thread, as its forge does: a merge
// request is !N, anything else #N.
func (r ThreadRef) Name() string {
	if r.Number > 0 {
		sign := "#"
		if r.Kind == KindMergeRequest {
			sign = "!"
		}
		return fmt.Sprintf("%s%s%d", r.Repo, sign, r.Number)
	}
	if r.Locator != "" {
		return r.Locator
	}
	return r.Repo + " " + r.Key
}

// ThreadHead is what a thread's conversation is called.
type ThreadHead struct {
	// ConversationID is the conversation's key within the source.
	ConversationID string
	// Title names the conversation, as the forge's notification emails do,
	// such as "[octo/widgets] Add widgets (PR #7)".
	Title string
	URL   string
}

// Post is one message of a thread.
type Post struct {
	// ID is the post's stable ID on its instance.
	ID string
	// Kind names the kind of post in the message's metadata, such as
	// "issue_comment" or "review".
	Kind   string
	Author Account
	// Title is the subject of a thread's opening post; empty for replies.
	Title string
	// Body is Markdown.
	Body      string
	URL       string
	CreatedAt time.Time
	// Metadata holds the post's forge-specific facts, such as a review's
	// state or a review comment's path and line.
	Metadata map[string]any
	// Raw is the forge's own JSON for the post: the archived evidence.
	Raw []byte
}

// AuthorKind is how a post's author is known.
type AuthorKind int

const (
	// AuthorAccount is an account on the instance, with a stable ID.
	AuthorAccount AuthorKind = iota
	// AuthorUnavailable is an account that no longer exists, such as
	// GitHub's ghost.
	AuthorUnavailable
	// AuthorMigrated is a name a migration carried over from another
	// forge; it is no account on this instance.
	AuthorMigrated
)

// Account is a post's author or the account a token belongs to.
type Account struct {
	Kind AuthorKind
	// ID is the account's stable ID on its instance; set for AuthorAccount only.
	ID string
	// Login is the account's username, or a migrated author's name.
	Login string
	URL   string
	// Email is an address the forge shows as the account's own public
	// email, never a placeholder; empty when unknown.
	Email string
	Bot   bool
}

// KnownThreads reads the threads a source has archived.
type KnownThreads interface {
	// Oldest returns up to limit archived threads, those checked least
	// recently first.
	Oldest(ctx context.Context, limit int) ([]KnownThread, error)
	// Each calls fn for every archived thread.
	Each(ctx context.Context, fn func(KnownThread) error) error
}

// KnownThread is an archived thread and when a sync last fetched it:
// ReadAt is when that fetch read the thread's first page, so a change made
// since may be missing from the archive, and CheckedAt is when it finished.
type KnownThread struct {
	Ref       ThreadRef
	ReadAt    time.Time
	CheckedAt time.Time
}

// Instance names a forge server: its host, a port other than the scheme's
// default, and a path, without scheme or trailing slash, such as
// "github.com" or "git.example.com:8443/gitea". IDs are unique only within
// an instance, so the instance is part of every key that leaves a source.
type Instance string

// ParseInstance canonicalizes a forge server's web or API base URL. It
// returns the instance and the URL with its scheme, for API calls.
func ParseInstance(raw string) (Instance, *url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, fmt.Errorf("forge URL must be an http or https URL without credentials, query or fragment: %q", raw)
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	switch {
	case port != "":
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		// An IPv6 address keeps its brackets, as in a URL.
		host = "[" + host + "]"
	}
	// One canonical path, with dot segments resolved, repeated slashes
	// collapsed and no trailing slash, gives both the instance and the base
	// URL, so parsing the base URL again gives the same instance.
	escaped := ""
	if strings.Trim(parsed.EscapedPath(), "/") != "" {
		escaped = strings.TrimRight(path.Clean("/"+parsed.EscapedPath()), "/")
	}
	base := &url.URL{Scheme: parsed.Scheme, Host: host}
	if escaped != "" {
		unescaped, err := url.PathUnescape(escaped)
		if err != nil {
			return "", nil, fmt.Errorf("forge URL has an invalid path: %q", raw)
		}
		base.Path, base.RawPath = unescaped, escaped
	}
	return Instance(host + escaped), base, nil
}

// AccountKey is an account's key outside its source: its participant
// identifier and the source's account identity.
func AccountKey(instance Instance, accountID string) string {
	return string(instance) + ":user:" + accountID
}

// SourceIdentifier is the identifier of the source for an account.
func SourceIdentifier(instance Instance, accountID string) string {
	return AccountKey(instance, accountID)
}

var (
	// ErrUnauthorized means the forge rejected the token.
	ErrUnauthorized = errors.New("the forge rejected the token")
	// ErrUnavailable means a thread is gone or the token may not read it;
	// the sync drops it.
	ErrUnavailable = errors.New("not available to this token")
)

// RateLimitError means a rate limit stops the run until Reset. The sync
// keeps its queue and the next sync continues.
type RateLimitError struct {
	Resource string
	Reset    time.Time
}

func (e *RateLimitError) Error() string {
	if e.Reset.IsZero() {
		return "forge " + e.Resource + " rate limit reached"
	}
	return fmt.Sprintf("forge %s rate limit reached; it resets at %s",
		e.Resource, e.Reset.UTC().Format(time.RFC3339))
}

// IsRateLimit reports whether err is a RateLimitError.
func IsRateLimit(err error) bool {
	_, ok := errors.AsType[*RateLimitError](err)
	return ok
}
