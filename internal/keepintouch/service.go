package keepintouch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

const (
	KindMetadataKey  = "msgvault.outreach.v1"
	OwnerMetadataKey = "msgvault.outreach_owner"
	OutreachList     = "keep-in-touch"
)

type Archive interface {
	ListKeepInTouchPeopleContext(ctx context.Context, afterID int64, limit int) ([]store.Person, error)
	ContactStateContext(ctx context.Context, personID int64, now time.Time) (store.ContactState, error)
	ListPersonUIDsContext(ctx context.Context, personID int64) ([]string, error)
	ArchiveUIDContext(ctx context.Context) (string, error)
	KeepInTouchCursorContext(ctx context.Context) (int64, error)
	AdvanceKeepInTouchCursorContext(ctx context.Context, expected int64, next int64) (bool, error)
}

type Kata interface {
	FindActionTaskComplete(ctx context.Context, action string) (taskclient.KataTask, bool, error)
	ListPersonTasksComplete(ctx context.Context, personUID string) ([]taskclient.KataTask, error)
	CreateTaskReused(ctx context.Context, project string, key string, create taskclient.KataCreate) (taskclient.KataTask, bool, error)
	NotifyTaskOnce(ctx context.Context, task taskclient.KataTask, cycle string, recipient string) (taskclient.KataTask, error)
}

type Candidate struct {
	Person     store.Person
	UIDs       []string
	State      store.ContactState
	Cycle      string
	ArchiveUID string
}
type Draft struct {
	Evidence   []EvidenceSnapshot          `json:"-"`
	Guard      func(context.Context) error `json:"-"`
	Text       string                      `json:"text"`
	Mode       string                      `json:"mode"`
	MessageIDs []int64                     `json:"message_ids"`
	Context    string                      `json:"context,omitempty"`
}
type DraftBuilder interface {
	Build(ctx context.Context, candidate Candidate) (Draft, error)
}
type Service struct {
	Config  Config
	Archive Archive
	Kata    Kata
	Project string
	Drafts  DraftBuilder
	Clock   func() time.Time
}
type Item struct {
	PersonID      int64     `json:"person_id"`
	Cycle         string    `json:"cycle"`
	Status        string    `json:"status"`
	LastContactAt time.Time `json:"last_contact_at"`
	DueAt         time.Time `json:"due_at"`
	Channel       string    `json:"channel,omitempty"`
	Issue         Issue     `json:"issue"`
}
type Issue struct {
	UID     string `json:"uid,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Project string `json:"project,omitempty"`
}
type Failure struct {
	Summary  string `json:"summary"`
	PersonID int64  `json:"person_id"`
	Error    string `json:"error"`
}
type Result struct {
	Items         []Item         `json:"items"`
	Skipped       map[string]int `json:"skipped"`
	DraftAttempts int            `json:"draft_attempts"`
	Failures      []Failure      `json:"failures"`
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

// Check walks stable ID pages and bounds actions separately from draft
// attempts. Handled cycles never occupy the action limit or require inference.
// Preview reads only the archive, without constructing providers or using Kata.
func (s *Service) Check(ctx context.Context, dryRun bool) (out Result, runErr error) {
	out = Result{Items: []Item{}, Skipped: map[string]int{}, Failures: []Failure{}}
	if err := s.Config.Validate(); err != nil {
		return out, err
	}
	if s.Archive == nil {
		return out, errors.New("keep-in-touch archive is unavailable")
	}
	if !dryRun && (s.Kata == nil || s.Project == "" || s.Config.Owner == "") {
		return out, errors.New("keep-in-touch requires a configured Kata integration and owner")
	}
	archive, err := s.Archive.ArchiveUIDContext(ctx)
	if err != nil {
		return out, err
	}
	if archive == "" {
		return out, errors.New("keep-in-touch archive has no durable identity")
	}
	start, err := s.Archive.KeepInTouchCursorContext(ctx)
	if err != nil {
		return out, err
	}
	resume := int64(0)
	if !dryRun {
		defer func() {
			checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, err := s.Archive.AdvanceKeepInTouchCursorContext(checkpointCtx, start, resume)
			if err != nil {
				runErr = errors.Join(runErr, err)
			}
		}()
	}
	now := s.now()
	scanStart := start
	if dryRun {
		scanStart = 0
	}
	after := scanStart
	wrapped := false
walk:
	for len(out.Items) < s.Config.Limit {
		people, err := s.Archive.ListKeepInTouchPeopleContext(ctx, after, 100)
		if err != nil {
			return out, err
		}
		if len(people) == 0 {
			if scanStart > 0 && !wrapped {
				after = 0
				wrapped = true
				continue
			}
			break
		}
		for _, person := range people {
			if wrapped && person.ID > scanStart {
				break walk
			}
			if person.ID <= after {
				return out, errors.New("keep-in-touch people cursor did not advance")
			}
			after = person.ID
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			state, err := s.Archive.ContactStateContext(ctx, person.ID, now)
			if err != nil {
				out.fail(person.ID, err)
				continue
			}
			if !Eligible(state, now, s.Config.ApproachDays) {
				reason := "not_due"
				if state.Stale {
					reason = "stale"
				} else if state.CadenceDueAt == nil || state.LastContactAt == nil {
					reason = "unknown_cadence"
				}
				out.Skipped[reason]++
				continue
			}
			uids, err := s.Archive.ListPersonUIDsContext(ctx, person.ID)
			if err != nil {
				out.fail(person.ID, err)
				continue
			}
			if len(uids) == 0 || uids[0] == "" {
				out.fail(person.ID, personagenda.ErrPersonIdentity)
				continue
			}
			candidate := Candidate{Person: person, UIDs: uids, State: state, ArchiveUID: archive, Cycle: CycleKey(archive, uids[0], *state.LastContactAt)}
			attemptsBefore, itemsBefore := out.DraftAttempts, len(out.Items)
			if dryRun {
				out.Items = append(out.Items, itemFor(candidate, "preview", taskclient.KataTask{}))
			} else if err := s.process(ctx, candidate, &out); err != nil {
				out.fail(person.ID, err)
			}
			if out.DraftAttempts > attemptsBefore || len(out.Items) > itemsBefore {
				resume = person.ID
			}
			if len(out.Items) >= s.Config.Limit {
				break walk
			}
		}
	}
	if len(out.Items) < s.Config.Limit && out.DraftAttempts < s.Config.MaxDraftsPerRun {
		resume = 0
	}
	if len(out.Failures) > 0 {
		return out, fmt.Errorf("keep-in-touch could not process %d people", len(out.Failures))
	}
	return out, nil
}
func (r *Result) fail(id int64, err error) {
	r.Failures = append(r.Failures, Failure{PersonID: id, Error: err.Error(), Summary: failureSummary(err)})
}

func (s *Service) process(ctx context.Context, c Candidate, out *Result) error {
	// Every historical UID participates in closed-cycle lookup, not just open
	// agenda lookup. Merging a person must not resurrect a completed cycle.
	for _, uid := range c.UIDs {
		marker := CycleKey(c.ArchiveUID, uid, *c.State.LastContactAt)
		task, found, err := s.Kata.FindActionTaskComplete(ctx, marker)
		if err != nil {
			return fmt.Errorf("cannot search all Kata reminders: %w", err)
		}
		if found {
			return s.deliver(ctx, c, marker, task, out)
		}
	}
	for _, uid := range c.UIDs {
		tasks, err := s.Kata.ListPersonTasksComplete(ctx, uid)
		if err != nil {
			return err
		}
		if len(tasks) > 100 {
			return errors.New("too many open person tasks to establish outreach eligibility")
		}
		for _, task := range tasks {
			if task.Status == "open" && task.Metadata[KindMetadataKey] == c.ArchiveUID {
				out.Skipped["open_reminder"]++
				return nil
			}
		}
	}
	if out.DraftAttempts >= s.Config.MaxDraftsPerRun {
		out.Skipped["draft_limit"]++
		return nil
	}
	if s.Drafts == nil {
		return errors.New("keep-in-touch draft provider is unavailable")
	}
	out.DraftAttempts++
	draft, err := s.Drafts.Build(ctx, c)
	if errors.Is(err, ErrEvidenceChanged) {
		out.Skipped["evidence_changed"]++
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(draft.Text) == "" {
		return errors.New("keep-in-touch draft is empty")
	}
	if ok, err := s.unchanged(ctx, c); err != nil {
		return err
	} else if !ok {
		out.Skipped["changed"]++
		return nil
	}
	if draft.Guard != nil {
		if err := draft.Guard(ctx); errors.Is(err, ErrEvidenceChanged) {
			out.Skipped["evidence_changed"]++
			return nil
		} else if err != nil {
			return err
		}
	}
	task, _, err := s.Kata.CreateTaskReused(ctx, s.Project, c.Cycle, taskclient.KataCreate{
		Title: "Keep in touch: " + personLabel(c.Person), Body: renderIssue(c, draft, s.now()), Owner: s.Config.Owner,
		Metadata: map[string]any{taskclient.ActionMetadataKey: c.Cycle, KindMetadataKey: c.ArchiveUID, OwnerMetadataKey: s.Config.Owner, personagenda.PersonMetadataKey: c.UIDs[0], personagenda.ListMetadataKey: OutreachList, EvidenceMetadataKey: draft.Evidence, DraftModeMetadataKey: draft.Mode},
	})
	if err != nil {
		// A concurrent winner may have a different generated draft. Its exact
		// action marker is the authority, independent of the provider's wording.
		winner, found, readErr := s.Kata.FindActionTaskComplete(ctx, c.Cycle)
		if readErr != nil || !found {
			return err
		}
		task = winner
	}
	return s.deliver(ctx, c, c.Cycle, task, out)
}
func (s *Service) deliver(ctx context.Context, c Candidate, marker string, task taskclient.KataTask, out *Result) error {
	if task.Status != "open" {
		out.Skipped["closed_cycle"]++
		return nil
	}
	owner, ok := task.Metadata[OwnerMetadataKey].(string)
	if !ok || owner == "" {
		return errors.New("existing outreach issue has no original recipient")
	}
	if receipt := task.Metadata[taskclient.OutreachDeliveryMetadataKey]; receipt != nil {
		if receipt != marker+":"+owner {
			return errors.New("existing outreach issue has a conflicting delivery receipt")
		}
		out.Skipped["already_delivered"]++
		return nil
	}
	if ok, err := s.unchanged(ctx, c); err != nil {
		return err
	} else if !ok {
		out.Skipped["changed"]++
		return nil
	}
	if err := s.validateTaskEvidence(ctx, c, task); errors.Is(err, ErrEvidenceChanged) {
		out.Skipped["evidence_changed"]++
		return nil
	} else if err != nil {
		return err
	}
	updated, err := s.Kata.NotifyTaskOnce(ctx, task, marker, owner)
	if err != nil {
		return err
	}
	if updated.Status != "open" {
		out.Skipped["closed_cycle"]++
		return nil
	}
	c.Cycle = marker
	out.Items = append(out.Items, itemFor(c, "delivered", updated))
	return nil
}
func (s *Service) unchanged(ctx context.Context, c Candidate) (bool, error) {
	uids, err := s.Archive.ListPersonUIDsContext(ctx, c.Person.ID)
	if err != nil {
		return false, err
	}
	if !slices.Equal(uids, c.UIDs) {
		return false, nil
	}
	state, err := s.Archive.ContactStateContext(ctx, c.Person.ID, s.now())
	if err != nil {
		return false, err
	}
	return Eligible(state, s.now(), s.Config.ApproachDays) && state.LastContactAt.Equal(*c.State.LastContactAt) && state.CadenceDueAt.Equal(*c.State.CadenceDueAt), nil
}
func itemFor(c Candidate, status string, task taskclient.KataTask) Item {
	return Item{PersonID: c.Person.ID, Cycle: c.Cycle, Status: status, LastContactAt: *c.State.LastContactAt, DueAt: *c.State.CadenceDueAt, Channel: string(c.State.LastContactChannel), Issue: Issue{UID: task.UID, Ref: task.QualifiedRef, Project: task.Project}}
}
func personLabel(p store.Person) string {
	if p.DisplayName != nil && strings.TrimSpace(*p.DisplayName) != "" {
		return *p.DisplayName
	}
	return fmt.Sprintf("person %d", p.ID)
}
func renderIssue(c Candidate, d Draft, now time.Time) string {
	days := (c.State.CadenceDueAt.Unix() - c.State.LastContactAt.Unix()) / 86400
	why := "approaching the saved contact interval"
	if !now.Before(*c.State.CadenceDueAt) {
		why = "the saved contact interval is due"
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Person: %s (`person:%d`, UID `%s`).\n\nLast qualifying contact: %s (%s).\nSaved cadence: %d days; due %s. Reminder reason: %s.\n", personLabel(c.Person), c.Person.ID, c.UIDs[0], c.State.LastContactAt.Format(time.RFC3339), c.State.LastContactRef, days, c.State.CadenceDueAt.Format(time.RFC3339), why)
	if c.State.LastContactChannel != "" {
		fmt.Fprintf(&body, "Suggested channel, from last contact: %s.\n", c.State.LastContactChannel)
	}
	fmt.Fprintf(&body, "\n%s (review before sending):\n\n%s\n", draftLabel(d.Mode), d.Text)
	if len(d.MessageIDs) > 0 {
		body.WriteString("\nConversation evidence (open these archived messages):\n")
		for _, id := range d.MessageIDs {
			fmt.Fprintf(&body, "- `message:%d`\n", id)
		}
	}
	if d.Context != "" {
		fmt.Fprintf(&body, "\nCurrent profile context for review (not sent to inference; verify before using):\n%s\n", d.Context)
	}
	body.WriteString("\nNo message was sent. Closing this issue stops reminders until another qualifying contact.\n")
	return body.String()
}

func draftLabel(mode string) string {
	switch mode {
	case "grounded":
		return "Draft from messages"
	case "neutral_no_evidence":
		return "Neutral draft, no evidence"
	default:
		return "Draft"
	}
}
