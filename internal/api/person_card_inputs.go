package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

type personCardMaintenanceStore interface {
	GetPersonTrackingContext(ctx context.Context, personID int64) (*store.PersonTracking, error)
	PrimaryCurrentEmploymentContext(ctx context.Context, personID int64) (store.EmploymentProjection, bool, error)
	ListPersonAttributeValuesContext(ctx context.Context, personID int64, query store.PersonAttributeQuery) ([]store.PersonAttributeValue, error)
}
type personCardSourcesStore interface {
	ListPersonSourcesContext(ctx context.Context, personID int64, limit int) ([]store.PersonSource, error)
	GetSourceByIDContext(ctx context.Context, sourceID int64) (*store.Source, error)
}
type personCardIdentityStore interface {
	GetIdentitiesForScope(sourceIDs []int64) (map[string]struct{}, error)
}
type personCardParticipantEmailStore interface {
	MeetingParticipantEmailsContext(ctx context.Context, ids []int64) ([]string, error)
}

func (s *Server) personCardGap(in *personcard.Inputs, code string, err error) {
	in.Gaps = append(in.Gaps, code)
	if err != nil {
		s.logger.Warn("optional person card read unavailable", "gap", code, "error", err)
	}
}

func (s *Server) gatherPersonCard(ctx context.Context, p *store.Person, in *personcard.Inputs, opts personcard.Options) {
	if b, ok := s.store.(ActivityStore); ok {
		state, err := b.ContactStateContext(ctx, p.ID, in.CollectedAt)
		if err != nil {
			s.personCardGap(in, "contact_state_unavailable", err)
		} else if state.LastContactAt != nil || !state.ComputedAt.IsZero() || state.Stale {
			in.ContactState = &personcard.ContactState{LastContactAt: state.LastContactAt, Channel: string(state.LastContactChannel), Ref: state.LastContactRef, LastInboundAt: state.LastInboundAt, LastOutboundAt: state.LastOutboundAt, InteractionCount: state.InteractionCount, CadenceStatus: state.CadenceStatus, CadenceDueAt: state.CadenceDueAt, ComputedAt: state.ComputedAt, Stale: state.Stale}
			in.Person.InferredChannel = string(state.InferredChannel)
		}
	} else {
		s.personCardGap(in, "contact_state_unavailable", nil)
	}
	s.personCardBrief(ctx, p.ID, in)
	s.personCardActions(ctx, p, in)
	s.personCardMeetings(ctx, p, in, opts)
	s.personCardFacts(ctx, p.ID, in, opts)
	if s.personCardOpenLoops == nil {
		s.personCardGap(in, "open_loops_unavailable", nil)
	} else {
		page, err := s.personCardOpenLoops.ListPersonOpenLoops(ctx, p.ID, opts.AsOf, 20)
		if err != nil {
			s.personCardGap(in, "open_loops_unavailable", err)
		} else {
			in.Loops = page
		}
	}
	if s.personAgendaOperations == nil || s.cfg == nil || !s.cfg.Integrations.Kata.Enabled {
		s.personCardGap(in, "agenda_not_configured", nil)
	} else {
		agenda, err := s.personAgendaOperations.List(ctx, p.ID)
		if err != nil {
			s.personCardGap(in, "agenda_unavailable", err)
		} else {
			in.Coverage.AgendaTruncated = agenda.Truncated
			for _, item := range agenda.Items {
				if item.State != personagenda.StateOpen {
					continue
				}
				a := personcard.AgendaItem{QualifiedRef: item.QualifiedRef, Title: item.Title, List: item.List, WebURL: item.WebURL}
				if item.PriorityValue != nil {
					v := int(*item.PriorityValue)
					a.Priority = &v
				}
				in.Agenda = append(in.Agenda, a)
			}
		}
	}
	s.personCardSources(ctx, p.ID, in)
}

func (s *Server) personCardBrief(ctx context.Context, id int64, in *personcard.Inputs) {
	b, ok := s.store.(PersonBriefStore)
	if !ok {
		s.personCardGap(in, "brief_unavailable", nil)
		return
	}
	brief, err := b.GetPersonBriefContext(ctx, id, 0)
	if errors.Is(err, store.ErrPersonBriefNotFound) {
		s.personCardGap(in, "brief_none", nil)
		return
	}
	if err != nil {
		s.personCardGap(in, "brief_unavailable", err)
		return
	}
	if brief == nil {
		return
	}
	evidence, err := b.ListPersonBriefEvidenceContext(ctx, brief.ID)
	if err != nil {
		s.personCardGap(in, "brief_unavailable", err)
		return
	}
	out := &personcard.Brief{Version: brief.Version, GeneratedAt: brief.GeneratedAt, UntrustedText: &personcard.BriefText{Text: brief.RenderedText}}
	for _, e := range evidence {
		if e.Supported && personcard.ValidCitationRef(e.SourceRef) {
			out.Citations = append(out.Citations, e.SourceRef)
			out.Evidence = append(out.Evidence, e.SourceRef)
		}
	}
	in.Brief = out
}

func (s *Server) personCardActions(ctx context.Context, p *store.Person, in *personcard.Inputs) {
	b, ok := s.store.(interface {
		ListMeetingActionsContext(ctx context.Context, query store.MeetingActionsQuery) (*meetingcontent.ActionsPage, error)
	})
	if !ok {
		s.personCardGap(in, "actions_unavailable", nil)
		return
	}
	resolved, err := resolver.Resolve(ctx, s.store, resolver.Reference{Kind: resolver.ReferencePerson, ID: p.ID}, nil)
	if errors.Is(err, resolver.ErrEmptyPopulation) {
		return
	}
	if err != nil {
		s.personCardGap(in, "actions_unavailable", err)
		return
	}
	page, err := b.ListMeetingActionsContext(ctx, store.MeetingActionsQuery{Scope: store.MeetingQueryScope{Person: &resolved.Scope}, Status: meetingcontent.StatusPending, Limit: 200})
	if err != nil {
		s.personCardGap(in, "actions_unavailable", err)
		return
	}
	if page == nil {
		s.personCardGap(in, "actions_unavailable", nil)
		return
	}
	c := page.Coverage
	in.Coverage.Actions = personcard.ActionCoverage{MeetingCount: int(c.MeetingCount), Available: int(c.Available), Partial: int(c.Partial), Unsupported: int(c.Unsupported), Unavailable: int(c.Unavailable), Truncated: page.NextCursor != ""}
	personEmails := make(map[string]struct{}, len(in.Person.Emails))
	for _, email := range in.Person.Emails {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			personEmails[email] = struct{}{}
		}
	}
	if len(p.ParticipantIDs) > 0 {
		if people, ok := s.store.(personCardParticipantEmailStore); ok {
			observed, err := people.MeetingParticipantEmailsContext(ctx, p.ParticipantIDs)
			if err != nil {
				s.personCardGap(in, "actions_unavailable", err)
				return
			}
			for _, email := range observed {
				if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
					personEmails[email] = struct{}{}
				}
			}
		}
	}
	owners := map[int64]map[string]struct{}{}
	for _, row := range page.Rows {
		if _, seen := owners[row.Meeting.SourceID]; !seen {
			identities := map[string]struct{}{}
			if b, ok := s.store.(personCardIdentityStore); ok {
				loaded, err := b.GetIdentitiesForScope([]int64{row.Meeting.SourceID})
				if err != nil {
					s.personCardGap(in, "actions_unavailable", err)
					return
				}
				for email := range loaded {
					identities[strings.ToLower(strings.TrimSpace(email))] = struct{}{}
				}
			}
			primary, err := personCardPrimaryEmail(s.cfg, row.Meeting.SourceType, row.Meeting.SourceIdentifier)
			if err != nil {
				s.personCardGap(in, "actions_unavailable", err)
				return
			}
			if primary != "" {
				identities[primary] = struct{}{}
			}
			owners[row.Meeting.SourceID] = identities
		}
		assignee := strings.ToLower(strings.TrimSpace(row.Action.AssigneeEmail))
		class := "none"
		if assignee != "" {
			class = "other"
			if _, ok := personEmails[assignee]; ok {
				class = "person"
			}
			if _, ok := owners[row.Meeting.SourceID][assignee]; ok {
				class = "owner"
			}
		}
		a := personcard.Action{MessageID: row.Meeting.MessageID, Ordinal: row.Action.Ordinal, Status: string(row.Action.Status), Text: row.Action.Title, Assignee: row.Action.AssigneeEmail, AssigneeClass: class, DueDate: row.Action.DueDate, SourceID: row.Meeting.SourceID}
		if row.Meeting.OccurredAt != nil {
			a.OccurredAt = *row.Meeting.OccurredAt
		}
		in.Actions = append(in.Actions, a)
	}
}

func (s *Server) personCardMeetings(ctx context.Context, p *store.Person, in *personcard.Inputs, opts personcard.Options) {
	b, ok := s.store.(MeetingRecordsBackend)
	if !ok {
		s.personCardGap(in, "meetings_unavailable", nil)
		return
	}
	to := minTime(opts.AsOf, in.CollectedAt)
	from := opts.AsOf.AddDate(0, 0, -365)
	if !from.Before(to) {
		return
	}
	emails := append([]string{}, in.Person.Emails...)
	if people, ok := s.store.(interface {
		MeetingParticipantEmailsContext(ctx context.Context, participantIDs []int64) ([]string, error)
	}); ok {
		observed, err := people.MeetingParticipantEmailsContext(ctx, p.ParticipantIDs)
		if err != nil {
			s.personCardGap(in, "meetings_unavailable", err)
			return
		}
		emails = append(emails, observed...)
	}
	rows, cursor, err := b.ListPersonMeetingsContext(ctx, store.OwnerScope(), store.PersonMeetingsQuery{ParticipantIDs: p.ParticipantIDs, Emails: emails, From: from, To: to, Limit: 100, NewestFirst: true})
	if err != nil {
		s.personCardGap(in, "meetings_unavailable", err)
		return
	}
	in.Coverage.MeetingsTruncated = cursor != ""
	participantIDs := make(map[int64]struct{}, len(p.ParticipantIDs))
	for _, id := range p.ParticipantIDs {
		if id > 0 {
			participantIDs[id] = struct{}{}
		}
	}
	participantEmails := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			participantEmails[email] = struct{}{}
		}
	}
	for _, row := range rows {
		m := personcard.Meeting{Ref: row.Ref, Start: row.Start, Basis: "invited", Title: row.Title}
		if row.ItemCount > 0 {
			record, err := b.GetMeetingRecordContext(ctx, store.OwnerScope(), row.Ref)
			if err != nil {
				s.personCardGap(in, "meetings_unavailable", err)
				return
			}
			m.Invited, m.Participated = personCardMeetingParticipantStatus(record, participantIDs, participantEmails)
			if row.ItemCount > len(record.Items) {
				in.Coverage.MeetingsTruncated = true
			}
			if personCardMeetingPacketOmitted(record.Items) {
				in.Coverage.MeetingsTruncated = true
			}
			if m.Participated {
				m.Basis = "notes"
				if row.HasTranscript {
					m.Basis = "transcript"
				}
			}
		}
		if row.Occurrence != nil {
			m.Cancelled = row.Occurrence.Cancelled
			for _, attendee := range row.Occurrence.Attendees {
				for _, email := range emails {
					if strings.EqualFold(strings.TrimSpace(attendee.Email), strings.TrimSpace(email)) {
						m.Invited = true
					}
				}
			}
		}
		if !m.Invited && !m.Participated {
			continue
		}
		in.Meetings = append(in.Meetings, m)
	}
}

func personCardMeetingPacketOmitted(items []store.MeetingItem) bool {
	for _, item := range items {
		if item.Content == nil || len(item.Content.OmittedMessageIDs) > 0 || len(item.Content.Meetings) == 0 {
			return true
		}
		if item.MessageID <= 0 {
			continue
		}
		found := false
		for _, entry := range item.Content.Meetings {
			if entry.Meeting.MessageID == item.MessageID {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

func personCardMeetingParticipantStatus(record store.MeetingRecord, participantIDs map[int64]struct{}, emails map[string]struct{}) (invited, participated bool) {
	for _, item := range record.Items {
		if item.Content == nil {
			continue
		}
		for _, entry := range item.Content.Meetings {
			sourceType := strings.ToLower(strings.TrimSpace(entry.Meeting.SourceType))
			calendarParticipants := sourceType == "twenty" || sourceType == "notion_meetings"
			for _, participant := range entry.Participants {
				idMatch := false
				if participant.ParticipantID != nil {
					_, idMatch = participantIDs[*participant.ParticipantID]
				}
				email := strings.ToLower(strings.TrimSpace(participant.Email))
				_, emailMatch := emails[email]
				if !idMatch && (!emailMatch || email == "") {
					continue
				}
				provenance := strings.ToLower(strings.TrimSpace(participant.Provenance))
				if provenance == "" && !calendarParticipants {
					provenance = "provider_attendee"
				}
				if calendarParticipants && provenance != "transcript_speaker" {
					invited = true
					continue
				}
				switch provenance {
				case "invited":
					invited = true
				case "provider_attendee", "transcript_speaker":
					participated = true
				}
			}
		}
	}
	for _, participant := range record.Participants {
		idMatch := participant.ParticipantID > 0
		if idMatch {
			_, idMatch = participantIDs[participant.ParticipantID]
		}
		email := strings.ToLower(strings.TrimSpace(participant.Email))
		_, emailMatch := emails[email]
		if !idMatch && (!emailMatch || email == "") {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(participant.Provenance), "invited") {
			invited = true
		}
	}
	return invited, participated
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Server) personCardSources(ctx context.Context, id int64, in *personcard.Inputs) {
	in.Freshness.CollectedAt = in.CollectedAt
	b, ok := s.store.(personCardSourcesStore)
	status, hasStatus := s.store.(SourceStatusStore)
	if !ok || !hasStatus {
		in.Freshness.Unavailable = true
		s.personCardGap(in, "sources_unavailable", nil)
		return
	}
	sources, err := b.ListPersonSourcesContext(ctx, id, 10)
	if err != nil {
		in.Freshness.Unavailable = true
		s.personCardGap(in, "sources_unavailable", err)
		return
	}
	if len(sources) > 0 {
		in.Freshness.SourcesOmitted = max(0, sources[0].TotalCount-len(sources))
		in.Freshness.OmittedStale = in.Freshness.SourcesOmitted > 0
	}
	for _, source := range sources {
		row, err := b.GetSourceByIDContext(ctx, source.SourceID)
		if err != nil {
			in.Freshness.Unavailable = true
			s.personCardGap(in, "sources_unavailable", err)
			continue
		}
		value, err := s.sourceStatus(ctx, status, row)
		if err != nil {
			in.Freshness.Unavailable = true
			s.personCardGap(in, "sources_unavailable", err)
			continue
		}
		out := personcard.SourceInput{SourceID: source.SourceID, SourceType: row.SourceType, Identifier: row.Identifier, CanSync: value.CanSync, SyncUnavailableReason: value.SyncUnavailableReason}
		if value.Scheduled {
			out.Scheduling = "scheduled"
		}
		if value.LatestSync != nil {
			out.Outcome = value.LatestSync.Status
			out.LastSyncErrors = value.LatestSync.ErrorsCount
		}
		if value.LastSuccessfulSync != nil && value.LastSuccessfulSync.CompletedAt != nil {
			at, err := time.Parse(time.RFC3339Nano, *value.LastSuccessfulSync.CompletedAt)
			if err != nil {
				in.Freshness.Unavailable = true
				s.personCardGap(in, "sources_unavailable", fmt.Errorf("parse completed sync time: %w", err))
			} else {
				out.LastSuccessfulSyncAt = &at
			}
		}
		in.Freshness.Sources = append(in.Freshness.Sources, out)
	}
}

func fillPersonCardProfile(ctx context.Context, b personCardMaintenanceStore, id int64, in *personcard.Inputs) error {
	tracking, err := b.GetPersonTrackingContext(ctx, id)
	if err != nil {
		return fmt.Errorf("read card tracking: %w", err)
	}
	if tracking != nil {
		in.Person.Tracked = tracking.Tracked
	}
	employment, found, err := b.PrimaryCurrentEmploymentContext(ctx, id)
	if err != nil {
		return fmt.Errorf("read card employment: %w", err)
	}
	if found {
		in.Person.UntrustedText.OrganizationName = employment.OrganizationName
		in.Person.UntrustedText.Title = employment.Title
	}
	attrs, err := b.ListPersonAttributeValuesContext(ctx, id, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	if err != nil {
		return fmt.Errorf("read card primary channel: %w", err)
	}
	for _, attr := range attrs {
		if attr.Value.Text != nil {
			in.Person.PrimaryChannel = *attr.Value.Text
			break
		}
	}
	return nil
}
