package importantdate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

type DeliveryStore interface {
	ClaimImportantDateDeliveryContext(ctx context.Context, key, fingerprint string) (bool, error)
	ClaimImportantDateNotificationContext(ctx context.Context, key, fingerprint, issue string) (bool, error)
	ImportantDateDeliveryContext(ctx context.Context, key string) (store.ImportantDateDeliveryState, string, error)
	AdvanceImportantDateDeliveryContext(ctx context.Context, key string, from, to store.ImportantDateDeliveryState, issue string) (bool, error)
	WithImportantDateConsentContext(ctx context.Context, subject, owner, project, recipient string, dispatch func(context.Context) error) (bool, error)
}

type DeliveryKata interface {
	ProjectUID(ctx context.Context, project string) (string, error)
	FindActionTask(ctx context.Context, project, marker string) (taskclient.KataTask, bool, error)
	GetTask(ctx context.Context, project, uid string) (taskclient.KataTask, error)
	CreateTaskReused(ctx context.Context, project, key string, create taskclient.KataCreate) (taskclient.KataTask, bool, error)
	NotifyIssue(ctx context.Context, project, uid, recipient, message string) error
}

type DeliveryService struct {
	Store DeliveryStore
	Kata  DeliveryKata
}
type DeliveryInput struct{ OccurrenceKey, Fingerprint, SubjectUID, OwnerUID, Project, ConsentScope, Recipient string }

var errDeliveryIneligible = errors.New("important-date occurrence is no longer eligible")
var errIssueClosed = errors.New("important-date issue is no longer open")

// Deliver submits at most one attempt per occurrence. A caller must resolve
// aliases and merge collisions before calling and revalidate the exact event
// and private destination through the supplied fenced callback on each write.
func (s DeliveryService) Deliver(ctx context.Context, input DeliveryInput, revalidate func(context.Context) (bool, error)) error {
	if s.Store == nil || s.Kata == nil || revalidate == nil || strings.TrimSpace(input.OccurrenceKey) == "" || strings.TrimSpace(input.Fingerprint) == "" || strings.TrimSpace(input.SubjectUID) == "" || !strings.HasPrefix(input.OwnerUID, "person/") || strings.TrimPrefix(input.OwnerUID, "person/") == "" || strings.TrimSpace(input.Project) == "" || strings.TrimSpace(input.ConsentScope) == "" || strings.TrimSpace(input.Recipient) == "" {
		return errors.New("invalid important-date delivery request")
	}
	claimed, err := s.Store.ClaimImportantDateDeliveryContext(ctx, input.OccurrenceKey, input.Fingerprint)
	if err != nil {
		return err
	}
	if !claimed {
		state, issueUID, err := s.Store.ImportantDateDeliveryContext(ctx, input.OccurrenceKey)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if state == store.ImportantDateIssueCreated {
			return s.notify(ctx, input, issueUID, revalidate)
		}
		return nil
	}
	if err := s.recoverAdvance(ctx, input.OccurrenceKey, store.ImportantDateClaimed, store.ImportantDateIssueInFlight, ""); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte("msgvault.important-date.v1\x00" + input.OccurrenceKey))
	marker := hex.EncodeToString(digest[:])
	var issue taskclient.KataTask
	var reused bool
	attempted, confirmed := false, false
	active, err := s.Store.WithImportantDateConsentContext(ctx, input.SubjectUID, input.OwnerUID, input.ConsentScope, input.Recipient, func(fenced context.Context) error {
		state, _, err := s.Store.ImportantDateDeliveryContext(fenced, input.OccurrenceKey)
		if err != nil {
			return err
		}
		if state != store.ImportantDateIssueInFlight {
			return errDeliveryIneligible
		}
		valid, err := revalidate(fenced)
		if err != nil {
			return err
		}
		if !valid {
			return errDeliveryIneligible
		}
		issue, reused, err = s.Kata.FindActionTask(fenced, input.Project, marker)
		if err != nil {
			return err
		}
		if !reused {
			attempted = true
			personUID := strings.TrimPrefix(input.OwnerUID, "person/")
			if subjectUID, ok := strings.CutPrefix(input.SubjectUID, "person/"); ok {
				personUID = subjectUID
			}
			issue, reused, err = s.Kata.CreateTaskReused(fenced, input.Project, marker, taskclient.KataCreate{Title: "Prepare for an upcoming date", Metadata: map[string]any{taskclient.ActionMetadataKey: marker, personagenda.PersonMetadataKey: personUID, "msgvault.important_date.subject": input.SubjectUID}})
			confirmed = err == nil
		}
		return err
	})
	if err != nil || !active {
		outcome := store.ImportantDateRetryIssue
		issueUID := ""
		if attempted {
			outcome = store.ImportantDateUnknown
			if confirmed && issue.UID != "" && issue.Project == input.Project {
				issueUID = issue.UID
				outcome = store.ImportantDateIssueCreated
				if reused || issue.Status != "open" {
					outcome = store.ImportantDateCancelled
				}
			}
		}
		recoveryErr := s.recoverAdvance(ctx, input.OccurrenceKey, store.ImportantDateIssueInFlight, outcome, issueUID)
		if errors.Is(err, errDeliveryIneligible) {
			return nil
		}
		return errors.Join(err, recoveryErr)
	}
	if issue.UID == "" || issue.Project != input.Project {
		recoveryErr := s.recoverAdvance(ctx, input.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateUnknown, "")
		return errors.Join(errors.New("important-date issue could not be reconciled"), recoveryErr)
	}
	if reused || issue.Status != "open" {
		return s.recoverAdvance(ctx, input.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateCancelled, issue.UID)
	}
	if err := s.recoverAdvance(ctx, input.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateIssueCreated, issue.UID); err != nil {
		return err
	}
	return s.notify(ctx, input, issue.UID, revalidate)
}

// notify claims a known, not-yet-attempted notification. Failed preflight can
// return to issue_created; once NotifyIssue is called the result is uncertain.
func (s DeliveryService) notify(ctx context.Context, input DeliveryInput, issueUID string, revalidate func(context.Context) (bool, error)) error {
	changed, err := s.Store.ClaimImportantDateNotificationContext(ctx, input.OccurrenceKey, input.Fingerprint, issueUID)
	if err != nil || !changed {
		return err
	}
	attempted, confirmed := false, false
	active, err := s.Store.WithImportantDateConsentContext(ctx, input.SubjectUID, input.OwnerUID, input.ConsentScope, input.Recipient, func(fenced context.Context) error {
		state, currentUID, err := s.Store.ImportantDateDeliveryContext(fenced, input.OccurrenceKey)
		if err != nil {
			return err
		}
		if state != store.ImportantDateNotifyInFlight || currentUID != issueUID {
			return errDeliveryIneligible
		}
		valid, err := revalidate(fenced)
		if err != nil {
			return err
		}
		if !valid {
			return errDeliveryIneligible
		}
		issue, err := s.Kata.GetTask(fenced, input.Project, issueUID)
		if errors.Is(err, taskclient.ErrNotFound) {
			return errIssueClosed
		}
		if err != nil {
			return err
		}
		if issue.Status != "open" {
			return errIssueClosed
		}
		attempted = true
		err = s.Kata.NotifyIssue(fenced, input.Project, issueUID, input.Recipient, "Review the upcoming date in msgvault")
		confirmed = err == nil
		return err
	})
	if err != nil || !active {
		outcome := store.ImportantDateIssueCreated
		if attempted {
			outcome = store.ImportantDateUnknown
			if confirmed {
				outcome = store.ImportantDateDelivered
			}
		} else if errors.Is(err, errIssueClosed) {
			outcome = store.ImportantDateCancelled
		}
		recoveryErr := s.recoverAdvance(ctx, input.OccurrenceKey, store.ImportantDateNotifyInFlight, outcome, issueUID)
		if errors.Is(err, errDeliveryIneligible) {
			return nil
		}
		if errors.Is(err, errIssueClosed) {
			return recoveryErr
		}
		return errors.Join(err, recoveryErr)
	}
	return s.recoverAdvance(ctx, input.OccurrenceKey, store.ImportantDateNotifyInFlight, store.ImportantDateDelivered, issueUID)
}

func (s DeliveryService) advance(ctx context.Context, key string, from, to store.ImportantDateDeliveryState, issue string) error {
	changed, err := s.Store.AdvanceImportantDateDeliveryContext(ctx, key, from, to, issue)
	if err != nil {
		return err
	}
	if !changed {
		return errors.New("important-date delivery state changed concurrently")
	}
	return nil
}

// recoverAdvance records a delivery outcome even if the caller is cancelled.
func (s DeliveryService) recoverAdvance(ctx context.Context, key string, from, to store.ImportantDateDeliveryState, issue string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.advance(cleanupCtx, key, from, to, issue)
}
