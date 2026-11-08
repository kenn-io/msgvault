// Package googlereviews manages Google review reads, archive backfills, and confirmed owner replies.
package googlereviews

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrCredentials  = errors.New("Google authorization is missing or expired; authorize the selected Google Reviews app")
	ErrPermission   = errors.New("Google denied access; check the owner's project eligibility and location permissions")
	ErrQuota        = errors.New("Google review quota reached; wait before reading again")
	ErrTemporary    = errors.New("Google reviews temporarily unavailable; retry a read later")
	ErrRejected     = errors.New("Google rejected the review request; check the configured target")
	ErrInvalid      = errors.New("invalid Google review request")
	ErrDenied       = errors.New("Google review access denied")
	ErrUnavailable  = errors.New("Google review provider unavailable; check own-project approval, credentials and permissions")
	ErrNotFound     = errors.New("Google review not found")
	ErrConflict     = errors.New("Google review changed or preview expired; create a new preview")
	ErrCapacity     = errors.New("Google review capacity reached; retry later")
	ErrUnknown      = errors.New("Google review reply outcome unknown; inspect live review before a new submission")
	ErrArchive      = errors.New("Google Reviews archive is unavailable")
	ErrArchiveBusy  = errors.New("Google Reviews backfill is already running")
	ErrArchiveCycle = errors.New("Google Reviews API repeated a page token")
	ErrSearch       = errors.New("invalid Google Reviews archive search")
)

var resourceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func validID(s string) bool { return resourceID.MatchString(s) }
func validLocation(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 4 && p[0] == "accounts" && validID(p[1]) && p[2] == "locations" && validID(p[3])
}
func validReviewName(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 6 && validLocation(strings.Join(p[:4], "/")) && p[4] == "reviews" && validID(p[5])
}

type Target struct {
	Name           string `toml:"name" json:"name"`
	Provider       string `toml:"provider" json:"provider"`
	Email          string `toml:"email" json:"-"`
	OAuthApp       string `toml:"oauth_app" json:"-"`
	Location       string `toml:"location" json:"-"`
	Enabled        bool   `toml:"enabled" json:"enabled"`
	RepliesEnabled bool   `toml:"replies_enabled" json:"replies_enabled"`
	OwnProject     bool   `toml:"own_project" json:"-"`
}

func (t Target) Validate() error {
	if !validID(t.Name) || t.Provider != "google" || t.OAuthApp == "" {
		return ErrInvalid
	}
	email, err := mail.ParseAddress(t.Email)
	if err != nil || email.Address != t.Email {
		return ErrInvalid
	}
	if t.Enabled && (!t.OwnProject || !validLocation(t.Location)) {
		return ErrInvalid
	}
	if t.Location != "" && !validLocation(t.Location) {
		return ErrInvalid
	}
	return nil
}

type Request struct {
	Action            string `json:"action"`
	Target            string `json:"target,omitempty"`
	ReviewID          string `json:"review_id,omitempty"`
	PageToken         string `json:"page_token,omitempty"`
	Comment           string `json:"comment,omitempty"`
	ConfirmationToken string `json:"confirmation_token,omitempty"`
	ReceiptID         string `json:"receipt_id,omitempty"`
	PageSize          int    `json:"page_size,omitempty"`
	Confirmed         bool   `json:"confirmed,omitempty"`
}

func (r Request) Validate() error {
	if !utf8.ValidString(r.PageToken) || len(r.PageToken) > 4096 {
		return ErrInvalid
	}
	// Compare the original request to its action's allowlisted fields. This also
	// rejects fields whose values are valid for another action.
	allowed := Request{Action: r.Action}
	switch r.Action {
	case "capabilities":
	case "invalidate_target":
		if !validID(r.Target) {
			return ErrInvalid
		}
		allowed.Target = r.Target
	case "resume_target":
		if !validID(r.Target) || !validID(r.ConfirmationToken) {
			return ErrInvalid
		}
		allowed.Target = r.Target
		allowed.ConfirmationToken = r.ConfirmationToken
	case "archive_status", "backfill":
		if !validID(r.Target) {
			return ErrInvalid
		}
		allowed.Target = r.Target
	case "list":
		if !validID(r.Target) || r.PageSize < 0 || r.PageSize > 50 {
			return ErrInvalid
		}
		allowed.Target = r.Target
		allowed.PageSize = r.PageSize
		allowed.PageToken = r.PageToken
	case "get", "assist_reply", "preview_reply":
		if !validID(r.Target) || !validID(r.ReviewID) {
			return ErrInvalid
		}
		allowed.Target = r.Target
		allowed.ReviewID = r.ReviewID
		if r.Action != "get" {
			if strings.TrimSpace(r.Comment) == "" || !utf8.ValidString(r.Comment) || len(r.Comment) > 4096 {
				return ErrInvalid
			}
			allowed.Comment = r.Comment
		}
	case "confirm_reply":
		if !validID(r.ConfirmationToken) || !r.Confirmed {
			return ErrInvalid
		}
		allowed.ConfirmationToken = r.ConfirmationToken
		allowed.Confirmed = true
	case "receipt":
		if !validID(r.ReceiptID) {
			return ErrInvalid
		}
		allowed.ReceiptID = r.ReceiptID
	default:
		return ErrInvalid
	}
	if r != allowed {
		return ErrInvalid
	}
	return nil
}

type Capability struct {
	Provider  string `json:"provider"`
	Available bool   `json:"available"`
	Read      bool   `json:"read"`
	Reply     bool   `json:"reply"`
	Reason    string `json:"reason,omitempty"`
}
type TargetInfo struct {
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Enabled        bool   `json:"enabled"`
	RepliesEnabled bool   `json:"replies_enabled"`
}
type Reviewer struct {
	DisplayName     string `json:"displayName,omitempty"`
	ProfilePhotoURL string `json:"profilePhotoUrl,omitempty"`
	IsAnonymous     bool   `json:"isAnonymous,omitempty"`
}
type Reply struct {
	Comment          string `json:"comment"`
	UpdateTime       string `json:"updateTime,omitempty"`
	ReviewReplyState string `json:"reviewReplyState,omitempty"`
	PolicyViolation  string `json:"policyViolation,omitempty"`
}
type Review struct {
	Name        string   `json:"name,omitempty"`
	ReviewID    string   `json:"reviewId,omitempty"`
	Reviewer    Reviewer `json:"reviewer"`
	StarRating  string   `json:"starRating,omitempty"`
	Comment     string   `json:"comment,omitempty"`
	CreateTime  string   `json:"createTime,omitempty"`
	UpdateTime  string   `json:"updateTime,omitempty"`
	ReviewReply *Reply   `json:"reviewReply,omitempty"`
}
type Page struct {
	Reviews          []Review `json:"reviews"`
	NextPageToken    string   `json:"nextPageToken,omitempty"`
	TotalReviewCount int64    `json:"totalReviewCount,omitempty"`
}

// BackfillState contains durable progress without returning Google's opaque
// paging token to API, CLI, or Web clients.
type BackfillState struct {
	Target              string     `json:"target"`
	Status              string     `json:"status"`
	RunID               int64      `json:"-"`
	PagesFetched        int64      `json:"pages_fetched"`
	ReviewsFetched      int64      `json:"reviews_fetched"`
	TotalReviewCount    int64      `json:"total_review_count"`
	ArchivedReviewCount int64      `json:"archived_review_count"`
	LastErrorCode       string     `json:"last_error_code,omitempty"`
	CoverageNote        string     `json:"coverage_note"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
	UpdatedAt           *time.Time `json:"updated_at,omitempty"`
	Location            string     `json:"-"`
	NextPageToken       string     `json:"-"`
}

type ArchivedReview struct {
	Target     string    `json:"target"`
	Review     Review    `json:"review"`
	ArchivedAt time.Time `json:"archived_at"`
	Location   string    `json:"-"`
}

type ArchiveSearchResponse struct {
	Query    string           `json:"query"`
	Target   string           `json:"target,omitempty"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Results  []ArchivedReview `json:"results"`
}

type ArchiveSearchRequest struct {
	Query    string `json:"query"`
	Target   string `json:"target,omitempty"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
}
type Preview struct {
	Review            Review    `json:"review"`
	Comment           string    `json:"comment"`
	ExpiresAt         time.Time `json:"expires_at,omitzero"`
	ConfirmationToken string    `json:"confirmation_token,omitempty"`
	ReceiptID         string    `json:"receipt_id,omitempty"`
}
type Receipt struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type Result struct {
	CredentialChangeToken string         `json:"credential_change_token,omitempty"`
	Capabilities          []Capability   `json:"capabilities,omitempty"`
	Targets               []TargetInfo   `json:"targets,omitempty"`
	Reviews               []Review       `json:"reviews,omitempty"`
	Review                *Review        `json:"review,omitempty"`
	NextPageToken         string         `json:"next_page_token,omitempty"`
	ArchiveSync           *BackfillState `json:"archive_sync,omitempty"`
	Preview               *Preview       `json:"preview,omitempty"`
	Receipt               *Receipt       `json:"receipt,omitempty"`
}

// Archive persists review pages and their resume cursor in the main archive.
// Implementations commit each page and cursor update in one transaction.
type Archive interface {
	BeginGoogleReviewsBackfill(ctx context.Context, target, location string) (BackfillState, error)
	ApplyGoogleReviewsPage(ctx context.Context, target, location, expectedPageToken string, page Page) (BackfillState, error)
	FailGoogleReviewsBackfill(ctx context.Context, target, location, errorCode string) (BackfillState, error)
	GoogleReviewsBackfillState(ctx context.Context, target, location string) (BackfillState, error)
	SearchGoogleReviews(ctx context.Context, query, target string, offset, limit int) (ArchiveSearchResponse, error)
}

// ValidateFor ensures a page only contains provider resource names belonging
// to the requested configured target and location before persistence.
func (p Page) ValidateFor(target, location string) error {
	if !validID(target) || !validLocation(location) || len(p.Reviews) > 50 ||
		!utf8.ValidString(p.NextPageToken) || len(p.NextPageToken) > 4096 || p.TotalReviewCount < 0 {
		return ErrInvalid
	}
	const maxPageBytes = 2 << 20
	pageBytes := 0
	addPageString := func(value string) bool {
		if !utf8.ValidString(value) || len(value) > maxPageBytes-pageBytes {
			return false
		}
		pageBytes += len(value)
		return true
	}
	if !addPageString(p.NextPageToken) {
		return ErrInvalid
	}
	for _, review := range p.Reviews {
		if !validID(review.ReviewID) || review.Name != "" && review.Name != location+"/reviews/"+review.ReviewID ||
			!addPageString(review.Name) || !addPageString(review.ReviewID) || !addPageString(review.Comment) ||
			!addPageString(review.Reviewer.DisplayName) || !addPageString(review.Reviewer.ProfilePhotoURL) ||
			!addPageString(review.StarRating) {
			return ErrInvalid
		}
		for _, value := range []string{review.CreateTime, review.UpdateTime} {
			if !addPageString(value) {
				return ErrInvalid
			}
			if value != "" {
				if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
					return ErrInvalid
				}
			}
		}
		if review.ReviewReply != nil {
			for _, value := range []string{review.ReviewReply.Comment, review.ReviewReply.UpdateTime, review.ReviewReply.ReviewReplyState, review.ReviewReply.PolicyViolation} {
				if !addPageString(value) {
					return ErrInvalid
				}
			}
			if review.ReviewReply.UpdateTime != "" {
				if _, err := time.Parse(time.RFC3339Nano, review.ReviewReply.UpdateTime); err != nil {
					return ErrInvalid
				}
			}
		}
	}
	return nil
}

type Provider interface {
	List(ctx context.Context, parent, pageToken string, pageSize int) (Page, error)
	Get(ctx context.Context, name string) (Review, error)
	UpdateReply(ctx context.Context, name, comment string) (Reply, error)
}
