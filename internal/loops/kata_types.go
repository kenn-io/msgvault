package loops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/textutil"
)

type TrackRequest struct {
	IssueRef string `json:"issue_ref,omitempty"`
	Title    string `json:"title,omitempty"`
	List     string `json:"list,omitempty"`
}

// KataIssue is the shared receipt returned by loop tracking and Kata writes.
// Its title is external, untrusted text.
type KataIssue struct {
	UID          string `json:"uid"`
	Ref          string `json:"ref"`
	QualifiedRef string `json:"qualified_ref"`
	Project      string `json:"project"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Revision     string `json:"revision"`
	WebURL       string `json:"web_url,omitempty"`
}

// LoopKata is the persisted issue cache. Live is true only when this response
// refreshed the currently installed link from Kata.
type LoopKata struct {
	IssueUID         string     `json:"issue_uid"`
	QualifiedRef     string     `json:"qualified_ref"`
	Title            string     `json:"title"`
	Status           string     `json:"status"`
	WebURL           string     `json:"web_url,omitempty"`
	LinkedVia        string     `json:"linked_via"`
	LinkedAt         time.Time  `json:"linked_at"`
	CheckedAt        *time.Time `json:"checked_at"`
	Live             bool       `json:"live"`
	Attempts         int        `json:"attempts"`
	NextAttemptAt    *time.Time `json:"next_attempt_at"`
	LastError        string     `json:"last_error,omitempty"`
	ClosedNotifiedAt *time.Time `json:"closed_notified_at"`
	ClosedLoopAt     *time.Time `json:"closed_loop_at"`
}

// DefaultTrackTitle is also used by write confirmations so the disclosed
// title matches the issue that tracking files.
func DefaultTrackTitle(loop Loop) string {
	name := strings.TrimSpace(loop.Counterparty.DisplayName)
	if name == "" {
		name = fmt.Sprintf("participant %d", loop.Counterparty.ParticipantID)
	}
	title := loop.Summary
	switch loop.Kind {
	case "reply_owed":
		title = "Reply to " + name + ": " + title
	case "awaiting_reply":
		title = "Waiting on " + name + ": " + title
	}
	return textutil.TruncateRunes(strings.ToValidUTF8(title, ""), 512)
}

// KataIssues is the archive evidence and issue boundary shared with other
// explicit loop trackers. It never grants issue status writes.
type KataIssues interface {
	Prepare(ctx context.Context, selectors []kataevidence.Selector) ([]kataevidence.Evidence, error)
	Create(ctx context.Context, key string, input kataissues.CreateInput) (kataissues.Result, error)
	Link(ctx context.Context, ref string, evidence []kataevidence.Reference) (taskclient.KataTask, error)
	Get(ctx context.Context, ref string) (taskclient.KataTask, error)
	Comment(ctx context.Context, issueUID, idempotencyKey, body string) error
}

// KataConnector resolves the configured project once per bridge pass.
type KataConnector interface {
	Connect(ctx context.Context) (KataIssues, error)
}

type EvidencePolicy struct{ FallbackToWindow bool }

var (
	ErrKataUnavailable    = errors.New("kata is unavailable")
	ErrLoopAlreadyTracked = errors.New("open loop is already tracked")
)

type AlreadyTrackedError struct{ Kata LoopKata }

func (e *AlreadyTrackedError) Error() string { return ErrLoopAlreadyTracked.Error() }
func (e *AlreadyTrackedError) Unwrap() error { return ErrLoopAlreadyTracked }
