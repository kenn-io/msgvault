// Package calcontrol applies source policy and delegated calendar grants to
// live Google Calendar operations, then writes successful changes to the archive.
package calcontrol

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/teambition/rrule-go"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
)

const (
	actionCreate = "create"
	actionUpdate = "update"
	actionMove   = "move"
	scopeFuture  = "future"
)

var ErrDenied = errors.New("calendar operation denied")
var ErrInvalid = errors.New("invalid calendar request")
var ErrPlanChanged = errors.New("calendar plan changed since preview; no write was performed")

// ErrInternal identifies daemon setup failures before provider operations begin.
var ErrInternal = errors.New("calendar daemon setup failed")

// Keep fingerprints stable across per-request Service instances without making
// redacted provider fields guessable from their hashes. Restarting the daemon
// invalidates pending previews.
var planFingerprintKey = sync.OnceValue(func() []byte { return []byte(rand.Text()) })

// Request keeps the OAuth account separate from the calendar being controlled.
// Event is a partial patch: omitted fields are preserved, explicit empty fields
// are cleared. OriginalStart identifies an occurrence by its original date or
// RFC3339 start, even when the occurrence has been rescheduled.
type Request struct {
	Action                  string          `json:"action" enum:"create,update,delete,move,respond,freebusy,conflicts"`
	Account                 string          `json:"account"`
	CalendarID              string          `json:"calendar_id"`
	EventID                 string          `json:"event_id,omitempty"`
	Destination             string          `json:"destination,omitempty"`
	Event                   gcal.EventInput `json:"event,omitzero"`
	AddAttendees            []string        `json:"add_attendees,omitempty"`
	SendUpdates             string          `json:"send_updates,omitempty" enum:"none,all,externalOnly"`
	Scope                   string          `json:"scope,omitempty" enum:"single,future,all"`
	OriginalStart           string          `json:"original_start,omitempty"`
	Response                string          `json:"response,omitempty" enum:"accepted,declined,tentative"`
	CalendarIDs             []string        `json:"calendar_ids,omitempty"`
	TimeMin                 time.Time       `json:"time_min,omitzero"`
	TimeMax                 time.Time       `json:"time_max,omitzero"`
	TimeZone                string          `json:"time_zone,omitempty"`
	DryRun                  bool            `json:"dry_run,omitempty"`
	ReadOnly                bool            `json:"read_only,omitempty"`
	ExpectedPlanFingerprint string          `json:"expected_plan_fingerprint,omitempty" doc:"Execute only if the OAuth account, planned writes, and normalized send_updates still match a previous dry run"`
}

// EventTarget identifies existing event content without adding it to a mutation.
type EventTarget struct {
	Summary string             `json:"summary,omitempty"`
	Start   gcal.EventDateTime `json:"start"`
}

type PlannedWrite struct {
	Action      string          `json:"action"`
	CalendarID  string          `json:"calendar_id"`
	EventID     string          `json:"event_id,omitempty"`
	Destination string          `json:"destination,omitempty"`
	Event       gcal.EventInput `json:"event,omitzero"`
	Target      *EventTarget    `json:"target,omitzero"`
}

// WriteReceipt is returned even if a remote success cannot be archived. It
// prevents an archive error from disguising a completed remote mutation.
type WriteReceipt struct {
	Action       string     `json:"action"`
	CalendarID   string     `json:"calendar_id"`
	Event        gcal.Event `json:"event"`
	MessageID    int64      `json:"message_id,omitempty"`
	Archived     bool       `json:"archived"`
	ArchiveError string     `json:"archive_error,omitempty"`
}
type Conflict struct {
	CalendarIDs []string  `json:"calendar_ids"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
}
type Result struct {
	Account         string                 `json:"account"`
	CalendarID      string                 `json:"calendar_id"`
	SendUpdates     string                 `json:"send_updates"`
	DryRun          bool                   `json:"dry_run"`
	Plan            []PlannedWrite         `json:"plan"`
	PlanFingerprint string                 `json:"plan_fingerprint,omitempty" doc:"Opaque comparison token bound to the OAuth account, planned writes, and normalized send_updates; valid within one daemon process"`
	Writes          []WriteReceipt         `json:"writes"`
	FreeBusy        *gcal.FreeBusyResponse `json:"freebusy,omitzero"`
	Conflicts       []Conflict             `json:"conflicts,omitempty"`
	OutcomeUnknown  bool                   `json:"outcome_unknown,omitempty"`
	OutcomeCode     string                 `json:"outcome_code,omitempty" enum:"calendar_partial,calendar_outcome_unknown" doc:"Machine-readable classification for a partial provider write"`
	Error           string                 `json:"error,omitempty"` // partial remote failure; do not replay completed writes
}

type Service struct {
	Source  config.GCalSource
	Client  gcal.ControlAPI
	Persist func(context.Context, gcal.Calendar, gcal.Event) (int64, error)
	// AcquireWrite serializes mutations after planning and confirmation checks.
	AcquireWrite func(context.Context) (func(), error)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
func denied(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDenied, fmt.Sprintf(format, args...))
}
func IsRead(action string) bool { return action == "freebusy" || action == "conflicts" }

// RequestPermissions returns capabilities evident before reading provider state.
// Existing guests can additionally require calendar.invite after the event is read.
func RequestPermissions(r Request) []agentgrant.Permission {
	if IsRead(r.Action) {
		return []agentgrant.Permission{agentgrant.PermissionCalendarRead}
	}
	permissions := []agentgrant.Permission{agentgrant.PermissionCalendarWrite}
	if (r.SendUpdates != "" && r.SendUpdates != "none") || r.Event.Attendees != nil || len(r.AddAttendees) > 0 || r.Action == "respond" {
		permissions = append(permissions, agentgrant.PermissionCalendarInvite)
	}
	return permissions
}

func requestCalendarIDs(r Request) []string {
	if IsRead(r.Action) && len(r.CalendarIDs) > 0 {
		return r.CalendarIDs
	}
	ids := []string{r.CalendarID}
	if r.Action == actionMove {
		ids = append(ids, r.Destination)
	}
	return ids
}

// AuthorizeSourceRequest checks exact grants before configuration policy or OAuth
// setup. The primary alias still needs a live lookup and canonical-ID check.
func AuthorizeSourceRequest(source config.GCalSource, r Request, grant *agentgrant.Grant) error {
	s := Service{Source: source}
	account := strings.ToLower(strings.TrimSpace(source.Email))
	ids := requestCalendarIDs(r)
	permissions := RequestPermissions(r)
	for _, id := range ids {
		id = s.resolveAlias(id)
		if id == "primary" {
			continue
		}
		for _, permission := range permissions {
			if err := authorizeGrant(id, account, permission, grant); err != nil {
				return err
			}
		}
	}
	if !source.Enabled {
		return denied("calendar source is disabled")
	}
	requestedAccount := strings.TrimSpace(r.Account)
	if !strings.EqualFold(requestedAccount, strings.TrimSpace(source.Email)) && !strings.EqualFold(requestedAccount, strings.TrimSpace(source.Name)) {
		return denied("account does not match configured source")
	}
	write := !IsRead(r.Action)
	if write && r.ReadOnly {
		return denied("read-only mode forbids event changes")
	}
	invite := slices.Contains(permissions, agentgrant.PermissionCalendarInvite)
	for _, id := range ids {
		id = s.resolveAlias(id)
		if id != "primary" {
			if err := s.authorize(id, account, write, invite, grant); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) Execute(ctx context.Context, r Request, grant *agentgrant.Grant) (*Result, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if err := AuthorizeSourceRequest(s.Source, r, grant); err != nil {
		return nil, err
	}
	account := strings.ToLower(strings.TrimSpace(s.Source.Email))
	write := !IsRead(r.Action)
	if write && s.Persist == nil {
		return nil, denied("archive write-through is unavailable")
	}
	if !write && len(r.CalendarIDs) > 0 {
		calendars, err := s.calendarsForRequest(ctx, r, grant)
		if err != nil {
			return nil, err
		}
		updates, _ := gcal.SendUpdates(r.SendUpdates)
		result := &Result{Account: account, SendUpdates: updates, DryRun: r.DryRun, Plan: []PlannedWrite{}, Writes: []WriteReceipt{}}
		return s.availability(ctx, r, grant, calendars, result)
	}
	calID := s.resolveAlias(r.CalendarID)
	destinationID := ""
	if r.Action == actionMove {
		destinationID = s.resolveAlias(r.Destination)
	}
	calendars, err := s.calendarsForRequest(ctx, r, grant)
	if err != nil {
		return nil, err
	}
	cal, err := lookupCalendar(calendars, calID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(cal.ID, account, write, slices.Contains(RequestPermissions(r), agentgrant.PermissionCalendarInvite), grant); err != nil {
		return nil, err
	}
	if err := requireCalendarRole(cal, write); err != nil {
		return nil, err
	}
	updates, _ := gcal.SendUpdates(r.SendUpdates)
	result := &Result{Account: account, CalendarID: cal.ID, SendUpdates: updates, DryRun: r.DryRun, Plan: []PlannedWrite{}, Writes: []WriteReceipt{}}
	if !write {
		return s.availability(ctx, r, grant, calendars, result)
	}
	input := r.Event
	var existing *gcal.Event
	if r.Action != actionCreate {
		existing, err = s.Client.GetEvent(ctx, cal.ID, r.EventID)
		if err != nil {
			return nil, fmt.Errorf("read event: %w", err)
		}
		existing, err = s.selectScope(ctx, cal, *existing, r)
		if err != nil {
			return nil, err
		}
	}
	if r.Action == actionCreate || r.Action == actionUpdate {
		var recurrence []string
		if input.Recurrence != nil {
			recurrence = *input.Recurrence
		} else if existing != nil {
			recurrence = existing.Recurrence
		}
		if len(recurrence) > 0 && r.Scope != scopeFuture {
			// Google requires an explicit expansion time zone for timed series.
			// Use the target calendar's zone when no event zone was supplied.
			for _, bounds := range []struct {
				target **gcal.EventDateTime
				prior  gcal.EventDateTime
			}{{&input.Start, eventBound(existing, true)}, {&input.End, eventBound(existing, false)}} {
				value := bounds.prior
				if *bounds.target != nil {
					value = **bounds.target
				}
				if value.IsZero() || value.IsAllDay() {
					continue
				}
				if value.TimeZone == "" {
					value.TimeZone = cal.TimeZone
				}
				if value.TimeZone == "" {
					return nil, invalid("timed recurrence requires a time zone on the event or target calendar")
				}
				*bounds.target = &value
			}
		}
	}
	invite := updates != "none" || (input.Attendees != nil) || len(r.AddAttendees) > 0 || r.Action == "respond"
	// Deleting, moving, or editing invited events can change invitation state
	// even when email notifications are suppressed with sendUpdates=none.
	if existing != nil && hasGuestAttendee(existing.Attendees) {
		invite = true
	}
	if invite {
		if err := s.authorize(cal.ID, account, true, true, grant); err != nil {
			return nil, err
		}
	}
	if r.Action == actionUpdate && len(r.AddAttendees) > 0 {
		attendees := slices.Clone(existing.Attendees)
		for i := range attendees {
			attendees[i] = attendeePatchValue(attendees[i])
		}
		for _, email := range r.AddAttendees {
			if !slices.ContainsFunc(attendees, func(a gcal.Attendee) bool { return strings.EqualFold(a.Email, email) }) {
				attendees = append(attendees, gcal.Attendee{Email: email})
			}
		}
		input.Attendees = &attendees
	}
	if r.Action == "respond" {
		var self *gcal.Attendee
		for _, a := range existing.Attendees {
			if a.Self {
				attendee := a
				self = &attendee
				break
			}
		}
		if self == nil {
			return nil, invalid("event has no self attendee; cannot respond for somebody else")
		}
		if self.Organizer {
			return nil, invalid("the organizer cannot respond to their own event")
		}
		attendee := attendeePatchValue(*self)
		attendee.ResponseStatus = r.Response
		attendees := []gcal.Attendee{attendee}
		omitted := true
		input = gcal.EventInput{Attendees: &attendees, AttendeesOmitted: &omitted}
	}
	destination := gcal.Calendar{}
	if r.Action == actionMove {
		destination, err = lookupCalendar(calendars, destinationID)
		if err != nil {
			return nil, err
		}
		if err := s.authorize(destination.ID, account, true, invite, grant); err != nil {
			return nil, err
		}
		if err := requireCalendarRole(destination, true); err != nil {
			return nil, err
		}
		if destination.ID == cal.ID {
			return nil, invalid("destination must differ from source calendar")
		}
		if existing.RecurringEventID != "" || len(existing.Recurrence) > 0 {
			return nil, invalid("move supports standalone events; recurring events require an explicit series edit")
		}
	}
	targetID := r.EventID
	if existing != nil {
		targetID = existing.ID
	}
	plan := []PlannedWrite{{Action: r.Action, CalendarID: cal.ID, EventID: targetID, Event: input}}
	if r.Action == actionMove {
		plan[0].Destination = destination.ID
	}
	if r.Scope == scopeFuture {
		plan, err = s.futurePlan(ctx, cal, *existing, r, input)
		if err != nil {
			return nil, err
		}
	}
	// Validate the merged range for patches without converting omitted fields
	// into explicit writes. A standalone patch can change just one bound.
	if r.Action == actionUpdate && r.Scope != scopeFuture && (input.Start != nil || input.End != nil) {
		merged := input
		if merged.Start == nil {
			merged.Start = &existing.Start
		}
		if merged.End == nil {
			merged.End = &existing.End
		}
		if err := validateRange(merged.Start, merged.End); err != nil {
			return nil, err
		}
	}
	if existing != nil {
		for i := range plan {
			if plan[i].EventID != "" {
				plan[i].Target = &EventTarget{Summary: existing.Summary, Start: existing.Start}
			}
		}
	}
	planJSON, err := json.Marshal(plan, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("marshal calendar plan: %w", err)
	}
	fingerprint := hmac.New(sha256.New, planFingerprintKey())
	fingerprint.Write([]byte(account))
	fingerprint.Write([]byte{0})
	fingerprint.Write([]byte(updates))
	fingerprint.Write([]byte{0})
	fingerprint.Write(planJSON)
	result.PlanFingerprint = hex.EncodeToString(fingerprint.Sum(nil))
	if r.ExpectedPlanFingerprint != "" && !hmac.Equal([]byte(r.ExpectedPlanFingerprint), []byte(result.PlanFingerprint)) {
		return nil, ErrPlanChanged
	}
	result.Plan = redactPlannedWrites(plan, r, account, grant)
	if r.DryRun {
		return result, nil
	}
	if s.AcquireWrite != nil {
		release, err := s.AcquireWrite(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	opts := gcal.MutationOptions{SendUpdates: updates}
	if existing != nil {
		opts.IfMatch = existing.ETag
	}
	truncatedETag := ""
	for index, step := range plan {
		var ev *gcal.Event
		switch step.Action {
		case actionCreate:
			ev, err = s.Client.InsertEvent(ctx, step.CalendarID, step.Event, gcal.MutationOptions{SendUpdates: updates})
		case actionUpdate, "respond":
			ev, err = s.Client.PatchEvent(ctx, step.CalendarID, step.EventID, step.Event, opts)
		case "delete":
			err = s.Client.DeleteEvent(ctx, step.CalendarID, step.EventID, opts)
			if err == nil {
				cancelled := *existing
				cancelled.Status = gcal.StatusCancelled
				ev = &cancelled
			}
		case actionMove:
			ev, err = s.Client.MoveEvent(ctx, step.CalendarID, step.EventID, step.Destination, opts)
		}
		if err != nil {
			if len(result.Writes) > 0 {
				if errors.Is(err, gcal.ErrOutcomeUnknown) {
					result.OutcomeUnknown = true
					result.OutcomeCode = "calendar_outcome_unknown"
					result.Error = step.Action + " outcome unknown after completed writes; reconcile the calendar and receipts before taking further action"
				} else {
					result.OutcomeCode = "calendar_partial"
					result.Error = step.Action + " failed after completed writes; reconcile the calendar and receipts before taking further action"
					if r.Scope == scopeFuture && index == 1 && step.Action == actionCreate {
						s.restoreRecurrence(ctx, result, cal, *existing, truncatedETag, updates, grant)
					}
				}
				return result, nil
			}
			return nil, err
		}
		if ev == nil || ev.ID == "" {
			result.OutcomeUnknown = true
			result.OutcomeCode = "calendar_outcome_unknown"
			result.Error = "provider returned no event after write; outcome unknown; reconcile the calendar and receipts before taking further action"
			return result, nil
		}
		if r.Scope == scopeFuture && index == 0 && len(plan) == 2 {
			truncatedETag = ev.ETag
		}
		// Use a short independent context after a successful remote change so a
		// disconnected caller cannot cancel the local archive write-through.
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if step.Action == actionMove {
			old := *existing
			old.Status = gcal.StatusCancelled
			s.archive(persistCtx, result, cal, old, "move-source", grant)
			s.archive(persistCtx, result, destination, *ev, "move-destination", grant)
		} else {
			s.archive(persistCtx, result, cal, *ev, step.Action, grant)
		}
		cancel()
	}
	return result, nil
}
func eventBound(event *gcal.Event, start bool) gcal.EventDateTime {
	if event == nil {
		return gcal.EventDateTime{}
	}
	if start {
		return event.Start
	}
	return event.End
}

func redactPlannedWrites(plan []PlannedWrite, request Request, account string, grant *agentgrant.Grant) []PlannedWrite {
	if grant == nil {
		return plan
	}
	redacted := slices.Clone(plan)
	for i := range redacted {
		if canReadEventContent(grant, account, redacted[i].CalendarID) || request.Action == actionCreate {
			continue
		}
		redacted[i].Target = nil
		switch {
		case request.Scope == scopeFuture:
			// A future split plan is synthesized from the existing event. Keep its
			// operation metadata, but do not reveal the provider-derived payload.
			redacted[i].Event = gcal.EventInput{}
		case request.Action == actionUpdate:
			// The request patch is caller-supplied. The merged plan can include
			// current attendees or other fields read from the provider.
			redacted[i].Event = request.Event
		default:
			redacted[i].Event = gcal.EventInput{}
		}
	}
	return redacted
}

func canReadEventContent(grant *agentgrant.Grant, account, calendarID string) bool {
	if grant == nil {
		return true
	}
	ref := agentgrant.SourceRef{Type: gcal.SourceType, Identifier: account + "/" + calendarID}
	return grant.Allows(agentgrant.PermissionCalendarEventRead, ref)
}

func (s *Service) archive(ctx context.Context, result *Result, cal gcal.Calendar, ev gcal.Event, action string, grant *agentgrant.Grant) {
	id, err := s.Persist(ctx, cal, ev)
	receiptEvent := ev
	if !canReadEventContent(grant, result.Account, cal.ID) {
		receiptEvent = gcal.Event{ID: ev.ID, Status: ev.Status}
	}
	receipt := WriteReceipt{Action: action, CalendarID: cal.ID, Event: receiptEvent, MessageID: id, Archived: err == nil}
	if err != nil {
		receipt.ArchiveError = err.Error()
	}
	result.Writes = append(result.Writes, receipt)
}
func (s *Service) resolveAlias(id string) string {
	if resolved, ok := s.Source.CalendarAliases[id]; ok {
		return resolved
	}
	return id
}
func authorizeGrant(id, account string, permission agentgrant.Permission, grant *agentgrant.Grant) error {
	ref := agentgrant.SourceRef{Type: gcal.SourceType, Identifier: account + "/" + id}
	if grant != nil && !grant.Allows(permission, ref) {
		return denied("grant lacks %s for calendar %q", permission, id)
	}
	return nil
}
func (s *Service) authorize(id, account string, write, invite bool, grant *agentgrant.Grant) error {
	permission := agentgrant.PermissionCalendarRead
	if write {
		permission = agentgrant.PermissionCalendarWrite
	}
	if err := authorizeGrant(id, account, permission, grant); err != nil {
		return err
	}
	if invite {
		if err := authorizeGrant(id, account, agentgrant.PermissionCalendarInvite, grant); err != nil {
			return err
		}
	}
	if write && !slices.Contains(s.Source.WriteCalendars, id) {
		return denied("calendar %q is not in write_calendars", id)
	}
	if invite && !slices.Contains(s.Source.InviteCalendars, id) {
		return denied("calendar %q is not in invite_calendars", id)
	}
	return nil
}

// Restore only a definitely failed replacement, guarded by the ETag returned by
// truncation. An unknown insert may have created a replacement and must reconcile.
func (s *Service) restoreRecurrence(ctx context.Context, result *Result, cal gcal.Calendar, original gcal.Event, etag, updates string, grant *agentgrant.Grant) {
	if etag == "" {
		result.Error += "; original recurrence was not restored because the truncation response omitted its ETag"
		return
	}
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	restored, err := s.Client.PatchEvent(restoreCtx, cal.ID, original.ID, gcal.EventInput{Recurrence: &original.Recurrence}, gcal.MutationOptions{SendUpdates: updates, IfMatch: etag})
	if errors.Is(err, gcal.ErrOutcomeUnknown) || (err == nil && (restored == nil || restored.ID == "")) {
		result.OutcomeUnknown = true
		result.OutcomeCode = "calendar_outcome_unknown"
		result.Error = "replacement failed; original recurrence restoration outcome unknown; reconcile the calendar and receipts before taking further action"
		return
	}
	if err != nil {
		result.Error = "replacement failed; original recurrence restoration failed; reconcile the calendar and receipts before taking further action"
		return
	}
	s.archive(restoreCtx, result, cal, *restored, "restore", grant)
	result.Error = "replacement failed after completed writes; original recurrence restored; inspect receipts before taking further action"
}

// Resolve primary grants before exposing policy details or reading events. Until
// this succeeds, provider lookup failures must not disclose source setup state.
func (s *Service) calendarsForRequest(ctx context.Context, r Request, grant *agentgrant.Grant) ([]gcal.Calendar, error) {
	unresolvedPrimary := grant != nil && slices.ContainsFunc(requestCalendarIDs(r), func(id string) bool { return s.resolveAlias(id) == "primary" })
	calendars, err := s.calendars(ctx)
	if !unresolvedPrimary {
		return calendars, err
	}
	if err != nil {
		return nil, denied("calendar access denied")
	}
	cal, err := lookupCalendar(calendars, "primary")
	if err != nil {
		return nil, denied("calendar access denied")
	}
	account := strings.ToLower(strings.TrimSpace(s.Source.Email))
	for _, permission := range RequestPermissions(r) {
		if err := authorizeGrant(cal.ID, account, permission, grant); err != nil {
			return nil, denied("calendar access denied")
		}
	}
	return calendars, nil
}

func (s *Service) calendars(ctx context.Context) ([]gcal.Calendar, error) {
	var result []gcal.Calendar
	token := ""
	seen := map[string]bool{}
	for {
		page, err := s.Client.ListCalendars(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("verify calendar access: %w", err)
		}
		if page == nil {
			return nil, errors.New("empty calendar list response")
		}
		result = append(result, page.Items...)
		token = page.NextPageToken
		if token == "" {
			return result, nil
		}
		if seen[token] {
			return nil, errors.New("repeated calendar list page token")
		}
		seen[token] = true
	}
}
func findCalendar(calendars []gcal.Calendar, id string, write bool) (gcal.Calendar, error) {
	cal, err := lookupCalendar(calendars, id)
	if err != nil {
		return gcal.Calendar{}, err
	}
	if err := requireCalendarRole(cal, write); err != nil {
		return gcal.Calendar{}, err
	}
	return cal, nil
}

func lookupCalendar(calendars []gcal.Calendar, id string) (gcal.Calendar, error) {
	for _, cal := range calendars {
		if cal.Deleted || (cal.ID != id && (id != "primary" || !cal.Primary)) {
			continue
		}
		return cal, nil
	}
	return gcal.Calendar{}, denied("calendar %q is not in the live calendar list", id)
}

func requireCalendarRole(cal gcal.Calendar, write bool) error {
	if write && cal.AccessRole != "owner" && cal.AccessRole != "writer" {
		return denied("calendar %q has accessRole %q; owner or writer required", cal.ID, cal.AccessRole)
	}
	if !write && !slices.Contains([]string{"owner", "writer", "reader", "freeBusyReader"}, cal.AccessRole) {
		return denied("calendar %q has no readable accessRole", cal.ID)
	}
	return nil
}

func hasGuestAttendee(attendees []gcal.Attendee) bool {
	return slices.ContainsFunc(attendees, func(attendee gcal.Attendee) bool { return !attendee.Organizer })
}

// attendeePatchValue drops provider-managed identity flags while preserving
// existing RSVP state when an attendee list is reused for a patch.
func attendeePatchValue(attendee gcal.Attendee) gcal.Attendee {
	attendee.Organizer = false
	attendee.Self = false
	return attendee
}

func (s *Service) availability(ctx context.Context, r Request, grant *agentgrant.Grant, calendars []gcal.Calendar, result *Result) (*Result, error) {
	ids := slices.Clone(r.CalendarIDs)
	if len(ids) == 0 {
		ids = []string{r.CalendarID}
	}
	if len(ids) > 50 {
		return nil, invalid("availability accepts at most 50 calendars")
	}
	items := []gcal.FreeBusyItem{}
	for _, id := range ids {
		target, err := findCalendar(calendars, s.resolveAlias(id), false)
		if err != nil {
			return nil, err
		}
		if err := s.authorize(target.ID, result.Account, false, false, grant); err != nil {
			return nil, err
		}
		if result.CalendarID == "" {
			result.CalendarID = target.ID
		}
		if !slices.ContainsFunc(items, func(item gcal.FreeBusyItem) bool { return item.ID == target.ID }) {
			items = append(items, gcal.FreeBusyItem{ID: target.ID})
		}
	}
	busy, err := s.Client.FreeBusy(ctx, gcal.FreeBusyRequest{TimeMin: r.TimeMin, TimeMax: r.TimeMax, TimeZone: r.TimeZone, Items: items})
	if err != nil {
		return nil, err
	}
	if busy == nil {
		return nil, errors.New("empty freebusy response")
	}
	for _, item := range items {
		value, ok := busy.Calendars[item.ID]
		if !ok {
			return nil, fmt.Errorf("freebusy omitted calendar %q", item.ID)
		}
		if len(value.Errors) > 0 {
			return nil, fmt.Errorf("freebusy unavailable for calendar %q: %s", item.ID, value.Errors[0].Reason)
		}
		for _, period := range value.Busy {
			if period.Start.IsZero() || !period.End.After(period.Start) {
				return nil, errors.New("invalid busy period in calendar response")
			}
		}
	}
	result.FreeBusy = busy
	if r.Action == "conflicts" {
		result.Conflicts = conflicts(busy)
	}
	return result, nil
}
func conflicts(busy *gcal.FreeBusyResponse) []Conflict {
	ids := make([]string, 0, len(busy.Calendars))
	for id := range busy.Calendars {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := []Conflict{}
	for i, id := range ids {
		for _, other := range ids[i+1:] {
			for _, a := range busy.Calendars[id].Busy {
				for _, b := range busy.Calendars[other].Busy {
					start, end := a.Start, a.End
					if b.Start.After(start) {
						start = b.Start
					}
					if b.End.Before(end) {
						end = b.End
					}
					if end.After(start) {
						result = append(result, Conflict{CalendarIDs: []string{id, other}, Start: start, End: end})
					}
				}
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].Start.Equal(result[j].Start) {
			return result[i].Start.Before(result[j].Start)
		}
		return strings.Join(result[i].CalendarIDs, "\x00") < strings.Join(result[j].CalendarIDs, "\x00")
	})
	return result
}
func (r Request) Validate() error {
	if !slices.Contains([]string{actionCreate, actionUpdate, "delete", actionMove, "respond", "freebusy", "conflicts"}, r.Action) {
		return invalid("unknown action %q", r.Action)
	}
	if r.ExpectedPlanFingerprint != "" && (IsRead(r.Action) || r.DryRun) {
		return invalid("expected_plan_fingerprint requires a non-dry-run event mutation")
	}
	if strings.TrimSpace(r.Account) == "" {
		return invalid("account is required")
	}
	if IsRead(r.Action) {
		if strings.TrimSpace(r.CalendarID) == "" && len(r.CalendarIDs) == 0 {
			return invalid("calendar_id or calendar_ids is required")
		}
	} else if strings.TrimSpace(r.CalendarID) == "" {
		return invalid("calendar_id is required")
	}
	if _, err := gcal.SendUpdates(r.SendUpdates); err != nil {
		return invalid("%v", err)
	}
	if r.Scope != "" && !slices.Contains([]string{"single", "all", scopeFuture}, r.Scope) {
		return invalid("scope must be single, future, or all")
	}
	if r.Event.AttendeesOmitted != nil {
		return invalid("attendeesOmitted is reserved for self RSVP")
	}
	if IsRead(r.Action) {
		if r.TimeMin.IsZero() || !r.TimeMax.After(r.TimeMin) {
			return invalid("availability requires time_min before time_max")
		}
		if r.EventID != "" || r.Destination != "" || r.Scope != "" || r.OriginalStart != "" || r.Response != "" || len(r.AddAttendees) > 0 || hasEventFields(r.Event) || r.SendUpdates != "" {
			return invalid("event mutation fields are not valid for availability")
		}
	} else {
		if !r.TimeMin.IsZero() || !r.TimeMax.IsZero() || len(r.CalendarIDs) > 0 || r.TimeZone != "" {
			return invalid("availability fields are only valid for freebusy or conflicts")
		}
		if r.Action != actionCreate && r.EventID == "" {
			return invalid("event_id is required")
		}
		if r.Action == actionCreate {
			if r.EventID != "" || r.Scope != "" || r.OriginalStart != "" {
				return invalid("create cannot select an existing event or recurrence scope")
			}
			if r.Event.Summary == nil || strings.TrimSpace(*r.Event.Summary) == "" {
				return invalid("create requires a summary")
			}
			if err := validateRange(r.Event.Start, r.Event.End); err != nil {
				return err
			}
		}
		if r.Action != actionMove && r.Destination != "" {
			return invalid("destination is only valid for move")
		}
		if r.Action == actionMove && r.Destination == "" {
			return invalid("move requires destination")
		}
		if r.Action != "respond" && r.Response != "" {
			return invalid("response is only valid for respond")
		}
		if r.Action == "respond" && !slices.Contains([]string{"accepted", "declined", "tentative"}, r.Response) {
			return invalid("response must be accepted, declined, or tentative")
		}
		if r.Action != actionUpdate && len(r.AddAttendees) > 0 {
			return invalid("add_attendees is only valid for update")
		}
		if r.Action != actionCreate && r.Action != actionUpdate && hasEventFields(r.Event) {
			return invalid("event fields are only valid for create or update")
		}
		if r.Action == actionUpdate && !hasEventFields(r.Event) && len(r.AddAttendees) == 0 {
			return invalid("update requires at least one field")
		}
		if r.Event.Attendees != nil && len(r.AddAttendees) > 0 {
			return invalid("attendees replacement and add_attendees are mutually exclusive")
		}
		if (r.Action == actionMove || r.Action == "respond") && r.Scope == scopeFuture {
			return invalid("future scope only supports update and delete")
		}
	}
	if err := validateEventInput(r.Event); err != nil {
		return err
	}
	for _, email := range r.AddAttendees {
		if !validEmail(email) {
			return invalid("invalid attendee email %q", email)
		}
	}
	return nil
}
func hasEventFields(in gcal.EventInput) bool {
	return in.ID != "" || in.Summary != nil || in.Description != nil || in.Location != nil || in.Start != nil || in.End != nil || in.Recurrence != nil || in.Attendees != nil || in.Reminders != nil || in.AttendeesOmitted != nil
}
func validEmail(value string) bool {
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value && strings.Contains(value, "@")
}
func validateEventInput(in gcal.EventInput) error {
	if in.ID != "" {
		return invalid("event.id is assigned by msgvault")
	}
	for _, dt := range []*gcal.EventDateTime{in.Start, in.End} {
		if dt != nil {
			if (dt.Date == "") == dt.DateTime.IsZero() {
				return invalid("start/end require exactly one of date or dateTime")
			}
			if dt.Date != "" {
				if _, err := time.Parse("2006-01-02", dt.Date); err != nil {
					return invalid("invalid all-day date")
				}
			}
			if dt.TimeZone != "" {
				if _, err := time.LoadLocation(dt.TimeZone); err != nil {
					return invalid("invalid IANA time zone %q", dt.TimeZone)
				}
			}
		}
	}
	if in.Attendees != nil {
		seen := map[string]bool{}
		for _, a := range *in.Attendees {
			key := strings.ToLower(a.Email)
			if !validEmail(a.Email) || seen[key] {
				return invalid("attendees require unique email addresses")
			}
			seen[key] = true
			if a.Self || a.Organizer || a.ResponseStatus != "" {
				return invalid("attendee replacement cannot set self, organizer, or RSVP status")
			}
		}
	}
	if in.Recurrence != nil {
		for _, line := range *in.Recurrence {
			if !strings.HasPrefix(line, "RRULE:") {
				return invalid("recurrence accepts RRULE lines only")
			}
			if _, err := rrule.StrToROption(strings.TrimPrefix(line, "RRULE:")); err != nil {
				return invalid("invalid recurrence: %v", err)
			}
		}
	}
	if in.Reminders != nil {
		if len(in.Reminders.Overrides) > 5 || (in.Reminders.UseDefault && len(in.Reminders.Overrides) > 0) {
			return invalid("reminders allow up to five overrides, or useDefault")
		}
		for _, reminder := range in.Reminders.Overrides {
			if !slices.Contains([]string{"email", "popup"}, reminder.Method) || reminder.Minutes < 0 || reminder.Minutes > 40320 {
				return invalid("reminders require email/popup and 0 to 40320 minutes")
			}
		}
	}
	return nil
}
func validateRange(start, end *gcal.EventDateTime) error {
	if start == nil || end == nil {
		return invalid("both start and end are required")
	}
	if start.IsAllDay() != end.IsAllDay() {
		return invalid("start and end must both be timed or all-day")
	}
	a, aok := start.Instant()
	b, bok := end.Instant()
	if !aok || !bok || !b.After(a) {
		return invalid("end must follow start; all-day end is exclusive")
	}
	return nil
}
