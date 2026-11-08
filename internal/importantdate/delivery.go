package importantdate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

type DeliveryStore interface {
	ClaimImportantDateDeliveryContext(context.Context, string, string) (bool, error)
	ClaimImportantDateNotificationContext(context.Context, string, string, string) (bool, error)
	ImportantDateDeliveryContext(context.Context, string) (store.ImportantDateDeliveryState, string, error)
	AdvanceImportantDateDeliveryContext(context.Context, string, store.ImportantDateDeliveryState, store.ImportantDateDeliveryState, string) (bool, error)
	WithImportantDateConsentContext(context.Context, string, string, string, string, func(context.Context) error) (bool, error)
}

type DeliveryKata interface {
	ProjectUID(context.Context, string) (string, error)
	FindActionTask(context.Context, string, string) (taskclient.KataTask, bool, error)
	GetTask(context.Context, string, string) (taskclient.KataTask, error)
	CreateTaskReused(context.Context, string, string, taskclient.KataCreate) (taskclient.KataTask, bool, error)
	NotifyIssue(context.Context, string, string, string, string) error
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
	if err := s.advance(ctx, input.OccurrenceKey, store.ImportantDateClaimed, store.ImportantDateIssueInFlight, ""); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte("msgvault.important-date.v1\x00" + input.OccurrenceKey))
	marker := hex.EncodeToString(digest[:])
	var issue taskclient.KataTask
	var reused bool
	attempted := false
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
			if strings.HasPrefix(input.SubjectUID, "person/") {
				personUID = strings.TrimPrefix(input.SubjectUID, "person/")
			}
			issue, reused, err = s.Kata.CreateTaskReused(fenced, input.Project, marker, taskclient.KataCreate{Title: "Prepare for an upcoming date", Metadata: map[string]any{taskclient.ActionMetadataKey: marker, personagenda.PersonMetadataKey: personUID, "msgvault.important_date.subject": input.SubjectUID}})
		}
		return err
	})
	if err != nil || !active {
		outcome := store.ImportantDateRetryIssue
		if attempted {
			outcome = store.ImportantDateUnknown
		}
		_ = s.advance(ctx, input.OccurrenceKey, store.ImportantDateIssueInFlight, outcome, "")
		if errors.Is(err, errDeliveryIneligible) || !active && err == nil {
			return nil
		}
		return err
	}
	if issue.UID == "" || issue.Project != input.Project {
		_ = s.advance(ctx, input.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateUnknown, "")
		return errors.New("important-date issue could not be reconciled")
	}
	if reused || issue.Status != "open" {
		return s.advance(ctx, input.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateCancelled, issue.UID)
	}
	if err := s.advance(ctx, input.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateIssueCreated, issue.UID); err != nil {
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
	attempted := false
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
		if err != nil {
			return err
		}
		if issue.Status != "open" {
			return errIssueClosed
		}
		attempted = true
		return s.Kata.NotifyIssue(fenced, input.Project, issueUID, input.Recipient, "Review the upcoming date in msgvault")
	})
	if err != nil || !active {
		outcome := store.ImportantDateIssueCreated
		if attempted {
			outcome = store.ImportantDateUnknown
		} else if errors.Is(err, errIssueClosed) {
			outcome = store.ImportantDateCancelled
		}
		_ = s.advance(ctx, input.OccurrenceKey, store.ImportantDateNotifyInFlight, outcome, issueUID)
		if errors.Is(err, errDeliveryIneligible) || errors.Is(err, errIssueClosed) || !active && err == nil {
			return nil
		}
		return err
	}
	return s.advance(ctx, input.OccurrenceKey, store.ImportantDateNotifyInFlight, store.ImportantDateDelivered, issueUID)
}

func (s DeliveryService) advance(ctx context.Context, key string, from, to store.ImportantDateDeliveryState, issue string) error {
	changed, err := s.Store.AdvanceImportantDateDeliveryContext(ctx, key, from, to, issue)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("important-date delivery state changed concurrently")
	}
	return nil
}
